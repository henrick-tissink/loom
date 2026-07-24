package delegate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
)

// fakeLauncher records every launch and never touches tmux. The call COUNT is
// the assertion that matters in this file: "the launcher was not called" is the
// only direct evidence that a refusal happened before any child existed.
type fakeLauncher struct {
	calls []session.Recipe
	err   error
}

func (f *fakeLauncher) Launch(r session.Recipe, w, h int, now time.Time) (string, error) {
	f.calls = append(f.calls, r)
	if f.err != nil {
		return "", f.err
	}
	return "loom-child-1", nil
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fixture builds a one-task run over a scratch repo, with the task in `state`.
type fixture struct {
	st      *store.Store
	run     store.DelegationRun
	m       Manifest
	task    Task
	repo    string
	loomDir string
	lnch    *fakeLauncher
	sp      *Spawner
}

func newFixture(t *testing.T, state TaskState) *fixture {
	t.Helper()
	st := openStore(t)
	repo := scratchRepo(t)
	base := headSHA(t, repo)
	loomDir := t.TempDir()

	run, err := st.InsertDelegationRun("atlas", "/w/innostream", "{}", `{"bankenstein":"`+base+`"}`, 1000)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{
		ID: "schema", Title: "Extract the account schema", Repo: "bankenstein",
		Paths:         []string{"db/migrations/**"},
		Brief:         "Move the account schema into a versioned migration.",
		Authorization: "You may modify db/migrations only. You may NOT change the HTTP layer.",
		Produces:      []Artifact{{ID: "account-schema", Path: "db/migrations/0007_account.sql"}},
		Check:         Check{Cmd: []string{"go", "test", "./internal/account/..."}},
	}
	sibling := Task{
		ID: "auth-api", Repo: "bankenstein", Paths: []string{"internal/auth/**"},
		Authorization: "auth only", Check: Check{Cmd: []string{"true"}},
	}
	if err := st.InsertDelegationTask(store.DelegationTask{
		RunID: run.ID, TaskID: task.ID, State: string(state), RepoLabel: task.Repo, UpdatedAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	m := Manifest{
		Name: "atlas", Project: "Innostream",
		Defaults:  Defaults{Model: "sonnet", Mode: "acceptEdits"},
		Repos:     map[string]RepoSetup{"bankenstein": {}},
		Tasks:     []Task{task, sibling},
		RepoPaths: map[string]string{"bankenstein": repo},
		Warnings:  []Warning{{Task: "schema", Text: "paths overlap with auth-api"}, {Task: "auth-api", Text: "not this one"}},
	}
	lnch := &fakeLauncher{}
	wt := &Worktrees{Layout: NewLayout(loomDir), Store: st}
	return &fixture{
		st: st, run: run, m: m, task: task, repo: repo, loomDir: loomDir, lnch: lnch,
		sp: &Spawner{Store: st, Launcher: lnch, Worktrees: wt, Now: func() time.Time { return time.Unix(2000, 0) }},
	}
}

func headSHA(t *testing.T, repo string) string {
	t.Helper()
	out, err := git(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func (f *fixture) state(t *testing.T) string {
	t.Helper()
	row, ok, err := f.st.GetDelegationTask(f.run.ID, f.task.ID)
	if err != nil || !ok {
		t.Fatalf("task row missing (ok=%v, err=%v)", ok, err)
	}
	return row.State
}

// §6.2 step 3 / §13.3, BINDING. Two claudes in one worktree on one branch is
// the worst outcome available in this design, and it is made STRUCTURALLY
// impossible rather than argued about in recovery: the approved→spawning CAS
// precedes every side effect, so a task that is not `approved` cannot reach the
// launcher, the git plumbing, or the filesystem.
//
// The launcher call count is the assertion. A refusal that still launched would
// pass every state-machine test in this file.
func TestSpawnRefusesATaskThatIsNotApproved(t *testing.T) {
	// `ready` is included on purpose: approval is the HUMAN's act (§5.1), and a
	// Spawn that quietly promoted a ready task would be auto-spawning, which is
	// the one thing Loom's stated principle forbids.
	for _, state := range []TaskState{
		StatePending, StateReady, StateSpawning, StateRunning, StateBlocked,
		StateChecking, StateVerified, StateFailed, StateMerged, StateAbandoned,
	} {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t, state)

			name, err := f.sp.Spawn(f.run, f.m, f.task)

			if !errors.Is(err, ErrTaskNotApproved) {
				t.Fatalf("Spawn err = %v, want ErrTaskNotApproved", err)
			}
			if name != "" {
				t.Errorf("Spawn returned session %q for a refused task", name)
			}
			if len(f.lnch.calls) != 0 {
				t.Fatalf("the launcher was called %d time(s) for a task in state %q", len(f.lnch.calls), state)
			}
			if got := f.state(t); got != string(state) {
				t.Errorf("state = %q, want %q — a refusal must not move the row", got, state)
			}
			dir := f.sp.Worktrees.Layout.Dir(f.run.Slug, f.task.Repo, f.task.ID)
			if _, err := os.Stat(dir); err == nil {
				t.Errorf("a worktree was created at %s for a refused spawn", dir)
			}
		})
	}
}

// The double-spawn case end to end: one approved task, two Spawn calls. The
// first wins, the second is refused by the same CAS — exactly one child, exactly
// one worktree, no second launch. This is the test the §6.2 precondition exists
// to pass.
func TestSpawnASecondTimeIsRefusedAfterTheFirstWins(t *testing.T) {
	f := newFixture(t, StateApproved)

	name, err := f.sp.Spawn(f.run, f.m, f.task)
	if err != nil {
		t.Fatalf("first Spawn: %v", err)
	}
	if name != "loom-child-1" {
		t.Fatalf("session name = %q", name)
	}
	if got := f.state(t); got != string(StateRunning) {
		t.Fatalf("state after a successful spawn = %q, want running", got)
	}

	name2, err := f.sp.Spawn(f.run, f.m, f.task)
	if !errors.Is(err, ErrTaskNotApproved) {
		t.Fatalf("second Spawn err = %v, want ErrTaskNotApproved", err)
	}
	if name2 != "" {
		t.Errorf("second Spawn returned session %q", name2)
	}
	if len(f.lnch.calls) != 1 {
		t.Fatalf("the launcher was called %d times; exactly one child may exist per task", len(f.lnch.calls))
	}
}

// The launch itself, asserted on the recipe rather than on tmux: the child's cwd
// is its worktree, its ONLY add-dir is its own .meta directory (~/.loom itself
// is never granted, so loom.db is not in a child's authorization scope), and the
// seed is a POINTER to brief.md rather than the brief — send-keys has a measured
// argv ceiling of ~16.3KB and a real brief exceeds it.
func TestSpawnLaunchesIntoTheWorktreeWithAPointerSeed(t *testing.T) {
	f := newFixture(t, StateApproved)
	if _, err := f.sp.Spawn(f.run, f.m, f.task); err != nil {
		t.Fatal(err)
	}
	if len(f.lnch.calls) != 1 {
		t.Fatalf("launch count = %d", len(f.lnch.calls))
	}
	r := f.lnch.calls[0]

	l := f.sp.Worktrees.Layout
	wantDir := physicalPath(l.Dir(f.run.Slug, f.task.Repo, f.task.ID))
	if r.Cwd != wantDir {
		t.Errorf("Cwd = %q, want the physically-resolved worktree %q", r.Cwd, wantDir)
	}
	wantMeta := physicalPath(l.MetaDir(f.run.Slug, f.task.Repo, f.task.ID))
	if len(r.AddDirs) != 1 || r.AddDirs[0] != wantMeta {
		t.Errorf("AddDirs = %v, want exactly [%s]", r.AddDirs, wantMeta)
	}
	briefPath := physicalPath(l.BriefPath(f.run.Slug, f.task.Repo, f.task.ID))
	if !strings.Contains(r.Seed, briefPath) {
		t.Errorf("seed %q does not name the brief file %q", r.Seed, briefPath)
	}
	if len(r.Seed) > 1024 {
		t.Errorf("the seed is %d bytes — it must be a pointer, not the brief", len(r.Seed))
	}
	// The brief must name the directory the child will actually be in. A brief
	// that says /var/... while getcwd() says /private/var/... is an
	// authorization boundary the child cannot check itself against.
	brief, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(brief), r.Cwd) {
		t.Errorf("the brief does not name the child's actual cwd %q", r.Cwd)
	}
	if r.Model != "sonnet" || r.Mode != "acceptEdits" {
		t.Errorf("model/mode = %q/%q, want the manifest defaults", r.Model, r.Mode)
	}
}

// §5.1: the human approves the brief they were SHOWN. The gate's brief and the
// file the child reads must be byte-identical, or the consent was given for a
// different document.
func TestSpawnPreviewBriefIsTheBriefTheChildReceives(t *testing.T) {
	f := newFixture(t, StateApproved)

	prev, err := f.sp.Preview(f.run, f.m, f.task)
	if err != nil {
		t.Fatal(err)
	}
	dir := f.sp.Worktrees.Layout.Dir(f.run.Slug, f.task.Repo, f.task.ID)
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("Preview created a worktree; looking at the gate must cost nothing")
	}

	if _, err := f.sp.Spawn(f.run, f.m, f.task); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(f.sp.Worktrees.Layout.BriefPath(f.run.Slug, f.task.Repo, f.task.ID))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != prev.Brief {
		t.Fatal("the brief rendered at the gate is not the brief written for the child")
	}
}

// The rest of §5.1's gate: the check argv verbatim, bypassPermissions flagged,
// the cap counter, and the task's own warnings carried to the moment of
// decision rather than left on the load screen.
func TestSpawnPreviewRendersTheGate(t *testing.T) {
	f := newFixture(t, StateApproved)
	f.m.Defaults.Mode = "bypassPermissions"

	prev, err := f.sp.Preview(f.run, f.m, f.task)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(prev.CheckArgv, " ") != "go test ./internal/account/..." {
		t.Errorf("CheckArgv = %v, want the manifest's argv verbatim", prev.CheckArgv)
	}
	if !prev.ModeRisky {
		t.Error("bypassPermissions was not flagged")
	}
	if prev.Branch != BranchName(f.run.Slug, f.task.ID) {
		t.Errorf("Branch = %q", prev.Branch)
	}
	if prev.Base != headSHA(t, f.repo) {
		t.Errorf("Base = %q, want the run's pinned base", prev.Base)
	}
	if prev.Cap != Concurrency3a || prev.Running != 0 || prev.CapReached {
		t.Errorf("cap counter = %d/%d (reached=%v), want 0/%d", prev.Running, prev.Cap, prev.CapReached, Concurrency3a)
	}
	if len(prev.Warnings) != 1 || prev.Warnings[0] != "paths overlap with auth-api" {
		t.Errorf("Warnings = %v, want only this task's", prev.Warnings)
	}
}

// §7 section 2, BINDING. Slice 1 §11 measured that removing explicit
// authorization-scope text raises scope overreach, so the manifest's text must
// appear VERBATIM and Loom's invariants must be APPENDED, never substituted for
// it. This test fails if anyone ever "tidies" the brief by summarizing.
func TestSpawnBriefCarriesAuthorizationVerbatimPlusLoomsInvariants(t *testing.T) {
	f := newFixture(t, StateApproved)
	c := Created{Dir: "/wt/schema", MetaDir: "/wt/schema.meta", Branch: "loom/atlas-1/schema", Base: "abc123"}

	b := Brief(f.run, f.m, f.task, c, AddDirs(c))

	if !strings.Contains(b, f.task.Authorization) {
		t.Fatal("the manifest's authorization text is not in the brief verbatim")
	}
	for _, want := range []string{
		"Write only inside this worktree: `/wt/schema`",
		"`/wt/schema.meta`",                        // the granted add-dir, named
		"Do not `git merge`",                       // no VCS games
		"`internal/auth/**`",                       // the sibling task's paths
		"Do not spawn subagents",                   //
		"db/migrations/0007_account.sql",           // artifact path
		"COMMITTED on this branch",                 // §7 section 4's rule
		"You do not declare done",                  // §7 section 5
		"go test ./internal/account/...",           // the check argv, verbatim
		"Do not report completion in prose",        //
		"/wt/schema.meta/block.json",               // §11.1's block file, outside the worktree
		"STOP at your prompt and say nothing",      //
		"Do not work around a block",               //
		"loom/atlas-1/schema",                      // identity
		"Extract the account schema",               // the title
		"Move the account schema into a versioned", // the task's own brief
	} {
		if !strings.Contains(b, want) {
			t.Errorf("brief is missing %q", want)
		}
	}
}

// Two spawns of the same task must produce the same bytes — the brief is
// assembled from maps (sibling paths, repos), and a map-ordered brief would make
// every re-spawn a different document and every diff of it noise.
func TestSpawnBriefIsDeterministic(t *testing.T) {
	f := newFixture(t, StateApproved)
	f.m.Tasks = append(f.m.Tasks, Task{ID: "zeta", Repo: "bankenstein", Paths: []string{"z/**", "a/**"}})
	c := Created{Dir: "/wt/schema", MetaDir: "/wt/schema.meta", Branch: "b", Base: "abc"}

	first := Brief(f.run, f.m, f.task, c, AddDirs(c))
	for i := 0; i < 20; i++ {
		if Brief(f.run, f.m, f.task, c, AddDirs(c)) != first {
			t.Fatal("Brief is not deterministic across calls")
		}
	}
	// Sibling paths sorted, and the task's own paths excluded from its own
	// don't-touch list — telling a child not to write the files it was told to
	// write is how a task blocks itself.
	if !strings.Contains(first, "`a/**`, `internal/auth/**`, `z/**`") {
		t.Errorf("sibling paths are not sorted:\n%s", first)
	}
	if strings.Contains(first, "`db/migrations/**`, ") || strings.Contains(first, ", `db/migrations/**`") {
		t.Error("the task's OWN declared paths appear in its don't-touch list")
	}
}

func TestSpawnAddDirsGrantOnlyTheMetaDirectory(t *testing.T) {
	if got := AddDirs(Created{Dir: "/wt/x", MetaDir: "/wt/x.meta"}); len(got) != 1 || got[0] != "/wt/x.meta" {
		t.Fatalf("AddDirs = %v, want [/wt/x.meta] — ~/.loom itself is never granted", got)
	}
	if got := AddDirs(Created{Dir: "/wt/x"}); got != nil {
		t.Fatalf("AddDirs = %v, want nil when there is no meta dir", got)
	}
}

func TestSpawnDelegationTagMatchesTheWorkflowConvention(t *testing.T) {
	if got := DelegationTag("atlas-7", "auth-api"); got != "dlg:atlas-7#auth-api" {
		t.Fatalf("DelegationTag = %q", got)
	}
}

// §6.6: 0 means unset and yields the default, and no configuration may express
// "unlimited".
func TestSpawnClampConcurrency(t *testing.T) {
	tests := []struct{ in, want int }{
		{0, ConcurrencyDefault}, {-5, ConcurrencyDefault},
		{1, 1}, {3, 3}, {10, 10}, {11, ConcurrencyMax}, {1000, ConcurrencyMax},
	}
	for _, tc := range tests {
		if got := ClampConcurrency(tc.in); got != tc.want {
			t.Errorf("ClampConcurrency(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A launch that fails leaves the task in `spawning` ON PURPOSE (§13.3): the
// failure may have produced a real tmux session whose row was never written,
// and moving the row back to `approved` is what would put a second claude in
// that worktree. Recovery adjudicates it by cwd; this test pins the ordering
// decision so nobody "fixes" it into a rollback.
func TestSpawnLeavesTheTaskSpawningWhenTheLaunchFails(t *testing.T) {
	f := newFixture(t, StateApproved)
	f.lnch.err = errors.New("tmux said no")

	if _, err := f.sp.Spawn(f.run, f.m, f.task); err == nil {
		t.Fatal("Spawn returned nil for a failed launch")
	}
	if got := f.state(t); got != string(StateSpawning) {
		t.Fatalf("state = %q, want spawning — a failed launch must not be rolled back", got)
	}
}

// A worktree that cannot be created, by contrast, RELEASES the claim: nothing
// was launched (Create's own preconditions run before any child exists), so the
// human gets their approve action back with the error beside it rather than a
// task wedged in `spawning` forever.
func TestSpawnReleasesTheClaimWhenTheWorktreeFails(t *testing.T) {
	f := newFixture(t, StateApproved)
	f.m.RepoPaths["bankenstein"] = filepath.Join(t.TempDir(), "not-a-repo")

	if _, err := f.sp.Spawn(f.run, f.m, f.task); err == nil {
		t.Fatal("Spawn returned nil for an impossible worktree")
	}
	if got := f.state(t); got != string(StateApproved) {
		t.Fatalf("state = %q, want approved", got)
	}
	if len(f.lnch.calls) != 0 {
		t.Fatal("the launcher was called despite a failed worktree creation")
	}
}

// §5.1 is the human's act and it is a CAS: two approvals of the same ready task
// yield exactly one `approved`.
func TestSpawnApproveIsACompareAndSwap(t *testing.T) {
	f := newFixture(t, StateReady)

	first, err := f.sp.Approve(f.run.ID, f.task.ID)
	if err != nil || !first {
		t.Fatalf("first Approve = (%v, %v)", first, err)
	}
	second, err := f.sp.Approve(f.run.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("a second Approve also claimed the task")
	}
	if got := f.state(t); got != string(StateApproved) {
		t.Fatalf("state = %q", got)
	}
}

// --- failure-mode probes (slice 3a) ---------------------------------------

// capLauncher is a fake launcher that does what session.Launcher actually does
// and the plain fakeLauncher does not: it UPSERTS a live sessions row at the
// recipe's cwd. That row is the only thing Worktrees.LiveChildren counts, so a
// cap test against a launcher that writes nothing is a test of an empty table.
//
// It also holds every launch open on a barrier until `hold` of them are in
// flight at once, which is what makes the race deterministic rather than a
// coin flip: without it a scheduler that happens to serialize passes.
type capLauncher struct {
	mu      sync.Mutex
	st      *store.Store
	n       int
	inFlt   int
	hold    int
	barrier chan struct{}
	once    sync.Once
}

func (l *capLauncher) Launch(r session.Recipe, w, h int, now time.Time) (string, error) {
	l.mu.Lock()
	l.n++
	name := fmt.Sprintf("loom-child-%d", l.n)
	l.inFlt++
	reached := l.inFlt
	l.mu.Unlock()
	if reached >= l.hold {
		l.once.Do(func() { close(l.barrier) })
	}
	select {
	case <-l.barrier:
	case <-time.After(2 * time.Second):
	}
	return name, l.st.Upsert(store.SessionRow{
		Name: name, Cwd: r.Cwd, EndedAt: -1, ExitCode: -1, LastStatus: "running",
	})
}

// §6.6 is BINDING: the cap is a HARD maximum, and 3a runs at 3. It is a safety
// property — each child is a real claude with real quota, §6.4's shared
// resources collide superlinearly, and the human at the merge gate is the
// actual queue.
//
// The count it is enforced against (Worktrees.LiveChildren) only becomes true
// AFTER Launcher.Launch has upserted the session row, so every spawn that is
// already past its cap check is invisible to every other spawn's cap check.
// That is not a theoretical window: the GUI's ApproveTask is a Wails binding,
// so §5.1's own "approve all 3 ready tasks" runs the loop concurrently, and
// §13's supported two-Loom-instances-one-DB configuration races by
// construction.
func TestSpawnConcurrentSpawnsMustNotExceedTheCap(t *testing.T) {
	const cap, tasks = 3, 5

	st := openStore(t)
	repo := scratchRepo(t)
	base := headSHA(t, repo)
	run, err := st.InsertDelegationRun("atlas", "/w/innostream", "{}", `{"api":"`+base+`"}`, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var defs []Task
	for i := 0; i < tasks; i++ {
		id := fmt.Sprintf("t%d", i)
		defs = append(defs, Task{ID: id, Repo: "api", Authorization: "a", Check: Check{Cmd: []string{"true"}}})
		if err := st.InsertDelegationTask(store.DelegationTask{
			RunID: run.ID, TaskID: id, State: string(StateApproved), RepoLabel: "api", UpdatedAt: 1000,
		}); err != nil {
			t.Fatal(err)
		}
	}
	m := Manifest{Name: "atlas", Repos: map[string]RepoSetup{"api": {}}, Tasks: defs,
		RepoPaths: map[string]string{"api": repo}}
	l := &capLauncher{st: st, hold: cap + 1, barrier: make(chan struct{})}
	sp := &Spawner{Store: st, Launcher: l,
		Worktrees: &Worktrees{Layout: NewLayout(t.TempDir()), Store: st, Cap: cap}}

	var wg sync.WaitGroup
	for _, tk := range defs {
		wg.Add(1)
		go func(tk Task) {
			defer wg.Done()
			_, _ = sp.Spawn(run, m, tk) // a refusal is the point; the error is not
		}(tk)
	}
	wg.Wait()

	if l.n > cap {
		t.Errorf("%d children launched against a cap of %d — the cap is advisory under "+
			"concurrent approval, because LiveChildren cannot see a launch that has not "+
			"written its session row yet", l.n, cap)
	}
}

// The same race, at the transition that IS guarded: §13.3's approved→spawning
// CAS. This one must hold, and it is the reason a lost cap check is a budget
// bug rather than a double-spawn: two concurrent approvals of ONE task contend
// on one row and exactly one wins.
func TestSpawnApproveIsSingletonUnderConcurrency(t *testing.T) {
	f := newFixture(t, StateReady)

	const racers = 8
	var wg sync.WaitGroup
	claims := make([]bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := f.sp.Approve(f.run.ID, f.task.ID)
			if err != nil {
				t.Errorf("Approve: %v", err)
			}
			claims[i] = ok
		}(i)
	}
	wg.Wait()

	won := 0
	for _, c := range claims {
		if c {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d of %d concurrent approvals claimed the task; exactly one may", won, racers)
	}
}

// The block template is the ONLY specification of block.json the child ever
// sees, and two of its fields are load-bearing rather than decorative:
//
//   - `need.artifact` is what Rendezvous.Unblocked reads. A needs-artifact block
//     without it can never satisfy the resume condition, so the park is
//     PERMANENT — the child stopped correctly and visibly and the run still
//     never moves, which is exactly the silent outcome §11.2 forbids.
//   - `paths` is what Propose turns into a scope amendment. Without it
//     ApplyScope refuses the widening as "grants no paths", so the one correct
//     response to a boundary drawn wrong (§11.3) cannot be granted.
//
// This test fails if either is ever dropped from the template, which is the only
// way the defect is visible: nothing at spawn time can tell that a brief is
// about to cost a future block its remedy.
func TestSpawnBriefTemplateCarriesTheFieldsTheProtocolActuallyReads(t *testing.T) {
	f := newFixture(t, StateApproved)
	c := Created{Dir: "/wt/schema", MetaDir: "/wt/schema.meta", Branch: "b", Base: "abc"}
	b := Brief(f.run, f.m, f.task, c, AddDirs(c))

	for _, tc := range []struct{ field, why string }{
		{`"need"`, "Rendezvous.Unblocked reads need.artifact; without it the park is permanent"},
		{`"artifact"`, "the artifact id is the machine-checkable half of the resume condition"},
		{`"paths"`, "ApplyScope refuses a scope amendment that grants no paths"},
		{`"kind"`, "ParseBlock refuses a block that names no kind"},
		{`"task"`, "ParseBlock refuses a block that names no task"},
	} {
		if !strings.Contains(b, tc.field) {
			t.Errorf("brief's block template omits %s — %s", tc.field, tc.why)
		}
	}

	// The 3a text promised a human would read the file and reply. §11.4 now
	// resumes needs-artifact automatically, and a child told to expect a human
	// is a child that decides after a compaction that nobody came.
	if strings.Contains(b, "a human will read that file") {
		t.Error("brief still promises the superseded 3a human-only reply")
	}
}
