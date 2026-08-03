package main

import (
	"testing"

	"github.com/henricktissink/loom/internal/delegate"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

// TestChildDone pins the child-done signal: a session actively Running is not
// done; idle/needs-you/ended are done; an unspawned task (no session) is not.
func TestChildDone(t *testing.T) {
	snap := status.Snapshot{Live: []status.Row{
		{SessionRow: store.SessionRow{Name: "working"}, Status: status.Running},
		{SessionRow: store.SessionRow{Name: "waiting"}, Status: status.NeedsYou},
		{SessionRow: store.SessionRow{Name: "resting"}, Status: status.Idle},
	}}
	cases := []struct {
		name string
		want bool
	}{
		{"working", false}, // still working
		{"waiting", true},  // needs you = stopped working
		{"resting", true},  // idle = stopped working
		{"ended", true},    // not live = ended
		{"", false},        // never spawned: no child yet
	}
	for _, c := range cases {
		if got := childDone(snap, c.name); got != c.want {
			t.Errorf("childDone(%q) = %v, want %v", c.name, got, c.want)
		}
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
