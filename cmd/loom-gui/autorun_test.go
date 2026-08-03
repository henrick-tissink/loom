package main

import (
	"testing"

	"github.com/henricktissink/loom/internal/delegate"
)

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
