package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-q", "-m", "base"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestGitDiff_NonRepo(t *testing.T) {
	if got := gitDiff(t.TempDir()); got.Error == "" {
		t.Errorf("non-repo should report an error, got %+v", got)
	}
	if got := gitDiff(""); got.Error == "" {
		t.Error("empty cwd should report an error")
	}
}

func TestGitDiff_CleanAndDirty(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)

	// Clean tree: no error, not dirty.
	if got := gitDiff(dir); got.Error != "" || got.Dirty {
		t.Fatalf("clean repo: %+v", got)
	}

	// Tracked change → shows in stat/patch and is dirty.
	tracked := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(tracked, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec.Command("git", "-C", dir, "add", "a.txt").Run()
	exec.Command("git", "-C", dir, "commit", "-q", "-m", "add a").Run()
	if err := os.WriteFile(tracked, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Untracked new file.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := gitDiff(dir)
	if got.Error != "" || !got.Dirty {
		t.Fatalf("dirty repo: %+v", got)
	}
	if got.Patch == "" || got.Stat == "" {
		t.Errorf("expected stat+patch for tracked change, got %+v", got)
	}
	if len(got.Untracked) != 1 || got.Untracked[0] != "new.txt" {
		t.Errorf("untracked = %v, want [new.txt]", got.Untracked)
	}
}
