package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/henricktissink/loom/internal/arch"
	"github.com/henricktissink/loom/internal/delegate"
	"github.com/henricktissink/loom/internal/gitdiff"
	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/store"
)

// This file is the orchestration VIEW's read side (orchestration-view spec
// §3.1, §5): one bound method, OrchestrationSnapshot, that turns a delegation
// run into something the seam can paint. It renders and analyses; it never
// authors, never advances a run and never writes.
//
// Two rules shape every line below, and both are easy to erode:
//
//  1. §3.1 — THE PAYLOAD IS THE GATE, NOT THE ROUTE. A hidden project's
//     overview stays reachable on purpose: Hide/Show and Solo live on it, and
//     projectAction() re-renders it in place, so the user is left standing on a
//     hidden project's overview the instant they hide it. There is no route
//     check to fall back on. The predicate therefore runs FIRST, before any
//     read, and a hidden or unattributable root returns the bare marker.
//  2. §5.2 — slice 4 computes the READ (ready set, bottleneck, longest
//     remaining chain) and is GIVEN everything else. The manifest and the task
//     rows are the authority; nothing here re-derives a fact one of them
//     already states, and the ready set specifically routes through
//     delegate.EffectiveGraph — the graph the runner itself schedules on —
//     rather than a second scheduler that could disagree with the one that
//     actually spawns children.
//
// ProjectDocuments and ProjectDocument — §3.1's other two gated entry points —
// live in orchestration.go and go through internal/arch. They are exercised by
// this file's tests because §3.1's gate is one rule across three doors, and a
// test suite that covered two of them would go green on the door that leaks.

// OrchestrationDTO is the whole seam payload for one project.
//
// Hidden is §3.1.1's SINGLE marker. When it is true every other field is at its
// zero value — no rev, no run title, no node or document count, no error text,
// no path — because a hidden project that rendered differently depending on
// whether it had a run would leak in one bit, which is exactly what the gate
// exists to prevent. The empty slices and the zero Strip marshal to constant
// text and are identical for every hidden project; they are here because the
// frontend indexes them unconditionally, and DocumentSetDTO already set that
// precedent.
type OrchestrationDTO struct {
	Hidden bool `json:"hidden"`

	// Rev is a fingerprint of the TOPOLOGY, not of the run's progress (§7.2).
	// It changes when the node set, the edges, the bindings or the per-node
	// visibility change — the things that force a re-layout — and deliberately
	// not when a check result or a session status changes, because those patch
	// in place and a re-layout under the user's cursor destroys pan, zoom and
	// an open inspector (main.js:1418's lesson).
	//
	// It is a hash rather than a counter because the delegation manifest has no
	// `rev` field to carry one and the run's manifest_json is a DB snapshot
	// rather than a file, so §7.2's (mtime, size) fallback has nothing to
	// stat. The client compares for equality only, and a hash answers that
	// question strictly better than an updated_at in whole seconds — which
	// cannot distinguish two writes within the same second.
	Rev uint64 `json:"rev"`
	// Unchanged reports rev == sinceRev: Nodes and Edges are omitted and the
	// client keeps the ones it has. Statuses, Strip and Blocked are ALWAYS
	// sent — they are the per-tick half.
	Unchanged bool `json:"unchanged"`

	// Runs is the switcher (§9: two active runs render one graph at a time,
	// selection is local UI state).
	Runs []RunRefDTO `json:"runs"`
	Run  *RunHeadDTO `json:"run"`

	Nodes []GraphNodeDTO `json:"nodes"`
	Edges []GraphEdgeDTO `json:"edges"`
	// Layout is §5.6's stage geometry, computed in Go beside the coordinates on
	// each node. Shipped with the layout half only: it changes exactly when the
	// topology does, which is what `rev` already gates.
	Layout   LayoutDTO         `json:"layout"`
	Statuses []NodeStatusDTO   `json:"statuses"`
	Strip    StripDTO          `json:"strip"`
	Blocked  []BlockedOnYouDTO `json:"blocked"`

	// Warnings are non-fatal and rendered as .po-warn cards. Error is the fatal
	// one: a manifest that will not decode, or a schema this build cannot
	// render. Both are named and visible — §9 has no row that renders as a
	// blank panel.
	Warnings []string `json:"warnings"`
	Error    string   `json:"error"`
}

// RunRefDTO is one entry of the run switcher.
type RunRefDTO struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Slug   string `json:"slug"`
	Status string `json:"status"`
}

// RunHeadDTO is the selected run's header.
//
// RedKind and BaselineFaults are DelegationRunDTO's two fields, repeated here
// deliberately. Status `deadlocked` has two readings — §10.2's baseline fault
// and §12.1's deadlock — and without the discriminator on the payload the seam
// has to call ProjectDelegation a second time the instant a run goes red, just
// to learn which sentence to render. That is a round trip on the one tick the
// user is least patient, and a failure mode: the second call can be refused,
// can race the switcher, or can simply not be bound, leaving the escalation
// band rendering an EMPTY wait-for cycle for every baseline fault — the exact
// misrender DelegationRunDTO.RedKind exists to prevent. Both values are derived
// by runRedKind from the run row alone, so this is a fold over a row already
// read and costs no query.
type RunHeadDTO struct {
	ID             int64              `json:"id"`
	Name           string             `json:"name"`
	Slug           string             `json:"slug"`
	Status         string             `json:"status"`
	CreatedAt      int64              `json:"createdAt"`
	UpdatedAt      int64              `json:"updatedAt"`
	RedKind        string             `json:"redKind"`
	BaselineFaults []BaselineFaultDTO `json:"baselineFaults"`
}

// GraphNodeDTO is one node's LAYOUT-STABLE half: identity and the fields that
// only change when the manifest does. The moving parts are in NodeStatusDTO.
//
// Hidden is §3.1.4's opaque placeholder. Such a node keeps its edges, its place
// in the ready set and its eligibility to be the bottleneck — the picture stays
// structurally true — and carries nothing else: no title, repo, worktree, brief
// path, authorization text, artifact list or check tail. Its ID is synthetic
// too; see hiddenNodeID.
type GraphNodeDTO struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // child | ghost
	Hidden bool   `json:"hidden"`

	// X, Y and Rank are §5.6's layout, computed in internal/arch. They are on
	// the LAYOUT half of the payload on purpose: coordinates change only when
	// the topology does, and shipping them on the per-tick half is precisely
	// the re-layout-under-the-cursor §7.3 forbids.
	//
	// A hidden node carries coordinates like any other. §3.1.4 keeps the
	// topology structurally true, and a placeholder with no position would be a
	// hole in the picture that says "something was here" more loudly than the
	// placeholder does.
	X    int `json:"x"`
	Y    int `json:"y"`
	Rank int `json:"rank"`

	Title    string `json:"title"`
	Repo     string `json:"repo"` // the manifest's repo LABEL
	Path     string `json:"path"` // that label's absolute work tree
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`

	// Brief and Authorization are §2.5: the authorization-scope text is shown
	// verbatim so its presence — or absence — is auditable at a glance.
	Brief         string `json:"brief"`
	Authorization string `json:"authorization"`

	// CheckCmd is §2.1's only legitimate source of a completion badge, carried
	// here so a node with no declared check renders "no check declared" rather
	// than a blank.
	CheckCmd  []string `json:"checkCmd"`
	Artifacts []string `json:"artifacts"`

	// Warnings are the per-node chips §9 enumerates: not isolated, no
	// authorization scope, no check declared.
	Warnings []string `json:"warnings"`
}

// NodeStatusDTO is the per-poll half (§7.3): everything that patches in place.
//
// State is the LIFECYCLE (authoritative, from the task row's one CAS-guarded
// column). SessionStatus is DECORATIVE (from the sessions row's last_status,
// which the status engine wrote on this same tick). §2.1 forbids conflating
// them: a child that says it is finished and whose check has not run is
// `checking`/`running` with no check result, never green, and no session status
// promotes it.
type NodeStatusDTO struct {
	ID    string `json:"id"`
	State string `json:"state"`

	SessionName   string `json:"sessionName"`
	SessionStatus string `json:"sessionStatus"`

	CheckStatus string `json:"checkStatus"`
	CheckExit   int64  `json:"checkExit"`
	CheckAt     int64  `json:"checkAt"`
	CheckOut    string `json:"checkOut"`

	Flags []string `json:"flags"`
	// Divergence is §12.3.1-2's file lists, read from the COLUMN.
	//
	// On the per-tick half despite §7.5's cost ceiling, because it costs nothing:
	// the report was computed and persisted at the two moments §12.3 names — every
	// check run, and immediately before every merge — and this is a decode of a
	// string already read, not a git call. §12.3.1 is written as "`diverged` flag
	// WITH THE FILE LIST"; a payload that carried the flag alone would say a task
	// diverged and make the human open the merge gate to learn from what, which
	// puts the evidence behind the very gate it is supposed to inform.
	//
	// A hidden node carries the zero value. File paths are the most identifying
	// thing on this struct.
	Divergence DivergenceDTO `json:"divergence"`
	// FlagDetails is the same set with §12.2's and §12.3's sentences attached
	// and a `loud` bit for the ones the spec requires be loud. Composed in Go —
	// see DelegationTaskDTO.FlagDetails for why the wording cannot live in a
	// frontend dictionary: two of these sentences are BINDING, and the second
	// place they get written is where the hedge gets dropped.
	FlagDetails []FlagDetailDTO `json:"flagDetails"`
	// PendingSeed is §11.4's debt read from the COLUMN, not from the
	// `seed-pending` flag. §10.3's park writes the column and the flag both, but
	// the column is the debt itself and the flag is a badge a later pass can
	// clear; a view keyed on the flag would show a parked child with nothing
	// saying Loom owes it a message.
	PendingSeed bool  `json:"pendingSeed"`
	SeedFailed  bool  `json:"seedFailed"`
	Blocked     bool  `json:"blocked"` // the child declared a block (§8)
	Ready       bool  `json:"ready"`
	UpdatedAt   int64 `json:"updatedAt"`
}

// GraphEdgeDTO is one derived producer→consumer dependency. Artifact is
// carried because the manifest declares dependencies over ARTIFACT ids, never
// task ids — an author cannot express a dependency without naming the thing
// that satisfies it, and erasing that in the DTO would erase the whole point.
//
// Kind is §5.1's table: `plan` for an edge the manifest declared, `park` for one
// discovered mid-task — an accepted amendment (§11.3) or a live `needs-artifact`
// block (§11.1). It is derived from delegate.Effective, which is the same graph
// the scheduler and the deadlock detector run on, and NOT from the manifest plus
// a second opinion.
//
// The rejected alternative is what shipped first and it is worth naming: the
// frontend synthesized park edges itself from live TaskPark reads. Three things
// were wrong with it. The edges vanished the moment a park resolved, so §2.6's
// run-health count — "a run that accumulates park edges is a run whose plan was
// wrong" — could only ever report the standing parks and never the accepted
// amendments, which are the parks that WORKED. The Go layout engine never saw
// them, so §5.6 could not route them through the back-edge band and §5.1's
// "drawn above the ranks" was a client-side elbow over server-side coordinates
// that had ranked the graph as if the edge did not exist. And it was a second
// derivation of "what does this run actually depend on", living in the one place
// with no test runner.
//
// There is no `gate` kind yet: §5.1's third row is the node → integration-node
// edge, and this payload synthesizes no integration nodes. A constant on every
// edge would be a field that looks answered and is not.
type GraphEdgeDTO struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Artifact string `json:"artifact"`
	Kind     string `json:"kind"` // plan | park
	Cycle    bool   `json:"cycle"`
}

// The two edge kinds this payload can produce (§5.1).
const (
	EdgePlan = "plan"
	EdgePark = "park"
)

// LayoutDTO is the stage's geometry (§5.6). The card size travels with the
// coordinates rather than living as a frontend constant: the painter needs it
// to draw the rectangle AND to aim an edge at a node's midpoint, and two copies
// of a number that must agree is how a graph ends up with arrows landing beside
// their nodes.
type LayoutDTO struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	NodeW  int `json:"nodeW"`
	NodeH  int `json:"nodeH"`
}

// StripDTO is the run strip's figures.
//
// HiddenNodes is a BARE COUNT and never a name — slice 1 §6.4 bans
// identity-bearing counts, and "2 nodes hidden" is the whole of what this
// surface may say about them.
//
// LongestChain is measured in NODES and labelled as such. §5.2 is binding: we
// have no schedule estimates and do not fabricate them, so there is no field
// here that could be rendered as "2h 40m".
type StripDTO struct {
	Nodes       int `json:"nodes"`
	HiddenNodes int `json:"hiddenNodes"`
	Edges       int `json:"edges"`
	Merged      int `json:"merged"`
	Verified    int `json:"verified"`
	// Mergeable is §5.2's queue: green in isolation AND green combined with its
	// verified siblings, waiting on a human press. Its own figure because it is
	// the only one on this strip the human can act on directly.
	Mergeable int `json:"mergeable"`
	Ready     int `json:"ready"`
	Running   int `json:"running"`
	Failed    int `json:"failed"`

	// Repos and SharedRepos are §2.7's cohesion read — an INDICATOR, not a
	// measurement. Edge density and the number of nodes touching one repo
	// correlate with inter-task cohesion; they do not measure it. It is here
	// because slice 1 §11 asks for the cheapest available empirical read on the
	// precondition the whole arc is conditioned on.
	Repos       int `json:"repos"`
	SharedRepos int `json:"sharedRepos"`

	Bottleneck   string `json:"bottleneck"`
	LongestChain int    `json:"longestChain"`
}

// BlockedOnYouDTO is one row of the strip §5.5 puts above the graph.
//
// It is computed from the ALREADY-FILTERED node set: a hidden node cannot
// become human-blocking, because the row would name it.
type BlockedOnYouDTO struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
	Action string `json:"action"` // attach | diff | check | merge | seed
	Since  int64  `json:"since"`
}

// OrchestrationSnapshot is the seam's per-poll call.
//
// runID selects the run (0 = the newest non-terminal one, else the newest);
// sinceRev is the client's last-seen topology fingerprint. The spec writes the
// signature as (root, sinceRev), but §9 also requires a switcher with exactly
// one graph drawn at a time and selection held as local UI state — which the
// client cannot express without naming the run it is looking at. Returning
// every run's graph instead would multiply the payload for a picture only one
// of which is ever drawn.
//
// The recover() is the house pattern for a poll-path method (ListSessions,
// ListRecent): a half-built engine or a nil store on this path must cost the
// seam, never the window.
func (a *App) OrchestrationSnapshot(root string, runID int64, sinceRev uint64) (out OrchestrationDTO) {
	out = emptyOrchestration()
	defer func() {
		if r := recover(); r != nil {
			// Visible, not silent: an empty seam with no explanation is how the
			// user debugs the wrong thing for an hour.
			out = emptyOrchestration()
			out.Error = fmt.Sprintf("orchestration view failed: %v", r)
		}
	}()

	res := a.resolver()
	// §3.1.1, and it is the FIRST statement for a reason: no disk, no DB, no
	// path, no count before the predicate has answered.
	//
	// orchestrationVisible is the ONE predicate the other two gated entry
	// points use (orchestration.go). Restating it here as
	// projectVisible(res, root) would have been subtly weaker — that helper
	// treats a nil resolver as "nothing is hidden", which is right for the rail
	// (an empty rail blamed on Loom is the worse failure) and wrong here, where
	// §3.1 says an unresolvable root is hidden and the payload is the only
	// gate. §3.1 rule 5: one filter site per call; a second would be a second
	// bug.
	if !orchestrationVisible(res, root) {
		return OrchestrationDTO{Hidden: true,
			Runs: []RunRefDTO{}, Nodes: []GraphNodeDTO{}, Edges: []GraphEdgeDTO{},
			Statuses: []NodeStatusDTO{}, Blocked: []BlockedOnYouDTO{}, Warnings: []string{}}
	}
	if a.st == nil {
		return out
	}

	runs, err := a.st.ListDelegationRuns(root)
	if err != nil {
		out.Error = "could not read delegation runs: " + err.Error()
		return out
	}
	for _, r := range runs {
		out.Runs = append(out.Runs, RunRefDTO{ID: r.ID, Name: r.Name, Slug: r.Slug, Status: r.Status})
	}
	run, ok := selectRun(runs, runID)
	if !ok {
		// No run is not an error state (§3, "absence of orchestration is not an
		// error"). Blocks 1-3 are absent; the document block still renders.
		return out
	}
	out.Run = &RunHeadDTO{ID: run.ID, Name: run.Name, Slug: run.Slug,
		Status: run.Status, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt}
	out.Run.RedKind, out.Run.BaselineFaults = runRedKind(run)

	// Decoded here rather than through orchestration.go's decodeSnapshot
	// because that helper flattens the error with %v, and §9 binds a parse
	// failure to render with a LINE AND COLUMN — "invalid character '}'" alone
	// is not actionable against a several-hundred-line manifest. Same two
	// lines otherwise, including re-deriving ProjectRoot, which is `json:"-"`
	// and therefore absent from the snapshot.
	var man delegate.Manifest
	if err := json.Unmarshal([]byte(run.ManifestJSON), &man); err != nil {
		out.Error = "manifest will not parse: " + jsonErrorAt(run.ManifestJSON, err)
		return out
	}
	man.ProjectRoot = run.ProjectRoot
	if man.Version != delegate.ManifestVersion {
		// §9: no partial draw. A shape we do not understand is named, not
		// guessed at.
		out.Error = fmt.Sprintf("manifest schema %d; this Loom renders schema %d — update Loom",
			man.Version, delegate.ManifestVersion)
		return out
	}

	tasks, err := a.st.ListDelegationTasks(run.ID)
	if err != nil {
		out.Error = "could not read delegation tasks: " + err.Error()
		return out
	}
	arts, err := a.st.ListDelegationArtifacts(run.ID)
	if err != nil {
		out.Error = "could not read published artifacts: " + err.Error()
		return out
	}

	// The amendment log, so the drawn graph is the EFFECTIVE one. A read failure
	// is a WARNING and not a fatal: the declared graph is still a true picture of
	// the plan, and refusing to draw anything because one supplementary table
	// would not read is a blank panel §9 has no row for. It is said out loud
	// because a graph silently missing its accepted edges is a graph that
	// disagrees with the scheduler about what is ready.
	amds, err := a.st.ListDelegationAmendments(run.ID)
	if err != nil {
		amds = nil
		out.Warnings = append(out.Warnings,
			"could not read this run's amendments; the graph shows the declared plan only: "+err.Error())
	}

	a.buildGraphPayload(&out, res, man, delegate.EffectiveFromRows(man, amds, tasks), tasks, arts, sinceRev)
	return out
}

// emptyOrchestration is the "nothing to say" payload: every slice non-nil so
// the frontend can index it without a guard, every scalar zero.
func emptyOrchestration() OrchestrationDTO {
	return OrchestrationDTO{
		Runs: []RunRefDTO{}, Nodes: []GraphNodeDTO{}, Edges: []GraphEdgeDTO{},
		Statuses: []NodeStatusDTO{}, Blocked: []BlockedOnYouDTO{}, Warnings: []string{},
	}
}

// selectRun implements §9's switcher default: the newest non-terminal run,
// falling back to the newest of any status so a finished run is still
// inspectable. An explicit id that is not in this project's list is refused
// rather than looked up — a run id is client input, and looking it up directly
// would let the seam of a visible project render a hidden project's run.
func selectRun(runs []store.DelegationRun, want int64) (store.DelegationRun, bool) {
	if want != 0 {
		for _, r := range runs {
			if r.ID == want {
				return r, true
			}
		}
		return store.DelegationRun{}, false
	}
	for _, r := range runs { // already newest-first from the store
		if r.Status != "done" && r.Status != "abandoned" {
			return r, true
		}
	}
	if len(runs) > 0 {
		return runs[0], true
	}
	return store.DelegationRun{}, false
}

// buildGraphPayload is everything after the gate: nodes, edges, the per-node
// visibility filter, the analysis and the strip.
func (a *App) buildGraphPayload(out *OrchestrationDTO, res *projects.Resolver,
	man delegate.Manifest, e delegate.EffectiveGraph,
	tasks []store.DelegationTask, arts []store.DelegationArtifact, sinceRev uint64) {

	byTask := make(map[string]store.DelegationTask, len(tasks))
	for _, t := range tasks {
		byTask[t.TaskID] = t
	}
	// Repo label → absolute work tree, SCOPED TO THIS PROJECT (orchestration.go's
	// helper). Scoping is the fail-closed direction: a snapshot naming a label
	// this project does not own resolves to "" and becomes a placeholder rather
	// than reaching into another project's tree for a path to render.
	repoPaths := a.repoPaths(man.ProjectRoot)

	// --- per-node visibility (§3.1.4) ---------------------------------------
	//
	// A node is judged by its REPO's owning project alone, deliberately NOT
	// ANDed with its worktree — and this is a correction to the spec, not a
	// shortcut. §3.1.4 as written says `res.Visible(repo, worktree)`. A
	// delegation worktree lives under ~/.loom, outside every project root and
	// every project_repos.path, so projects.Resolver cannot attribute it and
	// Visible FAILS CLOSED on it. Taken literally the rule would replace EVERY
	// node of EVERY run with a placeholder the moment anything is hidden —
	// including when the user solos precisely the project the run belongs to,
	// which is the one moment they most want to watch it. That is bug §14.1 of
	// the delegation spec, restated one layer up; delegate.Attributor.Visible
	// fixed it for session rows by deciding on the run's project alone, and
	// this is the same fix at the graph layer.
	hidden := make(map[string]bool, len(man.Tasks))
	ids := make(map[string]string, len(man.Tasks))
	hiddenN := 0
	for _, t := range man.Tasks {
		path := repoPaths[t.Repo]
		if path == "" || !visible(res, path) {
			hiddenN++
			hidden[t.ID] = true
			ids[t.ID] = hiddenNodeID(hiddenN)
			continue
		}
		ids[t.ID] = t.ID
	}

	// --- nodes --------------------------------------------------------------
	for _, t := range man.Tasks {
		row := byTask[t.ID]
		if hidden[t.ID] {
			// The opaque placeholder. Note the ID is synthetic: task ids are
			// agent-authored and routinely name the client ("acme-ledger"), so
			// carrying the real one through would defeat §12's marshalled-JSON
			// test while satisfying the letter of §3.1.4's field list.
			out.Nodes = append(out.Nodes, GraphNodeDTO{
				ID: ids[t.ID], Kind: "child", Hidden: true,
				CheckCmd: []string{}, Artifacts: []string{}, Warnings: []string{},
			})
			continue
		}
		n := GraphNodeDTO{
			ID: t.ID, Kind: "child",
			Title: t.Title, Repo: t.Repo, Path: repoPaths[t.Repo],
			Worktree: row.Worktree, Branch: row.Branch,
			Brief: t.Brief, Authorization: t.Authorization,
			CheckCmd:  append([]string{}, t.Check.Cmd...),
			Artifacts: []string{}, Warnings: []string{},
		}
		for _, art := range t.Produces {
			n.Artifacts = append(n.Artifacts, art.ID)
		}
		// §9's per-node chips. Each is a defect worth SEEING rather than a
		// blank: "no check declared" is §2.1's whole argument failing on one
		// node, and the isolation chip only fires once a child actually exists,
		// because a planned task has no worktree yet and that is normal.
		if len(t.Check.Cmd) == 0 {
			n.Warnings = append(n.Warnings, "no check declared")
		}
		if strings.TrimSpace(t.Authorization) == "" {
			n.Warnings = append(n.Warnings, "brief declares no authorization scope")
		}
		if row.Worktree == "" && (row.SessionName != "" || delegate.TaskState(row.State).HoldsAChild()) {
			n.Warnings = append(n.Warnings, "not isolated")
		}
		out.Nodes = append(out.Nodes, n)
	}

	// --- edges, plus §9's ghost row -----------------------------------------
	//
	// The drawn graph is e.WaitFor(): declared edges + accepted amendments + the
	// wait-for edges live `needs-artifact` blocks contribute. That is §12.1(a)'s
	// graph, and drawing it is the point — the picture must show what the run is
	// ACTUALLY waiting on, including the dependency nobody planned. The declared
	// set below is only the discriminator for §5.1's `kind`.
	g := e.WaitFor()
	planned := make(map[[2]string]bool, len(e.Declared().Edges))
	for _, ed := range e.Declared().Edges {
		planned[[2]string{ed.From, ed.To}] = true
	}
	for _, ed := range g.Edges {
		// Keyed on the ENDPOINTS and not on the whole Edge: a park that names the
		// same pair of tasks as a declared edge, over a different artifact id, is
		// the plan's dependency by another name. Counting it as a park would
		// inflate §2.6's "the plan was wrong" figure with a run whose plan was
		// right, and the whole value of that number is that it is not inflated.
		kind := EdgePark
		if planned[[2]string{ed.From, ed.To}] {
			kind = EdgePlan
		}
		out.Edges = append(out.Edges, GraphEdgeDTO{
			From: ids[ed.From], To: ids[ed.To], Kind: kind,
			Artifact: artifactLabel(ed.Artifact, hidden[ed.From] || hidden[ed.To]),
		})
	}
	ghosts := 0
	for _, t := range man.Tasks {
		for _, need := range t.Needs {
			if _, produced := g.Producer[need]; produced {
				continue
			}
			// A `needs` naming an artifact nobody publishes is §9's dangling
			// edge. BuildGraph drops it (an unsatisfiable dependency is not a
			// cycle), so without this the graph would silently draw a task as
			// ready-ish with its dependency simply absent.
			ghosts++
			gid := fmt.Sprintf("ghost-%d", ghosts)
			gh := GraphNodeDTO{ID: gid, Kind: "ghost", Hidden: hidden[t.ID],
				CheckCmd: []string{}, Artifacts: []string{}, Warnings: []string{}}
			if !hidden[t.ID] {
				gh.Title = "unknown: " + need
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("task %q needs artifact %q, which no task publishes", t.ID, need))
			}
			out.Nodes = append(out.Nodes, gh)
			out.Edges = append(out.Edges, GraphEdgeDTO{
				From: gid, To: ids[t.ID], Kind: EdgePlan,
				Artifact: artifactLabel(need, hidden[t.ID]),
			})
		}
	}
	// e.WaitCycle(), not DetectCycle over the declared graph: §4.5 makes a
	// DECLARED cycle impossible at load, so a cycle drawn here is one an
	// amendment or a block closed at runtime — which is precisely §12.1(a)'s
	// mutual wait, and precisely the picture the human needs to re-plan from.
	if cyc := e.WaitCycle(); cyc != nil {
		// §9: the graph still draws. The cycle's edges are flagged and named;
		// layout never fails on account of one.
		in := map[[2]string]bool{}
		var members []string
		for _, ed := range cyc.Path {
			in[[2]string{ids[ed.From], ids[ed.To]}] = true
			members = append(members, ids[ed.From])
		}
		for i := range out.Edges {
			if in[[2]string{out.Edges[i].From, out.Edges[i].To}] {
				out.Edges[i].Cycle = true
			}
		}
		out.Warnings = append(out.Warnings, "dependency cycle: "+strings.Join(members, " → "))
	}
	for _, w := range man.Warnings {
		out.Warnings = append(out.Warnings, w.Text)
	}

	// --- statuses (the per-tick half) ---------------------------------------
	states := make(map[string]delegate.TaskState, len(tasks))
	for _, t := range tasks {
		states[t.TaskID] = delegate.TaskState(t.State)
	}
	published := make(map[string]bool, len(arts))
	for _, art := range arts {
		if art.PublishedAt > 0 {
			published[art.ArtifactID] = true
		}
	}
	// §5.2: the ready set is the SCHEDULER's, not a second implementation. A
	// scheduler in the view that disagreed with the scheduler that spawns
	// children would show a task as offerable that the gate refuses — the
	// exact class of "confident and wrong" the whole arc is written against.
	//
	// EffectiveGraph.Ready and not delegate.Ready over the declared graph, which
	// is what shipped first: Runner.Tick's step 3b promotes pending → ready over
	// the EFFECTIVE graph, so the declared-graph version here was the second
	// scheduler that comment names — blind to an accepted amendment's edge and to
	// a live block, and therefore lighting up a node the runner will not promote
	// and the gate will refuse.
	ready := map[string]bool{}
	for _, id := range e.Ready(states, published) {
		ready[id] = true
	}

	for _, t := range man.Tasks {
		row := byTask[t.ID]
		st := NodeStatusDTO{ID: ids[t.ID], State: row.State, UpdatedAt: row.UpdatedAt,
			// Non-nil even for a hidden placeholder: the frontend indexes these
			// without a guard, and a null where a list is expected is a crash
			// there rather than a blank. The empty divergence is the same
			// argument — and for a hidden node it is also the whole payload.
			FlagDetails: []FlagDetailDTO{},
			Divergence:  divergenceDTO(gitdiff.Divergence{})}
		if row.State == "" {
			st.State = string(delegate.StatePending)
		}
		if hidden[t.ID] {
			// State only. It is what keeps rank, ready-set and bottleneck
			// structurally true (§3.1.4); everything else on this struct is
			// identity or evidence and stays behind the gate.
			st.Ready = ready[t.ID]
			out.Statuses = append(out.Statuses, st)
			continue
		}
		st.SessionName = row.SessionName
		st.SessionStatus = a.nodeSessionStatus(row.SessionName)
		st.CheckStatus, st.CheckExit, st.CheckAt, st.CheckOut = row.CheckStatus, row.CheckExit, row.CheckAt, row.CheckOut
		st.Divergence = divergenceDTO(delegate.DecodeDivergence(row.Divergence))
		st.PendingSeed = row.PendingSeed != ""
		st.SeedFailed = strings.TrimSpace(row.BlockJSON) != "" && row.State == string(delegate.StateBlocked)
		st.Blocked = row.State == string(delegate.StateBlocked)
		st.Ready = ready[t.ID]
		st.Flags = []string{}
		for f, on := range delegate.DecodeFlags(row.Flags) {
			if on {
				st.Flags = append(st.Flags, string(f))
			}
		}
		sort.Strings(st.Flags) // map iteration order is not a rendering order
		st.FlagDetails = flagDetails(st.Flags)
		out.Statuses = append(out.Statuses, st)
	}

	// --- blocked-on-you (§5.5), over the FILTERED set ------------------------
	byID := make(map[string]GraphNodeDTO, len(out.Nodes))
	for _, n := range out.Nodes {
		byID[n.ID] = n
	}
	for _, s := range out.Statuses {
		n, ok := byID[s.ID]
		if !ok || n.Hidden {
			continue
		}
		if reason, action := humanBlock(s); reason != "" {
			out.Blocked = append(out.Blocked, BlockedOnYouDTO{
				ID: s.ID, Title: n.Title, Reason: reason, Action: action, Since: s.UpdatedAt,
			})
		}
	}

	// --- layout (§5.6) -------------------------------------------------------
	//
	// Computed AFTER §3.1.4's node filtering and after the ghost nodes and the
	// cycle flags are in, because all three change the topology and the layout
	// is a function of the topology. Computed BEFORE the rev is taken, so a
	// coordinate can never disagree with the rev that gates it.
	placeLayout(out)

	// --- strip + analysis ---------------------------------------------------
	out.Strip = buildStrip(out, man, g, ids, hidden, hiddenN, repoPaths)
	out.Rev = topologyRev(out)
	if sinceRev != 0 && sinceRev == out.Rev {
		// §7.2/§7.3: the layout half is what the client already holds. Sending
		// it again is what makes the graph reshuffle under an unrelated status
		// tick.
		out.Unchanged = true
		out.Nodes, out.Edges = []GraphNodeDTO{}, []GraphEdgeDTO{}
		out.Layout = LayoutDTO{}
	}
}

// placeLayout runs §5.6's layered layout and writes the coordinates onto the
// nodes.
//
// The algorithm lives in internal/arch and not here, and not in graph.js: it is
// pure, it is the most test-sensitive piece of this slice, and the frontend has
// no test runner and may not gain a dependency to get one. This function is the
// shim — it converts DTOs to the layout's input, calls it, and copies the
// answer back. It decides nothing.
func placeLayout(out *OrchestrationDTO) {
	nodes := make([]arch.LayoutNode, 0, len(out.Nodes))
	for _, n := range out.Nodes {
		nodes = append(nodes, arch.LayoutNode{ID: n.ID})
	}
	edges := make([]arch.LayoutEdge, 0, len(out.Edges))
	for _, e := range out.Edges {
		// Band is §5.1's park row: the layout keeps it out of the ranking so the
		// planned columns do not shift when a child parks and shift back when the
		// park clears. The edge is still laid out and still drawn.
		edges = append(edges, arch.LayoutEdge{
			From: e.From, To: e.To, Cycle: e.Cycle, Band: e.Kind == EdgePark,
		})
	}
	placed, w, h := arch.Layout(nodes, edges)
	at := make(map[string]arch.Placement, len(placed))
	for _, p := range placed {
		at[p.ID] = p
	}
	for i := range out.Nodes {
		p := at[out.Nodes[i].ID]
		out.Nodes[i].X, out.Nodes[i].Y, out.Nodes[i].Rank = p.X, p.Y, p.Rank
	}
	out.Layout = LayoutDTO{Width: w, Height: h, NodeW: arch.NodeW, NodeH: arch.NodeH}
}

// artifactLabel keeps an artifact id off an edge that touches a hidden node.
// Artifact ids are agent-authored and name real things ("acme-ledger-v2").
func artifactLabel(id string, touchesHidden bool) string {
	if touchesHidden {
		return ""
	}
	return id
}

// hiddenNodeID is the placeholder's synthetic identity: positional, stable for
// a given manifest, and carrying nothing.
func hiddenNodeID(n int) string { return fmt.Sprintf("hidden-%d", n) }

// nodeSessionStatus resolves a node's live session status THROUGH THE CLAUDE
// SESSION ID, which is §5.3's non-negotiable field and §14's most likely field
// failure. A resumed child mints a new tmux name (ARCHITECTURE §4.1), so keying
// on the task row's session_name alone greys out every node the moment a step
// is resumed.
//
// It reads sessions.last_status rather than polling the engine again. The
// engine wrote that column on this same tick — ListSessions() already polled —
// so a second Poll here would run tmux twice per 1.5s for an answer that is at
// most one tick older, against §7.5's cost ceiling. The staleness is already
// disclosed (§14: status lags the truth by up to one poll) and is decorative by
// §2.1: completion comes from the check, never from this.
func (a *App) nodeSessionStatus(sessionName string) string {
	if a.st == nil || sessionName == "" {
		return ""
	}
	row, ok, err := a.st.Get(sessionName)
	if err != nil || !ok {
		return "unknown"
	}
	if row.ClaudeSessionID != "" {
		if latest, ok, err := a.st.GetLatestByClaudeSessionID(row.ClaudeSessionID); err == nil && ok {
			row = latest
		}
	}
	if row.LastStatus == "" {
		return "unknown"
	}
	return row.LastStatus
}

// humanBlock is §5.5: the node states where the HUMAN is the blocker. Each
// returns the action that unblocks it, because a list of problems with no verb
// is a list the user reads and then leaves.
//
// The merge row is `mergeable`, NOT `verified`, and that is a correction §10
// forced. While the merge gate was a human reading a check result and running
// `git merge` themselves, `verified` was literally "waiting for you". It is not
// any more: §10.2 is triggered when a task becomes verified and Loom enters
// integration without being asked, so `verified` is transient and Loom-driven —
// Progress.InFlight lists it for exactly that reason. `mergeable` is the state
// §5.2's gate shows, and StateMergeable's own comment is the rule: "reaching it
// is entirely Loom's doing while leaving it is entirely the human's".
//
// Listing `verified` here instead would put a row saying "waiting for you"
// above a task Loom is actively integrating, with a merge button that refuses —
// a list of problems whose verb does not work is worse than no list.
func humanBlock(s NodeStatusDTO) (reason, action string) {
	switch {
	case s.Blocked:
		return "child declared a block", "attach"
	case s.SeedFailed:
		return "seed FAILED", "seed"
	case s.PendingSeed:
		return "seed pending", "seed"
	case s.CheckStatus == "fail":
		return "check failed", "check"
	case s.State == string(delegate.StateMergeable):
		return "green in isolation and combined — awaiting your merge", "merge"
	case s.SessionStatus == "needs_you":
		return "needs you", "attach"
	}
	return "", ""
}

// buildStrip computes §5.2's read. Everything here is over the POST-FILTER
// payload (§3.1.5), never over the raw rows.
func buildStrip(out *OrchestrationDTO, man delegate.Manifest, g delegate.Graph,
	ids map[string]string, hidden map[string]bool, hiddenN int, repoPaths map[string]string) StripDTO {

	s := StripDTO{Nodes: len(out.Nodes), HiddenNodes: hiddenN, Edges: len(out.Edges)}
	for _, st := range out.Statuses {
		switch st.State {
		case string(delegate.StateMerged):
			s.Merged++
		case string(delegate.StateVerified):
			s.Verified++
		case string(delegate.StateMergeable):
			// §5.2's queue, counted separately from `verified` because the two
			// differ in WHO is holding the task: `verified` is Loom about to
			// integrate, `mergeable` is a human who has not pressed yet. Folding
			// them together would hide the only figure on this strip that the
			// human can act on directly.
			s.Mergeable++
		case string(delegate.StateRunning), string(delegate.StateChecking),
			string(delegate.StateSpawning), string(delegate.StateIntegrating):
			// `integrating` is in flight and Loom-driven (§10.2, serialized
			// run-wide). Before §10 shipped it fell through this switch and a task
			// mid-integration was counted as nothing at all — present in the node
			// count and absent from every figure that explains it.
			s.Running++
		case string(delegate.StateFailed):
			s.Failed++
		}
		if st.Ready {
			s.Ready++
		}
	}

	// Cohesion indicator: distinct repos, and how many carry more than one
	// task. Counted over the manifest (not the filtered nodes) because a hidden
	// node still occupies a repo, and a count that moved when a project was
	// hidden would be a count that reported hiding.
	perRepo := map[string]int{}
	for _, t := range man.Tasks {
		perRepo[t.Repo]++
	}
	s.Repos = len(perRepo)
	for _, n := range perRepo {
		if n > 1 {
			s.SharedRepos++
		}
	}

	done := map[string]bool{}
	for _, st := range out.Statuses {
		if st.State == string(delegate.StateMerged) || st.State == string(delegate.StateVerified) {
			done[st.ID] = true
		}
	}
	s.Bottleneck = bottleneck(g, ids, done, out.Statuses)
	s.LongestChain = longestChain(g, ids, done)
	return s
}

// bottleneck is §5.2: among the nodes currently blocking anything, the one with
// the largest count of TRANSITIVELY blocked descendants. Ties break on longest
// blocked duration, then on id so the answer is deterministic.
//
// Exactly one, or none — "none" being the honest answer when nothing is
// blocking, which is why this returns "" rather than the first node.
func bottleneck(g delegate.Graph, ids map[string]string, done map[string]bool, sts []NodeStatusDTO) string {
	kids := map[string][]string{}
	for _, e := range g.Edges {
		kids[e.From] = append(kids[e.From], e.To)
	}
	since := map[string]int64{}
	for _, st := range sts {
		since[st.ID] = st.UpdatedAt
	}
	best, bestN, bestSince := "", 0, int64(0)
	for _, id := range g.TaskIDs {
		if done[ids[id]] {
			continue
		}
		// Reachability with an explicit seen-set: a cycle must cost a bounded
		// walk, never a hang. §9 is emphatic that layout and analysis survive
		// one.
		seen := map[string]bool{id: true}
		stack := append([]string{}, kids[id]...)
		n := 0
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[cur] {
				continue
			}
			seen[cur] = true
			if !done[ids[cur]] {
				n++
			}
			stack = append(stack, kids[cur]...)
		}
		if n == 0 {
			continue
		}
		st := since[ids[id]]
		better := n > bestN ||
			(n == bestN && st != 0 && (bestSince == 0 || st < bestSince)) ||
			(n == bestN && st == bestSince && ids[id] < best)
		if best == "" || better {
			best, bestN, bestSince = ids[id], n, st
		}
	}
	return best
}

// longestChain is §5.2's longest remaining chain, in NODES. Not in minutes:
// weighting unstarted nodes by a made-up duration would put a confident, wrong
// number on the one screen meant to be trusted at a glance.
//
// The memo doubles as the cycle guard — a node already on the current path
// contributes 0 rather than recursing, so a cyclic manifest returns a number
// instead of a stack overflow.
func longestChain(g delegate.Graph, ids map[string]string, done map[string]bool) int {
	kids := map[string][]string{}
	for _, e := range g.Edges {
		kids[e.From] = append(kids[e.From], e.To)
	}
	memo := map[string]int{}
	onPath := map[string]bool{}
	var walk func(string) int
	walk = func(id string) int {
		if done[ids[id]] {
			return 0
		}
		if v, ok := memo[id]; ok {
			return v
		}
		if onPath[id] {
			return 0
		}
		onPath[id] = true
		best := 0
		for _, k := range kids[id] {
			if v := walk(k); v > best {
				best = v
			}
		}
		onPath[id] = false
		memo[id] = best + 1
		return memo[id]
	}
	longest := 0
	for _, id := range g.TaskIDs {
		if v := walk(id); v > longest {
			longest = v
		}
	}
	return longest
}

// topologyRev fingerprints the half of the payload that forces a re-layout
// (§7.2). Deliberately excludes state, check results, flags and session status:
// those are the patch-in-place half, and folding them in would re-layout the
// graph on every check — destroying pan, zoom and an open inspector.
//
// Per-node Hidden IS folded in, so unhiding a project re-lays the graph out on
// the next poll rather than leaving placeholders on screen until something else
// changes.
func topologyRev(out *OrchestrationDTO) uint64 {
	h := fnv.New64a()
	w := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	if out.Run != nil {
		w("run", fmt.Sprint(out.Run.ID))
	}
	for _, n := range out.Nodes {
		w("n", n.ID, n.Kind, fmt.Sprint(n.Hidden), n.Title, n.Repo, n.Path,
			n.Worktree, n.Branch, n.Brief, n.Authorization,
			strings.Join(n.CheckCmd, "\x00"), strings.Join(n.Artifacts, "\x00"),
			strings.Join(n.Warnings, "\x00"))
	}
	for _, e := range out.Edges {
		// Kind is folded in: a park edge appearing or clearing genuinely changes
		// the topology — it changes the ranking, because a banded edge is excluded
		// from it — so it must move the rev or the graph keeps the stale columns
		// until something unrelated changes.
		w("e", e.From, e.To, e.Artifact, e.Kind, fmt.Sprint(e.Cycle))
	}
	for _, s := range out.Warnings {
		w("w", s)
	}
	return jsSafeRev(h.Sum64())
}

// jsSafeRev truncates a 64-bit fingerprint to the 53 bits a JSON number can
// carry EXACTLY, and this is a correctness fix, not tidiness.
//
// The rev makes a round trip: Go marshals it into the snapshot, the webview
// parses it as an IEEE-754 double, and the client hands it straight back as
// `sinceRev` on the next poll. Above 2^53 that double has already lost the low
// bits, so the value Go receives is NOT the value Go sent and the §7.2
// comparison `sinceRev == out.Rev` can never be true. Measured on the shipped
// fnv64a: 20 of 20 sampled revs failed to round-trip. The observable effect was
// that `unchanged` never fired, every 1.5s poll shipped the full node and edge
// payload, and the client called setTopology on each one — which clears the
// card Map and replaceChildren()s both layers. That is precisely the
// "no re-layout, no innerHTML on the graph host" that §7.3 is written to
// forbid, arrived at through the wire type rather than through the render code
// everyone was watching.
//
// Rejected alternative: send the rev as a string. It round-trips exactly and
// costs nothing on the wire, but it changes the DTO's type, the client's
// comparison and the zero value that means "I have nothing" — three edits
// across a seam, to buy 11 bits of fingerprint that no caller needs. A 53-bit
// hash over the handful of distinct topologies one project sees in a session
// has a collision probability far below the odds of the manifest being
// mis-authored in the first place, and a collision degrades to "the graph did
// not re-layout when it should have", which the next topology change corrects.
func jsSafeRev(v uint64) uint64 {
	v &= (uint64(1) << 53) - 1
	// Never 0: 0 is the client's "I have nothing", and a real revision that
	// hashed to it would suppress the first full payload forever.
	if v == 0 {
		return 1
	}
	return v
}

// jsonErrorAt turns a decode failure into §9's line/column. json.SyntaxError
// carries a byte offset and nothing else, so the position is derived here —
// "line 12, column 3" is actionable, "invalid character '}'" alone is not.
func jsonErrorAt(src string, err error) string {
	var off int64 = -1
	switch e := err.(type) {
	case *json.SyntaxError:
		off = e.Offset
	case *json.UnmarshalTypeError:
		off = e.Offset
	}
	if off < 0 || int(off) > len(src) {
		return err.Error()
	}
	line, col := 1, 1
	for i := 0; i < int(off) && i < len(src); i++ {
		if src[i] == '\n' {
			line, col = line+1, 1
			continue
		}
		col++
	}
	return fmt.Sprintf("%s (line %d, column %d)", err.Error(), line, col)
}
