package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

// TestMigrationV5OnV4CopyDB guards the v5 migration (spec §2.4) running
// cleanly against a DB that has every v4 object but not yet workflow_runs —
// the "v4-copy DB" scenario.
func TestMigrationV5OnV4CopyDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loom.db")
	s, err := Open(p) // creates at latest version, including v5
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Roll the DB back to a v4-shape: drop the v5 object, roll user_version
	// back to 4.
	raw, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("DROP TABLE workflow_runs"); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 4"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(p)
	if err != nil {
		t.Fatalf("v5 migration on v4-copy DB failed: %v", err)
	}
	defer s2.Close()

	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 5 {
		t.Fatalf("user_version = %d, want 5", v)
	}
	if _, err := s2.InsertRun("wf", "{}", 100); err != nil {
		t.Fatalf("workflow_runs unusable after migration: %v", err)
	}
}

// TestMigrationV5Reentrant guards the same partial-apply scenario as
// TestMigrationsAreTransactional (store_test.go), for v5: workflow_runs
// already exists (e.g. a prior run got as far as the DDL before a crash)
// but user_version is stale at 4. IF NOT EXISTS + the per-migration
// transaction must make re-opening a no-op success, not an error.
func TestMigrationV5Reentrant(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loom.db")
	s, err := Open(p) // creates at latest version, including v5's objects
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 4"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(p)
	if err != nil {
		t.Fatalf("re-entrant v5 Open failed: %v", err)
	}
	defer s2.Close()

	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 5 {
		t.Fatalf("user_version = %d, want 5", v)
	}
	if _, err := s2.InsertRun("wf", "{}", 100); err != nil {
		t.Fatalf("workflow_runs unusable after re-entrant migration: %v", err)
	}
}

func TestInsertGetRunRoundtrip(t *testing.T) {
	s := open(t)
	id, err := s.InsertRun("plan-execute-review", `{"name":"plan-execute-review"}`, 1000)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetRun(id)
	if err != nil || !ok {
		t.Fatalf("GetRun: %v %v", ok, err)
	}
	want := RunRow{
		ID: id, Name: "plan-execute-review", DefJSON: `{"name":"plan-execute-review"}`,
		StepIdx: 0, SessionNames: []string{}, PendingSeed: "", Status: "running",
		CreatedAt: 1000, UpdatedAt: 1000,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestGetRunNotFound(t *testing.T) {
	s := open(t)
	_, ok, err := s.GetRun(999)
	if err != nil || ok {
		t.Fatalf("GetRun(999) = ok=%v err=%v, want ok=false, err=nil", ok, err)
	}
}

func TestActiveRunsFiltersStatusAndOrdersNewestFirst(t *testing.T) {
	s := open(t)
	id1, _ := s.InsertRun("a", "{}", 1000)
	id2, _ := s.InsertRun("b", "{}", 2000)
	id3, _ := s.InsertRun("c", "{}", 3000)
	if err := s.SetRunStatus(id2, "done", 4000); err != nil {
		t.Fatal(err)
	}

	active, err := s.ActiveRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("ActiveRuns = %d rows, want 2: %+v", len(active), active)
	}
	if active[0].ID != id3 || active[1].ID != id1 {
		t.Fatalf("ActiveRuns order = [%d,%d], want newest first [%d,%d]",
			active[0].ID, active[1].ID, id3, id1)
	}
}

func TestSetRunStatusDoneAndAbandonedExcludeFromActiveRuns(t *testing.T) {
	s := open(t)
	id1, _ := s.InsertRun("a", "{}", 1000)
	id2, _ := s.InsertRun("b", "{}", 2000)
	id3, _ := s.InsertRun("c", "{}", 3000)
	if err := s.SetRunStatus(id1, "done", 4000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunStatus(id2, "abandoned", 5000); err != nil {
		t.Fatal(err)
	}

	active, err := s.ActiveRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != id3 {
		t.Fatalf("ActiveRuns = %+v, want only id3=%d", active, id3)
	}

	got1, _, _ := s.GetRun(id1)
	if got1.Status != "done" || got1.UpdatedAt != 4000 {
		t.Fatalf("run1 = %+v, want status=done updated_at=4000", got1)
	}
	got2, _, _ := s.GetRun(id2)
	if got2.Status != "abandoned" || got2.UpdatedAt != 5000 {
		t.Fatalf("run2 = %+v, want status=abandoned updated_at=5000", got2)
	}
}

func TestSessionNamesJSONArrayIntegrityIncludingEmpty(t *testing.T) {
	s := open(t)
	id, _ := s.InsertRun("wf", "{}", 1000)

	got, _, _ := s.GetRun(id)
	if got.SessionNames == nil || len(got.SessionNames) != 0 {
		t.Fatalf("SessionNames on fresh insert = %#v, want empty non-nil slice", got.SessionNames)
	}
	var raw string
	if err := s.db.QueryRow("SELECT session_names FROM workflow_runs WHERE id=?", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "[]" {
		t.Fatalf("session_names column on insert = %q, want literal '[]' (never NULL/'')", raw)
	}

	claimed, err := s.AdvanceRunCAS(id, 0, 0, []string{"loom-step1"}, "", 2000)
	if err != nil || !claimed {
		t.Fatalf("AdvanceRunCAS: claimed=%v err=%v", claimed, err)
	}
	got, _, _ = s.GetRun(id)
	if !reflect.DeepEqual(got.SessionNames, []string{"loom-step1"}) {
		t.Fatalf("SessionNames after advance = %#v, want [loom-step1]", got.SessionNames)
	}
	if err := s.db.QueryRow("SELECT session_names FROM workflow_runs WHERE id=?", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != `["loom-step1"]` {
		t.Fatalf("session_names column after advance = %q", raw)
	}
}

// TestAdvanceRunCASSameSnapshotSecondCallerRejected is THE CAS proof (spec
// §2.6): two callers both act on expectedStepIdx=0 (the same snapshot), one
// racing the other. The first wins; the second — even though it passes the
// SAME expectedStepIdx it read before the first call landed — must get
// claimed=false, and the row must come back byte-identical to the
// post-first-call state (proving the rejected write touched nothing, not
// even updated_at).
func TestAdvanceRunCASSameSnapshotSecondCallerRejected(t *testing.T) {
	s := open(t)
	id, _ := s.InsertRun("wf", "{}", 1000)

	claimed1, err := s.AdvanceRunCAS(id, 0, 1, []string{"loom-step1", "loom-step2"}, "seed-2", 2000)
	if err != nil || !claimed1 {
		t.Fatalf("first AdvanceRunCAS: claimed=%v err=%v", claimed1, err)
	}
	after1, _, err := s.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}

	// Second caller acted on the SAME snapshot (expectedStepIdx=0), but the
	// stored step_idx is now 1 — this must be rejected.
	claimed2, err := s.AdvanceRunCAS(id, 0, 1, []string{"loom-step1", "loom-step2-other"}, "seed-other", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if claimed2 {
		t.Fatal("second AdvanceRunCAS from a stale snapshot: claimed=true, want false")
	}

	after2, _, err := s.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after1, after2) {
		t.Fatalf("row mutated by a rejected CAS: before=%+v after=%+v", after1, after2)
	}
}

func TestClearPendingSeed(t *testing.T) {
	s := open(t)
	id, _ := s.InsertRun("wf", "{}", 1000)
	claimed, err := s.AdvanceRunCAS(id, 0, 0, []string{"loom-step1"}, "pending seed text", 2000)
	if err != nil || !claimed {
		t.Fatalf("AdvanceRunCAS: claimed=%v err=%v", claimed, err)
	}
	got, _, _ := s.GetRun(id)
	if got.PendingSeed != "pending seed text" {
		t.Fatalf("PendingSeed = %q, want %q", got.PendingSeed, "pending seed text")
	}

	if err := s.ClearPendingSeed(id, 3000); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetRun(id)
	if got.PendingSeed != "" {
		t.Fatalf("PendingSeed after clear = %q, want empty", got.PendingSeed)
	}
	if got.UpdatedAt != 3000 {
		t.Fatalf("UpdatedAt after clear = %d, want 3000", got.UpdatedAt)
	}
}
