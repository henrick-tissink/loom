package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

// §10's tests. Every one of them builds its own repo under t.TempDir(), for
// worktree_test.go's reason: `git worktree add` and `git merge` write into
// whatever repo they are pointed at, and a test suite is not allowed to do that
// to the developer's checkout. Nothing in this file names the loom repo.

// ─── fixture ─────────────────────────────────────────────────────────────────

// integrationFixture is one run with one repo, its integration worktree already
// created, and an Integrator wired to a scratch store.
type integrationFixture struct {
	t     *testing.T
	repo  string
	store *store.Store
	i     *Integrator
	run   store.DelegationRun
	m     Manifest
}

// newIntegrationFixture builds the whole thing. checkCmd is the per-repo
// integration check; an empty one declares NO gate, which is the degradation
// §10.2 step 3 warns about rather than defaulting to a pass.
func newIntegrationFixture(t *testing.T, checkCmd []string, bootstrap []string) *integrationFixture {
	t.Helper()
	repo := newScratchRepo(t)
	s := newTestStore(t)

	man := map[string]any{"manifest": 1, "name": "atlas"}
	if len(checkCmd) > 0 {
		man["integration"] = map[string]any{
			"per_repo": map[string]any{"app": map[string]any{"cmd": checkCmd}},
		}
	}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD"))
	bases, _ := json.Marshal(map[string]string{"app": base})

	run, err := s.InsertDelegationRun("atlas", repo, string(raw), string(bases), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AdvanceDelegationRunCAS(run.ID, "planning", "running", 1000); err != nil || !ok {
		t.Fatalf("run → running: %v %v", ok, err)
	}
	run.Status = "running"

	m := Manifest{
		Version:   1,
		Name:      "atlas",
		Repos:     map[string]RepoSetup{"app": {Bootstrap: bootstrap}},
		RepoPaths: map[string]string{"app": repo},
	}
	f := &integrationFixture{
		t: t, repo: repo, store: s, run: run, m: m,
		i: &Integrator{
			Store:  s,
			Layout: Layout{Root: filepath.Join(t.TempDir(), "worktrees")},
			Repos:  map[string]string{"app": repo},
			Now:    func() time.Time { return time.Unix(2000, 0) },
		},
	}
	if _, err := f.i.Ensure(run, m, "app"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return f
}

// task creates a task branch carrying one commit, registers the manifest task
// and inserts the row at `verified` — the state §10.2 triggers from.
func (f *integrationFixture) task(id string, files map[string]string) Task {
	f.t.Helper()
	branch := BranchName(f.run.Slug, id)
	mustGit(f.t, f.repo, "checkout", "-q", "-b", branch, "main")
	for name, body := range files {
		writeFile(f.t, filepath.Join(f.repo, name), body)
		mustGit(f.t, f.repo, "add", name)
	}
	mustGit(f.t, f.repo, "commit", "-qm", "task "+id)
	mustGit(f.t, f.repo, "checkout", "-q", "main")

	tk := Task{ID: id, Repo: "app", Paths: []string{"**"}}
	f.m.Tasks = append(f.m.Tasks, tk)
	err := f.store.InsertDelegationTask(store.DelegationTask{
		RunID: f.run.ID, TaskID: id, State: string(StateVerified),
		RepoLabel: "app", Branch: branch, BaseSHA: strings.TrimSpace(mustGit(f.t, f.repo, "rev-parse", "main")),
		CheckStatus: string(CheckPass), UpdatedAt: 1000,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return tk
}

func (f *integrationFixture) integrationDir() string {
	return f.i.Layout.IntegrationDir(f.run.Slug, "app")
}

// reload re-reads the run row, because §10.2's attribution reads the baseline
// blob off it and the fixture's copy goes stale the moment a pass records one.
func (f *integrationFixture) reload() store.DelegationRun {
	f.t.Helper()
	r, found, err := f.store.GetDelegationRun(f.run.ID)
	if err != nil || !found {
		f.t.Fatalf("reload run: %v %v", found, err)
	}
	f.run = r
	return r
}

func (f *integrationFixture) state(id string) TaskState {
	f.t.Helper()
	row, found, err := f.store.GetDelegationTask(f.run.ID, id)
	if err != nil || !found {
		f.t.Fatalf("get task %s: %v %v", id, found, err)
	}
	return TaskState(row.State)
}

func (f *integrationFixture) row(id string) store.DelegationTask {
	f.t.Helper()
	row, _, err := f.store.GetDelegationTask(f.run.ID, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return row
}

// fakeBlockWriter stands in for the Detector so §10.3's ORDER is assertable
// without a filesystem detector.
type fakeBlockWriter struct {
	calls []Block
	err   error
}

func (w *fakeBlockWriter) Write(runSlug, repoLabel, taskID string, b Block) error {
	w.calls = append(w.calls, b)
	return w.err
}

// ─── §10.1 layout ────────────────────────────────────────────────────────────

func TestIntegrationLayoutNaming(t *testing.T) {
	l := Layout{Root: "/home/u/.loom/worktrees"}
	tests := []struct {
		name, got, want string
	}{
		{"worktree", l.IntegrationDir("atlas-7", "bankenstein"), "/home/u/.loom/worktrees/atlas-7/bankenstein/__integration"},
		{"branch", IntegrationBranch("atlas-7", "bankenstein"), "loom/atlas-7/integration/bankenstein"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
	// The `__integration` leaf cannot collide with a task's directory: task ids
	// are [a-z0-9-] (§4.4 rule 3), so none can start with an underscore. If that
	// charset ever widens, this is the thing that breaks.
	if !taskIDRe.MatchString("a") || taskIDRe.MatchString(integrationLeaf) {
		t.Errorf("a task id could name the integration leaf %q", integrationLeaf)
	}
}

// §10.1: one per repo per run, branched from the SAME pinned base as the
// children, and idempotent because recovery re-derives it from (run, repo).
func TestIntegrationEnsure(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	dir := f.integrationDir()

	base := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "main"))
	if got := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD")); got != base {
		t.Errorf("integration worktree at %s, want the run's pinned base %s", got, base)
	}
	if got := strings.TrimSpace(mustGit(t, dir, "symbolic-ref", "--short", "HEAD")); got != IntegrationBranch(f.run.Slug, "app") {
		t.Errorf("integration worktree on branch %s", got)
	}

	c, err := f.i.Ensure(f.run, f.m, "app")
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if !c.Reused {
		t.Error("a second Ensure did not report reuse; recovery re-runs this")
	}
}

// A restart removes the worktree but never the branch, and re-creating from base
// would silently discard every green staging merge the run has accumulated.
func TestIntegrationEnsureNeverRecreatesTheBranchFromBase(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	dir := f.integrationDir()

	// Something lands on the integration branch, then the worktree goes away.
	writeFile(t, filepath.Join(dir, "staged.txt"), "green work\n")
	mustGit(t, dir, "add", "staged.txt")
	mustGit(t, dir, "commit", "-qm", "staged")
	staged := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD"))
	mustGit(t, f.repo, "worktree", "remove", "--force", dir)

	if _, err := f.i.Ensure(f.run, f.m, "app"); err != nil {
		t.Fatalf("Ensure after the worktree went away: %v", err)
	}
	if got := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD")); got != staged {
		t.Errorf("HEAD = %s, want the staged head %s: the staging area was thrown away", got, staged)
	}
}

// ─── §4.2 the integration block ──────────────────────────────────────────────

func TestIntegrationOf(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr bool
		check   func(*testing.T, IntegrationSpec)
	}{
		{
			name: "absent block is legal and yields nothing",
			json: `{"manifest":1,"name":"atlas"}`,
			check: func(t *testing.T, s IntegrationSpec) {
				if len(s.PerRepo) != 0 || len(s.Cross) != 0 {
					t.Errorf("got %+v, want an empty spec", s)
				}
			},
		},
		{
			name:    "malformed snapshot is an error, never a silent skip",
			json:    `{"manifest":1,`,
			wantErr: true,
		},
		{
			name:    "a per-repo check with no command is refused",
			json:    `{"integration":{"per_repo":{"app":{"cmd":[]}}}}`,
			wantErr: true,
		},
		{
			name:    "a cross check with no repo has no cwd",
			json:    `{"integration":{"cross":[{"id":"contract","cmd":["true"],"needs_repos":["app"]}]}}`,
			wantErr: true,
		},
		{
			name:    "a cross check with no id cannot be named in a failure",
			json:    `{"integration":{"cross":[{"cmd":["true"],"repo":"app"}]}}`,
			wantErr: true,
		},
		{
			name: "timeouts resolve, and the manifest default applies",
			json: `{"defaults":{"check_timeout":"3m"},"integration":{"per_repo":{"app":{"cmd":["true"]}},` +
				`"cross":[{"id":"contract","cmd":["true"],"repo":"app","timeout":"90s"}]}}`,
			check: func(t *testing.T, s IntegrationSpec) {
				if got := s.PerRepo["app"].ResolvedTimeout; got != 3*time.Minute {
					t.Errorf("per-repo timeout = %s, want the manifest default 3m", got)
				}
				if got := s.Cross[0].ResolvedTimeout; got != 90*time.Second {
					t.Errorf("cross timeout = %s, want 90s", got)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := IntegrationOf(tt.json)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want an error; a check the author wrote and Loom silently skipped is the worst outcome here")
				}
				return
			}
			if err != nil {
				t.Fatalf("IntegrationOf: %v", err)
			}
			if tt.check != nil {
				tt.check(t, spec)
			}
		})
	}
}

func TestIntegrationBaselineCodec(t *testing.T) {
	if got := EncodeBaselines(nil); got != "" {
		t.Errorf("empty map encoded as %q, want the empty string (the column default)", got)
	}
	in := map[string]Baseline{"app": {Head: "abc", Status: CheckPass, At: 7, Out: "ok"}}
	round := DecodeBaselines(EncodeBaselines(in))
	if round["app"] != in["app"] {
		t.Errorf("round trip = %+v, want %+v", round["app"], in["app"])
	}
	if got := DecodeBaselines("{not json"); len(got) != 0 {
		t.Errorf("a corrupt column yielded %+v; it must degrade to empty, never error", got)
	}

	// The three-valued baseline: unknown is neither green nor red.
	tests := []struct {
		name               string
		b                  Baseline
		head               string
		wantKnown, wantRed bool
	}{
		{"a verdict about this head", Baseline{Head: "a", Status: CheckPass}, "a", true, false},
		{"a verdict about another head", Baseline{Head: "b", Status: CheckPass}, "a", false, false},
		{"a position with no verdict", Baseline{Head: "a"}, "a", false, false},
		{"red", Baseline{Head: "a", Status: CheckFail}, "a", true, true},
		{"infra-error is not evidence the tree is good", Baseline{Head: "a", Status: CheckInfraError}, "a", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.Known(tt.head); got != tt.wantKnown {
				t.Errorf("Known = %v, want %v", got, tt.wantKnown)
			}
			if got := tt.b.Red(); got != tt.wantRed {
				t.Errorf("Red = %v, want %v", got, tt.wantRed)
			}
		})
	}
}

func TestIntegrationTouchedDependencyManifest(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  bool
	}{
		{"nothing", nil, false},
		{"ordinary source", []string{"src/main.go", "README.md"}, false},
		{"go.mod at the root", []string{"go.mod"}, true},
		{"a nested lockfile", []string{"web/app/package-lock.json"}, true},
		{"a file merely NAMED like one", []string{"docs/go.mod.md"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := touchedDependencyManifest(tt.files); got != tt.want {
				t.Errorf("touchedDependencyManifest(%v) = %v, want %v", tt.files, got, tt.want)
			}
		})
	}
}

// ─── §10.2 the sequence ──────────────────────────────────────────────────────

// Green throughout: the task becomes mergeable, the integration branch KEEPS the
// merge, and the baseline is recorded so the next pass attributes for free.
func TestIntegrateGreenKeepsTheMergeAndPromotes(t *testing.T) {
	f := newIntegrationFixture(t, []string{"sh", "-c", "test ! -f broken.txt"}, nil)
	tk := f.task("schema", map[string]string{"schema.sql": "create table t;\n"})
	pre := strings.TrimSpace(mustGit(t, f.integrationDir(), "rev-parse", "HEAD"))

	res, err := f.i.Integrate(context.Background(), f.run, f.m, tk)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if !res.Green() {
		t.Fatalf("result = %+v, want green", res)
	}
	if res.Pre != pre {
		t.Errorf("Pre = %s, want %s", res.Pre, pre)
	}
	if _, err := os.Stat(filepath.Join(f.integrationDir(), "schema.sql")); err != nil {
		t.Errorf("the integration branch did not keep the merge: %v", err)
	}
	if got := f.state("schema"); got != StateMergeable {
		t.Errorf("state = %s, want mergeable", got)
	}
	b := DecodeBaselines(f.reload().Integration)["app"]
	if b.Head != res.Head || b.Status != CheckPass {
		t.Errorf("baseline = %+v, want a pass at %s", b, res.Head)
	}
}

// A repo with no declared per-repo check has NO gate. That is a real degradation
// and must be rendered as one rather than defaulted to something that passes.
func TestIntegrateWithoutAPerRepoCheckWarnsRatherThanClaimingAGate(t *testing.T) {
	f := newIntegrationFixture(t, nil, nil)
	tk := f.task("schema", map[string]string{"schema.sql": "x\n"})

	res, err := f.i.Integrate(context.Background(), f.run, f.m, tk)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if !res.Green() {
		t.Fatalf("result = %+v, want green (there is nothing to fail)", res)
	}
	if !hasSubstring(res.Warnings, "declares no integration.per_repo check") {
		t.Errorf("Warnings = %v, want the missing-gate degradation named", res.Warnings)
	}
}

// §10.2's two-row attribution table, both rows, plus the reset that every
// failure path ends in.
func TestIntegrateAttributionTable(t *testing.T) {
	tests := []struct {
		name string
		// check is the per-repo integration check.
		check []string
		// recordBaseline, when set, pre-records the previous pass's verdict at
		// `pre` — the cheap path §10.2 describes.
		recordBaseline CheckStatus
		wantBlame      Blame
		wantState      TaskState
		wantRunStatus  string
	}{
		{
			// Red with the task merged, green at `pre` without it. Recorded.
			name:           "red with the task, green at pre, from the record",
			check:          []string{"sh", "-c", "test ! -f broken.txt"},
			recordBaseline: CheckPass,
			wantBlame:      BlameTask,
			wantState:      StateBlocked,
			wantRunStatus:  "running",
		},
		{
			// Same, with NOTHING recorded: the check is re-run at `pre` rather
			// than guessing, because both available defaults are wrong.
			name:          "red with the task, green at pre, re-derived",
			check:         []string{"sh", "-c", "test ! -f broken.txt"},
			wantBlame:     BlameTask,
			wantState:     StateBlocked,
			wantRunStatus: "running",
		},
		{
			// Red in BOTH: a run-level fault. No task is blamed.
			name:           "red at pre too, from the record",
			check:          []string{"false"},
			recordBaseline: CheckFail,
			wantBlame:      BlameBaseline,
			wantState:      StateVerified,
			wantRunStatus:  "deadlocked",
		},
		{
			name:          "red at pre too, re-derived",
			check:         []string{"false"},
			wantBlame:     BlameBaseline,
			wantState:     StateVerified,
			wantRunStatus: "deadlocked",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newIntegrationFixture(t, tt.check, nil)
			blocks := &fakeBlockWriter{}
			f.i.Blocks = blocks
			tk := f.task("schema", map[string]string{"broken.txt": "boom\n"})
			dir := f.integrationDir()
			pre := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD"))

			if tt.recordBaseline != "" {
				if err := f.i.setBaselineFor(f.run.ID, "app", Baseline{Head: pre, Status: tt.recordBaseline, At: 1}); err != nil {
					t.Fatal(err)
				}
				f.reload()
			}

			res, err := f.i.Integrate(context.Background(), f.run, f.m, tk)
			if err != nil {
				t.Fatalf("Integrate: %v", err)
			}
			if res.Blame != tt.wantBlame {
				t.Errorf("Blame = %q, want %q (%s)", res.Blame, tt.wantBlame, res.Output)
			}
			if res.Stage != StagePerRepo {
				t.Errorf("Stage = %q, want per-repo", res.Stage)
			}
			if res.Head != "" {
				t.Errorf("Head = %q on a failure; a failed pass leaves nothing behind", res.Head)
			}
			// EVERY failure path ends at `pre`, with no debris.
			assertTreeAt(t, dir, pre)
			if got := f.state("schema"); got != tt.wantState {
				t.Errorf("state = %s, want %s", got, tt.wantState)
			}
			if got := f.reload().Status; got != tt.wantRunStatus {
				t.Errorf("run status = %s, want %s", got, tt.wantRunStatus)
			}
			// §10.3: a task-blamed failure is sent BACK TO THE CHILD.
			if tt.wantBlame == BlameTask {
				row := f.row("schema")
				if row.PendingSeed == "" {
					t.Error("no pending seed: the child was never told")
				}
				// §11.4's flag means A SEED IS OWED, and this park owes one for
				// longer than any other — the child is mid-task and delivery
				// waits for its next prompt. A column with no flag is a debt
				// nothing names, and §12.2's block-stale retry keys on the flag.
				if !DecodeFlags(row.Flags)[FlagSeedPending] {
					t.Errorf("flags = %q, want %s beside the pending seed", row.Flags, FlagSeedPending)
				}
				if !strings.Contains(row.BlockJSON, `"author":"loom"`) {
					t.Errorf("block declaration = %q, want one Loom authored", row.BlockJSON)
				}
				if len(blocks.calls) != 1 || blocks.calls[0].Author != AuthorLoom {
					t.Errorf("block file writes = %+v, want exactly one Loom-authored declaration", blocks.calls)
				}
			} else if f.row("schema").PendingSeed != "" {
				t.Error("a baseline fault seeded the child: no task is to blame for it")
			}
		})
	}
}

// Step 1's conflict: the merge is aborted, the tree is reset to `pre`, the
// conflicting files are named for §10.3's seed, and nothing is left half-merged.
func TestIntegrateMergeConflictIsResetAndNamed(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	f.i.Blocks = &fakeBlockWriter{}
	dir := f.integrationDir()

	// A commit on the integration branch that the task will disagree with.
	writeFile(t, filepath.Join(dir, "shared.txt"), "integration wins\n")
	mustGit(t, dir, "add", "shared.txt")
	mustGit(t, dir, "commit", "-qm", "a sibling landed here first")
	pre := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD"))

	tk := f.task("schema", map[string]string{"shared.txt": "the task wins\n"})

	res, err := f.i.Integrate(context.Background(), f.run, f.m, tk)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if res.Stage != StageMerge || res.Blame != BlameTask {
		t.Errorf("Stage/Blame = %q/%q, want merge/task", res.Stage, res.Blame)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "shared.txt" {
		t.Errorf("Conflicts = %v, want [shared.txt]: the child is told which files, not that there was a conflict", res.Conflicts)
	}
	assertTreeAt(t, dir, pre)
	if seed := f.row("schema").PendingSeed; !strings.Contains(seed, "shared.txt") {
		t.Errorf("the seed does not name the conflicting file:\n%s", seed)
	}
}

// Step 2: a merge that touches a dependency manifest re-runs bootstrap, and a
// failed bootstrap resets like every other failure path.
func TestIntegrateBootstrapFailureResetsToPre(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, []string{"sh", "-c", "exit 3"})
	f.i.Blocks = &fakeBlockWriter{}
	tk := f.task("schema", map[string]string{"go.mod": "module x\n"})
	dir := f.integrationDir()
	pre := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD"))

	res, err := f.i.Integrate(context.Background(), f.run, f.m, tk)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if res.Stage != StageBootstrap {
		t.Fatalf("Stage = %q, want bootstrap (%s)", res.Stage, res.Output)
	}
	assertTreeAt(t, dir, pre)
	if got := f.state("schema"); got != StateBlocked {
		t.Errorf("state = %s, want blocked", got)
	}
}

// A merge that touches NOTHING dependency-shaped must not pay for bootstrap. The
// test is that a bootstrap which would fail never runs.
func TestIntegrateSkipsBootstrapWhenNoDependencyManifestMoved(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, []string{"sh", "-c", "exit 3"})
	tk := f.task("schema", map[string]string{"schema.sql": "create table t;\n"})

	res, err := f.i.Integrate(context.Background(), f.run, f.m, tk)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if !res.Green() {
		t.Errorf("result = %+v; bootstrap ran for a merge that touched no dependency manifest", res)
	}
}

// The BINDING rule: the integration branch only ever contains work that was
// green END TO END. A red task must not be left in it for the next task to
// integrate on top of.
func TestIntegrateRedTaskIsNotLeftInTheIntegrationBranch(t *testing.T) {
	f := newIntegrationFixture(t, []string{"sh", "-c", "test ! -f broken.txt"}, nil)
	f.i.Blocks = &fakeBlockWriter{}
	dir := f.integrationDir()

	red := f.task("bad", map[string]string{"broken.txt": "boom\n"})
	if _, err := f.i.Integrate(context.Background(), f.run, f.m, red); err != nil {
		t.Fatalf("Integrate(bad): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "broken.txt")); err == nil {
		t.Fatal("the red task's commits are still in the integration branch: every later task now integrates on top of known-broken code")
	}

	// And the NEXT task is green, which is the whole consequence: without the
	// reset its check would be red for reasons that have nothing to do with it,
	// and §5.2's precondition would be unreachable for the rest of the run.
	f.reload()
	good := f.task("schema", map[string]string{"schema.sql": "create table t;\n"})
	res, err := f.i.Integrate(context.Background(), f.run, f.m, good)
	if err != nil {
		t.Fatalf("Integrate(schema): %v", err)
	}
	if !res.Green() {
		t.Errorf("the next task was red for a sibling's reasons: %+v", res)
	}
}

// §10.2's serialization is run-wide and is enforced INSIDE the UPDATE. A second
// task offered while one is `integrating` is refused, and the refusal is
// ErrIntegrationBusy — not an error condition, just next tick.
func TestIntegrateIsSerializedRunWide(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	a := f.task("alpha", map[string]string{"a.txt": "a\n"})
	b := f.task("beta", map[string]string{"b.txt": "b\n"})

	// Put alpha in `integrating` the way a pass would, then offer beta.
	if ok, _, err := f.store.ClaimTaskIntegrationCAS(f.run.ID, a.ID, []string{string(StateVerified)}, 1); err != nil || !ok {
		t.Fatalf("claim alpha: %v %v", ok, err)
	}
	if _, err := f.i.Integrate(context.Background(), f.run, f.m, b); !errors.Is(err, ErrIntegrationBusy) {
		t.Fatalf("Integrate(beta) = %v, want ErrIntegrationBusy", err)
	}
	if got := f.state("beta"); got != StateVerified {
		t.Errorf("beta = %s; a refused claim must leave the row completely untouched", got)
	}
}

// A task that is not `verified` any more is a different refusal with a different
// remedy: the caller drops the step rather than re-offering it.
func TestIntegrateRefusesATaskThatMovedElsewhere(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	tk := f.task("schema", map[string]string{"a.txt": "a\n"})
	if ok, err := f.store.AbandonTaskCAS(f.run.ID, tk.ID, 1); err != nil || !ok {
		t.Fatalf("abandon: %v %v", ok, err)
	}
	if _, err := f.i.Integrate(context.Background(), f.run, f.m, tk); !errors.Is(err, ErrTaskMovedElsewhere) {
		t.Fatalf("Integrate = %v, want ErrTaskMovedElsewhere", err)
	}
}

// ─── §10.2 step 4, cross checks ──────────────────────────────────────────────

// The cross check's environment is the whole reason it is worth writing: cwd is
// c.Repo's integration worktree and every needs_repos entry is exported as
// LOOM_REPO_<LABEL> pointing at ITS integration worktree — the producer's STAGED
// code, not its released code.
func TestIntegrationCrossEnvPointsAtStagedTrees(t *testing.T) {
	i := &Integrator{Layout: Layout{Root: "/root"}}
	run := store.DelegationRun{Slug: "atlas-7"}
	got := i.crossEnv(run, CrossCheck{ID: "contract", Repo: "ballista", Needs: []string{"bankenstein"}})
	want := map[string]string{
		"ballista":    "/root/atlas-7/ballista/__integration",
		"bankenstein": "/root/atlas-7/bankenstein/__integration",
	}
	for label, dir := range want {
		if got[label] != dir {
			t.Errorf("LOOM_REPO_%s = %q, want %q", label, got[label], dir)
		}
	}
}

// A cross check whose needed repo is at a RED per-repo integration is skipped,
// because its failure would be one nobody can attribute — the exact ambiguity
// the Blame table exists to remove. A repo with no declared gate is not red.
func TestIntegrationCrossReady(t *testing.T) {
	spec := IntegrationSpec{PerRepo: map[string]Check{
		"app": {Cmd: []string{"true"}},
		"lib": {Cmd: []string{"true"}},
	}}
	c := CrossCheck{ID: "contract", Repo: "app", Needs: []string{"app", "lib"}}
	tests := []struct {
		name      string
		baselines map[string]Baseline
		spec      IntegrationSpec
		want      bool
	}{
		{"both green", map[string]Baseline{
			"app": {Head: "a", Status: CheckPass}, "lib": {Head: "b", Status: CheckPass}}, spec, true},
		{"one red", map[string]Baseline{
			"app": {Head: "a", Status: CheckPass}, "lib": {Head: "b", Status: CheckFail}}, spec, false},
		{"no verdict yet is not red", map[string]Baseline{"app": {Head: "a"}}, spec, true},
		{"a repo with no declared gate is not red",
			map[string]Baseline{}, IntegrationSpec{PerRepo: map[string]Check{"app": {Cmd: []string{"true"}}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := &Integrator{}
			run := store.DelegationRun{Integration: EncodeBaselines(tt.baselines)}
			if got := i.crossReady(run, tt.spec, c); got != tt.want {
				t.Errorf("crossReady = %v, want %v", got, tt.want)
			}
		})
	}
}

// A red cross check resets like every other failure path and names itself.
func TestIntegrateCrossFailureResetsAndNamesTheCheck(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	f.i.Blocks = &fakeBlockWriter{}
	// Rewrite the snapshot to add a cross check that is red only once the task's
	// file is staged.
	f.run.ManifestJSON = `{"manifest":1,"name":"atlas","integration":{"per_repo":{"app":{"cmd":["true"]}},` +
		`"cross":[{"id":"contract","cmd":["sh","-c","test ! -f schema.sql"],"repo":"app","needs_repos":["app"]}]}}`
	tk := f.task("schema", map[string]string{"schema.sql": "create table t;\n"})
	dir := f.integrationDir()
	pre := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD"))

	res, err := f.i.Integrate(context.Background(), f.run, f.m, tk)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if res.Stage != StageCross || res.CrossCheck != "contract" {
		t.Fatalf("Stage/CrossCheck = %q/%q, want cross/contract", res.Stage, res.CrossCheck)
	}
	if res.Blame != BlameTask {
		t.Errorf("Blame = %q, want task: the cross check is green at pre", res.Blame)
	}
	assertTreeAt(t, dir, pre)
}

// ─── §10.4 the merge ─────────────────────────────────────────────────────────

// BINDING, and the thing revision 1 got wrong: approving B lands B's commits and
// NONE of A's, even though A verified first and is sitting in the cumulative
// integration branch. Merging the integration branch would have the human
// approve diff(B) and Loom land diff(A)+diff(B).
func TestMergeLandsOnlyTheApprovedTask(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	a := f.task("alpha", map[string]string{"a.txt": "alpha\n"})
	b := f.task("beta", map[string]string{"b.txt": "beta\n"})

	// Both verify and both are staged in the integration branch.
	if _, err := f.i.Integrate(context.Background(), f.run, f.m, a); err != nil {
		t.Fatalf("Integrate(alpha): %v", err)
	}
	f.reload()
	if _, err := f.i.Integrate(context.Background(), f.run, f.m, b); err != nil {
		t.Fatalf("Integrate(beta): %v", err)
	}
	f.reload()
	if _, err := os.Stat(filepath.Join(f.integrationDir(), "a.txt")); err != nil {
		t.Fatalf("alpha is not in the integration branch, so this test proves nothing: %v", err)
	}

	// The human approves ONLY beta.
	if _, err := f.i.Merge(context.Background(), f.run, f.m, b, false); err != nil {
		t.Fatalf("Merge(beta): %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "b.txt")); err != nil {
		t.Errorf("beta's work did not land: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "a.txt")); err == nil {
		t.Error("alpha landed on the user's branch: the human approved diff(beta) and Loom landed diff(alpha)+diff(beta)")
	}
	if got := f.state("beta"); got != StateMerged {
		t.Errorf("beta = %s, want merged", got)
	}
	// alpha is untouched: still verified... it was promoted to mergeable by its
	// own green pass and is waiting at its own §5.2 gate.
	if got := f.state("alpha"); got != StateMergeable {
		t.Errorf("alpha = %s, want mergeable: approving beta must not consume alpha's gate", got)
	}
}

// §10.4 step 2: after the merge the integration worktree is re-derived from the
// USER'S BRANCH HEAD and the per-repo check re-runs there, so every subsequent
// task stages on top of what actually shipped.
func TestMergeRederivesTheIntegrationWorktree(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	tk := f.task("schema", map[string]string{"schema.sql": "create table t;\n"})
	if _, err := f.i.Integrate(context.Background(), f.run, f.m, tk); err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	f.reload()

	// The user commits something of their own first, so "the user's branch head"
	// is provably not "the integration branch head".
	writeFile(t, filepath.Join(f.repo, "user.txt"), "mine\n")
	mustGit(t, f.repo, "add", "user.txt")
	mustGit(t, f.repo, "commit", "-qm", "the human's own work")

	res, err := f.i.Merge(context.Background(), f.run, f.m, tk, false)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	head := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))
	if got := strings.TrimSpace(mustGit(t, f.integrationDir(), "rev-parse", "HEAD")); got != head {
		t.Errorf("integration worktree at %s, want the user's branch head %s", got, head)
	}
	if res.Head != head || res.Status != CheckPass {
		t.Errorf("result = %+v, want the re-run recorded at %s", res, head)
	}
	if b := DecodeBaselines(f.reload().Integration)["app"]; b.Head != head || b.Status != CheckPass {
		t.Errorf("baseline = %+v, want a pass at the user's branch head", b)
	}
	// And the disclosure holds: the user's branch carries the human's own commit
	// too, so it is NOT the tree the pre-merge check certified. Step 2 is what
	// bounds that gap, and it just ran.
	if _, err := os.Stat(filepath.Join(f.integrationDir(), "user.txt")); err != nil {
		t.Errorf("the staging area does not reflect what actually shipped: %v", err)
	}
}

// A red re-run after the merge is a BASELINE fault — the user's own branch is
// red — and no task is blamed for it. In particular the task that just merged is
// not sent back for the state of a branch it has only just joined.
func TestMergeRederivationRedIsABaselineFault(t *testing.T) {
	f := newIntegrationFixture(t, []string{"sh", "-c", "test ! -f poison.txt"}, nil)
	tk := f.task("schema", map[string]string{"schema.sql": "create table t;\n"})
	if _, err := f.i.Integrate(context.Background(), f.run, f.m, tk); err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	f.reload()

	// The human poisons their own branch between the gate and the press.
	writeFile(t, filepath.Join(f.repo, "poison.txt"), "boom\n")
	mustGit(t, f.repo, "add", "poison.txt")
	mustGit(t, f.repo, "commit", "-qm", "the human breaks their own branch")

	res, err := f.i.Merge(context.Background(), f.run, f.m, tk, false)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Blame != BlameBaseline {
		t.Errorf("Blame = %q, want baseline: the user's own branch is red", res.Blame)
	}
	if got := f.state("schema"); got != StateMerged {
		t.Errorf("state = %s, want merged: the merge happened and no task is blamed for the baseline", got)
	}
}

// Merging into a dirty tree is how a human loses work to a machine. Refused with
// the offending files named — never stashed, never forced.
func TestMergeRefusesADirtyTarget(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	tk := f.task("schema", map[string]string{"schema.sql": "x\n"})
	if _, err := f.i.Integrate(context.Background(), f.run, f.m, tk); err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	f.reload()
	writeFile(t, filepath.Join(f.repo, "README.md"), "the human is mid-edit\n")

	for _, force := range []bool{false, true} {
		_, err := f.i.Merge(context.Background(), f.run, f.m, tk, force)
		if !errors.Is(err, ErrDirtyTarget) {
			t.Fatalf("Merge(force=%v) = %v, want ErrDirtyTarget", force, err)
		}
		if !strings.Contains(err.Error(), "README.md") {
			t.Errorf("the refusal does not name the offending file: %v", err)
		}
	}
	if got := f.state("schema"); got != StateMergeable {
		t.Errorf("state = %s; a refused merge must leave the row untouched", got)
	}
}

// A task that is not mergeable is not merged. The gate is the state, and force
// is §5.2's "past an unacknowledged divergence" — never past a missing gate
// silently.
func TestMergeRefusesATaskThatIsNotMergeable(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	tk := f.task("schema", map[string]string{"schema.sql": "x\n"})
	_, err := f.i.Merge(context.Background(), f.run, f.m, tk, false)
	if !errors.Is(err, ErrTaskMovedElsewhere) {
		t.Fatalf("Merge = %v, want a refusal naming the state", err)
	}
	if _, statErr := os.Stat(filepath.Join(f.repo, "schema.sql")); statErr == nil {
		t.Error("the merge happened anyway")
	}
}

// §5.2's `forced` flag is written for the RECORD. It is never read as permission
// by anything, which is why the only assertion here is that it is stored.
func TestMergeRecordsTheForcedFlag(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	tk := f.task("schema", map[string]string{"schema.sql": "x\n"})
	if _, err := f.i.Integrate(context.Background(), f.run, f.m, tk); err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	f.reload()
	if _, err := f.i.Merge(context.Background(), f.run, f.m, tk, true); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !DecodeFlags(f.row("schema").Flags)[FlagForced] {
		t.Error("the forced flag was not recorded")
	}
}

// ─── §10.5 the stale-contract alarm ──────────────────────────────────────────

func TestStaleContract(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	f.m.Tasks = append(f.m.Tasks, Task{
		ID: "schema", Repo: "app",
		Produces: []Artifact{{ID: "account-schema", Kind: "interface", Path: "schema.sql"}},
	})
	consumer := Task{ID: "api", Repo: "app", Needs: []string{"account-schema"}}
	f.m.Tasks = append(f.m.Tasks, consumer)

	err := f.store.InsertDelegationTask(store.DelegationTask{
		RunID: f.run.ID, TaskID: "api", State: string(StateVerified), RepoLabel: "app",
		NeedsSnapshot: EncodeNeedsBaselines(map[string]NeedsBaseline{
			"account-schema": {Fingerprint: "sha:aaa", Commit: "c1"},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	publish := func(fp, commit string) {
		t.Helper()
		if err := f.store.UpsertDelegationArtifact(store.DelegationArtifact{
			RunID: f.run.ID, ArtifactID: "account-schema", TaskID: "schema",
			Path: "schema.sql", Fingerprint: fp, CommitSHA: commit, PublishedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	publish("sha:aaa", "c1")
	if drifts, err := f.i.StaleContract(f.run, f.m, consumer); err != nil || len(drifts) != 0 {
		t.Fatalf("unchanged fingerprint reported drift: %+v %v", drifts, err)
	}

	// The producer is sent back by §10.3 and revises the interface. This is the
	// single most common cross-repo break, and the ONLY one Loom can see without
	// a cross-repo test.
	publish("sha:bbb", "c2")
	drifts, err := f.i.StaleContract(f.run, f.m, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if len(drifts) != 1 {
		t.Fatalf("drifts = %+v, want one", drifts)
	}
	d := drifts[0]
	if d.Artifact != "account-schema" || d.WasCommit != "c1" || d.NowCommit != "c2" {
		t.Errorf("drift = %+v, want both commits carried so `git log c1..c2` is available", d)
	}
}

// A `data` artifact changing is the normal course of a run. Firing on it would
// make the alarm the thing everyone clicks through.
func TestStaleContractIgnoresNonInterfaceArtifacts(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	f.m.Tasks = append(f.m.Tasks,
		Task{ID: "schema", Repo: "app", Produces: []Artifact{{ID: "rows", Kind: "data", Path: "rows.csv"}}})
	consumer := Task{ID: "api", Repo: "app", Needs: []string{"rows"}}

	if err := f.store.InsertDelegationTask(store.DelegationTask{
		RunID: f.run.ID, TaskID: "api", State: string(StateVerified), RepoLabel: "app",
		NeedsSnapshot: EncodeNeedsBaselines(map[string]NeedsBaseline{"rows": {Fingerprint: "a", Commit: "c1"}}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpsertDelegationArtifact(store.DelegationArtifact{
		RunID: f.run.ID, ArtifactID: "rows", TaskID: "schema", Fingerprint: "b", CommitSHA: "c2",
	}); err != nil {
		t.Fatal(err)
	}
	drifts, err := f.i.StaleContract(f.run, f.m, consumer)
	if err != nil || len(drifts) != 0 {
		t.Errorf("drifts = %+v (%v), want none: only `interface` artifacts carry a fingerprint contract", drifts, err)
	}
}

// No recorded baseline is an ABSENCE OF EVIDENCE. It yields no finding — the
// alarm compares strings and "we never wrote it down" is not a change — and it
// is surfaced as a warning rather than presented as evidence of absence.
func TestStaleContractWithNoBaselineIsAnAbsenceOfEvidence(t *testing.T) {
	m := Manifest{Tasks: []Task{
		{ID: "schema", Produces: []Artifact{{ID: "iface", Kind: "interface"}}},
	}}
	consumer := Task{ID: "api", Needs: []string{"iface"}}
	got := needsWithoutBaseline(m, consumer, store.DelegationTask{})
	if len(got) != 1 || got[0] != "iface" {
		t.Errorf("needsWithoutBaseline = %v, want [iface]", got)
	}
	withBaseline := store.DelegationTask{
		NeedsSnapshot: EncodeNeedsBaselines(map[string]NeedsBaseline{"iface": {Fingerprint: "x"}}),
	}
	if got := needsWithoutBaseline(m, consumer, withBaseline); len(got) != 0 {
		t.Errorf("needsWithoutBaseline = %v, want none", got)
	}
}

func TestNeedsBaselineCodec(t *testing.T) {
	if got := EncodeNeedsBaselines(nil); got != "" {
		t.Errorf("empty map encoded as %q, want the empty string", got)
	}
	in := map[string]NeedsBaseline{"iface": {Fingerprint: "sha:a", Commit: "c1"}}
	if got := DecodeNeedsBaselines(EncodeNeedsBaselines(in)); got["iface"] != in["iface"] {
		t.Errorf("round trip = %+v, want %+v", got, in)
	}
	if got := DecodeNeedsBaselines("{nope"); len(got) != 0 {
		t.Errorf("a corrupt column yielded %+v, want empty", got)
	}
}

// ─── §5.2 the preview ────────────────────────────────────────────────────────

// The gate renders every reason it is refusing, in full, rather than a disabled
// button with no explanation.
func TestMergePreviewRendersItsBlockers(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	tk := f.task("schema", map[string]string{"schema.sql": "x\n"})
	writeFile(t, filepath.Join(f.repo, "README.md"), "mid-edit\n")

	p, err := f.i.Preview(f.run, f.m, tk)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if p.Target != "main" {
		t.Errorf("Target = %q, want main", p.Target)
	}
	if !p.Dirty || len(p.DirtyFiles) == 0 {
		t.Errorf("Dirty = %v %v, want the offending files named", p.Dirty, p.DirtyFiles)
	}
	for _, want := range []string{"not mergeable", "uncommitted changes"} {
		if !hasSubstring(p.Blockers, want) {
			t.Errorf("Blockers = %v, want one containing %q", p.Blockers, want)
		}
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// assertTreeAt is step R's postcondition: the worktree is exactly at `pre`, with
// no merge in progress and no debris. Both halves matter — a reset alone leaves
// the untracked files a conflicted merge wrote, and those are precisely what
// makes the NEXT pass fail for no reason.
func assertTreeAt(t *testing.T, dir, pre string) {
	t.Helper()
	if got := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD")); got != pre {
		t.Errorf("HEAD = %s, want pre %s: a failure path did not reset", got, pre)
	}
	if status := strings.TrimSpace(mustGit(t, dir, "status", "--porcelain")); status != "" {
		t.Errorf("the integration worktree is dirty after a failure:\n%s", status)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("the integration worktree is not a work tree any more: %v", err)
	}
	// MERGE_HEAD lives in the worktree's private git dir; `rev-parse -q --verify`
	// is the portable way to ask whether a merge is still in progress.
	if out, err := gitOut(dir, "rev-parse", "--quiet", "--verify", "MERGE_HEAD"); err == nil && strings.TrimSpace(out) != "" {
		t.Errorf("a merge is still in progress: MERGE_HEAD = %s", strings.TrimSpace(out))
	}
}

func hasSubstring(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
