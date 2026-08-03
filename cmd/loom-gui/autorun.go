package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/delegate"
	"github.com/henricktissink/loom/internal/status"
)

// Autonomy Slice 1 (autonomy spec §11): the App-side entry points that flag a
// run autonomous, drive it, and stop it. The pure decision (NextActions) and
// the loop (AutoRunner) live in internal/delegate; this file wires their seams
// to the real store, status engine, git and the delegation Runner.

// autoTickInterval is how long the loop waits between iterations. A few seconds
// matches the UI poll and the child-done signal's own granularity; faster would
// just re-poll git and the status engine for no new information.
const autoTickInterval = 3 * time.Second

// autoRunHandle is one live auto-runner. It carries the cancel that AbortRun
// pulls and the two facts the loop keeps between ticks: the per-task lapse
// counter (how many times a check has failed) and the last escalation reason.
//
// Both are IN-MEMORY for Slice 1: a process restart loses the retry count
// (erring toward more retries, never fewer) and the reason string (the run is
// still `escalated` on disk, so the control room still stops for a human — it
// just cannot re-print why until Task 6 makes the reason durable).
type autoRunHandle struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when the loop goroutine has exited

	mu         sync.Mutex
	lapses     map[string]int
	escalation string
}

func (h *autoRunHandle) lapse(taskID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lapses[taskID]
}

func (h *autoRunHandle) bumpLapse(taskID string) {
	h.mu.Lock()
	h.lapses[taskID]++
	h.mu.Unlock()
}

func (h *autoRunHandle) resetLapse(taskID string) {
	h.mu.Lock()
	delete(h.lapses, taskID)
	h.mu.Unlock()
}

func (h *autoRunHandle) setEscalation(reason string) {
	h.mu.Lock()
	h.escalation = reason
	h.mu.Unlock()
}

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

// startAutoRunner launches the background loop for an autonomous run, once. A
// second call for a run already running is a no-op — the map is the singleton
// guard, the same shape SpawnOrchestrator leans on the store CAS for.
func (a *App) startAutoRunner(runID int64) {
	a.autoMu.Lock()
	defer a.autoMu.Unlock()
	if a.autoRunners == nil {
		a.autoRunners = map[int64]*autoRunHandle{}
	}
	if _, ok := a.autoRunners[runID]; ok {
		return
	}
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	h := &autoRunHandle{cancel: cancel, done: make(chan struct{}), lapses: map[string]int{}}
	a.autoRunners[runID] = h

	ar := &delegate.AutoRunner{
		RunID: runID,
		// The auto-runner's cap is the SAME §6.6 concurrency ceiling the store
		// enforces at spawn, so NextActions and ClaimTaskSpawnCAS agree and the
		// runner never even offers a spawn the store would refuse.
		Cap:      delegate.Concurrency3a,
		Interval: autoTickInterval,

		State:            func() (delegate.RunState, error) { return a.autoState(runID) },
		Advance:          func(from, to delegate.RunState) (bool, error) { return a.autoAdvance(runID, from, to) },
		RecordEscalation: func(reason string) error { h.setEscalation(reason); return nil },
		Tick:             func(ctx context.Context) error { return a.autoTick(ctx, runID) },
		Project:          func() ([]delegate.TaskView, error) { return a.autoProject(runID, h) },
		Spawn:            func(id string) error { return a.autoSpawn(ctx, runID, id) },
		Check:            func(id string) error { return a.autoCheck(ctx, runID, id, h) },
		Integrate:        func(id string) error { return a.autoIntegrate(ctx, runID, id) },
	}

	go func() {
		defer close(h.done)
		_ = ar.Run(ctx)
		a.autoMu.Lock()
		delete(a.autoRunners, runID)
		a.autoMu.Unlock()
	}()
}

// stopAutoRunner cancels a running loop and waits (bounded) for it to exit, so
// AbortRun does not return while a goroutine is still touching the store. The
// bound matters: a loop mid-Tick or mid-merge finishes that action before
// checking the context, and AbortRun must not hang on it — the run is already
// `aborted`, so a loop that outlives the wait sees that on its next iteration
// and stops itself. Safe to call for a run that isn't running.
func (a *App) stopAutoRunner(runID int64) {
	a.autoMu.Lock()
	h := a.autoRunners[runID]
	a.autoMu.Unlock()
	if h == nil || h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
	}
}

// escalationReason returns the live loop's recorded escalation reason, if any.
// In-memory (Slice 1): empty after a restart even for an escalated run.
func (a *App) escalationReason(runID int64) string {
	a.autoMu.Lock()
	h := a.autoRunners[runID]
	a.autoMu.Unlock()
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.escalation
}

// --- the seams -------------------------------------------------------------

// autoState reads the run's control state, and maps a DATA-axis deadlock onto
// an escalation. A baseline fault (§10.2) or a §12.1 deadlock sets run.Status
// `deadlocked`; that needs a human, which is exactly what `escalated` means, so
// the loop stops. The run_state axis is flipped to agree (best effort) so the
// control room does not show a `running` autonomous run that has actually
// stopped.
func (a *App) autoState(runID int64) (delegate.RunState, error) {
	run, ok, err := a.st.GetDelegationRun(runID)
	if err != nil {
		return "", err
	}
	if !ok {
		// The run vanished (abandoned + swept). Stop, don't spin.
		return delegate.RunStateAborted, nil
	}
	if run.Status == "deadlocked" {
		_, _ = a.autoAdvance(runID, delegate.RunStateRunning, delegate.RunStateEscalated)
		return delegate.RunStateEscalated, nil
	}
	return delegate.RunState(run.State), nil
}

func (a *App) autoAdvance(runID int64, from, to delegate.RunState) (bool, error) {
	return a.st.AdvanceDelegationRunStateCAS(runID, string(from), string(to), a.now().Unix())
}

// autoTick runs the existing scheduler pass. Its error is returned to the loop,
// which logs it and carries on — one bad tick must not kill the run.
func (a *App) autoTick(ctx context.Context, runID int64) error {
	r, err := a.delegationRunner()
	if err != nil {
		return err
	}
	_, err = r.Tick(ctx, runID)
	return err
}

// autoProject builds the current []TaskView: the row state, the child-done
// signal from the status engine, the "committed since" time from git, the last
// check time from the row, and the in-memory lapse count. This is Task 2's
// genuinely new signal, and the thing the demo validates first.
func (a *App) autoProject(runID int64, h *autoRunHandle) ([]delegate.TaskView, error) {
	rows, err := a.st.ListDelegationTasks(runID)
	if err != nil {
		return nil, err
	}
	// One status poll feeds every task's child-done bit. A nil engine (tests,
	// headless) leaves the snapshot empty, which childDone reads as "ended" —
	// safe, because the check-on-done rule also requires a commit since the last
	// check, so a task with no worktree/commit is never checked on that basis.
	var snap status.Snapshot
	if a.engine != nil {
		snap, _ = a.engine.Poll(a.now())
	}
	out := make([]delegate.TaskView, 0, len(rows))
	for _, row := range rows {
		v := delegate.TaskView{
			ID:        row.TaskID,
			State:     delegate.TaskState(row.State),
			ChildIdle: childDone(snap, row.SessionName),
			CheckedAt: row.CheckAt,
			Lapses:    h.lapse(row.TaskID),
		}
		if row.Worktree != "" {
			v.CommittedAt = delegate.WorktreeHeadTime(row.Worktree)
		}
		out = append(out, v)
	}
	return out, nil
}

// childDone reports whether a task's child has finished working: its session is
// idle/needs-you/done (not actively Running), or it is no longer live at all. An
// empty session name (not spawned) is not done — there is no child yet.
func childDone(snap status.Snapshot, sessionName string) bool {
	if sessionName == "" {
		return false
	}
	for _, row := range snap.Live {
		if row.Name == sessionName {
			return row.Status != status.Running
		}
	}
	// Not among the live rows: the session ended, which is done.
	return true
}

// autoSpawn is the ready/approved -> running executor. It mirrors ApproveTask's
// core (ready->approved CAS, then Spawner.Spawn) but tolerates an already-
// approved task (a prior tick's partial spawn), so a retry completes rather than
// re-approving. A cap/race no-op is a nil error, not a failure to surface.
func (a *App) autoSpawn(ctx context.Context, runID int64, taskID string) error {
	// ready -> approved. claimed=false is fine: an approved task from a prior
	// tick just proceeds to Spawn. Only a real error stops us.
	if _, err := a.st.AdvanceTaskCAS(runID, taskID,
		string(delegate.StateReady), string(delegate.StateApproved), a.now().Unix()); err != nil {
		return err
	}
	run, m, _, task, err := a.runAndTask(runID, taskID)
	if err != nil {
		return err
	}
	sp, err := a.spawner()
	if err != nil {
		return err
	}
	if _, err := sp.Spawn(run, m, task); err != nil {
		// A task that moved out of `approved` under us (a concurrent abandon, or
		// the cap backstop) is not this tick's problem — the next projection sees
		// the real state.
		if errors.Is(err, delegate.ErrTaskMovedElsewhere) {
			return nil
		}
		return err
	}
	return nil
}

// autoCheck runs a task's check and maintains the lapse counter. A pass verifies
// the task and clears its lapses; a fail increments them and returns the task to
// `running` so the next tick can retry (until NextActions escalates at the
// ceiling). The check itself runs out of band, exactly as the human RunTaskCheck
// does — a child cannot influence its own verdict.
func (a *App) autoCheck(ctx context.Context, runID int64, taskID string, h *autoRunHandle) error {
	r, err := a.delegationRunner()
	if err != nil {
		return err
	}
	// A retry starts from `failed`; r.Check only accepts running/blocked, so
	// return a failed task to running first. claimed=false (already running, or
	// moved) just means Check will re-assert the source set itself.
	if row, ok, gerr := a.st.GetDelegationTask(runID, taskID); gerr == nil && ok &&
		row.State == string(delegate.StateFailed) {
		_, _ = a.st.AdvanceTaskCAS(runID, taskID,
			string(delegate.StateFailed), string(delegate.StateRunning), a.now().Unix())
	}
	res, err := r.Check(ctx, runID, taskID)
	if err != nil {
		// A not-checkable race (the task moved) is not a lapse — nothing was
		// tested. Surface other errors to the log.
		if errors.Is(err, delegate.ErrTaskMovedElsewhere) {
			return nil
		}
		return err
	}
	if res.Status == delegate.CheckPass {
		h.resetLapse(taskID)
		return nil
	}
	// Any non-pass verdict (fail, unpublished, infra-error) is a lapse. The
	// counter is what NextActions escalates on at the ceiling.
	h.bumpLapse(taskID)
	return nil
}

// autoIntegrate is the verified -> merged executor: integrate the task, and if
// the integration is green, full-auto-merge it. A non-green integration (a
// baseline fault, a task-blamed red) leaves the run's state to say so —
// autoState maps a resulting deadlock to an escalation. A green integration that
// then refuses to merge (divergence, drift, conflict) leaves the task
// `mergeable`, which NextActions escalates on next tick. No merge ever happens
// on anything but green gates.
func (a *App) autoIntegrate(ctx context.Context, runID int64, taskID string) error {
	r, err := a.delegationRunner()
	if err != nil {
		return err
	}
	run, m, _, task, err := a.runAndTask(runID, taskID)
	if err != nil {
		return err
	}
	res, err := r.Integrator.Integrate(ctx, run, m, task)
	if err != nil {
		if errors.Is(err, delegate.ErrIntegrationBusy) {
			// Another task is integrating this run (§10.2 serializes passes). Not
			// an error — next tick retries.
			return nil
		}
		return err
	}
	if !res.Green() {
		// Red or parked: the integrator recorded the baseline/blame and moved the
		// task. Nothing to merge; the loop's autoState surfaces a deadlock.
		return nil
	}
	// Green: full-auto-merge with EMPTY acknowledgements and force=false. That is
	// the whole guarantee — a merge that needs a human to acknowledge a
	// divergence or drift is REFUSED here (ErrAckStale / a blocker), leaving the
	// task `mergeable` for NextActions to escalate. Force is never set by the
	// auto-runner.
	if _, merr := r.Merge(ctx, runID, taskID, delegate.MergeAck{}, false); merr != nil {
		// Expected on divergence/drift/conflict — leave mergeable, escalate next
		// tick. Logged by the loop, not fatal.
		return merr
	}
	return nil
}
