package delegate

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// TaskState is the value of `delegation_tasks.state` — §13.2's single
// CAS-guarded column. It is a string, not an int, because the column is a
// string and a schema whose meaning survives `sqlite3 loom.db` is worth more
// than four bytes a row.
//
// The full §13.2 path, now that §§9-12 are being built:
//
//	pending → ready → approved → spawning → running ⇄ blocked
//	        → checking → verified | failed
//	        → integrating → mergeable → merged
//
// plus `abandoned` from anywhere.
//
// `integrating` and `mergeable` were deliberately absent in 3a — they are only
// reachable once LOOM runs the merge (§10), which 3a did not do, and two
// unreachable states in every switch is a cost the compiler will not tell anyone
// about. §§9-12 makes them reachable, so they are here now and not one release
// earlier.
//
// Note what `integrating` does NOT mean: it is not "being merged into the user's
// branch". It is "§10.2's sequence is running against the per-repo INTEGRATION
// worktree", which is a test bed and, BINDING per §10.1, never a merge source
// into anything the user owns. The state after the human approves at §5.2 is
// `merged`, and it is reached from `mergeable` directly.
type TaskState string

const (
	StatePending  TaskState = "pending"
	StateReady    TaskState = "ready"
	StateApproved TaskState = "approved"
	StateSpawning TaskState = "spawning"
	StateRunning  TaskState = "running"
	StateBlocked  TaskState = "blocked"
	StateChecking TaskState = "checking"
	StateVerified TaskState = "verified"
	StateFailed   TaskState = "failed"
	// StateIntegrating is §10.2's sequence in flight for this task: the task's
	// branch is being merged into its repo's integration worktree and the
	// per-repo and cross checks are being run there. It is Loom-driven and
	// SERIALIZED RUN-WIDE (§10.2), so at most one task of a run is ever in it.
	//
	// There is no `integration_blocked` state. §10.2 names one, but §10.3 is
	// explicit that the remedy is "parked using the SAME MECHANISM as a
	// rendezvous" — Loom writes a pending_seed and delivers it to the still-live
	// child. That is `blocked` with a block declaration Loom authored, and it is
	// the same state, the same park, the same resume path and the same rendering
	// as a child-authored block. A separate state would fork §11.4's delivery
	// machinery in two on a distinction the child cannot observe.
	StateIntegrating TaskState = "integrating"
	// StateMergeable is §5.2's gate showing: green in isolation AND green
	// combined with its already-verified siblings. It is the only state from
	// which a merge into the user's own branch is offered, and reaching it is
	// entirely Loom's doing while leaving it is entirely the human's.
	StateMergeable TaskState = "mergeable"
	StateMerged    TaskState = "merged"
	StateAbandoned TaskState = "abandoned"
)

// numTaskStates is the size of the const block above, kept adjacent to it so
// adding a state and not updating this is a one-line diff away from the change
// that caused it. TestActiveChildrenAgreesWithHoldsAChild compares its own
// enumeration against this and fails when the two disagree.
//
// A hand-maintained count is the wrong shape and is here anyway: the states are
// string constants, so nothing at run time can enumerate them, and the
// alternatives are a generator or a slice-of-all that every switch would then be
// tempted to range over — which is exactly the indirection ActiveChildren and
// Terminal deliberately refuse (a safety switch must list its own cases).
const numTaskStates = 13

// Terminal reports whether no further transition is expected WITHOUT A HUMAN.
// That is the literal contract, and it is why `verified` and `failed` are in the
// set alongside the two obvious members:
//
//   - merged / abandoned: no outgoing edge exists at all;
//   - verified: with §10 built, its successor is `integrating`, which Loom
//     enters on its own — so the literal reading has weakened. It stays in the
//     set anyway, because the alternative is worse in the one place this
//     function is consumed: the deadlock predicate would declare DEADLOCK on
//     every run in the ordinary window between a green check and the run-wide
//     serialized integration slot opening (§10.2), and a false deadlock alarm
//     retrains the human to ignore the real one. Progress instead counts
//     verified as IN-FLIGHT explicitly — see graph.go;
//   - mergeable: §5.2's gate is showing and the only successor is `merged`,
//     which is the human pressing it. This is the case the literal contract was
//     written for: a run whose every task sits at the merge gate is waiting on a
//     human, which is the design working, not a run that has stopped;
//   - failed: the state machine has no outgoing edge either, and recovery is a
//     human amending the manifest or re-spawning.
//
// `integrating` is deliberately NOT in the set: it is Loom-driven, bounded by
// §12.2's watchdogs, and a run sitting in it forever is a fault worth naming.
//
// The deadlock predicate (§9.3) is the intended consumer. Ready
// deliberately does NOT call this: a scheduler is a safety property and it
// enumerates its own candidate states, so that a state added here cannot
// silently change which tasks Loom proposes to run — the same reason
// ActiveChildren enumerates rather than delegating to HoldsAChild, and the same
// reason Progress enumerates its four buckets.
func (s TaskState) Terminal() bool {
	switch s {
	case StateVerified, StateMergeable, StateFailed, StateMerged, StateAbandoned:
		return true
	}
	return false
}

// HoldsAChild reports whether a task in this state is holding a live `claude`
// and therefore a slot against §6.6's cap. Blocked counts: a blocked child holds
// its worktree AND its context, which is the entire reason Loom parks children
// rather than killing them. `checking` counts for the same reason — Loom runs
// the check, the child sits idle in its worktree, and it is still a process with
// a context. `spawning` counts because the session row is written after the
// process exists (§13.3), so a launch in flight may already be a real claude.
//
// `integrating` and `mergeable` count for the same reason and are the two §§9-12
// added. The child is alive in both: §10.2 runs against the INTEGRATION
// worktree, not the child's, so the child is idle at its prompt holding its
// context (and §10.3 sends the failure back to it, which only works because it
// is still there); and §10.4 step 3 does not remove the worktree until the merge
// actually happens, so a task parked at the §5.2 gate is still a claude and
// still a slot. Rejected alternative: counting only §6.6's literal "running and
// blocked", which would let a queue of mergeable tasks awaiting a human quietly
// admit unbounded new children — the human is the bottleneck the cap exists to
// protect (§6.6 reason 1), and lengthening their queue is the failure.
//
// This set is duplicated by ActiveChildren's switch ON PURPOSE — see its comment
// — so the cap cannot be quietly changed from here. The duplication is asserted
// by a test rather than left to good intentions.
func (s TaskState) HoldsAChild() bool {
	switch s {
	case StateSpawning, StateRunning, StateBlocked, StateChecking,
		StateIntegrating, StateMergeable:
		return true
	}
	return false
}

// Flag is one entry of `delegation_tasks.flags`, a JSON set kept deliberately
// OUT of the state column (§13.2). A state machine with a `stalled` state has to
// define the cross product of stalled × everything and stops being testable.
// Flags are set and cleared independently and NEVER gate a transition on their
// own.
type Flag string

const (
	// FlagOrphaned marks a task whose child session died. The session dying and
	// the work being worthless are unrelated events (§6.3): the worktree is kept
	// untouched and the run renders the task as recoverable.
	FlagOrphaned Flag = "orphaned"
	// FlagDiverged marks a task that touched files outside its declared paths
	// (§12.3.1). Non-blocking; it is what the merge gate demands a second
	// acknowledgement for.
	FlagDiverged Flag = "diverged"
	// FlagEnvSuspect marks a check failure whose output matched one of §6.4's
	// environment shapes (port in use, DB locked). It is a triage label on a
	// FAILURE and never turns one into a pass — see Result.EnvSuspect.
	FlagEnvSuspect Flag = "env-suspect"

	// The §§9-12 half of §13.2's list. None of these gates a transition on its
	// own; every one of them is rendered.

	// FlagStalled is §12.2's `no-progress` watchdog: `running`, branch head
	// unmoved AND transcript unadvanced for 20m. A LABEL — Loom never kills a
	// stalled child, because a stalled child may be mid-thought and an hour of
	// context is not Loom's to discard (§12.2, non-negotiable).
	FlagStalled Flag = "stalled"

	// FlagStaleContract is §10.5's alarm: an `interface` artifact this task
	// `needs` was re-fingerprinted after the task was spawned and the
	// fingerprint changed. It WITHDRAWS mergeability and re-parks the child via
	// §11's path. It is the only cross-repo break Loom can detect without a
	// cross-repo test, and it catches nothing else — it is not integration
	// testing and must never be rendered as if it were.
	FlagStaleContract Flag = "stale-contract"

	// FlagForced records that a human merged past an unacknowledged divergence
	// or a red gate. It is written for the record, never read as permission.
	FlagForced Flag = "forced"

	// FlagBlockMalformed is §11.2: a block.json that would not parse. LOUD, with
	// the raw content rendered, because a swallowed block is a child parked
	// forever with nobody told — the single worst outcome the rendezvous path
	// has.
	FlagBlockMalformed Flag = "block-malformed"

	// FlagOutsideWrites is §12.3.3's comparator firing: a file in some in-scope
	// repo's primary work tree, or in an add-dir'd integration worktree, changed
	// between spawn and now.
	//
	// It is NOT `diverged`. §13.2's list does not name this one, and reusing
	// `diverged` was the obvious move and is wrong: `diverged` means "the child
	// committed outside its declared paths IN ITS OWN WORKTREE", which is a
	// detector over a git diff and is per-§12.3 non-blocking-but-acknowledged,
	// whereas this means "something changed OUTSIDE the isolation boundary
	// altogether", which is the exact failure `--add-dir` cannot prevent
	// (read+write, no second trust prompt — see docs/spikes/
	// 2026-07-22-add-dir-spike.md) and which §12.3.3 requires be loud. One flag
	// for two findings with different mechanisms, different evidence and
	// different remedies would make the badge unreadable. Adding a flag outside
	// §13.2's enumeration is safe by construction: DecodeFlags round-trips
	// unknown flags precisely so the vocabulary can grow without a migration.
	//
	// Disclosed, and it must be in the rendering: the walk cannot distinguish
	// the human's own edits from a child's, so a repo the user is working in
	// produces FALSE POSITIVES. The text says "changed since spawn", never "the
	// child wrote this".
	FlagOutsideWrites Flag = "outside-writes"

	// FlagSeedPending is §11.4's undelivered-seed signal: a pending_seed is
	// durably owed to this task and has not been delivered — the child was not
	// at a state that consumes typed text before the gate timed out, or Loom
	// restarted between writing the column and sending.
	//
	// The name is the one the run already renders ("seed pending"), not
	// `seed-failed`: a timeout is explicitly NOT a failure and must not clear
	// the seed. The seed is OWED until it is delivered, §12.2's `block-stale`
	// offers the retry, and a flag whose name says "failed" invites a future
	// hand to add the clearing path that loses it.
	//
	// Like FlagOutsideWrites this is outside §13.2's enumeration; both are
	// recorded in the spec's flag list now. Growing the vocabulary needs no
	// migration because DecodeFlags round-trips unknown flags on purpose.
	//
	// This one is set and cleared through SetTaskFlagsCAS, not SetTaskFlags. It
	// is a durable claim about owed work rather than a badge a later poll
	// recomputes, so losing it to a concurrent watchdog's unconditional write
	// would strand a child at a prompt with nothing naming the debt.
	FlagSeedPending Flag = "seed-pending"

	// FlagConflict is §11.4's materialization conflict as a badge: step 2 tried to
	// merge a producer's branch into a parked child's worktree and git refused.
	//
	// The task STAYS blocked and the description of the conflict is written into
	// the durable pending_seed, which is what the child eventually reads — so the
	// failure was already visible without this. The flag exists because "visible
	// somewhere in the seed text" and "visible in the run list" are different
	// affordances: the seed is read once by a child, the badge is what tells a
	// human scanning twelve rows which one needs them. §11.4's own words are "the
	// task stays blocked with a conflict flag", and this is that flag.
	//
	// Set with FlagSeedPending, never instead of it: the seed is still owed. Like
	// the two above it was outside §13.2's ORIGINAL enumeration and is recorded in
	// the spec's flag list now; it needed no migration to add, because DecodeFlags
	// round-trips unknown flags on purpose.
	FlagConflict Flag = "conflict"

	// FlagFirstCheckGreen and FlagFirstCheckRed are §2's M2 made durable:
	// "fraction of tasks whose check was green THE FIRST TIME it ran after the
	// child stopped". Written once per task by Runner.recordFirstCheck and folded
	// into Measurements on read; see run.go for both.
	//
	// A flag rather than a column because DecodeFlags round-trips unknown flags
	// on purpose — that property is what lets the vocabulary grow without a
	// migration, and FlagOutsideWrites, FlagSeedPending and FlagConflict were all
	// added the same way. A flag rather than an in-memory counter on the Runner
	// because the measurement spans a whole initiative, which is days and several
	// Loom restarts; a number that resets when the process does is not a
	// measurement.
	//
	// TWO flags and not one, and this is the part that matters: with a single
	// `first-check-green` flag, "never checked" and "checked and red" would both
	// read as its absence, and M2's denominator would silently become the task
	// count — which reports a run that has barely started as failing §2's kill
	// criterion. The denominator is tasks carrying EITHER flag, and it is a fact
	// rather than an assumption.
	//
	// "After the child stopped" is ShouldRun's second condition (transcript Idle
	// or NeedsYou), reused verbatim: a check that ran mid-generation is testing a
	// tree the child is halfway through writing, and neither its pass nor its
	// failure says anything about the yield of an isolated child. A manual check
	// pressed while the child is mid-turn therefore contributes to neither flag.
	//
	// This is NOT product telemetry and nothing aggregates it across runs: §2 says
	// the measurements are "recorded per initiative, in the spec's follow-up, not
	// in the DB". These two flags are per-task state of ONE run that dies with the
	// run, and Measurements is computed on read.
	FlagFirstCheckGreen Flag = "first-check-green"
	FlagFirstCheckRed   Flag = "first-check-red"
)

// Flags is the decoded flag set. EncodeFlags must round-trip an unknown flag it
// was given rather than drop it, so a row written by a future Loom is not
// silently downgraded by this one — and so the vocabulary above can grow (see
// FlagOutsideWrites) without a migration.
type Flags map[Flag]bool

// DecodeFlags parses the stored JSON array. An unparseable value degrades to an
// empty set rather than an error: a corrupt flag column must cost a badge, never
// a run — the same "degrade, never block" rule session.DecodeAddDirs follows.
func DecodeFlags(s string) Flags {
	f := Flags{}
	if strings.TrimSpace(s) == "" {
		return f
	}
	var raw []string
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		// Deliberately not an error return and deliberately not a log line that
		// nobody reads: the caller is a renderer, and the worst outcome
		// available here is a task that will not render because its flag column
		// has a stray byte in it.
		return f
	}
	for _, v := range raw {
		if v == "" {
			continue
		}
		// UNKNOWN FLAGS ARE KEPT. A row written by a future Loom that knows
		// about `stale-contract` must survive a read/modify/write by this one;
		// filtering to the constants above is exactly how a two-instance setup
		// silently downgrades its own data.
		f[Flag(v)] = true
	}
	return f
}

// EncodeFlags renders the set as a sorted JSON array, and the EMPTY STRING —
// not "[]" — for the empty set, matching the migration default so an untouched
// row and a cleared row are byte-identical.
func EncodeFlags(f Flags) string {
	keys := make([]string, 0, len(f))
	for k, on := range f {
		// A false value is an absence, not a stored "off": Without deletes, and
		// a map literal in a caller's test may well spell a cleared flag this
		// way. Encoding `"orphaned": false` as a present flag would invert it.
		if on && k != "" {
			keys = append(keys, string(k))
		}
	}
	if len(keys) == 0 {
		// The EMPTY STRING, not "[]" — the migration's column default. An
		// untouched row and a cleared row must be byte-identical, or every
		// "has anything changed" comparison in the system gets a false
		// positive the first time a flag is set and cleared again.
		return ""
	}
	// Sorted so the encoding is a function of the SET and nothing else. Go's
	// map iteration order is randomized per run, so an unsorted encoding would
	// make two Looms rewrite the same column back and forth forever.
	sort.Strings(keys)
	b, err := json.Marshal(keys)
	if err != nil {
		return ""
	}
	return string(b)
}

// With returns a copy with flag set, and Without a copy with it cleared. Copies
// rather than mutation because a Flags value read off a task row is shared with
// whatever rendered it.
func (f Flags) With(flag Flag) Flags {
	out := f.clone()
	if flag != "" {
		out[flag] = true
	}
	return out
}

func (f Flags) Without(flag Flag) Flags {
	out := f.clone()
	// Deleted, not set to false: Flags is a SET, and EncodeFlags reading a
	// false entry as an absence is the only other consistent choice. Two
	// spellings of "cleared" is one too many.
	delete(out, flag)
	return out
}

// clone always returns a non-nil map, so With on a zero Flags (the value a
// caller gets from a struct it never populated) does not panic on assignment.
func (f Flags) clone() Flags {
	out := make(Flags, len(f)+1)
	for k, v := range f {
		out[k] = v
	}
	return out
}

// Sentinel errors. Every one of these is a condition a caller must be able to
// distinguish and RENDER — this package never degrades a refusal into a silent
// no-op, because an invisible failure in a system that spawns real agents is how
// a night's quota disappears with nothing on screen.
var (
	// ErrProjectNotFound and ErrProjectAmbiguous are §4.4 rule 2: the manifest's
	// `project` must resolve to EXACTLY one project root. Two errors and not one
	// because the remedies are opposite — one is a typo, the other is two
	// projects sharing a display name.
	ErrProjectNotFound  = errors.New("delegate: no such project")
	ErrProjectAmbiguous = errors.New("delegate: project name matches more than one project")

	// ErrEscapesRepo is §4.4 rule 7: an artifact path or a check cwd that
	// resolves outside its repo after filepath.Clean. Checked at load AND again
	// at execution against the materialized worktree, because a load-time pass
	// is a statement about a file that may since have been amended.
	ErrEscapesRepo = errors.New("delegate: path escapes its repo")

	// ErrWorktreeOccupied is §6.2 step 3's hard precondition: a live sessions
	// row already has cwd == physicalDir(<worktree>). Two claudes in one
	// worktree on one branch is the worst outcome available in this design, so
	// it is made structurally impossible at the launch site rather than argued
	// about in recovery.
	ErrWorktreeOccupied = errors.New("delegate: a live session already occupies this worktree")

	// ErrCapReached is §6.6. It is an error at the SPAWN site only; the
	// scheduler keeps proposing ready tasks and the gate keeps rendering them,
	// greyed with "cap reached (n/n)".
	ErrCapReached = errors.New("delegate: concurrency cap reached")

	// ErrTaskNotApproved is §13.3 step 1's refusal: the approved→spawning CAS
	// was rejected, so this task is not in `approved` any more. Either a spawn
	// already claimed it — the double-spawn case, and the reason the claim
	// precedes every side effect — or it was abandoned while the human was
	// looking at the gate.
	ErrTaskNotApproved = errors.New("delegate: task is not approved (already claimed, or abandoned)")

	// §§9-12's sentinels. Same rule: every one of these is a condition a caller
	// must be able to DISTINGUISH and RENDER.

	// ErrTaskMovedElsewhere is §13.3's CAS rejection, generalized past the spawn
	// site: some other actor — the other Loom instance, a watchdog, an abandon —
	// moved the row out from under this transition. Callers ABANDON THE STEP;
	// they never retry a CAS in a loop, because the row moving means the
	// premise the step was computed from is gone.
	ErrTaskMovedElsewhere = errors.New("delegate: task is no longer in the expected state")

	// ErrIntegrationBusy is §10.2's run-wide serialization refusing a second
	// concurrent integration. Not a failure: the caller re-offers the task on
	// the next tick. It exists as an error rather than a block because a cross
	// check reads several repos' integration worktrees at once and must not see
	// one mid-merge, and a blocking acquire inside a poll tick is how a stuck
	// merge takes the whole poll loop with it.
	ErrIntegrationBusy = errors.New("delegate: another integration is already running for this run")

	// ErrBaselineRed is §10.2's second attribution row: the integration check
	// was red at `pre`, WITHOUT the task merged. No task is blamed, the run goes
	// red, and spawning stops. It is a distinct error because the remedy is the
	// opposite of a task failure — nothing about the child is wrong.
	ErrBaselineRed = errors.New("delegate: the integration baseline is red; no task is to blame")

	// ErrDirtyTarget is §10.3's refusal: the user's own repo has uncommitted
	// changes and §5.2's merge would land on top of them. Merging into a dirty
	// tree is how a human loses work to a machine, so it is refused with the
	// offending files named — never stashed, never forced.
	ErrDirtyTarget = errors.New("delegate: the target repo's working tree is dirty")

	// ErrRepoBusy is ErrDirtyTarget's sibling and the hole between it and the
	// detached-HEAD refusal: the user's repo has an operation of the HUMAN'S OWN
	// in progress — a merge, a cherry-pick, a revert, a rebase — that
	// `git status --porcelain` need not report.
	//
	// A `git merge --no-commit` whose result happens to match HEAD leaves an
	// empty status and a live MERGE_HEAD, so the dirty guard passes, §10.4's
	// merge fails with "You have not concluded your merge", and the failure path
	// then ran `git merge --abort` UNCONDITIONALLY — destroying the human's
	// MERGE_HEAD and every conflict resolution staged into it, while reporting
	// that the TASK BRANCH conflicted. That is the only path found in this slice
	// that damages state Loom did not create, and it is refused here rather than
	// handled downstream: the abort is only ever safe for a merge Loom itself
	// started, and the only way to know it started one is to have proved none
	// was running first.
	ErrRepoBusy = errors.New("delegate: the target repo has an operation in progress")

	// ErrBranchMoved is §5.2's "the diff shown is the diff applied", enforced.
	// The task branch has commits past the sha the green §10.2 pass certified,
	// so merging it would land work that no check, no integration pass and no
	// preview ever saw. Refused rather than merged-anyway and refused rather
	// than silently merging the old sha: a partial merge of a branch is a state
	// no human asked for. Tick returns the task to `running` so the ordinary
	// check → verify → integrate cycle re-certifies the new head.
	ErrBranchMoved = errors.New("delegate: the task branch has moved past the commit the integration certified")

	// ErrAmendmentCycle is §11.3's last rule: every amendment re-runs cycle
	// detection over the AMENDED graph, and one that closes a loop is rejected
	// and escalated. This is the specific case where a loud block would
	// otherwise silently become a deadlock.
	ErrAmendmentCycle = errors.New("delegate: amendment would close a dependency cycle")

	// ErrNoSuchArtifact is §11.3's re-plan branch: a block names an artifact no
	// task produces. It is not an error in the child — naming an undeclared
	// artifact is the COMMON case for an unforeseen dependency — it is the
	// trigger for a re-plan request. Loom proposes the task/artifact to add and
	// never invents a task.
	ErrNoSuchArtifact = errors.New("delegate: no task produces this artifact")
)
