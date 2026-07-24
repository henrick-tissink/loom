package delegate

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

// §9 — scheduling over the EFFECTIVE graph.
//
// manifest.go owns the DECLARED graph: BuildGraph derives producer→consumer
// edges from artifact ids, DetectCycle walks them, and Ready answers "which
// tasks have no unmet edges" over that. That was the whole of 3a's scheduler.
//
// This file owns what the declared graph is not: the graph Loom is ACTUALLY
// scheduling against, which is the declared edges plus the amendments a human
// accepted (§11.3) plus the blocks children are currently sitting in (§11.1). A
// task with a live `needs-decision` block is not ready no matter what its edges
// say, and an accepted `needs-artifact` amendment is a real edge that appears in
// no manifest on disk — the on-disk file is never mutated, because Loom does not
// own it and the author may be editing it (§11.3).
//
// Everything here is PURE. No DB, no git, no side effects, no spawning: §9.1's
// "it proposes; §5.1 disposes" is a property of the code, not a convention, and
// the deadlock predicate is a safety property that must be testable from a
// literal. The store reads that feed it live in run.go.

// AmendmentKind is `delegation_amendments.kind`. A string for the same reason
// TaskState is: a schema that survives `sqlite3 loom.db` is worth more than the
// bytes.
type AmendmentKind string

const (
	// AmendEdge is §11.3's first case: a `needs-artifact` block naming an
	// artifact some task in the run already produces. The amendment is a new
	// dependency edge, consumer ← producer, and once accepted it gates Ready
	// exactly as a declared edge does.
	AmendEdge AmendmentKind = "edge"
	// AmendReplan is §11.3's second case: the block names an artifact NOBODY
	// produces. Loom cannot fix this — it does not invent tasks (§16) — so the
	// amendment is a RECORD OF A REQUEST addressed to the human, carrying the
	// exact task/artifact pair Loom would suggest adding. It contributes no
	// edge; an unproduced need is not a dependency, it is a hole in the plan.
	AmendReplan AmendmentKind = "re-plan"
	// AmendScope is §11.3's third case, from a `needs-scope` block: a proposed
	// widening of a task's authorized paths. NEVER auto-granted. When accepted
	// it rewrites brief.md in place and re-seeds the child (§11.4), and it
	// changes the divergence detector's declared set (§12.3.1) from that point
	// on — which is why it is durable and not a note.
	AmendScope AmendmentKind = "scope"
)

// Amendment is one append-only row on the run (§13.1's delegation_amendments).
// Append-only is the point: the manifest snapshot in delegation_runs stays the
// thing the run was started from, and the amendment log is the auditable
// difference between the plan and what actually happened. M3 in §2 is literally
// a count of these.
//
// Seq orders them within a run and is the CAS key for approval — two Loom
// instances approving the same amendment must not produce two edges.
type Amendment struct {
	RunID int64
	Seq   int64
	Kind  AmendmentKind

	// Task is the task the amendment is ABOUT: the consumer for AmendEdge and
	// AmendReplan, the task whose authorization widens for AmendScope.
	Task string
	// Artifact is the artifact named by the block. For AmendEdge it exists and
	// From names its producer; for AmendReplan it does not exist anywhere and
	// From is Loom's SUGGESTION of which task should produce it — a suggestion,
	// rendered as one, never applied.
	Artifact string
	From     string
	// Paths is AmendScope's proposed additional authorization, in the same glob
	// vocabulary as Task.Paths, so the divergence detector needs no second
	// matcher.
	Paths []string
	// Reason is the block's summary, carried so the amendment reads on its own
	// after the worktree is gone.
	Reason string

	CreatedAt time.Time
	// ApprovedAt is zero until a human accepts. An amendment is INERT until
	// then — Effective ignores it — because §11.3's whole shape is that Loom
	// proposes the amendment and the human grants it.
	ApprovedAt time.Time
	// RejectedAt is the human's NO, and it is a SECOND timestamp rather than a
	// negative ApprovedAt because the two answers are not each other's absence.
	// Without it, "refused" and "nobody has looked yet" are the same value, the
	// offer is re-rendered on every poll forever, and the one gate in §11.3 that
	// exists to be refusable has no way to record a refusal.
	//
	// It changes nothing about the effective graph — Accept already ignores
	// everything unapproved, and the store CAS makes the two exclusive — so this
	// is a fact about the OFFER, not about the plan. That is why Accepted() is
	// unchanged: a rejected amendment was never accepted, and adding
	// `&& !Rejected()` there would imply a state in which it had been.
	RejectedAt time.Time
}

// Accepted reports whether this amendment is part of the effective graph.
func (a Amendment) Accepted() bool { return !a.ApprovedAt.IsZero() }

// Rejected reports the human's durable NO. Decided, and therefore no longer on
// offer — which is a different question from Accepted, and both being false is
// the only state that means "still awaiting a human".
func (a Amendment) Rejected() bool { return !a.RejectedAt.IsZero() }

// Pending reports that the amendment is still awaiting a decision. The renderer
// asks this rather than `!Accepted()`, which was true for a refused amendment
// too and is what kept a rejected offer on the screen.
func (a Amendment) Pending() bool { return !a.Accepted() && !a.Rejected() }

// Edge returns the dependency edge this amendment contributes, and ok=false when
// it contributes none — every kind but an accepted AmendEdge.
func (a Amendment) Edge() (Edge, bool) {
	if !a.Accepted() {
		return Edge{}, false
	}
	return a.candidateEdge()
}

// candidateEdge is Edge without the approval test: the edge the amendment WOULD
// contribute. AmendmentCycle needs exactly this — it is asked about an amendment
// that is not approved yet, and that is the whole point of asking.
//
// A self-edge (From == Task) is returned rather than dropped. It is a cycle of
// length one, and dropping it here would convert the loudest possible amendment
// defect into an edge that silently does nothing; DetectCycle names it instead.
func (a Amendment) candidateEdge() (Edge, bool) {
	if a.Kind != AmendEdge || a.Task == "" || a.From == "" || a.Artifact == "" {
		return Edge{}, false
	}
	return Edge{From: a.From, To: a.Task, Artifact: a.Artifact}, true
}

// amendmentBody is the `body` column's payload. RunID/Seq/Kind/ApprovedAt/
// CreatedAt are COLUMNS and deliberately not repeated here: a body that carries
// its own copy of the approval state is a second source of truth for the one
// fact the CAS protects.
type amendmentBody struct {
	Task     string   `json:"task,omitempty"`
	Artifact string   `json:"artifact,omitempty"`
	From     string   `json:"from,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// EncodeAmendmentBody / DecodeAmendmentBody are the `body` column's codec. A
// JSON blob rather than columns because the three kinds carry disjoint payloads
// and four mostly-NULL columns is how a schema stops being readable.
func EncodeAmendmentBody(a Amendment) string {
	b, err := json.Marshal(amendmentBody{
		Task: a.Task, Artifact: a.Artifact, From: a.From,
		Paths: a.Paths, Reason: a.Reason,
	})
	if err != nil {
		// Unreachable for this struct — strings and a []string of strings — and
		// handled anyway with the empty body rather than a panic, because the
		// caller is a writer inside a transaction and the row is worth more than
		// the payload: an amendment that renders as malformed is recoverable, a
		// Loom that died mid-approval is not.
		return ""
	}
	return string(b)
}

// DecodeAmendmentBody degrades rather than failing: a body that will not parse
// yields the zero payload with the kind and seq intact, so the amendment still
// RENDERS (as malformed) instead of vanishing from the log. An invisible
// amendment is an edge the human cannot see and cannot revoke.
func DecodeAmendmentBody(kind AmendmentKind, body string) (Amendment, bool) {
	var p amendmentBody
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		return Amendment{Kind: kind}, false
	}
	return Amendment{
		Kind: kind, Task: p.Task, Artifact: p.Artifact, From: p.From,
		Paths: p.Paths, Reason: p.Reason,
	}, true
}

// EffectiveGraph is §12.1(a)'s "declared edges + accepted amendments + live
// blocks" — the only graph the scheduler and the deadlock detector may look at.
//
// It embeds the declared Graph rather than copying it so that a caller holding
// an EffectiveGraph cannot accidentally read the declared edges when it meant
// the effective ones: Declared() is explicit, and the embedded fields are what
// the two agree on (TaskIDs, Producer). Edges is deliberately NOT promoted —
// use Merged().
type EffectiveGraph struct {
	// declared is the manifest's own graph, from BuildGraph.
	declared Graph
	// name is the manifest name, carried only so a cycle found at runtime
	// renders through CycleError exactly as the loader's does (§12.1(a): "the
	// same CycleError rendering the loader uses"). Unexported because it is a
	// label, not a fact anything may branch on.
	name string
	// Added are the edges accepted amendments contributed, sorted the same way
	// BuildGraph sorts (To, Artifact) so the merged edge list is a function of
	// the SET and not of insertion order. Two Looms rendering the same run must
	// draw the same graph.
	Added []Edge
	// Blocks is the live block declaration per task, if any. A task with a live
	// block is not ready REGARDLESS of its edges — that is the whole content of
	// "live blocks" in §12.1's definition, and it is why a run can be deadlocked
	// with a perfectly acyclic manifest.
	Blocks map[string]Block
	// Amendments is the full log, accepted or not, for rendering and for
	// re-deriving Added.
	Amendments []Amendment
}

// Effective builds the graph the scheduler runs on. Pure and total: an amendment
// naming a task that is not in the manifest, or an artifact with no producer,
// contributes nothing rather than panicking — the snapshot a run replays may be
// older than an amendment written against a since-edited file, and BuildGraph's
// totality argument applies here verbatim.
//
// It does NOT run cycle detection. Detecting a cycle is AcceptAmendment's job
// (§11.3, at the moment of acceptance, where there is a human to escalate to)
// and DetectDeadlock's job (§12.1, where it is a finding). Doing it here would
// mean the scheduler silently returned an empty ready set for a run whose real
// problem is a cycle, which is precisely §4.5's "silent deadlock that looks like
// healthy progress".
func Effective(m Manifest, amendments []Amendment, blocks map[string]Block) EffectiveGraph {
	e := EffectiveGraph{declared: BuildGraph(m), name: m.Name}
	if len(amendments) > 0 {
		e.Amendments = append([]Amendment(nil), amendments...)
	}
	for id, b := range blocks {
		// An empty declaration is not a block. Callers hold a zero Block rather
		// than a *Block (Block.Empty's reason), so a map that has been written
		// and cleared arrives here full of them, and a task "blocked" by a zero
		// value would never be offered again.
		if b.Empty() {
			continue
		}
		if e.Blocks == nil {
			e.Blocks = make(map[string]Block, len(blocks))
		}
		e.Blocks[id] = b
	}
	e.Added = addedEdges(e.declared, e.Amendments)
	return e
}

// DecodeAmendmentRow turns one durable amendment row into an Amendment.
//
// Extracted from Runner.amendments so the READ-ONLY callers — a view rendering
// the effective graph on a poll — reconstruct the log through exactly the
// function the scheduler does. Two decoders would be two graphs, and the one the
// human is looking at would be the one that is not scheduling.
//
// A body that will not parse still yields an amendment, with kind and seq
// intact: an invisible amendment is an edge the human cannot see and cannot
// revoke (DecodeAmendmentBody's degrade rule).
func DecodeAmendmentRow(row store.DelegationAmendment) Amendment {
	a, _ := DecodeAmendmentBody(AmendmentKind(row.Kind), row.Body)
	a.RunID, a.Seq = row.RunID, row.Seq
	a.CreatedAt = time.Unix(row.CreatedAt, 0)
	if row.ApprovedAt != 0 {
		a.ApprovedAt = time.Unix(row.ApprovedAt, 0)
	}
	if row.RejectedAt != 0 {
		a.RejectedAt = time.Unix(row.RejectedAt, 0)
	}
	return a
}

// EffectiveFromRows is Effective over the DURABLE rows: the amendment log and
// the per-task `block_json` column, exactly as Runner.load reads them.
//
// It exists so a read-only caller can obtain the graph the scheduler runs on
// without a Runner, without a tick and without a write. The alternative — the
// view calling BuildGraph and rendering the DECLARED edges — is the defect
// Runner.Tick's step 3b comment names: a second scheduler that cannot see an
// approved amendment's edge, offering a task the real one says is still waiting.
//
// The block source is the COLUMN and not the file. The file is the trigger
// (§11.2) and only Detector.Poll may read it; a view that shelled out to a dozen
// worktrees per poll would be the load average §7.5 caps, and the column is what
// survives a restart anyway.
func EffectiveFromRows(m Manifest, amendments []store.DelegationAmendment, rows []store.DelegationTask) EffectiveGraph {
	log := make([]Amendment, 0, len(amendments))
	for _, row := range amendments {
		log = append(log, DecodeAmendmentRow(row))
	}
	blocks := make(map[string]Block, len(rows))
	for _, row := range rows {
		if b, ok := DecodeBlockRow(row.BlockJSON); ok {
			blocks[row.TaskID] = b
		}
	}
	return Effective(m, log, blocks)
}

// addedEdges derives Added from the log. Kept as a function rather than inlined
// because With() must produce exactly the same value Effective would on the next
// tick's re-read — an in-memory graph that disagrees with the one the DB
// reconstructs is two schedulers.
func addedEdges(g Graph, log []Amendment) []Edge {
	known := make(map[string]bool, len(g.TaskIDs))
	for _, id := range g.TaskIDs {
		known[id] = true
	}
	var out []Edge
	seen := map[Edge]bool{}
	for _, a := range log {
		ed, ok := a.Edge()
		if !ok {
			continue
		}
		// Totality: an endpoint that is not in this run's snapshot contributes
		// nothing. The alternative — inventing the node — would let an amendment
		// written against a since-edited manifest add a task to a run the human
		// never approved a task for.
		if !known[ed.From] || !known[ed.To] {
			continue
		}
		if seen[ed] {
			continue // approving the same edge twice is one dependency
		}
		seen[ed] = true
		out = append(out, ed)
	}
	sortEdges(out)
	return out
}

// sortEdges is BuildGraph's ordering, by (To, Artifact), applied to every edge
// list this file produces. Duplicated from BuildGraph rather than exported out
// of it: the ORDER is a property the two must share, and a shared helper in
// manifest.go would be an edit to a file this change does not own.
func sortEdges(es []Edge) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].To != es[j].To {
			return es[i].To < es[j].To
		}
		if es[i].Artifact != es[j].Artifact {
			return es[i].Artifact < es[j].Artifact
		}
		return es[i].From < es[j].From
	})
}

// Declared returns the manifest's own graph, unamended. Named rather than
// promoted because "the graph" is ambiguous exactly where it matters.
func (e EffectiveGraph) Declared() Graph { return e.declared }

// Name is the manifest name the graph was built from, for rendering.
func (e EffectiveGraph) Name() string { return e.name }

// Merged is the declared graph with the accepted amendment edges folded in: the
// value DetectCycle and DetectDeadlock walk. Needs is extended too, not just
// Edges, or a consumer's amended dependency would be invisible to Ready.
func (e EffectiveGraph) Merged() Graph {
	return foldEdges(e.declared, e.Added)
}

// foldEdges copies g and adds es to Edges, Needs and Producer. It copies rather
// than mutating: the declared graph is shared with everything the loader handed
// out, and a scheduler that edited it in place would leave the amended edges
// visible to the next caller that asked for the DECLARED graph — the exact
// confusion Declared()/Merged() exist to prevent.
func foldEdges(g Graph, es []Edge) Graph {
	if len(es) == 0 {
		return g
	}
	out := Graph{
		TaskIDs:  g.TaskIDs,
		Needs:    make(map[string][]string, len(g.Needs)+len(es)),
		Producer: make(map[string]string, len(g.Producer)+len(es)),
		Edges:    make([]Edge, 0, len(g.Edges)+len(es)),
	}
	for k, v := range g.Needs {
		out.Needs[k] = append([]string(nil), v...)
	}
	for k, v := range g.Producer {
		out.Producer[k] = v
	}
	out.Edges = append(out.Edges, g.Edges...)
	seen := make(map[Edge]bool, len(out.Edges))
	for _, ed := range out.Edges {
		seen[ed] = true
	}
	for _, ed := range es {
		if !seen[ed] {
			seen[ed] = true
			out.Edges = append(out.Edges, ed)
		}
		// An amended edge may name an artifact the SNAPSHOT does not know a
		// producer for (the human approved an edge over a manifest newer than
		// the snapshot). Recording it keeps Ready's published/verified test
		// answerable instead of silently unmet-forever, which would read as a
		// scheduler bug rather than as the stale snapshot it is.
		if _, ok := out.Producer[ed.Artifact]; !ok {
			out.Producer[ed.Artifact] = ed.From
		}
		if !contains(out.Needs[ed.To], ed.Artifact) {
			out.Needs[ed.To] = append(out.Needs[ed.To], ed.Artifact)
		}
	}
	sortEdges(out.Edges)
	return out
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// BlockEdges is the wait-for edges the LIVE blocks contribute (§12.1(a)'s third
// term). Only `needs-artifact` blocks produce one: the other three kinds wait on
// a human, which is shape (b), and drawing an edge for them would turn "two
// people owe me a decision" into a fabricated cycle between children.
//
// The producer is taken from the graph when the artifact is declared, and from
// the child's own `need.from` when it is not. That second case is the one that
// matters and it is why this is not simply Merged(): the common unforeseen
// dependency names an artifact NO manifest declares (§11.1), so a wait-for graph
// built only from declared producers cannot see the mutual wait that §12.1(a)
// exists to report — two children each parked on an artifact the other was
// going to write is precisely the shape, and it involves zero declared edges and
// zero accepted amendments.
func (e EffectiveGraph) BlockEdges() []Edge {
	if len(e.Blocks) == 0 {
		return nil
	}
	known := make(map[string]bool, len(e.declared.TaskIDs))
	for _, id := range e.declared.TaskIDs {
		known[id] = true
	}
	var out []Edge
	seen := map[Edge]bool{}
	// Manifest order, not map order: two Looms rendering one deadlock must name
	// the same cycle.
	for _, id := range e.declared.TaskIDs {
		b, ok := e.Blocks[id]
		if !ok || b.Kind != BlockNeedsArtifact || b.Need.Artifact == "" {
			continue
		}
		from := e.declared.Producer[b.Need.Artifact]
		if from == "" {
			from = b.Need.From
		}
		if from == "" || !known[from] {
			continue // a wait on nothing nameable is not an edge, it is a hole
		}
		ed := Edge{From: from, To: id, Artifact: b.Need.Artifact}
		if seen[ed] {
			continue
		}
		seen[ed] = true
		out = append(out, ed)
	}
	sortEdges(out)
	return out
}

// WaitFor is the graph §12.1(a) is defined over: declared edges + accepted
// amendments + live blocks. It is NOT the graph Ready runs on — a block already
// disqualifies its task there — and it is NOT the graph an amendment is checked
// against (see AmendmentCycle). It exists for exactly one question: is the run
// stopped because the children are waiting on each other?
func (e EffectiveGraph) WaitFor() Graph {
	return foldEdges(e.Merged(), e.BlockEdges())
}

// WaitCycle is §12.1(a)'s finding: the ACTUAL wait-for cycle, naming every task
// and artifact in it, or nil. A boolean would be useless here — the remedy is a
// human re-plan, and a re-plan needs the loop.
//
// It reuses DetectCycle rather than reimplementing a second walk, so the
// rendering the human sees at runtime is byte-identical to the one the loader
// produces at §4.5 — a human who has seen one has seen both.
func (e EffectiveGraph) WaitCycle() *CycleError {
	return DetectCycle(e.WaitFor(), e.name)
}

// With folds one amendment into the log and recomputes Added. An amendment with
// a Seq already present REPLACES it, because approval is an UPDATE of an
// existing row (§13.3's CAS on approved_at) and not a second row; appending
// would leave the pre-approval copy in the log and render the amendment twice.
//
// Seq 0 is treated as "not yet numbered" and always appends: the row's seq is
// assigned by the store, and collapsing every unnumbered proposal onto one
// another would lose all but the last.
func (e EffectiveGraph) With(a Amendment) EffectiveGraph {
	out := e
	out.Amendments = make([]Amendment, 0, len(e.Amendments)+1)
	replaced := false
	for _, old := range e.Amendments {
		if a.Seq != 0 && old.Seq == a.Seq && old.RunID == a.RunID {
			out.Amendments = append(out.Amendments, a)
			replaced = true
			continue
		}
		out.Amendments = append(out.Amendments, old)
	}
	if !replaced {
		out.Amendments = append(out.Amendments, a)
	}
	out.Added = addedEdges(e.declared, out.Amendments)
	return out
}

// AmendmentCycle is §11.3's last rule as a pure predicate: the cycle the
// amendment would close, or nil. block.go's Accept calls it and refuses on a
// non-nil result with ErrAmendmentCycle wrapping the *CycleError, so the human
// sees the loop and not a refusal.
//
// It runs for EVERY kind, per §4.5's "at every manifest amendment", even though
// only AmendEdge changes an edge. A scope or re-plan amendment can therefore be
// refused by a cycle it did not create — and that is correct: the graph it would
// be recorded against is already unsatisfiable, and the honest response to "the
// run is dead as planned" is to say so at the next moment a human is looking,
// not to accept an authorization widening for a task that will never be ready.
//
// It is checked against the DURABLE graph — declared + accepted amendments +
// this one — and NOT against WaitFor. Rejected alternative: including the live
// block edges, which would refuse a perfectly good amendment because two
// children are transiently parked, and parked-ness is exactly what accepting the
// amendment is about to fix. Mutual wait through live blocks is a §12.1 FINDING
// over a run that has stopped, not a veto on a human's edit.
func AmendmentCycle(e EffectiveGraph, a Amendment) *CycleError {
	cand := e.Added
	if ed, ok := a.candidateEdge(); ok {
		known := make(map[string]bool, len(e.declared.TaskIDs))
		for _, id := range e.declared.TaskIDs {
			known[id] = true
		}
		if known[ed.From] && known[ed.To] {
			cand = append(append([]Edge(nil), e.Added...), ed)
		}
	}
	return DetectCycle(foldEdges(e.declared, cand), e.name)
}

// Ready is §9.1's predicate over the EFFECTIVE graph. Same two conditions, both
// required — producer `verified` AND the artifact committed at its declared path
// on the producer's branch — with two additions §§9-12 makes real:
//
//   - a task with a live block is never ready, whatever its edges say. It is
//     already spawned and parked; proposing it again would offer the human an
//     approve action that would refuse at the launch site (§6.2 step 3);
//   - amended edges gate exactly as declared ones do. An unforeseen dependency
//     that the human accepted is a dependency.
//
// Delegating to manifest.Ready is deliberately NOT done: Ready enumerates its
// candidate states as a safety property, and the two functions answer different
// questions over different graphs. Sharing the body would tie 3a's proven
// scheduler to a graph it was never tested against.
func (e EffectiveGraph) Ready(states map[string]TaskState, published map[string]bool) []string {
	g := e.Merged()
	var out []string
	for _, id := range g.TaskIDs {
		if !schedulable(states[id]) {
			continue
		}
		if b, ok := e.Blocks[id]; ok && !b.Empty() {
			continue
		}
		if needsMet(g, id, states, published) {
			out = append(out, id)
		}
	}
	return out
}

// schedulable enumerates the states a task may be PROPOSED from. The
// enumeration is repeated from manifest.Ready rather than shared for that
// function's own stated reason: a scheduler is a safety property and it lists
// its own candidate states, so that adding a state to state.go cannot silently
// make a running task re-offerable.
func schedulable(s TaskState) bool {
	switch s {
	case "", StatePending, StateReady:
		return true
	}
	return false
}

// needsMet is §9.1's BOTH conditions over one task's effective needs.
func needsMet(g Graph, id string, states map[string]TaskState, published map[string]bool) bool {
	for _, art := range g.Needs[id] {
		producer, known := g.Producer[art]
		if !known || !published[art] {
			return false
		}
		// §9.1 says `verified`. The set is verified AND EVERYTHING DOWNSTREAM OF
		// IT, and that is a correction rather than a liberty: the state machine
		// runs verified → integrating → mergeable → merged, so a predicate that
		// admitted only verified and merged (3a's) would offer a consumer, then
		// WITHDRAW the offer while §10.2's integration ran and while the human
		// stood at §5.2's gate, then offer it again. A ready action that vanishes
		// under the cursor is worse than one that is late, and readiness must be
		// monotonic in the producer's progress or the gate cannot be trusted.
		//
		// Nothing is weakened: every one of these states means the check went
		// green (§8) and §8.3 published the artifact from a commit. §10.3 can
		// send a producer back to work afterwards, which is a stale base — but
		// that is already true at `verified`, is §10.5's disclosed alarm, and is
		// not made worse by naming the two intermediate states.
		switch states[producer] {
		case StateVerified, StateIntegrating, StateMergeable, StateMerged:
		default:
			return false
		}
	}
	return true
}

// Progress is §9.3's tick: the four buckets, computed once per transition and
// once per poll, plus the deadlock predicate over them.
//
// The buckets partition every task in the run, and the partition is the reason
// the predicate is trustworthy — a task that is in none of them is a state
// somebody added without reading this file, and Unclassified exists so that
// shows up as a rendered anomaly instead of a phantom deadlock.
type Progress struct {
	Ready []string
	// Waiting is a FIFTH bucket, and it is a deviation from §9.3's four with a
	// proof behind it. §9.3 names ready/running/blocked/terminal and Progress's
	// contract is that the buckets PARTITION the run; a `pending` task whose
	// producer has not finished is in none of the four. Putting it in
	// Unclassified would make Unclassified — documented as "always empty in a
	// correct build", and rendered by watchdog.go as ShapeStarved, i.e. "Loom
	// has a bug" — non-empty on every healthy multi-task run, which retrains the
	// human to ignore it. It is its own bucket: non-terminal, not in flight, and
	// waiting on a dependency inside the run.
	Waiting []string
	// InFlight is every task Loom or a child is actively working: spawning,
	// running, checking, integrating — AND `verified`, which is transient
	// because §10.2 is triggered when a task becomes verified and Loom enters
	// integration without being asked.
	//
	// Including `verified` here is load-bearing and is the resolution of the
	// tension in TaskState.Terminal's comment. §9.3's predicate reads "ready
	// empty, RUNNING empty, some task non-terminal"; taken literally with
	// `verified` terminal, a run whose one remaining task just went green would
	// be reported neither deadlocked (correct) nor in progress (wrong), and a
	// run whose integration slot never opened would look finished forever. It is
	// in-flight work; it is counted as in-flight work.
	//
	// `approved` is in-flight for the same reason one step earlier: the human
	// has pressed the gate and the spawn is owed. It is no longer proposable
	// (Ready will not re-offer it), so any other bucket would make a run whose
	// only remaining task was just approved read as deadlocked in the window
	// between the press and the launch.
	InFlight []string
	// Blocked is every task holding a live block declaration (§11.1). Blocked is
	// NOT in-flight — that is the entire point of the deadlock predicate, since
	// a run in which everything is blocked is exactly the state that looks like
	// progress and is not.
	Blocked []string
	// Terminal is TaskState.Terminal, including `mergeable`: a task waiting at
	// §5.2's gate is waiting on a human, which is the design working.
	Terminal []string
	// Unclassified is any task whose state matched no bucket. Always empty in a
	// correct build; rendered loudly when it is not, because the alternative is
	// a deadlock verdict computed from an incomplete partition.
	Unclassified []string
	// Deadlocked is §9.3's condition: Ready empty AND InFlight empty AND at
	// least one task non-terminal. The SHAPE of the deadlock — mutual wait
	// versus waiting on the outside world — is §12.1's, and lives in
	// watchdog.go's DetectDeadlock, because the two answer different questions:
	// this one is "has the run stopped", that one is "what do I tell the human".
	Deadlocked bool
}

// Tick computes Progress. Pure. Called on every state transition and on the poll
// loop (§9.3), so it must stay O(V+E) and allocation-cheap; it is, and there is
// no reason to be clever about a graph of a few dozen nodes.
func Tick(e EffectiveGraph, states map[string]TaskState, published map[string]bool) Progress {
	var p Progress
	p.Ready = e.Ready(states, published)
	ready := make(map[string]bool, len(p.Ready))
	for _, id := range p.Ready {
		ready[id] = true
	}

	g := e.Merged()
	for _, id := range g.TaskIDs {
		s := states[id]
		// The block declaration is tested FIRST and beats the state column,
		// because §11.2 makes the FILE the trigger: the block is observed in the
		// same tick that will move the row running→blocked under CAS, and a
		// bucketing that trusted the column would count a parked child as
		// running for exactly as long as it takes the CAS to land — which is the
		// window a deadlock hides in.
		if b, ok := e.Blocks[id]; ok && !b.Empty() {
			p.Blocked = append(p.Blocked, id)
			continue
		}
		switch {
		case ready[id]:
			// Already in p.Ready; the bucket is not repeated.
		case s == StateBlocked:
			// State says blocked with no live declaration: the file was cleared
			// or never parsed. Still blocked — the row is the durable fact and
			// the child is really parked — and block-malformed is the flag that
			// renders the discrepancy (§11.2).
			p.Blocked = append(p.Blocked, id)
		case s == StateApproved || s == StateSpawning || s == StateRunning ||
			s == StateChecking || s == StateIntegrating || s == StateVerified:
			p.InFlight = append(p.InFlight, id)
		case s.Terminal():
			// After the verified case above, deliberately: verified is Terminal
			// AND in-flight, and this file is where that is decided.
			p.Terminal = append(p.Terminal, id)
		case schedulable(s):
			p.Waiting = append(p.Waiting, id)
		default:
			p.Unclassified = append(p.Unclassified, id)
		}
	}

	// §9.3, literally: ready empty, running empty, some task non-terminal. The
	// non-terminal set is Waiting ∪ Blocked ∪ Unclassified — Terminal is by
	// definition not it, and InFlight is already known empty here. Unclassified
	// counts: an unrecognised state has no outgoing transition anyone can name,
	// so a run holding one has stopped, and reporting it as progress would hide
	// the very bug Unclassified exists to expose.
	p.Deadlocked = len(p.Ready) == 0 && len(p.InFlight) == 0 &&
		len(p.Waiting)+len(p.Blocked)+len(p.Unclassified) > 0
	return p
}

// PublishedSet turns the artifact rows into Ready's `published` argument.
//
// It is here, and not in the store, so that the RULE stays next to the predicate
// that depends on it: §8.3 publishes an artifact only after verifying it is
// committed at its declared path, so a row's mere existence is the fact Ready
// needs. A row with no commit sha is a half-written publish and does not count.
func PublishedSet(arts []store.DelegationArtifact) map[string]bool {
	if len(arts) == 0 {
		return nil
	}
	out := make(map[string]bool, len(arts))
	for _, a := range arts {
		if a.ArtifactID == "" || a.CommitSHA == "" {
			continue
		}
		out[a.ArtifactID] = true
	}
	return out
}

// ProducerRef is one entry of `delegation_tasks.base_producers`: a producer
// branch that was merged in to build this task's base (§9.2). Branch AND sha,
// both — the branch alone is a moving target (a failed integration sends the
// producer back to work, §10.3), and the sha is what makes a re-spawn reproduce
// the same tree and what §12.3's divergence computes against.
type ProducerRef struct {
	Task   string `json:"task"`
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
}

// BasePlan is §9.2's same-repo materialization, computed BEFORE any git runs.
// Pure: which producer branches must be merged into the run's pinned base, in
// which order, to give this task a worktree that contains everything it declares
// it needs.
//
// The multi-producer case is the normal one and revision 1 got it wrong. `needs`
// is a list, so `api` needing both `schema` and `config` is the shape any
// manifest with width has, and "branch from the producer's branch head",
// singular, is undefined for it. The plausible implementation picks the first
// producer and hands the child a tree missing a declared dependency; the child
// then either blocks — recording a SCHEDULER failure as a PLANNING failure and
// corrupting §2's binding kill criterion, which is the expensive part — or
// re-implements the missing piece outside its authorized paths.
//
// So: base at the run's pinned commit, then merge each same-repo producer in
// ASCENDING TASK-ID ORDER. Deterministic, so a re-spawn reproduces the tree
// byte-for-byte. One producer is the degenerate case and is STILL a merge, not a
// special path — a chain of length one is fast-forward-shaped, so §10's
// integration merges stay trivial either way, and a special case here is a
// second code path that only the rare shape exercises.
//
// Cross-repo producers contribute no merge; they arrive as an `--add-dir` on the
// consumer's launch (§9.2) and are in AddDirs.
type BasePlan struct {
	// Base is the run's pinned base sha for this task's repo. Every child of a
	// run branches from the same commit.
	Base string
	// Merge is the same-repo producers, ascending by task id.
	Merge []ProducerRef
	// AddDirs is the cross-repo producers' integration worktree paths, to be
	// passed as --add-dir. Direct reuse of slice 1's Recipe.AddDirs, which is
	// already persisted, physical-path-resolved and re-passed on resume.
	//
	// DISCLOSED (spike: docs/spikes/2026-07-22-add-dir-spike.md): --add-dir
	// grants read AND write with no second trust prompt, and the grant cannot be
	// technically narrowed. It is constrained by §7's authorization text and
	// CHECKED pre-merge by §12.3.3's comparator. Out-of-band plus checked, never
	// trusted to the brief.
	AddDirs []string
}

// PlanBase computes the BasePlan for one task. Pure — it reads the graph and the
// recorded task rows, and runs no git. Execution is worktree.go's (see the
// handoff note in run.go): the split exists so the ORDER, which is the part
// revision 1 got wrong, is testable from a literal with no repo on disk.
//
// `integration` is the run's per-repo integration worktree paths (§10.1,
// delegation_runs.integration), and it is a PARAMETER rather than something
// derived here for the reason every other path in this package is passed in:
// this file computes plans and never touches a filesystem. Without it AddDirs is
// uncomputable and the cross-repo half of §9.2 silently does nothing, which is a
// child launched without being able to SEE what it declares it needs.
//
// A producer whose row carries no branch head contributes a ref with an empty
// SHA rather than being dropped. MergeProducers refuses that loudly ("producer
// %s has no recorded sha"); dropping it would instead hand the child a tree
// missing a declared dependency, which is the exact revision-1 defect this
// function exists to prevent.
func PlanBase(e EffectiveGraph, m Manifest, t Task, tasks map[string]store.DelegationTask, bases map[string]string, integration map[string]string) BasePlan {
	p := BasePlan{Base: bases[t.Repo]}
	repos := make(map[string]string, len(m.Tasks))
	for _, mt := range m.Tasks {
		repos[mt.ID] = mt.Repo
	}
	g := e.Merged()
	seen := map[string]bool{}
	var dirs []string
	for _, art := range g.Needs[t.ID] {
		producer := g.Producer[art]
		if producer == "" || producer == t.ID || seen[producer] {
			continue
		}
		seen[producer] = true
		repo, known := repos[producer]
		if !known {
			continue // totality: a producer outside the snapshot is not a merge
		}
		if repo != t.Repo {
			// Cross-repo: the producer's REPO INTEGRATION worktree, not the
			// producer's own worktree. §10.1's is the tree that holds every
			// verified sibling of that repo; the child's own worktree holds one
			// task's work and disappears at §10.4 step 3.
			if dir := integration[repo]; dir != "" {
				dirs = append(dirs, dir)
			}
			continue
		}
		row := tasks[producer]
		p.Merge = append(p.Merge, ProducerRef{
			Task: producer, Branch: row.Branch, SHA: row.BranchHead,
		})
	}
	sort.Slice(p.Merge, func(i, j int) bool { return p.Merge[i].Task < p.Merge[j].Task })
	sort.Strings(dirs)
	for _, d := range dirs {
		if len(p.AddDirs) == 0 || p.AddDirs[len(p.AddDirs)-1] != d {
			p.AddDirs = append(p.AddDirs, d)
		}
	}
	return p
}

// EncodeProducers / DecodeProducers are `delegation_tasks.base_producers`. The
// empty slice encodes as the EMPTY STRING, not "[]", matching the column default
// so an untouched row and a no-producer row are byte-identical — EncodeFlags'
// rule, and for the same reason.
func EncodeProducers(refs []ProducerRef) string {
	if len(refs) == 0 {
		return ""
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodeProducers degrades to nil on unparseable input rather than erroring: the
// consumer is a renderer and a re-spawn planner, and neither is improved by
// refusing to work because a column has a stray byte.
func DecodeProducers(s string) []ProducerRef {
	if s == "" {
		return nil
	}
	var refs []ProducerRef
	if err := json.Unmarshal([]byte(s), &refs); err != nil {
		return nil
	}
	return refs
}

// ProducerConflict is §9.2's HARD STOP: two producer branches disagree about the
// same lines, discovered while building the consumer's base.
//
// It is not something the child absorbs. Two producers already disagree about
// the same file; that is real information about the PLAN, it belongs to a human,
// and asking a child to resolve it is asking it to make a design decision it was
// explicitly not authorized for (§7). The task does not spawn: it goes `blocked`
// with a Loom-authored `needs-decision` block naming both producers and every
// conflicting file, through the same park-and-resume path a child-authored block
// uses (block.go).
type ProducerConflict struct {
	Task string
	// Between is the two or more producers whose merge failed, in the order they
	// were merged, so the message can say which one was already in the tree.
	Between []ProducerRef
	// Files is the conflicting paths, sorted.
	Files []string
}

func (c *ProducerConflict) Error() string { return "delegate: producer branches conflict" }
