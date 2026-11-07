package main

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/overlap"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/transcript"
)

// Live sessions with pending changes to the same files (internal/overlap).
// A session's pending files depend on where it runs:
//
//   - a linked worktree with no other live session in it is the session's
//     own: everything the worktree has changed, plus the files of its
//     branch's commits that no other branch or remote has;
//   - anywhere else (the main checkout, a worktree two sessions share,
//     another repository it edits) the session owns the files it wrote
//     through its file tools that are still uncommitted there. An edit
//     a later read found clean (committed or reverted) is forgotten until
//     the session writes that file again, so another session changing the
//     file afterwards does not name it.
//
// git runs in the background, at most every changesTTL per checkout; the
// poll reads the last answer.

// OverlapDTO is another live session with pending changes to some of the
// same files.
type OverlapDTO struct {
	With  string   `json:"with"`  // its session name
	Files []string `json:"files"` // the shared files, repo-relative, at most overlapFiles
	More  int      `json:"more"`  // shared files beyond those listed
}

const (
	overlapFiles = 12
	changesTTL   = 10 * time.Second
	changesKeep  = 2 * time.Minute // drop a checkout no row has asked about for this long
	changesReads = 4               // git reads at once
)

type changesEntry struct {
	ch   overlap.Changes
	ok   bool      // a read has come back
	at   time.Time // when the last read started
	used time.Time
}

type overlapState struct {
	loc     *overlap.Locator
	now     func() time.Time
	pending func(ctx context.Context, root string, branch bool) (overlap.Changes, error)
	mu      sync.Mutex
	entries map[string]*changesEntry
	// clean holds, per session, the edits a read found clean: path → the
	// edit's time. A later edit of the path counts again.
	clean    map[string]map[string]int64
	inflight map[string]bool
	sem      chan struct{}
	wg       sync.WaitGroup // lets tests wait for reads
}

func newOverlapState(now func() time.Time) *overlapState {
	return &overlapState{loc: overlap.NewLocator(), now: now, pending: overlap.Pending,
		entries: map[string]*changesEntry{}, clean: map[string]map[string]int64{}, inflight: map[string]bool{}, sem: make(chan struct{}, changesReads)}
}

// changes returns a checkout's last read changes, when that read started,
// and starts a fresh read when they are stale. ok is false until a first
// read has come back.
func (o *overlapState) changes(root string, branch bool) (overlap.Changes, time.Time, bool) {
	key := root
	if branch {
		key += "\x00branch"
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	e := o.entries[key]
	if e == nil {
		e = &changesEntry{}
		o.entries[key] = e
	}
	e.used = now
	if (!e.ok || now.Sub(e.at) >= changesTTL) && !o.inflight[key] {
		o.inflight[key] = true
		o.wg.Add(1)
		go o.refresh(key, root, branch)
	}
	return e.ch, e.at, e.ok
}

func (o *overlapState) refresh(key, root string, branch bool) {
	defer o.wg.Done()
	o.sem <- struct{}{}
	defer func() { <-o.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := o.now()
	ch, err := o.pending(ctx, root, branch)
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.inflight, key)
	e := o.entries[key]
	if e == nil {
		return
	}
	if err != nil && ch.Dirty == nil && ch.Branch == nil {
		e.at = o.now() // keep the last answer; ask again after the TTL
		return
	}
	e.ch, e.ok, e.at = ch, true, start
}

func (o *overlapState) prune() {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	for k, e := range o.entries {
		if !o.inflight[k] && now.Sub(e.used) > changesKeep {
			delete(o.entries, k)
		}
	}
}

// cleaned reports whether a read after this edit found its file clean.
func (o *overlapState) cleaned(name string, e transcript.Edit) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	at, ok := o.clean[name][e.Path]
	return ok && at >= e.At
}

func (o *overlapState) forget(name string, e transcript.Edit) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.clean[name] == nil {
		o.clean[name] = map[string]int64{}
	}
	o.clean[name][e.Path] = e.At
}

// apply sets the rows' overlaps. rows are every live row of the poll,
// hidden projects' included: they decide whether a worktree has one
// session in it. Only the dtos (the visible rows) are compared, so a
// hidden project's session never appears in a chip.
func (o *overlapState) apply(dtos []SessionDTO, rows []status.Row) {
	if o == nil {
		return
	}
	defer o.prune()
	byName := make(map[string]status.Row, len(rows))
	home := map[string]overlap.Checkout{}
	perRoot := map[string]int{}
	for _, r := range rows {
		byName[r.Name] = r
		if co, ok := o.loc.Find(r.Cwd); ok {
			home[r.Name] = co
			perRoot[co.Root]++
		}
	}
	type dirtySet struct {
		files map[string]bool
		at    time.Time // the read's start; zero before a first read
	}
	dirty := map[string]dirtySet{}
	dirtyIn := func(root string) dirtySet {
		if d, ok := dirty[root]; ok {
			return d
		}
		ch, at, ok := o.changes(root, false)
		d := dirtySet{files: make(map[string]bool, len(ch.Dirty))}
		if ok {
			d.at = at
		}
		for _, f := range ch.Dirty {
			d.files[f] = true
		}
		dirty[root] = d
		return d
	}
	o.mu.Lock()
	for name := range o.clean {
		if _, live := byName[name]; !live {
			delete(o.clean, name)
		}
	}
	o.mu.Unlock()
	var sessions []overlap.Session
	for _, d := range dtos {
		r, ok := byName[d.Name]
		if !ok {
			continue
		}
		s := overlap.Session{Name: d.Name}
		co, inCo := home[d.Name]
		owned := inCo && co.Linked && perRoot[co.Root] == 1
		if owned {
			ch, _, _ := o.changes(co.Root, true)
			for _, f := range append(append([]string(nil), ch.Dirty...), ch.Branch...) {
				s.Files = append(s.Files, overlap.File{Repo: co.Repo, Path: f})
			}
		}
		for _, e := range r.Edited {
			if o.cleaned(d.Name, e) {
				continue
			}
			eco, ok := o.loc.Find(filepath.Dir(e.Path))
			if !ok || (owned && eco.Root == co.Root) {
				continue
			}
			rel, err := filepath.Rel(eco.Root, e.Path)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			ds := dirtyIn(eco.Root)
			switch {
			case ds.files[rel]:
				s.Files = append(s.Files, overlap.File{Repo: eco.Repo, Path: rel})
			case !ds.at.IsZero() && ds.at.Unix() > e.At:
				o.forget(d.Name, e)
			}
		}
		if len(s.Files) > 0 {
			sessions = append(sessions, s)
		}
	}
	hits := overlap.Find(sessions)
	for i := range dtos {
		for _, h := range hits[dtos[i].Name] {
			od := OverlapDTO{With: h.With, Files: h.Files}
			if len(od.Files) > overlapFiles {
				od.More = len(od.Files) - overlapFiles
				od.Files = od.Files[:overlapFiles]
			}
			dtos[i].Overlaps = append(dtos[i].Overlaps, od)
		}
	}
}
