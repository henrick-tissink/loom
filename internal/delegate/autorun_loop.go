package delegate

import (
	"context"
	"errors"
	"time"
)

// The auto-runner LOOP. NextActions (autorun.go) is the pure brain; this is the
// body around it — the one thing in autonomy that touches the clock, the store,
// tmux and git. Every one of those is an injected seam, so the loop itself is
// tested with fakes (autorun_loop_test.go drives a whole run to completion) and
// the GUI wires the real implementations.
//
// One AutoRunner runs per autonomous run, in its own goroutine. Each iteration:
// read the control state (stop if aborted/terminal) → Tick (which itself checks,
// integrates and promotes) → project the live world into []TaskView →
// NextActions → execute → sleep. The only executors are Spawn and Merge — the
// two steps Tick leaves undone — plus the loop's own escalate/done handling.

// AutoRunner drives one autonomous run. The zero value is not usable — every
// seam below must be set (the GUI's builder does that; tests set fakes).
type AutoRunner struct {
	Cap      int
	Interval time.Duration // between ticks; tests set it tiny

	// State reads the run's current control state each iteration, so an abort
	// (or a done/escalated set by a previous iteration or another instance)
	// stops the loop before it acts.
	State func() (RunState, error)
	// Advance is the control-state CAS (running→done, running→escalated). The
	// loop only ever transitions FROM running, so a lost claim means someone
	// else moved the run and the loop should stop without acting.
	Advance func(from, to RunState) (bool, error)
	// RecordEscalation persists WHY a run escalated, beside setting the state.
	// Called before Advance(→escalated) so the reason is durable first.
	RecordEscalation func(reason string) error

	// Tick runs the existing scheduler pass (delegate.Runner.Tick): block
	// detection, park re-proposals, pending→ready promotion, CHECKS, and
	// INTEGRATION. Its error is logged, not fatal — one bad tick must not kill
	// an autonomous run.
	Tick func(ctx context.Context) error
	// Project builds the current []TaskView from the task rows, the persisted
	// session status (child-idle), git (committed-since) and the recorded check
	// verdict.
	Project func() ([]TaskView, error)

	// Spawn launches a ready/approved task's child; Merge full-auto-merges a
	// mergeable task. Each returns the raw error; execute() classifies it. A
	// TRANSIENT error (isTransient — a lost race, another integration in flight,
	// the branch moved under a certify, a cancelled context) is logged and
	// retried next tick; anything else is genuine trouble (a real spawn failure,
	// a divergence/drift/conflict a human must acknowledge) and escalates the run.
	Spawn func(taskID string) error
	Merge func(taskID string) error

	// Sleep waits between iterations, cancellable. Injected so tests neither
	// wall-clock-wait nor need a real timer. Defaults to a ctx-aware sleep.
	Sleep func(ctx context.Context, d time.Duration)
	// Log surfaces one progress line. Optional; nil discards.
	Log func(string)
}

// Run drives the loop until the run reaches a terminal control state, the
// context is cancelled, or a seam returns a hard error. It returns nil on an
// ordinary stop (aborted/done/escalated) and the context's error on cancel.
func (ar *AutoRunner) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := ar.State()
		if err != nil {
			// The one place a read error IS fatal: without the control state the
			// loop cannot honour an abort, and a runner that cannot be stopped is
			// worse than one that stops.
			return err
		}
		switch state {
		case RunStateAborted, RunStateDone, RunStateEscalated:
			// Someone (an abort, a prior iteration, another instance) already
			// stopped the run. Nothing to do and nothing to record.
			return nil
		case RunStatePaused:
			// Halted but resumable: don't act, but keep the loop alive so a
			// resume is picked up. Sleep and re-check.
			ar.sleep(ctx)
			continue
		}

		if ar.Tick != nil {
			if err := ar.Tick(ctx); err != nil {
				ar.log("tick: " + err.Error())
			}
		}

		views, err := ar.Project()
		if err != nil {
			// A projection failure means this tick decides on stale data; skip
			// acting and try again rather than acting blind.
			ar.log("project: " + err.Error())
			ar.sleep(ctx)
			continue
		}

		stop := ar.execute(ctx, NextActions(views, ar.Cap))
		if stop {
			return nil
		}
		ar.sleep(ctx)
	}
}

// execute runs one tick's actions in order and reports whether the run reached a
// terminal control state (Done/Escalate). Spawn and Merge are classified the same
// way: a TRANSIENT error is logged and the task retried next tick; a
// non-transient one is trouble a human must see, so it escalates the run with the
// action's own reason. This symmetry is the fix for two review findings — a
// permanent spawn failure must NOT spin forever silently, and a merge that
// refuses only because the branch moved or another integration is in flight must
// NOT permanently halt the run.
func (ar *AutoRunner) execute(ctx context.Context, acts []AutoAction) (stop bool) {
	for _, a := range acts {
		if err := ctx.Err(); err != nil {
			return true
		}
		switch act := a.(type) {
		case Done:
			ar.finish(RunStateDone, "")
			return true
		case Escalate:
			ar.finish(RunStateEscalated, act.Reason)
			return true
		case SpawnTask:
			if err := ar.Spawn(act.ID); err != nil {
				if isTransient(err) {
					ar.log("spawn " + act.ID + " (transient, will retry): " + err.Error())
				} else {
					ar.finish(RunStateEscalated, "spawn failed for "+act.ID+": "+err.Error())
					return true
				}
			}
		case MergeTask:
			if err := ar.Merge(act.ID); err != nil {
				if isTransient(err) {
					ar.log("merge " + act.ID + " (transient, will retry): " + err.Error())
				} else {
					ar.finish(RunStateEscalated, "auto-merge refused for "+act.ID+": "+err.Error())
					return true
				}
			}
		}
	}
	return false
}

// isTransient reports whether an executor error is a routine, self-clearing
// condition the loop should retry next tick rather than escalate on. Each is a
// state the codebase already documents as "not a failure — next tick": a lost
// CAS race (the row moved), another integration holding the run-wide lock, the
// task branch moving past a certified commit (the child pushed a fix and Tick
// will re-check it), or a cancelled/expired context (an abort or shutdown, which
// the loop's own state check handles). Everything else — a real spawn failure, a
// divergence/drift/conflict a human must acknowledge — is genuine trouble.
func isTransient(err error) bool {
	return errors.Is(err, ErrTaskMovedElsewhere) ||
		errors.Is(err, ErrIntegrationBusy) ||
		errors.Is(err, ErrBranchMoved) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// finish records an escalation reason (if any) and CASes the run to its terminal
// control state. Reason first so it is durable before the state that points a
// human at it; a lost CAS means another writer already stopped the run.
func (ar *AutoRunner) finish(to RunState, reason string) {
	if to == RunStateEscalated && reason != "" && ar.RecordEscalation != nil {
		if err := ar.RecordEscalation(reason); err != nil {
			ar.log("record escalation: " + err.Error())
		}
	}
	if _, err := ar.Advance(RunStateRunning, to); err != nil {
		ar.log("advance to " + string(to) + ": " + err.Error())
	}
}

func (ar *AutoRunner) sleep(ctx context.Context) {
	if ar.Sleep != nil {
		ar.Sleep(ctx, ar.Interval)
		return
	}
	t := time.NewTimer(ar.Interval)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (ar *AutoRunner) log(msg string) {
	if ar.Log != nil {
		ar.Log(msg)
	}
}
