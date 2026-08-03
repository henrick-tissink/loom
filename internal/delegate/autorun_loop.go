package delegate

import (
	"context"
	"time"
)

// The auto-runner LOOP. NextActions (autorun.go) is the pure brain; this is the
// body around it — the one thing in autonomy that touches the clock, the store,
// tmux and git. Every one of those is an injected seam, so the loop itself is
// tested with fakes (autorun_loop_test.go drives a whole run to completion) and
// the GUI wires the real implementations (Tasks 3-5).
//
// One AutoRunner runs per autonomous run, in its own goroutine. Each iteration:
// read the control state (stop if aborted/terminal) → Tick (the existing state
// engine: block detection, ready-set promotion) → project the live world into
// []TaskView → NextActions → execute → sleep. Nothing here spawns or checks
// directly; it calls the executor seams, which reuse loom's existing primitives.

// AutoRunner drives one autonomous run. The zero value is not usable — every
// seam below must be set (the GUI's builder does that; tests set fakes).
type AutoRunner struct {
	RunID    int64
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
	// detection, park re-proposals, pending→ready promotion. Its error is
	// logged, not fatal — one bad tick must not kill an autonomous run.
	Tick func(ctx context.Context) error
	// Project builds the current []TaskView from the task rows, the status
	// engine (child-idle) and git (committed-since). The real projection is the
	// genuinely new signal Task 2's demo validates first.
	Project func() ([]TaskView, error)

	// The executor seams (Tasks 3-5). Each acts on ONE task and returns an
	// error only for a genuine failure to ACT; a no-op (cap race, already
	// claimed) is a nil error. A returned error is logged and the task is left
	// for the next tick — it does not stop the run.
	Spawn     func(taskID string) error
	Check     func(taskID string) error
	Integrate func(taskID string) error

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
// terminal control state (Done/Escalate). A single action's error escalates
// nothing on its own — it is logged and the task waits for the next tick, which
// is where NextActions' own escalation ceiling (repeated lapses, deadlock) fires
// instead.
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
				ar.log("spawn " + act.ID + ": " + err.Error())
			}
		case CheckTask:
			if err := ar.Check(act.ID); err != nil {
				ar.log("check " + act.ID + ": " + err.Error())
			}
		case IntegrateTask:
			if err := ar.Integrate(act.ID); err != nil {
				ar.log("integrate " + act.ID + ": " + err.Error())
			}
		}
	}
	return false
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
