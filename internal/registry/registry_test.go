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
