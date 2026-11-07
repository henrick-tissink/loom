package overlap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repo makes a repository with one commit and a linked worktree on branch
// feat under .claude/worktrees/w, the shape `claude --worktree` leaves.
func repo(t *testing.T) (main, wt string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main = filepath.Join(base, "r")
	run(t, base, "git", "init", "-q", "-b", "main", "r")
	write(t, filepath.Join(main, "a.go"), "package a\n")
	write(t, filepath.Join(main, "b.go"), "package a\n")
	run(t, main, "git", "add", ".")
	run(t, main, "git", "commit", "-q", "-m", "init")
	wt = filepath.Join(main, ".claude", "worktrees", "w")
	run(t, main, "git", "worktree", "add", "-q", "-b", "feat", wt)
	return main, wt
}

func TestLocatorFindsCheckouts(t *testing.T) {
	main, wt := repo(t)
	l := NewLocator()
	c, ok := l.Find(filepath.Join(main, "sub", "deeper"))
	if !ok || c.Root != main || c.Repo != filepath.Join(main, ".git") || c.Linked {
		t.Fatalf("main checkout: %+v %v", c, ok)
	}
	w, ok := l.Find(filepath.Join(wt, "pkg"))
	if !ok || w.Root != wt || !w.Linked {
		t.Fatalf("linked worktree: %+v %v", w, ok)
	}
	if w.Repo != c.Repo {
		t.Fatalf("worktree repo %q, main repo %q: want the same repository", w.Repo, c.Repo)
	}
	if _, ok := l.Find(t.TempDir()); ok {
		t.Fatal("a directory in no repository found a checkout")
	}
	if _, ok := l.Find("relative/dir"); ok {
		t.Fatal("a relative directory found a checkout")
	}
	// cached: still answers after the .git entry is gone
	if err := os.RemoveAll(filepath.Join(main, ".git")); err != nil {
		t.Fatal(err)
	}
	if c2, ok := l.Find(filepath.Join(main, "sub", "deeper")); !ok || c2 != c {
		t.Fatalf("cached answer changed: %+v %v", c2, ok)
	}
}

func TestPendingDirtyAndBranch(t *testing.T) {
	main, wt := repo(t)
	ctx := context.Background()
	write(t, filepath.Join(main, "a.go"), "package a // edited\n")
	write(t, filepath.Join(main, "new", "n.go"), "package n\n")
	ch, err := Pending(ctx, main, false)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ch.Dirty)
	if !reflect.DeepEqual(ch.Dirty, []string{"a.go", "new/n.go"}) || ch.Branch != nil {
		t.Fatalf("main pending = %+v", ch)
	}

	write(t, filepath.Join(wt, "b.go"), "package a // feat\n")
	run(t, wt, "git", "commit", "-qam", "feat b")
	write(t, filepath.Join(wt, "c.go"), "package a\n")
	ch, err = Pending(ctx, wt, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ch.Dirty, []string{"c.go"}) || !reflect.DeepEqual(ch.Branch, []string{"b.go"}) {
		t.Fatalf("worktree pending = %+v", ch)
	}

	// pushed is not merged: the branch's own remote copy does not hide it
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, main, "git", "init", "-q", "--bare", remote)
	run(t, main, "git", "remote", "add", "origin", remote)
	run(t, wt, "git", "push", "-q", "origin", "feat")
	run(t, wt, "git", "fetch", "-q", "origin")
	if ch, _ = Pending(ctx, wt, true); !reflect.DeepEqual(ch.Branch, []string{"b.go"}) {
		t.Fatalf("after push, branch files = %v, want [b.go]", ch.Branch)
	}

	// merged is merged: once main has the commit, the branch has nothing of its own
	run(t, main, "git", "stash", "-q", "-u")
	run(t, main, "git", "merge", "-q", "--ff-only", "feat")
	if ch, _ = Pending(ctx, wt, true); len(ch.Branch) != 0 {
		t.Fatalf("after merge, branch files = %v, want none", ch.Branch)
	}
}

func TestFind(t *testing.T) {
	r1, r2 := "/r1/.git", "/r2/.git"
	got := Find([]Session{
		{Name: "a", Files: []File{{r1, "x.go"}, {r1, "y.go"}, {r1, "x.go"}}},
		{Name: "b", Files: []File{{r1, "x.go"}, {r1, "y.go"}, {r2, "z.go"}}},
		{Name: "c", Files: []File{{r1, "y.go"}}},
		{Name: "d", Files: []File{{r1, "z.go"}}}, // same path as b's z.go, other repository
		{Name: "e"},
	})
	want := map[string][]Hit{
		"a": {{With: "b", Files: []string{"x.go", "y.go"}}, {With: "c", Files: []string{"y.go"}}},
		"b": {{With: "a", Files: []string{"x.go", "y.go"}}, {With: "c", Files: []string{"y.go"}}},
		"c": {{With: "a", Files: []string{"y.go"}}, {With: "b", Files: []string{"y.go"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Find = %+v\nwant %+v", got, want)
	}
}
