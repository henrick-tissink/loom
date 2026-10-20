package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/ccsessions"
	"github.com/henricktissink/loom/internal/worktree"
)

// WorktreeDTO is a worktree session's branch and what removing the worktree
// would lose. Err is set, and the counts are zero, when git could not read
// it; the row then says so instead of showing a clean worktree.
type WorktreeDTO struct {
	Name    string `json:"name"`
	Branch  string `json:"branch"`
	Changed int    `json:"changed"`
	Unique  int    `json:"unique"`
	Err     string `json:"err,omitempty"`
}

// worktreeTTL bounds how stale a row's worktree counts may be: two git runs
// per worktree row, every poll, would be wasted on numbers that change on
// the scale of a turn.
const worktreeTTL = 10 * time.Second

type worktreeEntry struct {
	dto WorktreeDTO
	at  time.Time
}

type worktreeCache struct {
	mu  sync.Mutex
	m   map[string]worktreeEntry
	now func() time.Time
}

func newWorktreeCache(now func() time.Time) *worktreeCache {
	return &worktreeCache{m: map[string]worktreeEntry{}, now: now}
}

// info returns the worktree a session's cwd is in, or nil when it is in
// none. onlyIfPresent drops a worktree whose directory is gone (a finished
// row then has nothing to remove); a live row keeps showing it, with the
// read error, because claude is running there.
func (c *worktreeCache) info(cwd string, onlyIfPresent bool) *WorktreeDTO {
	_, name, root, ok := worktree.Parse(cwd)
	if !ok {
		return nil
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		if onlyIfPresent {
			return nil
		}
		return &WorktreeDTO{Name: name, Err: "worktree not on disk"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if e, ok := c.m[root]; ok && now.Sub(e.at) < worktreeTTL {
		d := e.dto
		return &d
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d := WorktreeDTO{Name: name}
	if st, err := worktree.Read(ctx, root); err != nil {
		d.Err = err.Error()
	} else {
		d.Branch, d.Changed, d.Unique = st.Branch, st.Changed, st.Unique
	}
	c.m[root] = worktreeEntry{dto: d, at: now}
	return &d
}

func (c *worktreeCache) forget(cwd string) {
	if _, _, root, ok := worktree.Parse(cwd); ok {
		c.mu.Lock()
		delete(c.m, root)
		c.mu.Unlock()
	}
}

// worktreeInUse names a running session whose cwd is in the same worktree
// as cwd, or returns "". A resumed or fresh session reuses a finished row's
// worktree without re-locking it (the lock is written once, at creation), so
// the finished row's own state says nothing about who is in it now: loom's
// live rows and claude's registry (any claude, loom's or not) are asked.
func (a *App) worktreeInUse(cwd string) string {
	_, _, root, ok := worktree.Parse(cwd)
	if !ok {
		return ""
	}
	in := func(c string) bool {
		_, _, r, ok := worktree.Parse(c)
		return ok && r == root
	}
	if live, err := a.st.Live(); err == nil {
		for _, r := range live {
			if in(r.Cwd) {
				return "session " + r.Name
			}
		}
	}
	if a.launcher != nil {
		for _, e := range ccsessions.List(ccsessions.Dir(a.launcher.ClaudeConfigDir)) {
			if in(e.Cwd) {
				return fmt.Sprintf("a claude (pid %d)", e.PID)
			}
		}
	}
	return ""
}

// RemoveWorktree removes a finished session's worktree and keeps its branch.
// worktree.Remove refuses, with the reason, whenever that could lose work;
// a live session's worktree is refused here first.
func (a *App) RemoveWorktree(name string) error {
	if a.st == nil {
		return fmt.Errorf("store unavailable")
	}
	row, ok, err := a.st.Get(name)
	if err != nil || !ok {
		return fmt.Errorf("no session %q", name)
	}
	if row.EndedAt < 0 {
		return fmt.Errorf("kept: the session is still running")
	}
	if who := a.worktreeInUse(row.Cwd); who != "" {
		return fmt.Errorf("kept: %s is running in it", who)
	}
	if kind := a.jobAlive(row.Cwd); kind != "" {
		return fmt.Errorf("kept: its %s script is running; stop it first", kind)
	}
	defer a.wt.forget(row.Cwd)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return worktree.Remove(ctx, row.Cwd)
}
