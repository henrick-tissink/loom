package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/worktree"
)

// The work ledger: one row per claude worktree in every repository loom has
// run a session in, tying the worktree to its session, branch, PR and the
// work it still holds (changed files, commits ahead of the default branch,
// commits on no other branch, stashes). A row is settled by hand, or by
// itself once its PR merges, and never while a session runs in it or it
// holds work a removal could lose. "Clean up settled" removes settled
// worktrees (ignored build output goes with the directory) and keeps their
// branches; worktree.Remove still refuses anything that could lose work.
// Settled marks live in ~/.loom/ledger.json, keyed by worktree directory.

// ledgerMark is one worktree's settle state. Kept means the person undid a
// settle, so a merged PR does not settle it again.
type ledgerMark struct {
	SettledAt int64 `json:"settledAt,omitempty"`
	Auto      bool  `json:"auto,omitempty"`
	Kept      bool  `json:"kept,omitempty"`
}

type ledgerStore struct {
	mu   sync.Mutex
	path string // "" keeps marks in memory only
	m    map[string]ledgerMark
}

func newLedgerStore(path string) *ledgerStore {
	l := &ledgerStore{path: path, m: map[string]ledgerMark{}}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &l.m)
		}
	}
	if l.m == nil {
		l.m = map[string]ledgerMark{}
	}
	return l
}

func (l *ledgerStore) get(root string) (ledgerMark, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.m[root]
	return m, ok
}

// set stores a mark (delete when del) and writes the file atomically.
func (l *ledgerStore) set(root string, m ledgerMark, del bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	next := make(map[string]ledgerMark, len(l.m)+1)
	for k, v := range l.m {
		next[k] = v
	}
	if del {
		delete(next, root)
	} else {
		next[root] = m
	}
	if l.path != "" {
		if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
			return err
		}
		b, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return err
		}
		tmp := l.path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, l.path); err != nil {
			return err
		}
	}
	l.m = next
	return nil
}

// LedgerRowDTO is one worktree on the Work page.
type LedgerRowDTO struct {
	Root      string `json:"root"`
	Repo      string `json:"repo"`
	Name      string `json:"name"` // the worktree's name
	Branch    string `json:"branch"`
	Session   string `json:"session,omitempty"` // loom's row for it, newest first
	Title     string `json:"title,omitempty"`
	Live      bool   `json:"live"`
	Waiting   string `json:"waiting,omitempty"` // what a live session waits on
	Changed   int    `json:"changed"`
	Unique    int    `json:"unique"`
	Ahead     int    `json:"ahead"`
	Base      string `json:"base,omitempty"`
	Stashes   int    `json:"stashes"`
	PR        *PRDTO `json:"pr,omitempty"`
	Covered   bool   `json:"covered"` // HEAD is in what the merged PR took
	Settled   bool   `json:"settled"`
	SettledAt int64  `json:"settledAt,omitempty"`
	Auto      bool   `json:"auto,omitempty"`
	Orphan    bool   `json:"orphan"`
	Blocker   string `json:"blocker,omitempty"` // why it can't be settled now
	Err       string `json:"err,omitempty"`
}

func prState(r *LedgerRowDTO) string {
	if r.PR == nil {
		return ""
	}
	return r.PR.State
}

// blocker is why a row can't be settled (or cleaned up) now, or "".
func (a *App) blocker(r *LedgerRowDTO) string {
	switch {
	case r.Err != "":
		return "could not read it"
	case r.Live:
		return "a session is running in it"
	case r.Changed > 0:
		return fmt.Sprintf("%d changed or untracked %s", r.Changed, plural(r.Changed, "file", "files"))
	case r.Stashes > 0:
		return fmt.Sprintf("%d %s", r.Stashes, plural(r.Stashes, "stash", "stashes"))
	case r.Unique > 0 && !r.Covered:
		return fmt.Sprintf("%d %s on no other branch or remote", r.Unique, plural(r.Unique, "commit", "commits"))
	}
	if kind := a.jobAlive(r.Root); kind != "" {
		return "its " + kind + " script is running"
	}
	return ""
}

// ledgerRepos are the repositories loom's sessions ran in, with the newest
// session row for each worktree directory.
func (a *App) ledgerRepos(ctx context.Context) ([]string, map[string]store.SessionRow) {
	var rows []store.SessionRow
	if live, err := a.st.Live(); err == nil {
		rows = append(rows, live...)
	}
	if ended, err := a.st.Recent(2000); err == nil {
		rows = append(rows, ended...)
	}
	byRoot := map[string]store.SessionRow{}
	repos := map[string]bool{}
	seen := map[string]bool{}
	for _, r := range rows {
		if repo, _, root, ok := worktree.Parse(r.Cwd); ok {
			// git lists worktrees by their resolved path; a row keeps the
			// path the person picked, which may run through a symlink
			repos[realPath(repo)] = true
			if _, ok := byRoot[realPath(root)]; !ok { // live first, then newest ended
				byRoot[realPath(root)] = r
			}
			continue
		}
		if r.Cwd == "" || seen[r.Cwd] {
			continue
		}
		seen[r.Cwd] = true
		if top, err := worktree.Toplevel(ctx, r.Cwd); err == nil {
			repos[realPath(top)] = true
		}
	}
	out := make([]string, 0, len(repos))
	for r := range repos {
		out = append(out, r)
	}
	sort.Strings(out)
	return out, byRoot
}

// ledgerRow reads one worktree.
func (a *App) ledgerRow(ctx context.Context, repo string, e worktree.Entry, row store.SessionRow, hasRow bool) LedgerRowDTO {
	_, name, _, _ := worktree.Parse(e.Root + string(filepath.Separator))
	r := LedgerRowDTO{Root: e.Root, Repo: repo, Name: name, Branch: e.Branch}
	if hasRow {
		r.Session, r.Title = row.Name, row.Title
		if row.EndedAt < 0 {
			r.Live = true
			r.Waiting = a.dialogOpen(row.Name)
		}
	}
	if a.worktreeInUse(e.Root) != "" {
		r.Live = true
	}
	st, err := worktree.Read(ctx, e.Root)
	if err != nil {
		r.Err = err.Error()
	} else {
		r.Changed, r.Unique = st.Changed, st.Unique
		if st.Branch != "" {
			r.Branch = st.Branch
		}
	}
	r.Ahead, r.Base = worktree.Ahead(ctx, e.Root)
	r.Stashes = worktree.Stashes(ctx, e.Root, r.Branch)
	if a.prs != nil {
		r.PR = a.prs.forBranch(forgeDir(e.Root), r.Branch, r.Live)
	}
	if prState(&r) == "merged" {
		r.Covered = worktree.Covered(ctx, e.Root, r.PR.Head)
	}
	r.Blocker = a.blocker(&r)
	ps := prState(&r)
	held := r.Changed > 0 || r.Unique > 0 || r.Stashes > 0
	r.Orphan = !r.Live && held && ps != "open" && ps != "draft" && ps != "merged"
	return r
}

// applyMark settles a row whose PR has merged, drops a settle that work has
// since come back into, and copies the mark onto the row.
func (a *App) applyMark(r *LedgerRowDTO) {
	m, ok := a.ledger.get(r.Root)
	switch {
	case ok && m.SettledAt > 0 && r.Blocker != "" && r.Err == "":
		// (a row that could not be read keeps its mark: the read failed,
		// not the settle)
		// a session resumed in it, or work appeared: it is no longer settled
		_ = a.ledger.set(r.Root, ledgerMark{}, true)
		m, ok = ledgerMark{}, false
	case !ok && r.Blocker == "" && prState(r) == "merged":
		m = ledgerMark{SettledAt: a.now().Unix(), Auto: true}
		if a.ledger.set(r.Root, m, false) == nil {
			ok = true
		}
	}
	if ok && m.SettledAt > 0 {
		r.Settled, r.SettledAt, r.Auto = true, m.SettledAt, m.Auto
	}
}

// Ledger lists every claude worktree of the repositories loom's sessions
// ran in: orphans first, then open work, then settled rows.
func (a *App) Ledger() ([]LedgerRowDTO, error) {
	if a.st == nil || a.ledger == nil {
		return nil, fmt.Errorf("ledger unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repos, byRoot := a.ledgerRepos(ctx)
	out := []LedgerRowDTO{}
	for _, repo := range repos {
		es, err := worktree.List(ctx, repo)
		if err != nil {
			continue // a repository that moved or went away
		}
		for _, e := range es {
			row, has := byRoot[realPath(e.Root)]
			r := a.ledgerRow(ctx, repo, e, row, has)
			a.applyMark(&r)
			out = append(out, r)
		}
	}
	rank := func(r LedgerRowDTO) int {
		switch {
		case r.Orphan:
			return 0
		case !r.Settled:
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) < rank(out[j])
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// realPath is p with symlinks resolved, or p cleaned when it can't be.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// ledgerFind reads the current row for a worktree directory.
func (a *App) ledgerFind(root string) (LedgerRowDTO, error) {
	rows, err := a.Ledger()
	if err != nil {
		return LedgerRowDTO{}, err
	}
	for _, r := range rows {
		if r.Root == root {
			return r, nil
		}
	}
	return LedgerRowDTO{}, fmt.Errorf("no claude worktree at %s", root)
}

// Settle marks a worktree's work settled, refused with the reason while a
// session runs in it or it holds work a removal could lose.
func (a *App) Settle(root string) error {
	r, err := a.ledgerFind(root)
	if err != nil {
		return err
	}
	if r.Blocker != "" {
		return fmt.Errorf("not settled: %s", r.Blocker)
	}
	if r.Settled {
		return nil
	}
	return a.ledger.set(root, ledgerMark{SettledAt: a.now().Unix()}, false)
}

// Unsettle undoes a settle; a merged PR will not settle the row again.
func (a *App) Unsettle(root string) error {
	if a.ledger == nil {
		return fmt.Errorf("ledger unavailable")
	}
	return a.ledger.set(root, ledgerMark{Kept: true}, false)
}

// CleanupResultDTO is one settled worktree's cleanup outcome.
type CleanupResultDTO struct {
	Root string `json:"root"`
	Name string `json:"name"`
	Err  string `json:"err,omitempty"`
}

// CleanupSettled removes every settled worktree, keeping its branch. Each is
// read again first; one whose PR merged may hold commits on no other branch
// (a squash merge with the remote branch deleted), anything else that could
// lose work keeps it.
func (a *App) CleanupSettled() ([]CleanupResultDTO, error) {
	rows, err := a.Ledger()
	if err != nil {
		return nil, err
	}
	out := []CleanupResultDTO{}
	for _, r := range rows {
		if !r.Settled {
			continue
		}
		res := CleanupResultDTO{Root: r.Root, Name: r.Name}
		if r.Blocker != "" {
			res.Err = "kept: " + r.Blocker
			out = append(out, res)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		var err error
		if prState(&r) == "merged" {
			err = worktree.RemoveMerged(ctx, r.Root, r.PR.Head)
		} else {
			err = worktree.Remove(ctx, r.Root)
		}
		cancel()
		if err != nil {
			res.Err = err.Error()
		} else {
			_ = a.ledger.set(r.Root, ledgerMark{}, true)
			a.wt.forget(r.Root)
		}
		out = append(out, res)
	}
	return out, nil
}
