package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
)

// run.go — the CAS-guarded runner that sequences everything else: approve,
// spawn, check, integrate, park, resume (§15's own description of this file).
//
// It owns no rules. Every rule lives in the file that owns its spec section —
// §9 in graph.go, §10 in integrate.go, §11 in block.go and rendezvous.go, §12 in
// snapshot.go and watchdog.go — and this file's whole job is ORDER, CAS
// DISCIPLINE and VISIBILITY. That split is deliberate: a runner that also
// decides is a runner nobody can test, and every one of those rules is
// table-testable exactly because none of them needs a Runner to evaluate.
//
// # Two Loom instances against one DB is a SUPPORTED state
//
// Every multi-writer transition here is a compare-and-swap with RowsAffected()
// checked (store.AdvanceTaskCAS and friends), and a rejected CAS is never
// retried in a loop — it means the row moved, which means the premise the step
// was computed from is gone, so the step is ABANDONED and recomputed on the next
// tick from fresh state. A CAS retried in place is a CAS that has been argued
// out of being a CAS.
//
// A cap or a claim enforced OUTSIDE the UPDATE is advisory, not enforced. §6.6's
// cap is inside store.ClaimTaskSpawnCAS's WHERE clause for that reason, and
// nothing in this file may re-check it in Go and call that enforcement.

// Runner drives one Loom process's delegation runs.
type Runner struct {
	Store      *store.Store
	Layout     Layout
	Spawner    *Spawner
	Checker    *Checker
	Worktrees  *Worktrees
	Integrator *Integrator
	Rendezvous *Rendezvous
	Detector   *Detector
	Watchdogs  *Watchdogs
	// Amendments is the durable amendment log. An INTERFACE and not the store,
	// because delegation_amendments does not exist yet (§13.1 declares it,
	// migration v12 deliberately omits it — an empty table is an invitation to
	// write to it, and 3a would have shipped one). See the handoff note at the
	// bottom of this file. A nil Amendments degrades to "no amendments", which
	// is exactly a 3a run.
	Amendments AmendmentStore

	// Hidden reports whether a project root is currently hidden or filtered out
	// by solo (slice 1 §6.2). Injected as a function rather than taking a
	// projects.Resolver, because §14's table needs ONE bit and taking the
	// resolver would invite this file to re-derive attribution — which is
	// §14.1's whole warning: a delegation child's cwd matches no project target,
	// Visible fails CLOSED, and every child vanishes the moment anything is
	// hidden. Child visibility is decided by the run's project ALONE, via
	// Attributor. Nothing in this file routes a child through the raw resolver.
	Hidden func(projectRoot string) bool

	// Repos re-resolves repo label → primary work tree for a project root.
	//
	// It is REQUIRED for anything that touches git, and the reason is a property
	// of the snapshot: Manifest.ProjectRoot and Manifest.RepoPaths are
	// `json:"-"`, deliberately — they are resolved by the loader from the
	// machine's project table and a run that replayed a stale absolute path
	// would be worse than one that has none. So the manifest read back out of
	// manifest_json has NO repo paths, and every spawn from it failed at
	// `git worktree add` with an empty repo path until this existed.
	//
	// A function of the project root rather than a map on the Runner, because one
	// Runner serves every run in the process and those runs belong to different
	// projects. Same shape and same reason as Hidden: this package stays free of
	// internal/projects, and the caller that owns the resolver supplies the one
	// answer this file needs.
	Repos func(projectRoot string) map[string]string

	Now func() time.Time
}

// §14's hiding table, which this file implements and which is not symmetric —
// each row has its own reason and none of them generalizes:
//
//	Running/blocked children  untouched, keep running   §6.2a: hiding never
//	                                                    alters in-flight behaviour
//	Checks                    KEEP RUNNING, output not  The check is the run's
//	                          rendered                  CLOCK; suppressing it
//	                                                    stalls the run silently,
//	                                                    which is worse than the
//	                                                    spend. Output is what
//	                                                    would leak, not execution.
//	Seed deliveries (§11.4)   KEEP RUNNING              Continuation of in-flight
//	                                                    work, not new work
//	Spawns                    SUPPRESSED; approve stays A new tmux window titled
//	                          pending, greyed with a    with the client's repo is
//	                          reason                    exactly the leak §6 exists
//	                                                    to prevent, and it is
//	                                                    unambiguously new work
//	Merges                    suppressed                human-gated anyway, and
//	                                                    the gate is hidden
//	Deadlock notification     FIRES, label-free body    §6.4
//
// The greyed-with-a-reason part is load-bearing: hiding the action instead would
// make a suppressed run look like a stalled one, and the human would go looking
// for a bug.

// TickReport is one poll's worth of what happened, for the run view and for the
// logs. Every field is a rendering: a tick that did something invisible is a
// tick that will be debugged by reading source.
type TickReport struct {
	RunID int64
	// Progress is §9.3's four buckets as computed this tick.
	Progress Progress
	// Ready is what the gate may offer. Suppressed carries the tasks that WOULD
	// have been offered but were not, with the reason, so the UI can grey them
	// rather than drop them.
	Suppressed []SuppressedAction
	// Spawned/Checked/Integrated/Resumed name the tasks each stage acted on.
	Spawned    []string
	Checked    []string
	Integrated []string
	Resumed    []string
	// Blocks is what the detector saw, malformed ones included.
	Blocks []BlockEvent
	// Findings is §12.2's watchdog pass.
	Findings []Finding
	// Proposals is §11.3's amendments appended to the durable log this tick.
	// Appended, never granted: an amendment is inert until a human approves it,
	// and the row exists so the offer survives a restart and so §2's M3 has
	// something to count.
	Proposals []Proposal
	// Orphaned names the tasks whose child session died this tick (§6.3). The
	// worktree is untouched and nothing is killed — the flag is the whole action.
	Orphaned []string
	// Deadlock is non-nil when the run has stopped. It is needs-you-grade and
	// PERMANENT — a deadlock that clears itself on the next tick was not a
	// deadlock, and a red run row the human never saw is the hazard restated.
	Deadlock *Deadlock
	// Measurements is §2's M2 and M3 as of this tick. On every report and not
	// behind a flag: the decision rule in §2 is what says whether §§9-12 should
	// exist at all, and a number that has to be asked for is a number that gets
	// reconstructed by hand from a run nobody kept.
	Measurements Measurements
	// Errs collects every per-task failure this tick. Collected rather than
	// returned: one task's git failure must not cost the other eleven their
	// tick, and an error that stops the loop is an error that stops the run.
	Errs []TaskError
	At   time.Time
}

// SuppressedAction is an action Loom would have taken and did not, with the
// reason in the human's words. It exists because "failures degrade rather than
// crash, and are always VISIBLE" applies to deliberate refusals too — a
// suppressed spawn that renders as nothing is indistinguishable from a scheduler
// that is broken.
type SuppressedAction struct {
	TaskID string
	Action string // "spawn" | "merge"
	Reason string // "project is hidden" | "cap reached (4/4)" | "run budget exceeded"
}

// TaskError is one task's failure, carried rather than returned.
type TaskError struct {
	TaskID string
	Stage  string
	Err    error
}

// Create validates a manifest, pins one base commit per in-scope repo, writes
// the run and its task rows, and creates the per-repo integration worktrees
// (§10.1: "one per repo per run, created at RUN CREATION").
//
// Integration worktrees are created eagerly for a reason: a repo whose worktree
// cannot be created should fail while the human is still looking at the run,
// not an hour later behind the first green check, when there are children
// holding context and the only remedy left is expensive.
//
// The manifest is SNAPSHOTTED into delegation_runs.manifest_json
// (workflow_runs.def_json precedent): the run replays what it was created from
// even if the on-disk file is edited or deleted underneath it, and amendments
// are a separate append-only log rather than a mutation of either.
func (r *Runner) Create(m Manifest) (store.DelegationRun, error) {
	if r.Store == nil {
		return store.DelegationRun{}, errors.New("delegate: Runner needs a Store")
	}
	// §3 containment, re-asserted at the ACT rather than inherited from the load.
	// A manifest sitting under one project that names another must not create a
	// run attributed to the directory it happens to live in.
	if strings.TrimSpace(m.ProjectRoot) == "" {
		return store.DelegationRun{}, fmt.Errorf("delegate: manifest %q resolved to no project", m.Name)
	}
	// The hidden test is here and not only at the gate: a run creates worktrees
	// and, at its first approval, a tmux window titled with the client's repo.
	// §14's table refuses new Loom-initiated work on a hidden project, and this
	// is the earliest point that refusal can be made.
	if r.Hidden != nil && r.Hidden(m.ProjectRoot) {
		return store.DelegationRun{}, fmt.Errorf("%w: %s", ErrProjectHidden, m.ProjectRoot)
	}

	bases, err := PinBases(m)
	if err != nil {
		return store.DelegationRun{}, err
	}
	snapshot, err := json.Marshal(m)
	if err != nil {
		return store.DelegationRun{}, err
	}
	baseJSON, err := json.Marshal(bases)
	if err != nil {
		return store.DelegationRun{}, err
	}

	now := r.now().Unix()
	run, err := r.Store.InsertDelegationRun(m.Name, m.ProjectRoot, string(snapshot), string(baseJSON), now)
	if err != nil {
		return store.DelegationRun{}, err
	}

	// Tasks are inserted `pending`. Nothing is seeded `ready` here: Tick is the
	// ONE place that decides what is ready, and a creation path that promoted
	// rows itself would be a second scheduler that can disagree with the first.
	for _, t := range m.Tasks {
		if err := r.Store.InsertDelegationTask(store.DelegationTask{
			RunID: run.ID, TaskID: t.ID, State: string(StatePending),
			RepoLabel: t.Repo, UpdatedAt: now,
		}); err != nil {
			return run, fmt.Errorf("delegate: run %d created, but task %q could not be written: %w", run.ID, t.ID, err)
		}
	}

	// §10.1: one integration worktree per repo per run, created at RUN CREATION.
	// EAGERLY, and the eagerness is the whole point — a repo whose worktree
	// cannot be created should fail while the human is still looking at this run,
	// not an hour later behind the first green check when there are children
	// holding context and the only remedy left is expensive.
	//
	// A failure is REPORTED and does not roll the run back: the run row and its
	// tasks are real and usable, and deleting them would also delete the record
	// of why. The caller renders the error beside a run that will refuse to
	// integrate that repo.
	var ensureErrs []string
	if r.Integrator != nil {
		for _, label := range inScopeRepos(m) {
			if _, err := r.Integrator.Ensure(run, m, label); err != nil {
				ensureErrs = append(ensureErrs, fmt.Sprintf("repo %q: %v", label, err))
			}
		}
	}

	if _, err := r.Store.AdvanceDelegationRunCAS(run.ID, "planning", "running", now); err != nil {
		ensureErrs = append(ensureErrs, fmt.Sprintf("run stayed at `planning`: %v", err))
	}
	fresh, ok, err := r.Store.GetDelegationRun(run.ID)
	if err == nil && ok {
		run = fresh
	}
	if len(ensureErrs) > 0 {
		return run, fmt.Errorf("delegate: run %s created with faults: %s", run.Slug, strings.Join(ensureErrs, "; "))
	}
	return run, nil
}

// inScopeRepos is every repo label some task of the manifest names, sorted. The
// TASKS' repos and not the `repos` map: a manifest may declare setup for a repo
// no task uses, and creating an integration worktree for it would cost a `git
// worktree add` and a directory for work that will never happen.
func inScopeRepos(m Manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range m.Tasks {
		if t.Repo == "" || seen[t.Repo] {
			continue
		}
		seen[t.Repo] = true
		out = append(out, t.Repo)
	}
	sort.Strings(out)
	return out
}

// Tick is the poll. Order matters and is the spec's:
//
//  1. Load run, tasks, artifacts, amendments, blocks — ONE read each, no
//     per-task queries; a poll loop that is O(tasks) round-trips is the reason
//     §6.6 lists Loom's own poll cost as a cap justification.
//  2. Detector.Poll — the FILE is the trigger (§11.2), so blocks are observed
//     before anything is scheduled against a stale picture.
//     2b. Propose an amendment for every parked task that implies one (§11.3).
//     Appended, never granted, and idempotent by proposal identity.
//  3. Effective + Tick → Progress, and the ready set is PERSISTED (pending →
//     ready) under CAS — this is the only scheduler.
//  4. Checks for `running` tasks whose branch head moved (§8.2's debounce, via
//     check.go's ShouldRun). Runs even when hidden.
//  5. Integration for `verified` tasks, one at a time run-wide
//     (ErrIntegrationBusy is not an error condition, it is next tick).
//  6. Resume for `blocked` tasks whose condition is satisfied — materialize
//     THEN seed (§11.4), never the other way round.
//  7. Watchdogs.
//  8. Deadlock. Last, because every earlier step can clear the condition, and a
//     deadlock verdict computed before them would fire on a run that was about
//     to move.
//
// Spawns are NOT in this list. Nothing auto-spawns, ever (§16): Loom's stated
// principle is that nothing silently auto-advances, and unattended spawning is
// how a night's quota disappears with nothing on screen. Tick computes what is
// READY and the human presses approve.
func (r *Runner) Tick(ctx context.Context, runID int64) (TickReport, error) {
	rep := TickReport{RunID: runID, At: r.now()}
	st, err := r.load(runID)
	if err != nil {
		return rep, err
	}

	// Step 2: the FILE is the trigger (§11.2), so blocks are observed before
	// anything is scheduled against a stale picture. Poll performs the
	// running→blocked CAS itself; a rejected one means the other Loom instance
	// won and is not an error.
	if r.Detector != nil {
		events, err := r.Detector.Poll(st.run, st.m)
		if err != nil {
			rep.Errs = append(rep.Errs, TaskError{Stage: "detect", Err: err})
		}
		rep.Blocks = events
		if len(events) > 0 {
			// Re-read: Poll moved rows, and every bucket below is computed from
			// the state column. Re-deriving from the pre-poll snapshot would count
			// a task Loom just parked as running for one whole tick, which is the
			// window a deadlock hides in.
			if fresh, ferr := r.load(runID); ferr == nil {
				st = fresh
			}
		}
	}

	// Step 2b: §11.3's proposals. Every parked task is re-proposed on every tick
	// and the append is idempotent by proposal IDENTITY (kind, task, artifact,
	// producer, paths), not by "have I seen this block before" — the alternative
	// was to propose only for the blocks Poll reported as NEW this tick, which
	// loses the proposal for every block that was already on disk when Loom
	// started, and the whole point of the durable row is that the offer survives
	// a restart.
	rep.Proposals = append(rep.Proposals, r.propose(st, &rep)...)

	// Step 3.
	rep.Progress = Tick(st.e, st.states, st.published)

	// Step 3b: PERSIST the ready set, pending → ready, under CAS.
	//
	// Computing readiness and never writing it is why Approve could not be
	// pressed at all: Spawner.Approve's CAS expects `ready`, Create inserts
	// `pending`, and nothing in between moved the row. cmd/loom-gui does the
	// promotion today over `Ready(BuildGraph(m), …)` — the DECLARED graph — which
	// is a second scheduler that cannot see an approved amendment's edge, so a
	// task the effective graph says is still waiting on `account-schema` would be
	// offered for spawn by the view. One scheduler, and it is this one; the edge
	// set it reads is the effective one.
	//
	// Promotion happens even when the project is hidden. `ready` is a fact about
	// the graph, not an offer — §14's suppression is at the GATE, and Approve
	// refuses there with a reason the human can read. Demoting the row instead
	// would make a hidden run look like a stalled one.
	for _, id := range rep.Progress.Ready {
		if st.states[id] != StatePending {
			continue
		}
		// A rejected CAS is dropped, not retried: the row moved, so this tick's
		// ready set was computed from a picture that no longer exists, and the
		// next tick recomputes it from the fresh one.
		if _, err := r.Store.AdvanceTaskCAS(runID, id, string(StatePending), string(StateReady), rep.At.Unix()); err != nil {
			rep.Errs = append(rep.Errs, TaskError{TaskID: id, Stage: "ready", Err: err})
		}
	}

	hidden := r.hidden(st.run)

	// Step 4: checks for `running` tasks whose branch head moved. THEY RUN EVEN
	// WHEN HIDDEN (§14): the check is the run's clock, and suppressing it stalls
	// the run silently, which is worse than the spend. What hiding suppresses is
	// the OUTPUT, and that is the renderer's job, not this one's.
	for _, id := range st.order {
		row := st.rows[id]
		if TaskState(row.State) != StateRunning {
			continue
		}
		if !ShouldRun(r.branchHead(row), lastCheckedHead(row), r.transcriptStateOf(row)) {
			continue
		}
		if _, err := r.Check(ctx, runID, id); err != nil {
			rep.Errs = append(rep.Errs, TaskError{TaskID: id, Stage: "check", Err: err})
			continue
		}
		rep.Checked = append(rep.Checked, id)
	}

	// Step 4b: §10.3's re-attempt edge. A task parked by a failed integration
	// sits `blocked` behind a Loom-authored needs-decision declaration, and
	// Rendezvous.Unblocked answers false for that kind BY DESIGN — a human clears
	// those. The child's own remedy is to push commits, so the re-attempt trigger
	// is THE BRANCH HEAD MOVING: blocked + head moved ⇒ re-check ⇒ verified ⇒
	// integrate. Without this edge a §10.3 park is permanent, which §10.2
	// explicitly says it is not ("re-attempted whenever the child pushes new
	// commits").
	//
	// Only Loom-authored parks are re-attempted. A child that declared its own
	// needs-decision block and then happened to commit has not answered the
	// question it asked; re-checking it would clear a park the human owes an
	// answer to.
	for _, id := range st.order {
		row := st.rows[id]
		if TaskState(row.State) != StateBlocked {
			continue
		}
		b := st.e.Blocks[id]
		if b.Author != AuthorLoom || b.Kind != BlockNeedsDecision {
			continue
		}
		head := r.branchHead(row)
		if head == "" || head == lastCheckedHead(row) {
			continue
		}
		if _, err := r.Check(ctx, runID, id); err != nil {
			rep.Errs = append(rep.Errs, TaskError{TaskID: id, Stage: "recheck", Err: err})
			continue
		}
		rep.Checked = append(rep.Checked, id)
	}

	// Step 5: integration for `verified` tasks, ONE AT A TIME RUN-WIDE.
	// ErrIntegrationBusy is not an error condition — it is next tick — so it is
	// neither collected nor rendered as a fault.
	if r.Integrator != nil {
		if fresh, ferr := r.load(runID); ferr == nil {
			st = fresh
		}
		for _, id := range st.order {
			if TaskState(st.rows[id].State) != StateVerified {
				continue
			}
			t, ok := st.tasks[id]
			if !ok {
				continue
			}
			res, err := r.Integrator.Integrate(ctx, st.run, st.m, t)
			switch {
			case errors.Is(err, ErrIntegrationBusy):
				continue
			case err != nil:
				rep.Errs = append(rep.Errs, TaskError{TaskID: id, Stage: "integrate", Err: err})
				continue
			}
			rep.Integrated = append(rep.Integrated, id)
			// The result's warnings are the most consequential thing §10 can say
			// about a GREEN pass ("this repo declares no per-repo check: the
			// task's own check is the only evidence"), and they are not in Output.
			for _, w := range res.Warnings {
				rep.Suppressed = append(rep.Suppressed, SuppressedAction{
					TaskID: id, Action: "integrate", Reason: w,
				})
			}
		}
	}

	// Step 6: resume for `blocked` tasks whose condition is satisfied.
	// Rendezvous.Resume materializes THEN seeds; nothing here may bypass
	// Unblocked, because dependency-gated scheduling (§9) stays primary.
	//
	// Seed deliveries run even when hidden: a delivery is the continuation of
	// in-flight work, not new work (§14's table).
	if r.Rendezvous != nil {
		if fresh, ferr := r.load(runID); ferr == nil {
			st = fresh
		}
		for _, id := range st.order {
			if TaskState(st.rows[id].State) != StateBlocked {
				continue
			}
			if !r.Rendezvous.Unblocked(st.e, st.states, st.published, id) {
				continue
			}
			t, ok := st.tasks[id]
			if !ok {
				continue
			}
			if err := r.Rendezvous.Resume(st.run, st.m, t, st.e.Blocks[id]); err != nil {
				// ErrSeedUndelivered is explicitly NOT a failure: the seed stays
				// owed, the flag stays on and §12.2's retry offers it again. It is
				// still collected, because a run in which nothing is arriving is
				// something the human must be able to see.
				rep.Errs = append(rep.Errs, TaskError{TaskID: id, Stage: "resume", Err: err})
				continue
			}
			rep.Resumed = append(rep.Resumed, id)
		}
	}

	// Step 7: watchdogs. Pure pass, then the findings are APPLIED LITERALLY —
	// ActionOfferRetry renders an affordance and must not retry anything itself,
	// ActionStopSpawns suppresses new approvals only and touches nothing running.
	if fresh, ferr := r.load(runID); ferr == nil {
		st = fresh
		rep.Progress = Tick(st.e, st.states, st.published)
	}
	rep.Findings = r.watch(st, rep.At)

	stopSpawns := false
	for _, f := range rep.Findings {
		switch f.Action {
		case ActionFlag:
			if f.TaskID == "" || f.Flag == "" {
				continue
			}
			if err := setTaskFlag(r.Store, runID, f.TaskID, f.Flag, true, rep.At.Unix()); err != nil {
				rep.Errs = append(rep.Errs, TaskError{TaskID: f.TaskID, Stage: "flag", Err: err})
				continue
			}
			// §6.3's flag is also reported by name. "A worktree whose child died
			// is not garbage" — nothing was killed and nothing advanced, so the
			// ONLY trace of the death is this line and the badge, and a finding
			// the caller has to go looking for is one nobody sees.
			if f.Flag == FlagOrphaned {
				rep.Orphaned = append(rep.Orphaned, f.TaskID)
			}
		case ActionUnflag:
			// The clear is silent on purpose: a live child again is the absence of
			// a problem, and reporting it would put "orphaned" on a screen next to
			// a task that is working.
			if f.TaskID == "" || f.Flag == "" {
				continue
			}
			if err := setTaskFlag(r.Store, runID, f.TaskID, f.Flag, false, rep.At.Unix()); err != nil {
				rep.Errs = append(rep.Errs, TaskError{TaskID: f.TaskID, Stage: "flag", Err: err})
			}
		case ActionResolve:
			if r.Watchdogs == nil || f.TaskID == "" {
				continue
			}
			if _, err := r.Watchdogs.ResolveSpawnOrphan(st.run, f.TaskID, st.rows[f.TaskID].RepoLabel); err != nil {
				rep.Errs = append(rep.Errs, TaskError{TaskID: f.TaskID, Stage: "orphan", Err: err})
			}
		case ActionRecover:
			if f.TaskID == "" {
				continue
			}
			if reason := r.recoverStale(st, f.TaskID); reason != "" {
				rep.Suppressed = append(rep.Suppressed, SuppressedAction{
					TaskID: f.TaskID, Action: "recover", Reason: reason,
				})
			}
		case ActionStopSpawns:
			stopSpawns = true
		case ActionOfferRetry:
			// Deliberately nothing. The affordance is the render; retrying the
			// seed here would take the decision away from the human the watchdog
			// exists to inform, and would re-deliver into a live prompt.
		}
	}

	// The ready set, with everything that would have suppressed it named rather
	// than dropped. A suppressed action rendered as nothing is indistinguishable
	// from a scheduler that is broken.
	deadRun := st.run.Status == "deadlocked"
	for _, id := range rep.Progress.Ready {
		switch {
		case hidden:
			rep.Suppressed = append(rep.Suppressed, SuppressedAction{
				TaskID: id, Action: "spawn", Reason: "project is hidden"})
		case deadRun:
			rep.Suppressed = append(rep.Suppressed, SuppressedAction{
				TaskID: id, Action: "spawn", Reason: redRunReason(st.run)})
		case stopSpawns:
			rep.Suppressed = append(rep.Suppressed, SuppressedAction{
				TaskID: id, Action: "spawn", Reason: "run budget exceeded; nothing running is touched"})
		}
	}

	// Step 8: the deadlock verdict, LAST, because every step above can clear the
	// condition and a verdict computed before them would fire on a run that was
	// about to move.
	rep.Deadlock = DetectDeadlock(st.e, rep.Progress, st.states)
	if rep.Deadlock != nil && st.run.Status == "running" {
		// Needs-you-grade and PERMANENT. A red row the human never saw is the
		// hazard restated, and a deadlock that clears itself on the next tick was
		// not a deadlock.
		if _, err := r.Store.AdvanceDelegationRunCAS(runID, "running", "deadlocked", rep.At.Unix()); err != nil {
			rep.Errs = append(rep.Errs, TaskError{Stage: "deadlock", Err: err})
		}
	}

	// §2, last, from the freshest picture this tick has: the flags Check wrote
	// and the amendment rows step 2b appended are both in it. Free — it is a fold
	// over data already read, and it issues no query of its own.
	if fresh, ferr := r.load(runID); ferr == nil {
		st = fresh
	}
	rep.Measurements = measure(st)
	return rep, nil
}

// recoverStale is WatchWorkStale's action: a row left in `integrating` or
// `checking` by a Loom that died mid-pass is released back to the state the WORK
// is really in, and the integration worktree is reset to a commit that carries a
// verdict.
//
// It returns a sentence when something could not be done, and "" on a clean
// recovery. Silence on success is deliberate — a recovered row is the absence of
// a problem, and rendering it would put "recovered" beside a task that is simply
// working — but a recovery that could NOT complete is exactly the invisible
// wedge this whole row exists to expose, so it is reported.
//
// ORDER: the worktree is reset BEFORE the row is released. §10.2's BINDING rule
// is that the integration branch only ever contains work that was green end to
// end, and the dead pass left an unevaluated merge in it; releasing the row
// first would let the next tick integrate the NEXT task on top of that merge,
// which is the systematic mis-attribution reset-on-red exists to prevent. If the
// reset fails the row is left claimed, on purpose: a wedged run that says so is
// better than a staging branch quietly accumulating unevaluated merges.
//
// NOTHING of the child's is touched. No session, no worktree, no branch — the
// work is committed on the task's own branch and is re-checked and re-integrated
// from step 0 exactly as a first attempt.
func (r *Runner) recoverStale(st *runState, taskID string) string {
	row, ok := st.rows[taskID]
	if !ok {
		return ""
	}
	now := r.now().Unix()
	switch TaskState(row.State) {
	case StateChecking:
		// No tree to repair: a check reads the child's own worktree and writes
		// nothing to it. Back to `running`, which is where a red check lands too,
		// and §8.2's debounce re-runs it against the same head.
		if _, err := r.Store.AdvanceTaskCAS(st.run.ID, taskID,
			string(StateChecking), string(StateRunning), now); err != nil {
			return "the task could not be released from `checking`: " + err.Error()
		}
		return ""
	case StateIntegrating:
		base := DecodeBaselines(st.run.Integration)[row.RepoLabel]
		if base.Head == "" {
			// No recorded position to reset to. The row stays claimed and the
			// human is told, because guessing a commit for a branch Loom owns is
			// how a verified sibling's staging merge silently disappears.
			return fmt.Sprintf("a dead pass left %s in `integrating`, but repo %q has no recorded integration baseline "+
				"to reset the staging worktree to; the run cannot integrate until %s is released by hand",
				taskID, row.RepoLabel, taskID)
		}
		if r.Integrator == nil {
			return "no integrator is configured, so the staging worktree cannot be reset and the row is left claimed"
		}
		dir := r.Layout.IntegrationDir(st.run.Slug, row.RepoLabel)
		r.Integrator.reset(dir, base.Head)
		if head, err := gitOut(dir, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(head) != base.Head {
			return fmt.Sprintf("the staging worktree for repo %q could not be reset to %s; %s is left claimed rather than "+
				"integrating the next task on top of an unevaluated merge", row.RepoLabel, short(base.Head), taskID)
		}
		if _, err := r.Store.AdvanceTaskCAS(st.run.ID, taskID,
			string(StateIntegrating), string(StateVerified), now); err != nil {
			return "the task could not be released from `integrating`: " + err.Error()
		}
		return ""
	}
	return ""
}

// redRunReason says WHY a run is red, because §10.2's baseline fault and §12.1's
// deadlock both land on status `deadlocked` and the two remedies have nothing in
// common. The discriminator is the run's `integration` column: a repo whose
// baseline is not `pass` is a baseline fault, and no task is to blame for it.
func redRunReason(run store.DelegationRun) string {
	for _, repo := range sortedBaselineRepos(run) {
		b := DecodeBaselines(run.Integration)[repo]
		if b.Red() {
			return fmt.Sprintf("the integration baseline for repo %q is red (%s); no task is to blame and spawning stops until it is fixed",
				repo, b.Status)
		}
	}
	return "the run is deadlocked; spawning stops until it is re-planned"
}

// sortedBaselineRepos orders the run's recorded baselines so two Loom instances
// name the same repo first.
func sortedBaselineRepos(run store.DelegationRun) []string {
	bs := DecodeBaselines(run.Integration)
	out := make([]string, 0, len(bs))
	for k := range bs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Approve is §5.1's gate action: claim the task, create the worktree at the base
// PlanBase computed (§9.2's multi-producer merge included), write the brief,
// bootstrap, launch, bind the session. Most of it is spawn.go's already; this
// wraps it with the parts §§9-12 adds — the producer merge, the spawn snapshot,
// and the hidden-project suppression.
//
// ORDER, and the disclosure that goes with it. Where a pre-chosen id exists the
// CLAIM COMES FIRST: Spawner.Approve's ready→approved CAS and ClaimTaskSpawnCAS's
// approved→spawning CAS both precede every side effect, so two presses of this
// button contend on one row and exactly one of them creates a worktree.
//
// Where the id is MINTED BY THE LAUNCH it cannot: session.Launcher.Launch
// invents its own session name, so the task↔session link is written after the
// process exists and the sequence is launch-then-CAS. The accepted failure mode
// is workflow.Advance's, restated: a concurrent abandon can reject that final
// CAS and leave a live child whose row no longer says `spawning`. It is narrowed
// — Launch upserts the row with Cwd = physicalDir(worktree) in the same call
// that creates the tmux session, so recovery keys on cwd (§13.3) and Abandon
// sweeps the same paths — and it is NOT closed. §13.3's retraction of revision
// 1's "strictly better than workflow" claim stands.
//
// A ProducerConflict from the base merge is a HARD STOP and not a degraded
// spawn: the task goes `blocked` with a Loom-authored needs-decision block
// naming both producers and every conflicting file. Two producers already
// disagree about the same lines; that is real information about the plan, it
// belongs to a human, and asking a child to resolve it is asking it to make a
// design decision it was explicitly not authorized for.
func (r *Runner) Approve(ctx context.Context, runID int64, taskID string) error {
	st, err := r.load(runID)
	if err != nil {
		return err
	}
	t, ok := st.tasks[taskID]
	if !ok {
		return fmt.Errorf("delegate: task %q is not part of run %d", taskID, runID)
	}

	// §14: a new tmux window titled with the client's repo is exactly the leak
	// §6 exists to prevent, and it is unambiguously new work.
	if r.hidden(st.run) {
		return fmt.Errorf("%w: %s", ErrProjectHidden, st.run.ProjectRoot)
	}

	// BINDING (§10.2): spawning stops while the run is red. This refusal is the
	// only thing that actually implements it — integrate.go can SET the status on
	// a baseline fault but it cannot enforce a consequence at a gate it does not
	// own. Refusing here rather than dropping the action is the other half:
	// TickReport.Suppressed carries the same reason so the button greys with an
	// explanation instead of vanishing.
	if st.run.Status == "deadlocked" {
		return fmt.Errorf("%w: %s", ErrRunRed, redRunReason(st.run))
	}

	if r.Spawner == nil {
		return errors.New("delegate: Runner needs a Spawner to approve")
	}
	if claimed, err := r.Spawner.Approve(runID, taskID); err != nil {
		return err
	} else if !claimed {
		return fmt.Errorf("%w: run %d task %q is not `ready`", ErrTaskMovedElsewhere, runID, taskID)
	}

	// §9.2's base: the run's pinned commit with every same-repo producer merged
	// in, plus the cross-repo producers' integration worktrees as add-dirs. The
	// PLAN is computed here and EXECUTED inside Worktrees.Create, between
	// `worktree add` and bootstrap — never by this caller afterwards, which would
	// leave a complete-looking worktree missing its declared dependencies on disk.
	plan := PlanBase(st.e, st.m, t, st.rows, st.bases, st.integrationDirs())

	_, err = r.Spawner.SpawnWith(st.run, st.m, t, plan)
	var conflict *ProducerConflict
	if errors.As(err, &conflict) {
		// A HARD STOP, not a degraded spawn. Two producers already disagree about
		// the same lines; that is real information about the plan, it belongs to a
		// human, and asking a child to resolve it is asking it to make a design
		// decision it was explicitly not authorized for. The park uses §11's one
		// park mechanism, Loom-authored, so there is one resume path too.
		r.park(st.run, t, conflict)
		return err
	}
	return err
}

// park writes §9.2's producer-conflict declaration and moves the task to
// `blocked`. Durable row FIRST, file second: a file with no row is a park no
// query can see, and every recovery path in this package re-derives from rows.
func (r *Runner) park(run store.DelegationRun, t Task, c *ProducerConflict) {
	now := r.now()
	b := Block{
		Version: BlockVersion, Run: run.Slug, Task: t.ID, At: now,
		Kind:    BlockNeedsDecision,
		Author:  AuthorLoom,
		Summary: "two producers conflict in this task's base",
		Detail:  c.Error() + "\n\nconflicting files:\n" + strings.Join(c.Files, "\n"),
		ResumeWhen: "a human decides which producer owns these lines and the plan is amended; " +
			"Loom will not resolve this on the child's behalf",
	}
	if body, err := json.Marshal(b); err == nil {
		_ = r.Store.SetTaskBlock(run.ID, t.ID, string(body), now.Unix())
	}
	// From ANY of the states the failed spawn could have left behind. Spawn
	// releases its claim back to `approved` when the worktree step fails, but a
	// concurrent abandon may have moved it further; AdvanceTaskFromAnyCAS refuses
	// rather than resurrecting anything terminal.
	_, _, _ = r.Store.AdvanceTaskFromAnyCAS(run.ID, t.ID,
		[]string{string(StateApproved), string(StateSpawning), string(StateRunning)},
		string(StateBlocked), now.Unix())
	if r.Detector != nil {
		_ = r.Detector.Write(run.Slug, t.Repo, t.ID, b)
	}
}

// Check runs §8's check for one task and records the result under CAS, then
// computes §12.3's divergence AND §12.3.3's snapshot drift. Both, on every check
// run — the second is a different mechanism over different evidence and skipping
// it here would leave the pre-merge computation as the only one, which is too
// late to be a gate.
func (r *Runner) Check(ctx context.Context, runID int64, taskID string) (Result, error) {
	st, err := r.load(runID)
	if err != nil {
		return Result{}, err
	}
	t, ok := st.tasks[taskID]
	if !ok {
		return Result{}, fmt.Errorf("delegate: task %q is not part of run %d", taskID, runID)
	}
	row, ok := st.rows[taskID]
	if !ok || row.Worktree == "" {
		return Result{}, fmt.Errorf("delegate: task %q has no worktree to check", taskID)
	}

	// §2's M2 is measured HERE and nowhere else, because this is the only place
	// that holds both the verdict and the answer to "had the child stopped when
	// it ran". The observation is taken BEFORE the claim so it describes the
	// moment the check was decided on rather than the moment it finished, which
	// may be ten minutes and one more turn of the child's later.
	atRest := r.transcriptStateOf(row)

	// The claim comes first and it is a CAS. `blocked` is in the accepted set for
	// §10.3's re-attempt edge — a task Loom parked on a failed integration is
	// re-checked when its branch head moves — and `checking` is not, so two
	// Looms cannot both run one task's check.
	claimed, prev, err := r.Store.AdvanceTaskFromAnyCAS(runID, taskID,
		[]string{string(StateRunning), string(StateBlocked)}, string(StateChecking), r.now().Unix())
	if err != nil {
		return Result{}, err
	}
	if !claimed {
		return Result{}, fmt.Errorf("%w: task %q is %s, not checkable", ErrTaskMovedElsewhere, taskID, prev)
	}

	res := r.checker().Run(ctx, CheckRequest{
		RunID: runID, TaskID: taskID, Worktree: row.Worktree,
		Check: t.Check, Artifacts: t.Produces,
		// §10.2 points these at the INTEGRATION worktrees where there is one:
		// a check that reads a sibling repo must read the staged tree, not the
		// human's, or a green task is evidence about a tree nobody will merge.
		RepoDirs: st.repoDirsForCheck(),
	})

	// Where the check lands. A red check goes back to `running` and not to
	// `failed`: the child is alive, holds the context, and its remedy is to
	// commit again — §8.2's debounce will re-run this the moment it does.
	next := StateRunning
	if res.Status == CheckPass {
		next = StateVerified
	}
	recorded, err := r.Store.RecordTaskCheckCAS(runID, taskID, string(StateChecking), string(next),
		string(res.Status), int64(res.Exit), res.Output, res.BranchHead, res.RanAt.Unix())
	if err != nil {
		return res, err
	}
	if !recorded {
		// The task left `checking` while its own check was running — an abandon,
		// or the other Loom instance. Everything below this line is a consequence
		// OF the recorded result, so none of it may happen: publishing an
		// abandoned task's artifacts would unblock its consumers on work nobody
		// is going to merge, and §2's M2 would count a verdict no row carries.
		//
		// Returned as an error rather than swallowed. The previous shape dropped
		// `claimed` on the floor and reported a pass for a row that no longer
		// existed in that state, which is exactly the invisible failure §13.3's
		// discipline exists to prevent.
		return res, fmt.Errorf("%w: task %q left `checking` while its check ran; the result is discarded", ErrTaskMovedElsewhere, taskID)
	}

	// §2's M2, durable, once per task. See FlagFirstCheckGreen for why it is a
	// flag and why there are two of them.
	r.recordFirstCheck(runID, row, res, atRest)

	// §8.3: publish from the RESULT's own Published bit, not from a pass. A check
	// that fails after its artifacts were verified has published artifacts, and
	// reading publication off the status would leave delegation_artifacts empty
	// for every red task and strand its consumers as unready forever.
	if res.Published {
		r.publish(runID, t, row, res)
	}
	if res.EnvSuspect {
		_ = setTaskFlag(r.Store, runID, taskID, FlagEnvSuspect, true, res.RanAt.Unix())
	}

	// §12.3, BOTH mechanisms, on every check run. They are different mechanisms
	// over different evidence: the diff says what the child COMMITTED outside its
	// declared paths, the snapshot says what CHANGED outside its worktree
	// entirely. Skipping the second here would leave the pre-merge computation as
	// the only one, which is far too late to be a gate.
	r.recordDivergence(st, t, row)
	r.recordDrift(st, t, taskID)
	return res, nil
}

// publish records §8.3's verified artifacts. Errors are collected into nothing
// and dropped for the same reason every other per-artifact write in this package
// is: a run must not stop because one row would not write, and the next check
// re-publishes.
func (r *Runner) publish(runID int64, t Task, row store.DelegationTask, res Result) {
	for _, a := range t.Produces {
		_ = r.Store.UpsertDelegationArtifact(store.DelegationArtifact{
			RunID: runID, ArtifactID: a.ID, TaskID: t.ID, Path: artifactPath(a),
			Fingerprint: res.Fingerprints[a.ID],
			CommitSHA:   res.BranchHead,
			PublishedAt: res.RanAt.Unix(),
		})
	}
	_ = row
}

// recordDivergence is §12.3.1/2: what the child committed outside its declared
// paths, and what it committed inside a sibling's. Non-blocking; it sets
// `diverged` and is the thing §5.2's second acknowledgement is about.
func (r *Runner) recordDivergence(st *runState, t Task, row store.DelegationTask) {
	d, err := TaskDivergence(row.Worktree, row.BaseSHA, st.m, t)
	if err != nil {
		return
	}
	now := r.now().Unix()
	_ = r.Store.SetTaskDivergence(st.run.ID, t.ID, EncodeDivergence(d), now)
	_ = setTaskFlag(r.Store, st.run.ID, t.ID, FlagDiverged,
		len(d.Outside) > 0 || len(d.Siblings) > 0, now)
}

// recordDrift is §12.3.3: the out-of-worktree tripwire. It sets FlagOutsideWrites
// and NEVER FlagDiverged — the two findings have different mechanisms and
// different confidence, and one badge for both would let this one's disclosed
// false positives launder a real commit-level finding.
//
// Compare is used rather than Snapshot.Check() because only a caller holding the
// manifest can report NoBaseline for a task whose column is empty, and an
// absence of evidence must render as one rather than as "no change".
func (r *Runner) recordDrift(st *runState, t Task, taskID string) {
	row := st.rows[taskID]
	plan := PlanBase(st.e, st.m, t, st.rows, st.bases, st.integrationDirs())
	drift := Compare(DecodeSnapshot(row.SpawnSnapshot), TakeSnapshot(SnapshotDirs(st.m, plan)))
	if drift.Empty() {
		return
	}
	_ = setTaskFlag(r.Store, st.run.ID, taskID, FlagOutsideWrites, true, r.now().Unix())
}

// Merge is §5.2's action. It re-computes divergence and drift immediately before
// merging (§12.3, "again immediately before every merge — before, because a
// divergence discovered after a merge is a fact, not a gate"), re-checks the
// clean-tree precondition, and refuses unless the human's acknowledgements match
// what is currently on screen.
//
// `force` records FlagForced and is never a silent path: the flag is written for
// the record, and it is never read as permission by anything.
//
// The IntegrationResult is RETURNED, not swallowed. §10.4 step 2 re-derives the
// staging area from the user's branch head and re-runs the per-repo gate, and
// its findings — "the user's own branch is red after this merge", "the child's
// worktree was not removed" — arrive on that result's Warnings and nowhere else.
// Dropping it left the caller to infer them from the baseline column, which
// records the verdict but not the sentence, so the two most important things a
// human learns at the merge gate were the two things the gate could not say.
// On the refusal paths the result is the zero value: nothing ran.
func (r *Runner) Merge(ctx context.Context, runID int64, taskID string, ack MergeAck, force bool) (IntegrationResult, error) {
	var zero IntegrationResult
	if r.Integrator == nil {
		return zero, errors.New("delegate: Runner needs an Integrator to merge")
	}
	st, err := r.load(runID)
	if err != nil {
		return zero, err
	}
	t, ok := st.tasks[taskID]
	if !ok {
		return zero, fmt.Errorf("delegate: task %q is not part of run %d", taskID, runID)
	}
	// §14: merges are suppressed on a hidden project. Human-gated anyway, and
	// the gate is hidden.
	if r.hidden(st.run) {
		return zero, fmt.Errorf("%w: %s", ErrProjectHidden, st.run.ProjectRoot)
	}

	// The gate re-asserts its own precondition BEFORE computing a preview, so a
	// row that moved is reported as a moved row. Preview would also refuse — it
	// lists "the task is merged, not mergeable" among its blockers — but as a
	// BLOCKER, and the two mean opposite things to the caller: a blocker is a
	// finding to read and re-acknowledge, a moved row is a screen to re-read.
	// Squashing them loses the difference at the one gate where a human is about
	// to write to their own branch.
	//
	// `merged` and `abandoned` are refused even under force. §5.2's force is
	// permission to merge past an unacknowledged divergence or a red gate; it has
	// never meant "merge it a second time", and a terminal row is not a gate.
	row, ok := st.rows[taskID]
	if !ok {
		return zero, fmt.Errorf("%w: task %q has no row in run %d", ErrTaskMovedElsewhere, taskID, runID)
	}
	if s := TaskState(row.State); s != StateMergeable && (!force || s == StateMerged || s == StateAbandoned) {
		return zero, fmt.Errorf("%w: task %q is %s, not mergeable", ErrTaskMovedElsewhere, taskID, s)
	}

	// Preview RE-COMPUTES divergence and drift — it does not read the column —
	// which is §12.3's "again immediately before every merge". Before, because a
	// divergence discovered after a merge is a fact, not a gate.
	p, err := r.Integrator.Preview(st.run, st.m, t)
	if err != nil {
		return zero, err
	}
	if len(p.Blockers) > 0 && !force {
		return zero, fmt.Errorf("delegate: task %q cannot be merged: %s", taskID, strings.Join(p.Blockers, "; "))
	}

	// The acknowledgement is compared against what is CURRENTLY on screen, not
	// treated as a boolean. A preview computed at T and approved at T+5m may be
	// describing a different divergence, and "I acknowledged something" is not
	// consent to whatever is there now. The two lists are compared SEPARATELY
	// because the two findings have different mechanisms and different
	// confidence; one checkbox for both would let a false positive launder a
	// real finding.
	if diff := ackMismatch("scope divergence", ack.Divergence, divergedFiles(p.Divergence)); diff != "" && !force {
		return zero, fmt.Errorf("%w: %s", ErrAckStale, diff)
	}
	if diff := ackMismatch("changes outside the worktree", ack.Drift, driftFiles(p.Divergence.Drift)); diff != "" && !force {
		return zero, fmt.Errorf("%w: %s", ErrAckStale, diff)
	}

	// `force` is never a silent path: the flag is written for the record BEFORE
	// the merge, so a forced merge that then fails still carries the evidence
	// that it was forced. Nothing reads the flag as permission.
	if force {
		_ = setTaskFlag(r.Store, runID, taskID, FlagForced, true, r.now().Unix())
	}
	return r.Integrator.Merge(ctx, st.run, st.m, t, force)
}

// divergedFiles flattens §12.3.1 and .2 into the one list the human sees at the
// gate. Sorted and de-duplicated: a file that is both outside the task's paths
// and inside a sibling's is one acknowledgement, not two.
func divergedFiles(d DivergenceReport) []string {
	seen := map[string]bool{}
	var out []string
	add := func(fs []string) {
		for _, f := range fs {
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	add(d.Outside)
	for _, sib := range sortedKeys(d.Siblings) {
		add(d.Siblings[sib])
	}
	sort.Strings(out)
	return out
}

// ackMismatch names the difference between what the human acknowledged and what
// is there now, in both directions. Both directions matter: a file that appeared
// since the preview is new information the human never saw, and one that
// disappeared means they are approving a picture that no longer exists.
func ackMismatch(what string, acked, current []string) string {
	a := map[string]bool{}
	for _, f := range acked {
		a[f] = true
	}
	c := map[string]bool{}
	for _, f := range current {
		c[f] = true
	}
	var added, gone []string
	for _, f := range current {
		if !a[f] {
			added = append(added, f)
		}
	}
	for _, f := range acked {
		if !c[f] {
			gone = append(gone, f)
		}
	}
	if len(added) == 0 && len(gone) == 0 {
		return ""
	}
	sort.Strings(added)
	sort.Strings(gone)
	var b strings.Builder
	fmt.Fprintf(&b, "the %s changed since you looked", what)
	if len(added) > 0 {
		fmt.Fprintf(&b, "; new: %s", strings.Join(added, ", "))
	}
	if len(gone) > 0 {
		fmt.Fprintf(&b, "; no longer there: %s", strings.Join(gone, ", "))
	}
	b.WriteString(" — read it again and re-acknowledge")
	return b.String()
}

// MergeAck is the human's explicit second acknowledgement (§5.2). It carries
// what was acknowledged, not a boolean: a preview computed at T and approved at
// T+5m may be describing a different divergence, and "I acknowledged something"
// is not consent to whatever is there now.
type MergeAck struct {
	// Divergence is the acknowledged file list, compared against the freshly
	// computed one. A mismatch re-prompts rather than proceeding.
	Divergence []string
	// Drift is the acknowledged §12.3.3 file list, separately — the two findings
	// have different mechanisms and different confidence, and one checkbox for
	// both would let a false positive launder a real finding.
	Drift []string
}

// Abandon abandons a run: CAS every task, then SWEEP BY CWD over the run's
// deterministic worktree paths (§13.3's disclosed residual hole) so a child
// stranded by a lost CAS is still found by the one identity that exists the
// instant the child does. Nothing is killed and no branch is deleted — abandon
// stops Loom offering work, it does not destroy any.
func (r *Runner) Abandon(runID int64) error {
	st, err := r.load(runID)
	if err != nil {
		return err
	}
	now := r.now().Unix()
	var errs []error
	for _, id := range st.order {
		if _, err := r.Store.AbandonTaskCAS(runID, id, now); err != nil {
			errs = append(errs, fmt.Errorf("task %q: %w", id, err))
		}
	}
	if _, err := r.Store.AbandonDelegationRunCAS(runID, now); err != nil {
		errs = append(errs, err)
	}

	// §13.3's disclosed residual hole: a child stranded by a lost CAS still holds
	// a worktree, real quota and §6.4's shared ports, and the ONE identity that
	// exists the instant the child does is its cwd. The sweep finds it there.
	//
	// Nothing is killed and no branch is deleted. Abandon stops Loom OFFERING
	// work; it does not destroy any — Loom is a window, never an owner, and a
	// swept child may be mid-thought with an hour of irreplaceable context.
	if r.Watchdogs != nil {
		rows, serr := r.Watchdogs.SweepAbandon(st.run, st.m)
		if serr != nil {
			errs = append(errs, fmt.Errorf("sweep: %w", serr))
		}
		if len(rows) > 0 {
			names := make([]string, 0, len(rows))
			for _, row := range rows {
				names = append(names, row.Name)
			}
			sort.Strings(names)
			// An error value because it must be RENDERED: these sessions are still
			// live and still spending, and a sweep that reported them only into a
			// log is a sweep the human never learns the result of.
			errs = append(errs, fmt.Errorf("%w: %s", ErrChildrenStillLive, strings.Join(names, ", ")))
		}
	}
	return errors.Join(errs...)
}

// ─────────────────────────────────────────────────────────────────────────────
// §11.3 — dynamic amendments. Loom PROPOSES; a human GRANTS.

// Proposal is one amendment appended to the durable log this tick, in the shape
// the run view needs to offer it. The Amendment itself rather than an id,
// because the offer IS the proposal ("add `account-schema` to `auth-api`,
// produced by `schema`") and a caller that had to go and re-read the row to
// render it would be a second decoder of the same bytes.
type Proposal struct {
	Amendment
	// Replan is §11.3's second branch: the block names an artifact NOBODY
	// produces, so there is no edge to add and the offer is a re-plan request.
	// A separate bit and not an inference from Kind at every call site, because
	// the two render completely differently — one is a checkbox, the other is a
	// conversation with the plan's author.
	Replan bool
}

// propose turns every parked task's block into §11.3's amendment and appends it,
// unapproved, to the durable log.
//
// APPENDED, NEVER GRANTED. §11.3's whole shape is that Loom proposes and the
// human grants, and Accept refuses an amendment whose ApprovedAt is zero, so a
// row written here changes nothing about the graph until ApproveAmendment moves
// it. That is also why this is safe to run on every tick.
//
// Idempotent by proposal IDENTITY — (kind, task, artifact, producer, paths) —
// and deliberately NOT by the block: a child that re-words its block file has
// not encountered a second dependency, and counting it as one would inflate the
// one number §2 uses to decide whether this whole approach survives. The Reason
// is excluded from the identity for exactly that reason.
func (r *Runner) propose(st *runState, rep *TickReport) []Proposal {
	seen := map[string]bool{}
	for _, a := range st.e.Amendments {
		seen[amendmentKey(a)] = true
	}
	var out []Proposal
	for _, id := range st.order {
		row, ok := st.rows[id]
		if !ok || TaskState(row.State) != StateBlocked {
			continue
		}
		b, ok := st.e.Blocks[id]
		if !ok || b.Empty() {
			continue
		}
		a, perr := Propose(st.e, st.m, b)
		replan := errors.Is(perr, ErrNoSuchArtifact)
		if perr != nil && !replan {
			// A block Loom cannot turn into a proposal is still a parked child,
			// and the park is already rendered. The failure is collected so the
			// human can see WHY no offer appeared beside it.
			rep.Errs = append(rep.Errs, TaskError{TaskID: id, Stage: "propose", Err: perr})
			continue
		}
		if a.Kind == "" {
			// needs-decision and blocked-external imply no amendment (§12.1(b)).
			// Manufacturing a row for them would put entries in §2's M3 that are
			// not unforeseen dependencies at all — Propose's own words.
			continue
		}
		if seen[amendmentKey(a)] {
			continue
		}
		seen[amendmentKey(a)] = true
		seq, err := r.appendAmendment(st.run.ID, a)
		if err != nil {
			rep.Errs = append(rep.Errs, TaskError{TaskID: id, Stage: "propose", Err: err})
			continue
		}
		a.RunID, a.Seq = st.run.ID, seq
		a.CreatedAt = r.now()
		out = append(out, Proposal{Amendment: a, Replan: replan})
	}
	return out
}

// amendmentKey is a proposal's identity for the append-once rule. Reason is
// excluded — see propose.
func amendmentKey(a Amendment) string {
	return strings.Join([]string{string(a.Kind), a.Task, a.Artifact, a.From,
		strings.Join(a.Paths, "|")}, "\x00")
}

// ApproveAmendment is the human granting one proposal (§11.3), and the ORDER is
// the whole function:
//
//  1. VALIDATE first, against the current effective graph, with the approval
//     simulated. §11.3's last rule is that every amendment re-runs cycle
//     detection over the AMENDED graph and one that closes a loop is rejected —
//     and `approved_at` is a one-way CAS on an append-only table, so a cycle
//     discovered after the write could not be taken back.
//  2. CAS. Two Loom instances pressing approve on the same row produce ONE
//     grant; the loser is told its snapshot moved and does not retry.
//  3. Apply the side effect the kind implies. AmendEdge needs none — the edge is
//     folded by Effective on the next load, which is the same value the next
//     tick would compute anyway. AmendReplan needs none either: Loom never
//     invents a task (§16), so the row is a record of an offer a human now owes
//     the plan's author an answer to. Only AmendScope touches anything, and it
//     touches the child's brief.
//
// The side effect follows the CAS rather than preceding it for the reason
// ClaimTaskSpawnCAS precedes the worktree: the claim is what makes a double
// press produce one act. The cost is disclosed — a failure between the CAS and
// ApplyScope leaves an approved amendment whose brief rewrite is still owed —
// and it is REPORTED rather than swallowed, so the human can press it again.
//
// DISCLOSED RACE, and it is the same shape §13.3 discloses for spawn: two
// instances approving two individually-acyclic amendments can jointly close a
// cycle, because step 1 validates against the graph each of them read. Nothing
// silently breaks — the cycle then has no ready task and §9.3's deadlock
// detector renders it as a wait-for cycle on the next tick, which is exactly the
// mechanism that exists for cycles this one cannot see.
func (r *Runner) ApproveAmendment(runID, seq int64) error {
	st, err := r.load(runID)
	if err != nil {
		return err
	}
	var a Amendment
	found := false
	for _, cand := range st.e.Amendments {
		if cand.Seq == seq {
			a, found = cand, true
			break
		}
	}
	if !found {
		return fmt.Errorf("delegate: run %d has no amendment %d", runID, seq)
	}
	if a.Accepted() {
		return fmt.Errorf("%w: amendment %d is already approved", ErrAmendmentClaimed, seq)
	}

	now := r.now()
	// Step 1. The copy carries the approval the CAS has not made yet: Accept
	// refuses an unapproved amendment by design (ErrAmendmentNotApproved), so
	// validating the real value would answer the wrong question — "may this be
	// folded now", rather than "would folding it close a cycle".
	candidate := a
	candidate.ApprovedAt = now
	if _, err := Accept(st.e, candidate); err != nil {
		return err
	}

	// Step 2.
	claimed, err := r.approveAmendmentCAS(runID, seq, now.Unix())
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("%w: amendment %d was approved by someone else", ErrAmendmentClaimed, seq)
	}

	// Step 3.
	if candidate.Kind != AmendScope {
		return nil
	}
	if r.Rendezvous == nil {
		return fmt.Errorf("delegate: amendment %d is approved, but a scope widening needs a Rendezvous to reach the child", seq)
	}
	t, ok := st.tasks[candidate.Task]
	if !ok {
		return fmt.Errorf("delegate: amendment %d is approved, but task %q is not part of run %d", seq, candidate.Task, runID)
	}
	if err := r.Rendezvous.ApplyScope(st.run, t, candidate); err != nil {
		return fmt.Errorf("delegate: amendment %d is approved but its brief rewrite is still owed: %w", seq, err)
	}
	return nil
}

// AmendmentLog is the run's whole amendment log, decoded, approved or not. The
// run view needs it and must not decode `body` a second time — a second decoder
// is a second opinion about what an amendment says.
func (r *Runner) AmendmentLog(runID int64) []Amendment { return r.amendments(runID) }

// appendAmendment and approveAmendmentCAS route through the injected
// AmendmentStore when there is one and through the store otherwise. Two paths
// because AmendmentStore predates the table and still exists as the seam a test
// injects at; the store path is the real one.
func (r *Runner) appendAmendment(runID int64, a Amendment) (int64, error) {
	if r.Amendments != nil {
		return r.Amendments.Append(runID, a)
	}
	return r.Store.AppendDelegationAmendment(runID, string(a.Kind), EncodeAmendmentBody(a), r.now().Unix())
}

func (r *Runner) approveAmendmentCAS(runID, seq, now int64) (bool, error) {
	if r.Amendments != nil {
		return r.Amendments.Approve(runID, seq, now)
	}
	return r.Store.ApproveDelegationAmendmentCAS(runID, seq, now)
}

func (r *Runner) rejectAmendmentCAS(runID, seq, now int64) (bool, error) {
	if r.Amendments != nil {
		return r.Amendments.Reject(runID, seq, now)
	}
	return r.Store.RejectDelegationAmendmentCAS(runID, seq, now)
}

// RejectAmendment is the human declining one proposal (§11.3), made durable.
//
// It is the mirror of ApproveAmendment with two of the three steps missing, and
// the absences are the point:
//
//  1. NO validation pass. Cycle detection asks "would folding this close a
//     loop", and nothing is being folded. Refusing to record a NO because the
//     graph it would have produced is bad is the wrong answer to the right
//     question.
//  2. CAS, guarded on BOTH decision columns, so an approve and a reject racing
//     from two Loom instances produce exactly one decision and the loser is
//     told rather than overwriting.
//  3. NO side effect. Approval rewrites a brief and re-seeds a child; a
//     rejection touches nothing, which is why it can be the one decision in
//     this file that is safe to record before anything else happens.
//
// The row is NOT deleted and Effective keeps ignoring it, exactly as it ignored
// it while it was merely unapproved. The record survives because a parked child
// is waiting on the far side of this decision and a human reading the run an
// hour later needs to see that the answer was no rather than that the question
// was never asked. It also keeps `propose` idempotent: the append-once rule is
// keyed on the proposal's identity across the WHOLE log, so a rejected amendment
// that vanished would be re-proposed on the very next tick.
func (r *Runner) RejectAmendment(runID, seq int64) error {
	st, err := r.load(runID)
	if err != nil {
		return err
	}
	var a Amendment
	found := false
	for _, cand := range st.e.Amendments {
		if cand.Seq == seq {
			a, found = cand, true
			break
		}
	}
	if !found {
		return fmt.Errorf("delegate: run %d has no amendment %d", runID, seq)
	}
	switch {
	case a.Accepted():
		return fmt.Errorf("%w: amendment %d is already approved", ErrAmendmentClaimed, seq)
	case a.Rejected():
		return fmt.Errorf("%w: amendment %d was already rejected", ErrAmendmentClaimed, seq)
	}

	claimed, err := r.rejectAmendmentCAS(runID, seq, r.now().Unix())
	if err != nil {
		return err
	}
	if !claimed {
		// The row moved between the load and the CAS — the other instance
		// approved it, or rejected it first. Either way the caller is holding a
		// stale screen and must re-read rather than retry.
		return fmt.Errorf("%w: amendment %d was decided by someone else", ErrAmendmentClaimed, seq)
	}
	return nil
}

// ErrAmendmentClaimed is the approval CAS refusing: the row was already granted,
// by the other Loom instance or by a second press of the same button. Its own
// sentinel because the caller must NOT retry — the amendment is approved, and
// what the caller is holding is a stale screen.
var ErrAmendmentClaimed = errors.New("delegate: this amendment has already been approved")

// ─────────────────────────────────────────────────────────────────────────────
// §2 — the measurements, and why they are computed rather than stored.

// The two flags M2 is folded over — FlagFirstCheckGreen and FlagFirstCheckRed —
// live with the rest of the vocabulary in state.go; only the writer and the fold
// are here, next to the check they are written from.

// recordFirstCheck writes M2's verdict once per task, and never again — the
// SECOND check's outcome is the thing M2 exists to be different from.
func (r *Runner) recordFirstCheck(runID int64, row store.DelegationTask, res Result, at transcript.State) {
	if at != transcript.StateIdle && at != transcript.StateNeedsYou {
		return
	}
	f := DecodeFlags(row.Flags)
	if f[FlagFirstCheckGreen] || f[FlagFirstCheckRed] {
		return
	}
	flag := FlagFirstCheckRed
	if res.Status == CheckPass {
		flag = FlagFirstCheckGreen
	}
	// Dropped on error like every other flag write in this package: a run must
	// not stop because a badge would not write. It costs one task's contribution
	// to a measurement, and the measurement says so — see Measurements.Unmeasured.
	_ = setTaskFlag(r.Store, runID, row.TaskID, flag, true, res.RanAt.Unix())
}

// Measurements is §2's M2 and M3 for ONE run, computed on read.
//
// §2 is binding and it is a schedule constraint with a kill criterion: "Build
// §§9–12 in full only if, on at least one real initiative of ≥4 tasks: M3 ≤ 1
// per 4 tasks and M2 ≥ 0.5." Those two numbers decide whether this approach
// survives, which is why they are computed by the runner from evidence it
// already holds rather than reconstructed afterwards from a run nobody kept.
//
// Nothing here is stored and nothing accumulates across runs. §2 records the
// measurements "per initiative, in the spec's follow-up, not in the DB", so this
// is a fold over one run's amendment log and one run's task flags, recomputed
// every tick and thrown away.
type Measurements struct {
	Tasks int

	// M3 — "count of unforeseen cross-task dependencies encountered per task".
	//
	// ENCOUNTERED, not granted: the log is counted whether or not the human
	// approved the amendment, because a dependency the human refused to add was
	// still hit by a child that could not proceed without it. M3 measures
	// inter-task cohesion, and cohesion does not care what the human decided.
	Unforeseen map[string]int
	// UnforeseenTotal and PerFourTasks are the numerator and §2's own unit. The
	// unit is per-four-tasks and not per-task because that is how the kill
	// criterion is written ("M3 ≤ 1 per 4 tasks"), and restating it as 0.25
	// invites a reader to compare it against the wrong threshold.
	UnforeseenTotal int
	PerFourTasks    float64
	// ScopeWidenings is deliberately NOT part of M3 and is reported beside it so
	// nobody folds it in later. A `needs-scope` block is a child asking to write
	// somewhere else in its OWN repo; it is an authorization argument, not a
	// cross-task dependency, and counting it would make M3 fail for a plan whose
	// tasks never touched each other.
	ScopeWidenings int

	// M2 — the fraction of tasks whose check was green the first time it ran
	// after the child stopped.
	GreenFirstTime []string
	RedFirstTime   []string
	// Unmeasured names the tasks with no first-check verdict yet. It is reported
	// rather than folded into either side: an unfinished run must not read as a
	// failing one, and a task whose flag write was dropped must not read as red.
	Unmeasured []string
	Fraction   float64

	// M2Met / M3Met are §2's thresholds evaluated, and Enough is its "≥4 tasks"
	// precondition. Three bools rather than one verdict because a run that fails
	// only M3 has a completely different consequence in §2's decision rule — the
	// salvage path is the single-delegated-child subset — from one that fails
	// only M2.
	M2Met, M3Met, Enough bool
	// Provisional is true while any task has no verdict. §2's rule is about a
	// finished initiative, and a fraction over three checked tasks out of twelve
	// is not the number the rule is written about.
	Provisional bool
}

// Verdict is the measurement as a sentence, for the run view and for whoever
// writes the spec's follow-up. It never says "passed" while Provisional: a
// half-run initiative reading as a green kill-criterion is how a decision gets
// made on a number nobody finished measuring.
func (m Measurements) Verdict() string {
	var b strings.Builder
	fmt.Fprintf(&b, "M3 %d unforeseen dependencies over %d tasks (%.2f per 4 tasks, threshold 1)",
		m.UnforeseenTotal, m.Tasks, m.PerFourTasks)
	if len(m.GreenFirstTime)+len(m.RedFirstTime) == 0 {
		b.WriteString("; M2 not measurable yet — no task has been checked after its child stopped")
	} else {
		fmt.Fprintf(&b, "; M2 %d of %d checked tasks green first time (%.2f, threshold 0.50)",
			len(m.GreenFirstTime), len(m.GreenFirstTime)+len(m.RedFirstTime), m.Fraction)
	}
	switch {
	case !m.Enough:
		fmt.Fprintf(&b, " — §2's rule needs an initiative of at least 4 tasks; this run has %d, so it decides nothing", m.Tasks)
	case m.Provisional:
		fmt.Fprintf(&b, " — PROVISIONAL: %d of %d tasks have no first-check verdict yet", len(m.Unmeasured), m.Tasks)
	case m.M2Met && m.M3Met:
		b.WriteString(" — both thresholds met")
	case !m.M3Met:
		b.WriteString(" — M3 IS OVER THRESHOLD: §2's decision rule says the answer is not parallel children, it is the single-delegated-child subset")
	default:
		b.WriteString(" — M2 IS UNDER THRESHOLD: isolated children are not yielding green work first time")
	}
	return b.String()
}

// measure folds one run's log and flags into §2's two numbers. Pure over a
// loaded runState, so every row of §2's decision rule is table-testable without
// a store.
func measure(st *runState) Measurements {
	m := Measurements{Tasks: len(st.order), Unforeseen: map[string]int{}}
	for _, a := range st.e.Amendments {
		switch a.Kind {
		case AmendEdge, AmendReplan:
			if a.Task == "" {
				continue
			}
			m.Unforeseen[a.Task]++
			m.UnforeseenTotal++
		case AmendScope:
			m.ScopeWidenings++
		}
	}
	for _, id := range st.order {
		f := DecodeFlags(st.rows[id].Flags)
		switch {
		case f[FlagFirstCheckGreen]:
			m.GreenFirstTime = append(m.GreenFirstTime, id)
		case f[FlagFirstCheckRed]:
			m.RedFirstTime = append(m.RedFirstTime, id)
		default:
			m.Unmeasured = append(m.Unmeasured, id)
		}
	}
	if m.Tasks > 0 {
		m.PerFourTasks = float64(m.UnforeseenTotal) * 4 / float64(m.Tasks)
	}
	if checked := len(m.GreenFirstTime) + len(m.RedFirstTime); checked > 0 {
		m.Fraction = float64(len(m.GreenFirstTime)) / float64(checked)
	}
	m.Enough = m.Tasks >= 4
	m.Provisional = len(m.Unmeasured) > 0
	// The thresholds, spelled exactly as §2 spells them.
	m.M3Met = m.PerFourTasks <= 1
	m.M2Met = len(m.GreenFirstTime)+len(m.RedFirstTime) > 0 && m.Fraction >= 0.5
	return m
}

// Measure is §2's numbers for one run, on demand — the same value Tick reports,
// for the caller that wants them without a poll.
func (r *Runner) Measure(runID int64) (Measurements, error) {
	st, err := r.load(runID)
	if err != nil {
		return Measurements{}, err
	}
	return measure(st), nil
}

// liveness answers §6.3's question about ONE task — is the bound child session
// still breathing? — and is the whole of the store access that row needs. It
// lives here rather than in Watch because Watch is a pure pass over already-read
// facts; three-valued because a failed read is not a death (see Liveness).
func (r *Runner) liveness(row store.DelegationTask) Liveness {
	if row.SessionName == "" {
		return LivenessUnknown
	}
	sess, found, err := r.Store.Get(row.SessionName)
	if err != nil {
		return LivenessUnknown
	}
	// A row that is GONE is a death, not an unknown: the session was named on the
	// task, so it existed, and a name that no longer resolves is the strongest
	// evidence of an ended child there is.
	if found && sess.EndedAt == -1 {
		return LivenessAlive
	}
	return LivenessDead
}

// ─────────────────────────────────────────────────────────────────────────────
// The loaded picture, and the small readers every stage shares.

// runState is ONE tick's worth of reads. It exists so §9's steps are computed
// from one consistent snapshot rather than each re-querying: a poll loop that is
// O(tasks) round-trips is exactly the cost §6.6 lists as a cap justification,
// and steps computed from different reads can disagree about the same task.
type runState struct {
	run   store.DelegationRun
	m     Manifest
	tasks map[string]Task
	rows  map[string]store.DelegationTask
	// layout is the Runner's, carried so the per-repo integration paths are
	// derived from the SAME function Ensure created them with.
	layout Layout
	// order is manifest order, so two Loom instances iterate identically and
	// render the same list.
	order     []string
	states    map[string]TaskState
	published map[string]bool
	bases     map[string]string
	e         EffectiveGraph
}

// integrationDirs is the run's per-repo integration worktree map, decoded from
// delegation_runs.integration. PlanBase needs it and cannot derive it: without
// it AddDirs is uncomputable and §9.2's cross-repo half silently does nothing,
// which is a child launched unable to SEE what it declares it needs.
//
// It falls back to the DETERMINISTIC path when the column has no entry for a
// repo, because Layout.IntegrationDir is the same function Ensure created the
// worktree with — a missing column entry means "not recorded", not "not there",
// and treating the two the same would drop a real grant.
func (s *runState) integrationDirs() map[string]string {
	out := map[string]string{}
	for _, label := range inScopeRepos(s.m) {
		out[label] = physicalPath(s.layout.IntegrationDir(s.run.Slug, label))
	}
	return out
}

// repoDirsForCheck is the LOOM_REPO_<LABEL> map a check runs with. §10.2 points
// these at the integration worktrees rather than the human's primary trees: a
// check that reads a sibling repo must read the STAGED tree, or a green task is
// evidence about a tree nobody is going to merge.
func (s *runState) repoDirsForCheck() map[string]string {
	out := map[string]string{}
	for label, dir := range s.m.RepoPaths {
		out[label] = dir
	}
	for label, dir := range s.integrationDirs() {
		if dir != "" && dirExists(dir) {
			out[label] = dir
		}
	}
	return out
}

func (r *Runner) load(runID int64) (*runState, error) {
	if r.Store == nil {
		return nil, errors.New("delegate: Runner needs a Store")
	}
	run, ok, err := r.Store.GetDelegationRun(runID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("delegate: no delegation run %d", runID)
	}
	var m Manifest
	if err := json.Unmarshal([]byte(run.ManifestJSON), &m); err != nil {
		return nil, fmt.Errorf("delegate: run %s has an unreadable manifest snapshot: %w", run.Slug, err)
	}
	// Rehydrate what the snapshot deliberately does not carry (see Runner.Repos).
	// From the RUN ROW's project root and not from the manifest's `project`
	// name: §3's containment was decided when the run was created, and
	// re-resolving a display name years later is how a run moves to a different
	// project because somebody renamed one.
	m.ProjectRoot = run.ProjectRoot
	if r.Repos != nil {
		m.RepoPaths = r.Repos(run.ProjectRoot)
	}

	rows, err := r.Store.ListDelegationTasks(runID)
	if err != nil {
		return nil, err
	}
	arts, err := r.Store.ListDelegationArtifacts(runID)
	if err != nil {
		return nil, err
	}

	st := &runState{
		run: run, m: m, layout: r.Layout,
		tasks: map[string]Task{}, rows: map[string]store.DelegationTask{},
		states: map[string]TaskState{}, bases: map[string]string{},
	}
	for _, t := range m.Tasks {
		st.tasks[t.ID] = t
		st.order = append(st.order, t.ID)
	}
	blocks := map[string]Block{}
	for _, row := range rows {
		st.rows[row.TaskID] = row
		st.states[row.TaskID] = TaskState(row.State)
		if b, ok := DecodeBlockRow(row.BlockJSON); ok {
			blocks[row.TaskID] = b
		}
	}
	st.published = PublishedSet(arts)
	_ = json.Unmarshal([]byte(run.BaseSHAs), &st.bases)
	st.e = Effective(m, r.amendments(runID), blocks)
	// Effective builds its own Blocks from the argument; re-asserting it here
	// would be a second source of truth. The assignment below is only for the
	// case where Effective was handed nothing.
	if len(st.e.Blocks) == 0 && len(blocks) > 0 {
		st.e.Blocks = blocks
	}
	return st, nil
}

// amendments reads the durable log. A nil Amendments store degrades to "no
// amendments", which is exactly a run with none — the graph is then the declared
// one, which is the correct answer and not a silent failure.
func (r *Runner) amendments(runID int64) []Amendment {
	if r.Amendments != nil {
		as, err := r.Amendments.List(runID)
		if err != nil {
			return nil
		}
		return as
	}
	rowsAmd, err := r.Store.ListDelegationAmendments(runID)
	if err != nil {
		return nil
	}
	out := make([]Amendment, 0, len(rowsAmd))
	for _, row := range rowsAmd {
		// One decoder, shared with EffectiveFromRows. A body that will not parse
		// still RENDERS, with the kind and seq intact — an invisible amendment is
		// an edge the human cannot see and cannot revoke.
		out = append(out, DecodeAmendmentRow(row))
	}
	return out
}

// Deadlock is §12.1's finding over ALREADY-PERSISTED state: nil when the run is
// progressing, otherwise the wait-for cycle or the list of decisions owed.
//
// It reads and computes; it writes NOTHING. That is the whole reason it exists
// beside Tick, which reaches the same verdict — Tick runs checks, integrations
// and seed deliveries and flips the run to `deadlocked` on the way, so a view
// that had to call it to learn WHY a run is red would be advancing the run in
// order to render it. §12.1 asks for the cycle on the poll, and a poll must not
// be a writer.
//
// Progress is recomputed here rather than taken from the caller, and it is the
// same pure Tick() over the same loaded state, so the verdict cannot disagree
// with the runner's for any state the runner did not itself just change.
//
// At is left zero deliberately. DetectDeadlock does not stamp one and a
// read-only detector must not invent one: a "detected at" that moved on every
// poll would render as a deadlock that keeps re-happening.
func (r *Runner) Deadlock(runID int64) (*Deadlock, error) {
	st, err := r.load(runID)
	if err != nil {
		return nil, err
	}
	return DetectDeadlock(st.e, Tick(st.e, st.states, st.published), st.states), nil
}

// watch assembles §12.2's observations and runs the pure pass. Every field is a
// FACT ALREADY READ — the transcript state comes from the session row the status
// engine maintains, matched by SHAPE, never scraped from a pane.
//
// WatchCheckTimeout is never emitted from here: check.go already owns §8.1's
// timeout, and a second one is how two components come to disagree about whether
// a check is still running.
func (r *Runner) watch(st *runState, now time.Time) []Finding {
	obs := make([]Observation, 0, len(st.order))
	spawned := 0
	for _, id := range st.order {
		row, ok := st.rows[id]
		if !ok {
			continue
		}
		if row.SessionName != "" {
			spawned++
		}
		o := Observation{
			TaskID: id, State: TaskState(row.State),
			Since:          time.Unix(row.UpdatedAt, 0),
			LastBranchHead: row.BranchHead,
			BranchHead:     r.branchHead(row),
			PendingSeed:    row.PendingSeed,
			Flags:          DecodeFlags(row.Flags),
			Busy:           r.Integrator.Busy(st.run.ID),
		}
		if row.PendingSeed != "" {
			// When the DEBT was incurred, from its own durable column
			// (delegation_tasks.seed_owed_since, migration v17) and never from the
			// row's updated_at.
			//
			// updated_at was the previous answer and it made §12.2's `block-stale`
			// row unreachable in exactly the case it exists for. SetTaskPendingSeed
			// stamps updated_at, Tick re-attempts delivery on every poll, and a
			// child that never returns to a prompt is re-attempted forever — so the
			// debt was permanently zero seconds old and the one escalation a parked
			// child has could never fire. A pre-v17 row has 0 here; falling back to
			// Since keeps such a row watchable rather than silently exempt, and the
			// worst that fallback can do is what the old code always did.
			o.UnblockedAt = time.Unix(row.SeedOwedSince, 0)
			if row.SeedOwedSince == 0 {
				o.UnblockedAt = o.Since
			}
		}
		o.TranscriptState, o.TranscriptAt = r.transcript(row)
		o.Child = r.liveness(row)
		obs = append(obs, o)
	}
	f := Watch(now, obs, Budget{StartedAt: time.Unix(st.run.CreatedAt, 0), Spawned: spawned})
	// Run-scoped findings (empty TaskID) sort first: a budget that has stopped
	// every spawn explains every other row on the screen, and reading it last
	// means reading eleven task findings that all have one cause.
	sort.SliceStable(f, func(i, j int) bool {
		return (f[i].TaskID == "") && (f[j].TaskID != "")
	})
	return f
}

// transcript reads the child's engine-derived state from the session row rather
// than re-reading a transcript file. The status engine already owns that read
// and writes its verdict to sessions.last_status; a second reader here would be
// a second classifier that can disagree with the one the rest of Loom renders.
func (r *Runner) transcript(row store.DelegationTask) (transcript.State, time.Time) {
	if row.SessionName == "" {
		return transcript.StateUnknown, time.Time{}
	}
	sess, ok, err := r.Store.Get(row.SessionName)
	if err != nil || !ok {
		return transcript.StateUnknown, time.Time{}
	}
	return parseTranscriptState(sess.LastStatus), time.Unix(row.UpdatedAt, 0)
}

// parseTranscriptState is transcript.State.String() read backwards. A string
// nobody recognises is StateUnknown, not a panic: the column is written by
// another component and a vocabulary that grows there must cost a watchdog's
// precision, never a tick.
func parseTranscriptState(s string) transcript.State {
	switch strings.TrimSpace(s) {
	case "running":
		return transcript.StateRunning
	case "needs_you":
		return transcript.StateNeedsYou
	case "idle":
		return transcript.StateIdle
	}
	return transcript.StateUnknown
}

// transcriptStateOf is the state half of transcript(), for the callers that do
// not need the timestamp.
func (r *Runner) transcriptStateOf(row store.DelegationTask) transcript.State {
	s, _ := r.transcript(row)
	return s
}

// driftFiles flattens §12.3.3's per-directory finding into the one acknowledgeable
// list. The DIRECTORY IS KEPT in each entry: "config.json changed" in a repo the
// human is working in and in an integration worktree are different findings, and
// collapsing them to a bare basename would let one acknowledgement cover both.
func driftFiles(d SnapshotDrift) []string {
	var out []string
	for _, dir := range sortedKeys(d.Changed) {
		for _, f := range d.Changed[dir] {
			out = append(out, filepath.Join(dir, f))
		}
	}
	sort.Strings(out)
	return out
}

// dirExists reports whether a path is a directory. A stat error is "no": an
// integration worktree that cannot be statted must not be handed to a check as
// a repo dir, and falling back to the human's primary tree is the safe answer.
func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// lastCheckedHead is §8.2's "since the last check", with the BASE standing in
// until a check has actually run.
//
// The column alone is wrong at exactly one moment and it is the moment every
// task passes through. A freshly spawned task has branch_head "" and a real head
// — the base commit the worktree was branched from — so ShouldRun reads
// "" != <base> as "the child has committed", and the first tick that finds the
// child at rest runs a full check against a tree the child has not touched. That
// check is guaranteed red or unpublished, it spends the check's timeout, and
// under §2's M2 it would record a first-check-red verdict for every task in
// every run before any child had done anything at all — poisoning the one number
// §2's kill criterion turns on.
//
// The fix belongs here and not in ShouldRun: ShouldRun is handed two shas and
// cannot know that one of them is a base, while the caller holds the row that
// records it. Rejected alternative: seeding branch_head with the base at claim
// time, which would make the column mean two different things (a sha a check ran
// against, and a sha no check has run against) and would silently break every
// query that reads it as evidence of a completed check.
func lastCheckedHead(row store.DelegationTask) string {
	if row.BranchHead != "" {
		return row.BranchHead
	}
	return row.BaseSHA
}

// branchHead is the child branch's current head, read from the worktree. An
// unreadable one yields "", which every caller treats as "unchanged" — a git
// failure must cost a debounce, never a run.
func (r *Runner) branchHead(row store.DelegationTask) string {
	if row.Worktree == "" {
		return ""
	}
	return worktreeHead(row.Worktree)
}

// hidden is §14's one bit. It is asked about the RUN'S PROJECT and never about a
// child's cwd: a delegated child's worktree and its .meta add-dir match no
// project target, projects.Resolver.Visible ANDs over cwd ∪ add_dirs and fails
// CLOSED, so routing a child through the raw resolver hides every child the
// moment anything is hidden. That is the exact bug §14.1 exists to fix.
func (r *Runner) hidden(run store.DelegationRun) bool {
	return r.Hidden != nil && r.Hidden(run.ProjectRoot)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) checker() *Checker {
	if r.Checker != nil {
		return r.Checker
	}
	return &Checker{}
}

// DecodeBlockRow parses the durable copy of a block declaration. A row that will
// not parse yields no block rather than an error: the task is still `blocked` by
// its state column, which is the durable fact, and `block-malformed` is the flag
// that renders the discrepancy (§11.2).
//
// Exported for EffectiveFromRows, so a read-only caller building the effective
// graph decides "is this task blocked" with the same leniency the scheduler
// does. It is deliberately NOT ParseBlock: that one validates a child's freshly
// written file and rejects what it will not honour, whereas this reads a copy
// Loom itself wrote and must never lose a live block to a validation rule that
// was tightened after the row landed.
func DecodeBlockRow(s string) (Block, bool) {
	if strings.TrimSpace(s) == "" {
		return Block{}, false
	}
	var b Block
	if err := json.Unmarshal([]byte(s), &b); err != nil {
		return Block{}, false
	}
	if b.Empty() {
		return Block{}, false
	}
	return b, true
}

// ErrProjectHidden is §14's refusal of NEW Loom-initiated work on a hidden
// project. A distinct sentinel and not a generic error because the UI must grey
// the action WITH THIS REASON rather than drop it — an action that vanishes
// makes a suppressed run look like a stalled one.
var ErrProjectHidden = errors.New("delegate: the run's project is hidden; new work is not offered")

// ErrRunRed is §10.2's "spawning stops" as a refusal. See Approve for why the
// refusal has to live at the gate rather than in the component that sets the
// status.
var ErrRunRed = errors.New("delegate: the run is red; spawning is stopped")

// ErrAckStale is §5.2's second acknowledgement refusing a picture that has
// moved. Its own sentinel so a caller can re-prompt rather than report a failure.
var ErrAckStale = errors.New("delegate: the merge acknowledgement no longer matches what is on screen")

// ErrChildrenStillLive is Abandon's sweep finding live sessions at the run's
// deterministic worktree paths. NOT a failure of the abandon — the rows are all
// abandoned — but the human must be told, because those children are still
// spending and Loom will not end them.
var ErrChildrenStillLive = errors.New("delegate: children are still live at this run's worktrees (nothing was killed)")

// AmendmentStore is the durable amendment log (§13.1's delegation_amendments).
//
// The table exists now and *store.Store implements all three operations, so a
// nil AmendmentStore is the ORDINARY case and this interface is the seam a test
// injects at. It is kept because it states the ONE rule that matters at the
// seam and states it where a future implementor will read it: Approve is a CAS
// on (run, seq), so two Loom instances approving the same amendment cannot
// produce two edges.
type AmendmentStore interface {
	// Append writes a proposal. Seq is assigned by the store, monotonically per
	// run, and returned.
	Append(runID int64, a Amendment) (seq int64, err error)
	// Approve is the CAS: it must set approved_at only where it is currently 0,
	// and report claimed=false otherwise.
	Approve(runID, seq int64, now int64) (claimed bool, err error)
	// Reject is the same CAS for the human's NO, and it must guard on BOTH
	// decision columns: an implementation that only checked rejected_at would
	// let a rejection overwrite an approval whose side effect has already run.
	Reject(runID, seq int64, now int64) (claimed bool, err error)
	// List returns the whole log for a run, ordered by seq, approved or not.
	List(runID int64) ([]Amendment, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// HANDOFF NOTES — files this package does not own.
//
// The store notes are DONE and are not restated: the amendments table, its CAS,
// SetTaskSpawnSnapshot, the run-integration CAS and the integrating/mergeable
// transitions all exist. So are the two that used to head this list — the M2
// flags now live in state.go with the rest of the vocabulary, and §6.3's
// dead-child row is a row of Watch (WatchChildGone) over Observation.Child
// rather than a second pass in the caller. What is left:
//
// internal/delegate/manifest.go (nobody owns it this wave):
//
//   - Manifest should eventually carry the `integration` block as a field with
//     load-time validation (unknown repo label, unknown needs_repos entry, empty
//     cmd, cwd escaping its repo — §4.4 rule 7 applies to it verbatim).
//     IntegrationOf decoding the snapshot is the seam that avoids editing the
//     frozen loader now; it is not the end state.
//
// cmd/loom-gui/orchestration.go — the four notes it carried are DONE: the GUI
// builds a long-lived Runner (App.deleg) with Repos and Hidden supplied from the
// projects resolver, its refreshReady is a read of the rows behind Tick step
// 3b's promotion, TickDelegationRun renders the findings, the offers and §2's
// verdict, and ApproveDelegationAmendment is §11.3's press. What is left there:
//
//   - App.ApproveTask still runs its own ready→approved CAS plus Spawner.Spawn
//     rather than Runner.Approve, so the §§9-12 half of the spawn — §9.2's
//     producer merge and §12.3.3's spawn snapshot — never happens on the path a
//     human actually presses. It is a duplicate of Approve with a narrower body,
//     and the reason it survives is that it returns the session NAME (Approve
//     returns only an error) and the GUI needs it. Approve should grow that
//     return, or the binding should read the bound session back off the row.
//   - Nothing calls Tick on a timer yet. The bindings tick on demand — create,
//     refresh, check, grant — which means a run with no human looking at it does
//     not advance: no checks, no integrations, no seed deliveries, no watchdogs.
//     §11.2's cadence is the run view's own 2s (BlockPoll) and that is the poll
//     loop this is owed.
// ─────────────────────────────────────────────────────────────────────────────
