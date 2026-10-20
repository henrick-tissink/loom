package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newWorktree makes a repo with one commit and a claude-style worktree.
func newWorktree(t *testing.T) (repo, root string) {
	t.Helper()
	repo, _ = filepath.EvalSymlinks(t.TempDir())
	run(t, repo, "init", "-q", "-b", "main")
	run(t, repo, "commit", "-q", "--allow-empty", "-m", "a")
	root = filepath.Join(repo, Dir, "loom-1a2b3c4d")
	run(t, repo, "worktree", "add", "-q", "-b", "worktree-loom-1a2b3c4d", root)
	return repo, root
}

func TestParse(t *testing.T) {
	repo, name, root, ok := Parse("/w/app/.claude/worktrees/loom-1a/sub/dir")
	if !ok || repo != "/w/app" || name != "loom-1a" || root != "/w/app/.claude/worktrees/loom-1a" {
		t.Fatalf("Parse = %q %q %q %v", repo, name, root, ok)
	}
	for _, p := range []string{"/w/app", "/w/app/.claude/worktrees/", "/w/app/.claude/worktreesx/a", ".claude/worktrees/a"} {
		if _, _, _, ok := Parse(p); ok {
			t.Errorf("Parse(%q) should not match", p)
		}
	}
	if SessionName("1a2b3c4d-5e6f") != "loom-1a2b3c4d" {
		t.Fatal(SessionName("1a2b3c4d-5e6f"))
	}
}

func TestReadAndRemove(t *testing.T) {
	ctx := context.Background()
	repo, root := newWorktree(t)
	if top, err := Toplevel(ctx, repo); err != nil || top != repo {
		t.Fatalf("Toplevel = %q, %v", top, err)
	}
	st, err := Read(ctx, root)
	if err != nil || st != (State{Branch: "worktree-loom-1a2b3c4d"}) {
		t.Fatalf("fresh state = %+v, %v", st, err)
	}

	// an untracked file: kept
	os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644)
	if st, _ := Read(ctx, root); st.Changed != 1 {
		t.Fatalf("changed = %+v", st)
	}
	if err := Remove(ctx, root); err == nil || !strings.Contains(err.Error(), "1 changed") {
		t.Fatalf("dirty remove: %v", err)
	}

	// committed there and nowhere else: kept
	run(t, root, "add", "f")
	run(t, root, "commit", "-q", "-m", "b")
	if st, _ := Read(ctx, root); st.Changed != 0 || st.Unique != 1 {
		t.Fatalf("unique = %+v", st)
	}
	if err := Remove(ctx, root); err == nil || !strings.Contains(err.Error(), "1 commit on no other branch") {
		t.Fatalf("unique remove: %v", err)
	}

	// merged into main: removable; the branch stays
	run(t, repo, "merge", "-q", "--ff-only", "worktree-loom-1a2b3c4d")
	if err := Remove(ctx, root); err != nil {
		t.Fatalf("clean remove: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still there: %v", err)
	}
	run(t, repo, "rev-parse", "--verify", "worktree-loom-1a2b3c4d")
	if err := Remove(ctx, root); err == nil {
		t.Fatal("removing a gone worktree must say so")
	}
}

func TestRemoveLocks(t *testing.T) {
	ctx := context.Background()
	orig := pidAlive
	t.Cleanup(func() { pidAlive = orig })

	repo, root := newWorktree(t)
	run(t, repo, "worktree", "lock", "--reason", "claude session loom-1a2b3c4d (pid 4242 start now)", root)
	pidAlive = func(int) bool { return true }
	if err := Remove(ctx, root); err == nil || !strings.Contains(err.Error(), "running claude (pid 4242)") {
		t.Fatalf("live lock: %v", err)
	}
	pidAlive = func(int) bool { return false }
	if err := Remove(ctx, root); err != nil {
		t.Fatalf("dead claude's lock is released: %v", err)
	}

	repo, root = newWorktree(t)
	run(t, repo, "worktree", "lock", "--reason", "mine", root)
	if err := Remove(ctx, root); err == nil || !strings.Contains(err.Error(), "locked (mine)") {
		t.Fatalf("someone else's lock: %v", err)
	}
	if err := Remove(ctx, "/w/not-a-worktree"); err == nil {
		t.Fatal("a non-worktree path must be refused")
	}
}

func TestDiscard(t *testing.T) {
	ctx := context.Background()
	orig := pidAlive
	t.Cleanup(func() { pidAlive = orig })

	repo, root := newWorktree(t)
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644)
	run(t, root, "add", "f.txt")
	run(t, root, "commit", "-q", "-m", "attempt")
	os.WriteFile(filepath.Join(root, "uncommitted.txt"), []byte("y"), 0o644)
	run(t, repo, "worktree", "lock", "--reason", "claude session loom-1a2b3c4d (pid 4242 start now)", root)
	pidAlive = func(int) bool { return true }
	if err := Discard(ctx, root); err == nil || !strings.Contains(err.Error(), "running claude") {
		t.Fatalf("live claude: %v", err)
	}
	pidAlive = func(int) bool { return false }
	if err := Remove(ctx, root); err == nil {
		t.Fatal("Remove should still refuse a worktree with work in it")
	}
	if err := Discard(ctx, root); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("the worktree is still there")
	}
	// The branch, and the commit on it, are kept.
	run(t, repo, "rev-parse", "--verify", "worktree-loom-1a2b3c4d")
	if err := Discard(ctx, root); err == nil {
		t.Fatal("discarding a gone worktree should say so")
	}
}
