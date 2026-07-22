package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/workflow"
)

// launchCols/launchRows size the tmux panes workflow steps launch into, matching
// the values LaunchSession uses.
const launchCols, launchRows = 120, 32

// WorkflowDefDTO is a loaded workflow definition for the WORKFLOWS list.
type WorkflowDefDTO struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Steps   int    `json:"steps"`
	Project string `json:"project"` // step-1 project basename
}

// LoadErrorDTO is a malformed definition file (shown dim-red).
type LoadErrorDTO struct {
	Path string `json:"path"`
	Err  string `json:"err"`
}

// WorkflowsDTO bundles defs + load errors so the single bound call returns both.
type WorkflowsDTO struct {
	Defs   []WorkflowDefDTO `json:"defs"`
	Errors []LoadErrorDTO   `json:"errors"`
}

// RunDTO is one active run for the RUNS list.
type RunDTO struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	StepLabel   string `json:"stepLabel"` // "step N/M label"
	StepIdx     int    `json:"stepIdx"`
	StepCount   int    `json:"stepCount"`
	Status      string `json:"status"`
	SessionName string `json:"sessionName"`
	Live        bool   `json:"live"`        // current step's session is attachable
	PendingSeed bool   `json:"pendingSeed"` // a seed is queued (retryable)
	SeedFailed  bool   `json:"seedFailed"`  // last seed delivery failed
	DefErr      bool   `json:"defErr"`      // def_json snapshot didn't parse
}

// StepPreviewDTO mirrors workflow.StepPreview for the advance confirm dialog.
type StepPreviewDTO struct {
	Label       string `json:"label"`
	Relation    string `json:"relation"`
	Seed        string `json:"seed"`
	Unavailable bool   `json:"unavailable"`
	Finish      bool   `json:"finish"`
}

// AdvanceResultDTO encodes the outcomes the UI must distinguish without a
// thrown error: ContinueDead offers a "fork instead" retry; Stale means the
// run moved on and the list should just refresh.
type AdvanceResultDTO struct {
	Advanced     bool   `json:"advanced"`
	ContinueDead bool   `json:"continueDead"`
	Stale        bool   `json:"stale"`
	Error        string `json:"error"`
}

// runStepLabel renders "step N/M label" from a run's def_json snapshot. defErr
// is true when the snapshot doesn't parse (rendered dim-red). Pure/testable.
func runStepLabel(defJSON string, stepIdx int64) (label string, count int, defErr bool) {
	var def workflow.Definition
	if err := json.Unmarshal([]byte(defJSON), &def); err != nil || len(def.Steps) == 0 {
		return "", 0, true
	}
	count = len(def.Steps)
	name := ""
	if stepIdx >= 0 && int(stepIdx) < count {
		name = def.Steps[stepIdx].Label
	}
	return fmt.Sprintf("step %d/%d %s", stepIdx+1, count, name), count, false
}

// defDirs is every directory a definition names — each step's Project, which
// LoadAll has already resolved from a label to an absolute path. A definition
// is attributed by the UNION, not by step 1, for the same reason a run is: a
// chain that starts in a visible repo and moves into a hidden one would
// otherwise sit in the list naming the hidden project's work.
func defDirs(d workflow.Definition) []string {
	out := make([]string, 0, len(d.Steps))
	for _, s := range d.Steps {
		if s.Project != "" {
			out = append(out, s.Project)
		}
	}
	return out
}

// defsToDTOs maps loaded definitions to their list view (step-1 project shown
// as a basename), dropping the ones §6 hides — the definitions list is its own
// leak surface, distinct from the runs list. Pure/testable.
func defsToDTOs(defs []workflow.Definition, res *projects.Resolver) []WorkflowDefDTO {
	out := make([]WorkflowDefDTO, 0, len(defs))
	for _, d := range defs {
		if !visible(res, defDirs(d)...) {
			continue
		}
		proj := ""
		if len(d.Steps) > 0 && d.Steps[0].Project != "" {
			proj = filepath.Base(d.Steps[0].Project)
		}
		out = append(out, WorkflowDefDTO{Name: d.Name, Path: d.Path, Steps: len(d.Steps), Project: proj})
	}
	return out
}

func loadErrsToDTOs(errs []workflow.LoadError) []LoadErrorDTO {
	out := make([]LoadErrorDTO, 0, len(errs))
	for _, e := range errs {
		out = append(out, LoadErrorDTO{Path: e.Path, Err: e.Err})
	}
	return out
}

// buildRunDTO folds a run row plus its resolved current-step session into the
// RUNS list shape (replicates the TUI's buildWFRunRow).
func (a *App) buildRunDTO(run store.RunRow) RunDTO {
	label, count, defErr := runStepLabel(run.DefJSON, run.StepIdx)
	d := RunDTO{
		ID: run.ID, Name: run.Name, Status: run.Status,
		StepLabel: label, StepIdx: int(run.StepIdx), StepCount: count,
		PendingSeed: run.PendingSeed != "", DefErr: defErr,
	}
	if a.runner != nil {
		if row, ok := a.runner.ResolveStepSession(run); ok {
			d.SessionName = row.Name
			d.Live = row.EndedAt < 0
			d.SeedFailed = row.SeedStatus == "failed"
		}
	}
	return d
}

// ListWorkflows returns the loaded definitions and any malformed files.
func (a *App) ListWorkflows() WorkflowsDTO {
	if a.runner == nil {
		return WorkflowsDTO{Defs: []WorkflowDefDTO{}, Errors: []LoadErrorDTO{}}
	}
	defs, errs := workflow.LoadAll(a.workflowsDir, a.workflowRepos())
	return WorkflowsDTO{Defs: defsToDTOs(defs, a.resolver()), Errors: loadErrsToDTOs(errs)}
}

// runDirs is the UNION of every directory a run touches: each step's resolved
// path from the def_json snapshot, plus the cwd (and add-dirs) of every
// session the run has actually launched. All derivable — no schema change.
//
// Attributing a run to step 1 is wrong (§6.3): a run that has advanced into a
// hidden repo would keep a visible row naming that project's live session.
func (a *App) runDirs(run store.RunRow) []string {
	var out []string
	var def workflow.Definition
	if err := json.Unmarshal([]byte(run.DefJSON), &def); err == nil {
		out = append(out, defDirs(def)...)
	}
	if a.st != nil {
		for _, name := range run.SessionNames {
			if row, ok, err := a.st.Get(name); err == nil && ok {
				out = append(out, sessionDirs(row)...)
			}
		}
	}
	return out
}

// ListRuns returns the active runs for the RUNS list.
func (a *App) ListRuns() (out []RunDTO) {
	out = []RunDTO{}
	defer func() { _ = recover() }()
	if a.runner == nil || a.st == nil {
		return out
	}
	runs, err := a.st.ActiveRuns()
	if err != nil {
		return out
	}
	res := a.resolver()
	for _, r := range runs {
		if !visible(res, a.runDirs(r)...) {
			continue
		}
		out = append(out, a.buildRunDTO(r))
	}
	return out
}

// StartWorkflow starts the definition at defPath (its step 1 launches now) and
// returns the new run id.
func (a *App) StartWorkflow(defPath string) (int64, error) {
	if a.runner == nil {
		return 0, fmt.Errorf("workflows unavailable")
	}
	defs, _ := workflow.LoadAll(a.workflowsDir, a.workflowRepos())
	for _, d := range defs {
		if d.Path == defPath {
			return a.runner.Start(d, launchCols, launchRows, a.now())
		}
	}
	return 0, fmt.Errorf("workflow not found: %s", defPath)
}

// PreviewAdvance computes what advancing the run would do (reads the current
// step's transcript to substitute {{prev.*}} tokens) for the confirm dialog.
func (a *App) PreviewAdvance(runID int64) (StepPreviewDTO, error) {
	if a.runner == nil || a.st == nil {
		return StepPreviewDTO{}, fmt.Errorf("workflows unavailable")
	}
	run, ok, err := a.st.GetRun(runID)
	if err != nil {
		return StepPreviewDTO{}, err
	}
	if !ok {
		return StepPreviewDTO{}, fmt.Errorf("run %d not found", runID)
	}
	p, err := a.runner.Preview(run)
	if err != nil {
		return StepPreviewDTO{}, err
	}
	return StepPreviewDTO{Label: p.Label, Relation: p.Relation, Seed: p.Seed, Unavailable: p.Unavailable, Finish: p.Finish}, nil
}

// AdvanceRun advances the run one step. forceFork demotes a dead `continue` to
// a fork. Outcomes are encoded in the DTO (ContinueDead / Stale) so the UI can
// offer the right recovery instead of a generic error.
func (a *App) AdvanceRun(runID int64, forceFork bool) AdvanceResultDTO {
	if a.runner == nil || a.st == nil {
		return AdvanceResultDTO{Error: "workflows unavailable"}
	}
	run, ok, err := a.st.GetRun(runID)
	if err != nil {
		return AdvanceResultDTO{Error: err.Error()}
	}
	if !ok || run.Status != "running" {
		return AdvanceResultDTO{Stale: true}
	}
	switch err := a.runner.Advance(run, forceFork, launchCols, launchRows, a.now()); {
	case err == nil:
		return AdvanceResultDTO{Advanced: true}
	case errors.Is(err, workflow.ErrContinueDead):
		return AdvanceResultDTO{ContinueDead: true}
	case errors.Is(err, workflow.ErrRunAdvancedElsewhere):
		return AdvanceResultDTO{Stale: true}
	default:
		return AdvanceResultDTO{Error: err.Error()}
	}
}

// FinishRun marks a run done (used when the preview reports Finish).
func (a *App) FinishRun(runID int64) error { return a.runOp(runID, a.runner.Finish) }

// AbandonRun marks a run abandoned (does not kill its session).
func (a *App) AbandonRun(runID int64) error { return a.runOp(runID, a.runner.Abandon) }

// runOp fetches a run and applies a now-taking Runner terminal transition.
func (a *App) runOp(runID int64, op func(store.RunRow, time.Time) error) error {
	if a.runner == nil || a.st == nil {
		return fmt.Errorf("workflows unavailable")
	}
	run, ok, err := a.st.GetRun(runID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("run %d not found", runID)
	}
	return op(run, a.now())
}

// RetryRunSeed re-delivers a run's pending seed.
func (a *App) RetryRunSeed(runID int64) error {
	if a.runner == nil || a.st == nil {
		return fmt.Errorf("workflows unavailable")
	}
	run, ok, err := a.st.GetRun(runID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("run %d not found", runID)
	}
	return a.runner.RetryPendingSeed(run)
}

// AttachRun resolves a run's current-step session name so the frontend can
// select (attach) it. Errors when the step has no resolvable session.
func (a *App) AttachRun(runID int64) (string, error) {
	if a.runner == nil || a.st == nil {
		return "", fmt.Errorf("workflows unavailable")
	}
	run, ok, err := a.st.GetRun(runID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("run %d not found", runID)
	}
	row, ok := a.runner.ResolveStepSession(run)
	if !ok {
		return "", fmt.Errorf("no session for this step")
	}
	return row.Name, nil
}
