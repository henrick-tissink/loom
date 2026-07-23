package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

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

	Flags      []string `json:"flags"`
	Terminal   bool     `json:"terminal"`
	HoldsChild bool     `json:"holdsChild"`

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
		Needs: []string{}, CheckArgv: []string{}, Blocked: row.BlockJSON,
		Divergence: divergenceDTO(delegate.DecodeDivergence(row.Divergence)),
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
	// The ready set moves when a check lands — that is the whole of 3a's
	// scheduler. Re-evaluated here rather than only on a poll so a green check
	// makes its consumers approvable in the same gesture.
	a.refreshReady(runID, m)

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
// BINDING (§2): in 3a the merge gate is a human reading the check result and
// running `git merge` themselves — Loom PRINTS the command and does not run it.
// §§9-12's integration worktrees, Loom-run merge, force-merge and the
// divergence acknowledgement are all deferred until 3a has run on a real
// initiative, and there is deliberately no execute path here for one of them to
// be smuggled in through.
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

	bases, err := delegate.PinBases(m)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	snapshot, err := json.Marshal(m)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	baseJSON, err := json.Marshal(bases)
	if err != nil {
		out.Error = err.Error()
		return out
	}

	now := a.now().Unix()
	run, err := a.st.InsertDelegationRun(m.Name, root, string(snapshot), string(baseJSON), now)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.RunID, out.Slug, out.Bases = run.ID, run.Slug, bases

	// Tasks are inserted `pending`; refreshReady below promotes the ones with
	// no unmet edges. Two steps rather than one so there is exactly ONE place
	// that decides what is ready — a run whose creation seeded `ready` directly
	// would have a second, subtly different scheduler in it.
	for _, t := range m.Tasks {
		if err := a.st.InsertDelegationTask(store.DelegationTask{
			RunID: run.ID, TaskID: t.ID, State: string(delegate.StatePending),
			RepoLabel: t.Repo, UpdatedAt: now,
		}); err != nil {
			out.Error = fmt.Sprintf("run %d created, but task %q could not be written: %v", run.ID, t.ID, err)
			return out
		}
	}
	if _, err := a.st.AdvanceDelegationRunCAS(run.ID, "planning", "running", now); err != nil {
		// Not fatal: the tasks exist and the run is usable. Surfaced anyway,
		// because a run stuck in `planning` is a status chip that will confuse
		// whoever reads it later.
		out.Error = err.Error()
	}
	out.Ready = a.refreshReady(run.ID, m)
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
	m, mErr := decodeSnapshot(run)
	if mErr == nil {
		m.RepoPaths = a.repoPaths(run.ProjectRoot)
		a.refreshReady(runID, m)
	}
	out.Runs = append(out.Runs, a.delegationRunDTO(run))
	return out
}

// refreshReady promotes pending → ready for every task §9.1's pure predicate
// proposes, and returns the tasks that are ready AFTERWARDS.
//
// The promotion is a CAS from `pending` per task, so it is idempotent, cannot
// resurrect a task that has moved on, and is safe for two Loom instances to run
// at once. A task Ready proposes that is already `ready` is simply not moved.
//
// It PROPOSES ONLY. Nothing here spawns: §5.1's human gate is the only path to
// a child, and a scheduler that could start work would make that sentence
// false.
func (a *App) refreshReady(runID int64, m delegate.Manifest) []string {
	rows, err := a.st.ListDelegationTasks(runID)
	if err != nil {
		return []string{}
	}
	states := make(map[string]delegate.TaskState, len(rows))
	for _, r := range rows {
		states[r.TaskID] = delegate.TaskState(r.State)
	}
	// `published` is the artifact ids §8.3 has verified as committed, read from
	// delegation_artifacts. Both halves of §9.1 are required — a producer that
	// is verified but published nothing means the check did not cover the
	// handoff — so an empty table correctly leaves every needs-bearing task
	// pending rather than waving it through.
	arts, err := a.st.ListDelegationArtifacts(runID)
	if err != nil {
		return []string{}
	}
	published := make(map[string]bool, len(arts))
	for _, art := range arts {
		published[art.ArtifactID] = true
	}

	now := a.now().Unix()
	ready := delegate.Ready(delegate.BuildGraph(m), states, published)
	out := make([]string, 0, len(ready))
	for _, id := range ready {
		if states[id] == delegate.StatePending || states[id] == "" {
			if _, err := a.st.AdvanceTaskCAS(runID, id,
				string(delegate.StatePending), string(delegate.StateReady), now); err != nil {
				continue
			}
		}
		out = append(out, id)
	}
	return out
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
