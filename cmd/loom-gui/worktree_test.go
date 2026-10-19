package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func testWorktree(t *testing.T) (repo, root string) {
	t.Helper()
	repo, _ = filepath.EvalSymlinks(t.TempDir())
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "a")
	root = filepath.Join(repo, ".claude", "worktrees", "loom-1a2b3c4d")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "worktree-loom-1a2b3c4d", root)
	return repo, root
}

func TestWorktreeCacheInfo(t *testing.T) {
	_, root := testWorktree(t)
	now := time.Unix(1000, 0)
	c := newWorktreeCache(func() time.Time { return now })
	if c.info("/w/plain", false) != nil {
		t.Fatal("a cwd outside any worktree has no worktree info")
	}
	d := c.info(filepath.Join(root, "sub"), false)
	if d == nil || d.Name != "loom-1a2b3c4d" || d.Branch != "worktree-loom-1a2b3c4d" || d.Changed != 0 || d.Err != "" {
		t.Fatalf("info = %+v", d)
	}
	os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644)
	if d := c.info(root, false); d.Changed != 0 {
		t.Fatalf("within the TTL the cached state is served: %+v", d)
	}
	now = now.Add(worktreeTTL)
	if d := c.info(root, false); d.Changed != 1 {
		t.Fatalf("after the TTL git is read again: %+v", d)
	}
	gone := filepath.Join(filepath.Dir(root), "loom-gone")
	if c.info(gone, true) != nil {
		t.Fatal("a finished row's vanished worktree is not offered for removal")
	}
	if d := c.info(gone, false); d == nil || d.Err == "" {
		t.Fatalf("a live row's vanished worktree says so: %+v", d)
	}
}

func TestRemoveWorktree(t *testing.T) {
	_, root := testWorktree(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Upsert(store.SessionRow{Name: "loom-live", ClaudeSessionID: "a", Cwd: root, CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "running"})
	st.Upsert(store.SessionRow{Name: "loom-done", ClaudeSessionID: "b", Cwd: root, CreatedAt: 1, EndedAt: 2, ExitCode: 0, LastStatus: "done"})
	app := newApp(nil, tmux.New(), st, nil, nil, time.Now)
	if err := app.RemoveWorktree("loom-live"); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("live session's worktree: %v", err)
	}
	// a resumed session reuses a finished row's worktree: the finished row
	// must not remove it from under the live one
	if err := app.RemoveWorktree("loom-done"); err == nil || !strings.Contains(err.Error(), "session loom-live is running in it") {
		t.Fatalf("finished row sharing a live session's worktree: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("worktree removed under a live session: %v", err)
	}
	st.Upsert(store.SessionRow{Name: "loom-live", ClaudeSessionID: "a", Cwd: root, CreatedAt: 1, EndedAt: 3, ExitCode: 0, LastStatus: "done"})
	if err := app.RemoveWorktree("loom-done"); err != nil {
		t.Fatalf("clean finished worktree: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("worktree still on disk: %v", err)
	}
}
