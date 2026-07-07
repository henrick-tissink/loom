package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/henricktissink/loom/internal/transcript"
)

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	ccd := t.TempDir()
	mk := func(parts ...string) {
		if err := os.MkdirAll(filepath.Join(parts...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk(root, "gitproj", ".git") // include: has .git
	mk(root, "plaindir")        // exclude: nothing
	mk(root, ".hidden", ".git") // exclude: hidden
	mk(root, "clauded")         // include: has transcripts
	mk(ccd, "projects", transcript.ProjectDirName(filepath.Join(root, "clauded")))
	os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o644) // exclude: file

	ps, err := Discover(root, ccd)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Label != "clauded" || ps[1].Label != "gitproj" {
		t.Fatalf("Discover = %+v", ps)
	}
	if ps[1].Path != filepath.Join(root, "gitproj") {
		t.Fatalf("Path = %q", ps[1].Path)
	}
}

func TestDiscoverNested(t *testing.T) {
	root := t.TempDir()
	ccd := t.TempDir()
	mk := func(parts ...string) {
		if err := os.MkdirAll(filepath.Join(parts...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk(root, "group", "webrepo", ".git")      // include: repo one level inside a group dir
	mk(root, "group", "plainchild")           // exclude: no .git/transcripts
	mk(root, "group", ".hiddenchild", ".git") // exclude: hidden child
	mk(root, "group2", "clchild")             // include: child with transcripts only
	mk(ccd, "projects", transcript.ProjectDirName(filepath.Join(root, "group2", "clchild")))
	mk(root, "gitproj", ".git")
	mk(root, "gitproj", "nested", ".git") // exclude: project dirs are not descended into
	mk(root, "emptygroup", "plain")       // exclude: group with no project children

	ps, err := Discover(root, ccd)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gitproj", "group/webrepo", "group2/clchild"}
	if len(ps) != len(want) {
		t.Fatalf("Discover = %+v, want labels %v", ps, want)
	}
	for i, w := range want {
		if ps[i].Label != w {
			t.Fatalf("Label[%d] = %q, want %q (all: %+v)", i, ps[i].Label, w, ps)
		}
	}
	if ps[1].Path != filepath.Join(root, "group", "webrepo") {
		t.Fatalf("nested Path = %q", ps[1].Path)
	}
}
