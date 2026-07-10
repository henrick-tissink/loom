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

func TestPickEditor(t *testing.T) {
	none := func(string) (string, error) { return "", errors.New("nope") }
	if got := pickEditor(none); got != "open" {
		t.Errorf("no editors → want open, got %q", got)
	}
	onlyCode := func(b string) (string, error) {
		if b == "code" {
			return "/usr/bin/code", nil
		}
		return "", errors.New("nope")
	}
	if got := pickEditor(onlyCode); got != "code" {
		t.Errorf("want code, got %q", got)
	}
}
