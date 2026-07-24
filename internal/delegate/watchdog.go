package delegate

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
)

// §12.1–12.2 — the deadlock detector and the watchdogs.
//
// NO WATCHDOG KILLS ANYTHING. Not on stall, not on timeout, not on budget. Loom
// is a window, never an owner; a stalled child may be mid-thought, and the human
// can attach and look. "Degrade the label, never the session" is the governing
// principle and it is explicitly not negotiable for a process holding an hour of
// irreplaceable context. Every action in this file is a flag, an adoption, or a
// refusal to offer NEW work.
//
// The deadlock half has the opposite temperament and the same root: a deadlock
// is never a quiet state, because the entire hazard is that it LOOKS LIKE
// PROGRESS. Every task has a plausible state, nothing is erroring, and the run
// will sit there forever. So the escalation is needs-you-grade — run row red and
// permanent, counted in the project's needs-you, notification through the
// existing path — subject to slice 1 §6.4's rule that a notification escalating
// out of a HIDDEN project degrades to a label-free body.

// DeadlockShape is §12.1's two shapes, distinguished because the REMEDIES
// differ. Collapsing them into "deadlocked" would leave the human with a red run
// and no next action, which is the failure mode of every deadlock detector that
// is ignored.
type DeadlockShape string

const (
	// ShapeMutualWait — a cycle in the EFFECTIVE graph (declared edges +
	// accepted amendments + live blocks). Always fatal to the run as planned and
	// requires a human re-plan. Rendered as the actual wait-for cycle, naming
	// every task and artifact in it — the same CycleError rendering the loader
	// uses, because a human who has seen one has seen both.
	//
	// §4.5 makes a DECLARED cycle impossible at load. This shape exists because
	// an amendment or a block can close one at runtime, when there are already
	// children holding context.
	ShapeMutualWait DeadlockShape = "mutual-wait"
	// ShapeExternal — all remaining work is blocked on something outside the
	// run: needs-decision, needs-scope, blocked-external. Rendered as an
	// ACTIONABLE LIST of the specific decisions owed, not as a status.
	ShapeExternal DeadlockShape = "external"
	// ShapeUnreachable — some remaining task `needs` an artifact that NOTHING
	// LEFT IN THE RUN CAN PRODUCE: its producer was abandoned or failed, or no
	// task produces it at all. Not in §12.1 either, and it is a fault in the
	// PLAN, which is why it may not fall through to ShapeStarved.
	//
	// It exists because abandoning one task reaches it immediately, and Abandon
	// is a first-class action (§13.2: `abandoned` from anywhere). The consumer
	// can never be ready — needsMet requires the producer verified or beyond —
	// nothing is in flight, and no block is live, so every term of ShapeStarved's
	// definition holds and the human was told to report a bug in Loom. The
	// actionable answer is a sentence: "`schema` was abandoned; `auth-api` needs
	// a re-plan", and it maps onto §11.3's re-plan branch exactly as an
	// unproduced needs-artifact block does.
	//
	// It is ranked ABOVE ShapeExternal for the same reason ShapeMutualWait is:
	// the remedy is a re-plan, and granting every owed decision still leaves the
	// consumer waiting on a task that is never going to run.
	ShapeUnreachable DeadlockShape = "unreachable"
	// ShapeStarved — non-terminal work exists, nothing is ready, nothing is in
	// flight, and NOTHING IS BLOCKED EITHER. Not in §12.1, and it is not a third
	// kind of deadlock: it is the shape that means Loom has a bug or a task row
	// is in a state no bucket claims (Progress.Unclassified). It is rendered as
	// a fault in Loom rather than a fault in the plan, because telling the human
	// to re-plan a manifest that is fine is worse than admitting the tool is
	// confused.
	ShapeStarved DeadlockShape = "starved"
)

// Deadlock is the detector's finding. Nil means the run is progressing.
type Deadlock struct {
	Shape DeadlockShape
	// Cycle is the wait-for path for ShapeMutualWait, in the same edge form
	// CycleError carries so the two render identically.
	Cycle []Edge
	// Owed is ShapeExternal's actionable list.
	Owed []OwedDecision
	// Unreachable is ShapeUnreachable's list, in the same Edge form the cycle
	// uses: From is the producer that will never deliver (empty when no task
	// produces the artifact at all), To is the task waiting on it.
	Unreachable []Edge
	// Stuck is every non-terminal task, for the other shapes.
	Stuck []string
	At    time.Time
}

// OwedDecision is one thing a human must do before the run can move.
type OwedDecision struct {
	TaskID  string
	Kind    BlockKind
	Summary string
	// Since is when the block was declared, so the list can be ordered oldest
	// first — the decision that has cost the most is the one to show at the top.
	Since time.Time
}

// DetectDeadlock classifies a stopped run. PURE, and it takes the Progress that
// already decided WHETHER the run stopped rather than recomputing it: §9.3 owns
// the predicate, §12.1 owns the diagnosis, and two implementations of "is this
// run stuck" that can disagree is the one outcome worth designing against.
//
// Returns nil when Progress.Deadlocked is false. It does NOT re-derive the
// condition — if a caller passes a Progress it did not compute from the same
// graph, the answer is theirs.
func DetectDeadlock(e EffectiveGraph, p Progress, states map[string]TaskState) *Deadlock {
	if !p.Deadlocked {
		return nil
	}
	d := &Deadlock{Shape: ShapeStarved, Stuck: nonTerminal(states)}

	// Order matters and it is the order of REMEDIES, not of likelihood. A cycle
	// is fatal to the run as planned however many blocks are also live: telling
	// the human "three decisions are owed" when granting all three still leaves
	// the loop closed sends them round it once by hand.
	// e.WaitCycle(), NOT DetectCycle(e.Merged(), ...). §12.1(a) defines the shape
	// over "declared edges + accepted amendments + LIVE BLOCKS", which is exactly
	// EffectiveGraph.WaitFor, and the distinction is the whole finding rather than
	// a nicety: §11.1's commonest unforeseen dependency names an artifact no
	// manifest declares, so two children each parked on an artifact the other was
	// going to write is a mutual wait containing ZERO declared edges and ZERO
	// accepted amendments. Over Merged() alone that run classifies as
	// ShapeExternal — "three decisions are owed" — and a human who grants all
	// three finds the loop still closed. WaitCycle also carries the run name, so
	// the rendering is byte-identical to the loader's §4.5 message.
	if cyc := e.WaitCycle(); cyc != nil {
		d.Shape = ShapeMutualWait
		d.Cycle = cyc.Path
		return d
	}

	// An unreachable producer, BEFORE the owed list and for ShapeMutualWait's
	// reason: the remedy is a re-plan, and no number of granted decisions makes
	// a task that was abandoned produce its artifact.
	if un := unreachableNeeds(e, states); len(un) > 0 {
		d.Shape = ShapeUnreachable
		d.Unreachable = un
		return d
	}

	// Every live block is owed, INCLUDING needs-artifact. That looks wrong
	// against §12.1(b)'s list, which names only the three outside-the-run kinds,
	// and it is right: the predicate that got us here says nothing is ready and
	// nothing is in flight, so there is no task left that could ever publish the
	// artifact this child is waiting for. A needs-artifact block in a run that
	// has stopped is a re-plan owed to a human exactly like the other three —
	// and §11.3's re-plan branch is the affordance it maps to. Filtering it out
	// would classify the commonest deadlock in the design as ShapeStarved, i.e.
	// as a bug in Loom.
	for _, id := range sortedKeys(e.Blocks) {
		b := e.Blocks[id]
		if b.Empty() {
			continue
		}
		d.Owed = append(d.Owed, OwedDecision{
			TaskID: id, Kind: b.Kind, Summary: b.Summary, Since: b.At,
		})
	}
	// Oldest first: the decision that has cost the most is the one to show at
	// the top. Ties break on task id so two Looms render the same list.
	sort.SliceStable(d.Owed, func(i, j int) bool {
		if !d.Owed[i].Since.Equal(d.Owed[j].Since) {
			return d.Owed[i].Since.Before(d.Owed[j].Since)
		}
		return d.Owed[i].TaskID < d.Owed[j].TaskID
	})
	if len(d.Owed) > 0 {
		d.Shape = ShapeExternal
	}
	return d
}

// unreachableNeeds is every edge whose producer can never deliver, over the
// EFFECTIVE graph, for tasks that are still expected to move.
//
// "Can never deliver" is deliberately narrow and reads only the state column:
//
//   - the producer is `abandoned` or `failed` — both terminal, neither publishes;
//   - no task in the run produces the artifact at all, which is the same hole
//     §11.3's re-plan branch records when a child names an artifact nobody
//     writes, reached here from the manifest rather than from a block.
//
// It deliberately does NOT consult the published set. A producer that reached a
// good terminal state without publishing is also a hole, but the honest evidence
// for it is the artifact table, DetectDeadlock is pure over (graph, progress,
// states) by design, and widening the signature to smuggle a fourth input in
// would put the deadlock verdict and §9.3's predicate on different pictures —
// the one outcome §12.1's comment says is worth designing against. The narrow
// set is the one that is certainly true.
//
// Order is manifest order then artifact, so two Looms name the same edge first.
func unreachableNeeds(e EffectiveGraph, states map[string]TaskState) []Edge {
	g := e.Merged()
	var out []Edge
	for _, id := range g.TaskIDs {
		if states[id].Terminal() {
			continue
		}
		needs := append([]string(nil), g.Needs[id]...)
		sort.Strings(needs)
		for _, art := range needs {
			producer, known := g.Producer[art]
			switch {
			case !known:
				out = append(out, Edge{To: id, Artifact: art})
			case states[producer] == StateAbandoned || states[producer] == StateFailed:
				out = append(out, Edge{From: producer, To: id, Artifact: art})
			}
		}
	}
	return out
}

// nonTerminal is every task still expected to move, sorted. Computed from the
// state map rather than from Progress's buckets because ShapeStarved's whole
// point is that the buckets did not classify something.
func nonTerminal(states map[string]TaskState) []string {
	var out []string
	for id, st := range states {
		if !st.Terminal() {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// WatchdogKind is §12.2's table, one constant per row.
type WatchdogKind string

const (
	// WatchNoProgress: `running`, branch head unmoved AND transcript unadvanced
	// for 20m. Both conditions, not either — a child that is thinking hard has
	// an advancing transcript and an unmoved branch, and a child that committed
	// and went idle has the reverse. Only the conjunction is a stall. Action:
	// flag `stalled`.
	WatchNoProgress WatchdogKind = "no-progress"
	// WatchCheckTimeout: §8.1's timeout, already enforced by check.go. Listed
	// here so the table is complete and so nobody adds a second timeout.
	WatchCheckTimeout WatchdogKind = "check-timeout"
	// WatchBlockStale: `blocked`, unblock condition satisfied >5m, seed not
	// delivered. Action: render `seed pending` and OFFER a retry. Not an
	// automatic retry — the durable column already survives restarts, and an
	// unattended retry loop against a live agent's prompt is how a seed gets
	// delivered five times.
	WatchBlockStale WatchdogKind = "block-stale"
	// WatchSpawnOrphan: `spawning` for >60s. Resolved by CWD, never by tag —
	// see ResolveSpawnOrphan.
	WatchSpawnOrphan WatchdogKind = "spawn-orphan"
	// WatchRunBudget: the run exceeded its max children or wall-clock. Action:
	// STOP OFFERING NEW SPAWNS. Nothing running is touched.
	WatchRunBudget WatchdogKind = "run-budget"
	// WatchChildGone is §6.3 rather than a row of §12.2's table: a task in
	// `running` or `blocked` whose child session has ended. It sits here because
	// it is the same shape as the rest — an observation over already-read facts
	// whose entire action is a flag — and because the alternative (a second pass
	// in the caller) left the one watchdog that can fire at ANY stage outside the
	// one function that is exhaustively table-tested. §12.2's `spawn-orphan` row
	// covers the `spawning` window alone; without this, a child that dies later
	// leaves the run rendering as healthy forever.
	//
	// It is the only kind that also CLEARS its flag (ActionUnflag): a badge that
	// survives a successful re-spawn sends the human looking for a dead child that
	// is sitting there working.
	WatchChildGone WatchdogKind = "child-gone"
	// WatchWorkStale is `integrating` or `checking` for longer than any check can
	// legitimately take, with no Loom in this process working the task.
	//
	// It is not in §12.2's table and it closes the same hole the table exists to
	// close. §10.2 runs merge → bootstrap → check → cross inside one call, and a
	// Loom that dies anywhere in that window leaves the row at `integrating` —
	// which Progress counts as InFlight, so the run is NOT deadlocked; which no
	// other watchdog row names, so nothing fires; and which
	// ClaimTaskIntegrationCAS's run-wide predicate counts, so EVERY OTHER TASK IN
	// THE RUN can never integrate again. The run then renders as healthy progress,
	// permanently, which is precisely the shape §12.1 exists to make impossible.
	// The un-green merge left in the integration branch is the second half: §10.2's
	// BINDING "only ever contains work green end to end" was maintained by
	// in-process error paths alone, and a process that is gone has no error path.
	//
	// The action is ActionRecover, and like everything else in this file it kills
	// nothing: the row is released back to the state the work is really in and the
	// integration worktree is reset to the last baseline that carried a verdict.
	// The task is then re-attempted from step 0 exactly as a first attempt.
	WatchWorkStale WatchdogKind = "work-stale"
)

// Watchdog thresholds, §12.2's table verbatim. Constants rather than
// configuration: they are the spec's numbers, and a knob here would let a run be
// configured out of the only supervision it has.
const (
	NoProgressAfter  = 20 * time.Minute
	BlockStaleAfter  = 5 * time.Minute
	SpawnOrphanAfter = 60 * time.Second
	// WorkStaleAfter has no row in §12.2 to copy, so it is derived rather than
	// invented: §4.3's check timeout defaults to 10 minutes and §10.2 can run a
	// bootstrap, a per-repo check and a cross check inside one pass, so the
	// longest legitimate occupancy is a small multiple of that. 20 minutes
	// matches NoProgressAfter, which is the number this design already uses for
	// "long enough that a human should look", and a task genuinely mid-pass in
	// THIS process is exempt by Observation.Busy rather than by the threshold —
	// so the number only has to outlast a plausible pass, not every conceivable
	// one.
	WorkStaleAfter = 20 * time.Minute
)

// Liveness is what the caller could establish about a task's child session, and
// it is THREE-valued on purpose. The obvious shape — `ChildAlive bool` — is
// wrong twice over: its zero value is "dead", so every Observation literal in
// every table would silently ask for an `orphaned` flag, and a `sessions` read
// that FAILED would be indistinguishable from a child that died. A DB blip must
// not put an orphan badge on twelve live children at once, so "I could not tell"
// is a value and it is the default.
type Liveness uint8

const (
	// LivenessUnknown: no session is bound to the task yet, or the read failed.
	// Evidence of nothing, and the zero value for exactly that reason.
	LivenessUnknown Liveness = iota
	LivenessAlive
	LivenessDead
)

// Observation is one task's inputs to the pure watchdog pass. A struct of
// already-read facts rather than a store handle, so every row of §12.2's table
// is testable from a literal with no DB, no git and no clock.
type Observation struct {
	TaskID string
	State  TaskState
	// Since is when the task entered its current state (delegation_tasks.
	// updated_at). The `spawning >60s` and `blocked >5m` rows measure from here.
	Since time.Time

	// BranchHead is the child branch's current head; LastBranchHead is the last
	// one recorded. Equal means the child has committed nothing since.
	BranchHead, LastBranchHead string
	// TranscriptAt is the child transcript's last advance, and TranscriptState
	// its engine-derived state — matched by SHAPE, never by string, which is why
	// this is transcript.State and not a scraped pane.
	TranscriptAt    time.Time
	TranscriptState transcript.State

	// PendingSeed is non-empty when a seed is owed. UnblockedAt is when the
	// condition became satisfiable; the pair is the `block-stale` row.
	PendingSeed string
	UnblockedAt time.Time

	// Child is §6.3's fact: is the bound `sessions` row still live? Read by the
	// caller, which is the only party holding the store — the pass stays pure.
	Child Liveness

	// Flags is the current set, so a pass does not re-report a flag that is
	// already on. A watchdog that re-fires every tick is one whose notifications
	// get muted, which costs the real one.
	Flags Flags

	// Busy is "THIS process is actively working this task right now" — it holds
	// the in-process integration lock for the run. Only the `work-stale` row
	// reads it, and only to stay silent while a legitimate long pass is running.
	//
	// Its zero value is the LOUD direction, deliberately and unlike Liveness's:
	// "I do not know whether anyone is working this" must mean "report it", because
	// the whole finding is about a row nobody is working, and a false negative here
	// is a run wedged forever with nothing said. A spurious finding costs a reset
	// to a commit that already carried a verdict.
	Busy bool
}

// Budget is §12.2's `run-budget` row. Zero fields mean unlimited, which is the
// honest default: Loom does not know what a run should cost, and inventing a cap
// would make it a number the human trusted for no reason.
type Budget struct {
	MaxChildren int
	MaxWall     time.Duration
	StartedAt   time.Time
	// Spawned counts children EVER launched by this run, not currently live.
	// The budget is about spend, and a child that ran for an hour and finished
	// spent its quota.
	Spawned int
}

// WatchdogAction is what a finding asks the caller to do, enumerated so the
// caller cannot invent a harsher one. There is no `kill`.
type WatchdogAction string

const (
	ActionFlag       WatchdogAction = "flag"        // set Finding.Flag; render
	ActionUnflag     WatchdogAction = "unflag"      // clear Finding.Flag; render nothing
	ActionOfferRetry WatchdogAction = "offer-retry" // render an affordance; do NOT retry
	ActionResolve    WatchdogAction = "resolve"     // run ResolveSpawnOrphan
	ActionStopSpawns WatchdogAction = "stop-spawns" // suppress new approvals only
	// ActionRecover releases a row abandoned mid-work by a dead process and
	// resets the integration worktree to a commit that carries a verdict. It
	// touches no session and no branch of the child's: recovery here is a
	// statement about Loom's own bookkeeping, never about the work.
	ActionRecover WatchdogAction = "recover"
)

// Finding is one watchdog observation, ready to render.
type Finding struct {
	TaskID string
	Kind   WatchdogKind
	Action WatchdogAction
	// Flag is the flag ActionFlag asks for, empty otherwise.
	Flag Flag
	// Detail is the human sentence, carrying the numbers ("no commit and no
	// transcript advance for 34m"). A watchdog that says only "stalled" makes
	// the human go and find out what it meant.
	Detail string
	At     time.Time
}

// Watch is the pure pass over §12.2's table. It never writes; run.go applies the
// findings. Two reasons: the whole table becomes table-driven-testable, and the
// "no watchdog kills anything" rule becomes structurally true rather than
// carefully maintained — this function has nothing to kill with.
func Watch(now time.Time, obs []Observation, b Budget) []Finding {
	var out []Finding

	// §12.2's `run-budget` row. It is run-scoped, so it carries no TaskID, and
	// its action is STOP OFFERING NEW SPAWNS — nothing running is touched. Both
	// limbs can fire at once and both are reported: "over on children" and "over
	// on wall clock" are different sentences to a human deciding whether to
	// widen the budget or stop the run.
	if b.MaxChildren > 0 && b.Spawned >= b.MaxChildren {
		out = append(out, Finding{
			Kind: WatchRunBudget, Action: ActionStopSpawns, At: now,
			Detail: fmt.Sprintf("run has spawned %d of a maximum %d children; no new spawns are offered (nothing running is touched)",
				b.Spawned, b.MaxChildren),
		})
	}
	if b.MaxWall > 0 && !b.StartedAt.IsZero() && now.Sub(b.StartedAt) >= b.MaxWall {
		out = append(out, Finding{
			Kind: WatchRunBudget, Action: ActionStopSpawns, At: now,
			Detail: fmt.Sprintf("run has been going %s, past its %s budget; no new spawns are offered (nothing running is touched)",
				roundish(now.Sub(b.StartedAt)), roundish(b.MaxWall)),
		})
	}

	for _, o := range obs {
		// §6.3, before the switch and not inside it, because it is the one row
		// that fires at more than one state AND must not be swallowed by the stall
		// row's early `continue`s: a child can be both stalled and dead, and the
		// two facts have different remedies.
		//
		// The two literal states are the spec's ("a flag on `running`/`blocked`").
		// `checking` is deliberately excluded — Loom is running that task's check
		// itself, the verdict is minutes away, and flagging mid-check puts an
		// `orphaned` badge on a task about to be `verified`. Nothing later
		// qualifies either: the work is committed and a dead child is no longer
		// relevant to it.
		if o.State == StateRunning || o.State == StateBlocked {
			switch {
			case o.Child == LivenessDead && !o.Flags[FlagOrphaned]:
				out = append(out, Finding{
					TaskID: o.TaskID, Kind: WatchChildGone, Action: ActionFlag, Flag: FlagOrphaned, At: now,
					Detail: "the child session has ended; the worktree and branch are untouched and NOTHING was killed — recovery is a re-spawn onto the same worktree",
				})
			case o.Child == LivenessAlive && o.Flags[FlagOrphaned]:
				out = append(out, Finding{
					TaskID: o.TaskID, Kind: WatchChildGone, Action: ActionUnflag, Flag: FlagOrphaned, At: now,
					Detail: "a live child is bound to this task again; the orphan badge is cleared",
				})
			}
		}

		switch o.State {
		case StateRunning:
			// BOTH conditions, never either. A child thinking hard has an
			// advancing transcript and an unmoved branch; a child that committed
			// and went idle has the reverse. Only the conjunction is a stall.
			if o.BranchHead != o.LastBranchHead {
				continue
			}
			// A zero TranscriptAt means no transcript has been observed at all,
			// which is not evidence of advance — it is the strongest case there
			// is. Measure from the state entry instead, so a child that produced
			// nothing whatsoever for 20m is still caught. Rejected: skipping the
			// task, which makes the loudest silence the one nobody watches.
			last := o.TranscriptAt
			if last.IsZero() {
				last = o.Since
			}
			if last.IsZero() || now.Sub(last) < NoProgressAfter {
				continue
			}
			// Already flagged: say nothing. A watchdog that re-fires every tick
			// is one whose notifications get muted, which costs the real one.
			// This dedup is ONLY for ActionFlag — see below.
			if o.Flags[FlagStalled] {
				continue
			}
			out = append(out, Finding{
				TaskID: o.TaskID, Kind: WatchNoProgress, Action: ActionFlag, Flag: FlagStalled, At: now,
				Detail: fmt.Sprintf("no commit and no transcript advance for %s (transcript last read %s); the child is NOT killed — attach and look",
					roundish(now.Sub(last)), o.TranscriptState),
			})

		case StateBlocked:
			// The `block-stale` row: the unblock condition has been satisfied for
			// more than 5m and the seed the task is owed is still undelivered.
			if o.PendingSeed == "" || o.UnblockedAt.IsZero() || now.Sub(o.UnblockedAt) <= BlockStaleAfter {
				continue
			}
			// NOT deduped on FlagSeedPending, unlike the stall above, and that is
			// deliberate: this finding IS the retry affordance, so suppressing it
			// once the flag is on would take the button away and leave the flag
			// pointing at nothing. It is also why the action is offer-retry and
			// never retry: the seed column is durable and survives restarts, and
			// an unattended retry loop against a live agent's prompt is how a
			// seed gets delivered five times.
			out = append(out, Finding{
				TaskID: o.TaskID, Kind: WatchBlockStale, Action: ActionOfferRetry, At: now,
				Detail: fmt.Sprintf("seed pending: unblocked %s ago and the seed has not been delivered; retry is offered, never automatic",
					roundish(now.Sub(o.UnblockedAt))),
			})

		case StateIntegrating, StateChecking:
			// A pass this process is actually running is not stale, however long
			// it takes. Nothing here is time-boxed on Loom's behalf: check.go owns
			// §8.1's timeout and a second one is how two components come to
			// disagree about whether a check is still running.
			if o.Busy || o.Since.IsZero() || now.Sub(o.Since) <= WorkStaleAfter {
				continue
			}
			out = append(out, Finding{
				TaskID: o.TaskID, Kind: WatchWorkStale, Action: ActionRecover, At: now,
				Detail: fmt.Sprintf("stuck in `%s` for %s with nothing working it — most likely a Loom that died mid-pass; "+
					"the row is released and the integration worktree is reset to its last recorded baseline. "+
					"Nothing of the child's is touched", o.State, roundish(now.Sub(o.Since))),
			})

		case StateSpawning:
			if o.Since.IsZero() || now.Sub(o.Since) <= SpawnOrphanAfter {
				continue
			}
			// Deduped on FlagOrphaned, which is what ResolveSpawnOrphan writes
			// when it finds committed work and no live session: that outcome
			// leaves the row in `spawning` on purpose (the work is preserved and
			// recovery is §6.3's human-driven re-spawn), so without this the
			// resolver would be re-run on every tick forever against a task it
			// has already adjudicated.
			if o.Flags[FlagOrphaned] {
				continue
			}
			out = append(out, Finding{
				TaskID: o.TaskID, Kind: WatchSpawnOrphan, Action: ActionResolve, At: now,
				Detail: fmt.Sprintf("still spawning after %s; resolving by worktree cwd (never by tag)", roundish(now.Sub(o.Since))),
			})
		}
	}

	// Sorted so a poll tick renders the same list twice. Run-scoped findings
	// carry no task id and therefore sort first, which is also where they belong
	// on screen: a budget stop explains why the ready tasks below it are greyed.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TaskID != out[j].TaskID {
			return out[i].TaskID < out[j].TaskID
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// roundish renders a duration the way the run view says it: whole minutes past a
// minute, whole seconds below. `34m` and `1h12m`, never `34m17.4351s` — a
// watchdog sentence is read by a human deciding whether to attach.
func roundish(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

// Watchdogs is the half that needs the store: §13.3's spawn-orphan resolution,
// which is a query and a CAS.
type Watchdogs struct {
	Store     *store.Store
	Worktrees *Worktrees
	Layout    Layout
	Now       func() time.Time
}

// OrphanOutcome is ResolveSpawnOrphan's verdict.
type OrphanOutcome string

const (
	// OutcomeAdopted — a live session row exists at the deterministic worktree
	// cwd. Complete the CAS to `running`, write session_name, re-apply the tag
	// if missing.
	OutcomeAdopted OrphanOutcome = "adopted"
	// OutcomeReapproved — no live row and the branch has NO commits. Safe to CAS
	// back to `approved` and let the human decide again.
	OutcomeReapproved OrphanOutcome = "re-approved"
	// OutcomeOrphaned — no live row and the branch HAS commits. Flag `orphaned`;
	// the work is preserved, untouched, and the run renders the task as
	// recoverable (§6.3). The worktree is not removed and the branch is not
	// deleted — a worktree whose child died is not garbage.
	OutcomeOrphaned OrphanOutcome = "orphaned"
)

// ResolveSpawnOrphan resolves a task stuck in `spawning`.
//
// RECOVERY KEYS ON `sessions.cwd`, NEVER ON A TAG (§13.3, BINDING correction).
// Launcher.Launch upserts the session row with Cwd = physicalDir(<worktree>) in
// the same call that creates the tmux session; that is the ONE identity that
// exists the instant the child does. Tags are a DB column written afterwards —
// tmux carries only `loom-<uuid>`, because ARCHITECTURE §4.1 forbids embedding a
// label in the tmux name (`.` and `:` break `tmux -t` targeting) — so there is
// nothing in tmux to interrogate.
//
// Revision 1's rule ("no tag + branch has no commits ⇒ re-approve") was actively
// DANGEROUS: a crash between Launch and SetTags leaves a real, running child
// with no tag and — because it has only just started — no commits, so the
// watchdog would have re-approved it and put a SECOND `claude` in the same
// worktree on the same branch. That is strictly worse than workflow's accepted
// "stranded but idle session". §6.2 step 3's precondition is the second line of
// defence: even a misjudging recovery cannot double-spawn, because Create
// refuses to launch into an occupied worktree.
//
// The cwd compared is the PHYSICALLY RESOLVED one (session.PhysicalDir, via
// worktree.go's physicalDir), because that is the string Launch stored and a
// symlinked ~/.loom would otherwise make every comparison miss.
//
// RESIDUAL HOLE, DISCLOSED: §13.3's step-4 CAS can be rejected by a concurrent
// abandon, leaving a live child whose row is no longer in `spawning` — which a
// watchdog scanning `spawning` never sees. Abandon therefore also sweeps by cwd
// over the run's deterministic worktree paths, which closes it for the abandon
// case specifically. A crash between a successful Launch and any subsequent row
// write in a task abandoned elsewhere remains a stranded-but-idle session,
// dashboard-visible via its `dlg:` tag. The hole is narrowed. It is not closed,
// and nothing here should claim it is.
func (w *Watchdogs) ResolveSpawnOrphan(run store.DelegationRun, taskID, repoLabel string) (OrphanOutcome, error) {
	if w == nil || w.Store == nil || w.Worktrees == nil {
		return "", errors.New("delegate: ResolveSpawnOrphan needs a Store and a Worktrees")
	}
	now := w.now()

	task, ok, err := w.Store.GetDelegationTask(run.ID, taskID)
	if err != nil {
		return "", fmt.Errorf("delegate: spawn-orphan %s/%s: %w", run.Slug, taskID, err)
	}
	if !ok {
		return "", fmt.Errorf("delegate: spawn-orphan %s/%s: %w", run.Slug, taskID, ErrTaskMovedElsewhere)
	}

	// The row's own worktree is preferred over the recomputed one because
	// ClaimTaskSpawnCAS wrote it in the SAME statement that claimed the task, so
	// it is the path this spawn actually used. Layout is the fallback for a row
	// written before that, and the two agree by construction.
	dir := task.Worktree
	if strings.TrimSpace(dir) == "" {
		dir = w.Layout.Dir(run.Slug, repoLabel, taskID)
	}

	sess, found, err := w.Worktrees.Occupant(dir)
	if err != nil {
		return "", fmt.Errorf("delegate: spawn-orphan %s/%s: %w", run.Slug, taskID, err)
	}
	if found {
		// ADOPT. The child is real and is running; the only thing missing is the
		// row write that the crash interrupted.
		claimed, err := w.Store.BindTaskSessionCAS(run.ID, taskID, sess.Name, now.UnixMilli())
		if err != nil {
			return "", fmt.Errorf("delegate: spawn-orphan %s/%s: adopt: %w", run.Slug, taskID, err)
		}
		if !claimed {
			// Someone else moved the row — the other Loom instance adopting it
			// first, or an abandon. Never retried: the premise is gone.
			return "", ErrTaskMovedElsewhere
		}
		// The tag is cosmetic (dashboard filtering only, spawn.go) and is
		// re-applied on a best-effort basis: failing an adoption because a
		// human-facing label did not stick would strand a live child for the
		// sake of a badge.
		if !strings.Contains(sess.Tags, DelegationTag(run.Slug, taskID)) {
			_ = w.Store.SetTags(sess.Name, DelegationTag(run.Slug, taskID))
		}
		return OutcomeAdopted, nil
	}

	if branchHasCommits(dir, task.BaseSHA, task.Branch) {
		// ORPHANED. Work exists and nothing is alive to own it. The state stays
		// `spawning` — deliberately. §13.3 authorizes re-approve ONLY for the
		// no-commits case, and §6.3's recovery is a human-driven re-spawn onto
		// this same worktree, so moving the row here would either license a
		// second claude over committed work or invent a transition the spec does
		// not have. The flag is what makes it visible and what stops the
		// watchdog re-running (see Watch's dedup).
		if err := w.flag(run.ID, task, FlagOrphaned); err != nil {
			return "", err
		}
		return OutcomeOrphaned, nil
	}

	// No live session and no commits: nothing was accomplished and nothing is at
	// risk, so hand the decision back to the human.
	claimed, err := w.Store.AdvanceTaskCAS(run.ID, taskID,
		string(StateSpawning), string(StateApproved), now.UnixMilli())
	if err != nil {
		return "", fmt.Errorf("delegate: spawn-orphan %s/%s: re-approve: %w", run.Slug, taskID, err)
	}
	if !claimed {
		return "", ErrTaskMovedElsewhere
	}
	return OutcomeReapproved, nil
}

// flag sets one flag through the CAS form, preserving every flag already on the
// row (including ones this build does not know — DecodeFlags round-trips them on
// purpose). A rejected CAS is NOT an error here: it means another writer changed
// the flag set concurrently, and the next tick recomputes from fresh state. A
// badge is not worth failing an orphan adjudication over.
func (w *Watchdogs) flag(runID int64, task store.DelegationTask, f Flag) error {
	next := DecodeFlags(task.Flags).With(f)
	if _, err := w.Store.SetTaskFlagsCAS(runID, task.TaskID, task.Flags, EncodeFlags(next), w.now().UnixMilli()); err != nil {
		return fmt.Errorf("delegate: flag %s on %s: %w", f, task.TaskID, err)
	}
	return nil
}

// branchHasCommits answers "is there work in this worktree that a re-approve
// would put a second claude on top of".
//
// It errs toward "yes there is" ONLY when it has evidence. A missing worktree, a
// branch that was never created, an unreadable base — all of those mean the
// spawn died before it produced anything, which is exactly the re-approve case,
// and reporting them as "has commits" would park a task at `orphaned` forever
// with an empty worktree the human then has to disprove by hand.
func branchHasCommits(dir, base, branch string) bool {
	if dir == "" || branch == "" {
		return false
	}
	if base == "" {
		// No pinned base to count from; ask whether the branch resolves at all
		// and treat a resolvable branch with a commit as work.
		_, err := gitOut(dir, "rev-parse", "--verify", branch+"^{commit}")
		return err == nil
	}
	out, err := gitOut(dir, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	return err == nil && n > 0
}

func (w *Watchdogs) now() time.Time {
	if w != nil && w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// SweepAbandon is the cwd sweep the disclosure above requires: on abandon, look
// for live sessions at any of the run's deterministic worktree paths and
// reconcile the rows, so a child stranded by a lost CAS is still findable by the
// one identity that exists.
func (w *Watchdogs) SweepAbandon(run store.DelegationRun, m Manifest) ([]store.SessionRow, error) {
	if w == nil || w.Store == nil || w.Worktrees == nil {
		return nil, errors.New("delegate: SweepAbandon needs a Store and a Worktrees")
	}
	var (
		found []store.SessionRow
		errs  []string
	)
	for _, t := range m.Tasks {
		dir := w.Layout.Dir(run.Slug, t.Repo, t.ID)
		sess, ok, err := w.Worktrees.Occupant(dir)
		if err != nil {
			// One unreadable path does not abort the sweep: the whole value of
			// the sweep is the sessions it DOES find, and aborting on the first
			// failure is how the one stranded child stays unfound.
			errs = append(errs, fmt.Sprintf("%s: %v", t.ID, err))
			continue
		}
		if !ok {
			continue
		}
		// RECONCILE, NOT KILL — and this is the file where that has to be said
		// twice. The session is live and holds context; abandoning the TASK is a
		// statement about the plan, not about the process. All the sweep does is
		// re-apply the `dlg:` tag so the child is findable on the dashboard by
		// the one identity that survived, which is the disclosed residual of
		// §13.3.
		if !strings.Contains(sess.Tags, DelegationTag(run.Slug, t.ID)) {
			if err := w.Store.SetTags(sess.Name, DelegationTag(run.Slug, t.ID)); err != nil {
				errs = append(errs, fmt.Sprintf("%s: tag: %v", t.ID, err))
			}
		}
		found = append(found, sess)
	}
	if len(errs) > 0 {
		// The rows AND the error: a partial sweep that reports only the failure
		// hides the children it did find, which are the actionable half.
		return found, fmt.Errorf("delegate: abandon sweep for run %s: %s", run.Slug, strings.Join(errs, "; "))
	}
	return found, nil
}
