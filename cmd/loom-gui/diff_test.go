package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
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

// TestSessionDiff_sectioned pins §8's shape: one section per directory the
// session was launched with, never one diff over row.Cwd alone. The old
// single-repo shape showed a scoped multi-repo session an authoritative-looking
// patch covering only the primary repo.
//
// Sections, not a concatenated patch: the frontend splits on
// /\n(?=diff --git )/, so a header injected between repos would be swallowed
// into the preceding file's hunk.
func TestSessionDiff_sectioned(t *testing.T) {
	primary, extra := t.TempDir(), t.TempDir()
	gitInit(t, primary)
	gitInit(t, extra)
	if err := os.WriteFile(filepath.Join(extra, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t)
	if err := a.st.Upsert(store.SessionRow{
		Name: "multi", Cwd: primary, AddDirs: session.EncodeAddDirs([]string{extra}),
		EndedAt: -1, ExitCode: -1,
	}); err != nil {
		t.Fatal(err)
	}

	got := a.SessionDiff("multi")
	if len(got.Repos) != 2 {
		t.Fatalf("want one section per directory, got %d: %+v", len(got.Repos), got.Repos)
	}
	if got.Repos[0].Path != primary || got.Repos[1].Path != extra {
		t.Fatalf("sections must follow cwd ∪ add_dirs order: %+v", got.Repos)
	}
	for _, r := range got.Repos {
		if r.Label == "" {
			t.Errorf("every section needs a heading: %+v", r)
		}
		if r.Error != "" {
			t.Errorf("git repo reported an error: %+v", r)
		}
	}
	if got.Repos[0].Dirty {
		t.Errorf("clean primary marked dirty: %+v", got.Repos[0])
	}
	// The add-dir's change is the one the old shape lost entirely.
	if !got.Repos[1].Dirty || len(got.Repos[1].Untracked) != 1 {
		t.Errorf("add-dir section missed its change: %+v", got.Repos[1])
	}
}

// A session with no recorded directory still yields one section carrying the
// error, so the panel renders a reason rather than an empty list.
func TestSessionDiff_noDirectory(t *testing.T) {
	a := newTestApp(t)
	got := a.SessionDiff("unknown")
	if len(got.Repos) != 1 || got.Repos[0].Error == "" {
		t.Fatalf("want one error section, got %+v", got.Repos)
	}
}
