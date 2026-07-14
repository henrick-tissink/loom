package main

import (
	"testing"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

func TestSnapshotToDTOs_mapsFieldsAndStatus(t *testing.T) {
	snap := status.Snapshot{Live: []status.Row{
		{SessionRow: store.SessionRow{Name: "api-migration", ProjectLabel: "sauce-api"},
			Status: status.NeedsYou, Title: "Migrate the API"},
		{SessionRow: store.SessionRow{Name: "nested-discovery", ProjectLabel: "loom"},
			Status: status.Running, Title: ""},
	}}

	got := snapshotToDTOs(snap)

	if len(got) != 2 {
		t.Fatalf("want 2 DTOs, got %d", len(got))
	}
	if got[0] != (SessionDTO{Name: "api-migration", Project: "sauce-api", Title: "Migrate the API", Status: "needs_you"}) {
		t.Errorf("row 0 mismatch: %+v", got[0])
	}
	if got[1].Status != "running" || got[1].Project != "loom" {
		t.Errorf("row 1 mismatch: %+v", got[1])
	}
}

func TestSnapshotToDTOs_emptyIsNonNil(t *testing.T) {
	got := snapshotToDTOs(status.Snapshot{})
	if got == nil {
		t.Fatal("want non-nil empty slice (marshals to [] not null)")
	}
	if len(got) != 0 {
		t.Fatalf("want 0, got %d", len(got))
	}
}

func TestRecentToDTOs_filtersLiveAndDerivesStatus(t *testing.T) {
	rows := []store.SessionRow{
		{Name: "a", ProjectLabel: "p", Title: "done one", EndedAt: 100, ExitCode: 0},
		{Name: "b", ProjectLabel: "p", EndedAt: 200, ExitCode: 1},
		{Name: "live", ProjectLabel: "p", EndedAt: -1, ExitCode: -1}, // still live → skipped
	}
	// summaryFor stubs the stored summary by claude session id.
	sumFor := func(id string) string {
		if id == "cs-a" {
			return "Goal: x. Outcome: y."
		}
		return ""
	}
	rows[0].ClaudeSessionID = "cs-a"
	got := recentToDTOs(rows, sumFor)
	if len(got) != 2 {
		t.Fatalf("want 2 (live excluded), got %d", len(got))
	}
	if got[0].Status != "done" || got[0].Title != "done one" {
		t.Errorf("row a: %+v", got[0])
	}
	if got[0].Summary != "Goal: x. Outcome: y." {
		t.Errorf("row a summary not attached: %+v", got[0])
	}
	if got[1].Status != "error" || got[1].Summary != "" {
		t.Errorf("row b should be error with no summary: %+v", got[1])
	}
}

func TestRecentToDTOs_emptyIsNonNil(t *testing.T) {
	if recentToDTOs(nil, nil) == nil {
		t.Fatal("want non-nil empty slice")
	}
}
