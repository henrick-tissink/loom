package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/arch"
	"github.com/henricktissink/loom/internal/delegate"
	"github.com/henricktissink/loom/internal/gitdiff"
	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/store"
)

// This file is the DTO shim for the orchestrator (slice 2) and the
// architecture-document view (slice 4). It holds no policy of its own.
//
// That is the whole design rule here and it is easy to erode: the §3.1
// visibility gate, the containment check and the document admission rules ALL
// live inside internal/arch, evaluated through internal/projects and nowhere
// else. A second entry point that reads disk itself would pass every test in
// internal/arch while bypassing every rule in it — which is exactly how the
// surface that forgets a branch still goes green. So every function below is a
// thin wrapper, and adding a third one means routing it through arch too.

// OrchestratorDTO is the orchestrator half of the project overview.
//
// Live is derived rather than shipped raw: the store's sentinel is ended_at ==
// -1, and a frontend that re-derived that from the number would be a second
// place the sentinel is known.
type OrchestratorDTO struct {
	SessionName string `json:"sessionName"`
	SpawnedAt   int64  `json:"spawnedAt"`
	EndedAt     int64  `json:"endedAt"`
	Live        bool   `json:"live"`
	// Claiming is the "claim in flight" state (§9): a row exists but no
	// session name has been written yet, because the claim is deliberately
	// taken BEFORE the launch. Rendering it as live would promise a session
	// the user cannot attach to.
	Claiming bool `json:"claiming"`

	// SpawnCount and LastSession come from state.json's spawn ledger, and
	// AssembledAt/BriefBytes from its brief record. Zero when no brief has
	// ever been assembled, which the frontend renders as blanks — never as an
	// error, since "never briefed" is the normal state of a new project.
	SpawnCount  int    `json:"spawnCount"`
	LastSession string `json:"lastSession"`
	AssembledAt int64  `json:"assembledAt"`
	BriefBytes  int    `json:"briefBytes"`
	// TruncatedSections names the brief sections that hit their cap (§5.3).
	// Surfaced because a silently truncated brief is a brief whose missing
	// half nobody knows about.
	TruncatedSections []string `json:"truncatedSections"`

	// NotesDir is where the agent-authored brain lives (§3). Empty means "not
	// materialized yet" — the first spawn writes it.
	NotesDir string `json:"notesDir"`
}

// orchestratorsByRoot reads the orchestrator table ONCE and indexes it.
//
// ListProjectDetails is a per-project loop, so the obvious spelling — ask for
// this project's orchestrator inside the loop — is an N+1 against SQLite for a
// table with at most one row per project. One read plus a map is the same
// answer without the fan-out, and it keeps the whole DTO on a single
// consistent snapshot: a per-project query could interleave with a spawn and
// return a set no single instant ever had.
//
// A store error yields a nil map, not a failure: the orchestrator block is
// supplementary, and a project list that refuses to render because one
// supplementary table was unreadable is a worse outcome than a list with the
// block missing.
func (a *App) orchestratorsByRoot() map[string]store.Orchestrator {
	if a.st == nil {
		return nil
	}
	rows, err := a.st.ListOrchestrators()
	if err != nil {
		return nil
	}
	out := make(map[string]store.Orchestrator, len(rows))
	for _, o := range rows {
		out[o.ProjectRoot] = o
	}
	return out
}

// orchestratorDTO joins a project's orchestrator row with its state.json
// ledger. Both halves are optional and independently so: a project can have a
// live orchestrator and no state file (first spawn racing the write), or a
// state file and no orchestrator (every generation has ended). Neither case is
// an error and neither suppresses the other half.
func (a *App) orchestratorDTOFor(root string, o store.Orchestrator, haveRow bool) *OrchestratorDTO {
	if root == store.UngroupedRoot {
		return nil
	}
	dto := &OrchestratorDTO{}
	if haveRow {
		dto.SessionName = o.SessionName
		dto.SpawnedAt = o.SpawnedAt
		dto.EndedAt = o.EndedAt
		dto.Live = o.EndedAt < 0
		dto.Claiming = o.EndedAt < 0 && o.SessionName == ""
	}
	if a.orch != nil {
		if st, ok := a.orch.State(root); ok {
			dto.SpawnCount = st.SpawnCount
			dto.LastSession = st.LastSession
			dto.AssembledAt = st.AssembledAt
			dto.BriefBytes = st.BriefBytes
			dto.NotesDir = st.NotesDir
			dto.TruncatedSections = st.TruncatedSections
		}
	}
	if dto.TruncatedSections == nil {
		dto.TruncatedSections = []string{}
	}
	if !haveRow && dto.SpawnCount == 0 && dto.AssembledAt == 0 {
		// Nothing to say about this project at all. A zero block would render
		// an empty orchestrator card on every never-touched project.
		return nil
	}
	return dto
}

// SpawnOrchestrator launches the project's orchestrator session (§7).
//
// The singleton guarantee is the STORE's compare-and-swap, not a check here: a
// UI guard is per-process and two Loom instances against one loom.db is a
// supported state, so a disabled button is a courtesy and the claim is the
// enforcement. This method therefore does not pre-check for an existing
// orchestrator — it lets the claim refuse and surfaces the refusal.
func (a *App) SpawnOrchestrator(root, intent string) (string, error) {
	if a.orch == nil {
		return "", fmt.Errorf("orchestrator unavailable")
	}
	res, err := a.orch.Spawn(root, intent, launchCols, launchRows)
	if err != nil {
		return "", err
	}
	return res.SessionName, nil
}

// ReassembleBrief rewrites brief.md and state.json without spawning (§10). It
// is how a human refreshes the drift report before deciding whether a new
// orchestrator is worth the tokens, and it deliberately does not touch the
// spawn ledger — a refresh is not a spawn.
func (a *App) ReassembleBrief(root string) error {
	if a.orch == nil {
		return fmt.Errorf("orchestrator unavailable")
	}
	_, err := a.orch.Reassemble(root, "")
	return err
}

// SetProjectNotesDir repoints the project's notes directory (§3).
//
// It goes through orchestrator.Service.MoveNotes rather than writing the
// column directly: MoveNotes enforces that the directory already exists, which
// is the same precondition the spawn path applies, and a second writer that
// skipped it would let a typo'd path be stored and only fail at the next spawn
// — far from the gesture that caused it.
func (a *App) SetProjectNotesDir(root, dir string) error {
	if a.orch == nil {
		return fmt.Errorf("orchestrator unavailable")
	}
	return a.orch.MoveNotes(root, dir)
}

// SweepOrchestrators is §7/§9's recovery pass, run beside projects.Sweep.
//
// The count is returned rather than logged so the caller can surface it. A
// sweep that quietly stops working is how the per-project singleton silently
// becomes a duplicate, and a duplicate orchestrator is two agents writing the
// same notes files.
func (a *App) SweepOrchestrators() (int64, error) {
	if a.orch == nil {
		return 0, nil
	}
	return a.orch.Sweep()
}

// --- architecture documents (orchestration-view §3.1, §4) ----------------

// DocumentSetDTO is arch.Set for the frontend.
//
// Hidden is §3.1.1's SINGLE marker: when true every other field is zero. No
// count, no title, no path, no error text — a hidden project that rendered
// differently depending on whether it had documents would leak in one bit,
// which is the whole failure the gate exists to prevent.
type DocumentSetDTO struct {
	Hidden    bool            `json:"hidden"`
	Documents []arch.Document `json:"documents"`
	Refusals  []arch.Refusal  `json:"refusals"`
	Warnings  []string        `json:"warnings"`
}

// ProjectDocuments returns the project's architecture document set.
//
// Repos is this project's project_repos.path rows IN LIST ORDER: the order is
// part of the request because the convention scan walks it, and a set that
// reordered between calls would reorder the rendered document list for no
// reason the user can see.
//
// Manifest is false until slice 3's manifest loader is wired (stage 4b), so
// the convention scan runs. When a manifest exists its documents[] entries map
// 1:1 onto arch.Declared and Manifest goes true, which stands the convention
// scan down — §4.1: discovery is DECLARED FIRST, discovered second, because a
// rendering layer must not guess which markdown file is the architecture once
// the orchestrator has said.
func (a *App) ProjectDocuments(root string) DocumentSetDTO {
	out := DocumentSetDTO{Documents: []arch.Document{}, Refusals: []arch.Refusal{}, Warnings: []string{}}
	if a.st == nil {
		return out
	}
	set := arch.Documents(a.resolver(), a.docRequest(root), a.docs)
	if set.Hidden {
		// Return the bare marker. Copying the (already zero) fields across
		// would be a place a future edit could leak one.
		return DocumentSetDTO{Hidden: true,
			Documents: []arch.Document{}, Refusals: []arch.Refusal{}, Warnings: []string{}}
	}
	if set.Documents != nil {
		out.Documents = set.Documents
	}
	if set.Refusals != nil {
		out.Refusals = set.Refusals
	}
	if set.Warnings != nil {
		out.Warnings = set.Warnings
	}
	return out
}

// docRequest builds the arch.Request for a project. Extracted so that
// ProjectDocuments and ProjectDocumentsRev cannot drift: a freshness probe over
// a DIFFERENT tree set than the payload it guards is worse than no probe at
// all, because it would report "fresh" while the rendered set was stale and
// there would be no symptom pointing at the disagreement.
func (a *App) docRequest(root string) arch.Request {
	repos := []string{}
	if a.st != nil {
		if rows, err := a.st.ListProjectRepos(root); err == nil {
			for _, r := range rows {
				repos = append(repos, r.Path)
			}
		}
	}
	// Manifest stays false until slice 3's loader is wired (§4.1), matching
	// ProjectDocuments — see its comment for why the flag exists.
	return arch.Request{Root: root, Repos: repos}
}

// ProjectDocumentsRev is §7.4's freshness probe: the fingerprint of the
// document set's (path, size, mtime), computed without reading, parsing or
// rendering anything.
//
// It exists because the frontend had no way to ask "did the documents change"
// short of calling ProjectDocuments — which loads and markdown-renders the
// whole set. Paying that on the 1.5s poll is what §7.5's cost ceiling forbids,
// so documents refreshed on open and manual Refresh only, and an ADR written
// while the overview was on screen never appeared.
//
// Returns 0 for a hidden or unattributable project, which is the same constant
// a project with no documents returns. §3.1.1 again: a number that moved for a
// hidden project would report that its files were being edited, one bit per
// poll, which is exactly the signal the marker exists to withhold. The
// frontend's response to 0 is to render nothing, so the two cases are also
// indistinguishable downstream.
func (a *App) ProjectDocumentsRev(root string) uint64 {
	rev, ok := arch.Rev(a.resolver(), a.docRequest(root))
	if !ok {
		return 0
	}
	// Same 53-bit truncation as the graph rev, for the same reason: this
	// number round-trips through a JSON number in the webview, and a value the
	// client cannot hold exactly is a value that compares unequal to itself
	// forever. See jsSafeRev (arch.go) for the measurement.
	return jsSafeRev(rev)
}

// DocumentDTO is one opened document, or the reason it was refused.
type DocumentDTO struct {
	Hidden   bool           `json:"hidden"`
	Doc      *arch.Document `json:"doc"`
	Refusal  *arch.Refusal  `json:"refusal"`
	Error    string         `json:"error"`
	NotFound bool           `json:"notFound"`
}

// ProjectDocument opens one document by absolute path.
//
// The path is UNTRUSTED input even when it came from a document Loom itself
// rendered: an agent-authored relative link inside a document is the same
// input class as a manifest documents[] entry. arch.Open re-applies containment
// and visibility on every call, which is why this takes a path rather than an
// index into the last set — an index would make the check skippable by
// anything holding a stale set.
func (a *App) ProjectDocument(path string) DocumentDTO {
	doc, ref, err := arch.Open(a.resolver(), path, a.docs)
	switch {
	case err == nil:
		return DocumentDTO{Doc: &doc}
	case errors.Is(err, arch.ErrHidden):
		// §3.1.1 again: the marker and nothing else. Not "not found", which
		// would distinguish a hidden project with the file from one without.
		return DocumentDTO{Hidden: true}
	case arch.Refused(err):
		// A named refusal renders as a .po-warn card: the path and the rule it
		// broke. Silently dropping it would be the worst of both worlds — no
		// content and no explanation.
		return DocumentDTO{Refusal: &ref, Error: err.Error()}
	default:
		return DocumentDTO{Error: err.Error(), NotFound: true}
	}
}

// --- the §3.1 gate --------------------------------------------------------

// orchestrationVisible is orchestration-view §3.1's predicate, and it is
// deliberately NOT projects.go's projectVisible().
//
// projectVisible fails OPEN — a nil resolver means "nothing is hidden", which
// is right for the rail, where an empty list blamed on Loom is the worse
// failure. It is wrong here. §3.1 is explicit: an unattributable, unresolvable
// or unknown root is treated as HIDDEN, because the route to this data is
// deliberately open (ListProjectDetails is unfiltered so a hidden project can
// be unhidden from its own overview) and the payload is the only gate there is.
// Everything the bindings below return — repo and worktree paths, branch names,
// check output tails, manifest task titles — is exactly the client-identifying
// material §6 exists to keep off a shared screen.
//
// The Project() lookup is not redundant with ProjectVisible(): ProjectVisible
// answers "not hidden" for a root it has never heard of, which is the
// unattributable case §3.1 requires us to refuse.
func orchestrationVisible(res *projects.Resolver, root string) bool {
	if res == nil || root == "" || root == store.UngroupedRoot {
		return false
	}
	if _, ok := res.Project(root); !ok {
		return false
	}
	return res.ProjectVisible(root)
}

// --- slice 2: the orchestrator seam (orchestrator §10) --------------------

// OrchestratorStateDTO is the per-project orchestrator read, gated.
//
// It exists beside the ListProjectDetails join rather than replacing it: §10
// binds the overview's block to that join precisely so there is no per-project
// IPC and no N+1. This is the seam a surface keyed on ONE root uses — §3.1's
// three gated calls are all of that shape — and it carries the gate the join's
// host screen deliberately does not have.
//
// Hidden is the single marker again: when true, Orchestrator is nil. Not "no
// orchestrator", which is a different fact and would render differently.
type OrchestratorStateDTO struct {
	Hidden       bool             `json:"hidden"`
	Orchestrator *OrchestratorDTO `json:"orchestrator"` // nil = none has ever run
}

// ProjectOrchestrator answers §10's three questions for one project: does this
// project have an orchestrator, is it live, and what does its state ledger say.
func (a *App) ProjectOrchestrator(root string) OrchestratorStateDTO {
	if !orchestrationVisible(a.resolver(), root) {
		return OrchestratorStateDTO{Hidden: true}
	}
	if a.st == nil {
		return OrchestratorStateDTO{}
	}
	o, ok := a.orchestratorsByRoot()[root]
	return OrchestratorStateDTO{Orchestrator: a.orchestratorDTOFor(root, o, ok)}
}

// --- slice 3a: delegation runs and tasks ---------------------------------

// DelegationDTO is a project's delegation runs, newest first.
type DelegationDTO struct {
	Hidden bool               `json:"hidden"`
	Runs   []DelegationRunDTO `json:"runs"`
	// Error is a store failure, rendered rather than swallowed. An empty run
	// list and an unreadable run list are different facts and a surface that
	// showed the same thing for both would hide the second one forever.
	Error string `json:"error"`
}

// DelegationRunDTO is one row of delegation_runs plus its tasks.
type DelegationRunDTO struct {
	ID        int64  `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	// ManifestError is a snapshot that will not decode. The tasks still list —
	// their state lives in the task rows, not in the snapshot — but their
	// titles, needs and check argv do not, and saying so is the difference
	// between a degraded render and a lie.
	ManifestError string              `json:"manifestError"`
	Tasks         []DelegationTaskDTO `json:"tasks"`

	// RedKind discriminates the two things status `deadlocked` can mean, because
	// they read completely differently to a human and share a status only
	// because inventing a second red status string would make every existing
	// consumer of delegation_runs.status treat the new value as not-red
	// (integrate.go's rejected alternative, kept rejected here).
	//
	// §12.1's deadlock is a wait-for cycle or a list of owed decisions; §10.2's
	// baseline fault is "this repo's own tree is broken and no child is to
	// blame". A view that renders `deadlocked` as a wait-for cycle
	// unconditionally shows an EMPTY cycle for every baseline fault — the
	// specific misrender this field exists to prevent.
	//
	// Empty unless Status is `deadlocked`. It is derived rather than stored: the
	// discriminator is delegation_runs.integration, which is already the place
	// §10.2 says the baseline result lives, and a second column recording which
	// kind of red a run is would be a second thing to keep in step with it.
	RedKind string `json:"redKind"`
	// BaselineFaults is the repos whose recorded baseline is not `pass`, with
	// the captured reason. Non-empty exactly when RedKind is `baseline-fault`.
	BaselineFaults []BaselineFaultDTO `json:"baselineFaults"`
}

// The two readings of status `deadlocked`. Exported as strings rather than an
// enum because they cross the wire to JS, where an unknown value must be
// renderable as itself.
const (
	// RedBaselineFault — §10.2: a repo's integration baseline is red, so the
	// merge could not be attributed to any child. Nobody is blamed and the
	// remedy is to repair the repo, not to re-plan the manifest.
	RedBaselineFault = "baseline-fault"
	// RedDeadlock — §12.1: the run stopped with work outstanding. The remedy is
	// a human re-plan, and the render is the wait-for cycle or the owed
	// decisions.
	RedDeadlock = "deadlock"
)

// BaselineFaultDTO is one repo's failing integration baseline (§10.2).
type BaselineFaultDTO struct {
	Repo string `json:"repo"`
	// Status is the CheckStatus recorded at Head — `fail`, `unpublished`,
	// `env-suspect` or `infra-error`. Carried verbatim rather than collapsed to
	// a boolean: `infra-error` means the gate never ran and `fail` means it ran
	// and said no, and a human repairs those differently.
	Status string `json:"status"`
	Head   string `json:"head"`
	At     int64  `json:"at"`
	// Reason is Baseline.Out — the captured output, already capped by the writer.
	Reason string `json:"reason"`
}

// runRedKind derives §10.2-vs-§12.1 from the run row alone.
//
// Baseline.Red() is the predicate and it is deliberately not re-implemented
// here: it counts `unpublished` and `infra-error` as red, because none of them
// is evidence the tree is good, and a view that read "not pass" as "fine" would
// certify on the absence of a result.
//
// A `deadlocked` run with NO red baseline is a §12.1 deadlock. That fallback is
// the right way round: a baseline fault always leaves its reason in the column,
// so the absence of one is positive evidence of the other reading, whereas
// defaulting to `baseline-fault` would invent a repo fault out of a decode
// failure (DecodeBaselines degrades to an empty map on corrupt JSON).
func runRedKind(r store.DelegationRun) (string, []BaselineFaultDTO) {
	if r.Status != "deadlocked" {
		return "", []BaselineFaultDTO{}
	}
	faults := []BaselineFaultDTO{}
	bs := delegate.DecodeBaselines(r.Integration)
	for _, repo := range sortedRepoLabels(bs) {
		b := bs[repo]
		if !b.Red() {
			continue
		}
		faults = append(faults, BaselineFaultDTO{
			Repo: repo, Status: string(b.Status), Head: b.Head, At: b.At, Reason: b.Out,
		})
	}
	if len(faults) == 0 {
		return RedDeadlock, faults
	}
	return RedBaselineFault, faults
}

// sortedRepoLabels keeps the fault list stable across polls: the source is a map
// and an unsorted render would reshuffle the rows on every tick for no reason a
// user can see (flagList's rule).
func sortedRepoLabels(m map[string]delegate.Baseline) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DelegationTaskDTO is one task's state chip and the facts behind it.
//
// Terminal and HoldsChild are derived here rather than shipped as a state
// string for the frontend to switch on, for the same reason OrchestratorDTO
// derives Live from the ended_at sentinel: a second place that enumerates the
// state sets is a second place that forgets a state when one is added. §13.2's
// flags stay a separate list because they are a separate axis — a task is
// `running` AND `diverged`, never one or the other.
type DelegationTaskDTO struct {
	TaskID      string `json:"taskId"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Repo        string `json:"repo"`
	Branch      string `json:"branch"`
	Worktree    string `json:"worktree"`
	SessionName string `json:"sessionName"`

	CheckStatus string `json:"checkStatus"`
	CheckExit   int64  `json:"checkExit"`
	CheckAt     int64  `json:"checkAt"`

	Flags []string `json:"flags"`
	// FlagDetails is the same set with §12.2's and §12.3's sentences attached,
	// and a `loud` bit for the ones the spec requires be loud.
	//
	// In Go rather than in the frontend because the TUI must not say something
	// different about the same badge, and because two of these sentences are
	// BINDING wording: §12.3.3's drift says "changed since spawn" and never "the
	// child wrote this" (the walk cannot tell the human's own edits from a
	// child's), and §12.2's stalled is a LABEL — nothing was killed. A frontend
	// dictionary would be the second place those get written, and the second
	// place is where the hedge gets dropped.
	FlagDetails []FlagDetailDTO `json:"flagDetails"`
	Terminal    bool            `json:"terminal"`
	HoldsChild  bool            `json:"holdsChild"`

	// Needs names ARTIFACT ids, never task ids (§4.2) — rendered as text in 3a;
	// slice 4 owes the graph.
	Needs []string `json:"needs"`
	// CheckArgv is rendered VERBATIM. It is arbitrary code from an
	// agent-authored file and the human's reading of it is the whole review;
	// there is no sandbox and nothing here claims one.
	CheckArgv []string `json:"checkArgv"`
	// Blocked carries block.json's raw text (§11.1). Raw, because a malformed
	// declaration is a `block-malformed` render and never a silent drop — a
	// swallowed block is a child parked forever with nobody told.
	Blocked string `json:"blocked"`
	// Divergence is the LAST RECORDED report (§12.3.1-2), decoded from the row.
	// Read here rather than recomputed: this DTO is built for every task on
	// every poll, and shelling out to git per task per tick is how a list view
	// becomes a load average. The recompute happens at the two moments §12.3
	// names — every check run, and immediately before every merge.
	Divergence DivergenceDTO `json:"divergence"`

	// SeedOwed is §11.4's durable debt read straight from the column, and it is
	// deliberately NOT the `seed-pending` flag.
	//
	// The two disagree, and the disagreement is a real one: Rendezvous.Seed sets
	// the flag with the column, but §10.3's park (Integrator.park) writes the
	// pending seed WITHOUT it — an integration send-back is therefore an owed
	// delivery that carries no badge until something later tries to deliver it.
	// A run list that only rendered the flag would show a parked child with
	// nothing saying Loom owes it a message, which is the state §11.4 exists to
	// make visible. This reads the debt itself; the flag stays rendered beside it
	// because a flag that IS set also means a delivery has already been tried.
	SeedOwed bool `json:"seedOwed"`

	// SnapshotBaseline reports whether §12.3.3's out-of-worktree tripwire has a
	// baseline for this task at all — i.e. whether delegation_tasks.spawn_snapshot
	// was written at spawn.
	//
	// It exists so the view can render SnapshotDrift.NoBaseline DISTINCTLY from
	// "no change". Those are an absence of EVIDENCE and evidence of ABSENCE, and
	// showing the second when the truth is the first tells the human the child
	// wrote nothing outside its worktree when in fact nobody ever looked.
	//
	// The DRIFT itself is deliberately not computed here. Comparing it means
	// stat-walking every in-scope repo and integration worktree, per task, on
	// every poll — the same load-average argument that keeps Divergence a read
	// of the last recorded report rather than a fresh git call. §12.3 names the
	// two moments the comparison actually happens (every check run, and
	// immediately before every merge) and both are the runner's, not a view's.
	SnapshotBaseline bool `json:"snapshotBaseline"`
}

// ProjectDelegation lists a project's runs and tasks with their state chips.
func (a *App) ProjectDelegation(root string) DelegationDTO {
	out := DelegationDTO{Runs: []DelegationRunDTO{}}
	if !orchestrationVisible(a.resolver(), root) {
		return DelegationDTO{Hidden: true, Runs: []DelegationRunDTO{}}
	}
	if a.st == nil {
		return out
	}
	runs, err := a.st.ListDelegationRuns(root)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for _, r := range runs {
		out.Runs = append(out.Runs, a.delegationRunDTO(r))
	}
	return out
}

func (a *App) delegationRunDTO(r store.DelegationRun) DelegationRunDTO {
	dto := DelegationRunDTO{
		ID: r.ID, Slug: r.Slug, Name: r.Name, Status: r.Status,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		Tasks: []DelegationTaskDTO{},
	}
	dto.RedKind, dto.BaselineFaults = runRedKind(r)
	m, mErr := decodeSnapshot(r)
	if mErr != nil {
		dto.ManifestError = mErr.Error()
	}
	byID := map[string]delegate.Task{}
	for _, t := range m.Tasks {
		byID[t.ID] = t
	}
	rows, err := a.st.ListDelegationTasks(r.ID)
	if err != nil {
		dto.ManifestError = strings.TrimSpace(dto.ManifestError + " " + err.Error())
		return dto
	}
	for _, row := range rows {
		dto.Tasks = append(dto.Tasks, taskDTO(row, byID[row.TaskID]))
	}
	return dto
}

func taskDTO(row store.DelegationTask, t delegate.Task) DelegationTaskDTO {
	st := delegate.TaskState(row.State)
	d := DelegationTaskDTO{
		TaskID: row.TaskID, Title: t.Title, State: row.State,
		Repo: row.RepoLabel, Branch: row.Branch, Worktree: row.Worktree,
		SessionName: row.SessionName,
		CheckStatus: row.CheckStatus, CheckExit: row.CheckExit, CheckAt: row.CheckAt,
		Flags: flagList(row.Flags), Terminal: st.Terminal(), HoldsChild: st.HoldsAChild(),
		FlagDetails: flagDetails(flagList(row.Flags)),
		SeedOwed:    strings.TrimSpace(row.PendingSeed) != "",
		Needs:       []string{}, CheckArgv: []string{}, Blocked: row.BlockJSON,
		Divergence: divergenceDTO(delegate.DecodeDivergence(row.Divergence)),
		// Presence of the column, not a decode: DecodeSnapshot degrades a corrupt
		// value to an empty snapshot, so decoding here would report NoBaseline for
		// a task that HAS one and would hide the corruption behind a plausible
		// answer.
		SnapshotBaseline: strings.TrimSpace(row.SpawnSnapshot) != "",
	}
	if t.Needs != nil {
		d.Needs = t.Needs
	}
	if t.Check.Cmd != nil {
		d.CheckArgv = t.Check.Cmd
	}
	return d
}

// flagList decodes §13.2's flag set into a stable order. Sorted because the
// set is a map and an unsorted render would reshuffle the chips on every poll
// for no reason a user can see.
func flagList(encoded string) []string {
	out := []string{}
	for f, on := range delegate.DecodeFlags(encoded) {
		if on {
			out = append(out, string(f))
		}
	}
	sort.Strings(out)
	return out
}

// FlagDetailDTO is one §13.2 flag with the sentence behind it.
type FlagDetailDTO struct {
	Name string `json:"name"`
	Note string `json:"note"`
	// Loud marks the flags the spec requires be rendered loudly rather than as
	// one chip among several: the two that mean the isolation boundary may have
	// been crossed, and the one that means a child may be parked forever.
	Loud bool `json:"loud"`
}

// flagDetails attaches §12.2's and §12.3's wording to a flag set.
//
// An UNKNOWN flag is rendered with a note saying so rather than dropped:
// DecodeFlags round-trips unknown flags on purpose so the vocabulary can grow
// without a migration, and a badge this build cannot explain is still a badge a
// newer Loom wrote for a reason.
func flagDetails(flags []string) []FlagDetailDTO {
	out := make([]FlagDetailDTO, 0, len(flags))
	for _, f := range flags {
		d := FlagDetailDTO{Name: f}
		switch delegate.Flag(f) {
		case delegate.FlagStalled:
			d.Note = "no progress for 20m: the branch head has not moved and the transcript has " +
				"not advanced. A LABEL — nothing was killed, and a stalled child may be mid-thought"
		case delegate.FlagOrphaned:
			d.Note = "the child session is gone. The worktree and the branch are untouched; " +
				"re-spawn carries the context back by claude id"
		case delegate.FlagDiverged:
			d.Note = "committed files outside this task's declared paths. Non-blocking, and the " +
				"merge gate demands an explicit acknowledgement of the list"
		case delegate.FlagOutsideWrites:
			d.Note = "files outside the worktree changed since this task was spawned. It says " +
				"CHANGED SINCE SPAWN and not `the child wrote this`: the walk cannot tell the " +
				"human's own edits from a child's"
			d.Loud = true
		case delegate.FlagStaleContract:
			d.Note = "an interface artifact this task needs was re-fingerprinted after it was " +
				"spawned. Mergeability is withdrawn; this is the only cross-repo break Loom can " +
				"see without a cross-repo test, and it catches nothing else"
			d.Loud = true
		case delegate.FlagBlockMalformed:
			d.Note = "this child's block declaration will not parse. Read the raw text: a " +
				"swallowed block is a child parked forever with nobody told"
			d.Loud = true
		case delegate.FlagSeedPending:
			d.Note = "a seed is durably owed to this child and has not been delivered. Owed, not " +
				"failed — the retry is offered and the text is not lost"
		case delegate.FlagConflict:
			d.Note = "a producer's branch would not merge into this child's worktree. The task " +
				"stays parked and the seed describes the conflict"
		case delegate.FlagEnvSuspect:
			d.Note = "the check failed with an environment shape (port in use, DB locked). A " +
				"triage label on a FAILURE; it never turns one into a pass"
		case delegate.FlagForced:
			d.Note = "a human merged this past an unacknowledged divergence or a red gate. " +
				"Recorded for the record and never read as permission"
		case delegate.FlagFirstCheckGreen:
			d.Note = "§2's M2: this task's first check after the child stopped was green"
		case delegate.FlagFirstCheckRed:
			d.Note = "§2's M2: this task's first check after the child stopped was red"
		default:
			d.Note = "no description in this build — a flag written by a newer Loom"
		}
		out = append(out, d)
	}
	return out
}

// decodeSnapshot reads a run's manifest SNAPSHOT (workflow_runs.def_json's
// precedent: a run replays what it was created from even if the on-disk file
// was edited or deleted).
//
// The resolved-at-load fields are `json:"-"` and therefore absent from the
// snapshot, so they are re-derived from the store here. RepoPaths in
// particular: it is label→absolute path, it is what Spawn and the check runner
// need, and re-deriving it from the SAME launch-target set delegate.NewResolver
// is built over is what keeps this from becoming a second attribution
// derivation — which is how a hidden client leaks.
func decodeSnapshot(r store.DelegationRun) (delegate.Manifest, error) {
	var m delegate.Manifest
	if err := json.Unmarshal([]byte(r.ManifestJSON), &m); err != nil {
		return delegate.Manifest{}, fmt.Errorf("manifest snapshot unreadable: %v", err)
	}
	m.ProjectRoot = r.ProjectRoot
	return m, nil
}

func (a *App) repoPaths(root string) map[string]string {
	out := map[string]string{}
	for _, t := range a.targets() {
		if t.ProjectRoot == root {
			out[t.Label] = t.Path
		}
	}
	return out
}

// runAndTask is the common preamble of every task-scoped binding: load the run,
// APPLY THE GATE ON ITS PROJECT, decode the snapshot, find the task row and the
// task definition. The gate is here rather than in each caller so there is one
// filter site per call and not two — §3.1 rule 5, and the same reason §4.2
// gives for path matching.
func (a *App) runAndTask(runID int64, taskID string) (store.DelegationRun, delegate.Manifest, store.DelegationTask, delegate.Task, error) {
	var (
		run  store.DelegationRun
		m    delegate.Manifest
		row  store.DelegationTask
		task delegate.Task
	)
	if a.st == nil {
		return run, m, row, task, errors.New("delegation unavailable: no store")
	}
	run, ok, err := a.st.GetDelegationRun(runID)
	if err != nil {
		return run, m, row, task, err
	}
	if !ok {
		return run, m, row, task, fmt.Errorf("no such delegation run: %d", runID)
	}
	if !orchestrationVisible(a.resolver(), run.ProjectRoot) {
		return run, m, row, task, errHiddenProject
	}
	m, err = decodeSnapshot(run)
	if err != nil {
		return run, m, row, task, err
	}
	m.RepoPaths = a.repoPaths(run.ProjectRoot)
	row, ok, err = a.st.GetDelegationTask(runID, taskID)
	if err != nil {
		return run, m, row, task, err
	}
	if !ok {
		return run, m, row, task, fmt.Errorf("run %d has no task %q", runID, taskID)
	}
	for _, t := range m.Tasks {
		if t.ID == taskID {
			task = t
		}
	}
	if task.ID == "" {
		return run, m, row, task, fmt.Errorf("task %q is absent from run %d's manifest snapshot", taskID, runID)
	}
	return run, m, row, task, nil
}

// errHiddenProject never reaches the frontend as text — every binding turns it
// into the bare `hidden: true` marker. It exists so runAndTask has one return
// path and the callers cannot accidentally render the reason (which would name
// the project, in one bit).
var errHiddenProject = errors.New("hidden")

// --- slice 3a: the manifest "validate" affordance (§2) --------------------

// ManifestReportDTO is the GUI-side validate affordance §2 names.
type ManifestReportDTO struct {
	Hidden bool   `json:"hidden"`
	Dir    string `json:"dir"`
	// Manifests are the files that LOADED. Errors are the files that did not,
	// each with its path and reason — never a panic, and a bad file never costs
	// the user the other files (workflow.LoadAll's contract, for its reason).
	Manifests []ManifestSummaryDTO `json:"manifests"`
	Errors    []ManifestErrorDTO   `json:"errors"`
}

// ManifestSummaryDTO is one valid manifest. Warnings ride along with it rather
// than in a separate list: §4.4 rule 10's findings are legal, worth a glance,
// and belong beside the thing they are about.
type ManifestSummaryDTO struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Project  string   `json:"project"`
	Tasks    []string `json:"tasks"`
	Repos    []string `json:"repos"`
	Warnings []string `json:"warnings"`
}

type ManifestErrorDTO struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// ValidateManifests loads and validates <project>/.loom/manifests/*.json.
//
// It performs no writes and creates no run: looking must cost nothing, which is
// what makes it usable as the authoring loop's inner iteration. A malformed
// file — unparseable JSON, a dependency cycle, a repo from another project — is
// a reported LoadError, which is the whole point of the affordance.
func (a *App) ValidateManifests(root string) ManifestReportDTO {
	out := ManifestReportDTO{Manifests: []ManifestSummaryDTO{}, Errors: []ManifestErrorDTO{}}
	if !orchestrationVisible(a.resolver(), root) {
		// The dir is a path under the client's workspace; even that does not
		// cross.
		return ManifestReportDTO{Hidden: true, Manifests: []ManifestSummaryDTO{}, Errors: []ManifestErrorDTO{}}
	}
	out.Dir = delegate.ManifestDir(root)
	ms, errs := delegate.LoadAll(out.Dir, delegate.NewResolver(a.targets()))
	for _, m := range ms {
		s := ManifestSummaryDTO{
			Name: m.Name, Path: m.Path, Project: m.Project,
			Tasks: []string{}, Repos: []string{}, Warnings: []string{},
		}
		for _, t := range m.Tasks {
			s.Tasks = append(s.Tasks, t.ID)
		}
		for label := range m.Repos {
			s.Repos = append(s.Repos, label)
		}
		sort.Strings(s.Repos)
		for _, w := range m.Warnings {
			if w.Task != "" {
				s.Warnings = append(s.Warnings, w.Task+": "+w.Text)
				continue
			}
			s.Warnings = append(s.Warnings, w.Text)
		}
		out.Manifests = append(out.Manifests, s)
	}
	for _, e := range errs {
		out.Errors = append(out.Errors, ManifestErrorDTO{Path: e.Path, Error: e.Err})
	}
	return out
}

// --- slice 3a: approve-to-spawn (§5.1) ------------------------------------

// SpawnResultDTO is the outcome of the approve gate.
type SpawnResultDTO struct {
	Hidden      bool   `json:"hidden"`
	SessionName string `json:"sessionName"`
	Error       string `json:"error"`
}

// ApproveTask is §5.1's gate: the human presses it and Loom claims the task and
// launches the child. It is the ONLY path from `ready` to a running child —
// Loom never launches a child on its own.
//
// Hidden projects are refused, per delegation §14's table: a spawn is
// unambiguously NEW Loom-initiated work, and a new tmux window titled with the
// client's repo is exactly the leak §6.2b exists to prevent. (Note the contrast
// with SpawnOrchestrator above, which orchestrator §10 explicitly exempts:
// there the human is on the hidden project's own overview and asked for it.
// Here the approve action is greyed with a reason instead, and pressing it
// anyway must not become a second path around the same rule.)
//
// The singleton guarantee is the STORE's compare-and-swap, not a check here:
// the approve step is a CAS ready→approved (the same statement
// delegate.Spawner.Approve wraps — inlined only so the ordering below is this
// function's to state), and Spawn's first act is a CAS approved→spawning that
// records the worktree in the same statement. Pressing this twice, or two Loom
// instances pressing it once each, contend on one row and exactly one wins.
func (a *App) ApproveTask(runID int64, taskID string) SpawnResultDTO {
	run, m, _, task, err := a.runAndTask(runID, taskID)
	switch {
	case errors.Is(err, errHiddenProject):
		return SpawnResultDTO{Hidden: true}
	case err != nil:
		return SpawnResultDTO{Error: err.Error()}
	}
	// The CAS runs BEFORE the spawner is built, and the order is deliberate:
	// `approved` is a real state a task legitimately sits in (§6.6's cap-reached
	// tasks live there), so a launcher that turns out to be unavailable leaves
	// the human's decision recorded and the spawn retryable, rather than
	// discarding a gate they already passed.
	claimed, err := a.st.AdvanceTaskCAS(runID, taskID,
		string(delegate.StateReady), string(delegate.StateApproved), a.now().Unix())
	if err != nil {
		return SpawnResultDTO{Error: err.Error()}
	}
	if !claimed {
		// Not an error to swallow: the task was not `ready`, which is either a
		// stale screen or the other Loom instance winning the race, and both are
		// facts the human needs on the button they just pressed.
		return SpawnResultDTO{Error: fmt.Sprintf("task %q is not ready to approve", taskID)}
	}
	sp, err := a.spawner()
	if err != nil {
		return SpawnResultDTO{Error: err.Error()}
	}
	name, err := sp.Spawn(run, m, task)
	if err != nil {
		// Spawn returns the session name ALONGSIDE an error for §13.3's
		// disclosed residual hole (a concurrent abandon after a real launch), so
		// both are carried. Dropping the name would strand a live child with
		// nothing on screen naming it.
		return SpawnResultDTO{SessionName: name, Error: err.Error()}
	}
	return SpawnResultDTO{SessionName: name}
}

// spawner builds §5.1/§13.3's spawner from the App's own store and launcher.
//
// It is built per call rather than held as a field because the construction is
// cheap (three pointers) and holds no state — Worktrees' state is the
// filesystem and the store, so a cached Spawner would buy nothing and would
// have to be invalidated when a.loomDir is wired.
func (a *App) spawner() (*delegate.Spawner, error) {
	if a.launcher == nil {
		// Only the LAUNCH needs a launcher. The gate and the discard path do
		// not, and requiring one there would make "look at this task" fail on a
		// Loom that cannot start tmux — see worktrees().
		return nil, errors.New("delegation unavailable: no launcher")
	}
	w, err := a.worktrees()
	if err != nil {
		return nil, err
	}
	return &delegate.Spawner{
		Store: a.st, Launcher: a.launcher, Worktrees: w,
		Width: launchCols, Height: launchRows, Now: a.now,
	}, nil
}

// worktrees is the half of the spawner that needs no launcher: §5.1's preview
// and §6.3's discard both operate on the filesystem and the store only.
//
// Split out because folding them into spawner() made looking at a gate — and,
// worse, CLEANING UP a worktree — fail on a Loom with no launcher wired. A
// cleanup path that is unavailable exactly when something is already wrong is
// the shape that leaves a repo needing manual repair.
func (a *App) worktrees() (*delegate.Worktrees, error) {
	if a.st == nil {
		return nil, errors.New("delegation unavailable: no store")
	}
	// An unset ~/.loom degrades VISIBLY rather than rooting the layout at "."
	// and scattering worktrees through the process's cwd.
	home := a.loomDir
	if home == "" {
		return nil, errors.New("delegation unavailable: ~/.loom is unknown")
	}
	return &delegate.Worktrees{
		Layout: delegate.NewLayout(home),
		Store:  a.st,
		// 3a runs at 3 (§6.6). The cap lives on Worktrees, which is what
		// enforces it inside Create — one knob that the enforcing code reads
		// beats two knobs that can disagree.
		Cap: delegate.Concurrency3a,
	}, nil
}

// --- slice 3a: the check (§8) ---------------------------------------------

// CheckResultDTO is one check execution, reported.
type CheckResultDTO struct {
	Hidden bool   `json:"hidden"`
	TaskID string `json:"taskId"`
	Status string `json:"status"` // pass|fail|unpublished|infra-error
	Exit   int    `json:"exit"`
	Output string `json:"output"`
	RanAt  int64  `json:"ranAt"`
	// State is the task's state AFTER the result was recorded, so the caller
	// does not have to re-derive the mapping from status to state — a second
	// derivation is a second place to get `unpublished` wrong.
	State string `json:"state"`
	// EnvSuspect is §6.4's triage label: the output matched one of a small set
	// of environment-failure shapes. It NEVER turns a failure into a pass; it
	// lets a human triaging ten red checks tell "your code is wrong" from "your
	// neighbour took port 3000" in one glance. A heuristic, not a diagnosis.
	EnvSuspect bool `json:"envSuspect"`
	// Unpublished names the artifact paths that were missing or uncommitted
	// (§8.3). The paths, not a count: the remedy is "commit this file".
	Unpublished []string `json:"unpublished"`
	// Divergence is §12.3.1-2, recomputed on this run and persisted. Carried on
	// the check result because a human reading a check is the human who should
	// learn the child wrote outside what the manifest declared.
	Divergence DivergenceDTO `json:"divergence"`
	// DivergenceError is a failure to COMPUTE the divergence, kept separate
	// from Error: the check result above is still valid and must still render.
	// Said out loud rather than degraded to an empty list, because an empty
	// list is the answer a human acts on.
	DivergenceError string `json:"divergenceError"`
	Error           string `json:"error"`
}

// DivergenceDTO is §12.3.1-2's report: what a task touched that its manifest
// did not say it would.
//
// A DETECTOR, never the isolation mechanism — the worktree is what keeps
// children apart, and this says what went wrong afterwards. The distinction is
// in the DTO comment because the frontend is where it would most easily be
// rendered as a permission system.
type DivergenceDTO struct {
	// Outside is every touched file matching none of the task's own declared
	// `paths`. Non-blocking in 3a: §5.2's second acknowledgement arrives with
	// §10's merge gate, which is deferred.
	Outside []string `json:"outside"`
	// Siblings maps another task id in the SAME repo to the touched files that
	// fall inside its declared paths. Stronger than Outside — it predicts the
	// merge conflict before integration reaches it.
	Siblings map[string][]string `json:"siblings"`
	// Empty is carried rather than derived in the frontend so "no divergence"
	// and "divergence was never computed" cannot render the same.
	Empty bool `json:"empty"`
}

func divergenceDTO(d gitdiff.Divergence) DivergenceDTO {
	out := DivergenceDTO{Outside: []string{}, Siblings: map[string][]string{}, Empty: d.Empty()}
	if d.Outside != nil {
		out.Outside = d.Outside
	}
	for id, files := range d.Siblings {
		out.Siblings[id] = files
	}
	return out
}

// recordDivergence computes §12.3.1-2 for one task, persists it, and updates
// the `diverged` flag on the caller's flag set (which the caller writes — one
// SetTaskFlags per call, so env-suspect and diverged cannot clobber each other
// through two read-modify-writes of the same column).
//
// The flag is CLEARED when the divergence is empty. A child that wrote outside
// its paths and then moved the file back has not diverged, and a flag that only
// ever goes on is a flag a human stops reading. It is deliberately NOT cleared
// when the computation fails: "we could not tell" must not read as "it is fine".
func (a *App) recordDivergence(runID int64, taskID string, row store.DelegationTask,
	m delegate.Manifest, task delegate.Task, flags *delegate.Flags) (DivergenceDTO, string) {

	d, err := delegate.TaskDivergence(row.Worktree, row.BaseSHA, m, task)
	if err != nil {
		// The previously recorded value is left in place. Overwriting a real
		// divergence with an empty one because git failed once is exactly the
		// silent downgrade this package refuses everywhere else.
		return divergenceDTO(delegate.DecodeDivergence(row.Divergence)), err.Error()
	}
	if d.Empty() {
		*flags = flags.Without(delegate.FlagDiverged)
	} else {
		*flags = flags.With(delegate.FlagDiverged)
	}
	if err := a.st.SetTaskDivergence(runID, taskID, delegate.EncodeDivergence(d), a.now().Unix()); err != nil {
		return divergenceDTO(d), err.Error()
	}
	return divergenceDTO(d), ""
}

// checkableStates is the legal source set for a check claim, ENUMERATED rather
// than derived from a predicate — the same reason delegate.Ready and
// delegate.ActiveChildren enumerate. A state added to §13.2 must not silently
// become checkable, and the compiler will not say so; a reader of this list
// will.
//
//   - running / blocked: the ordinary case, and the one §8.2 automates.
//   - verified / failed: a re-check is a human's right. The check is the only
//     evidence in the system, and refusing to re-run it after a red result
//     would make a flake permanent.
//
// Everything else is excluded, and each exclusion is a defect a probe found or
// would have:
//
//   - merged / abandoned are TERMINAL (§13.2 gives neither an outgoing edge).
//     A merged task keeps its worktree column — §6.3 removes the directory, not
//     the column — so a green re-check rewrote `merged` to `verified` and the
//     run offered a merge gate for work that had already landed.
//   - checking is a check ALREADY IN FLIGHT. §8.2's "at most one check in
//     flight per task" is BINDING, and it is the only thing standing between
//     two Loom instances and the same agent-authored argv running twice against
//     one worktree.
//   - pending / ready / approved / spawning have no child work to be a
//     statement about. The worktree guard above already turns most of these
//     away; this makes it structural rather than incidental.
var checkableStates = []string{
	string(delegate.StateRunning),
	string(delegate.StateBlocked),
	string(delegate.StateVerified),
	string(delegate.StateFailed),
}

// RunTaskCheck runs a task's check and reports the result (§8.2's "a manual
// 'run check' action is always available").
//
// The check runs OUT OF BAND, as a subprocess in the child's worktree — never
// inside the child's session, never as a slash command, never as anything the
// child could influence the reported result of. §8 exists because a child's
// self-reported "done" was wrong in ~22.6% of validated misalignment episodes.
//
// Hidden projects get the bare marker and no execution. §14's table says checks
// KEEP RUNNING while hidden and that it is the output, not the execution, that
// would leak — but that row is about the run's own background clock, which is
// not this. This is a human pressing a button on a screen, and the thing it
// returns is the output. There is no background check loop in 3a for it to
// stall.
func (a *App) RunTaskCheck(runID int64, taskID string) CheckResultDTO {
	_, m, row, task, err := a.runAndTask(runID, taskID)
	switch {
	case errors.Is(err, errHiddenProject):
		return CheckResultDTO{Hidden: true}
	case err != nil:
		return CheckResultDTO{TaskID: taskID, Error: err.Error(), Unpublished: []string{}}
	}
	if row.Worktree == "" {
		return CheckResultDTO{TaskID: taskID, State: row.State, Unpublished: []string{},
			Error: fmt.Sprintf("task %q has no worktree yet — nothing to check", taskID)}
	}

	// Claim the task for the duration, from an EXPLICIT LEGAL SOURCE SET.
	//
	// Revision 1 CASed from `row.State` — whatever had just been read — and
	// that is not a guard at all: it succeeds against every state including the
	// ones it must refuse. Two probes found it. `merged` and `abandoned` are
	// terminal (§13.2 gives neither an outgoing edge), and a merged task keeps
	// its worktree column (§6.3 removes only the directory), so a re-check
	// rewrote `merged` to `verified` and the run then offered a merge gate for
	// work that had already landed. And an instance that read `checking` CASed
	// checking→checking, which SQLite counts as an affected row, so §8.2's
	// BINDING "at most one check in flight per task" admitted a second one —
	// two Looms running the same agent-authored argv against the same worktree,
	// the loser's result silently overwritten.
	claimed, from, err := a.st.AdvanceTaskFromAnyCAS(runID, taskID, checkableStates, string(delegate.StateChecking), a.now().Unix())
	if err != nil {
		return CheckResultDTO{TaskID: taskID, State: row.State, Unpublished: []string{}, Error: err.Error()}
	}
	if !claimed {
		return CheckResultDTO{TaskID: taskID, State: row.State, Unpublished: []string{},
			Error: fmt.Sprintf("task %q is %s — no check is available from that state "+
				"(it is already in flight, or the task is finished)", taskID, row.State)}
	}
	// `from` is the state the CAS ACTUALLY matched, not the one that was read a
	// moment earlier. It is what `unpublished` and `infra-error` restore the
	// task to below, and restoring to a stale read would undo whatever moved
	// the row in between.

	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	c := &delegate.Checker{}
	res := c.Run(ctx, delegate.CheckRequest{
		RunID: runID, TaskID: taskID, Worktree: row.Worktree,
		Check: task.Check, Artifacts: task.Produces, RepoDirs: m.RepoPaths,
	})

	// §13.2 has no `unpublished` or `infra-error` STATE, and inventing one would
	// widen the state machine for two conditions that say the same thing: the
	// check made no statement about done. Those return the task to the state it
	// was claimed from, with the result recorded — the row still shows what
	// happened, and the task is still exactly as done as it was.
	to := from
	switch res.Status {
	case delegate.CheckPass:
		to = string(delegate.StateVerified)
	case delegate.CheckFail:
		to = string(delegate.StateFailed)
	}
	// §8.2's debounce compares the recorded head against the worktree's current
	// one, so what is recorded must be the sha the check ACTUALLY ran against —
	// which only Checker.Run, holding the worktree at execution time, can know.
	// Re-deriving it here would race a commit landing during the check.
	//
	// Run leaves it "" when the head is unreadable (a worktree with no commits
	// yet). The previously recorded head is carried forward in that case rather
	// than overwritten: "" reads to the debounce as "never checked", which would
	// re-run this check on every tick forever.
	head := res.BranchHead
	if head == "" {
		head = row.BranchHead
	}
	if _, err := a.st.RecordTaskCheckCAS(runID, taskID, string(delegate.StateChecking), to,
		string(res.Status), int64(res.Exit), res.Output, head, a.now().Unix()); err != nil {
		return CheckResultDTO{TaskID: taskID, State: string(delegate.StateChecking),
			Status: string(res.Status), Exit: res.Exit, Output: res.Output,
			Unpublished: res.Unpublished, Error: err.Error()}
	}
	// §12.3: "divergence is computed on EVERY CHECK RUN and again immediately
	// before every merge". Both halves are wired — this one and
	// TaskMergeCommand's — because a divergence discovered after a merge is a
	// fact rather than a gate, and because the flag is what the merge gate's
	// second acknowledgement (§5.2, deferred with §10) will read.
	//
	// Computed AFTER the check and outside its result: divergence is not a
	// verdict, must never turn a green check red, and a failure to compute it
	// must not cost the check result that was just recorded. It is reported on
	// its own field instead.
	flags := delegate.DecodeFlags(row.Flags)
	if res.EnvSuspect {
		// A flag, never a status (§6.4): making it a status would let a caller
		// switch on it and forget that the check FAILED.
		flags = flags.With(delegate.FlagEnvSuspect)
	}
	div, divErr := a.recordDivergence(runID, taskID, row, m, task, &flags)
	_ = a.st.SetTaskFlags(runID, taskID, delegate.EncodeFlags(flags), a.now().Unix())

	// §9.1's second condition — "the artifact exists at its declared path,
	// committed, on the producer's branch" — is exactly what §8.3 just
	// verified, and this is the only moment in the system that holds the
	// answer. Recorded whenever publication PASSED, including behind a red
	// check: the artifact list is a fact about the tree, the check is a fact
	// about the code, and conflating them would leave every consumer of a
	// failing task's artifact unready forever with nothing on screen saying why.
	//
	// Ready still requires the producer to be `verified`, so recording a
	// publication under a red check cannot unblock anything on its own.
	if res.Published {
		a.recordArtifacts(runID, taskID, task, res)
	}
	// The ready set moves when a check lands. Re-evaluated here rather than only
	// on a poll so a green check makes its consumers approvable in the same
	// gesture — and through the RUNNER, so this hand-pressed check and the poll
	// loop cannot disagree about what became ready.
	//
	// A tick failure is not folded into the check result: the check ran, its
	// output is what the human asked for, and losing it because the scheduler
	// could not run afterwards is the worse outcome. It surfaces on the next
	// poll's own report, which is where a stopped scheduler belongs.
	_, _ = a.refreshReady(runID)

	out := CheckResultDTO{
		TaskID: taskID, Status: string(res.Status), Exit: res.Exit, Output: res.Output,
		RanAt: res.RanAt.Unix(), State: to, EnvSuspect: res.EnvSuspect,
		Unpublished: res.Unpublished, Divergence: div, DivergenceError: divErr,
	}
	if out.Unpublished == nil {
		out.Unpublished = []string{}
	}
	return out
}

// recordArtifacts writes the task's declared artifacts to
// `delegation_artifacts` at the commit §8.3 verified them on.
//
// The commit is the check's BranchHead — the sha Checker.Run captured while it
// held the worktree — and not a fresh `git rev-parse` here, which would race a
// commit landing during the check and record a publication against a tree
// nothing ever verified. An unreadable head leaves the column empty rather than
// guessing; the id and path are still the load-bearing part, and §10.5's
// stale-contract alarm (deferred) is the only consumer of the sha.
//
// A write failure is not surfaced as a check error: the check result is the
// thing the human asked for, and losing it because a supplementary table was
// unwritable is the worse outcome. It is not silent either — the artifact
// simply does not appear, which §9.1 reads as "not published" and renders as a
// consumer that stays pending.
func (a *App) recordArtifacts(runID int64, taskID string, task delegate.Task, res delegate.Result) {
	now := a.now().Unix()
	for _, art := range task.Produces {
		_ = a.st.UpsertDelegationArtifact(store.DelegationArtifact{
			RunID: runID, ArtifactID: art.ID, TaskID: taskID, Path: art.Path,
			Fingerprint: res.Fingerprints[art.ID], CommitSHA: res.BranchHead, PublishedAt: now,
		})
	}
}

// --- slice 3a: the merge gate is a human running git (§2, §10.4) ----------

// MergeCommandDTO is the command a human runs. There is no execute path.
type MergeCommandDTO struct {
	Hidden bool `json:"hidden"`
	// Argv is the command as an argv array, and Display is its single-line
	// rendering for a copy button. Two fields because the argv is the truth and
	// the string is the affordance; deriving one from the other in the frontend
	// is how a quoting bug becomes a command a human pastes into a shell.
	Argv    []string `json:"argv"`
	Display string   `json:"display"`
	Repo    string   `json:"repo"`
	Branch  string   `json:"branch"`
	// CheckStatus is the recorded result the human is deciding against. Not a
	// gate here: 3a has no test-gated integration (§10 is deferred), so this is
	// evidence, and calling it a certificate would be a lie.
	CheckStatus string `json:"checkStatus"`
	// RepoDirty is §10.3's refusal, surfaced as a warning because Loom is not
	// the one running the merge: merging into a dirty tree is how a human loses
	// work to a machine, and the human is the machine here.
	RepoDirty bool     `json:"repoDirty"`
	Warnings  []string `json:"warnings"`
	// Divergence is §12.3's "again immediately before every merge", recomputed
	// here rather than read from the row. Before, not after: a divergence
	// discovered after a merge is a fact, not a gate — and the human at this
	// gate is the one running the merge, so this is their last chance to see
	// that the child wrote outside what the manifest declared.
	//
	// It does not BLOCK in 3a. §5.2's second explicit acknowledgement arrives
	// with §10's merge gate, which is deferred; Loom does not run this merge, so
	// the only thing it can do is say so, loudly, in Warnings.
	Divergence      DivergenceDTO `json:"divergence"`
	DivergenceError string        `json:"divergenceError"`
	Error           string        `json:"error"`
}

// TaskMergeCommand returns the merge command as TEXT and never executes it.
//
// It WAS 3a's whole merge gate (§2: "Loom prints the command and does not
// execute it"). §10 is now built — MergeTask below runs the merge — and this is
// kept beside it as the copy-me affordance for the human who wants to do it by
// hand: a Loom with no ~/.loom, a repo mid-rebase, a merge someone wants to
// watch. It holds no state, so keeping it costs nothing.
//
// What it must never become is a second merge path with different
// preconditions. Everything §5.2 requires — the recomputed divergence, the
// acknowledgement compared against what is on screen, the clean-tree refusal,
// the `forced` record — lives on MergeTask; this function only ever produces a
// string, and its warnings say what the executed gate would have refused.
//
// What is merged is the TASK'S OWN BRANCH (§10.4) and nothing else. The
// integration branch is cumulative, so merging it would land every sibling that
// verified first — the human approves diff(B) and gets diff(A)+diff(B), with
// A's own gate never shown. That is the exact shape §5.2 exists to forbid, and
// it is worth stating on a function that only builds a string, because the
// string is what a human will run.
func (a *App) TaskMergeCommand(runID int64, taskID string) MergeCommandDTO {
	run, m, row, task, err := a.runAndTask(runID, taskID)
	switch {
	case errors.Is(err, errHiddenProject):
		return MergeCommandDTO{Hidden: true}
	case err != nil:
		return MergeCommandDTO{Argv: []string{}, Warnings: []string{}, Error: err.Error()}
	}
	repo := m.RepoPaths[row.RepoLabel]
	branch := row.Branch
	if branch == "" {
		branch = delegate.BranchName(run.Slug, taskID)
	}
	out := MergeCommandDTO{
		Repo: repo, Branch: branch, CheckStatus: row.CheckStatus,
		Warnings: []string{}, Argv: []string{},
	}
	if repo == "" {
		out.Error = fmt.Sprintf("repo %q is not a known repo of this project", row.RepoLabel)
		return out
	}
	out.Argv = []string{"git", "-C", repo, "merge", "--no-ff", "-m",
		fmt.Sprintf("loom: merge %s/%s", run.Slug, taskID), branch}
	out.Display = strings.Join(out.Argv, " ")
	if row.CheckStatus != string(delegate.CheckPass) {
		// Rendered, never hidden and never blocking: a red merge you can explain
		// is fine, a red merge nobody was told about is not. Loom is not
		// executing this, so the only thing it can do is say so.
		out.Warnings = append(out.Warnings,
			fmt.Sprintf("check is %q — this task has not been certified done", displayStatus(row.CheckStatus)))
	}
	if delegate.Dirty(repo) {
		out.RepoDirty = true
		out.Warnings = append(out.Warnings,
			"the repo's work tree is dirty — merging into a dirty tree is how work is lost")
	}

	// §12.3's second computation moment. Recomputed and PERSISTED, not read:
	// the child may have committed since the last check, and the file list a
	// human is about to merge on the strength of has to be about the tree that
	// is actually going to land.
	flags := delegate.DecodeFlags(row.Flags)
	div, divErr := a.recordDivergence(runID, taskID, row, m, task, &flags)
	_ = a.st.SetTaskFlags(runID, taskID, delegate.EncodeFlags(flags), a.now().Unix())
	out.Divergence, out.DivergenceError = div, divErr
	switch {
	case divErr != "":
		out.Warnings = append(out.Warnings,
			"divergence could not be computed ("+divErr+") — this merge is unreviewed for scope")
	case !div.Empty:
		// Two sentences, because the two comparisons mean different things and
		// collapsing them would hide the one that predicts a conflict.
		if n := len(div.Outside); n > 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"%d file(s) outside this task's declared paths: %s", n, strings.Join(div.Outside, ", ")))
		}
		for _, id := range sortedKeys(div.Siblings) {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"touches files declared by sibling task %q (predicts a merge conflict): %s",
				id, strings.Join(div.Siblings[id], ", ")))
		}
	}
	return out
}

// sortedKeys keeps the warning order a function of the SET and nothing else.
// Go randomizes map iteration per run, so an unsorted render would reshuffle a
// human's warnings on every poll for no reason they can see.
func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func displayStatus(s string) string {
	if s == "" {
		return "never run"
	}
	return s
}

// --- per-session memory (orchestration-view §10, §11 stage 4b) --------------

// SessionMemoryDTO is one child's own account of its work, as the memory
// indexer already distilled it: what it was asked, what it reported, and which
// files it touched.
//
// It is NOT evidence of completion, and the frontend renders it under the check
// result rather than beside it for that reason. §2.1 is the whole arc's spine:
// a child's self-report is a child's self-report, and the only thing that ever
// promotes a node to done is a recorded check result. This block exists so the
// human reading a failed check can see what the child THOUGHT it did.
type SessionMemoryDTO struct {
	// Hidden is the §3.1 marker, and as everywhere else it is the only field
	// set when it is true.
	Hidden bool `json:"hidden"`

	Ask     string `json:"ask"`
	Outcome string `json:"outcome"`
	// Files is the indexer's touched-file list as a single pre-joined string,
	// which is how store.Transcript holds it. Splitting it here would be a
	// second parser for a format internal/memory already owns.
	Files string `json:"files"`
	// Summary is the on-demand LLM distillation, present only when one has
	// already been generated. This method never generates one: it is called
	// from an inspector open, and a binding that could spend an API call on a
	// click is a binding that will do it on a poll after the next refactor.
	Summary string `json:"summary"`

	// Missing says the transcript file the row was built from is gone. Said out
	// loud rather than rendered as an empty block, because "the child said
	// nothing" and "the record was deleted" are different facts.
	Missing bool `json:"missing"`
}

// SessionMemory returns a child session's distilled ask/outcome/files for the
// node inspector body (§10: "internal/memory — per-session ask/outcome/files
// for the node inspector body. No new extraction.").
//
// No new extraction is meant literally: this reads the `transcripts` row the
// indexer already wrote and computes nothing. Before it existed the inspector
// fell back to the finished-session summary carried in ListRecent, which is
// only present for sessions that have ENDED and been summarized — so a running
// child, which is the one a human is most likely to be inspecting, showed
// nothing at all.
//
// Resolution goes through the CLAUDE session id, not the tmux name, for the
// same reason nodeSessionStatus does (arch.go): a resumed child mints a new
// tmux name (ARCHITECTURE §4.1), and keying on the name alone blanks the block
// the moment a step is resumed. GetLatestByClaudeSessionID then follows the
// resume chain to the newest row before the transcript lookup.
//
// The gate is the ATTRIBUTOR's, not the bare resolver's (delegation §14.1): a
// delegation child's cwd is a worktree under ~/.loom that matches no project
// target, so res.Visible would fail closed on every child and this block would
// be permanently empty the moment anything was hidden.
func (a *App) SessionMemory(sessionName string) SessionMemoryDTO {
	if a.st == nil || sessionName == "" {
		return SessionMemoryDTO{}
	}
	row, ok, err := a.st.Get(sessionName)
	if err != nil || !ok {
		return SessionMemoryDTO{}
	}
	if !a.attributor(a.resolver()).Visible(row) {
		return SessionMemoryDTO{Hidden: true}
	}
	if row.ClaudeSessionID == "" {
		return SessionMemoryDTO{}
	}
	if latest, ok, err := a.st.GetLatestByClaudeSessionID(row.ClaudeSessionID); err == nil && ok {
		// Re-gate on the row we actually read from. The resume chain can cross
		// into a different cwd — a child restarted against another repo — and
		// the row that came back is the one whose text is about to be
		// rendered, so it is the row the predicate has to answer for.
		if !a.attributor(a.resolver()).Visible(latest) {
			return SessionMemoryDTO{Hidden: true}
		}
	}
	t, ok, err := a.st.GetTranscript(row.ClaudeSessionID)
	if err != nil || !ok {
		// Not an error: a session that has produced no transcript yet is the
		// normal state of a child in its first seconds. The frontend renders
		// the empty DTO as "nothing recorded yet".
		return SessionMemoryDTO{}
	}
	return SessionMemoryDTO{
		Ask:     t.Ask,
		Outcome: t.Outcome,
		Files:   t.Files,
		Summary: t.LLMSummary,
		Missing: t.FileMissing,
	}
}

// --- slice 3a: creating a run, and the scheduler (§9.1) -------------------

// StartRunDTO is the outcome of creating a run from a validated manifest.
type StartRunDTO struct {
	Hidden bool   `json:"hidden"`
	RunID  int64  `json:"runId"`
	Slug   string `json:"slug"`
	// Ready names the tasks that are immediately approvable — the ones with no
	// `needs`. Returned so the caller does not have to poll to discover that a
	// brand-new run already has work to gate.
	Ready []string `json:"ready"`
	// Bases is repo label → pinned commit, echoed because it is the one
	// decision a run cannot revisit (§6.2 step 1) and a human should be able to
	// see what their children will branch from.
	Bases map[string]string `json:"bases"`
	Error string            `json:"error"`
}

// StartDelegationRun creates a run from one of the project's manifests.
//
// Before this existed, nothing in production wrote a `delegation_runs` row:
// every piece of 3a was reachable only from a hand-seeded database, which means
// §2's "slice 3a is built AND RUN on one real initiative" could not happen and
// the kill criterion could not be measured. That is the gap this closes; it
// adds no mechanism of its own.
//
// The manifest is re-validated here rather than trusted from a previous
// ValidateManifests call. Looking and acting are separate moments, and the file
// is agent-authored — the one that was green on screen is not necessarily the
// one on disk now.
func (a *App) StartDelegationRun(root, manifestName string) StartRunDTO {
	out := StartRunDTO{Ready: []string{}, Bases: map[string]string{}}
	if !orchestrationVisible(a.resolver(), root) {
		// A run creates a worktree and a tmux window named after the project's
		// repo. §14's table refuses new Loom-initiated work on a hidden project
		// for exactly that reason, and this is the earliest point to refuse it.
		return StartRunDTO{Hidden: true, Ready: []string{}, Bases: map[string]string{}}
	}
	if a.st == nil {
		out.Error = "delegation unavailable: no store"
		return out
	}

	ms, loadErrs := delegate.LoadAll(delegate.ManifestDir(root), delegate.NewResolver(a.targets()))
	var m delegate.Manifest
	for _, cand := range ms {
		if cand.Name == manifestName {
			m = cand
			break
		}
	}
	if m.Name == "" {
		// Name the load error for this manifest if there was one. "no such
		// manifest" when the truth is "it has a dependency cycle" sends a human
		// looking for a missing file.
		for _, e := range loadErrs {
			if strings.TrimSuffix(filepath.Base(e.Path), ".json") == manifestName {
				out.Error = fmt.Sprintf("manifest %q did not load: %s", manifestName, e.Err)
				return out
			}
		}
		out.Error = fmt.Sprintf("no valid manifest named %q under %s", manifestName, delegate.ManifestDir(root))
		return out
	}
	// §3 containment, re-asserted at the ACT rather than inherited from the
	// load: the manifest names its project by display name, and a manifest
	// sitting under one project that names another must not create a run
	// attributed to the project whose directory it happens to live in.
	if m.ProjectRoot != root {
		out.Error = fmt.Sprintf("manifest %q belongs to project %q, not this one", m.Name, m.Project)
		return out
	}

	// ONE creation path. This body used to duplicate Runner.Create — pin,
	// marshal, insert, insert tasks, CAS to `running`, ensure the §10.1
	// worktrees — and the duplicate had already diverged twice: the GUI copy
	// carried an `integration` splice the Runner's did not, and the Runner's
	// carried an Ensure the GUI's had to grow separately. Two creation paths
	// mean the run a human starts and the run a test creates are different
	// runs, which is the shape of bug that survives every test.
	//
	// BEHAVIOUR CHANGE, deliberate: Create needs a Runner, and a Runner needs
	// ~/.loom. A Loom with no loom dir now REFUSES to create the run instead of
	// creating one and reporting that its worktrees are missing. That is the
	// honest outcome — every task of such a run would refuse at spawn, since a
	// child's worktree lives under ~/.loom too — and it fails at the gesture
	// rather than three clicks later.
	r, err := a.delegationRunner()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	// PinBases twice is a wasted `git rev-parse` per repo and is not worth a
	// second return value on Create: the DTO echoes the pinned bases (§6.2
	// step 1 is the one decision a run cannot revisit) and the run row stores
	// them as JSON. Decoding the row back is the version that cannot disagree
	// with what was stored — a second Pin could, if a repo moved between the
	// two calls, and would then show the human a base no child will use.
	run, err := r.Create(m)
	if run.ID == 0 {
		// Nothing was written. Create refuses before the insert for a hidden
		// project, an unpinnable repo or a manifest with no project.
		out.Error = errText(err, "the run was not created")
		return out
	}
	out.RunID, out.Slug = run.ID, run.Slug
	out.Bases = decodeBases(run.BaseSHAs)
	if err != nil {
		// Created WITH FAULTS: §10.1's worktrees, or the `planning`→`running`
		// CAS. Reported beside a usable run and never rolled back — deleting
		// the row would also delete the record of why.
		out.Error = err.Error()
	}

	ready, tickErr := a.refreshReady(run.ID)
	out.Ready = ready
	if tickErr != nil {
		// The run EXISTS and its tasks are written; what failed is the first
		// scheduler pass. Reported on the same field as the `planning` fault
		// above and for the same reason: a run whose scheduler never ran will
		// show no ready tasks, and "nothing is ready yet" is indistinguishable
		// from "the scheduler is not running" unless it says so.
		out.Error = strings.TrimSpace(out.Error + " " + tickErr.Error())
	}
	return out
}

// errText renders an error for a DTO, falling back to a sentence when the
// error is nil. A binding that returns a failure with an empty `error` string
// renders as a silent no-op, which is the one failure mode the house rule
// against invisible degradation exists to forbid.
func errText(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

// decodeBases reads `delegation_runs.base_shas` back into repo label → commit.
//
// Degrades to an EMPTY map, never nil: the DTO's `bases` is rendered as a list
// and a null there is a JS error at the call site, whereas an empty map renders
// as "no pinned bases" — which is exactly what an unreadable column means.
func decodeBases(baseJSON string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(baseJSON) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(baseJSON), &out); err != nil {
		return map[string]string{}
	}
	return out
}

// RefreshDelegationRun re-evaluates §9.1's ready set and returns the run.
//
// It exists as its own binding because Ready depends on facts that change
// outside Loom — a child commits its artifact, a human merges a branch — and a
// run whose ready set only advanced when a check happened to run would strand
// work with no way to unstick it but restarting.
func (a *App) RefreshDelegationRun(runID int64) DelegationDTO {
	// DelegationDTO and not DelegationRunDTO: the hidden marker lives on the
	// wrapper, and a per-run Hidden field would be a second spelling of §3.1
	// that every other run-shaped payload does not have.
	out := DelegationDTO{Runs: []DelegationRunDTO{}}
	if a.st == nil {
		out.Error = "delegation unavailable: no store"
		return out
	}
	run, ok, err := a.st.GetDelegationRun(runID)
	switch {
	case err != nil:
		out.Error = err.Error()
		return out
	case !ok:
		out.Error = fmt.Sprintf("no such delegation run: %d", runID)
		return out
	case !orchestrationVisible(a.resolver(), run.ProjectRoot):
		return DelegationDTO{Hidden: true, Runs: []DelegationRunDTO{}}
	}
	// The manifest is no longer decoded here to feed the scheduler: the runner
	// re-derives it from the snapshot and re-resolves the repo paths itself
	// (Runner.Repos), so a second decode would be a second place for the two to
	// disagree about which repos a run is in scope of.
	if _, tickErr := a.refreshReady(runID); tickErr != nil {
		out.Error = tickErr.Error()
	}
	// Re-read: the tick moved rows, and returning the run row that was read
	// before it would render one poll behind its own scheduler.
	if fresh, ok, err := a.st.GetDelegationRun(runID); err == nil && ok {
		run = fresh
	}
	out.Runs = append(out.Runs, a.delegationRunDTO(run))
	return out
}

// refreshReady drives the ONE scheduler — delegate.Runner.Tick — and then READS
// the rows it moved.
//
// It used to promote pending → ready itself, over `Ready(BuildGraph(m), …)`.
// That is the DECLARED graph, and it was a second scheduler with a defect the
// first one does not have: it cannot see an approved amendment's edge (§11.3),
// so a task the EFFECTIVE graph says is still waiting on `account-schema` was
// offered for spawn by the view, gate and all. Tick step 3b owns the promotion
// now, over the effective edge set, under the same per-task CAS.
//
// The read afterwards is deliberately a read of the STATE COLUMN and not of the
// report's Ready bucket. Progress.Ready is what the graph proposes; the column
// is what was actually written, and a CAS that lost a race to the other Loom
// instance must not be rendered as an offer this instance can honour.
//
// A Tick that cannot even be built degrades to the read alone: nothing is
// promoted, the run renders with no ready tasks, and the reason travels back to
// the caller rather than being swallowed — see tickRun.
func (a *App) refreshReady(runID int64) ([]string, error) {
	_, tickErr := a.tickRun(runID)
	rows, err := a.st.ListDelegationTasks(runID)
	if err != nil {
		return []string{}, errors.Join(tickErr, err)
	}
	out := []string{}
	for _, r := range rows {
		if delegate.TaskState(r.State) == delegate.StateReady {
			out = append(out, r.TaskID)
		}
	}
	sort.Strings(out)
	return out, tickErr
}

// tickRun runs one poll of §9's loop for a run.
//
// Errors are RETURNED rather than logged: a tick that cannot run is a run that
// stopped moving, and the two ways that happens (no ~/.loom, no store) are both
// permanent until a human does something. The per-task faults inside a tick are
// a different class and stay on the report — one task's git failure must not
// cost the other eleven their tick.
func (a *App) tickRun(runID int64) (delegate.TickReport, error) {
	r, err := a.delegationRunner()
	if err != nil {
		return delegate.TickReport{RunID: runID}, err
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return r.Tick(ctx, runID)
}

// delegationRunner builds §§9-12's runner once and keeps it.
//
// Repos is the field this whole seam exists for. Manifest.ProjectRoot and
// Manifest.RepoPaths are `json:"-"` — resolved by the loader from the machine's
// project table, deliberately never snapshotted, because a run that replayed a
// stale absolute path would be worse than one that has none. So a manifest read
// back out of manifest_json carries no repo paths at all, and every git call
// downstream of it (worktree add, base pin, merge) would get "". The GUI holds
// the projects resolver and is the only caller that can answer, which is exactly
// why the Runner takes a function instead of importing internal/projects.
//
// Hidden is §14's ONE bit and it is the §3.1 predicate, not projects.go's: an
// unattributable or unknown root reads as HIDDEN here, so a root Loom cannot
// place suppresses new spawns rather than authorizing them. It never routes a
// CHILD through the resolver — child visibility is the Attributor's (§14.1), and
// a raw resolver would fail closed on every worktree cwd and hide the whole run.
func (a *App) delegationRunner() (*delegate.Runner, error) {
	a.delegMu.Lock()
	defer a.delegMu.Unlock()
	if a.deleg != nil {
		return a.deleg, nil
	}
	w, err := a.worktrees()
	if err != nil {
		return nil, err
	}
	layout := delegate.NewLayout(a.loomDir)
	checker := &delegate.Checker{}
	detector := &delegate.Detector{Layout: layout, Store: a.st}
	r := &delegate.Runner{
		Store:     a.st,
		Layout:    layout,
		Checker:   checker,
		Worktrees: w,
		Detector:  detector,
		Integrator: &delegate.Integrator{
			Store: a.st, Layout: layout, Checker: checker,
			// Repos is left nil: Integrator falls back to Manifest.RepoPaths,
			// which the Runner re-resolves per run through Repos below. One
			// answer, from one place, per project — a map pinned here would be
			// whichever project happened to be open when the Runner was built.
			Worktrees: w, Blocks: detector, Now: a.now,
		},
		Rendezvous: &delegate.Rendezvous{
			Store: a.st, Layout: layout, Detector: detector,
			Width: launchCols, Height: launchRows, Now: a.now,
		},
		Watchdogs: &delegate.Watchdogs{Store: a.st, Worktrees: w, Layout: layout, Now: a.now},
		// The durable log lives in delegation_amendments and *store.Store
		// implements all three operations, so the nil interface is the ordinary
		// case: Runner.amendments falls through to the store. It is left nil
		// rather than wrapped so there is no second implementation to keep in
		// step with the CAS.
		Hidden: func(root string) bool { return !orchestrationVisible(a.resolver(), root) },
		Repos:  a.repoPaths,
		Now:    a.now,
	}
	// The two seeding seams are set only when they EXIST. A nil *tmux.Client
	// stored in the Sender interface is a non-nil interface holding nil, which
	// turns "no terminal wired" into a panic inside a poll tick instead of the
	// visible ErrSeedUndelivered the seed path is built to report.
	if a.tm != nil {
		r.Rendezvous.Tmux = a.tm
	}
	if a.launcher != nil {
		r.Rendezvous.Resumer = a.launcher
		r.Rendezvous.ClaudeConfigDir = a.launcher.ClaudeConfigDir
		// Only the LAUNCH needs a launcher, and a Loom that cannot start tmux
		// must still tick: checks, blocks, integration and the watchdogs are all
		// reachable without one. A nil Spawner refuses at Approve with a reason.
		r.Spawner = &delegate.Spawner{
			Store: a.st, Launcher: a.launcher, Worktrees: w,
			Width: launchCols, Height: launchRows, Now: a.now,
		}
	}
	a.deleg = r
	return r, nil
}

// --- slice 3a: §5.1's gate, rendered ---------------------------------------

// SpawnPreviewDTO is §5.1's BINDING list, and every field on it is on that
// list. It is the human's whole review: the child is arbitrary code with a
// model's judgement behind it, and there is no sandbox.
type SpawnPreviewDTO struct {
	Hidden   bool   `json:"hidden"`
	TaskID   string `json:"taskId"`
	Title    string `json:"title"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`
	Base     string `json:"base"`
	// Brief is the FULL assembled brief, byte-identical to the file the child
	// will read — Preview and Spawn assemble it from the same planned value for
	// exactly this reason. A summarized brief at the gate would mean the human
	// approved something the child never receives.
	Brief string `json:"brief"`
	// CheckArgv is verbatim. It is arbitrary code from an agent-authored file,
	// and the human's reading of it is the entire review.
	CheckArgv []string `json:"checkArgv"`
	Model     string   `json:"model"`
	Mode      string   `json:"mode"`
	// ModeRisky is bypassPermissions, which §5.1 renders IN RED. Legal,
	// flagged, never silent.
	ModeRisky bool `json:"modeRisky"`
	// SeedFiles are the gitignored files about to be copied into the worktree,
	// and SeedRefused the ones that will not be, with reasons. Both, because a
	// refused .env is a check that will fail for a reason unrelated to the
	// child's work and the human can only fix it BEFORE the spawn.
	SeedFiles   []string `json:"seedFiles"`
	SeedRefused []string `json:"seedRefused"`
	// RepoDirty is §6.2's disclosure: children branch from committed HEAD, so
	// uncommitted work in the user's own tree is invisible to every child.
	RepoDirty bool `json:"repoDirty"`
	// Running/Cap/CapReached are §6.6's counter. The action is GREYED at the
	// cap, never removed — hiding it would make a capped run look stalled.
	Running    int      `json:"running"`
	Cap        int      `json:"cap"`
	CapReached bool     `json:"capReached"`
	Warnings   []string `json:"warnings"`
	Error      string   `json:"error"`
}

// TaskSpawnPreview renders §5.1's gate for one task.
//
// It performs no writes and creates no worktree: the human may look and walk
// away, and looking must cost nothing. That is what makes it usable as the
// authoring loop's inner iteration, and it is why this is a separate binding
// from ApproveTask rather than a field on its result.
func (a *App) TaskSpawnPreview(runID int64, taskID string) SpawnPreviewDTO {
	empty := SpawnPreviewDTO{CheckArgv: []string{}, SeedFiles: []string{},
		SeedRefused: []string{}, Warnings: []string{}}
	run, m, _, task, err := a.runAndTask(runID, taskID)
	switch {
	case errors.Is(err, errHiddenProject):
		return SpawnPreviewDTO{Hidden: true}
	case err != nil:
		empty.TaskID, empty.Error = taskID, err.Error()
		return empty
	}
	w, err := a.worktrees()
	if err != nil {
		empty.TaskID, empty.Error = taskID, err.Error()
		return empty
	}
	// Preview needs no launcher: looking must cost nothing, including a working
	// tmux.
	sp := &delegate.Spawner{Store: a.st, Worktrees: w, Width: launchCols, Height: launchRows, Now: a.now}
	p, err := sp.Preview(run, m, task)
	if err != nil {
		empty.TaskID, empty.Error = taskID, err.Error()
		return empty
	}
	out := SpawnPreviewDTO{
		TaskID: p.TaskID, Title: p.Title, Repo: p.Repo, Branch: p.Branch,
		Worktree: p.Worktree, Base: p.Base, Brief: p.Brief,
		CheckArgv: p.CheckArgv, Model: p.Model, Mode: p.Mode, ModeRisky: p.ModeRisky,
		SeedFiles: p.SeedFiles, SeedRefused: []string{}, RepoDirty: p.RepoDirty,
		Running: p.Running, Cap: p.Cap, CapReached: p.CapReached, Warnings: p.Warnings,
	}
	for _, r := range p.SeedRefused {
		// Flattened to text here rather than shipped as a struct: the frontend
		// renders it as one line beside the copies, and a second shape would be
		// a second thing to keep in sync for no gain.
		out.SeedRefused = append(out.SeedRefused, r.File+" — "+r.Why)
	}
	if out.CheckArgv == nil {
		out.CheckArgv = []string{}
	}
	if out.SeedFiles == nil {
		out.SeedFiles = []string{}
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	return out
}

// --- slice 3a: §6.3's human discard ----------------------------------------

// DiscardResultDTO is the outcome of discarding a task's worktree.
type DiscardResultDTO struct {
	Hidden bool `json:"hidden"`
	// Removed says the directory is gone. The BRANCH is never gone — Loom does
	// not delete branches, ever (§6.3) — and neither is `<task-id>.meta/`,
	// which holds the only durable record of what the child was told.
	Removed bool   `json:"removed"`
	State   string `json:"state"`
	Error   string `json:"error"`
}

// DiscardTaskWorktree is §6.3's "discarded by the human" row: the worktree
// directory is removed, the branch is KEPT, and the task is abandoned.
//
// Before this existed, nothing in production called Worktrees.Remove: every
// worktree a run created stayed registered in the user's repo forever, and the
// only cleanup was the user learning `git worktree remove`. That is the
// "needs manual repair" class arriving by omission, and it is the reason this
// binding exists rather than a nicer one.
//
// force is the human saying so. Without it Remove refuses while a LIVE session
// occupies the directory (pulling a tree out from under a running claude yields
// a session that cannot write and cannot say why) and refuses uncommitted work.
// With it, the discard proceeds — and the branch still survives, which is what
// makes the act recoverable.
//
// The row is abandoned BEFORE the removal is attempted only when the removal
// succeeds; the order below is remove-then-abandon deliberately, because a task
// marked abandoned whose directory is still on disk is the state with no
// remaining action attached to it.
func (a *App) DiscardTaskWorktree(runID int64, taskID string, force bool) DiscardResultDTO {
	run, m, row, task, err := a.runAndTask(runID, taskID)
	switch {
	case errors.Is(err, errHiddenProject):
		return DiscardResultDTO{Hidden: true}
	case err != nil:
		return DiscardResultDTO{Error: err.Error()}
	}
	repo := m.RepoPaths[task.Repo]
	if repo == "" {
		return DiscardResultDTO{State: row.State,
			Error: fmt.Sprintf("repo %q is not a known repo of this project — its worktree list "+
				"cannot be pruned without it", task.Repo)}
	}
	w, err := a.worktrees()
	if err != nil {
		return DiscardResultDTO{State: row.State, Error: err.Error()}
	}
	if err := w.Remove(repo, run.Slug, task.Repo, taskID, force); err != nil {
		// Loud, and the row is untouched: the remedy (discard, which is force)
		// is the human's to choose, and a task marked abandoned whose worktree
		// is still there would have no action left to offer them.
		return DiscardResultDTO{State: row.State, Error: err.Error()}
	}
	out := DiscardResultDTO{Removed: true, State: row.State}
	claimed, err := a.st.AbandonTaskCAS(runID, taskID, a.now().Unix())
	switch {
	case err != nil:
		out.Error = err.Error()
	case claimed:
		out.State = string(delegate.StateAbandoned)
	default:
		// The worktree is gone and the row would not move. Said out loud: the
		// alternative is a task rendering as live with nothing on disk.
		out.Error = fmt.Sprintf("the worktree was removed but task %q could not be abandoned "+
			"(it is %s)", taskID, row.State)
	}
	return out
}

// --- §§9-12: the tick, its findings, and §11.3's offers ---------------------

// TickReportDTO is one poll of §9's loop, rendered.
//
// Every field on delegate.TickReport is here, and that is the point rather than
// completeness for its own sake: "a tick that did something invisible is a tick
// that will be debugged by reading source". A DTO that carried only the happy
// buckets would drop the suppressions, the watchdog findings and the per-task
// faults — which are precisely the three things a human looking at a run that is
// not moving needs.
type TickReportDTO struct {
	Hidden bool  `json:"hidden"`
	RunID  int64 `json:"runId"`
	At     int64 `json:"at"`

	// §9.3's buckets. Five and not four: `waiting` is a task whose producer has
	// not finished, and folding it into Unclassified — which the deadlock
	// detector renders as "Loom has a bug" — would make the loud bucket
	// non-empty on every healthy multi-task run.
	Ready        []string `json:"ready"`
	Waiting      []string `json:"waiting"`
	InFlight     []string `json:"inFlight"`
	Blocked      []string `json:"blocked"`
	TerminalIDs  []string `json:"terminal"`
	Unclassified []string `json:"unclassified"`

	// What the tick DID, named per task. Empty lists, never nulls: the frontend
	// renders a list and a null is a crash there.
	Checked    []string `json:"checked"`
	Integrated []string `json:"integrated"`
	Resumed    []string `json:"resumed"`
	Orphaned   []string `json:"orphaned"`

	// Suppressed is every action Loom would have taken and did not, with the
	// reason in the human's words. Rendered GREYED WITH THE REASON and never
	// dropped — a suppressed spawn that renders as nothing is indistinguishable
	// from a scheduler that is broken (§14's table says the same thing).
	Suppressed []SuppressedActionDTO `json:"suppressed"`
	// Findings is §12.2's watchdog pass. NOTHING HERE KILLS ANYTHING: the
	// actions are flag, offer-retry, resolve and stop-spawns, and a `stalled`
	// child is a label on a child that is still running.
	Findings []FindingDTO `json:"findings"`
	// Blocks is what §11.2's detector saw this tick, malformed ones included.
	Blocks []BlockEventDTO `json:"blocks"`

	// Offers is §11.3's amendment log, UNAPPROVED — the standing offers. It is
	// the log rather than this tick's proposals because the durable row is what
	// makes the offer survive a restart, and a view that rendered only what the
	// current tick re-proposed would lose every offer made before Loom last
	// started.
	Offers []AmendmentDTO `json:"offers"`
	// Granted is the approved half, for the record. §2's M3 counts BOTH — a
	// dependency the human refused was still hit by a child that could not
	// proceed without it — so the two lists are shown, never summed into one.
	Granted []AmendmentDTO `json:"granted"`
	// Declined is the third, and it exists for the same reason Granted does: the
	// row survives the decision, so the decision has to be visible. A rejected
	// amendment that simply disappeared would be re-decided by the next human to
	// read the run — and, because `propose` will not re-offer it, re-decided
	// against an offer that is no longer on the screen at all.
	Declined []AmendmentDTO `json:"declined"`

	Deadlock *DeadlockDTO `json:"deadlock"`

	// Measurements is §2's M2/M3 and Verdict is §2's own sentence about them.
	// On the run view and not behind a debug flag: it is the number the decision
	// to keep or kill this whole approach is made on, and a number that has to be
	// asked for is one that gets reconstructed by hand from a run nobody kept.
	Measurements MeasurementsDTO `json:"measurements"`
	Verdict      string          `json:"verdict"`

	// Errs is the per-task faults this tick collected. One task's git failure
	// must not cost the other eleven their tick, so they are carried; carried
	// and not rendered is the same as swallowed, so they are here.
	Errs []TaskErrorDTO `json:"errs"`
	// Error is the tick failing WHOLESALE — no ~/.loom, no store, an unreadable
	// run. Distinct from Errs for the reason those two classes differ: this one
	// means nothing moved at all.
	Error string `json:"error"`
}

type SuppressedActionDTO struct {
	TaskID string `json:"taskId"`
	Action string `json:"action"`
	Reason string `json:"reason"`
}

type FindingDTO struct {
	TaskID string `json:"taskId"`
	Kind   string `json:"kind"`
	// Action is what the finding ASKS FOR, carried verbatim so the frontend
	// cannot invent a harsher one. `offer-retry` is an affordance the human
	// presses; it is never a retry that already happened.
	Action string `json:"action"`
	Flag   string `json:"flag"`
	Detail string `json:"detail"`
	At     int64  `json:"at"`
}

type BlockEventDTO struct {
	TaskID string `json:"taskId"`
	Kind   string `json:"kind"`
	// Author is "loom" for a §9.2 producer conflict or a §10.3 integration
	// failure, empty for a child's own declaration. Rendered, because "Loom
	// stopped this" and "the child asked for something" are different sentences.
	Author     string `json:"author"`
	Summary    string `json:"summary"`
	Detail     string `json:"detail"`
	ResumeWhen string `json:"resumeWhen"`
	Artifact   string `json:"artifact"`
	At         int64  `json:"at"`
	// Cleared is a block.json that DISAPPEARED. An event because a task stuck in
	// `blocked` with no declaration on disk is otherwise indistinguishable from
	// one that is legitimately parked.
	Cleared bool `json:"cleared"`
	// Malformed is §11.2's loud case, with the raw text. A swallowed block is a
	// child parked forever with nobody told — the worst outcome this path has —
	// so the raw content is rendered rather than the parse error alone.
	Malformed bool   `json:"malformed"`
	Raw       string `json:"raw"`
	ParseErr  string `json:"parseError"`
}

// AmendmentDTO is one §11.3 amendment as an OFFER.
//
// Approved is on the wire because the difference between a proposal and a
// granted edge is the entire mechanism: Loom proposes, the human grants, and an
// amendment is INERT until then. A view that rendered a proposal as a done deed
// would show an edge the effective graph does not have.
type AmendmentDTO struct {
	Seq  int64  `json:"seq"`
	Kind string `json:"kind"`
	// Task is the task the amendment is ABOUT, Producer the task that would
	// satisfy it (for a re-plan, Loom's SUGGESTION of which task should produce
	// the artifact — a suggestion, rendered as one, never applied).
	Task     string   `json:"task"`
	Artifact string   `json:"artifact"`
	Producer string   `json:"producer"`
	Paths    []string `json:"paths"`
	// Reason is the child's own block summary, carried so the offer still reads
	// after the worktree is gone.
	Reason     string `json:"reason"`
	CreatedAt  int64  `json:"createdAt"`
	ApprovedAt int64  `json:"approvedAt"`
	Approved   bool   `json:"approved"`
	// RejectedAt/Rejected is §11.3's durable NO (migration v16). Carried even
	// though a rejected amendment leaves the Offers list, because the Declined
	// list renders it: a decision a human made an hour ago and can no longer see
	// is a decision they will make again.
	RejectedAt int64 `json:"rejectedAt"`
	Rejected   bool  `json:"rejected"`
	// Replan is §11.3's second branch: the block names an artifact NOBODY
	// produces, so there is no edge to add and there is nothing to approve. It
	// is a separate bit and not an inference at the call site because the two
	// render completely differently — an edge is a checkbox, a re-plan is a
	// conversation with the plan's author.
	Replan bool `json:"replan"`
}

type DeadlockDTO struct {
	Shape string `json:"shape"`
	// Cycle is the wait-for path, as `from → to (artifact)` edges.
	Cycle []EdgeDTO `json:"cycle"`
	// Owed is the actionable list for a run stopped on human decisions, oldest
	// first — the decision that has cost the most is the one to show at the top.
	Owed  []OwedDecisionDTO `json:"owed"`
	Stuck []string          `json:"stuck"`
	At    int64             `json:"at"`
}

type EdgeDTO struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Artifact string `json:"artifact"`
}

type OwedDecisionDTO struct {
	TaskID  string `json:"taskId"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	Since   int64  `json:"since"`
}

// MeasurementsDTO is §2's kill criterion, per run.
//
// The numerators and denominators travel, not just the two ratios: §2's rule is
// "M3 ≤ 1 per 4 tasks and M2 ≥ 0.5 on an initiative of at least 4 tasks", and a
// ratio without its counts cannot be checked against that sentence by the person
// it is addressed to.
type MeasurementsDTO struct {
	Tasks           int      `json:"tasks"`
	UnforeseenTotal int      `json:"unforeseenTotal"`
	PerFourTasks    float64  `json:"perFourTasks"`
	GreenFirstTime  []string `json:"greenFirstTime"`
	RedFirstTime    []string `json:"redFirstTime"`
	// Unmeasured are the tasks with no first-check verdict yet. Named rather
	// than counted, because they are what makes the numbers PROVISIONAL and the
	// human can go and look at them.
	Unmeasured []string `json:"unmeasured"`
	Fraction   float64  `json:"fraction"`
	// Enough is §2's "at least 4 tasks": under it the run decides nothing, which
	// is a different statement from failing.
	Enough      bool `json:"enough"`
	Provisional bool `json:"provisional"`
	M2Met       bool `json:"m2Met"`
	M3Met       bool `json:"m3Met"`
}

type TaskErrorDTO struct {
	TaskID string `json:"taskId"`
	Stage  string `json:"stage"`
	Error  string `json:"error"`
}

// TickDelegationRun runs one poll of §9's loop and returns everything it saw.
//
// It is the run view's own clock. NOTHING HERE SPAWNS — Tick computes what is
// ready and the human presses approve (§16), and a poll that could start work is
// how a night's quota disappears with nothing on screen.
func (a *App) TickDelegationRun(runID int64) TickReportDTO {
	out := emptyTickDTO(runID)
	if a.st == nil {
		out.Error = "delegation unavailable: no store"
		return out
	}
	run, ok, err := a.st.GetDelegationRun(runID)
	switch {
	case err != nil:
		out.Error = err.Error()
		return out
	case !ok:
		out.Error = fmt.Sprintf("no such delegation run: %d", runID)
		return out
	case !orchestrationVisible(a.resolver(), run.ProjectRoot):
		// The bare marker. §14's table keeps the run's own clock going — checks
		// and seed deliveries are in-flight work and are not suppressed — and it
		// is the OUTPUT that must not cross, which is this return.
		return TickReportDTO{Hidden: true}
	}
	rep, err := a.tickRun(runID)
	if err != nil {
		out.Error = err.Error()
	}
	return a.tickDTO(runID, rep, out.Error)
}

// tickDTO flattens the report. Split from the binding so the amendment log read
// — which is a second query — happens in one place for both the tick and the
// approve path, and cannot answer differently between them.
func (a *App) tickDTO(runID int64, rep delegate.TickReport, errText string) TickReportDTO {
	out := emptyTickDTO(runID)
	out.Error = errText
	if !rep.At.IsZero() {
		out.At = rep.At.Unix()
	}
	out.Ready = orEmptyIDs(rep.Progress.Ready)
	out.Waiting = orEmptyIDs(rep.Progress.Waiting)
	out.InFlight = orEmptyIDs(rep.Progress.InFlight)
	out.Blocked = orEmptyIDs(rep.Progress.Blocked)
	out.TerminalIDs = orEmptyIDs(rep.Progress.Terminal)
	out.Unclassified = orEmptyIDs(rep.Progress.Unclassified)
	out.Checked = orEmptyIDs(rep.Checked)
	out.Integrated = orEmptyIDs(rep.Integrated)
	out.Resumed = orEmptyIDs(rep.Resumed)
	out.Orphaned = orEmptyIDs(rep.Orphaned)

	for _, s := range rep.Suppressed {
		out.Suppressed = append(out.Suppressed, SuppressedActionDTO{
			TaskID: s.TaskID, Action: s.Action, Reason: s.Reason})
	}
	for _, f := range rep.Findings {
		// The `unflag` action is deliberately not rendered: it is a badge being
		// CLEARED because a live child is bound to the task again, and putting
		// "orphaned" on screen next to a task that is working would report the
		// absence of a problem as a problem.
		if f.Action == delegate.ActionUnflag {
			continue
		}
		out.Findings = append(out.Findings, FindingDTO{
			TaskID: f.TaskID, Kind: string(f.Kind), Action: string(f.Action),
			Flag: string(f.Flag), Detail: f.Detail, At: unixOrZero(f.At),
		})
	}
	for _, b := range rep.Blocks {
		out.Blocks = append(out.Blocks, blockEventDTO(b))
	}
	for _, am := range a.amendmentLog(runID) {
		dto := amendmentDTO(am)
		switch {
		case dto.Approved:
			out.Granted = append(out.Granted, dto)
		case dto.Rejected:
			// NOT an offer. This is the whole point of v16: before it, a refused
			// amendment was indistinguishable from one nobody had read, so it
			// came back on every poll forever.
			out.Declined = append(out.Declined, dto)
		default:
			out.Offers = append(out.Offers, dto)
		}
	}
	if rep.Deadlock != nil {
		out.Deadlock = deadlockDTO(rep.Deadlock)
	}
	out.Measurements = measurementsDTO(rep.Measurements)
	out.Verdict = rep.Measurements.Verdict()
	for _, e := range rep.Errs {
		msg := ""
		if e.Err != nil {
			msg = e.Err.Error()
		}
		out.Errs = append(out.Errs, TaskErrorDTO{TaskID: e.TaskID, Stage: e.Stage, Error: msg})
	}
	return out
}

// amendmentLog reads §11.3's durable log through the runner when there is one,
// and straight from the store when there is not.
//
// The fallback is not a duplicate scheduler — it is the same read Runner
// performs, and it exists so a Loom that cannot build a runner (no ~/.loom)
// still SHOWS the offers it has already recorded. An offer that vanishes because
// a seam is unwired is an offer the human never answers.
func (a *App) amendmentLog(runID int64) []delegate.Amendment {
	if r, err := a.delegationRunner(); err == nil {
		return r.AmendmentLog(runID)
	}
	rows, err := a.st.ListDelegationAmendments(runID)
	if err != nil {
		return nil
	}
	out := make([]delegate.Amendment, 0, len(rows))
	for _, row := range rows {
		// A body that will not parse still RENDERS, with its kind and seq
		// intact: an invisible amendment is an edge the human cannot see.
		am, _ := delegate.DecodeAmendmentBody(delegate.AmendmentKind(row.Kind), row.Body)
		am.RunID, am.Seq = row.RunID, row.Seq
		am.CreatedAt = time.Unix(row.CreatedAt, 0)
		if row.ApprovedAt != 0 {
			am.ApprovedAt = time.Unix(row.ApprovedAt, 0)
		}
		if row.RejectedAt != 0 {
			am.RejectedAt = time.Unix(row.RejectedAt, 0)
		}
		out = append(out, am)
	}
	return out
}

// AmendmentResultDTO is the outcome of §11.3's grant.
type AmendmentResultDTO struct {
	Hidden bool `json:"hidden"`
	// Approved is the CAS having moved the row THIS call. False with an empty
	// Error never happens; false with an Error is either a refusal (a cycle) or
	// the row already being granted, and those read differently.
	Approved bool `json:"approved"`
	// Claimed says the amendment was ALREADY approved — by the other Loom
	// instance, or by a second press of the same button. Not an error to retry:
	// the amendment is granted and what the caller is holding is a stale screen.
	Claimed bool   `json:"claimed"`
	Error   string `json:"error"`
	// Tick is the run re-ticked after the grant, so the new edge's consequences
	// — a task that is now waiting, a block that can now be resumed — are on
	// screen in the same gesture rather than one poll later.
	Tick TickReportDTO `json:"tick"`
}

// ApproveDelegationAmendment is §11.3's HUMAN GRANT, and it is the only path
// from a proposal to an edge.
//
// Loom proposes and never applies: propose() appends the row unapproved on every
// tick, Effective ignores an unapproved amendment entirely, and this is the
// press. That shape is why the amendment log can be written from a poll loop at
// all — a row that changes nothing until a human moves it is safe to re-derive.
//
// The cycle check runs against the AMENDED graph before the CAS (§11.3's last
// rule): an amendment that closes a loop turns a loud block into a silent
// deadlock, so it is refused with the cycle named.
func (a *App) ApproveDelegationAmendment(runID, seq int64) AmendmentResultDTO {
	out := AmendmentResultDTO{Tick: emptyTickDTO(runID)}
	if a.st == nil {
		out.Error = "delegation unavailable: no store"
		return out
	}
	run, ok, err := a.st.GetDelegationRun(runID)
	switch {
	case err != nil:
		out.Error = err.Error()
		return out
	case !ok:
		out.Error = fmt.Sprintf("no such delegation run: %d", runID)
		return out
	case !orchestrationVisible(a.resolver(), run.ProjectRoot):
		// The bare marker, and no write. Granting an amendment is a change to
		// the plan of a project this screen may not name.
		return AmendmentResultDTO{Hidden: true, Tick: TickReportDTO{Hidden: true}}
	}
	r, err := a.delegationRunner()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	switch err := r.ApproveAmendment(runID, seq); {
	case err == nil:
		out.Approved = true
	case errors.Is(err, delegate.ErrAmendmentClaimed):
		// Not a failure the caller retries: the edge exists. Said out loud
		// anyway, because the button they pressed did nothing and a silent
		// no-op reads as a broken button.
		out.Claimed, out.Error = true, err.Error()
	default:
		out.Error = err.Error()
	}
	rep, tickErr := a.tickRun(runID)
	tickText := ""
	if tickErr != nil {
		tickText = tickErr.Error()
	}
	out.Tick = a.tickDTO(runID, rep, tickText)
	return out
}

func emptyTickDTO(runID int64) TickReportDTO {
	return TickReportDTO{
		RunID: runID,
		Ready: []string{}, Waiting: []string{}, InFlight: []string{},
		Blocked: []string{}, TerminalIDs: []string{}, Unclassified: []string{},
		Checked: []string{}, Integrated: []string{}, Resumed: []string{}, Orphaned: []string{},
		Suppressed: []SuppressedActionDTO{}, Findings: []FindingDTO{},
		Blocks: []BlockEventDTO{}, Offers: []AmendmentDTO{}, Granted: []AmendmentDTO{},
		Declined: []AmendmentDTO{},
		Errs:     []TaskErrorDTO{},
	}
}

// orEmptyIDs keeps a nil slice off the wire. A JSON null where the frontend
// expects a list is a crash there, and every list on these DTOs is rendered.
func orEmptyIDs(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func blockEventDTO(b delegate.BlockEvent) BlockEventDTO {
	out := BlockEventDTO{TaskID: b.TaskID, Cleared: b.Cleared}
	if b.Malformed != nil {
		out.Malformed = true
		out.Raw = b.Malformed.Raw
		if b.Malformed.Err != nil {
			out.ParseErr = b.Malformed.Err.Error()
		}
		if out.TaskID == "" {
			out.TaskID = b.Malformed.TaskID
		}
		return out
	}
	out.Kind = string(b.Block.Kind)
	out.Author = string(b.Block.Author)
	out.Summary = b.Block.Summary
	out.Detail = b.Block.Detail
	out.ResumeWhen = b.Block.ResumeWhen
	out.Artifact = b.Block.Need.Artifact
	out.At = unixOrZero(b.Block.At)
	return out
}

func amendmentDTO(a delegate.Amendment) AmendmentDTO {
	out := AmendmentDTO{
		Seq: a.Seq, Kind: string(a.Kind), Task: a.Task, Artifact: a.Artifact,
		Producer: a.From, Paths: []string{}, Reason: a.Reason,
		CreatedAt: unixOrZero(a.CreatedAt), ApprovedAt: unixOrZero(a.ApprovedAt),
		Approved:   a.Accepted(),
		RejectedAt: unixOrZero(a.RejectedAt), Rejected: a.Rejected(),
		// Derived from the KIND, which is the durable discriminator, rather than
		// carried from the in-memory Proposal: an offer read back after a restart
		// must render as the same thing it was when it was made.
		Replan: a.Kind == delegate.AmendReplan,
	}
	if a.Paths != nil {
		out.Paths = a.Paths
	}
	return out
}

func deadlockDTO(d *delegate.Deadlock) *DeadlockDTO {
	out := &DeadlockDTO{
		Shape: string(d.Shape), Cycle: []EdgeDTO{}, Owed: []OwedDecisionDTO{},
		Stuck: orEmptyIDs(d.Stuck), At: unixOrZero(d.At),
	}
	for _, e := range d.Cycle {
		out.Cycle = append(out.Cycle, EdgeDTO{From: e.From, To: e.To, Artifact: e.Artifact})
	}
	for _, o := range d.Owed {
		out.Owed = append(out.Owed, OwedDecisionDTO{
			TaskID: o.TaskID, Kind: string(o.Kind), Summary: o.Summary, Since: unixOrZero(o.Since)})
	}
	return out
}

// RunDeadlockDTO is §12.1's finding on the POLL path.
//
// Deadlock is nil when the run is progressing, and that is the ordinary answer.
// Nil is distinguishable from "we could not tell" because Error carries the
// second case: a run whose deadlock could not be computed must not render as a
// healthy one.
type RunDeadlockDTO struct {
	Hidden   bool         `json:"hidden"`
	RunID    int64        `json:"runId"`
	Deadlock *DeadlockDTO `json:"deadlock"`
	Error    string       `json:"error"`
}

// RunDeadlock is §12.1's wait-for cycle, READ-ONLY.
//
// It exists because the only other way to reach a DeadlockDTO was
// TickDelegationRun, which is a writer: it polls block files, runs checks,
// performs integrations, promotes pending → ready and flips the run's status. A
// view that had to call it every 1.5s to render WHY a run is red would be
// advancing the run in order to draw it — and §12.1 asks for the cycle on the
// poll. This runs delegate's detector over already-persisted state and writes
// nothing.
//
// The verdict can therefore be one tick behind the runner's, and that is the
// correct trade rather than a defect: §14 already discloses that status lags the
// truth by up to one poll, and a deadlock that resolves is resolved by a tick
// this method deliberately does not perform.
func (a *App) RunDeadlock(runID int64) RunDeadlockDTO {
	out := RunDeadlockDTO{RunID: runID}
	if a.st == nil {
		out.Error = "delegation unavailable: no store"
		return out
	}
	run, ok, err := a.st.GetDelegationRun(runID)
	switch {
	case err != nil:
		out.Error = err.Error()
		return out
	case !ok:
		out.Error = fmt.Sprintf("no such delegation run: %d", runID)
		return out
	case !orchestrationVisible(a.resolver(), run.ProjectRoot):
		// §3.1's bare marker. A deadlock names tasks and artifacts, which are
		// agent-authored and routinely name the client; there is no label-free
		// version of this payload worth sending.
		return RunDeadlockDTO{Hidden: true}
	}
	r, err := a.delegationRunner()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	d, err := r.Deadlock(runID)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if d != nil {
		out.Deadlock = deadlockDTO(d)
	}
	return out
}

func measurementsDTO(m delegate.Measurements) MeasurementsDTO {
	return MeasurementsDTO{
		Tasks: m.Tasks, UnforeseenTotal: m.UnforeseenTotal, PerFourTasks: m.PerFourTasks,
		GreenFirstTime: orEmptyIDs(m.GreenFirstTime), RedFirstTime: orEmptyIDs(m.RedFirstTime),
		Unmeasured: orEmptyIDs(m.Unmeasured), Fraction: m.Fraction,
		Enough: m.Enough, Provisional: m.Provisional, M2Met: m.M2Met, M3Met: m.M3Met,
	}
}

// --- §10: the integration gate, and it EXECUTES now -------------------------

// §10 is the section 3a deferred and it is the one the evidence calls
// load-bearing. Three bindings, in the order a human meets them:
//
//	IntegrateTask  — run §10.2's sequence for one verified task and say what
//	                 happened, including WHICH of §10.2's two attributions it is.
//	TaskMergeGate  — §5.2's gate: what will land, what diverged, why the button
//	                 is refused. No writes.
//	MergeTask      — §10.4, executed, behind the acknowledgement the gate handed
//	                 out. This is the only thing in Loom that writes to a branch
//	                 the user owns.
//
// The human approval stays IN FRONT of the merge and is not a boolean: the ack
// lists travel back from the gate and are compared against a freshly computed
// divergence (delegate.Runner.Merge), so a preview read five minutes ago cannot
// authorize what is there now.

// IntegrationResultDTO is one pass of §10.2, rendered.
//
// Stage is carried because "the integration is red" is ambiguous and the
// remedies are completely different — a conflict is a human decision, a
// bootstrap failure is usually §6.4's environment, a per-repo failure is the
// task, a cross failure may be either side of the seam.
type IntegrationResultDTO struct {
	Hidden bool   `json:"hidden"`
	TaskID string `json:"taskId"`
	Repo   string `json:"repo"`
	// Ran distinguishes "the sequence executed" from every other outcome. A DTO
	// with a green-looking zero value and no such bit would render a refusal as
	// a pass at stage "".
	Ran bool `json:"ran"`
	// Busy is §10.2's run-wide serialization refusing, and it is NOT a fault:
	// one integration at a time, run-wide, because a cross check reads several
	// repos' integration worktrees at once and must not see one mid-merge. The
	// answer is "next tick", so it is a distinct bit rather than an error string
	// a frontend would paint red.
	Busy bool `json:"busy"`

	Stage  string `json:"stage"`
	Status string `json:"status"`
	// Blame is §10.2's attribution table, verbatim: `task` or `baseline`.
	// BlameNote is the same fact in the sentence the human needs, because the
	// two have OPPOSITE remedies and a bare word on a chip does not carry that.
	Blame     string `json:"blame"`
	BlameNote string `json:"blameNote"`
	// RunLevelFault is Blame == baseline hoisted to a bit: it means the run row
	// goes red, spawning stops and NO TASK IS BLAMED. A frontend that had to
	// compare a string to decide whether to blame a child is a frontend that
	// will one day blame the wrong one.
	RunLevelFault bool `json:"runLevelFault"`

	// CrossCheck names the failing `integration.cross` entry (§10.5's honest
	// part); Conflicts is the file list §10.3 sends back to the child.
	CrossCheck string   `json:"crossCheck"`
	Conflicts  []string `json:"conflicts"`
	// Pre is what the task was integrated ON TOP OF, and it is on the result
	// even when the pass was green: it is the only way to say that. Head is the
	// integration head after a green pass and is empty after a red one —
	// §10.2's reset means a failed pass leaves nothing behind.
	Pre    string `json:"pre"`
	Head   string `json:"head"`
	Output string `json:"output"`
	RanAt  int64  `json:"ranAt"`
	// Warnings is everything that happened which did not change the verdict and
	// which a human must still see — the loudest being "this repo declares no
	// per-repo gate", which is a REAL degradation: the task's own check becomes
	// the only evidence behind §5.2.
	Warnings []string `json:"warnings"`

	// State and RunStatus are read back AFTER the pass rather than derived from
	// the result, for taskDTO's reason: a second place that maps a verdict onto
	// a state is a second place to get it wrong, and §10.2's transitions are
	// CASes that can lose to the other Loom instance.
	State     string `json:"state"`
	RunStatus string `json:"runStatus"`
	Error     string `json:"error"`
}

// IntegrateTask runs §10.2 for one task and reports the pass.
//
// The sequence is Loom's, not this file's: Integrator.Integrate owns the claim,
// the reset-on-red and the attribution, and this binding exists so a human can
// ask for a pass now rather than waiting for the tick that would have. Both
// paths call the same function — a second sequence spelled out here would be a
// second definition of what "green" means, and §5.2 reads it.
//
// Hidden projects get the bare marker and NO execution, which differs from
// RunTaskCheck's neighbouring rule on purpose. A check is the run's clock (§14's
// table keeps it running while hidden); an integration pass ends in a state
// transition that offers a MERGE gate, and §14 suppresses merges on a hidden
// project because a merge is human-gated work whose gate cannot be shown.
func (a *App) IntegrateTask(runID int64, taskID string) IntegrationResultDTO {
	out := emptyIntegrationDTO(taskID)
	run, m, _, task, err := a.runAndTask(runID, taskID)
	switch {
	case errors.Is(err, errHiddenProject):
		return IntegrationResultDTO{Hidden: true}
	case err != nil:
		out.Error = err.Error()
		return out
	}
	out.Repo = task.Repo
	r, err := a.delegationRunner()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := r.Integrator.Integrate(ctx, run, m, task)
	switch {
	case errors.Is(err, delegate.ErrIntegrationBusy):
		out.Busy = true
	case err != nil:
		out.Error = err.Error()
	default:
		out.Ran = true
	}
	// The partial result is rendered even on error. sequence() fills Stage and
	// Conflicts before it returns, and dropping them because the call also
	// reported an error would hide the one thing that says WHERE it stopped.
	out = fillIntegrationDTO(out, res)
	out.State, out.RunStatus = a.taskAndRunState(runID, taskID)
	return out
}

func emptyIntegrationDTO(taskID string) IntegrationResultDTO {
	return IntegrationResultDTO{TaskID: taskID, Conflicts: []string{}, Warnings: []string{}}
}

func fillIntegrationDTO(out IntegrationResultDTO, res delegate.IntegrationResult) IntegrationResultDTO {
	out.Stage, out.Status = string(res.Stage), string(res.Status)
	out.Blame, out.BlameNote = string(res.Blame), blameNote(res.Blame)
	out.RunLevelFault = res.Blame == delegate.BlameBaseline
	out.CrossCheck, out.Pre, out.Head, out.Output = res.CrossCheck, res.Pre, res.Head, res.Output
	out.RanAt = unixOrZero(res.RanAt)
	out.Conflicts = orEmptyIDs(res.Conflicts)
	out.Warnings = orEmptyIDs(res.Warnings)
	return out
}

// blameNote is §10.2's table in the two sentences the human acts on. In Go and
// not in the frontend because the TUI must not say something different about the
// same verdict, and because the difference between these two rows is the
// difference between parking a child and telling the human their repo is broken.
func blameNote(b delegate.Blame) string {
	switch b {
	case delegate.BlameTask:
		return "red with this task merged and green without it: the task is to blame, " +
			"and §10.3 parks the child with the failure rather than fixing it here"
	case delegate.BlameBaseline:
		return "red with this task merged AND red without it: this is a run-level BASELINE fault. " +
			"No task is to blame, spawning stops, and the repair is to the repo's own tree"
	}
	return ""
}

// taskAndRunState re-reads the two columns every §10 binding reports afterwards.
// A read and not a derivation: each of these transitions is a CAS that can lose
// to the other Loom instance, and reporting the state this process INTENDED is
// how a UI ends up offering a gate the row never reached.
func (a *App) taskAndRunState(runID int64, taskID string) (state, runStatus string) {
	if a.st == nil {
		return "", ""
	}
	if row, ok, err := a.st.GetDelegationTask(runID, taskID); err == nil && ok {
		state = row.State
	}
	if run, ok, err := a.st.GetDelegationRun(runID); err == nil && ok {
		runStatus = run.Status
	}
	return state, runStatus
}

// MergeGateDTO is §5.2 rendered: the branch that will land, the target it lands
// in, what diverged, and every reason the button is refused.
//
// It is the one place a human reads a diff a machine wrote into a tree they own,
// so nothing here is summarized: Blockers are listed in full rather than
// expressed as a disabled button, and the two acknowledgement lists travel
// verbatim so the press can be compared against exactly what was shown.
type MergeGateDTO struct {
	Hidden bool   `json:"hidden"`
	TaskID string `json:"taskId"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	// Target is the user's CURRENTLY checked-out branch, resolved here and
	// re-resolved at merge time. A human who switches branches between reading
	// this and pressing it must not silently land the work somewhere else.
	Target string `json:"target"`

	Dirty      bool     `json:"dirty"`
	DirtyFiles []string `json:"dirtyFiles"`

	// Divergence is §12.3.1-2 recomputed for this gate — never the recorded
	// column. Drift is §12.3.3, a DIFFERENT mechanism over different evidence,
	// kept apart so the UI cannot conflate "the child committed outside its
	// paths" with "a file outside the worktree changed since spawn".
	Divergence DivergenceDTO `json:"divergence"`
	Drift      DriftDTO      `json:"drift"`
	// AckDivergence and AckDrift are what MergeTask must be handed back. Two
	// lists and not one bit: "I acknowledged something" is not consent to
	// whatever is there now, and one checkbox covering both would let §12.3.3's
	// disclosed false positives launder a real §12.3.1 finding.
	AckDivergence []string `json:"ackDivergence"`
	AckDrift      []string `json:"ackDrift"`

	// Integration is the evidence behind the gate: which baseline this task is
	// staged on and what the recorded verdict there is.
	Integration IntegrationResultDTO `json:"integration"`
	Blockers    []string             `json:"blockers"`
	Warnings    []string             `json:"warnings"`
	Error       string               `json:"error"`
}

// DriftDTO is §12.3.3's out-of-worktree tripwire.
//
// Summary comes from delegate and is not re-worded here: §12.3.3 makes the
// WORDING binding — "changed since spawn", never "the child wrote this" —
// because the walk cannot distinguish the human's own edits from a child's, and
// a detector that overclaims is one the human learns to dismiss.
type DriftDTO struct {
	Changed map[string][]string `json:"changed"`
	// NoBaseline is an absence of EVIDENCE and renders distinctly from "no
	// change": nobody looked, which is not the same as nothing happened.
	NoBaseline []string `json:"noBaseline"`
	Summary    string   `json:"summary"`
	Empty      bool     `json:"empty"`
}

// TaskMergeGate renders §5.2 for one task and writes nothing.
//
// Looking must cost nothing — the same reason TaskSpawnPreview is separate from
// ApproveTask. The divergence IS recomputed here (git, per press), which is not
// free, but §12.3 requires it immediately before every merge and a gate that
// showed a cached list would be showing a picture of a different tree.
func (a *App) TaskMergeGate(runID int64, taskID string) MergeGateDTO {
	out := emptyMergeGateDTO(taskID)
	run, m, _, task, err := a.runAndTask(runID, taskID)
	switch {
	case errors.Is(err, errHiddenProject):
		return MergeGateDTO{Hidden: true}
	case err != nil:
		out.Error = err.Error()
		return out
	}
	r, err := a.delegationRunner()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	p, err := r.Integrator.Preview(run, m, task)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	return mergeGateDTO(p)
}

func emptyMergeGateDTO(taskID string) MergeGateDTO {
	return MergeGateDTO{
		TaskID: taskID, DirtyFiles: []string{},
		Divergence:    DivergenceDTO{Outside: []string{}, Siblings: map[string][]string{}, Empty: true},
		Drift:         DriftDTO{Changed: map[string][]string{}, NoBaseline: []string{}, Empty: true},
		AckDivergence: []string{}, AckDrift: []string{},
		Integration: emptyIntegrationDTO(taskID),
		Blockers:    []string{}, Warnings: []string{},
	}
}

func mergeGateDTO(p delegate.MergePreview) MergeGateDTO {
	out := emptyMergeGateDTO(p.TaskID)
	out.Repo, out.Branch, out.Target = p.Repo, p.Branch, p.Target
	out.Dirty, out.DirtyFiles = p.Dirty, orEmptyIDs(p.DirtyFiles)
	out.Divergence = DivergenceDTO{
		Outside:  orEmptyIDs(p.Divergence.Outside),
		Siblings: map[string][]string{},
		Empty:    len(p.Divergence.Outside) == 0 && len(p.Divergence.Siblings) == 0,
	}
	for id, files := range p.Divergence.Siblings {
		out.Divergence.Siblings[id] = files
	}
	out.Drift = driftDTO(p.Divergence.Drift)
	out.AckDivergence = ackDivergenceList(p.Divergence)
	out.AckDrift = ackDriftList(p.Divergence.Drift)
	out.Integration = fillIntegrationDTO(emptyIntegrationDTO(p.TaskID), p.Integration)
	out.Integration.Repo = p.Repo
	out.Integration.Ran = p.Integration.Stage != ""
	out.Blockers = orEmptyIDs(p.Blockers)
	out.Warnings = orEmptyIDs(p.Warnings)
	return out
}

func driftDTO(d delegate.SnapshotDrift) DriftDTO {
	out := DriftDTO{Changed: map[string][]string{}, NoBaseline: orEmptyIDs(d.NoBaseline),
		Summary: d.Summary(), Empty: d.Empty()}
	for dir, files := range d.Changed {
		out.Changed[dir] = files
	}
	return out
}

// ackDivergenceList and ackDriftList flatten the two findings into the lists the
// merge press echoes back.
//
// They are a SET each, and that is what makes this safe to compute here:
// delegate compares the acknowledgement as a set (Runner.ackMismatch builds maps
// on both sides), so this file agreeing about membership is enough and it cannot
// drift into a second opinion about ordering. Membership itself is deliberately
// the same rule — the union of §12.3.1 and §12.3.2 for one, `dir/file` for the
// other, because "config.json changed" in the human's own repo and in an
// integration worktree are different findings and one acknowledgement must not
// cover both.
func ackDivergenceList(d delegate.DivergenceReport) []string {
	seen := map[string]bool{}
	out := []string{}
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
	for _, id := range sortedKeys(d.Siblings) {
		add(d.Siblings[id])
	}
	sort.Strings(out)
	return out
}

func ackDriftList(d delegate.SnapshotDrift) []string {
	out := []string{}
	for _, dir := range sortedKeys(d.Changed) {
		for _, f := range d.Changed[dir] {
			out = append(out, filepath.Join(dir, f))
		}
	}
	sort.Strings(out)
	return out
}

// MergeResultDTO is §10.4 attempted.
type MergeResultDTO struct {
	Hidden bool `json:"hidden"`
	// Merged is the branch actually landing in the user's branch. It is the only
	// field that may be believed about that; everything else is why it did not.
	Merged bool   `json:"merged"`
	State  string `json:"state"`
	// RunStatus can go red on a merge that SUCCEEDED: §10.4 step 2 re-derives the
	// integration worktree from the user's branch head and re-runs the per-repo
	// check there, and a red result is a BASELINE fault — the user's own branch
	// is red — with no task blamed.
	RunStatus string `json:"runStatus"`
	Forced    bool   `json:"forced"`
	// The refusals, each its own bit because each has its own remedy: re-read
	// the gate, commit or stash, re-read the row, answer the blockers.
	Stale   bool `json:"stale"`
	Dirty   bool `json:"dirty"`
	Moved   bool `json:"moved"`
	Refused bool `json:"refused"`
	// Baseline is the repo's integration baseline AFTER the attempt, which is
	// where §10.4 step 2's re-derivation lands durably. Kept beside Warnings,
	// not replaced by it: the column survives a restart and the warnings do not.
	Baseline *BaselineFaultDTO `json:"baseline"`
	// Warnings is §10.4 step 2 in sentences — "the user's own branch is red
	// after this merge", "the child's worktree was not removed". They come off
	// the IntegrationResult Runner.Merge returns, which used to be discarded,
	// leaving the human to infer both from a status column that records the
	// verdict and not the reason. Rendered LOUD, never as a footnote: a merge
	// that succeeded and left the user's branch red is the single most important
	// thing this screen can say.
	Warnings []string `json:"warnings"`
	// Gate is the gate RECOMPUTED, and only on a refusal: after a merge the gate
	// is spent, and re-rendering it would show "the task is merged, not
	// mergeable" as though something were wrong.
	Gate  *MergeGateDTO `json:"gate"`
	Error string        `json:"error"`
}

// MergeTask is §5.2's action and §10.4's merge, executed.
//
// What is merged is the TASK'S OWN BRANCH. Never the integration branch, which
// is cumulative: the human approves diff(B) and would get diff(A)+diff(B), with
// A's own gate never shown and A's divergence never acknowledged. That is the
// exact shape §5.2 exists to forbid, on the one mechanism the evidence says is
// load-bearing, and it is enforced in delegate — this binding cannot choose a
// source.
//
// The acknowledgements are the human approval and they are LISTS, not a boolean:
// Runner.Merge recomputes the divergence and refuses when what was acknowledged
// is not what is there now, in both directions (a file that appeared is new
// information; one that disappeared means the human is approving a picture that
// no longer exists).
//
// force is §5.2's escape hatch and it is never silent: it records the `forced`
// flag for the record, and it is still refused against a dirty target tree —
// permission to merge past an unacknowledged divergence has never been
// permission to merge onto uncommitted work.
func (a *App) MergeTask(runID int64, taskID string, ackDivergence, ackDrift []string, force bool) MergeResultDTO {
	out := MergeResultDTO{Forced: force, Warnings: []string{}}
	run, m, _, task, err := a.runAndTask(runID, taskID)
	switch {
	case errors.Is(err, errHiddenProject):
		// §14: merges are suppressed on a hidden project. Human-gated anyway,
		// and the gate is hidden.
		return MergeResultDTO{Hidden: true}
	case err != nil:
		out.Error = err.Error()
		return out
	}
	r, err := a.delegationRunner()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ack := delegate.MergeAck{Divergence: ackDivergence, Drift: ackDrift}
	res, err := r.Merge(ctx, runID, taskID, ack, force)
	// The warnings are carried on EVERY outcome, not only the successful one.
	// §10.4 step 2 runs after the merge lands, so a result that also carries a
	// refusal is the case where the sentences matter most.
	if len(res.Warnings) > 0 {
		out.Warnings = append(out.Warnings, res.Warnings...)
	}
	switch {
	case err == nil:
		out.Merged = true
	case errors.Is(err, delegate.ErrAckStale):
		out.Stale, out.Error = true, err.Error()
	case errors.Is(err, delegate.ErrDirtyTarget):
		out.Dirty, out.Error = true, err.Error()
	case errors.Is(err, delegate.ErrTaskMovedElsewhere):
		out.Moved, out.Error = true, err.Error()
	default:
		out.Refused, out.Error = true, err.Error()
	}
	out.State, out.RunStatus = a.taskAndRunState(runID, taskID)
	if run, ok, rerr := a.st.GetDelegationRun(runID); rerr == nil && ok {
		if b, found := delegate.DecodeBaselines(run.Integration)[task.Repo]; found {
			out.Baseline = &BaselineFaultDTO{
				Repo: task.Repo, Status: string(b.Status), Head: b.Head, At: b.At, Reason: b.Out}
		}
	}
	if !out.Merged {
		if p, perr := r.Integrator.Preview(run, m, task); perr == nil {
			g := mergeGateDTO(p)
			out.Gate = &g
		}
	}
	return out
}

// --- §10.5: cross-repo, and the honest limits -------------------------------

// RunIntegrationDTO is the run's staging area, per repo, plus §10.5's two
// mechanisms and the sentence that says what neither of them is.
//
// The limits are on the payload rather than left to the frontend to remember,
// because §10.5 is the one part of this design whose weakness is structural: no
// VCS operation can surface a cross-repo interface break, `git merge` in one
// repo cannot know another calls a function whose signature changed, and this
// design does not invent one. A screen that renders green per-repo baselines and
// says nothing else IS the misreading.
type RunIntegrationDTO struct {
	Hidden bool                 `json:"hidden"`
	RunID  int64                `json:"runId"`
	Repos  []RepoIntegrationDTO `json:"repos"`
	// Cross is the declared `integration.cross` checks. Empty is the common case
	// and is the whole of §10.5's disclosure — see Limits.
	Cross []CrossCheckDTO `json:"cross"`
	// Limits is §10.5 in the human's words, computed from what this run actually
	// declares. Always non-empty: even a run WITH cross checks gets the sentence
	// saying the stale-contract alarm is not integration testing.
	Limits []string `json:"limits"`
	// Drifts is the stale-contract alarm evaluated LIVE for every task that needs
	// an interface artifact — the flag is what some earlier pass found, and this
	// is about now.
	Drifts []ContractDriftDTO `json:"drifts"`
	Error  string             `json:"error"`
}

// RepoIntegrationDTO is one repo's staging area (§10.1).
type RepoIntegrationDTO struct {
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`
	// Status/Head/At/Out are the recorded baseline. Status "" is a THIRD value
	// and load-bearing: the worktree's position is recorded with no verdict when
	// no per-repo check has ever run there. Rendering it as green would certify
	// on the absence of a result; as red would blame every first integration.
	Status string `json:"status"`
	Head   string `json:"head"`
	At     int64  `json:"at"`
	Out    string `json:"out"`
	Red    bool   `json:"red"`
	// HasCheck is whether `integration.per_repo` declares a gate for this repo.
	// False is a REAL degradation and is rendered as one: the task's own check
	// becomes the only evidence behind §5.2's approval.
	HasCheck  bool     `json:"hasCheck"`
	CheckArgv []string `json:"checkArgv"`
}

// CrossCheckDTO is one declared cross-repo check.
type CrossCheckDTO struct {
	ID   string   `json:"id"`
	Repo string   `json:"repo"`
	Argv []string `json:"argv"`
	// NeedsRepos are the repos whose integration worktrees are exported to the
	// check as LOOM_REPO_<LABEL>. NeedsStatus is each one's CURRENTLY RECORDED
	// baseline, which is evidence about whether this check can run — not a
	// verdict that it will. The scheduling decision is delegate's and is
	// deliberately not re-derived here: a second opinion about readiness is how
	// a view starts promising a check that never runs.
	NeedsRepos  []string          `json:"needsRepos"`
	NeedsStatus map[string]string `json:"needsStatus"`
}

// ContractDriftDTO is §10.5's stale-contract alarm firing for one artifact.
type ContractDriftDTO struct {
	TaskID    string `json:"taskId"`
	Artifact  string `json:"artifact"`
	Producer  string `json:"producer"`
	WasCommit string `json:"wasCommit"`
	NowCommit string `json:"nowCommit"`
}

// RunIntegration renders §10.1's staging area and §10.5's limits for one run.
func (a *App) RunIntegration(runID int64) RunIntegrationDTO {
	out := RunIntegrationDTO{RunID: runID, Repos: []RepoIntegrationDTO{},
		Cross: []CrossCheckDTO{}, Limits: []string{}, Drifts: []ContractDriftDTO{}}
	if a.st == nil {
		out.Error = "delegation unavailable: no store"
		return out
	}
	run, ok, err := a.st.GetDelegationRun(runID)
	switch {
	case err != nil:
		out.Error = err.Error()
		return out
	case !ok:
		out.Error = fmt.Sprintf("no such delegation run: %d", runID)
		return out
	case !orchestrationVisible(a.resolver(), run.ProjectRoot):
		// The bare marker: repo labels, branch names, worktree paths and captured
		// check output are exactly the client-identifying material §6 keeps off a
		// shared screen.
		return RunIntegrationDTO{Hidden: true}
	}
	m, mErr := decodeSnapshot(run)
	if mErr != nil {
		out.Error = mErr.Error()
	}
	m.RepoPaths = a.repoPaths(run.ProjectRoot)

	spec, specErr := delegate.IntegrationOf(run.ManifestJSON)
	if specErr != nil {
		// LOUD. A malformed integration block means checks the author wrote are
		// not running, and a gate that is green on evidence nobody produced is the
		// worst outcome §10 has.
		out.Error = strings.TrimSpace(out.Error + " " + specErr.Error())
	}
	baselines := delegate.DecodeBaselines(run.Integration)

	// The layout is the runner's when there is one. Without ~/.loom there are no
	// paths to render, and the baselines still are — a degraded panel that names
	// the verdicts beats an empty one that names nothing.
	var layout delegate.Layout
	haveLayout := false
	if r, rerr := a.delegationRunner(); rerr == nil {
		layout, haveLayout = r.Layout, true
	}

	for _, label := range sortedRepoLabelsOf(m, baselines) {
		b := baselines[label]
		dto := RepoIntegrationDTO{
			Repo: label, Status: string(b.Status), Head: b.Head, At: b.At, Out: b.Out,
			Red: b.Red(), CheckArgv: []string{},
		}
		if haveLayout {
			dto.Branch = delegate.IntegrationBranch(run.Slug, label)
			dto.Worktree = layout.IntegrationDir(run.Slug, label)
		}
		if c, declared := spec.PerRepo[label]; declared {
			dto.HasCheck, dto.CheckArgv = true, orEmptyIDs(c.Cmd)
		}
		out.Repos = append(out.Repos, dto)
	}

	for _, c := range spec.Cross {
		dto := CrossCheckDTO{ID: c.ID, Repo: c.Repo, Argv: orEmptyIDs(c.Cmd),
			NeedsRepos: orEmptyIDs(c.Needs), NeedsStatus: map[string]string{}}
		for _, need := range c.Needs {
			dto.NeedsStatus[need] = string(baselines[need].Status)
		}
		out.Cross = append(out.Cross, dto)
	}
	out.Limits = integrationLimits(spec, out.Repos)
	out.Drifts = a.contractDrifts(run, m)
	return out
}

// sortedRepoLabelsOf is every repo the run stages: the manifest's in-scope set
// UNION whatever the baseline column already carries.
//
// The union and not just the manifest, because a baseline recorded for a repo
// the snapshot no longer names is precisely the thing a human needs to see —
// dropping it would make a stale red verdict invisible while it still counts
// against §10.2's attribution.
func sortedRepoLabelsOf(m delegate.Manifest, baselines map[string]delegate.Baseline) []string {
	seen := map[string]bool{}
	for _, t := range m.Tasks {
		seen[t.Repo] = true
	}
	for label := range baselines {
		seen[label] = true
	}
	out := make([]string, 0, len(seen))
	for label := range seen {
		if label != "" {
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out
}

// integrationLimits is §10.5 said out loud, and the wording is the spec's.
func integrationLimits(spec delegate.IntegrationSpec, repos []RepoIntegrationDTO) []string {
	out := []string{}
	var ungated []string
	for _, r := range repos {
		if !r.HasCheck {
			ungated = append(ungated, r.Repo)
		}
	}
	if len(ungated) > 0 {
		out = append(out, fmt.Sprintf(
			"no integration.per_repo check is declared for %s: each task's own check is the "+
				"only evidence behind the merge gate for those repos", strings.Join(ungated, ", ")))
	}
	if len(spec.Cross) == 0 {
		out = append(out, "this run declares no integration.cross check, so Loom CANNOT provide "+
			"test-gated cross-repo integration: no VCS operation can surface a cross-repo interface "+
			"break, and the seam is a human read at the merge gate")
	}
	// Said whether or not cross checks exist. The alarm is the thing most likely
	// to be mistaken for integration testing, and it is at its most convincing
	// exactly when it has just fired.
	out = append(out, "the stale-contract alarm catches one thing — an interface artifact "+
		"re-fingerprinted after its consumer was spawned. It is not integration testing and "+
		"catches nothing else")
	return out
}

// contractDrifts evaluates §10.5's alarm for every task that needs something.
//
// Per task and on demand, never on the poll: it reads the artifact table and the
// task's spawn-time baseline, and the run view already has a load ceiling that
// keeps taskDTO free of git and of per-task queries.
func (a *App) contractDrifts(run store.DelegationRun, m delegate.Manifest) []ContractDriftDTO {
	out := []ContractDriftDTO{}
	r, err := a.delegationRunner()
	if err != nil {
		return out
	}
	for _, t := range m.Tasks {
		if len(t.Needs) == 0 {
			continue
		}
		drifts, derr := r.Integrator.StaleContract(run, m, t)
		if derr != nil {
			// Per task, dropped: one task's unreadable row must not cost the
			// others their alarm. The flag on the row is the durable trace, and
			// §5.2's gate re-evaluates this per merge with the failure rendered.
			continue
		}
		for _, d := range drifts {
			out = append(out, ContractDriftDTO{
				TaskID: t.ID, Artifact: d.Artifact, Producer: d.Producer,
				WasCommit: d.WasCommit, NowCommit: d.NowCommit})
		}
	}
	return out
}

// --- §11: the park, its declaration, and the resume -------------------------

// ParkDTO is one parked child (§11.1) as the human meets it: what the child
// said, in its own words, and what Loom can do about it.
//
// The declaration is rendered from the ROW, which holds the bytes the child
// wrote (Detector.record stores the raw file, deliberately, because the human's
// whole remedy is reading what the child actually wrote). A malformed one is
// rendered RAW and loudly — §11.2's worst outcome is a swallowed block, which is
// a child parked forever with nobody told.
type ParkDTO struct {
	Hidden bool   `json:"hidden"`
	TaskID string `json:"taskId"`
	State  string `json:"state"`
	// Parked is the STATE, not the presence of a declaration. The two disagree in
	// both directions and each disagreement is a finding: blocked with no
	// declaration is §11.2's cleared block, and a declaration on a running task
	// is a park the detector has not yet acted on.
	Parked     bool     `json:"parked"`
	HasBlock   bool     `json:"hasBlock"`
	Kind       string   `json:"kind"`
	Author     string   `json:"author"`
	Summary    string   `json:"summary"`
	Detail     string   `json:"detail"`
	ResumeWhen string   `json:"resumeWhen"`
	Artifact   string   `json:"artifact"`
	From       string   `json:"from"`
	At         int64    `json:"at"`
	Paths      []string `json:"paths"`
	Malformed  bool     `json:"malformed"`
	Raw        string   `json:"raw"`
	ParseError string   `json:"parseError"`
	// PendingSeed is §11.4's durable column: the text Loom owes this child. It is
	// rendered because "seed pending" with nothing behind it is a state the
	// workflow view already refuses to show.
	PendingSeed string   `json:"pendingSeed"`
	Flags       []string `json:"flags"`
	SessionName string   `json:"sessionName"`
	// Resumable and ResumeNote are the affordance and its reason. A greyed
	// button with no sentence is how a human concludes the park is permanent.
	Resumable  bool   `json:"resumable"`
	ResumeNote string `json:"resumeNote"`
	Error      string `json:"error"`
}

// TaskPark renders one task's park. No writes, no git, no delivery.
func (a *App) TaskPark(runID int64, taskID string) ParkDTO {
	out := ParkDTO{TaskID: taskID, Paths: []string{}, Flags: []string{}}
	_, _, row, _, err := a.runAndTask(runID, taskID)
	switch {
	case errors.Is(err, errHiddenProject):
		return ParkDTO{Hidden: true}
	case err != nil:
		out.Error = err.Error()
		return out
	}
	return parkDTO(row)
}

func parkDTO(row store.DelegationTask) ParkDTO {
	out := ParkDTO{
		TaskID: row.TaskID, State: row.State,
		Parked:      delegate.TaskState(row.State) == delegate.StateBlocked,
		PendingSeed: row.PendingSeed, Flags: flagList(row.Flags),
		SessionName: row.SessionName, Paths: []string{},
	}
	raw := strings.TrimSpace(row.BlockJSON)
	if raw != "" {
		out.HasBlock = true
		b, err := delegate.ParseBlock([]byte(row.BlockJSON))
		if err != nil {
			out.Malformed, out.Raw, out.ParseError = true, row.BlockJSON, err.Error()
		} else {
			out.Kind, out.Author = string(b.Kind), string(b.Author)
			out.Summary, out.Detail, out.ResumeWhen = b.Summary, b.Detail, b.ResumeWhen
			out.Artifact, out.From = b.Need.Artifact, b.Need.From
			// The child's requested authorization, for a needs-scope block. It is
			// what §11.3's proposal is BUILT from and it is never auto-granted —
			// rendering it here is what lets a human read the ask before pressing
			// approve on the amendment that would widen the brief.
			out.Paths = orEmptyIDs(b.Paths)
			out.At = unixOrZero(b.At)
		}
	}
	out.Resumable, out.ResumeNote = resumability(row, out)
	return out
}

// resumability is what the resume button may do, and WHY when it may not.
//
// The rule is deliberately narrow: a manual resume re-attempts a delivery Loom
// has ALREADY decided is due — the durable pending_seed is that decision, and
// §12.2's `block-stale` watchdog is the affordance's own justification ("render
// seed pending, offer retry").
//
// Rejected, and this is the important half: recomputing Rendezvous.Unblocked
// here so the button could also unblock a task the tick has not got to. That
// needs the effective graph, the state map and the published set — i.e. a second
// scheduler in a DTO shim, which is the exact defect refreshReady's comment
// records being removed. Dependency-gated scheduling stays primary and the tick
// stays the only thing that decides a park is over; this button only re-attempts
// what that decision already owes.
func resumability(row store.DelegationTask, p ParkDTO) (bool, string) {
	switch {
	case !p.Parked:
		return false, "this task is not parked"
	case p.Malformed:
		return false, "the declaration will not parse: fix or remove block.json in the task's " +
			".meta/ directory — Loom will not guess what the child meant"
	case strings.TrimSpace(row.PendingSeed) != "":
		return true, "a seed is durably owed to this child; retry the delivery"
	case p.Kind == string(delegate.BlockNeedsArtifact):
		return false, "waiting on the producer: the run's own tick materializes the artifact and " +
			"seeds the child when its producer is verified and the artifact is published"
	}
	return false, "a human clears this park: answer it, or approve the amendment it raised — " +
		"the tick does not resume a needs-decision, needs-scope or blocked-external block on its own"
}

// ResumeResultDTO is §11.4's delivery re-attempted.
type ResumeResultDTO struct {
	Hidden bool `json:"hidden"`
	// Resumed means the whole of §11.4 completed: materialize, seed, clear the
	// declaration, and only then the blocked → running CAS. The order is
	// delegate's and it is load-bearing — a task marked running whose seed never
	// arrived is a child sitting at a prompt Loom believes is working.
	Resumed bool   `json:"resumed"`
	State   string `json:"state"`
	// Owed is the continue gate timing out. NOT a failure: the seed stays in the
	// column, the flag stays on, and the retry stays offered.
	Owed bool `json:"owed"`
	// ChildGone is a park whose child is no longer there. A different decision
	// for the human — re-spawn by claude id, or abandon — and never a retry loop.
	ChildGone bool `json:"childGone"`
	// Conflict is §11.4 step 2 refusing: the producer's branch would not merge
	// into the child's worktree. The task STAYS blocked and the seed describes
	// the conflict instead of telling the child to continue.
	Conflict bool    `json:"conflict"`
	Park     ParkDTO `json:"park"`
	Error    string  `json:"error"`
}

// ResumeTask re-attempts §11.4's delivery for a parked child.
//
// It never bypasses the tick's decision that the park is over (see
// resumability), and it never invents a seed: what it re-delivers is the text
// already in pending_seed, through Rendezvous, which owns the order and the
// double-delivery CAS.
func (a *App) ResumeTask(runID int64, taskID string) ResumeResultDTO {
	out := ResumeResultDTO{Park: ParkDTO{TaskID: taskID, Paths: []string{}, Flags: []string{}}}
	run, m, row, task, err := a.runAndTask(runID, taskID)
	switch {
	case errors.Is(err, errHiddenProject):
		// §14: seed DELIVERIES keep running while hidden — they are the
		// continuation of in-flight work — but that is the tick's own clock. This
		// is a human pressing a button on a screen, and what it returns is the
		// child's declaration.
		return ResumeResultDTO{Hidden: true, Park: ParkDTO{Hidden: true}}
	case err != nil:
		out.Error = err.Error()
		return out
	}
	park := parkDTO(row)
	out.Park = park
	if !park.Resumable {
		// The refusal carries the same sentence the button was greyed with, so a
		// second Loom instance racing this one cannot produce a bare no-op.
		out.Error = park.ResumeNote
		out.State = row.State
		return out
	}
	r, err := a.delegationRunner()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	b, perr := delegate.ParseBlock([]byte(row.BlockJSON))
	if perr != nil {
		out.Error = perr.Error()
		out.State = row.State
		return out
	}
	switch err := r.Rendezvous.Resume(run, m, task, b); {
	case err == nil:
		out.Resumed = true
	case errors.Is(err, delegate.ErrSeedUndelivered):
		out.Owed, out.Error = true, err.Error()
	case errors.Is(err, delegate.ErrChildGone):
		out.ChildGone, out.Error = true, err.Error()
	default:
		var conflict *delegate.ProducerConflict
		if errors.As(err, &conflict) {
			out.Conflict = true
		}
		out.Error = err.Error()
	}
	if fresh, ok, ferr := a.st.GetDelegationTask(runID, taskID); ferr == nil && ok {
		out.Park = parkDTO(fresh)
		out.State = fresh.State
	}
	return out
}

// --- §11.3: refusing an amendment -------------------------------------------

// RejectDelegationAmendment is the human declining an offer, durably.
//
// §11.3's mechanism is that Loom proposes and the human grants: an unapproved
// amendment is INERT (Effective ignores it entirely), so declining one changes
// nothing about the plan. What the decline has to do is be REMEMBERED. Before
// migration v16 it could not be: `approved_at = 0` meant "proposed", a refusal
// was indistinguishable from an offer nobody had read, and the offer came back
// on the next poll forever — the one gate in this design built to be refusable
// had no way to record a refusal.
//
// The write is delegate.Runner.RejectAmendment and not a store call from here,
// for this file's standing rule: a DTO shim that performs a transition itself is
// a second definition of the transition. The Runner's CAS is guarded on BOTH
// decision columns, so an approve and a reject racing from two Loom instances
// produce exactly one decision and the loser is TOLD.
//
// The row is not deleted and the log stays append-only. That is not a
// limitation being worked around — it is what keeps `propose` idempotent: the
// append-once rule is keyed on a proposal's identity across the whole log, so a
// rejected amendment that vanished would be re-proposed on the very next tick.
//
// Rejected alternative, deliberately: seeding the child with the refusal from
// here. It is the actionable half — a `needs-scope` child that is never told
// waits forever — but it would leave block.json on disk with the task still
// `blocked`, so the very next detector poll re-parks it and the child receives a
// refusal it cannot act on. Clearing the park properly is finishResume's job.
//
// An already-APPROVED amendment cannot be revoked, and that refusal is loud: the
// edge is in the effective graph, children may have been spawned against it, and
// pretending otherwise would be the worst thing this binding could do.
type AmendmentRejectDTO struct {
	Hidden bool `json:"hidden"`
	// Rejected is "this offer will not be granted"; Persisted is whether that
	// survives a restart. They are still two fields rather than one: a reject
	// whose durable write failed must not render as a reject that stuck, and
	// collapsing them is how a human comes to believe an offer is gone when the
	// other Loom instance is still showing it.
	Rejected  bool `json:"rejected"`
	Persisted bool `json:"persisted"`
	// Granted is the refusal above: the amendment is already approved and an
	// approved amendment is not revocable.
	Granted bool `json:"granted"`
	// Note is the sentence the UI must render beside the press so a human is
	// never left guessing what the decision did.
	Note  string        `json:"note"`
	Tick  TickReportDTO `json:"tick"`
	Error string        `json:"error"`
}

func (a *App) RejectDelegationAmendment(runID, seq int64) AmendmentRejectDTO {
	out := AmendmentRejectDTO{Tick: emptyTickDTO(runID)}
	if a.st == nil {
		out.Error = "delegation unavailable: no store"
		return out
	}
	run, ok, err := a.st.GetDelegationRun(runID)
	switch {
	case err != nil:
		out.Error = err.Error()
		return out
	case !ok:
		out.Error = fmt.Sprintf("no such delegation run: %d", runID)
		return out
	case !orchestrationVisible(a.resolver(), run.ProjectRoot):
		return AmendmentRejectDTO{Hidden: true, Tick: TickReportDTO{Hidden: true}}
	}
	row, found, err := a.st.GetDelegationAmendment(runID, seq)
	switch {
	case err != nil:
		out.Error = err.Error()
		return out
	case !found:
		out.Error = fmt.Sprintf("run %d has no amendment %d", runID, seq)
		return out
	case row.ApprovedAt != 0:
		out.Granted = true
		out.Error = fmt.Sprintf("amendment %d is already approved: an approved amendment is an edge "+
			"in the effective graph and is not revocable", seq)
		return out
	case row.RejectedAt != 0:
		// Already decided, and the second press is not an error the human needs
		// to act on: the answer they wanted is the answer on the row.
		out.Rejected, out.Persisted = true, true
		out.Note = "already declined; the offer is closed and stays in the log"
		out.Tick = emptyTickDTO(runID)
		return out
	}

	r, err := a.delegationRunner()
	if err != nil {
		// Rejected NOTHING. The press did not take, and saying it did is the one
		// outcome this binding must never produce.
		out.Error = err.Error()
		return out
	}
	if err := r.RejectAmendment(runID, seq); err != nil {
		out.Error = err.Error()
		if errors.Is(err, delegate.ErrAmendmentClaimed) {
			// The other instance decided it between our read and our CAS. The
			// screen is stale, not wrong; the tick below re-renders it.
			out.Tick = a.tickAfterDecision(runID)
		}
		return out
	}
	out.Rejected, out.Persisted = true, true
	out.Note = "declined, durably. Nothing was granted: an unapproved amendment is inert, and the " +
		"row stays in the append-only log so the decision is legible later and the same offer is " +
		"not re-proposed on the next tick"
	out.Tick = a.tickAfterDecision(runID)
	return out
}

// tickAfterDecision re-runs the scheduler and renders it, folding a tick failure
// into the report rather than losing it. Shared by the two amendment decisions
// so neither can grow a differently-shaped answer to "what changed".
func (a *App) tickAfterDecision(runID int64) TickReportDTO {
	rep, err := a.tickRun(runID)
	text := ""
	if err != nil {
		text = err.Error()
	}
	return a.tickDTO(runID, rep, text)
}
