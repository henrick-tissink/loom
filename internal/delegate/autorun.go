package delegate

import (
	"fmt"
	"sort"
)

// This is the brain of the auto-runner (spec §11): given a read of every task in
// an autonomous run plus the concurrency cap, decide what to do THIS tick. It is
// a pure function so the entire autonomy decision is testable without a live
// tmux, a store, or a clock — the loop around it (autorun_loop, a later task)
// only executes what this returns.

// RunMode is delegation_runs.mode: whether a run is human-gated or autonomous.
// It is a distinct axis from the run's data status (planning|running|…) and
// from its RunState below — mode says WHO drives the run, RunState says what
// the auto-runner is doing right now.
type RunMode string

const (
	// RunModeSupervised is the default: every spawn is a human ApproveTask.
	RunModeSupervised RunMode = "supervised"
	// RunModeAuto hands the run to the auto-runner (spec §11). The agent still
	// never spawns; the auto-runner is loom code and only executes the already
	// validated manifest.
	RunModeAuto RunMode = "auto"
)

// RunState is delegation_runs.run_state: the auto-runner's control state for an
// autonomous run. Supervised runs sit at RunStateRunning and never leave it —
// this axis only moves under the auto-runner.
type RunState string

const (
	RunStateRunning   RunState = "running"   // the loop is driving the run
	RunStatePaused    RunState = "paused"    // temporarily halted, resumable
	RunStateEscalated RunState = "escalated" // stopped, waiting on a human decision
	RunStateAborted   RunState = "aborted"   // stopped for good by an abort
	RunStateDone      RunState = "done"      // every task terminal
)

// AutoAction is one thing the auto-runner does this tick.
type AutoAction interface{ isAutoAction() }

// SpawnTask launches a ready task's child (reusing the ApproveTask spawn core).
type SpawnTask struct{ ID string }

// CheckTask runs a done child's check.
type CheckTask struct{ ID string }

// IntegrateTask integrates + full-auto-merges a verified task on green gates.
type IntegrateTask struct{ ID string }

// Escalate pauses the run and pulls in the human, with a reason.
type Escalate struct{ Reason string }

// Done marks the run complete (every task terminal).
type Done struct{}

func (SpawnTask) isAutoAction()     {}
func (CheckTask) isAutoAction()     {}
func (IntegrateTask) isAutoAction() {}
func (Escalate) isAutoAction()      {}
func (Done) isAutoAction()          {}

// TaskView is the auto-runner's projection of one task: its row state plus the
// two facts only the live world knows — whether its child has finished working
// (from the status engine) and whether it has committed since it was last
// checked. Pure input to NextActions.
type TaskView struct {
	ID          string
	State       TaskState
	ChildIdle   bool  // child session is idle/ended (done working) per status.Snapshot
	CommittedAt int64 // last commit on the task's branch; 0 = none yet
	CheckedAt   int64 // last time its check ran; 0 = never
	Lapses      int   // failed-check count
}

// MaxCheckRetries is how many times a task's check may fail (each followed by a
// nudge to the child to fix it) before the run escalates to the human.
const MaxCheckRetries = 3

// countsAgainstCap reports whether a state occupies a worktree/child, i.e. holds
// a concurrency slot. Explicit (not derived from HoldsAChild) so the cap policy
// is stated here and this function stays self-contained.
func countsAgainstCap(s TaskState) bool {
	switch s {
	case StateApproved, StateSpawning, StateRunning, StateBlocked, StateChecking, StateIntegrating, StateMergeable:
		return true
	}
	return false
}

func isTerminal(s TaskState) bool { return s == StateMerged || s == StateAbandoned }

// NextActions decides the tick's work, deterministically (task-id order), purely
// from the task views and the cap. Precedence: a lapsed task escalates and stops
// the tick; otherwise integrate verified, check done children, and spawn ready
// up to the remaining cap; a fully-terminal run is Done; an all-blocked run
// deadlocks.
func NextActions(tasks []TaskView, cap int) []AutoAction {
	sorted := append([]TaskView(nil), tasks...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	// Done: a non-empty run whose every task is terminal.
	if len(sorted) > 0 {
		allTerminal := true
		for _, t := range sorted {
			if !isTerminal(t.State) {
				allTerminal = false
				break
			}
		}
		if allTerminal {
			return []AutoAction{Done{}}
		}
	}

	// Escalation on repeated check failure takes precedence and stops the tick.
	for _, t := range sorted {
		if t.Lapses >= MaxCheckRetries {
			return []AutoAction{Escalate{Reason: fmt.Sprintf("check failed %dx: %s", t.Lapses, t.ID)}}
		}
	}

	var acts []AutoAction

	// Integrate every verified task (full-auto-merge happens in the executor,
	// only on green gates).
	for _, t := range sorted {
		if t.State == StateVerified {
			acts = append(acts, IntegrateTask{ID: t.ID})
		}
	}

	// Check a running task whose child has finished working and has committed
	// since its last check.
	for _, t := range sorted {
		if t.State == StateRunning && t.ChildIdle && t.CommittedAt > t.CheckedAt {
			acts = append(acts, CheckTask{ID: t.ID})
		}
	}

	// Spawn ready tasks up to the remaining concurrency slots.
	live := 0
	for _, t := range sorted {
		if countsAgainstCap(t.State) {
			live++
		}
	}
	remaining := cap - live
	for _, t := range sorted {
		if remaining <= 0 {
			break
		}
		if t.State == StateReady {
			acts = append(acts, SpawnTask{ID: t.ID})
			remaining--
		}
	}

	// Deadlock: nothing to do, nothing live, nothing ready, yet something is
	// blocked — the run cannot make progress on its own.
	if len(acts) == 0 {
		blocked, ready, liveNow := false, false, false
		for _, t := range sorted {
			switch {
			case t.State == StateBlocked:
				blocked = true
			case t.State == StateReady:
				ready = true
			case countsAgainstCap(t.State):
				liveNow = true
			}
		}
		if blocked && !ready && !liveNow {
			return []AutoAction{Escalate{Reason: "deadlock: all remaining tasks are blocked"}}
		}
	}

	return acts
}
