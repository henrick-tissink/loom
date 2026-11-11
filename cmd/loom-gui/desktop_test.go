package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDesktopCommandReportsFailure(t *testing.T) {
	if err := desktopCommand("/does-not-exist/loom-opener"); err == nil {
		t.Fatal("missing opener succeeded")
	}
	if err := desktopCommand("sh", "-c", "exit 7"); err == nil {
		t.Fatal("failed opener succeeded")
	}
}
func TestDesktopCommandPreservesArguments(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	text := "space ' quote $ dollar % percent"
	if err := desktopCommand("sh", "-c", `printf '%s' "$1" > "$2"`, "sh", text, out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil || string(b) != text {
		t.Fatalf("%q %v", b, err)
	}
}

func TestDesktopCommandTimeout(t *testing.T) {
	if err := desktopCommand("sleep", "30"); err == nil {
		t.Fatal("hanging opener succeeded")
	}
}
