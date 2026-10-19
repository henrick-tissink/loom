package main

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/forge"
	"github.com/henricktissink/loom/internal/transcript"
	"github.com/henricktissink/loom/internal/worktree"
)

// PRDTO is the pull request (GitHub) or merge request (GitLab) for the
// branch a session last worked on. Err is set when the forge could not be
// asked; Number is then the last answer it gave, or 0 when it never did.
type PRDTO struct {
	Kind   string `json:"kind"` // github | gitlab
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"` // open | draft | merged | closed
	URL    string `json:"url"`
	Checks string `json:"checks"` // passing | failing | running | ""
	Review string `json:"review"` // approved | changes_requested | review_required | ""
	Branch string `json:"branch"`
	Err    string `json:"err,omitempty"`
}

// branchOverlap is how far before the last read's end a rescan starts.
const branchOverlap = 4 << 10

// prLookups bounds the gh/glab processes running at once.
const prLookups = 4

type branchEntry struct {
	size   int64
	mod    time.Time
	branch string
}

type prEntry struct {
	dto  *PRDTO // the last answer; nil = the branch has no PR
	err  string
	kind string // the forge, kept for a lookup that failed before any answer
	at   time.Time
}

// prCache answers a row's PR from memory and refreshes it in the
// background: a lookup is a network call (two on GitLab), so the 1.5 s poll
// never waits on one. A new answer shows on the next poll. Lookups are keyed
// by (directory, branch), so rows sharing a branch share one lookup.
type prCache struct {
	mu       sync.Mutex
	now      func() time.Time
	lookup   func(ctx context.Context, dir, branch string) (*forge.PR, error)
	branches map[string]branchEntry // transcript path → its newest gitBranch
	prs      map[string]prEntry
	inflight map[string]bool
	sem      chan struct{}
	wg       sync.WaitGroup // lets tests wait for refreshes
}

func newPRCache(now func() time.Time, lookup func(ctx context.Context, dir, branch string) (*forge.PR, error)) *prCache {
	return &prCache{now: now, lookup: lookup, branches: map[string]branchEntry{}, prs: map[string]prEntry{},
		inflight: map[string]bool{}, sem: make(chan struct{}, prLookups)}
}

// prTTL is how long an answer stands. A live session's open PR is the one
// whose checks move under you; a merged or closed one is settled.
func prTTL(e prEntry, live bool) time.Duration {
	switch {
	case e.err != "":
		return 2 * time.Minute
	case e.dto == nil && live:
		return 2 * time.Minute
	case e.dto == nil:
		return 10 * time.Minute
	case e.dto.State == "merged" || e.dto.State == "closed":
		return 15 * time.Minute
	case live:
		return time.Minute
	}
	return 5 * time.Minute
}

// info returns the PR for the branch in a session's transcript, or nil when
// the session has no branch, the branch has no PR, or the first lookup has
// not come back yet. cwd is where gh/glab run; a removed worktree falls back
// to its main checkout.
func (c *prCache) info(cwd, transcriptPath string, live bool) *PRDTO {
	dir := cwd
	if !isDir(dir) {
		repo, _, _, ok := worktree.Parse(cwd)
		if !ok || !isDir(repo) {
			return nil
		}
		dir = repo
	}
	branch := c.branch(transcriptPath)
	if branch == "" {
		return nil
	}
	key := dir + "\x00" + branch
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.prs[key]
	if (!ok || c.now().Sub(e.at) >= prTTL(e, live)) && !c.inflight[key] {
		c.inflight[key] = true
		c.wg.Add(1)
		go c.refresh(key, dir, branch)
	}
	if !ok || (e.dto == nil && e.err == "") {
		return nil
	}
	d := PRDTO{Branch: branch, Kind: e.kind}
	if e.dto != nil {
		d = *e.dto
	}
	d.Err = e.err
	return &d
}

func (c *prCache) refresh(key, dir, branch string) {
	defer c.wg.Done()
	c.sem <- struct{}{}
	defer func() { <-c.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pr, err := c.lookup(ctx, dir, branch)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, key)
	e := c.prs[key]
	e.at = c.now()
	if err != nil {
		// keep the last answer: a dropped connection shouldn't blank a PR
		e.err = err.Error()
		if e.dto == nil && pr != nil {
			e.kind = pr.Kind // so a GitLab failure reads "MR ?"
		}
	} else {
		e.err = ""
		e.dto = nil
		if pr != nil {
			e.dto = &PRDTO{Kind: pr.Kind, Number: pr.Number, Title: pr.Title, State: pr.State,
				URL: pr.URL, Checks: pr.Checks, Review: pr.Review, Branch: branch}
		}
	}
	c.prs[key] = e
}

// branch reads a transcript's newest gitBranch, again only when the file
// has changed since the last read.
func (c *prCache) branch(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	c.mu.Lock()
	e, ok := c.branches[path]
	c.mu.Unlock()
	if ok && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
		return e.branch
	}
	// A growing transcript is scanned only from just before where the last
	// read ended (the overlap catches a record cut at that edge), so a live
	// session costs its new bytes per poll, not a tail read; a branch-less
	// stretch keeps the branch already known.
	var since int64
	if ok && fi.Size() > e.size {
		since = max(0, e.size-branchOverlap)
	}
	b, err := transcript.LastBranchSince(path, since)
	if err != nil {
		return ""
	}
	if b == "" && since > 0 {
		b = e.branch
	}
	c.mu.Lock()
	c.branches[path] = branchEntry{size: fi.Size(), mod: fi.ModTime(), branch: b}
	c.mu.Unlock()
	return b
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// transcriptFor is a session's transcript path, or "" when loom does not
// know the claude config dir (tests without a launcher).
func (a *App) transcriptFor(cwd, sessionID string) string {
	if a.launcher == nil || sessionID == "" {
		return ""
	}
	return transcript.Path(a.launcher.ClaudeConfigDir, cwd, sessionID)
}
