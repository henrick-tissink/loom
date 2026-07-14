package main

import (
	"regexp"
	"testing"

	"github.com/henricktissink/loom/internal/session"
)

func TestNewFanGroupID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{6}$`)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		g := newFanGroupID()
		if !re.MatchString(g) {
			t.Fatalf("group id %q is not 6 hex chars", g)
		}
		seen[g] = true
	}
	if len(seen) < 15 { // random: collisions should be rare
		t.Errorf("group ids not random enough: %d distinct of 20", len(seen))
	}
}

func TestFanout_NoLauncherOrProjects(t *testing.T) {
	// No launcher wired → error, never panics.
	a := &App{}
	if got := a.Fanout([]string{"/x"}, "", "", "hi"); got.Error == "" {
		t.Error("expected error when launcher is nil")
	}
	// Launcher present but empty selection → error before any launch.
	a2 := &App{launcher: &session.Launcher{}}
	if got := a2.Fanout(nil, "", "", "hi"); got.Error == "" {
		t.Error("expected error on empty project selection")
	}
}
