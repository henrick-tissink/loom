package cleanup

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestScanKeepsJunkSkipsEnvAndTracked(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.co",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.co")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init")
	write(".gitignore", "node_modules/\n*.log\n.env\n")
	write("node_modules/pkg/index.js", "x")
	write("debug.log", "log")
	write(".env", "SECRET=1")      // git-ignored but NOT junk — must be kept
	write("main.go", "package main") // tracked — must never be junk
	git("add", ".gitignore", "main.go")
	git("commit", "-m", "init")

	got, err := Scan([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.Rel] = true
	}
	if !seen["node_modules"] {
		t.Fatalf("node_modules should be junk; got %+v", got)
	}
	if !seen["debug.log"] {
		t.Fatalf("debug.log should be junk; got %+v", got)
	}
	if seen[".env"] {
		t.Fatal(".env is git-ignored but NOT junk — must not be removable")
	}
	if seen["main.go"] {
		t.Fatal("tracked file must never be junk")
	}

	// Remove only touches the confirmed set; a path outside it is refused.
	freed, err := Remove([]string{root}, []string{
		filepath.Join(root, "node_modules"),
		filepath.Join(root, "main.go"), // must be refused
	})
	if err != nil {
		t.Fatal(err)
	}
	if freed <= 0 {
		t.Fatal("freed should be > 0")
	}
	if _, e := os.Stat(filepath.Join(root, "node_modules")); !os.IsNotExist(e) {
		t.Fatal("node_modules should have been removed")
	}
	if _, e := os.Stat(filepath.Join(root, "main.go")); e != nil {
		t.Fatal("main.go must NOT have been removed")
	}
}
