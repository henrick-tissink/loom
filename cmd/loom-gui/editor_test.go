package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.go")
	if err := os.WriteFile(f, []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := resolveFile(dir, "x.go"); got != f {
		t.Errorf("relative: got %q want %q", got, f)
	}
	if got := resolveFile("/nowhere", f); got != f {
		t.Errorf("absolute: got %q want %q", got, f)
	}
	if got := resolveFile(dir, "missing.go"); got != "" {
		t.Errorf("missing should be empty, got %q", got)
	}
	if got := resolveFile(dir, ""); got != "" {
		t.Errorf("empty should be empty, got %q", got)
	}
	if got := resolveFile(dir, "."); got != "" {
		t.Errorf("dir should resolve empty (not a regular file), got %q", got)
	}
}

// TestResolveFileIn covers the multi-repo case (§5): a scoped session prints
// paths relative to whichever repo the agent was working in, so resolving
// against cwd alone loses every link into an add-dir sibling.
func TestResolveFileIn(t *testing.T) {
	primary, sibling := t.TempDir(), t.TempDir()
	only := filepath.Join(sibling, "only.go")
	for _, f := range []string{filepath.Join(primary, "both.go"), filepath.Join(sibling, "both.go"), only} {
		if err := os.WriteFile(f, []byte("package x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bases := []string{primary, sibling}

	if got, want := resolveFileIn(bases, "only.go"), only; got != want {
		t.Errorf("add-dir file: got %q want %q", got, want)
	}
	// cwd first: a same-named file in two repos resolves to the primary. This
	// is deterministic rather than right — the exact callers pass absolutes.
	if got, want := resolveFileIn(bases, "both.go"), filepath.Join(primary, "both.go"); got != want {
		t.Errorf("ambiguous file: got %q want %q", got, want)
	}
	if got := resolveFileIn(bases, "nope.go"); got != "" {
		t.Errorf("missing in every base should be empty, got %q", got)
	}
	// No session row (empty base set) must still open an absolute path.
	if got := resolveFileIn(nil, only); got != only {
		t.Errorf("no bases, absolute path: got %q want %q", got, only)
	}
	if got := resolveFileIn(nil, "only.go"); got != "" {
		t.Errorf("no bases, relative path: got %q want empty", got)
	}
}

func TestEditorArgv(t *testing.T) {
	if got := editorArgv("code", "/a/b.go", 88); len(got) != 3 || got[1] != "-g" || got[2] != "/a/b.go:88" {
		t.Errorf("code: %v", got)
	}
	if got := editorArgv("cursor", "/a/b.go", 0); got[2] != "/a/b.go" {
		t.Errorf("cursor no-line: %v", got)
	}
	if got := editorArgv("zed", "/a/b.go", 5); got[0] != "zed" || got[1] != "/a/b.go:5" {
		t.Errorf("zed: %v", got)
	}
	if got := editorArgv("whatever", "/a/b.go", 5); got[0] != "open" || got[1] != "/a/b.go" {
		t.Errorf("fallback: %v", got)
	}
}

func TestEditorApp(t *testing.T) {
	cases := map[string]string{
		"cursor":   "Cursor",
		"code":     "Visual Studio Code",
		"zed":      "Zed",
		"open":     "", // the `open` fallback already activates the default app
		"whatever": "",
	}
	for bin, want := range cases {
		if got := editorApp(bin); got != want {
			t.Errorf("editorApp(%q) = %q, want %q", bin, got, want)
		}
	}
}

func TestEditorCommands(t *testing.T) {
	// A GUI editor launched from another GUI app opens the file in the
	// background; we must follow up with `open -a <App>` to bring it to the
	// front. So cursor/code/zed yield TWO commands: open-at-line, then activate.
	got := editorCommands("cursor", "/a/b.go", 88)
	if len(got) != 2 {
		t.Fatalf("cursor: want 2 commands (open + activate), got %v", got)
	}
	if got[0][0] != "cursor" || got[0][len(got[0])-1] != "/a/b.go:88" {
		t.Errorf("cursor open cmd: %v", got[0])
	}
	if len(got[1]) != 3 || got[1][0] != "open" || got[1][1] != "-a" || got[1][2] != "Cursor" {
		t.Errorf("cursor activate cmd: %v", got[1])
	}

	if got := editorCommands("code", "/a/b.go", 0); len(got) != 2 || got[1][2] != "Visual Studio Code" {
		t.Errorf("code: want activate 'Visual Studio Code', got %v", got)
	}

	// The `open` fallback already raises the default app: a single command.
	if got := editorCommands("open", "/a/b.go", 0); len(got) != 1 || got[0][0] != "open" {
		t.Errorf("open fallback should be one command, got %v", got)
	}
}

func TestPickEditor(t *testing.T) {
	none := func(string) (string, error) { return "", errors.New("nope") }
	if got := pickEditor(none, ""); got != "open" {
		t.Errorf("no editors → want open, got %q", got)
	}
	all := func(string) (string, error) { return "/usr/bin/x", nil }
	// Auto: first of cursor/code/zed.
	if got := pickEditor(all, ""); got != "cursor" {
		t.Errorf("auto → want cursor, got %q", got)
	}
	// Preferred wins when installed.
	if got := pickEditor(all, "zed"); got != "zed" {
		t.Errorf("preferred zed → want zed, got %q", got)
	}
	// Preferred but NOT installed → fall back to auto.
	onlyCode := func(b string) (string, error) {
		if b == "code" {
			return "/usr/bin/code", nil
		}
		return "", errors.New("nope")
	}
	if got := pickEditor(onlyCode, "zed"); got != "code" {
		t.Errorf("preferred zed missing → want code, got %q", got)
	}
}
