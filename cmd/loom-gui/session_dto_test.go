package main

import (
	"testing"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/taskfile"
)

func TestSnapshotToDTOs_mapsFieldsAndStatus(t *testing.T) {
	snap := status.Snapshot{Live: []status.Row{
		{SessionRow: store.SessionRow{Name: "api-migration", ProjectLabel: "sauce-api"},
			Status: status.NeedsYou, Title: "Migrate the API"},
		{SessionRow: store.SessionRow{Name: "nested-discovery", ProjectLabel: "loom"},
			Status: status.Running, Title: ""},
	}}

	got := snapshotToDTOs(snap, nil)

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

func TestSnapshotToDTOs_taskAndAskFields(t *testing.T) {
	snap := status.Snapshot{Live: []status.Row{
		{SessionRow: store.SessionRow{Name: "a"}, Status: status.NeedsYou, Ask: "merge it?", Since: 50,
			HasTask: true, Task: taskfile.Task{Status: "open", Done: 2, Total: 3, Outcome: "the rail shows it"},
			Checkpoint: taskfile.Checkpoint{WrittenAt: 40, NextStep: "hand back"}},
		{SessionRow: store.SessionRow{Name: "b"}, Status: status.Running},
	}}
	got := snapshotToDTOs(snap, nil)
	if got[0].Ask != "merge it?" || got[0].Since != 50 || got[0].TaskStatus != "open" || got[0].TaskDone != 2 ||
		got[0].TaskTotal != 3 || got[0].TaskOutcome != "the rail shows it" || got[0].CheckpointAt != 40 || got[0].NextStep != "hand back" {
		t.Fatalf("row a: %+v", got[0])
	}
	if got[1].TaskStatus != "" {
		t.Fatalf("no task file must read as empty status: %+v", got[1])
	}
}

func TestSnapshotToDTOs_emptyIsNonNil(t *testing.T) {
	got := snapshotToDTOs(status.Snapshot{}, nil)
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
	// trFor stubs the indexed transcript by claude session id.
	trFor := func(id string) store.Transcript {
		if id == "cs-a" {
			return store.Transcript{LLMSummary: "Goal: x. Outcome: y.", Outcome: "y landed"}
		}
		return store.Transcript{}
	}
	rows[0].ClaudeSessionID = "cs-a"
	got := recentToDTOs(rows, trFor, nil)
	if len(got) != 2 {
		t.Fatalf("want 2 (live excluded), got %d", len(got))
	}
	if got[0].Status != "done" || got[0].Title != "done one" {
		t.Errorf("row a: %+v", got[0])
	}
	if got[0].Summary != "Goal: x. Outcome: y." || got[0].Outcome != "y landed" {
		t.Errorf("row a summary/outcome not attached: %+v", got[0])
	}
	if got[1].Status != "error" || got[1].Summary != "" {
		t.Errorf("row b should be error with no summary: %+v", got[1])
	}
}

func TestRecentToDTOs_emptyIsNonNil(t *testing.T) {
	if recentToDTOs(nil, nil, nil) == nil {
		t.Fatal("want non-nil empty slice")
	}
}
