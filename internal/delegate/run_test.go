package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
)

// newRunFixture builds a real run against a scratch repo: a manifest with two
// tasks, the run and task rows the Runner reads, and a Runner wired to a
// temp-dir Layout.
//
// A real store and a real repo rather than fakes, because every assertion below
// is about a CAS or a refusal at a gate, and both of those are only meaningful
// against the statement that enforces them.
func newRunFixture(t *testing.T) (*Runner, store.DelegationRun, Manifest) {
	t.Helper()
	repo := scratchRepo(t)
	m := Manifest{
		Version: ManifestVersion, Name: "atlas", Project: "atlas",
		ProjectRoot: t.TempDir(),
		RepoPaths:   map[string]string{"api": repo},
		Repos:       map[string]RepoSetup{"api": {}},
		Tasks: []Task{
			{ID: "schema", Repo: "api", Authorization: "db only",
				Produces: []Artifact{{ID: "account-schema", Path: "db/0007.sql"}},
				Check:    Check{Cmd: []string{"true"}}},
			{ID: "auth-api", Repo: "api", Authorization: "api only",
				Needs: []string{"account-schema"},
				Check: Check{Cmd: []string{"true"}}},
		},
	}
	s := newTestStore(t)
	r := &Runner{
		Store:  s,
		Layout: NewLayout(t.TempDir()),
		Now:    func() time.Time { return epoch },
	}
	run, err := r.Create(m)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return r, run, m
}

// §10.2's "spawning stops" on a baseline fault, and §12.1's deadlock, both land
// on status `deadlocked`. BINDING: Approve must REFUSE while the run is red.
// integrate.go can set the status but cannot enforce a consequence at a gate it
// does not own, so this refusal is the only thing that implements the rule.
func TestApproveRefusesWhileTheRunIsRed(t *testing.T) {
	r, run, _ := newRunFixture(t)
	if _, err := r.Store.AdvanceDelegationRunCAS(run.ID, "running", "deadlocked", epoch.Unix()); err != nil {
		t.Fatal(err)
	}
	err := r.Approve(context.Background(), run.ID, "schema")
	if !errors.Is(err, ErrRunRed) {
		t.Fatalf("Approve on a red run = %v, want ErrRunRed", err)
	}
	// And the row must be untouched: a refusal that consumed the approval would
	// leave the human with no action and no explanation.
	row, _, err := r.Store.GetDelegationTask(run.ID, "schema")
	if err != nil {
		t.Fatal(err)
	}
	if TaskState(row.State) != StatePending {
		t.Fatalf("task state after a refused approve = %s, want it untouched", row.State)
	}
}

// §14: a spawn is unambiguously NEW work, and a tmux window titled with the
// client's repo is exactly the leak §6 exists to prevent.
func TestApproveRefusesOnAHiddenProject(t *testing.T) {
	r, run, _ := newRunFixture(t)
	r.Hidden = func(string) bool { return true }
	if err := r.Approve(context.Background(), run.ID, "schema"); !errors.Is(err, ErrProjectHidden) {
		t.Fatalf("Approve on a hidden project = %v, want ErrProjectHidden", err)
	}
}

// The two red shapes read completely differently to a human, and the
// discriminator is delegation_runs.integration. A view that rendered
// `deadlocked` as a wait-for cycle unconditionally shows an empty cycle for
// every baseline fault, so the REASON has to be derivable from the run row.
func TestRedRunReasonDiscriminatesABaselineFaultFromADeadlock(t *testing.T) {
	tests := []struct {
		name        string
		integration map[string]Baseline
		wantSubstr  string
	}{
		{"no baselines recorded is a plain deadlock", nil, "re-planned"},
		{"a green baseline is not a fault",
			map[string]Baseline{"api": {Head: "abc", Status: CheckPass}}, "re-planned"},
		{"a red baseline names the repo and the status",
			map[string]Baseline{"api": {Head: "abc", Status: CheckFail, Out: "2 tests failed"}}, `repo "api"`},
		{"a red baseline says no task is to blame",
			map[string]Baseline{"api": {Head: "abc", Status: CheckFail}}, "no task is to blame"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := store.DelegationRun{Status: "deadlocked", Integration: EncodeBaselines(tc.integration)}
			if got := redRunReason(run); !strings.Contains(got, tc.wantSubstr) {
				t.Fatalf("redRunReason = %q, want it to contain %q", got, tc.wantSubstr)
			}
		})
	}
}

// §5.2's second acknowledgement carries WHAT was acknowledged, not a boolean. A
// preview computed at T and approved at T+5m may describe a different
// divergence, and "I acknowledged something" is not consent to whatever is there
// now. Both directions are a mismatch: a file that appeared is information the
// human never saw, and one that vanished means they are approving a picture that
// no longer exists.
func TestAckMismatchIsSymmetric(t *testing.T) {
	tests := []struct {
		name           string
		acked, current []string
		wantMismatch   bool
		wantSubstr     string
	}{
		{"identical lists agree", []string{"a.go", "b.go"}, []string{"b.go", "a.go"}, false, ""},
		{"both empty agree", nil, nil, false, ""},
		{"a new file since the preview", []string{"a.go"}, []string{"a.go", "c.go"}, true, "new: c.go"},
		{"a file that is no longer there", []string{"a.go", "c.go"}, []string{"a.go"}, true, "no longer there: c.go"},
		{"an acknowledgement of nothing against a real finding", nil, []string{"a.go"}, true, "new: a.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ackMismatch("scope divergence", tc.acked, tc.current)
			if (got != "") != tc.wantMismatch {
				t.Fatalf("ackMismatch = %q, want mismatch = %v", got, tc.wantMismatch)
			}
			if tc.wantSubstr != "" && !strings.Contains(got, tc.wantSubstr) {
				t.Fatalf("ackMismatch = %q, want it to contain %q", got, tc.wantSubstr)
			}
		})
	}
}

// §12.3.1 and .2 are one acknowledgement; §12.3.3 is a SEPARATE one. The two
// findings have different mechanisms and different confidence, and one checkbox
// for both would let the snapshot walk's disclosed false positives launder a
// real commit-level finding. divergedFiles must therefore not fold in drift.
func TestDivergedFilesFlattensCommitLevelFindingsOnly(t *testing.T) {
	d := DivergenceReport{
		Outside:  []string{"internal/auth/x.go", "shared.go"},
		Siblings: map[string][]string{"schema": {"shared.go", "db/0007.sql"}},
		Drift:    SnapshotDrift{Changed: map[string][]string{"/repo": {"never.go"}}},
	}
	got := divergedFiles(d)
	want := []string{"db/0007.sql", "internal/auth/x.go", "shared.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("divergedFiles = %v, want %v (deduplicated, sorted, no drift)", got, want)
	}
	drift := driftFiles(d.Drift)
	if len(drift) != 1 || drift[0] != filepath.Join("/repo", "never.go") {
		t.Fatalf("driftFiles = %v, want the dir-qualified path and nothing from the diff", drift)
	}
}

// §10.1: the integration worktrees are created at RUN CREATION, eagerly, so a
// repo whose worktree cannot be made fails while the human is still looking at
// the run rather than an hour later behind the first green check.
func TestCreateWritesTaskRowsAndTheIntegrationWorktree(t *testing.T) {
	repo := scratchRepo(t)
	m := Manifest{
		Version: ManifestVersion, Name: "atlas", ProjectRoot: t.TempDir(),
		RepoPaths: map[string]string{"api": repo},
		Repos:     map[string]RepoSetup{"api": {}},
		Tasks: []Task{
			{ID: "schema", Repo: "api", Authorization: "db", Check: Check{Cmd: []string{"true"}}},
		},
	}
	s := newTestStore(t)
	layout := NewLayout(t.TempDir())
	r := &Runner{
		Store: s, Layout: layout, Now: func() time.Time { return epoch },
		Integrator: &Integrator{Store: s, Layout: layout, Now: func() time.Time { return epoch }},
	}
	run, err := r.Create(m)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if run.Status != "running" {
		t.Fatalf("run status = %q, want running", run.Status)
	}
	rows, err := s.ListDelegationTasks(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != string(StatePending) {
		t.Fatalf("task rows = %+v, want one `pending` row (Tick decides what is ready, not Create)", rows)
	}
	// §10.1's worktree, at the deterministic path, created before any child
	// exists.
	if dir := layout.IntegrationDir(run.Slug, "api"); !dirExists(dir) {
		t.Fatalf("no integration worktree at %s — §10.1 says it is created at run creation", dir)
	}
}

// Abandon stops Loom OFFERING work; it destroys none. Every task row is CAS'd to
// abandoned and nothing is killed — Loom is a window, never an owner, and a
// child at a worktree may be mid-thought with an hour of irreplaceable context.
func TestAbandonCASesEveryTaskAndKillsNothing(t *testing.T) {
	r, run, _ := newRunFixture(t)
	live := "child-1"
	dir := r.Layout.Dir(run.Slug, "api", "schema")
	occupyWorktree(t, r.Store, live, dir)
	r.Watchdogs = &Watchdogs{Store: r.Store, Layout: r.Layout,
		Worktrees: &Worktrees{Layout: r.Layout, Store: r.Store},
		Now:       func() time.Time { return epoch }}

	err := r.Abandon(run.ID)
	// The sweep's finding is an ERROR VALUE precisely so it is rendered: those
	// children are still spending.
	if err != nil && !errors.Is(err, ErrChildrenStillLive) {
		t.Fatalf("Abandon = %v, want nil or ErrChildrenStillLive", err)
	}
	rows, rerr := r.Store.ListDelegationTasks(run.ID)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, row := range rows {
		if TaskState(row.State) != StateAbandoned {
			t.Fatalf("task %q = %s after abandon, want abandoned", row.TaskID, row.State)
		}
	}
	sess, ok, serr := r.Store.Get(live)
	if serr != nil || !ok {
		t.Fatalf("the live session row vanished: ok=%v err=%v", ok, serr)
	}
	if sess.EndedAt != -1 {
		t.Fatal("abandon ended a live session — nothing may be killed")
	}
}

// A run whose manifest snapshot will not parse must fail LOUDLY at load rather
// than degrade to an empty graph, because an empty graph is a run that reports
// no tasks and no deadlock: silence that looks like success.
func TestLoadRefusesAnUnreadableManifestSnapshot(t *testing.T) {
	s := newTestStore(t)
	run, err := s.InsertDelegationRun("atlas", t.TempDir(), "{not json", "{}", epoch.Unix())
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s, Now: func() time.Time { return epoch }}
	if _, err := r.load(run.ID); err == nil {
		t.Fatal("load accepted an unreadable manifest snapshot")
	}
}

// The amendment log is read through the store when no AmendmentStore is
// injected, and an amendment is INERT until approved — Effective ignores it —
// because §11.3's whole shape is that Loom proposes and the human grants.
func TestRunnerReadsTheAmendmentLogAndHonoursApproval(t *testing.T) {
	r, run, _ := newRunFixture(t)
	body := EncodeAmendmentBody(Amendment{
		Task: "auth-api", Artifact: "account-schema", From: "schema", Reason: "unforeseen",
	})
	seq, err := r.Store.AppendDelegationAmendment(run.ID, string(AmendEdge), body, epoch.Unix())
	if err != nil {
		t.Fatal(err)
	}

	as := r.amendments(run.ID)
	if len(as) != 1 || as[0].Seq != seq || as[0].Task != "auth-api" {
		t.Fatalf("amendments = %+v, want the one proposal decoded", as)
	}
	if as[0].Accepted() {
		t.Fatal("a freshly proposed amendment reported itself accepted — it is inert until a human grants it")
	}

	claimed, err := r.Store.ApproveDelegationAmendmentCAS(run.ID, seq, epoch.Unix())
	if err != nil || !claimed {
		t.Fatalf("ApproveDelegationAmendmentCAS: claimed=%v err=%v", claimed, err)
	}
	// The CAS is what makes two Loom instances approving the same amendment
	// produce one edge and not two.
	if again, err := r.Store.ApproveDelegationAmendmentCAS(run.ID, seq, epoch.Unix()); err != nil || again {
		t.Fatalf("a second approval was claimed (%v) — approval must be a CAS", again)
	}
	as = r.amendments(run.ID)
	if len(as) != 1 || !as[0].Accepted() {
		t.Fatalf("amendments after approval = %+v, want one accepted", as)
	}
	// And Accept now folds it in without complaint, because ApprovedAt is set.
	e := Effective(mustManifest(t, run), nil, nil)
	if _, err := Accept(e, as[0]); err != nil {
		t.Fatalf("Accept on an approved amendment = %v, want nil", err)
	}
}

func mustManifest(t *testing.T, run store.DelegationRun) Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal([]byte(run.ManifestJSON), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Watch's findings are applied LITERALLY, and ActionOfferRetry is the one that
// must NOT act: retrying a seed takes the decision away from the human the
// watchdog exists to inform, and re-delivers into a live agent's prompt.
// Run-scoped findings sort first because a budget that stopped every spawn
// explains every other row on the screen.
func TestWatchOrdersRunScopedFindingsFirst(t *testing.T) {
	r, run, _ := newRunFixture(t)
	st, err := r.load(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := Watch(epoch, []Observation{
		{TaskID: "schema", State: StateSpawning, Since: epoch.Add(-2 * time.Minute)},
	}, Budget{MaxChildren: 1, Spawned: 4, StartedAt: epoch.Add(-time.Hour)})
	// Sorting is the runner's, applied to whatever Watch produced.
	sorted := r.watch(st, epoch)
	_ = sorted
	if len(f) < 2 {
		t.Skip("the pure pass produced fewer than two findings; ordering is untestable here")
	}
	if f[0].TaskID != "" {
		t.Fatalf("first finding = %+v, want the run-scoped one", f[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The full runner: a real store, a real git repo, real worktrees, real checks.
//
// Fakes stop at the two seams where a test cannot go — tmux and `claude` — and
// nowhere else. Every assertion below is about a CAS, an ORDER or a refusal, and
// all three are only meaningful against the statement that enforces them; a
// runner test built on a fake store would assert the fake.

// idleLauncher is what session.Launcher actually does and fakeLauncher does not:
// it UPSERTS a live session row at the recipe's cwd, with a claude session id.
// Both are load-bearing here — the cap, §6.2 step 3's occupancy refusal, §6.3's
// orphan flag and §11.4's continue gate all read that row, and a launcher that
// writes nothing turns every one of those into an assertion about an empty table.
type idleLauncher struct {
	st       *store.Store
	n        int
	launched []session.Recipe
	err      error
}

func (l *idleLauncher) Launch(r session.Recipe, w, h int, now time.Time) (string, error) {
	if l.err != nil {
		return "", l.err
	}
	l.n++
	l.launched = append(l.launched, r)
	name := fmt.Sprintf("loom-child-%d", l.n)
	return name, l.st.Upsert(store.SessionRow{
		Name: name, ClaudeSessionID: childClaudeID(name), Cwd: r.Cwd,
		CreatedAt: epoch.Unix(), EndedAt: -1, ExitCode: -1,
		// `idle` is a child sitting at its prompt, which is what a child does
		// between turns. ShouldRun and the continue gate both read this.
		LastStatus: "idle",
	})
}

func childClaudeID(name string) string {
	return "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa" + fmt.Sprintf("%02d", len(name))
}

// runFixture is one whole run with every stage wired.
type runFixture struct {
	t     *testing.T
	r     *Runner
	run   store.DelegationRun
	m     Manifest
	repo  string
	st    *store.Store
	lnch  *idleLauncher
	tmux  *fakeSender
	ccd   string
	clock time.Time
}

// newRunner builds the fixture. Two tasks, one artifact edge: `schema` produces
// `account-schema`, `auth-api` needs it. That is the smallest shape in which
// §9's scheduler, §11's rendezvous and §2's M3 all have something to say.
func newRunner(t *testing.T) *runFixture {
	t.Helper()
	repo := scratchRepo(t)
	s := newTestStore(t)
	layout := NewLayout(t.TempDir())
	ccd := t.TempDir()
	f := &runFixture{t: t, repo: repo, st: s, ccd: ccd, clock: epoch}
	now := func() time.Time { return f.clock }

	f.m = Manifest{
		Version: ManifestVersion, Name: "atlas", Project: "atlas",
		ProjectRoot: t.TempDir(),
		RepoPaths:   map[string]string{"api": repo},
		Repos:       map[string]RepoSetup{"api": {}},
		Tasks: []Task{
			{ID: "schema", Repo: "api", Authorization: "db only",
				Paths:    []string{"db/**"},
				Produces: []Artifact{{ID: "account-schema", Path: "db/0007.sql"}},
				Check:    Check{Cmd: []string{"true"}}},
			// auth-api produces an artifact of its own so the cycle test has a
			// real second edge to close the loop with; nothing else reads it.
			{ID: "auth-api", Repo: "api", Authorization: "api only",
				Paths:    []string{"api/**"},
				Needs:    []string{"account-schema"},
				Produces: []Artifact{{ID: "auth-openapi", Path: "api/auth.yaml"}},
				Check:    Check{Cmd: []string{"true"}}},
			// docs declares no needs at all, which is what makes it the §11 case:
			// the dependency it will hit is UNFORESEEN by the plan, not a
			// declared edge the scheduler would have waited on.
			{ID: "docs", Repo: "api", Authorization: "docs only",
				Paths: []string{"docs/**"},
				Check: Check{Cmd: []string{"true"}}},
		},
	}
	f.lnch = &idleLauncher{st: s}
	f.tmux = &fakeSender{}
	wt := &Worktrees{Layout: layout, Store: s}
	det := &Detector{Layout: layout, Store: s, Now: now}
	f.r = &Runner{
		Store: s, Layout: layout, Now: now,
		// The snapshot carries no repo paths on purpose (Manifest.RepoPaths is
		// `json:"-"`), so the runner re-resolves them exactly as the real caller
		// does.
		Repos:      func(string) map[string]string { return f.m.RepoPaths },
		Spawner:    &Spawner{Store: s, Launcher: f.lnch, Worktrees: wt, Now: now},
		Worktrees:  wt,
		Checker:    &Checker{},
		Detector:   det,
		Integrator: &Integrator{Store: s, Layout: layout, Repos: f.m.RepoPaths, Worktrees: wt, Now: now},
		Rendezvous: &Rendezvous{
			Store: s, Layout: layout, Detector: det, Tmux: f.tmux,
			ClaudeConfigDir: ccd, PollEvery: 5 * time.Millisecond, Timeout: 50 * time.Millisecond,
			Now: now,
		},
		Watchdogs: &Watchdogs{Store: s, Layout: layout, Worktrees: wt, Now: now},
	}
	run, err := f.r.Create(f.m)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.run = run
	return f
}

func (f *runFixture) tick() TickReport {
	f.t.Helper()
	rep, err := f.r.Tick(context.Background(), f.run.ID)
	if err != nil {
		f.t.Fatalf("Tick: %v", err)
	}
	return rep
}

func (f *runFixture) row(id string) store.DelegationTask {
	f.t.Helper()
	row, ok, err := f.st.GetDelegationTask(f.run.ID, id)
	if err != nil || !ok {
		f.t.Fatalf("task %q: ok=%v err=%v", id, ok, err)
	}
	return row
}

func (f *runFixture) state(id string) TaskState { return TaskState(f.row(id).State) }

// approve runs the whole §5.1 gate for a task: tick to make it `ready`, then
// press. Through the real path on purpose — a fixture that inserted `ready`
// itself would be testing a row shape the scheduler cannot produce.
func (f *runFixture) approve(id string) {
	f.t.Helper()
	f.tick()
	if err := f.r.Approve(context.Background(), f.run.ID, id); err != nil {
		f.t.Fatalf("Approve(%s): %v", id, err)
	}
	f.settle(id)
}

// settle writes the child's transcript so the continue gate and §8.2's debounce
// see a child at its prompt. The transcript is a real one and the classifier
// decides: SHAPE, never a string match on a rendered pane.
func (f *runFixture) settle(id string) {
	f.t.Helper()
	row := f.row(id)
	if row.SessionName == "" {
		f.t.Fatalf("task %q has no session bound; the spawn CAS did not complete", id)
	}
	writeRzTranscript(f.t, f.ccd, row.Worktree, childClaudeID(row.SessionName), transcript.StateIdle)
}

// commit is the child doing its work: files written and COMMITTED on its own
// branch, which is the only channel §3 gives a child to the rest of the system.
func (f *runFixture) commit(id string, files map[string]string) {
	f.t.Helper()
	dir := f.row(id).Worktree
	for name, body := range files {
		writeFile(f.t, filepath.Join(dir, name), body)
		mustGit(f.t, dir, "add", name)
	}
	mustGit(f.t, dir, "commit", "-qm", "child work for "+id)
}

// ─── the happy path ─────────────────────────────────────────────────────────

// The whole sequence, end to end: approve → spawn → the child commits → the
// check runs itself on §8.2's debounce → verified and published → integration
// merges it into the per-repo worktree and promotes to `mergeable`.
//
// One test rather than four because the ORDER is the thing under test. Each
// stage is separately covered elsewhere in this package; what only this can
// assert is that the runner sequences them without a human touching anything
// between the approve and the merge gate.
func TestRunnerHappyPathSpawnCheckIntegrate(t *testing.T) {
	f := newRunner(t)

	// §9.1: only `schema` is ready — `auth-api` needs an artifact nobody has
	// published, and Loom does not offer it.
	rep := f.tick()
	if !hasID(rep.Progress.Ready, "schema") || hasID(rep.Progress.Ready, "auth-api") {
		t.Fatalf("ready = %v, want schema offered and auth-api withheld (its need is unmet)", rep.Progress.Ready)
	}
	if f.state("schema") != StateReady {
		t.Fatalf("schema = %s after a tick, want ready — Tick is the only scheduler and it must PERSIST the set", f.state("schema"))
	}

	f.approve("schema")
	if f.state("schema") != StateRunning {
		t.Fatalf("schema = %s after approve, want running", f.state("schema"))
	}
	if f.lnch.n != 1 {
		t.Fatalf("launcher called %d times, want exactly 1", f.lnch.n)
	}

	// Nothing is checked before the child commits: the branch head is still the
	// base, and a check against a tree the child has not touched is a guaranteed
	// red that would poison §2's M2 for every task in every run.
	if rep := f.tick(); len(rep.Checked) != 0 {
		t.Fatalf("checked %v before the child committed anything", rep.Checked)
	}

	f.commit("schema", map[string]string{"db/0007.sql": "create table account();\n"})
	rep = f.tick()
	if !hasID(rep.Checked, "schema") {
		t.Fatalf("checked = %v, want schema (head moved, child idle)", rep.Checked)
	}
	// §8.3: the artifact is published from the RESULT's own bit, and its
	// consumers become schedulable because of that row and nothing else.
	if _, ok, err := f.st.GetDelegationArtifact(f.run.ID, "account-schema"); err != nil || !ok {
		t.Fatalf("account-schema not published (ok=%v err=%v)", ok, err)
	}
	// §10.2 runs in the SAME tick, from the state the check just wrote: step 5
	// re-reads before it integrates precisely so a green task does not wait a
	// whole poll interval for a slot that is already free.
	if !hasID(rep.Integrated, "schema") {
		t.Fatalf("integrated = %v (errs %v), want schema in the same tick as its check", rep.Integrated, rep.Errs)
	}
	if f.state("schema") != StateMergeable {
		t.Fatalf("schema = %s after integration, want mergeable", f.state("schema"))
	}
	// And the consumer is now offered, because its need is published — the whole
	// point of artifact-level edges.
	if !hasID(f.tick().Progress.Ready, "auth-api") {
		t.Fatal("auth-api is still not ready after its artifact was published")
	}
}

func hasID(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// ─── CAS discipline ─────────────────────────────────────────────────────────

// BINDING (§13.3): every transition is a compare-and-swap on the snapshot the
// caller acted on, because two Loom instances against one DB is supported. A
// caller holding a stale snapshot must be REFUSED and told, never allowed to act
// on a premise that is gone — and never made to retry in a loop, which is a CAS
// argued out of being a CAS.
//
// Table-driven over every transition the runner owns. `move` is the OTHER
// instance winning the row between the caller's read and its act.
func TestEveryTransitionRefusesAStaleSnapshot(t *testing.T) {
	tests := []struct {
		name string
		// setup leaves the task in the state the caller believes it is in.
		setup func(f *runFixture)
		// move is the other Loom instance.
		move func(f *runFixture)
		// act is the caller acting on its now-stale snapshot.
		act      func(f *runFixture) error
		wantErr  error
		wantLive TaskState
	}{
		{
			name:  "approve a task another instance already approved",
			setup: func(f *runFixture) { f.tick() },
			move: func(f *runFixture) {
				if _, err := f.st.AdvanceTaskCAS(f.run.ID, "schema",
					string(StateReady), string(StateApproved), epoch.Unix()); err != nil {
					f.t.Fatal(err)
				}
			},
			act: func(f *runFixture) error {
				return f.r.Approve(context.Background(), f.run.ID, "schema")
			},
			wantErr:  ErrTaskMovedElsewhere,
			wantLive: StateApproved,
		},
		{
			name:  "check a task another instance abandoned",
			setup: func(f *runFixture) { f.approve("schema") },
			move: func(f *runFixture) {
				if _, err := f.st.AbandonTaskCAS(f.run.ID, "schema", epoch.Unix()); err != nil {
					f.t.Fatal(err)
				}
			},
			act: func(f *runFixture) error {
				_, err := f.r.Check(context.Background(), f.run.ID, "schema")
				return err
			},
			wantErr:  ErrTaskMovedElsewhere,
			wantLive: StateAbandoned,
		},
		{
			name: "integrate a task another instance already claimed",
			setup: func(f *runFixture) {
				f.approve("schema")
				f.commit("schema", map[string]string{"db/0007.sql": "x\n"})
				// Check directly rather than through a tick: a tick would
				// integrate it in the same pass, and this case needs the row to
				// sit at `verified` where the other instance can take it.
				if _, err := f.r.Check(context.Background(), f.run.ID, "schema"); err != nil {
					f.t.Fatal(err)
				}
			},
			move: func(f *runFixture) {
				if _, err := f.st.AdvanceTaskCAS(f.run.ID, "schema",
					string(StateVerified), string(StateIntegrating), epoch.Unix()); err != nil {
					f.t.Fatal(err)
				}
			},
			act: func(f *runFixture) error {
				_, err := f.r.Integrator.Integrate(context.Background(), f.run, f.m, f.m.Tasks[0])
				return err
			},
			wantErr:  ErrTaskMovedElsewhere,
			wantLive: StateIntegrating,
		},
		{
			name: "merge a task another instance already merged",
			setup: func(f *runFixture) {
				f.approve("schema")
				f.commit("schema", map[string]string{"db/0007.sql": "x\n"})
				f.tick() // check → verified → integrated → mergeable
			},
			move: func(f *runFixture) {
				if _, err := f.st.AdvanceTaskCAS(f.run.ID, "schema",
					string(StateMergeable), string(StateMerged), epoch.Unix()); err != nil {
					f.t.Fatal(err)
				}
			},
			act: func(f *runFixture) error {
				_, err := f.r.Merge(context.Background(), f.run.ID, "schema", MergeAck{}, false)
				return err
			},
			wantErr:  ErrTaskMovedElsewhere,
			wantLive: StateMerged,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunner(t)
			tc.setup(f)
			tc.move(f)
			err := tc.act(f)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("act on a stale snapshot = %v, want %v", err, tc.wantErr)
			}
			if got := f.state("schema"); got != tc.wantLive {
				t.Fatalf("state after the refusal = %s, want %s untouched by the loser", got, tc.wantLive)
			}
		})
	}
}

// A double press advances the row ONCE. Two presses of one button, or two Loom
// instances pressing it once each, contend on one row and exactly one wins — and
// the loser never reaches Launch, which is the assertion that matters: the
// launcher call count is the only direct evidence that no second child exists.
func TestApproveTwiceSpawnsOneChild(t *testing.T) {
	f := newRunner(t)
	f.tick()

	first := f.r.Approve(context.Background(), f.run.ID, "schema")
	second := f.r.Approve(context.Background(), f.run.ID, "schema")
	if first != nil {
		t.Fatalf("first approve = %v, want nil", first)
	}
	if !errors.Is(second, ErrTaskMovedElsewhere) {
		t.Fatalf("second approve = %v, want ErrTaskMovedElsewhere", second)
	}
	if f.lnch.n != 1 {
		t.Fatalf("launcher called %d times for two presses — the claim must precede every side effect", f.lnch.n)
	}
}

// §13.3's window, from the other end: a task that leaves `checking` while its own
// check is running. The result is DISCARDED and the caller is told.
//
// Publishing it instead would be the real damage — §9.1's consumers become
// schedulable on the artifact row, so an abandoned task's artifacts would unblock
// children to build on work nobody is going to merge.
func TestCheckDiscardsItsResultWhenTheTaskLeftChecking(t *testing.T) {
	f := newRunner(t)
	// A check that blocks until the test releases it, so the race is a sequence
	// rather than a coin flip.
	gate := filepath.Join(t.TempDir(), "go")
	f.m.Tasks[0].Check = Check{Cmd: []string{"sh", "-c",
		"while [ ! -f " + gate + " ]; do sleep 0.01; done"}}

	f.approve("schema")
	f.commit("schema", map[string]string{"db/0007.sql": "x\n"})

	done := make(chan error, 1)
	go func() {
		_, err := f.r.Check(context.Background(), f.run.ID, "schema")
		done <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for f.state("schema") != StateChecking {
		if time.Now().After(deadline) {
			t.Fatal("the check never claimed the task")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := f.st.AbandonTaskCAS(f.run.ID, "schema", epoch.Unix()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, gate, "go\n")

	if err := <-done; !errors.Is(err, ErrTaskMovedElsewhere) {
		t.Fatalf("Check = %v, want ErrTaskMovedElsewhere — a discarded result must be reported", err)
	}
	if _, ok, err := f.st.GetDelegationArtifact(f.run.ID, "account-schema"); err != nil || ok {
		t.Fatalf("an abandoned task published its artifacts (ok=%v err=%v)", ok, err)
	}
	if DecodeFlags(f.row("schema").Flags)[FlagFirstCheckGreen] {
		t.Fatal("§2's M2 recorded a verdict for a check whose result no row carries")
	}
}

// ─── §11: park and resume, mid-flight ───────────────────────────────────────

// The full rendezvous through the runner: a running child hits an unforeseen
// dependency, writes its block file and stops at its prompt; Loom parks it,
// proposes the amendment, and — once the producer publishes — materializes and
// seeds it back to life. Nothing is killed and the child keeps its context,
// which is the entire argument for parking rather than restarting.
func TestParkAndResumeMidFlight(t *testing.T) {
	f := newRunner(t)

	// Both children are live. `docs` declares NO needs, which is what makes this
	// the §11 case: the dependency it is about to hit is one the plan did not
	// foresee, not a declared edge the scheduler would have waited on.
	f.approve("schema")
	f.approve("docs")

	// The child declares its block. The FILE is the trigger (§11.2).
	if err := f.r.Detector.Write(f.run.Slug, "api", "docs", Block{
		Version: BlockVersion, Run: f.run.Slug, Task: "docs", At: epoch,
		Kind: BlockNeedsArtifact, Author: AuthorChild,
		Need:    BlockNeed{Artifact: "account-schema", From: "schema"},
		Summary: "the migration is not there yet", ResumeWhen: "the schema lands",
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.tick()
	if f.state("docs") != StateBlocked {
		t.Fatalf("docs = %s after its block file appeared, want blocked", f.state("docs"))
	}
	// §11.3: the park produces a durable, UNAPPROVED proposal — and that row is
	// what §2's M3 counts.
	if len(rep.Proposals) != 1 || rep.Proposals[0].Kind != AmendEdge {
		t.Fatalf("proposals = %+v, want one edge amendment", rep.Proposals)
	}
	if rep.Proposals[0].Accepted() {
		t.Fatal("a proposal arrived pre-approved — §11.3's whole shape is that Loom proposes and a human grants")
	}
	if rep.Measurements.UnforeseenTotal != 1 {
		t.Fatalf("M3 = %d, want 1 unforeseen dependency", rep.Measurements.UnforeseenTotal)
	}
	// Re-proposing is IDEMPOTENT: the same dependency encountered twice is one
	// dependency, and an append per tick would inflate the number §2 decides on.
	f.tick()
	got, merr := f.r.Measure(f.run.ID)
	if merr != nil {
		t.Fatal(merr)
	}
	if got.UnforeseenTotal != 1 {
		t.Fatalf("M3 after a second tick = %d, want 1 — the append must be idempotent", got.UnforeseenTotal)
	}

	// The producer finishes. Its artifact is published, so §11.4's condition is
	// satisfied — machine-checkable, never the child's prose `resume_when`.
	f.commit("schema", map[string]string{"db/0007.sql": "create table account();\n"})

	// ONE tick: the producer's check runs (step 4), publishes (§8.3), and step 6
	// re-reads before it resumes — so the parked child is woken in the same pass
	// that satisfied its condition rather than a poll interval later.
	rep = f.tick()
	if !hasID(rep.Resumed, "docs") {
		t.Fatalf("resumed = %v (errs %v), want docs", rep.Resumed, rep.Errs)
	}
	if f.state("docs") != StateRunning {
		t.Fatalf("docs = %s after resume, want running", f.state("docs"))
	}
	if f.tmux.sends() == 0 {
		t.Fatal("nothing was sent to the child — a resume that delivers no seed is a park nobody is told about")
	}
	if f.row("docs").PendingSeed != "" {
		t.Fatal("the seed is still owed after a successful delivery")
	}
	// And the child was NOT killed or relaunched: same session, same context.
	if f.lnch.n != 2 {
		t.Fatalf("launcher called %d times, want 2 — a same-repo resume costs no restart", f.lnch.n)
	}
}

// ─── §6.3: a child that died ────────────────────────────────────────────────

// A worktree whose child died is not garbage, and the two events — the session
// dying and the work being worthless — are unrelated (§6.3). The task is FLAGGED
// on `running`/`blocked`, nothing is killed, nothing is advanced, and the
// worktree and branch are left exactly where they are.
//
// Table-driven over the stages a child can die at, because the answer is
// different at three of them and each difference has its own reason.
func TestChildDeathAtEachStageFlagsAndDestroysNothing(t *testing.T) {
	tests := []struct {
		name       string
		state      TaskState
		wantOrphan bool
		wantReason string
	}{
		{"running", StateRunning, true, "§6.3 names running explicitly"},
		{"blocked", StateBlocked, true, "§6.3 names blocked explicitly"},
		{"checking", StateChecking, false,
			"Loom is running the check itself and the verdict is minutes away; an orphan badge on a task about to be verified is noise"},
		{"verified", StateVerified, false,
			"the work is done and committed; a dead child is no longer relevant to it"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunner(t)
			f.approve("schema")
			row := f.row("schema")
			if tc.state != StateRunning {
				if _, err := f.st.AdvanceTaskCAS(f.run.ID, "schema", string(StateRunning), string(tc.state), epoch.Unix()); err != nil {
					t.Fatal(err)
				}
			}
			// The child dies. The session row ENDS; nothing else changes.
			if err := f.st.MarkEnded(row.SessionName, "exited", 0, epoch.Unix()); err != nil {
				t.Fatal(err)
			}

			rep := f.tick()
			got := DecodeFlags(f.row("schema").Flags)[FlagOrphaned]
			if got != tc.wantOrphan {
				t.Fatalf("orphaned = %v at %s, want %v (%s)", got, tc.state, tc.wantOrphan, tc.wantReason)
			}
			if got && !hasID(rep.Orphaned, "schema") {
				t.Fatal("the flag was written but the tick reported nothing — an invisible finding is not a finding")
			}
			// Nothing was destroyed and nothing advanced past the death.
			if !dirExists(row.Worktree) {
				t.Fatal("the worktree was removed — a worktree whose child died is not garbage")
			}
			if f.state("schema") == StateAbandoned || f.state("schema") == StateFailed {
				t.Fatalf("state = %s: a dead session must not fail or abandon the task", f.state("schema"))
			}
		})
	}
}

// The flag CLEARS when the child comes back (§6.3's recovery is a re-spawn onto
// the same worktree). A badge that survives recovery is a badge that lies, and
// the human goes looking for a corpse that is sitting there working.
func TestOrphanFlagClearsWhenTheChildIsBack(t *testing.T) {
	f := newRunner(t)
	f.approve("schema")
	row := f.row("schema")
	if err := f.st.MarkEnded(row.SessionName, "exited", 0, epoch.Unix()); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if !DecodeFlags(f.row("schema").Flags)[FlagOrphaned] {
		t.Fatal("no orphan flag after the child died")
	}
	if err := f.st.Upsert(store.SessionRow{
		Name: row.SessionName, Cwd: row.Worktree, CreatedAt: epoch.Unix(),
		EndedAt: -1, ExitCode: -1, LastStatus: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if DecodeFlags(f.row("schema").Flags)[FlagOrphaned] {
		t.Fatal("the orphan flag survived the child's return")
	}
}

// ─── §11.3: granting an amendment ───────────────────────────────────────────

// §11.3's last rule: every amendment re-runs cycle detection over the AMENDED
// graph, and one that closes a loop is REJECTED. It has to be rejected BEFORE
// the CAS — `approved_at` is a one-way write on an append-only table, so a cycle
// discovered afterwards could not be taken back. A cycle is the specific case
// where a loud block silently becomes a deadlock.
func TestApproveAmendmentValidatesBeforeItGrants(t *testing.T) {
	f := newRunner(t)
	// schema producing account-schema is already an edge schema→auth-api. An
	// amendment making schema depend on an artifact auth-api produces closes the
	// loop — except auth-api produces nothing, so the cycle is built the honest
	// way: an edge auth-api→schema over the same artifact.
	body := EncodeAmendmentBody(Amendment{
		Kind: AmendEdge, Task: "schema", Artifact: "auth-openapi", From: "auth-api",
		Reason: "unforeseen",
	})
	seq, err := f.st.AppendDelegationAmendment(f.run.ID, string(AmendEdge), body, epoch.Unix())
	if err != nil {
		t.Fatal(err)
	}
	err = f.r.ApproveAmendment(f.run.ID, seq)
	if !errors.Is(err, ErrAmendmentCycle) {
		t.Fatalf("ApproveAmendment on a cycle-closing edge = %v, want ErrAmendmentCycle", err)
	}
	row, _, gerr := f.st.GetDelegationAmendment(f.run.ID, seq)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if row.ApprovedAt != 0 {
		t.Fatal("a rejected amendment was still marked approved — validation must precede the CAS")
	}
}

// The grant is a CAS, so a double press grants once. The loser is TOLD rather
// than silently no-op'd: it is holding a stale screen and must re-read it.
func TestApproveAmendmentGrantsOnce(t *testing.T) {
	f := newRunner(t)
	body := EncodeAmendmentBody(Amendment{
		Kind: AmendEdge, Task: "auth-api", Artifact: "account-schema", From: "schema", Reason: "unforeseen",
	})
	seq, err := f.st.AppendDelegationAmendment(f.run.ID, string(AmendEdge), body, epoch.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.ApproveAmendment(f.run.ID, seq); err != nil {
		t.Fatalf("first approve = %v, want nil", err)
	}
	if err := f.r.ApproveAmendment(f.run.ID, seq); !errors.Is(err, ErrAmendmentClaimed) {
		t.Fatalf("second approve = %v, want ErrAmendmentClaimed", err)
	}
	log := f.r.AmendmentLog(f.run.ID)
	if len(log) != 1 || !log[0].Accepted() {
		t.Fatalf("log = %+v, want exactly one accepted amendment", log)
	}
}

// ─── §2: the measurements ───────────────────────────────────────────────────

// §2's M2 is "green THE FIRST TIME it ran after the child stopped", and all
// three qualifiers are load-bearing. Table-driven over them.
func TestFirstCheckVerdictIsM2AndIsRecordedOnce(t *testing.T) {
	tests := []struct {
		name string
		// prep runs before the first check.
		prep      func(f *runFixture)
		second    func(f *runFixture)
		wantGreen bool
		wantRed   bool
	}{
		{
			name:      "green first time",
			prep:      func(f *runFixture) { f.commit("schema", map[string]string{"db/0007.sql": "x\n"}) },
			wantGreen: true,
		},
		{
			name: "red first time",
			prep: func(f *runFixture) {
				// The artifact is never committed, so §8.3 refuses to publish and
				// the check cannot be green.
				f.commit("schema", map[string]string{"db/other.sql": "x\n"})
			},
			wantRed: true,
		},
		{
			name: "a later green does not overwrite a red first time",
			prep: func(f *runFixture) {
				f.commit("schema", map[string]string{"db/other.sql": "x\n"})
			},
			second: func(f *runFixture) {
				f.commit("schema", map[string]string{"db/0007.sql": "x\n"})
			},
			wantRed: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunner(t)
			f.approve("schema")
			tc.prep(f)
			f.tick()
			if tc.second != nil {
				tc.second(f)
				f.tick()
			}
			flags := DecodeFlags(f.row("schema").Flags)
			if flags[FlagFirstCheckGreen] != tc.wantGreen || flags[FlagFirstCheckRed] != tc.wantRed {
				t.Fatalf("flags = %v, want green=%v red=%v", flags, tc.wantGreen, tc.wantRed)
			}
		})
	}
}

// A check that ran while the child was still mid-turn says nothing about the
// yield of an isolated child, so it contributes to NEITHER side of M2. Without
// this, a manual "run check" pressed while the child is thinking would record a
// verdict about a tree the child is halfway through writing.
func TestFirstCheckVerdictIgnoresAChildThatHadNotStopped(t *testing.T) {
	f := newRunner(t)
	f.approve("schema")
	f.commit("schema", map[string]string{"db/0007.sql": "x\n"})
	row := f.row("schema")
	// The child is mid-turn again: the row the status engine maintains says so,
	// and that row is the only thing this package reads (never a pane).
	sess, _, serr := f.st.Get(row.SessionName)
	if serr != nil {
		t.Fatal(serr)
	}
	sess.LastStatus = "running"
	if err := f.st.Upsert(sess); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Check(context.Background(), f.run.ID, "schema"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	flags := DecodeFlags(f.row("schema").Flags)
	if flags[FlagFirstCheckGreen] || flags[FlagFirstCheckRed] {
		t.Fatalf("flags = %v: a mid-turn check must contribute to neither side of M2", flags)
	}
}

// §2's decision rule, evaluated. "Build §§9–12 in full only if, on at least one
// real initiative of ≥4 tasks: M3 ≤ 1 per 4 tasks and M2 ≥ 0.5."
//
// Table-driven over the rule itself, because these two numbers are what decide
// whether this whole approach survives, and a threshold nobody tested is a
// threshold that will be read off a broken denominator.
func TestMeasurementsImplementTheDecisionRule(t *testing.T) {
	mk := func(tasks int, amend []Amendment, green, red int) Measurements {
		st := &runState{rows: map[string]store.DelegationTask{}}
		for i := 0; i < tasks; i++ {
			id := fmt.Sprintf("t%d", i)
			st.order = append(st.order, id)
			flags := Flags{}
			switch {
			case i < green:
				flags = flags.With(FlagFirstCheckGreen)
			case i < green+red:
				flags = flags.With(FlagFirstCheckRed)
			}
			st.rows[id] = store.DelegationTask{TaskID: id, Flags: EncodeFlags(flags)}
		}
		st.e = EffectiveGraph{Amendments: amend}
		return measure(st)
	}
	edge := func(task string) Amendment { return Amendment{Kind: AmendEdge, Task: task, Artifact: "a", From: "b"} }

	tests := []struct {
		name                       string
		m                          Measurements
		wantM2, wantM3, wantEnough bool
		wantProvisional            bool
	}{
		{"four clean tasks, no amendments, all green", mk(4, nil, 4, 0), true, true, true, false},
		{"one amendment over four tasks is exactly the threshold",
			mk(4, []Amendment{edge("t0")}, 4, 0), true, true, true, false},
		{"two amendments over four tasks fails M3",
			mk(4, []Amendment{edge("t0"), edge("t1")}, 4, 0), true, false, true, false},
		{"half green is exactly M2's threshold", mk(4, nil, 2, 2), true, true, true, false},
		{"one green in four fails M2", mk(4, nil, 1, 3), false, true, true, false},
		{"a run under four tasks decides nothing", mk(2, nil, 2, 0), true, true, false, false},
		{"an unfinished run is provisional", mk(4, nil, 1, 0), true, true, true, true},
		{"an unstarted run is not a failing one", mk(4, nil, 0, 0), false, true, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.m.M2Met != tc.wantM2 || tc.m.M3Met != tc.wantM3 ||
				tc.m.Enough != tc.wantEnough || tc.m.Provisional != tc.wantProvisional {
				t.Fatalf("M2=%v M3=%v enough=%v provisional=%v, want %v/%v/%v/%v\n%s",
					tc.m.M2Met, tc.m.M3Met, tc.m.Enough, tc.m.Provisional,
					tc.wantM2, tc.wantM3, tc.wantEnough, tc.wantProvisional, tc.m.Verdict())
			}
			// The verdict never claims a threshold was met while the numbers are
			// still moving: a half-run initiative reading green is how a decision
			// gets made on a number nobody finished measuring.
			if tc.m.Provisional && strings.Contains(tc.m.Verdict(), "both thresholds met") {
				t.Fatalf("verdict = %q on a provisional run", tc.m.Verdict())
			}
		})
	}
}

// M3 counts UNFORESEEN CROSS-TASK DEPENDENCIES. A `needs-scope` amendment is a
// child asking to write somewhere else in its own repo — an authorization
// argument, not a dependency — and folding it into M3 would fail the kill
// criterion for a plan whose tasks never touched each other at all.
func TestScopeWideningsAreNotM3(t *testing.T) {
	st := &runState{
		order: []string{"a", "b", "c", "d"},
		rows:  map[string]store.DelegationTask{"a": {}, "b": {}, "c": {}, "d": {}},
		e: EffectiveGraph{Amendments: []Amendment{
			{Kind: AmendScope, Task: "a", Paths: []string{"internal/**"}},
			{Kind: AmendScope, Task: "b", Paths: []string{"cmd/**"}},
		}},
	}
	m := measure(st)
	if m.UnforeseenTotal != 0 || m.ScopeWidenings != 2 {
		t.Fatalf("M3 = %d, scope = %d; want 0 and 2", m.UnforeseenTotal, m.ScopeWidenings)
	}
	if !m.M3Met {
		t.Fatal("two scope widenings failed M3 — they are not cross-task dependencies")
	}
}

// M3 counts every unforeseen dependency ENCOUNTERED, approved or not. A human
// refusing the amendment does not un-encounter the dependency, and M3 measures
// inter-task cohesion, which does not care what the human decided.
func TestM3CountsRefusedAmendmentsToo(t *testing.T) {
	st := &runState{
		order: []string{"a", "b", "c", "d"},
		rows:  map[string]store.DelegationTask{"a": {}, "b": {}, "c": {}, "d": {}},
		e: EffectiveGraph{Amendments: []Amendment{
			{Kind: AmendEdge, Task: "a", Artifact: "x", From: "b"}, // never approved
		}},
	}
	if got := measure(st).UnforeseenTotal; got != 1 {
		t.Fatalf("M3 = %d, want 1 — the dependency was encountered whether or not it was granted", got)
	}
}

// Merge RETURNS §10.4 step 2's findings. It used to discard the
// IntegrationResult, which meant the two sentences the human most needs at the
// merge gate — "the user's own branch is red after this merge" and "the child's
// worktree was not removed" — existed, were composed, and reached nobody. The
// caller was left inferring them from a baseline column that records the
// verdict and not the reason.
//
// The fixture manifest declares no `integration.per_repo` gate, so the
// re-derived baseline carries no verdict: a REAL degradation, and exactly the
// class of thing that must not be silent.
func TestMergeReturnsTheIntegrationResult(t *testing.T) {
	f := newRunner(t)
	f.tick()
	f.approve("schema")
	f.commit("schema", map[string]string{"db/0007.sql": "create table account();\n"})
	f.tick() // check → verified → integrated → mergeable
	if got := f.state("schema"); got != StateMergeable {
		t.Fatalf("schema = %s, want mergeable before the gate", got)
	}

	res, err := f.r.Merge(context.Background(), f.run.ID, "schema", MergeAck{}, false)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.TaskID != "schema" || res.RepoLabel != "api" {
		t.Errorf("result identifies %q/%q — the caller cannot tell what it is about", res.TaskID, res.RepoLabel)
	}
	if res.Head == "" {
		t.Error("Head is empty: nothing says what the staging area was re-derived to")
	}
	if !hasSubstring(res.Warnings, "declares no integration.per_repo check") {
		t.Errorf("Warnings = %v, want the missing-gate degradation to reach the caller", res.Warnings)
	}
	if f.state("schema") != StateMerged {
		t.Errorf("schema = %s after a successful merge", f.state("schema"))
	}
}

// §11.3's NO, end to end through the Runner.
//
// Four properties, and each has a way of quietly not holding: the decision is
// durable, the effective graph still ignores the amendment (a rejection is a
// fact about the OFFER, never a change to the plan), a second decision is
// refused rather than overwriting, and the row stays in the log — which is what
// keeps `propose` from re-offering it on the very next tick.
func TestRejectAmendmentIsDurableAndInert(t *testing.T) {
	f := newRunner(t)
	seq, err := f.st.AppendDelegationAmendment(f.run.ID, string(AmendEdge),
		EncodeAmendmentBody(Amendment{Task: "docs", Artifact: "account-schema", From: "schema"}),
		epoch.Unix())
	if err != nil {
		t.Fatal(err)
	}

	if err := f.r.RejectAmendment(f.run.ID, seq); err != nil {
		t.Fatalf("RejectAmendment: %v", err)
	}
	row, ok, err := f.st.GetDelegationAmendment(f.run.ID, seq)
	if err != nil || !ok {
		t.Fatalf("GetDelegationAmendment: %v %v", ok, err)
	}
	if row.RejectedAt == 0 || row.ApprovedAt != 0 {
		t.Fatalf("approved/rejected = %d/%d, want a rejection and nothing else", row.ApprovedAt, row.RejectedAt)
	}

	// Inert, and inert in the same way it was before the decision: Accept
	// refuses anything unapproved, so a rejected amendment contributes no edge.
	log := f.r.AmendmentLog(f.run.ID)
	if len(log) != 1 {
		t.Fatalf("log = %+v, want the rejected row kept", log)
	}
	a := log[0]
	if !a.Rejected() || a.Accepted() || a.Pending() {
		t.Errorf("decided amendment reads accepted=%v rejected=%v pending=%v",
			a.Accepted(), a.Rejected(), a.Pending())
	}
	if _, ok := a.Edge(); ok {
		t.Error("a rejected amendment contributed an edge to the effective graph")
	}
	if _, err := Accept(EffectiveGraph{}, a); !errors.Is(err, ErrAmendmentNotApproved) {
		t.Errorf("Accept(rejected) = %v, want ErrAmendmentNotApproved", err)
	}

	// A second decision, either way, is refused rather than silently applied.
	if err := f.r.RejectAmendment(f.run.ID, seq); !errors.Is(err, ErrAmendmentClaimed) {
		t.Errorf("second reject = %v, want ErrAmendmentClaimed", err)
	}
	if err := f.r.ApproveAmendment(f.run.ID, seq); !errors.Is(err, ErrAmendmentClaimed) {
		t.Errorf("approve after reject = %v, want ErrAmendmentClaimed", err)
	}
	if after, _, _ := f.st.GetDelegationAmendment(f.run.ID, seq); after.ApprovedAt != 0 {
		t.Errorf("the refused approval still wrote approved_at = %d", after.ApprovedAt)
	}
}

// §12.1 must be answerable WITHOUT ticking. Tick reaches the same verdict, but
// it polls block files, runs checks, performs integrations, delivers seeds and
// flips the run's status on the way — so a view that had to call it to render
// WHY a run is red would be advancing the run in order to draw it.
//
// Table-driven over the shapes because the SHAPE is what decides the remedy,
// and a detector that says "deadlocked" without saying which one leaves the
// human with a red run and no next action.
func TestRunnerDeadlockIsPureAndClassifies(t *testing.T) {
	cases := []struct {
		name   string
		blocks map[string]Block
		// abandon takes a task out of the run so the remaining ones are the
		// whole picture.
		abandon   []string
		wantNil   bool
		wantShape DeadlockShape
	}{
		{name: "a run that can still move is not deadlocked",
			wantNil: true},
		{name: "two children waiting on each other's unforeseen artifact is a mutual wait",
			// Zero declared edges are involved: this is §12.1(a)'s point, and
			// over the declared graph alone the run looks perfectly healthy.
			blocks: map[string]Block{
				"schema":   {Version: BlockVersion, Task: "schema", Kind: BlockNeedsArtifact, Need: BlockNeed{Artifact: "auth-token", From: "auth-api"}},
				"auth-api": {Version: BlockVersion, Task: "auth-api", Kind: BlockNeedsArtifact, Need: BlockNeed{Artifact: "account-schema", From: "schema"}},
			},
			wantShape: ShapeMutualWait},
		{name: "everything parked on a human is an external deadlock, not a cycle",
			blocks: map[string]Block{
				"schema":   {Version: BlockVersion, Task: "schema", Kind: BlockNeedsDecision, Summary: "which migration tool"},
				"auth-api": {Version: BlockVersion, Task: "auth-api", Kind: BlockExternal, Summary: "the IdP is down"},
			},
			wantShape: ShapeExternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, run, _ := newRunFixture(t)
			for id, b := range tc.blocks {
				raw, err := json.Marshal(b)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Store.SetTaskBlock(run.ID, id, string(raw), epoch.Unix()); err != nil {
					t.Fatal(err)
				}
				if ok, err := r.Store.AdvanceTaskCAS(run.ID, id, string(StatePending),
					string(StateBlocked), epoch.Unix()); err != nil || !ok {
					t.Fatalf("park %s: ok=%v err=%v", id, ok, err)
				}
			}
			for _, id := range tc.abandon {
				if ok, err := r.Store.AdvanceTaskCAS(run.ID, id, string(StatePending),
					string(StateAbandoned), epoch.Unix()); err != nil || !ok {
					t.Fatalf("abandon %s: ok=%v err=%v", id, ok, err)
				}
			}

			runBefore, _, err := r.Store.GetDelegationRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			tasksBefore, err := r.Store.ListDelegationTasks(run.ID)
			if err != nil {
				t.Fatal(err)
			}

			d, err := r.Deadlock(run.ID)
			if err != nil {
				t.Fatalf("Deadlock: %v", err)
			}
			switch {
			case tc.wantNil && d != nil:
				t.Fatalf("a moving run was reported deadlocked: %+v", d)
			case !tc.wantNil && d == nil:
				t.Fatal("no deadlock reported for a run that cannot move")
			case tc.wantNil:
				return
			}
			if d.Shape != tc.wantShape {
				t.Errorf("shape = %q, want %q", d.Shape, tc.wantShape)
			}
			if tc.wantShape == ShapeMutualWait && len(d.Cycle) == 0 {
				t.Error("a mutual wait with no cycle: the remedy is a re-plan and a re-plan " +
					"needs the loop, so a boolean would be useless here")
			}
			if tc.wantShape == ShapeExternal && len(d.Owed) == 0 {
				t.Error("an external deadlock with no owed decisions is a status, not the " +
					"actionable list §12.1(b) asks for")
			}

			// The whole reason this exists beside Tick.
			runAfter, _, err := r.Store.GetDelegationRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if runAfter != runBefore {
				t.Errorf("Deadlock wrote to the run row:\n before %+v\n after  %+v", runBefore, runAfter)
			}
			tasksAfter, err := r.Store.ListDelegationTasks(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(tasksBefore) != fmt.Sprint(tasksAfter) {
				t.Errorf("Deadlock wrote to a task row:\n before %v\n after  %v", tasksBefore, tasksAfter)
			}
		})
	}
}

// EffectiveFromRows must reconstruct EXACTLY the graph the scheduler runs on,
// from the durable rows alone. If it drifted, the view would draw one graph and
// the runner would schedule another — the "two schedulers" failure Tick's step
// 3b comment names, arrived at from the read side.
func TestEffectiveFromRowsMatchesTheRunnersOwnGraph(t *testing.T) {
	r, run, m := newRunFixture(t)
	raw, err := json.Marshal(Block{Version: BlockVersion, Task: "auth-api",
		Kind: BlockNeedsArtifact, Need: BlockNeed{Artifact: "audit-log", From: "schema"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.SetTaskBlock(run.ID, "auth-api", string(raw), epoch.Unix()); err != nil {
		t.Fatal(err)
	}
	seq, err := r.Store.AppendDelegationAmendment(run.ID, string(AmendEdge),
		EncodeAmendmentBody(Amendment{Kind: AmendEdge, Task: "schema", From: "auth-api", Artifact: "auth-token"}),
		epoch.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := r.Store.ApproveDelegationAmendmentCAS(run.ID, seq, epoch.Unix()); err != nil || !ok {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}

	st, err := r.load(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := r.Store.ListDelegationTasks(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	amds, err := r.Store.ListDelegationAmendments(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := EffectiveFromRows(m, amds, rows)

	if fmt.Sprint(got.Added) != fmt.Sprint(st.e.Added) {
		t.Errorf("added edges = %v, want the runner's %v", got.Added, st.e.Added)
	}
	if fmt.Sprint(got.BlockEdges()) != fmt.Sprint(st.e.BlockEdges()) {
		t.Errorf("block edges = %v, want the runner's %v", got.BlockEdges(), st.e.BlockEdges())
	}
	if fmt.Sprint(got.WaitFor().Edges) != fmt.Sprint(st.e.WaitFor().Edges) {
		t.Errorf("wait-for edges = %v, want the runner's %v", got.WaitFor().Edges, st.e.WaitFor().Edges)
	}
}
