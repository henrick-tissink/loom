package main

import (
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

func TestSearchHitsToDTOs(t *testing.T) {
	hits := []store.SearchHit{
		{SessionID: "sid1", Title: "Fix walker", Cwd: "/Users/x/Sauce/loom", Ask: "fix the walker", Snippet: "line\nwith \x01match\x02 here"},
	}
	got := searchHitsToDTOs(hits)
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].Project != "loom" {
		t.Errorf("project basename: %q", got[0].Project)
	}
	if got[0].Snippet != "line with \x01match\x02 here" {
		t.Errorf("snippet flatten (markers preserved): %q", got[0].Snippet)
	}
}

func TestSearchHitsToDTOs_emptyNonNil(t *testing.T) {
	if searchHitsToDTOs(nil) == nil {
		t.Fatal("want non-nil empty slice")
	}
}

func TestApp_SearchSessions_nilStoreNonNil(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if app.SearchSessions("x") == nil {
		t.Fatal("SearchSessions must return non-nil (marshals to [])")
	}
}

func TestApp_ResumeSearchHit_guards(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if _, err := app.ResumeSearchHit("sid", "/x"); err == nil {
		t.Fatal("nil launcher must error")
	}
}
