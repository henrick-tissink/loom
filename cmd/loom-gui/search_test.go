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
	got := searchHitsToDTOs(hits, nil)
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
	if searchHitsToDTOs(nil, nil) == nil {
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

// §5: a multi-repo session resumed FROM SEARCH must keep its add-dirs. The
// search hit carries only a conversation id and a cwd, so the row has to be
// looked up rather than synthesised.
func TestResumeRow_prefersStoredRowForAddDirs(t *testing.T) {
	app := newTestApp(t)
	stored := store.SessionRow{
		Name: "loom-multi", ClaudeSessionID: "conv-1", ProjectLabel: "innostream/bankenstein",
		Cwd: "/w/bankenstein", AddDirs: `["/w/ballista"]`,
		CreatedAt: 10, EndedAt: 20, ExitCode: 0, LastStatus: "done",
	}
	if err := app.st.Upsert(stored); err != nil {
		t.Fatal(err)
	}
	noCwd := store.SessionRow{
		Name: "loom-nocwd", ClaudeSessionID: "conv-nocwd",
		CreatedAt: 10, EndedAt: 20, ExitCode: 0, LastStatus: "done",
	}
	if err := app.st.Upsert(noCwd); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		st          *store.Store
		id, cwd     string
		wantCwd     string
		wantAddDirs string
		wantLabel   string
	}{
		{
			name: "stored row wins", st: app.st, id: "conv-1", cwd: "/w/bankenstein",
			wantCwd: "/w/bankenstein", wantAddDirs: `["/w/ballista"]`,
			wantLabel: "innostream/bankenstein",
		},
		{
			// Transcripts outlive sessions rows, so an unmatched hit still resumes.
			name: "no row falls back to the hit", st: app.st, id: "conv-gone", cwd: "/w/loom",
			wantCwd: "/w/loom", wantLabel: "loom",
		},
		{
			// Resume refuses an empty cwd; the hit's cwd is the better answer.
			name: "stored row without cwd is rejected", st: app.st, id: "conv-nocwd", cwd: "/w/loom",
			wantCwd: "/w/loom", wantLabel: "loom",
		},
		{
			name: "nil store degrades", st: nil, id: "conv-1", cwd: "/w/loom",
			wantCwd: "/w/loom", wantLabel: "loom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resumeRow(tc.st, tc.id, tc.cwd)
			if got.ClaudeSessionID != tc.id {
				t.Errorf("id = %q, want %q", got.ClaudeSessionID, tc.id)
			}
			if got.Cwd != tc.wantCwd {
				t.Errorf("cwd = %q, want %q", got.Cwd, tc.wantCwd)
			}
			if got.AddDirs != tc.wantAddDirs {
				t.Errorf("addDirs = %q, want %q", got.AddDirs, tc.wantAddDirs)
			}
			if got.ProjectLabel != tc.wantLabel {
				t.Errorf("label = %q, want %q", got.ProjectLabel, tc.wantLabel)
			}
		})
	}
}
