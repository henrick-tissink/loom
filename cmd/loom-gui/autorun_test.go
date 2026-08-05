package main

import (
	"testing"

	"github.com/henricktissink/loom/internal/delegate"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

// TestChildDoneRow pins the child-done predicate over a persisted session row: a
// live session actively Running is not done; idle/needs-you and an ended session
// are done.
func TestChildDoneRow(t *testing.T) {
	cases := []struct {
		name string
		row  store.SessionRow
		want bool
	}{
		{"running", store.SessionRow{EndedAt: -1, LastStatus: string(status.Running)}, false},
		{"needs-you", store.SessionRow{EndedAt: -1, LastStatus: string(status.NeedsYou)}, true},
		{"idle", store.SessionRow{EndedAt: -1, LastStatus: string(status.Idle)}, true},
		{"ended", store.SessionRow{EndedAt: 2000, LastStatus: string(status.Running)}, true},
	}
	for _, c := range cases {
		if got := childDoneRow(c.row); got != c.want {
			t.Errorf("childDoneRow(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestChildDoneUnspawned: a task with no session name has no child yet, so it is
// not "done".
func TestChildDoneUnspawned(t *testing.T) {
	a := newTestApp(t)
	if a.childDone("") {
		t.Fatal("an unspawned task must not read as child-done")
	}
}

// TestDelegationRunDTOCarriesAutonomy: an autonomous run surfaces mode and
// run_state through the control-room DTO, so the frontend can render its state
// and offer a Kill.
func TestDelegationRunDTOCarriesAutonomy(t *testing.T) {
	a := newTestApp(t)
	root, _ := seedProjectWithManifest(t, a, "atlas", twoTaskManifest)
	id, err := a.StartAutonomousRun(root, "atlas")
	if err != nil {
		t.Fatalf("StartAutonomousRun: %v", err)
	}
	// Stop the loop so the read is deterministic; abort lands run_state=aborted.
	if err := a.AbortRun(id); err != nil {
		t.Fatalf("AbortRun: %v", err)
	}

	dto := a.ProjectDelegation(root)
	if dto.Hidden || len(dto.Runs) == 0 {
		t.Fatalf("no runs surfaced: %+v", dto)
	}
	run := dto.Runs[0]
	if run.Mode != string(delegate.RunModeAuto) {
		t.Fatalf("mode = %q, want auto", run.Mode)
	}
	if run.RunState != string(delegate.RunStateAborted) {
		t.Fatalf("runState = %q, want aborted", run.RunState)
	}
}

// TestAutoStateMapsDeadlockToEscalated: a run whose DATA status went
// `deadlocked` (a baseline fault or §12.1 deadlock) stops the loop as an
// escalation, and both axes end up agreeing.
func TestAutoStateMapsDeadlockToEscalated(t *testing.T) {
	a := newTestApp(t)
	run, err := a.st.InsertDelegationRun("atlas", "/w/x", `{"manifest":1,"name":"atlas"}`, "{}", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.AdvanceDelegationRunCAS(run.ID, "planning", "deadlocked", 1100); err != nil {
		t.Fatal(err)
	}

	st, err := a.autoState(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != delegate.RunStateEscalated {
		t.Fatalf("autoState = %q, want escalated", st)
	}
	got, _, _ := a.st.GetDelegationRun(run.ID)
	if got.State != string(delegate.RunStateEscalated) {
		t.Fatalf("run_state = %q, want escalated — the axes must agree", got.State)
	}
}

// TestStartAutonomousRunFlagsAndAborts is Autonomy Slice 1, Task 1: a run
// started autonomously is flagged auto/running, and AbortRun stops it —
// run_state=aborted — without abandoning the run's data status.
func TestStartAutonomousRunFlagsAndAborts(t *testing.T) {
	a := newTestApp(t)
	root, _ := seedProjectWithManifest(t, a, "atlas", twoTaskManifest)

	id, err := a.StartAutonomousRun(root, "atlas")
	if err != nil {
		t.Fatalf("StartAutonomousRun: %v", err)
	}
	run, ok, err := a.st.GetDelegationRun(id)
	if err != nil || !ok {
		t.Fatalf("GetDelegationRun(%d): ok=%v err=%v", id, ok, err)
	}
	if run.Mode != string(delegate.RunModeAuto) || run.State != string(delegate.RunStateRunning) {
		t.Fatalf("run mode/state = %q/%q, want auto/running", run.Mode, run.State)
	}

	if err := a.AbortRun(id); err != nil {
		t.Fatalf("AbortRun: %v", err)
	}
	run, _, _ = a.st.GetDelegationRun(id)
	if run.State != string(delegate.RunStateAborted) {
		t.Fatalf("run_state = %q, want aborted", run.State)
	}
}
