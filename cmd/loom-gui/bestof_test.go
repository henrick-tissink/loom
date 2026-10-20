package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/bestof"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/worktree"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestBestOfCompareAndKeep(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loombo%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := newApp(nil, tm, st, nil, nil, time.Now)
	if _, err := a.LaunchBestOf("/r", "", "", "x", nil, 2); err == nil {
		t.Fatal("launched with no launcher")
	}
	bo, err := newBestofState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.bestof = bo

	repo, _ := filepath.EvalSymlinks(t.TempDir())
	gitT(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "app.txt"), []byte("v1\n"), 0o644)
	gitT(t, repo, "add", "app.txt")
	gitT(t, repo, "commit", "-q", "-m", "base")
	oldBase := gitT(t, repo, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(repo, "upstream.txt"), []byte("not the attempts' work\n"), 0o644)
	gitT(t, repo, "add", "upstream.txt")
	gitT(t, repo, "commit", "-q", "-m", "newer upstream")

	// Two attempts as claude makes them, each changing the file its own way.
	names := []string{"loom-aaaa1111", "loom-bbbb2222"}
	roots := make([]string, 2)
	for i, n := range names {
		wn := worktree.SessionName(strings.TrimPrefix(n, "loom-"))
		roots[i] = filepath.Join(repo, worktree.Dir, wn)
		gitT(t, repo, "worktree", "add", "-q", "-b", "worktree-"+wn, roots[i])
		st.Upsert(store.SessionRow{Name: n, Cwd: roots[i], CreatedAt: 1, EndedAt: -1})
	}
	os.WriteFile(filepath.Join(roots[0], "app.txt"), []byte("v1\nattempt one\n"), 0o644)
	gitT(t, roots[0], "commit", "-qam", "one")
	os.WriteFile(filepath.Join(roots[1], "app.txt"), []byte("v1\nattempt two\n"), 0o644)
	os.WriteFile(filepath.Join(roots[1], "extra.txt"), []byte("new file\n"), 0o644)
	// Attempt two is live: a pane stands in for its claude.
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	if err := tm.NewSession(names[1], roots[1], "sleep 60", 80, 24); err != nil {
		t.Fatal(err)
	}

	// The recorded base is older than where claude cut the branches (say,
	// from origin's newer default branch): main's later commit must not
	// count as the attempts' work.
	g := bestof.Group{ID: "g1", Repo: repo, Base: oldBase, Seed: "make it better", Created: 1, Sessions: names}
	bo.store.Put(g)
	if p := placeDTO(bo.places(), names[1]); p == nil || p.Index != 2 || p.Of != 2 || p.Group != "g1" {
		t.Fatalf("place %+v", p)
	}

	c, err := a.BestOfCompare("g1")
	if err != nil || len(c.Attempts) != 2 || c.Seed != "make it better" {
		t.Fatalf("compare: %+v %v", c, err)
	}
	one, two := c.Attempts[0], c.Attempts[1]
	if !strings.Contains(one.Diff.Patch, "+attempt one") || strings.Contains(one.Diff.Patch, "attempt two") || strings.Contains(one.Diff.Patch, "upstream") || one.Diff.Commits != 1 || one.Branch == "" {
		t.Fatalf("attempt one: %+v", one)
	}
	if !strings.Contains(two.Diff.Patch, "+attempt two") || !strings.Contains(two.Diff.Patch, "+new file") || strings.Contains(two.Diff.Patch, "upstream") || two.Diff.Commits != 0 {
		t.Fatalf("attempt two: %+v", two)
	}

	// A third attempt whose claude has not made its worktree yet.
	early := filepath.Join(repo, worktree.Dir, "loom-cccc3333")
	st.Upsert(store.SessionRow{Name: "loom-cccc3333", Cwd: early, CreatedAt: 1, EndedAt: -1})
	tm.NewSession("loom-cccc3333", repo, "sleep 60", 80, 24)
	g.Sessions = append(g.Sessions, "loom-cccc3333")
	bo.store.Put(g)

	if err := a.KeepBestOf("g1", "loom-nope"); err == nil {
		t.Fatal("kept a session outside the group")
	}
	if err := a.KeepBestOf("g1", names[0]); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if tm.HasSession(names[1]) {
		t.Fatal("the discarded attempt's session is still running")
	}
	if tm.HasSession("loom-cccc3333") {
		t.Fatal("an attempt kept away before its worktree existed is still running")
	}
	if _, err := os.Stat(roots[1]); !os.IsNotExist(err) {
		t.Fatal("the discarded attempt's worktree is still there")
	}
	gitT(t, repo, "rev-parse", "--verify", "worktree-"+filepath.Base(roots[1]))
	if b, _ := os.ReadFile(filepath.Join(roots[0], "app.txt")); string(b) != "v1\nattempt one\n" {
		t.Fatal("the kept attempt was touched")
	}
	if err := a.KeepBestOf("g1", names[1]); err == nil {
		t.Fatal("a second, different keep was allowed")
	}
	c, _ = a.BestOfCompare("g1")
	if c.Kept != names[0] || !c.Attempts[1].Gone || !c.Attempts[1].Discarded || c.Attempts[0].Gone {
		t.Fatalf("after keep: %+v", c)
	}
}
