package main

import (
	"errors"
	"fmt"

	"github.com/henricktissink/loom/internal/delegate"
)

// Autonomy Slice 1 (autonomy spec §11): the App-side entry points that flag a
// run autonomous and stop it. The auto-runner loop itself lands in Task 2;
// startAutoRunner/stopAutoRunner are the seams it plugs into, no-ops until then.

// StartAutonomousRun creates a delegation run and hands it to the auto-runner.
//
// It reuses StartDelegationRun's ONE creation path verbatim — pin, snapshot,
// insert, §10.1 worktrees, first tick — then flags the run `auto` and starts
// the runner. The agent never spawns: this is loom code, invoked by a human
// (or, in Slice 2, by the orchestrator running `loom run start --auto`), and it
// only ever executes the manifest that already validated.
func (a *App) StartAutonomousRun(root, manifestName string) (int64, error) {
	res := a.StartDelegationRun(root, manifestName)
	if res.Hidden {
		// The bare marker never names the project; the caller gets a generic
		// refusal, matching every other hidden-gated binding.
		return 0, errors.New("project is hidden")
	}
	if res.RunID == 0 {
		// Nothing was written — a bad manifest, an unpinnable repo, no ~/.loom.
		return 0, errors.New(errText(nil, res.Error))
	}
	// The run exists. StartDelegationRun may still have reported a FAULT
	// (res.Error non-empty with a real RunID: §10.1 worktrees or the first
	// tick) — that is recorded on the run and surfaced in the control room; it
	// does not stop us flagging the run auto, since every one of those faults is
	// retried by the loop's own tick.
	if err := a.st.SetDelegationRunMode(res.RunID, string(delegate.RunModeAuto), a.now().Unix()); err != nil {
		return res.RunID, err
	}
	a.startAutoRunner(res.RunID)
	return res.RunID, nil
}

// AbortRun stops an autonomous run's loop before its next spawn/check/merge.
//
// It moves run_state to `aborted` via a CAS from any non-terminal control state
// — abort is the one control transition a human issues whatever the loop is
// doing (AbortDelegationRunStateCAS's contract). It deliberately does NOT touch
// the run's data `status`: an abort ends the automation, it does not rewrite
// what the run had already accomplished.
func (a *App) AbortRun(runID int64) error {
	if a.st == nil {
		return errors.New("delegation unavailable: no store")
	}
	claimed, err := a.st.AbortDelegationRunStateCAS(runID, a.now().Unix())
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("run %d is already stopped", runID)
	}
	a.stopAutoRunner(runID)
	return nil
}

// startAutoRunner launches the background loop for an autonomous run. Task 2
// gives it a body (the App owns a map[int64]*delegate.AutoRunner); until then
// starting a run flags it auto and records the intent, and the loop is a no-op.
func (a *App) startAutoRunner(runID int64) {}

// stopAutoRunner signals a running loop to exit. Paired with startAutoRunner;
// Task 2 fills it in. A no-op stop is safe — AbortRun has already moved the run
// to `aborted`, which the loop checks before every action.
func (a *App) stopAutoRunner(runID int64) {}
