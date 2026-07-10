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
