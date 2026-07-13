package main

import (
	"os"
	"strings"
	"testing"
)

func TestBuildPATH_addsMissingWellKnownDirs(t *testing.T) {
	// A Finder-like minimal PATH, missing Homebrew and ~/.local/bin.
	got := buildPATH("/usr/bin:/bin", "", "/Users/x")
	dirs := strings.Split(got, string(os.PathListSeparator))
	set := map[string]bool{}
	for _, d := range dirs {
		set[d] = true
	}
	for _, want := range []string{"/usr/bin", "/bin", "/opt/homebrew/bin", "/usr/local/bin", "/Users/x/.local/bin", "/Users/x/go/bin"} {
		if !set[want] {
			t.Errorf("expected PATH to contain %q; got %q", want, got)
		}
	}
}

func TestBuildPATH_dedupesAndPreservesOrder(t *testing.T) {
	got := buildPATH("/opt/homebrew/bin:/usr/bin", "/usr/bin:/other", "/Users/x")
	dirs := strings.Split(got, string(os.PathListSeparator))
	// First occurrence order: existing first, then new shell dir, no dupes.
	if dirs[0] != "/opt/homebrew/bin" || dirs[1] != "/usr/bin" {
		t.Fatalf("order not preserved: %v", dirs)
	}
	count := 0
	for _, d := range dirs {
		if d == "/usr/bin" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("/usr/bin duplicated %d times: %v", count, dirs)
	}
	if !strings.Contains(got, "/other") {
		t.Errorf("shell-only dir /other missing: %q", got)
	}
}

func TestHydrateLocale_defaultsWhenUnset(t *testing.T) {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	hydrateLocale()
	if got := os.Getenv("LC_CTYPE"); got != "UTF-8" {
		t.Fatalf("LC_CTYPE = %q, want UTF-8", got)
	}
}

func TestHydrateLocale_preservesExisting(t *testing.T) {
	os.Unsetenv("LC_ALL")
	os.Unsetenv("LC_CTYPE")
	t.Setenv("LANG", "en_US.UTF-8")
	hydrateLocale()
	if got := os.Getenv("LC_CTYPE"); got != "" {
		t.Fatalf("LC_CTYPE = %q, want unset (LANG already set)", got)
	}
}
