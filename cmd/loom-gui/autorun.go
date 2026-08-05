package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/henricktissink/loom/internal/delegate"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

// Autonomy Slice 1 (autonomy spec §11): the App-side entry points that flag a
// run autonomous, drive it, and stop it. The pure decision (NextActions) and the
// loop (AutoRunner) live in internal/delegate; this file wires their seams to the
// real store, session status and the delegation Runner.
//
// DIVISION OF LABOUR: Runner.Tick already checks, integrates and promotes, so the
// only executors here are Spawn (via Runner.Approve, which builds the §9.2
// producer-merged base) and Merge (the one gate Tick leaves to §5.2). See the
// header of internal/delegate/autorun.go.

// autoTickInterval is how long the loop waits between iterations. A few seconds
// matches the UI poll and the child-done signal's own granularity; faster would
// just re-read git and the session table for no new information.
const autoTickInterval = 3 * time.Second

// autoRunHandle is one live auto-runner: the cancel AbortRun pulls, and a done
// channel closed when the goroutine exits (so AbortRun can join it). The
// escalation reason is NOT kept here — it is durable on the run row
// (delegation_runs.escalation), so it survives this goroutine exiting and a
// process restart.
type autoRunHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// StartAutonomousRun creates a delegation run and hands it to the auto-runner.
//
// It reuses StartDelegationRun's ONE creation path verbatim — pin, snapshot,
// insert, §10.1 worktrees, first tick — then flags the run `auto` and starts the
// runner. The agent never spawns: this is loom code, invoked by a human (or, in
// Slice 2, the orchestrator running `loom run start --auto`), and it only ever
// executes the manifest that already validated.
func (a *App) StartAutonomousRun(root, manifestName string) (int64, error) {
	res := a.StartDelegationRun(root, manifestName)
	if res.Hidden {
		return 0, errors.New("project is hidden")
	}
	if res.RunID == 0 {
		return 0, errors.New(errText(nil, res.Error))
	}
	// The run exists. A reported FAULT (res.Error with a real RunID) is recorded
	// on the run and retried by the loop's own tick, so it does not stop us
	// flagging the run auto.
	if err := a.st.SetDelegationRunMode(res.RunID, string(delegate.RunModeAuto), a.now().Unix()); err != nil {
		return res.RunID, err
	}
	a.startAutoRunner(res.RunID)
	return res.RunID, nil
}

// AbortRun stops an autonomous run's loop before its next spawn or merge.
//
// It moves run_state to `aborted` via a CAS from any non-terminal control state
// — abort is the one control transition a human issues whatever the loop is
// doing. It deliberately does NOT touch the run's data `status`: an abort ends
// the automation, it does not rewrite what the run accomplished.
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

// SweepAutoRuns re-adopts autonomous runs left live (run_state running/paused) by
// a previous process: the loop goroutines are in-memory and die with the process,
// so without this an in-flight auto run — or one created by `loom run start
// --auto` while the GUI was down — would sit `running` with nothing driving it.
// Called once on startup, after the context is wired. Returns how many it
// adopted. (A run created by the CLI while THIS process is already running is
// adopted at the next startup, not mid-session — that live hand-off is Slice 2.)
func (a *App) SweepAutoRuns() int {
	if a.st == nil {
		return 0
	}
	runs, err := a.st.ResumableAutoRuns()
	if err != nil {
		a.logAuto(0, "sweep auto runs: "+err.Error())
		return 0
	}
	for _, r := range runs {
		a.startAutoRunner(r.ID)
	}
	return len(runs)
}

// startAutoRunner launches the background loop for an autonomous run, once. A
// second call for a run already running is a no-op — the map is the singleton
// guard.
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
	h := &autoRunHandle{cancel: cancel, done: make(chan struct{})}
	a.autoRunners[runID] = h

	ar := &delegate.AutoRunner{
		// The auto-runner's cap is the SAME §6.6 ceiling the store enforces at
		// spawn, so NextActions and ClaimTaskSpawnCAS agree and the runner never
		// offers a spawn the store would refuse.
		Cap:      delegate.Concurrency3a,
		Interval: autoTickInterval,

		State:            func() (delegate.RunState, error) { return a.autoState(runID) },
		Advance:          func(from, to delegate.RunState) (bool, error) { return a.autoAdvance(runID, from, to) },
		RecordEscalation: func(reason string) error { return a.st.SetDelegationRunEscalation(runID, reason, a.now().Unix()) },
		Tick:             func(ctx context.Context) error { return a.autoTick(ctx, runID) },
		Project:          func() ([]delegate.TaskView, error) { return a.autoProject(runID) },
		Spawn:            func(id string) error { return a.autoSpawn(ctx, runID, id) },
		Merge:            func(id string) error { return a.autoMerge(ctx, runID, id) },
		Log:              func(msg string) { a.logAuto(runID, msg) },
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
// `aborted`, so a loop that outlives the wait sees that and stops itself. Safe to
// call for a run that isn't running.
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

// logAuto records a backend auto-runner diagnostic. It goes to the process log,
// not the frontend: these are loop-internal errors (a swallowed CAS, a failed
// tick), and the user-facing signal is the run's own state and escalation reason.
func (a *App) logAuto(runID int64, msg string) {
	if runID == 0 {
		log.Printf("autorun: %s", msg)
		return
	}
	log.Printf("autorun run %d: %s", runID, msg)
}

// --- the seams -------------------------------------------------------------

// autoState reads the run's control state, and maps a DATA-axis deadlock onto an
// escalation. A baseline fault (§10.2) or a §12.1 deadlock sets run.Status
// `deadlocked`; that needs a human, which is what `escalated` means, so the loop
// stops. It records the reason durably and flips run_state to agree so the
// control room does not show a `running` run that has actually stopped — and,
// unlike a silent best-effort write, it LOGS a failure of either step (the case
// where the axes could otherwise disagree).
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
		reason := "run deadlocked: a dependency cycle or a repo-baseline fault stopped progress (see the run's integration report)"
		if werr := a.st.SetDelegationRunEscalation(runID, reason, a.now().Unix()); werr != nil {
			a.logAuto(runID, "record deadlock escalation: "+werr.Error())
		}
		if _, aerr := a.autoAdvance(runID, delegate.RunStateRunning, delegate.RunStateEscalated); aerr != nil {
			a.logAuto(runID, "advance to escalated on deadlock: "+aerr.Error())
		}
		return delegate.RunStateEscalated, nil
	}
	return delegate.RunState(run.State), nil
}

func (a *App) autoAdvance(runID int64, from, to delegate.RunState) (bool, error) {
	return a.st.AdvanceDelegationRunStateCAS(runID, string(from), string(to), a.now().Unix())
}

// autoTick runs the existing scheduler pass — which itself checks, integrates and
// promotes. Its error is returned to the loop, which logs it and carries on; one
// bad tick must not kill the run.
func (a *App) autoTick(ctx context.Context, runID int64) error {
	r, err := a.delegationRunner()
	if err != nil {
		return err
	}
	_, err = r.Tick(ctx, runID)
	return err
}

// autoProject builds the current []TaskView: the row state, the child-idle signal
// from the PERSISTED session status (not a fresh engine Poll, which would consume
// the UI's one-shot needs-you edge), the "committed since" time from git, the
// last check time, and that check's verdict. NextActions reads these to decide
// spawn / merge / escalate.
func (a *App) autoProject(runID int64) ([]delegate.TaskView, error) {
	rows, err := a.st.ListDelegationTasks(runID)
	if err != nil {
		return nil, err
	}
	out := make([]delegate.TaskView, 0, len(rows))
	for _, row := range rows {
		v := delegate.TaskView{
			ID:          row.TaskID,
			State:       delegate.TaskState(row.State),
			ChildIdle:   a.childDone(row.SessionName),
			CheckedAt:   row.CheckAt,
			CheckStatus: row.CheckStatus,
		}
		if row.Worktree != "" {
			v.CommittedAt = delegate.WorktreeHeadTime(row.Worktree)
		}
		out = append(out, v)
	}
	return out, nil
}

// childDone reports whether a task's child has stopped working, from the
// PERSISTED session status the UI's poll maintains — deliberately not
// a.engine.Poll, whose one-shot needs-you edge the UI's notifier depends on and
// which a second poller would silently consume.
func (a *App) childDone(sessionName string) bool {
	if sessionName == "" {
		return false // not spawned: there is no child yet
	}
	row, ok, err := a.st.Get(sessionName)
	if err != nil || !ok {
		return true // gone = done
	}
	return childDoneRow(row)
}

// childDoneRow is the pure predicate: an ended session is done; a live one is
// done once it is no longer actively Running (idle/needs-you/done).
func childDoneRow(row store.SessionRow) bool {
	if row.EndedAt != -1 {
		return true
	}
	return row.LastStatus != string(status.Running)
}

// autoSpawn is the ready/approved -> running executor. It routes through
// Runner.Approve — the ONE spawn path that computes §9.2's producer-merged base
// (a same-repo producer's committed output merged in, cross-repo producers'
// integration worktrees add-dir'd) — so an autonomous consumer task is not
// launched blind to what it declares it needs. Approve is idempotent for a task
// already `approved` from a prior tick's partial spawn. A cap/race no-op is a nil
// error, not a failure to surface.
func (a *App) autoSpawn(ctx context.Context, runID int64, taskID string) error {
	r, err := a.delegationRunner()
	if err != nil {
		return err
	}
	if err := r.Approve(ctx, runID, taskID); err != nil {
		// The task moved out from under us (a concurrent abandon, the cap
		// backstop, or a stale ready-set) — not this tick's problem; the next
		// projection sees the real state.
		if errors.Is(err, delegate.ErrTaskMovedElsewhere) {
			return nil
		}
		return err
	}
	return nil
}

// autoMerge is the mergeable -> merged executor: the one step Tick leaves to
// §5.2's gate. Tick integrated the task to `mergeable` (all §10.2 gates green);
// this merges it with EMPTY acknowledgements and force=false. That is the whole
// full-auto-merge guarantee — a merge that would need a human to acknowledge a
// scope divergence, out-of-worktree drift, or a conflict is REFUSED here, and the
// loop turns that refusal into an escalation (never a forced merge). A benign
// race (the task already moved) is a nil error, not an escalation.
func (a *App) autoMerge(ctx context.Context, runID int64, taskID string) error {
	r, err := a.delegationRunner()
	if err != nil {
		return err
	}
	if _, merr := r.Merge(ctx, runID, taskID, delegate.MergeAck{}, false); merr != nil {
		if errors.Is(merr, delegate.ErrTaskMovedElsewhere) {
			return nil
		}
		return merr
	}
	return nil
}
