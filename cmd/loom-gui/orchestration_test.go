package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/delegate"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
)

// These tests carry orchestration-view §3.1 ("the server call is the gate, not
// the route") and delegation §2's 3a boundary. §3.1 is the one that is easy to
// pass while broken: ListProjectDetails is DELIBERATELY unfiltered so a hidden
// project can be unhidden from its own overview, so every binding keyed on a
// root inherits no filter at all and has to carry its own. Hence a gate case on
// every single binding rather than one representative test.

// seedRun writes a project, its repo, a run and one task, and returns the run.
// The manifest snapshot is written as the real thing would be: the on-disk JSON
// shape, without the resolved-at-load fields, because those are `json:"-"` and a
// test that seeded them would never notice they are re-derived.
type delegSeed struct {
	root     string
	repoPath string
	state    string
	worktree string
	argv     []string // the task's check argv; nil means ["true"]
	// paths is the task's declared `paths` (§4.2) — the divergence detector's
	// input. siblingPaths, when set, adds a SECOND task in the same repo so
	// §12.3.2's sibling comparison has something to hit.
	paths        []string
	siblingPaths []string
	// baseSHA is what the worktree was cut from. Empty leaves the row's
	// base_sha unset, which is what a task looks like before it is claimed.
	baseSHA string
}

func seedDelegationRun(t *testing.T, a *App, s delegSeed) store.DelegationRun {
	t.Helper()
	root, repoPath := s.root, s.repoPath
	if repoPath == "" {
		repoPath = root
	}
	argv := s.argv
	if argv == nil {
		argv = []string{"true"}
	}
	if err := a.st.UpsertProject(store.Project{
		Root: root, Name: "Atlas", Origin: "discovered", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.st.UpsertProjectRepo(store.ProjectRepo{
		Path: repoPath, ProjectRoot: root, Label: "api", AddedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	tasks := []map[string]any{{
		"id": "schema", "title": "Extract the account schema", "repo": "api",
		"authorization": "only db/", "needs": []string{"cfg"},
		"check": map[string]any{"cmd": argv},
		"paths": s.paths,
	}}
	if s.siblingPaths != nil {
		tasks = append(tasks, map[string]any{
			"id": "handlers", "title": "Rewrite the handlers", "repo": "api",
			"authorization": "only http/",
			"check":         map[string]any{"cmd": []string{"true"}},
			"paths":         s.siblingPaths,
		})
	}
	snap := map[string]any{
		"manifest": 1, "name": "atlas", "project": "Atlas", "tasks": tasks,
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	bases, err := json.Marshal(map[string]string{"api": "deadbeef"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := a.st.InsertDelegationRun("atlas", root, string(b), string(bases), 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.st.InsertDelegationTask(store.DelegationTask{
		RunID: run.ID, TaskID: "schema", State: s.state, RepoLabel: "api",
		Branch: delegate.BranchName(run.Slug, "schema"), Worktree: s.worktree,
		BaseSHA: s.baseSHA, UpdatedAt: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if s.siblingPaths != nil {
		if err := a.st.InsertDelegationTask(store.DelegationTask{
			RunID: run.ID, TaskID: "handlers", State: string(delegate.StatePending),
			RepoLabel: "api", UpdatedAt: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return run
}

func hideProject(t *testing.T, a *App, root string) {
	t.Helper()
	if err := a.st.SetProjectHidden(root, true, 11); err != nil {
		t.Fatal(err)
	}
}

// TestOrchestrationGate_hiddenProject is §3.1 on every binding at once. A new
// binding that forgets the gate fails here, which is the entire point of the
// table: §6.3's leak-surface list grows by four in this slice and the one that
// gets forgotten is the one nobody wrote a test for.
func TestOrchestrationGate_hiddenProject(t *testing.T) {
	root := t.TempDir()

	cases := []struct {
		name string
		// call returns (hidden, payloadEmpty) — payloadEmpty asserts §3.1's
		// second half: the marker and NOTHING else, because a hidden project
		// that rendered a count or a path would leak in exactly one bit.
		call func(a *App, run store.DelegationRun) (bool, bool)
	}{
		{"ProjectOrchestrator", func(a *App, _ store.DelegationRun) (bool, bool) {
			d := a.ProjectOrchestrator(root)
			return d.Hidden, d.Orchestrator == nil
		}},
		{"ProjectDelegation", func(a *App, _ store.DelegationRun) (bool, bool) {
			d := a.ProjectDelegation(root)
			return d.Hidden, len(d.Runs) == 0 && d.Error == ""
		}},
		{"ValidateManifests", func(a *App, _ store.DelegationRun) (bool, bool) {
			d := a.ValidateManifests(root)
			return d.Hidden, d.Dir == "" && len(d.Manifests) == 0 && len(d.Errors) == 0
		}},
		{"ApproveTask", func(a *App, run store.DelegationRun) (bool, bool) {
			d := a.ApproveTask(run.ID, "schema")
			return d.Hidden, d.SessionName == "" && d.Error == ""
		}},
		{"RunTaskCheck", func(a *App, run store.DelegationRun) (bool, bool) {
			d := a.RunTaskCheck(run.ID, "schema")
			return d.Hidden, d.Status == "" && d.Output == "" && d.Error == "" && d.State == ""
		}},
		{"TaskMergeCommand", func(a *App, run store.DelegationRun) (bool, bool) {
			d := a.TaskMergeCommand(run.ID, "schema")
			return d.Hidden, len(d.Argv) == 0 && d.Display == "" && d.Repo == "" && d.Branch == ""
		}},
		{"StartDelegationRun", func(a *App, _ store.DelegationRun) (bool, bool) {
			// A run creates a worktree and a tmux window named after the
			// client's repo. §14's table refuses NEW Loom-initiated work on a
			// hidden project, and this is the earliest place to refuse it.
			d := a.StartDelegationRun(root, "atlas")
			return d.Hidden, d.RunID == 0 && d.Slug == "" && d.Error == "" && len(d.Bases) == 0
		}},
		{"RefreshDelegationRun", func(a *App, run store.DelegationRun) (bool, bool) {
			d := a.RefreshDelegationRun(run.ID)
			return d.Hidden, len(d.Runs) == 0 && d.Error == ""
		}},
		{"TaskSpawnPreview", func(a *App, run store.DelegationRun) (bool, bool) {
			// The brief names the repo, the authorization and the worktree
			// path. It is the single largest leak surface in this slice.
			d := a.TaskSpawnPreview(run.ID, "schema")
			return d.Hidden, d.Brief == "" && d.Worktree == "" && d.Branch == "" &&
				len(d.CheckArgv) == 0 && d.Error == ""
		}},
		{"DiscardTaskWorktree", func(a *App, run store.DelegationRun) (bool, bool) {
			d := a.DiscardTaskWorktree(run.ID, "schema", false)
			return d.Hidden, !d.Removed && d.State == "" && d.Error == ""
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			run := seedDelegationRun(t, a, delegSeed{root: root, state: string(delegate.StateReady)})

			if hidden, _ := tc.call(a, run); hidden {
				t.Fatalf("%s reported hidden while the project is visible", tc.name)
			}

			hideProject(t, a, root)
			hidden, empty := tc.call(a, run)
			if !hidden {
				t.Fatalf("%s crossed the bridge for a hidden project", tc.name)
			}
			if !empty {
				t.Errorf("%s returned payload beside the hidden marker", tc.name)
			}
		})
	}
}

// TestOrchestrationGate_failsClosed is §3.1 rule 1's other half: an
// unattributable, unresolvable or unknown root is treated as hidden. This is
// where the gate differs from projects.go's projectVisible(), which fails OPEN
// on purpose because an empty rail blamed on Loom is the worse failure there.
func TestOrchestrationGate_failsClosed(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	seedDelegationRun(t, a, delegSeed{root: root, state: string(delegate.StateReady)})

	for _, tc := range []struct {
		name string
		root string
	}{
		{"unknown root", filepath.Join(t.TempDir(), "never-registered")},
		{"empty root", ""},
		{"reserved Ungrouped row", store.UngroupedRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.ProjectDelegation(tc.root); !got.Hidden {
				t.Errorf("ProjectDelegation(%q) must fail closed, got %+v", tc.root, got)
			}
			if got := a.ValidateManifests(tc.root); !got.Hidden {
				t.Errorf("ValidateManifests(%q) must fail closed, got %+v", tc.root, got)
			}
		})
	}

	// No resolver at all (no project service) is the same answer: we cannot
	// know what is hidden, and here that means we do not answer.
	bare := newApp(nil, nil, a.st, nil, nil, a.now)
	if got := bare.ProjectDelegation(root); !got.Hidden {
		t.Errorf("no resolver must fail closed, got %+v", got)
	}
}

// TestProjectDelegation_listsRunsAndTasks is the list binding: state chips come
// from the task ROWS, titles/needs/argv from the manifest SNAPSHOT.
func TestProjectDelegation_listsRunsAndTasks(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	run := seedDelegationRun(t, a, delegSeed{root: root, state: string(delegate.StateRunning)})
	if err := a.st.SetTaskFlags(run.ID, "schema",
		delegate.EncodeFlags(delegate.Flags{delegate.FlagDiverged: true}), 12); err != nil {
		t.Fatal(err)
	}

	got := a.ProjectDelegation(root)
	if got.Hidden || got.Error != "" || len(got.Runs) != 1 {
		t.Fatalf("want one run, got %+v", got)
	}
	r := got.Runs[0]
	if r.Slug != "atlas-"+strconv.FormatInt(run.ID, 10) || r.Status != "planning" || r.ManifestError != "" {
		t.Fatalf("run DTO wrong: %+v", r)
	}
	if len(r.Tasks) != 1 {
		t.Fatalf("want one task, got %+v", r.Tasks)
	}
	task := r.Tasks[0]
	switch {
	case task.State != string(delegate.StateRunning):
		t.Errorf("state = %q", task.State)
	case task.Title != "Extract the account schema":
		t.Errorf("title not read from the snapshot: %q", task.Title)
	case len(task.Needs) != 1 || task.Needs[0] != "cfg":
		t.Errorf("needs = %v", task.Needs)
	case len(task.CheckArgv) != 1 || task.CheckArgv[0] != "true":
		t.Errorf("check argv = %v", task.CheckArgv)
	case len(task.Flags) != 1 || task.Flags[0] != string(delegate.FlagDiverged):
		t.Errorf("flags = %v", task.Flags)
	// Derived here, not in the frontend: `running` holds a child and is not
	// terminal, and a second place enumerating those sets is a second place
	// that forgets a state.
	case task.Terminal || !task.HoldsChild:
		t.Errorf("derived state booleans wrong: %+v", task)
	}
}

// TestProjectDelegation_malformedSnapshotIsReported: a snapshot that will not
// decode degrades to a named error beside the tasks it can still list. It must
// not panic and it must not silently render an empty run.
func TestProjectDelegation_malformedSnapshotIsReported(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	if err := a.st.UpsertProject(store.Project{
		Root: root, Name: "Atlas", Origin: "discovered", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	run, err := a.st.InsertDelegationRun("atlas", root, "{not json", "{}", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.st.InsertDelegationTask(store.DelegationTask{
		RunID: run.ID, TaskID: "schema", State: string(delegate.StateReady),
		RepoLabel: "api", UpdatedAt: 10,
	}); err != nil {
		t.Fatal(err)
	}

	got := a.ProjectDelegation(root)
	if len(got.Runs) != 1 {
		t.Fatalf("run dropped over a bad snapshot: %+v", got)
	}
	if got.Runs[0].ManifestError == "" {
		t.Error("a snapshot that will not decode must be reported, not swallowed")
	}
	if len(got.Runs[0].Tasks) != 1 {
		t.Errorf("task rows must still list — their state is not in the snapshot: %+v", got.Runs[0])
	}
}

// TestValidateManifests is delegation §2's GUI-side validate affordance: a bad
// file is a reported LoadError with its path and reason, never a panic, and it
// never costs the user the other files.
func TestValidateManifests(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	seedDelegationRun(t, a, delegSeed{root: root, state: string(delegate.StateReady)})

	dir := delegate.ManifestDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	good := `{"manifest":1,"name":"good","project":"Atlas","tasks":[
	  {"id":"schema","title":"t","repo":"` + "api" + `","authorization":"only db/",
	   "check":{"cmd":["true"]}}]}`
	writeTestFile(t, filepath.Join(dir, "good.json"), good)
	writeTestFile(t, filepath.Join(dir, "broken.json"), "{ this is not json")
	// A cycle is the failure §4.5 calls silent: every task sits pending and the
	// run looks like healthy progress. It must be a load ERROR.
	cycle := `{"manifest":1,"name":"cycle","project":"Atlas","tasks":[
	  {"id":"a","repo":"` + "api" + `","authorization":"x","needs":["bb"],
	   "produces":[{"id":"aa","path":"a.txt"}],"check":{"cmd":["true"]}},
	  {"id":"b","repo":"` + "api" + `","authorization":"x","needs":["aa"],
	   "produces":[{"id":"bb","path":"b.txt"}],"check":{"cmd":["true"]}}]}`
	writeTestFile(t, filepath.Join(dir, "cycle.json"), cycle)

	got := a.ValidateManifests(root)
	if got.Hidden || got.Dir != dir {
		t.Fatalf("report dir = %q, want %q (hidden=%v)", got.Dir, dir, got.Hidden)
	}
	if len(got.Manifests) != 1 || got.Manifests[0].Name != "good" {
		t.Fatalf("the valid manifest must survive its bad neighbours: %+v", got.Manifests)
	}
	if len(got.Manifests[0].Tasks) != 1 || got.Manifests[0].Tasks[0] != "schema" {
		t.Errorf("task ids = %v", got.Manifests[0].Tasks)
	}
	paths := map[string]string{}
	for _, e := range got.Errors {
		paths[filepath.Base(e.Path)] = e.Error
	}
	if len(paths) != 2 {
		t.Fatalf("want two reported errors, got %+v", got.Errors)
	}
	if paths["broken.json"] == "" {
		t.Error("malformed JSON must be reported with its path")
	}
	if !strings.Contains(paths["cycle.json"], "cycle") {
		t.Errorf("cycle error must name the cycle: %q", paths["cycle.json"])
	}
}

// TestApproveTask_claimsThenReportsFailure: the CAS is the gate and it precedes
// every side effect, so a launcher that is unavailable leaves the human's
// decision recorded (`approved`) and the failure VISIBLE — never a silent
// no-op, and never a task dragged back to `ready` after the human passed a gate.
func TestApproveTask_claimsThenReportsFailure(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	run := seedDelegationRun(t, a, delegSeed{root: root, state: string(delegate.StateReady)})

	got := a.ApproveTask(run.ID, "schema")
	if got.Hidden {
		t.Fatal("visible project reported hidden")
	}
	if got.Error == "" {
		t.Fatal("an unavailable launcher must be reported, not swallowed")
	}
	row, ok, err := a.st.GetDelegationTask(run.ID, "schema")
	if err != nil || !ok {
		t.Fatalf("task row: %v %v", ok, err)
	}
	if row.State != string(delegate.StateApproved) {
		t.Errorf("state = %q, want approved", row.State)
	}

	// Pressing it again is refused by the same CAS — this is the two-instance
	// property, and the refusal is reported rather than becoming a second spawn.
	if again := a.ApproveTask(run.ID, "schema"); !strings.Contains(again.Error, "not ready") {
		t.Errorf("second approve must be refused by the CAS, got %+v", again)
	}
}

// TestRunTaskCheck is §8: exit 0 is pass and anything else is fail, decided on
// the exit code alone with no output parsing, recorded together with the state
// so no reader can see `verified` beside a stale result.
func TestRunTaskCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		argv      []string
		wantStat  delegate.CheckStatus
		wantState delegate.TaskState
	}{
		{"exit 0 is pass", []string{"true"}, delegate.CheckPass, delegate.StateVerified},
		{"non-zero is fail", []string{"false"}, delegate.CheckFail, delegate.StateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			root := t.TempDir()
			run := seedDelegationRun(t, a, delegSeed{
				root: root, state: string(delegate.StateRunning),
				worktree: t.TempDir(), argv: tc.argv,
			})

			got := a.RunTaskCheck(run.ID, "schema")
			if got.Hidden || got.Error != "" {
				t.Fatalf("check did not run: %+v", got)
			}
			if got.Status != string(tc.wantStat) {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStat)
			}
			if got.State != string(tc.wantState) {
				t.Errorf("reported state = %q, want %q", got.State, tc.wantState)
			}
			row, _, err := a.st.GetDelegationTask(run.ID, "schema")
			if err != nil {
				t.Fatal(err)
			}
			if row.State != string(tc.wantState) || row.CheckStatus != string(tc.wantStat) {
				t.Errorf("row not recorded with the result: %+v", row)
			}
		})
	}
}

// TestRunTaskCheck_noWorktree: a task that has never spawned has nothing for a
// check to be a statement about. It reports and does not run — and does not
// move the state, which would be a certification nobody earned.
func TestRunTaskCheck_noWorktree(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	run := seedDelegationRun(t, a, delegSeed{root: root, state: string(delegate.StateReady)})

	got := a.RunTaskCheck(run.ID, "schema")
	if got.Error == "" {
		t.Error("a task with no worktree must report, not run")
	}
	row, _, _ := a.st.GetDelegationTask(run.ID, "schema")
	if row.State != string(delegate.StateReady) {
		t.Errorf("state moved to %q on a check that never ran", row.State)
	}
}

// TestTaskMergeCommand_isTextAndNeverExecutes is delegation §2's binding line:
// in 3a the merge gate is a HUMAN running git merge; Loom prints the command
// and does not execute it. The assertion is on the repo, not on the return
// value — a mock would only prove that the mock was not called.
//
// It also asserts §10.4: what is merged is the TASK'S OWN BRANCH. The
// integration branch is cumulative, so a gate that merged it would land every
// sibling that verified first, with those siblings' own gates never shown.
func TestTaskMergeCommand_isTextAndNeverExecutes(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	repo := scratchGitRepo(t)
	run := seedDelegationRun(t, a, delegSeed{root: root, repoPath: repo, state: string(delegate.StateVerified)})

	// A branch with a commit the user's branch does not have. If anything
	// executed the command, marker.txt would appear.
	runGit(t, repo, "checkout", "-b", delegate.BranchName(run.Slug, "schema"))
	writeTestFile(t, filepath.Join(repo, "marker.txt"), "from the child")
	runGit(t, repo, "add", "marker.txt")
	runGit(t, repo, "commit", "-m", "child work")
	runGit(t, repo, "checkout", "-")

	got := a.TaskMergeCommand(run.ID, "schema")
	if got.Hidden || got.Error != "" {
		t.Fatalf("merge command not produced: %+v", got)
	}
	want := []string{"git", "-C", repo, "merge", "--no-ff", "-m",
		"loom: merge " + run.Slug + "/schema", delegate.BranchName(run.Slug, "schema")}
	if strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %v, want %v", got.Argv, want)
	}
	if got.Display != strings.Join(want, " ") {
		t.Errorf("display = %q", got.Display)
	}
	if _, err := os.Stat(filepath.Join(repo, "marker.txt")); err == nil {
		t.Fatal("the merge was EXECUTED — 3a prints the command and never runs it")
	}
	if head := runGitOut(t, repo, "rev-list", "--count", "HEAD"); head != "1" {
		t.Fatalf("the user's branch moved: %s commits, want 1", head)
	}
}

// TestTaskMergeCommand_warnsOnRedCheck: never blocking, always said. A red
// merge you can explain is fine; one nobody was told about is not — and Loom
// is not the one running this, so saying so is the only thing it can do.
func TestTaskMergeCommand_warnsOnRedCheck(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	repo := scratchGitRepo(t)
	run := seedDelegationRun(t, a, delegSeed{root: root, repoPath: repo, state: string(delegate.StateFailed)})

	got := a.TaskMergeCommand(run.ID, "schema")
	if len(got.Argv) == 0 {
		t.Fatalf("the command must still be offered: %+v", got)
	}
	if len(got.Warnings) == 0 || !strings.Contains(got.Warnings[0], "never run") {
		t.Errorf("an uncertified task must warn: %+v", got.Warnings)
	}
}

// --- helpers --------------------------------------------------------------

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	// The parent is created here so a caller can write a nested path without
	// each one repeating a MkdirAll. Divergence fixtures write "http/router.go"
	// into a fresh scratch repo, and a missing directory would fail as an
	// unrelated I/O error rather than as the case under test.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// scratchRepo builds a throwaway git repo under t.TempDir(). Never the loom
// repo: these tests create branches and would otherwise mutate the tree they
// are running in.
func scratchGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "test")
	writeTestFile(t, filepath.Join(dir, "README.md"), "scratch")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// The spawner's ~/.loom used to be derived as filepath.Dir(a.settings.path),
// which is "" whenever the settings store is absent — so a real ApproveTask
// degraded to "delegation unavailable" for a reason that had nothing to do with
// delegation. It is App.loomDir now, handed over from cfg.LoomDir in main.go.
//
// The assertion is on the layout the spawner was built with, not on a launched
// child: a launch needs tmux, and what broke was the path derivation.
func TestSpawner_usesLoomDirNotTheSettingsPath(t *testing.T) {
	a := newTestApp(t)
	a.launcher = &session.Launcher{}
	a.loomDir = filepath.Join(t.TempDir(), ".loom")
	// settings is deliberately left nil: the old derivation would have failed
	// exactly here, and that is the regression this pins.
	a.settings = nil

	sp, err := a.spawner()
	if err != nil {
		t.Fatalf("spawner unavailable with loomDir set: %v", err)
	}
	want := delegate.NewLayout(a.loomDir)
	if got := sp.Worktrees.Layout.Root; got != want.Root {
		t.Errorf("layout root = %q, want %q", got, want.Root)
	}
}

// The other half: an unset ~/.loom degrades VISIBLY. Rooting the layout at "."
// would scatter worktrees through whatever directory the app was started from,
// which is worse than a refusal a human can read.
func TestSpawner_unsetLoomDirIsAVisibleRefusal(t *testing.T) {
	a := newTestApp(t)
	a.launcher = &session.Launcher{}
	a.loomDir = ""

	if _, err := a.spawner(); err == nil {
		t.Fatal("spawner built a layout with no ~/.loom")
	} else if !strings.Contains(err.Error(), "~/.loom is unknown") {
		t.Errorf("refusal must name the cause, got %v", err)
	}
}

// §8.2's debounce compares the recorded head against the worktree's current
// one, so RunTaskCheck must record the sha the CHECK reported — not one it
// re-derives afterwards, and not the row's stale value.
func TestRunTaskCheck_recordsTheBranchHeadTheCheckRanAgainst(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	wt := scratchGitRepo(t)
	run := seedDelegationRun(t, a, delegSeed{
		root: root, state: string(delegate.StateRunning), worktree: wt, argv: []string{"true"},
	})

	got := a.RunTaskCheck(run.ID, "schema")
	if got.Error != "" {
		t.Fatalf("check did not run: %+v", got)
	}
	head := strings.TrimSpace(runGitOut(t, wt, "rev-parse", "HEAD"))
	row, _, err := a.st.GetDelegationTask(run.ID, "schema")
	if err != nil {
		t.Fatal(err)
	}
	if row.BranchHead != head {
		t.Errorf("branch_head = %q, want %q — §8.2's debounce re-runs forever without it",
			row.BranchHead, head)
	}
}

// An unreadable head (a worktree that is not a repo, which is what a task looks
// like before its child commits) must not ERASE the previously recorded one:
// "" reads back as "never checked" and re-fires the check on every poll tick.
func TestRunTaskCheck_unreadableHeadDoesNotClobber(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	run := seedDelegationRun(t, a, delegSeed{
		root: root, state: string(delegate.StateRunning), worktree: t.TempDir(), argv: []string{"true"},
	})
	if err := a.st.SetTaskBranchHead(run.ID, "schema", "cafebabe", 11); err != nil {
		t.Fatal(err)
	}

	if got := a.RunTaskCheck(run.ID, "schema"); got.Error != "" {
		t.Fatalf("check did not run: %+v", got)
	}
	row, _, err := a.st.GetDelegationTask(run.ID, "schema")
	if err != nil {
		t.Fatal(err)
	}
	if row.BranchHead != "cafebabe" {
		t.Errorf("branch_head = %q, want the carried-forward %q", row.BranchHead, "cafebabe")
	}
}

// --- failure-mode probes (slice 3a) ---------------------------------------

// RunTaskCheck claims the task with AdvanceTaskCAS(from = the state it just
// read, to = "checking"). That guard is only a guard against a state it did not
// read: `merged` and `abandoned` are terminal (§13.2 puts no outgoing edge on
// either, and delegate.TaskState.Terminal agrees), and reading one of them and
// then CASing out of it moves a finished task back into the live machine.
//
// The consequence is not cosmetic. A `merged` task whose worktree column still
// names a directory (§6.3 keeps the column; only the directory goes) can be
// re-checked, and a green result rewrites `merged` to `verified` — the run
// then offers a merge gate for work that has already landed.
func TestRunTaskCheck_doesNotResurrectATerminalTask(t *testing.T) {
	for _, state := range []delegate.TaskState{delegate.StateMerged, delegate.StateAbandoned} {
		t.Run(string(state), func(t *testing.T) {
			a := newTestApp(t)
			root := t.TempDir()
			run := seedDelegationRun(t, a, delegSeed{
				root: root, state: string(state), worktree: t.TempDir(), argv: []string{"true"},
			})

			got := a.RunTaskCheck(run.ID, "schema")
			row, _, err := a.st.GetDelegationTask(run.ID, "schema")
			if err != nil {
				t.Fatal(err)
			}
			if row.State != string(state) {
				t.Errorf("a check moved a terminal task from %q to %q (reported %+v)",
					state, row.State, got)
			}
		})
	}
}

// §8.2 binds "at most one check in flight per task", and the claim is what is
// supposed to enforce it across two Loom instances against one DB (§13).
//
// It does not: the CAS's expected state is whatever the caller read, so an
// instance that reads `checking` CASes checking→checking, which SQLite counts
// as a matched row, and claimed comes back true. Two instances then run the
// same agent-authored argv against the same worktree at the same time — the
// exact double execution §4.3's "the human approved this one argv" argument
// rules out — and whichever finishes last writes the recorded result.
func TestRunTaskCheck_refusesASecondCheckWhileOneIsInFlight(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	// `checking` is the state the other instance's claim already left behind.
	run := seedDelegationRun(t, a, delegSeed{
		root: root, state: string(delegate.StateChecking), worktree: t.TempDir(), argv: []string{"true"},
	})

	got := a.RunTaskCheck(run.ID, "schema")
	if got.Error == "" {
		t.Errorf("a second check was admitted while one was already in flight: %+v", got)
	}
}

// --- the check claim's legal source set (§8.2, §13.2) ---------------------

// The claim is what enforces §8.2's BINDING "at most one check in flight per
// task" and §13.2's terminal states. It must name the states a check is legal
// FROM, in code, rather than CAS from whatever it happened to read a moment
// earlier — a CAS whose expected value is the value just read succeeds against
// every state, including the ones it exists to refuse.
func TestRunTaskCheck_claimsOnlyFromLegalStates(t *testing.T) {
	cases := []struct {
		name      string
		from      delegate.TaskState
		wantAdmit bool
		wantState string
	}{
		{name: "running is the ordinary case", from: delegate.StateRunning,
			wantAdmit: true, wantState: string(delegate.StateVerified)},
		{name: "blocked — a parked child's worktree still holds its work",
			from: delegate.StateBlocked, wantAdmit: true, wantState: string(delegate.StateVerified)},
		{name: "verified — a re-check is a human's right", from: delegate.StateVerified,
			wantAdmit: true, wantState: string(delegate.StateVerified)},
		{name: "failed — refusing a re-run would make a flake permanent",
			from: delegate.StateFailed, wantAdmit: true, wantState: string(delegate.StateVerified)},
		{name: "merged is terminal — a green re-check would offer a merge gate for landed work",
			from: delegate.StateMerged, wantState: string(delegate.StateMerged)},
		{name: "abandoned is terminal", from: delegate.StateAbandoned,
			wantState: string(delegate.StateAbandoned)},
		{name: "checking is already in flight — two Looms must not run one argv twice",
			from: delegate.StateChecking, wantState: string(delegate.StateChecking)},
		{name: "spawning has no child work to be a statement about",
			from: delegate.StateSpawning, wantState: string(delegate.StateSpawning)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			root := t.TempDir()
			run := seedDelegationRun(t, a, delegSeed{
				root: root, state: string(tc.from), worktree: t.TempDir(), argv: []string{"true"},
			})

			got := a.RunTaskCheck(run.ID, "schema")
			if tc.wantAdmit && got.Error != "" {
				t.Fatalf("check refused from %s: %+v", tc.from, got)
			}
			if !tc.wantAdmit && got.Error == "" {
				t.Fatalf("check admitted from %s: %+v", tc.from, got)
			}
			row, _, err := a.st.GetDelegationTask(run.ID, "schema")
			if err != nil {
				t.Fatal(err)
			}
			if row.State != tc.wantState {
				t.Errorf("state = %q, want %q", row.State, tc.wantState)
			}
		})
	}
}

// `unpublished` and `infra-error` return the task to the state the CLAIM
// MATCHED, not to the state that was read before it. §13.2 has no state for
// "the check made no statement", so the row must end exactly as done as it
// started — and restoring a stale read would undo whatever moved the row in
// between.
func TestRunTaskCheck_noVerdictRestoresTheClaimedState(t *testing.T) {
	for _, from := range []delegate.TaskState{delegate.StateRunning, delegate.StateFailed} {
		t.Run(string(from), func(t *testing.T) {
			a := newTestApp(t)
			root := t.TempDir()
			// A worktree that is not a repo: Published cannot verify anything,
			// so the check short-circuits without running.
			run := seedDelegationRun(t, a, delegSeed{
				root: root, state: string(from), worktree: t.TempDir(),
				argv: []string{"loom-no-such-binary-anywhere"},
			})
			got := a.RunTaskCheck(run.ID, "schema")
			if got.Status != string(delegate.CheckInfraError) {
				t.Fatalf("status = %q, want infra-error: %+v", got.Status, got)
			}
			row, _, err := a.st.GetDelegationTask(run.ID, "schema")
			if err != nil {
				t.Fatal(err)
			}
			if row.State != string(from) {
				t.Errorf("state = %q, want the claimed %q — a check that made no statement "+
					"must leave the task exactly as done as it was", row.State, from)
			}
		})
	}
}

// --- divergence reporting (§12.3.1-2, in 3a per §2) -----------------------

// divergedRepo builds a worktree whose child committed `touched`, and returns
// the repo and its pinned base.
func divergedRepo(t *testing.T, touched ...string) (dir, base string) {
	t.Helper()
	dir = scratchGitRepo(t)
	base = strings.TrimSpace(runGitOut(t, dir, "rev-parse", "HEAD"))
	for _, p := range touched {
		writeTestFile(t, filepath.Join(dir, p), "child wrote this")
		runGit(t, dir, "add", "--", p)
	}
	if len(touched) > 0 {
		runGit(t, dir, "commit", "-q", "-m", "child work")
	}
	return dir, base
}

// §12.3: "divergence is computed on EVERY CHECK RUN". Before this it was a
// primitive with tests and no caller — `delegation_tasks.divergence` had no
// production writer at all, which made `paths` decoration rather than a
// detector.
func TestRunTaskCheck_recordsDivergence(t *testing.T) {
	cases := []struct {
		name         string
		declared     []string
		siblingPaths []string
		touched      []string
		wantOutside  []string
		wantSibling  string
		wantFlag     bool
	}{
		{name: "inside the declared paths records nothing",
			declared: []string{"db/**"}, siblingPaths: []string{"http/**"},
			touched: []string{"db/schema.sql"}},
		{name: "outside the declared paths is recorded and flagged",
			declared: []string{"db/**"}, siblingPaths: []string{"http/**"},
			touched:     []string{"db/schema.sql", "README2.md"},
			wantOutside: []string{"README2.md"}, wantFlag: true},
		{name: "inside a sibling's paths predicts the merge conflict",
			declared: []string{"db/**"}, siblingPaths: []string{"http/**"},
			touched:     []string{"http/router.go"},
			wantOutside: []string{"http/router.go"}, wantSibling: "handlers", wantFlag: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			wt, base := divergedRepo(t, tc.touched...)
			run := seedDelegationRun(t, a, delegSeed{
				root: t.TempDir(), state: string(delegate.StateRunning), worktree: wt,
				argv: []string{"true"}, baseSHA: base,
				paths: tc.declared, siblingPaths: tc.siblingPaths,
			})

			got := a.RunTaskCheck(run.ID, "schema")
			if got.Error != "" || got.DivergenceError != "" {
				t.Fatalf("check: %+v", got)
			}
			if !reflect.DeepEqual(got.Divergence.Outside, orEmpty(tc.wantOutside)) {
				t.Errorf("Outside = %v, want %v", got.Divergence.Outside, tc.wantOutside)
			}
			if tc.wantSibling != "" && len(got.Divergence.Siblings[tc.wantSibling]) == 0 {
				t.Errorf("Siblings = %v, want a hit on %q", got.Divergence.Siblings, tc.wantSibling)
			}
			if got.Divergence.Empty == tc.wantFlag {
				t.Errorf("Empty = %v with wantFlag = %v", got.Divergence.Empty, tc.wantFlag)
			}

			// Persisted, not just returned: §5.2's second acknowledgement gates
			// a human decision, so it has to still be there when they come back.
			row, _, err := a.st.GetDelegationTask(run.ID, "schema")
			if err != nil {
				t.Fatal(err)
			}
			stored := delegate.DecodeDivergence(row.Divergence)
			if !reflect.DeepEqual(orEmpty(stored.Outside), orEmpty(tc.wantOutside)) {
				t.Errorf("stored Outside = %v, want %v", stored.Outside, tc.wantOutside)
			}
			if flagged := delegate.DecodeFlags(row.Flags)[delegate.FlagDiverged]; flagged != tc.wantFlag {
				t.Errorf("diverged flag = %v, want %v", flagged, tc.wantFlag)
			}
		})
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// The flag is CLEARED when a later check finds nothing. A child that wrote
// outside its paths and then moved the file back has not diverged, and a flag
// that only ever goes on is a flag a human stops reading.
func TestRunTaskCheck_divergenceFlagClears(t *testing.T) {
	a := newTestApp(t)
	wt, base := divergedRepo(t, "stray.txt")
	run := seedDelegationRun(t, a, delegSeed{
		root: t.TempDir(), state: string(delegate.StateRunning), worktree: wt,
		argv: []string{"true"}, baseSHA: base, paths: []string{"db/**"},
	})
	if got := a.RunTaskCheck(run.ID, "schema"); got.Divergence.Empty {
		t.Fatalf("expected a divergence on the first check: %+v", got)
	}

	runGit(t, wt, "rm", "-q", "stray.txt")
	runGit(t, wt, "commit", "-q", "-m", "put it back")
	got := a.RunTaskCheck(run.ID, "schema")
	if !got.Divergence.Empty {
		t.Fatalf("divergence survived its own correction: %+v", got.Divergence)
	}
	row, _, err := a.st.GetDelegationTask(run.ID, "schema")
	if err != nil {
		t.Fatal(err)
	}
	if delegate.DecodeFlags(row.Flags)[delegate.FlagDiverged] {
		t.Error("the diverged flag was not cleared")
	}
	if row.Divergence != "" {
		t.Errorf("divergence column = %q, want the empty string (EncodeFlags' rule: an "+
			"untouched row and a cleared row must be byte-identical)", row.Divergence)
	}
}

// A failure to compute must be VISIBLE and must not cost the check result that
// was just recorded, and it must not overwrite a real divergence with an empty
// one. "We could not tell" is not "it is fine".
func TestRunTaskCheck_divergenceFailureIsVisibleAndNonDestructive(t *testing.T) {
	a := newTestApp(t)
	wt, base := divergedRepo(t, "stray.txt")
	run := seedDelegationRun(t, a, delegSeed{
		root: t.TempDir(), state: string(delegate.StateRunning), worktree: wt,
		argv: []string{"true"}, baseSHA: base, paths: []string{"db/**"},
	})
	if got := a.RunTaskCheck(run.ID, "schema"); got.Divergence.Empty {
		t.Fatalf("expected a divergence on the first check: %+v", got)
	}

	// Take the repo away. The check itself is a `true` in a directory that still
	// exists, so it is the CAPTURE that fails and nothing else — which is the
	// point: a broken detector must not take a valid check result with it.
	// `verified` is a legal source for a second check, so no state surgery is
	// needed to get here.
	if err := os.RemoveAll(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}

	got := a.RunTaskCheck(run.ID, "schema")
	if got.Status != string(delegate.CheckPass) {
		t.Errorf("check status = %q, want pass — a divergence failure must not cost the "+
			"check result", got.Status)
	}
	if got.DivergenceError == "" {
		t.Error("a divergence that could not be computed was reported as no divergence")
	}
	if !reflect.DeepEqual(got.Divergence.Outside, []string{"stray.txt"}) {
		t.Errorf("Outside = %v, want the last known %v — a failed capture must not "+
			"overwrite a real divergence with an empty one", got.Divergence.Outside,
			[]string{"stray.txt"})
	}
	after, _, err := a.st.GetDelegationTask(run.ID, "schema")
	if err != nil {
		t.Fatal(err)
	}
	if after.Divergence == "" {
		t.Error("the stored divergence was erased by a capture that failed")
	}
}

// §12.3: "and again IMMEDIATELY BEFORE EVERY MERGE — before, because a
// divergence discovered after a merge is a fact, not a gate." The human at this
// gate is the one running the merge, so this is their last chance to see it.
func TestTaskMergeCommand_recomputesAndWarnsOnDivergence(t *testing.T) {
	a := newTestApp(t)
	wt, base := divergedRepo(t)
	repo := scratchGitRepo(t)
	run := seedDelegationRun(t, a, delegSeed{
		root: t.TempDir(), repoPath: repo, state: string(delegate.StateVerified),
		worktree: wt, baseSHA: base, paths: []string{"db/**"}, siblingPaths: []string{"http/**"},
	})
	if got := a.TaskMergeCommand(run.ID, "schema"); !got.Divergence.Empty {
		t.Fatalf("a task that touched nothing must not warn: %+v", got.Divergence)
	}

	// The child commits after the last check ran. The gate must see it.
	writeTestFile(t, filepath.Join(wt, "http", "router.go"), "package http")
	runGit(t, wt, "add", "--", "http/router.go")
	runGit(t, wt, "commit", "-q", "-m", "late work")

	got := a.TaskMergeCommand(run.ID, "schema")
	if got.Divergence.Empty {
		t.Fatal("the merge gate read a stale divergence instead of recomputing it")
	}
	joined := strings.Join(got.Warnings, "\n")
	if !strings.Contains(joined, "http/router.go") {
		t.Errorf("warnings do not name the file: %q", joined)
	}
	if !strings.Contains(joined, "handlers") {
		t.Errorf("warnings do not name the sibling task whose paths were hit: %q", joined)
	}
	// Loom does not run this merge (§2), so a divergence WARNS and never blocks.
	if len(got.Argv) == 0 || got.Error != "" {
		t.Errorf("the merge command was withheld: %+v", got)
	}
	row, _, err := a.st.GetDelegationTask(run.ID, "schema")
	if err != nil {
		t.Fatal(err)
	}
	if !delegate.DecodeFlags(row.Flags)[delegate.FlagDiverged] {
		t.Error("the merge-time recompute did not persist its finding")
	}
}

// The task list renders the LAST RECORDED divergence and does not recompute:
// this DTO is built for every task on every poll, and shelling out to git per
// task per tick is how a list view becomes a load average.
func TestProjectDelegation_taskCarriesTheRecordedDivergence(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	run := seedDelegationRun(t, a, delegSeed{
		root: root, state: string(delegate.StateRunning), worktree: t.TempDir(),
	})
	if err := a.st.SetTaskDivergence(run.ID, "schema",
		`{"outside":["README.md"],"siblings":{"handlers":["http/x.go"]}}`, 12); err != nil {
		t.Fatal(err)
	}
	got := a.ProjectDelegation(root)
	if len(got.Runs) != 1 || len(got.Runs[0].Tasks) != 1 {
		t.Fatalf("unexpected shape: %+v", got)
	}
	d := got.Runs[0].Tasks[0].Divergence
	if !reflect.DeepEqual(d.Outside, []string{"README.md"}) || d.Empty {
		t.Errorf("Divergence = %+v, want the recorded value", d)
	}
	if !reflect.DeepEqual(d.Siblings["handlers"], []string{"http/x.go"}) {
		t.Errorf("Siblings = %v", d.Siblings)
	}
}

// --- creating a run, and the scheduler (§9.1) ------------------------------

// seedProjectWithManifest registers a project whose one repo is a real scratch
// git repo, writes `body` as <root>/.loom/manifests/<name>.json, and returns the
// root and the repo path.
func seedProjectWithManifest(t *testing.T, a *App, name, body string) (root, repo string) {
	t.Helper()
	root = t.TempDir()
	repo = scratchGitRepo(t)
	if err := a.st.UpsertProject(store.Project{
		Root: root, Name: "Atlas", Origin: "discovered", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.st.UpsertProjectRepo(store.ProjectRepo{
		Path: repo, ProjectRoot: root, Label: "api", AddedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(delegate.ManifestDir(root), name+".json"), body)
	// A throwaway ~/.loom. Worktrees are Loom's own state and live under it
	// (never in the user's repo), so the layout has to be rooted somewhere the
	// test owns.
	a.loomDir = t.TempDir()
	return root, repo
}

// A two-task manifest whose second task needs the first's artifact. This is the
// shape §9.1 exists for, and the shape a run creation has to get right: one task
// ready immediately, one waiting on an edge.
const twoTaskManifest = `{"manifest":1,"name":"atlas","project":"Atlas","tasks":[
  {"id":"schema","title":"Schema","repo":"api","authorization":"only db/",
   "paths":["db/**"],"produces":[{"id":"schema.sql","path":"db/schema.sql"}],
   "check":{"cmd":["true"]}},
  {"id":"handlers","title":"Handlers","repo":"api","authorization":"only http/",
   "paths":["http/**"],"needs":["schema.sql"],"check":{"cmd":["true"]}}]}`

// §2 puts "spawn from an approved task" in 3a and says 3a is "built AND RUN on
// one real initiative". Before this binding existed nothing in production wrote
// a `delegation_runs` row, so the whole arc was reachable only from a
// hand-seeded database and the kill criterion could not be measured.
func TestStartDelegationRun(t *testing.T) {
	a := newTestApp(t)
	root, repo := seedProjectWithManifest(t, a, "atlas", twoTaskManifest)
	head := strings.TrimSpace(runGitOut(t, repo, "rev-parse", "HEAD"))

	got := a.StartDelegationRun(root, "atlas")
	if got.Error != "" || got.RunID == 0 {
		t.Fatalf("StartDelegationRun: %+v", got)
	}
	if got.Slug != "atlas-1" {
		t.Errorf("slug = %q, want atlas-1 — it is the worktree and branch component", got.Slug)
	}
	// §6.2 step 1: pinned per REPO ON THE RUN, once, so every child branches
	// from the same commit.
	if got.Bases["api"] != head {
		t.Errorf("pinned base = %q, want HEAD %q", got.Bases["api"], head)
	}
	// §9.1: a task with no needs is ready as soon as the run is created; one
	// with an unmet edge is not, and BOTH halves of the predicate are required
	// so an empty artifact table cannot wave it through.
	if !reflect.DeepEqual(got.Ready, []string{"schema"}) {
		t.Errorf("Ready = %v, want [schema]", got.Ready)
	}
	rows, err := a.st.ListDelegationTasks(got.RunID)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, r := range rows {
		states[r.TaskID] = r.State
	}
	if states["schema"] != string(delegate.StateReady) ||
		states["handlers"] != string(delegate.StatePending) {
		t.Errorf("states = %v, want schema ready and handlers pending", states)
	}

	run, _, err := a.st.GetDelegationRun(got.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "running" {
		t.Errorf("run status = %q, want running", run.Status)
	}
	if run.ProjectRoot != root {
		t.Errorf("run project root = %q, want %q — §14.1's attribution keys on it", run.ProjectRoot, root)
	}
	// The snapshot is what the run replays from (workflow_runs.def_json's
	// precedent), so an edited or deleted manifest must not change a live run.
	if !strings.Contains(run.ManifestJSON, `"handlers"`) {
		t.Errorf("manifest snapshot does not carry the tasks: %q", run.ManifestJSON)
	}
}

// A run creation is an ACT, and the manifest is agent-authored. Every refusal
// below leaves no run behind — a half-created run is a set of task rows with
// nothing that can ever advance them.
func TestStartDelegationRun_refusals(t *testing.T) {
	cases := []struct {
		name      string
		manifest  string
		file      string
		ask       string
		wantInErr string
	}{
		{name: "a manifest that did not load names its reason, not 'no such file'",
			file: "atlas", ask: "atlas", manifest: "{ this is not json", wantInErr: "did not load"},
		{name: "a name that matches nothing", file: "atlas", ask: "other",
			manifest: twoTaskManifest, wantInErr: "no valid manifest"},
		{name: "a dependency cycle is a load error, not a run",
			file: "atlas", ask: "atlas", wantInErr: "did not load",
			manifest: `{"manifest":1,"name":"atlas","project":"Atlas","tasks":[
			  {"id":"a","repo":"api","authorization":"x","needs":["bb"],
			   "produces":[{"id":"aa","path":"a.txt"}],"check":{"cmd":["true"]}},
			  {"id":"b","repo":"api","authorization":"x","needs":["aa"],
			   "produces":[{"id":"bb","path":"b.txt"}],"check":{"cmd":["true"]}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			root, _ := seedProjectWithManifest(t, a, tc.file, tc.manifest)

			got := a.StartDelegationRun(root, tc.ask)
			if !strings.Contains(got.Error, tc.wantInErr) {
				t.Fatalf("error = %q, want it to mention %q", got.Error, tc.wantInErr)
			}
			if got.RunID != 0 {
				t.Errorf("a refused creation left run %d behind", got.RunID)
			}
			runs, err := a.st.ListDelegationRuns(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 0 {
				t.Errorf("%d runs exist after a refusal", len(runs))
			}
		})
	}
}

// A repo with no commits cannot be pinned, and §6.2 refuses AT CREATION rather
// than at the first approve: `git worktree add` would fail with an empty base
// anyway, and the failure belongs next to the gesture that caused it.
func TestStartDelegationRun_unpinnableRepoRefusesTheWholeRun(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main") // no commits at all
	if err := a.st.UpsertProject(store.Project{
		Root: root, Name: "Atlas", Origin: "discovered", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.st.UpsertProjectRepo(store.ProjectRepo{
		Path: repo, ProjectRoot: root, Label: "api", AddedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(delegate.ManifestDir(root), "atlas.json"), twoTaskManifest)

	got := a.StartDelegationRun(root, "atlas")
	if !strings.Contains(got.Error, "pin a base commit") {
		t.Fatalf("error = %q, want a pinning refusal", got.Error)
	}
	if got.RunID != 0 {
		t.Errorf("a run was created against an unpinnable repo")
	}
}

// §9.1 is BOTH halves: the producer verified AND the artifact committed on its
// branch. This walks the whole loop through the real bindings — publish, check,
// and the consumer becomes ready — because that loop is what 3a's M2 and M3
// measurements are taken over, and it had no production path at all.
func TestRefreshReady_bothHalvesOfTheEdge(t *testing.T) {
	a := newTestApp(t)
	root, _ := seedProjectWithManifest(t, a, "atlas", twoTaskManifest)
	started := a.StartDelegationRun(root, "atlas")
	if started.Error != "" {
		t.Fatal(started.Error)
	}

	// Give `schema` a worktree that has published its artifact, and put it in a
	// state a check can be claimed from.
	wt := scratchGitRepo(t)
	writeTestFile(t, filepath.Join(wt, "db", "schema.sql"), "create table t;")
	runGit(t, wt, "add", "--", "db/schema.sql")
	runGit(t, wt, "commit", "-q", "-m", "publish")
	// Driven through the REAL claim sequence (§13.3) rather than by rewriting
	// the row: the worktree column is written BY the claim, and a test that set
	// it directly would not notice if that ever stopped being true.
	mustClaim(t, func() (bool, error) {
		return a.st.AdvanceTaskCAS(started.RunID, "schema",
			string(delegate.StateReady), string(delegate.StateApproved), 20)
	})
	mustClaim(t, func() (bool, error) {
		return a.st.ClaimTaskSpawnCAS(started.RunID, "schema", wt,
			delegate.BranchName("atlas-1", "schema"), started.Bases["api"], "",
			delegate.Concurrency3a, 20)
	})
	mustClaim(t, func() (bool, error) {
		return a.st.BindTaskSessionCAS(started.RunID, "schema", "loom-child", 20)
	})

	// Before the check: the artifact is committed but the producer is not
	// verified, so the consumer must stay pending. Producer-verified-without-
	// the-artifact and artifact-without-verification are both unready.
	if got := a.RefreshDelegationRun(started.RunID); taskState(t, got, "handlers") != string(delegate.StatePending) {
		t.Fatalf("handlers moved before its producer was verified")
	}

	res := a.RunTaskCheck(started.RunID, "schema")
	if res.Status != string(delegate.CheckPass) {
		t.Fatalf("check: %+v", res)
	}
	arts, err := a.st.ListDelegationArtifacts(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || arts[0].ArtifactID != "schema.sql" {
		t.Fatalf("artifacts = %+v, want the published schema.sql", arts)
	}
	if arts[0].CommitSHA == "" {
		t.Error("the publication was recorded with no commit — §10.5's alarm has nothing to compare")
	}

	after := a.RefreshDelegationRun(started.RunID)
	if got := taskState(t, after, "handlers"); got != string(delegate.StateReady) {
		t.Errorf("handlers = %q after its producer verified and published, want ready", got)
	}
}

func taskState(t *testing.T, d DelegationDTO, taskID string) string {
	t.Helper()
	if len(d.Runs) != 1 {
		t.Fatalf("want exactly one run, got %+v", d)
	}
	for _, task := range d.Runs[0].Tasks {
		if task.TaskID == taskID {
			return task.State
		}
	}
	t.Fatalf("no task %q in %+v", taskID, d.Runs[0].Tasks)
	return ""
}

// --- §5.1's gate, and §6.3's discard ---------------------------------------

// §5.1 is BINDING and enumerates what the human is deciding about. Every item
// on that list is asserted here, because the gate is the whole review: the
// child is arbitrary code and there is no sandbox.
func TestTaskSpawnPreview_rendersTheBindingList(t *testing.T) {
	a := newTestApp(t)
	manifest := `{"manifest":1,"name":"atlas","project":"Atlas",
	  "repos":{"api":{"seed_files":[".env",".not-ignored"]}},
	  "tasks":[{"id":"schema","title":"Schema","repo":"api",
	    "authorization":"only db/","mode":"bypassPermissions","model":"opus",
	    "produces":[{"id":"schema.sql","path":"db/schema.sql"}],
	    "check":{"cmd":["go","test","./db/"]}}]}`
	root, repo := seedProjectWithManifest(t, a, "atlas", manifest)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), ".env\n")
	writeTestFile(t, filepath.Join(repo, ".env"), "SECRET=1")
	writeTestFile(t, filepath.Join(repo, ".not-ignored"), "tracked-ish")
	runGit(t, repo, "add", "--", ".gitignore")
	runGit(t, repo, "commit", "-q", "-m", "ignore env")
	// Leave the repo dirty so §6.2's disclosure has something to disclose.
	writeTestFile(t, filepath.Join(repo, "README.md"), "edited by the human")

	started := a.StartDelegationRun(root, "atlas")
	if started.Error != "" {
		t.Fatal(started.Error)
	}
	got := a.TaskSpawnPreview(started.RunID, "schema")
	if got.Error != "" {
		t.Fatalf("preview: %+v", got)
	}
	if got.Branch == "" || got.Worktree == "" || got.Base == "" {
		t.Errorf("branch/worktree/base must all be shown: %+v", got)
	}
	if !strings.Contains(got.Brief, "only db/") {
		t.Error("the brief must carry the authorization VERBATIM — it is what the child reads")
	}
	if !reflect.DeepEqual(got.CheckArgv, []string{"go", "test", "./db/"}) {
		t.Errorf("CheckArgv = %v, want the argv verbatim", got.CheckArgv)
	}
	if got.Mode != "bypassPermissions" || !got.ModeRisky {
		t.Errorf("bypassPermissions must be flagged: mode=%q risky=%v", got.Mode, got.ModeRisky)
	}
	if got.Model != "opus" {
		t.Errorf("model = %q", got.Model)
	}
	if !reflect.DeepEqual(got.SeedFiles, []string{".env"}) {
		t.Errorf("SeedFiles = %v, want the gitignored .env — the human is being shown that a "+
			"secret is about to be handed to an agent", got.SeedFiles)
	}
	if len(got.SeedRefused) != 1 || !strings.Contains(got.SeedRefused[0], ".not-ignored") {
		t.Errorf("SeedRefused = %v, want the refusal named — a refused seed is a check that "+
			"will fail for a reason unrelated to the child's work", got.SeedRefused)
	}
	if !got.RepoDirty {
		t.Error("a dirty repo must be disclosed: children branch from committed HEAD")
	}
	if got.Cap != delegate.Concurrency3a || got.CapReached {
		t.Errorf("cap counter = %d/%d capReached=%v, want 0/%d",
			got.Running, got.Cap, got.CapReached, delegate.Concurrency3a)
	}

	// Looking must cost NOTHING. The gate is the authoring loop's inner
	// iteration, and a preview that created a worktree would make walking away
	// expensive.
	rows, err := a.st.ListDelegationTasks(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].State != string(delegate.StateReady) || rows[0].Worktree != "" {
		t.Errorf("preview mutated the task row: %+v", rows[0])
	}
}

// §6.3's "discarded by the human" row: the worktree DIRECTORY goes, the BRANCH
// stays. Before this binding existed nothing in production called
// Worktrees.Remove, so every worktree a run created stayed registered in the
// user's repo forever and the only cleanup was learning `git worktree remove`.
func TestDiscardTaskWorktree(t *testing.T) {
	a := newTestApp(t)
	root, repo := seedProjectWithManifest(t, a, "atlas", twoTaskManifest)
	started := a.StartDelegationRun(root, "atlas")
	if started.Error != "" {
		t.Fatal(started.Error)
	}

	// Create the worktree the way a spawn would, through the same object.
	w, err := a.worktrees()
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := a.st.GetDelegationRun(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	created, err := w.Create(delegate.Request{
		RunSlug: run.Slug, TaskID: "schema", RepoLabel: "api", RepoPath: repo,
		Base: started.Bases["api"], Brief: "brief",
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := delegate.BranchName(run.Slug, "schema")

	got := a.DiscardTaskWorktree(started.RunID, "schema", true)
	if got.Error != "" || !got.Removed {
		t.Fatalf("discard: %+v", got)
	}
	if got.State != string(delegate.StateAbandoned) {
		t.Errorf("state = %q, want abandoned", got.State)
	}
	if _, err := os.Stat(created.Dir); !os.IsNotExist(err) {
		t.Errorf("the worktree directory survived the discard: %v", err)
	}
	// BINDING (§6.3): Loom never deletes a branch. It is a few bytes and the
	// only durable record of a discarded attempt — the single irreversible act
	// available in this design.
	if out := runGitOut(t, repo, "branch", "--list", branch); !strings.Contains(out, branch) {
		t.Errorf("the branch was deleted; branches are NEVER deleted by Loom (branch --list: %q)", out)
	}
	// And the user's repo is left needing no manual repair: no stale worktree
	// entry, and the primary tree is clean.
	if out := runGitOut(t, repo, "worktree", "list"); strings.Contains(out, created.Dir) {
		t.Errorf("a stale worktree entry survived: %q", out)
	}
	if out := runGitOut(t, repo, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("the user's repo is dirty after a discard: %q", out)
	}
}

// Without force, a discard refuses while a LIVE session occupies the directory:
// pulling a tree out from under a running claude yields a session that cannot
// write, cannot say why, and leaves the repo's worktree list disagreeing with
// the disk. The row is left untouched so the human still has an action.
func TestDiscardTaskWorktree_refusesAnOccupiedWorktreeWithoutForce(t *testing.T) {
	a := newTestApp(t)
	root, repo := seedProjectWithManifest(t, a, "atlas", twoTaskManifest)
	started := a.StartDelegationRun(root, "atlas")
	if started.Error != "" {
		t.Fatal(started.Error)
	}
	w, err := a.worktrees()
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := a.st.GetDelegationRun(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	created, err := w.Create(delegate.Request{
		RunSlug: run.Slug, TaskID: "schema", RepoLabel: "api", RepoPath: repo,
		Base: started.Bases["api"], Brief: "brief",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.st.Upsert(store.SessionRow{
		Name: "loom-child", Cwd: created.Dir, EndedAt: -1, ExitCode: -1, LastStatus: "running",
	}); err != nil {
		t.Fatal(err)
	}

	got := a.DiscardTaskWorktree(started.RunID, "schema", false)
	if got.Error == "" || got.Removed {
		t.Fatalf("an occupied worktree was discarded without force: %+v", got)
	}
	if _, err := os.Stat(created.Dir); err != nil {
		t.Errorf("the worktree was removed despite the refusal: %v", err)
	}
	if got.State == string(delegate.StateAbandoned) {
		t.Error("the task was abandoned even though its worktree is still there")
	}
}

func mustClaim(t *testing.T, claim func() (bool, error)) {
	t.Helper()
	claimed, err := claim()
	if err != nil || !claimed {
		t.Fatalf("a CAS that must win lost: claimed=%v err=%v", claimed, err)
	}
}
