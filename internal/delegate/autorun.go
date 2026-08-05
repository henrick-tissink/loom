package delegate

import (
	"fmt"
	"sort"
	"strings"
)

// WorktreeHeadTime returns the committer time (unix seconds) of a worktree's
// HEAD commit, or 0 when the worktree has no commit yet (an unborn HEAD, or a
// directory git cannot read). It is the auto-runner's "has the child committed"
// signal — a TaskView's CommittedAt — and 0 is the honest answer for "nothing
// committed", which the check-on-done rule reads as "no new work to check".
func WorktreeHeadTime(dir string) int64 {
	out, err := gitOut(dir, "log", "-1", "--format=%ct")
	if err != nil {
		return 0
	}
	var t int64
	if _, err := fmt.Sscan(strings.TrimSpace(out), &t); err != nil {
		return 0
	}
	return t
}

// This is the brain of the auto-runner (spec §11): given a read of every task in
// an autonomous run plus the concurrency cap, decide what to do THIS tick. It is
// a pure function so the entire autonomy decision is testable without a live
// tmux, a store, or a clock — the loop around it (autorun_loop) only executes
// what this returns.
//
// DIVISION OF LABOUR — the load-bearing fact this whole file is built on. The
// loop calls Runner.Tick BEFORE consulting NextActions every iteration, and Tick
// ALREADY drives most of the pipeline itself: it promotes pending→ready, CHECKS
// running tasks whose branch head moved (green→verified, red→back to running),
// re-checks blocked tasks, INTEGRATES verified tasks (verified→mergeable), and
// resumes parked tasks. So the auto-runner does NOT check or integrate — Tick
// does. It adds only the three things Tick never does:
//
//   1. SPAWN ready/approved tasks (Tick promotes to `ready` but never launches);
//   2. MERGE mergeable tasks (Tick integrates to `mergeable` but never merges —
//      that is §5.2's human gate, which the auto-runner performs on green);
//   3. ESCALATE when the run is genuinely stuck (a red check on an idle child
//      that will not fix itself, or an all-blocked deadlock).
//
// An earlier revision had NextActions also emit Check and Integrate actions; it
// was wrong because Tick front-runs both, so those actions never fired on their
// intended states and a `mergeable` task (Tick's own output) was misread as a
// refused merge. This division is the fix.

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

// SpawnTask launches a ready/approved task's child (via Runner.Approve, which
// computes the §9.2 producer-merged base). Tick never spawns.
type SpawnTask struct{ ID string }

// MergeTask full-auto-merges a `mergeable` task — the one step Tick leaves to
// §5.2's gate. The executor merges only on clean gates; a refusal escalates.
type MergeTask struct{ ID string }

// Escalate pauses the run and pulls in the human, with a reason.
type Escalate struct{ Reason string }

// Done marks the run complete (every task terminal).
type Done struct{}

func (SpawnTask) isAutoAction() {}
func (MergeTask) isAutoAction() {}
func (Escalate) isAutoAction()  {}
func (Done) isAutoAction()      {}

// TaskView is the auto-runner's projection of one task: its row state plus the
// live facts NextActions needs — whether the child has stopped working, when it
// last committed, when it was last checked, and that check's verdict. Pure input
// to NextActions; the App builds it from the row + the persisted session status
// + git.
type TaskView struct {
	ID          string
	State       TaskState
	ChildIdle   bool   // child session is idle/ended (stopped working)
	CommittedAt int64  // committer time of the branch HEAD; 0 = nothing committed
	CheckedAt   int64  // when the check last ran (row.CheckAt); 0 = never
	CheckStatus string // that check's verdict (row.CheckStatus): ""|pass|fail|unpublished|infra-error
}

// checkIsRed reports whether a check has run and did not pass. A pass moves a
// task to `verified` (so a `running` task never carries `pass`), and "" means no
// check has run yet — neither is a reason to escalate.
func checkIsRed(status string) bool {
	return status != "" && status != string(CheckPass)
}

// countsAgainstCap reports whether a state holds a concurrency slot — a live or
// imminent child. It deliberately MATCHES the set the store's ClaimTaskSpawnCAS
// counts (spawning/running/blocked/checking) plus `approved` (a claimed,
// about-to-spawn slot), so NextActions never offers a spawn the store's own cap
// would refuse. Post-child states (verified/integrating/mergeable) do NOT count:
// their child has finished and they are on the merge path, so they must not
// block a fresh task from spawning.
func countsAgainstCap(s TaskState) bool {
	switch s {
	case StateApproved, StateSpawning, StateRunning, StateBlocked, StateChecking:
		return true
	}
	return false
}

func isTerminal(s TaskState) bool { return s == StateMerged || s == StateAbandoned }

// NextActions decides the tick's work, deterministically (task-id order), purely
// from the task views and the cap. Precedence: a fully-terminal run is Done; a
// red-checked idle child that will not fix itself escalates and stops the tick;
// otherwise merge every mergeable task and spawn ready/approved up to the
// remaining cap; an all-blocked remainder is a deadlock.
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

	// Escalate a genuinely failed check. Tick lands a red check back in `running`
	// (never a `failed` state) and re-runs it only when the branch head moves; so
	// a `running` task whose check is red, whose child has STOPPED, and which has
	// not committed anything since that check, will never turn green on its own —
	// a human is needed. The idle guard is what distinguishes this from a child
	// still working on a fix (which will commit and trigger Tick's re-check).
	// This stops the tick: an escalation pauses the whole run.
	for _, t := range sorted {
		if t.State == StateRunning && t.ChildIdle && checkIsRed(t.CheckStatus) && t.CommittedAt <= t.CheckedAt {
			return []AutoAction{Escalate{Reason: fmt.Sprintf("check failed for %s (%s) and its child is idle with no new work", t.ID, t.CheckStatus)}}
		}
	}

	var acts []AutoAction

	// Merge every `mergeable` task. Tick integrated it to `mergeable` (all §10.2
	// gates green) but never merges — that is §5.2's human gate, which the
	// auto-runner performs. The executor merges only when nothing needs a human
	// acknowledgement (divergence/drift/conflict); a refusal escalates there.
	for _, t := range sorted {
		if t.State == StateMergeable {
			acts = append(acts, MergeTask{ID: t.ID})
		}
	}

	// Spawn ready and approved tasks up to the remaining concurrency slots. Both
	// go through the same executor (Runner.Approve, which is idempotent for an
	// already-`approved` task from a prior tick's partial spawn), so `approved`
	// is not a stranded state.
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
		if t.State == StateReady || t.State == StateApproved {
			acts = append(acts, SpawnTask{ID: t.ID})
			remaining--
		}
	}

	// Deadlock: no action this tick and EVERY non-terminal task is blocked. A
	// task in a transient state Tick advances (verified/integrating/checking/…)
	// is not a deadlock — Tick moves it next iteration — so the guard is the
	// strict "nothing left but blocked tasks", not "some task is blocked".
	if len(acts) == 0 {
		anyNonTerminal, allBlocked := false, true
		for _, t := range sorted {
			if isTerminal(t.State) {
				continue
			}
			anyNonTerminal = true
			if t.State != StateBlocked {
				allBlocked = false
			}
		}
		if anyNonTerminal && allBlocked {
			return []AutoAction{Escalate{Reason: "deadlock: all remaining tasks are blocked"}}
		}
	}

	return acts
}
