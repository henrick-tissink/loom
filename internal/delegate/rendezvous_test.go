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

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
)

// fakeSender is the tmux seam. The SEND COUNT is the assertion that matters in
// this file: "the seed was delivered once" is not observable from the database
// alone, because a double delivery clears the column exactly as a single one
// does. Only counting what reached the pane can tell them apart.
type fakeSender struct {
	mu       sync.Mutex
	literals []string
	enters   int
	killed   []string
	err      error
	// onSend runs inside the send, so a test can assert what was TRUE ON DISK at
	// the moment the child was told about it — which is the whole content of
	// §11.4's materialize-then-seed ordering.
	onSend func(text string)
}

func (f *fakeSender) SendLiteral(session, s string) error {
	f.mu.Lock()
	f.literals = append(f.literals, s)
	hook := f.onSend
	f.mu.Unlock()
	if hook != nil {
		hook(s)
	}
	return f.err
}

func (f *fakeSender) SendEnter(session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enters++
	return f.err
}

func (f *fakeSender) KillSession(session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, session)
	return nil
}

func (f *fakeSender) sends() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.literals)
}

func (f *fakeSender) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.literals) == 0 {
		return ""
	}
	return f.literals[len(f.literals)-1]
}

// fakeResumer records the cross-repo relaunch. Resume and not Launch, because a
// Launch would start a fresh claude and lose the context the park exists to keep.
type fakeResumer struct {
	calls []store.SessionRow
	err   error
}

func (f *fakeResumer) Resume(old store.SessionRow, w, h int, now time.Time) (string, error) {
	f.calls = append(f.calls, old)
	if f.err != nil {
		return "", f.err
	}
	return "loom-child-resumed", nil
}

// rzFixture is one blocked consumer with a live child, a transcript the continue
// gate will accept, and a producer task beside it.
type rzFixture struct {
	st       *store.Store
	run      store.DelegationRun
	m        Manifest
	task     Task
	tmux     *fakeSender
	resumer  *fakeResumer
	r        *Rendezvous
	cwd      string
	claudeID string
	session  string
}

func newRzFixture(t *testing.T, gate transcript.State) *rzFixture {
	t.Helper()
	st := openStore(t)
	run, err := st.InsertDelegationRun("atlas", "/w/innostream", "{}", "{}", 1000)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Name: "atlas",
		Tasks: []Task{
			{ID: "schema", Repo: "bankenstein", Produces: []Artifact{{ID: "account-schema", Path: "db/0007.sql"}}},
			{ID: "auth-api", Repo: "bankenstein", Needs: []string{"account-schema"}},
			{ID: "ballista-client", Repo: "ballista", Needs: []string{"account-schema"}},
		},
	}
	if err := st.InsertDelegationTask(store.DelegationTask{
		RunID: run.ID, TaskID: "schema", State: string(StateVerified), RepoLabel: "bankenstein",
		Branch: BranchName(run.Slug, "schema"), UpdatedAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	// The consumer is walked through the real transitions rather than inserted
	// at `blocked` with a session name written behind the store's back: the CAS
	// pair is the only writer of session_name, and a fixture that bypasses it
	// would be testing a row shape the running system cannot produce.
	if err := st.InsertDelegationTask(store.DelegationTask{
		RunID: run.ID, TaskID: "auth-api", State: string(StateApproved), RepoLabel: "bankenstein", UpdatedAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	ccd := t.TempDir()
	claudeID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	writeRzTranscript(t, ccd, cwd, claudeID, gate)
	name := "loom-child-1"
	if err := st.Upsert(store.SessionRow{
		Name: name, ClaudeSessionID: claudeID, ProjectLabel: "bankenstein", Cwd: cwd,
		CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	layout := NewLayout(t.TempDir())
	// approved → spawning → running → blocked, all through the CAS API. The
	// worktree is left empty on purpose: Materialize falls back to the
	// deterministic layout path, which is where the git tests below build it.
	if claimed, err := st.ClaimTaskSpawnCAS(run.ID, "auth-api", "", BranchName(run.Slug, "auth-api"), "", "", 8, 1000); err != nil || !claimed {
		t.Fatalf("ClaimTaskSpawnCAS: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := st.BindTaskSessionCAS(run.ID, "auth-api", name, 1000); err != nil || !claimed {
		t.Fatalf("BindTaskSessionCAS: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := st.AdvanceTaskCAS(run.ID, "auth-api", string(StateRunning), string(StateBlocked), 1000); err != nil || !claimed {
		t.Fatalf("park: claimed=%v err=%v", claimed, err)
	}

	if err := os.MkdirAll(layout.MetaDir(run.Slug, "bankenstein", "auth-api"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmux := &fakeSender{}
	resumer := &fakeResumer{}
	det := &Detector{Layout: layout, Store: st, Now: func() time.Time { return time.Unix(2000, 0) }}
	return &rzFixture{
		st: st, run: run, m: m, task: m.Tasks[1], tmux: tmux, resumer: resumer,
		cwd: cwd, claudeID: claudeID, session: name,
		r: &Rendezvous{
			Store: st, Layout: layout, Detector: det, Tmux: tmux, Resumer: resumer,
			ClaudeConfigDir: ccd,
			PollEvery:       10 * time.Millisecond,
			Timeout:         200 * time.Millisecond,
			Now:             func() time.Time { return time.Unix(2000, 0) },
		},
	}
}

// writeRzTranscript writes a transcript whose TAIL classifies as the wanted
// state. Shape, never string: the lines are real transcript records and the
// classifier decides — matching claude's rendered UI by string is the mistake
// this codebase does not make twice.
func writeRzTranscript(t *testing.T, ccd, cwd, sessionID string, want transcript.State) {
	t.Helper()
	lines := []string{
		fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"read your brief"},"cwd":%q,"timestamp":"2026-07-22T00:00:00Z"}`, cwd),
	}
	if want != transcript.StateIdle {
		// A pending tool_use with no result classifies as Running: the child is
		// mid-turn and typed text would be swallowed.
		lines = append(lines, fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]},"cwd":%q,"timestamp":"2026-07-22T00:00:05Z"}`, cwd))
	}
	dir := filepath.Join(ccd, "projects", transcript.ProjectDirName(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *rzFixture) row(t *testing.T) store.DelegationTask {
	t.Helper()
	row, ok, err := f.st.GetDelegationTask(f.run.ID, "auth-api")
	if err != nil || !ok {
		t.Fatalf("task row missing (ok=%v err=%v)", ok, err)
	}
	return row
}

func (f *rzFixture) writeBlockFile(t *testing.T, b Block) {
	t.Helper()
	det := &Detector{Layout: f.r.Layout, Now: func() time.Time { return time.Unix(2000, 0) }}
	if err := det.Write(f.run.Slug, "bankenstein", "auth-api", b); err != nil {
		t.Fatal(err)
	}
}

// §11.4's happy path, end to end: the seed is delivered, the declaration is gone,
// the debt badge is cleared and the task is running again.
func TestResumeDeliversTheSeedAndUnparksTheTask(t *testing.T) {
	f := newRzFixture(t, transcript.StateIdle)
	b := Block{Version: BlockVersion, Task: "auth-api", Kind: BlockNeedsDecision, Summary: "which tenancy model"}
	f.writeBlockFile(t, b)

	if err := f.r.Resume(f.run, f.m, f.task, b); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if f.tmux.sends() != 1 || f.tmux.enters != 1 {
		t.Fatalf("sends = %d, enters = %d, want 1 and 1 (a literal nobody pressed Enter on is not delivered)", f.tmux.sends(), f.tmux.enters)
	}
	row := f.row(t)
	if row.State != string(StateRunning) {
		t.Fatalf("state = %q, want running", row.State)
	}
	if row.PendingSeed != "" {
		t.Fatalf("pending_seed = %q, want cleared", row.PendingSeed)
	}
	if DecodeFlags(row.Flags)[FlagSeedPending] {
		t.Fatalf("flags = %q, want seed-pending cleared", row.Flags)
	}
	if row.BlockJSON != "" {
		t.Fatalf("block_json = %q, want cleared", row.BlockJSON)
	}
	if _, err := os.Stat(f.r.Layout.BlockPath(f.run.Slug, "bankenstein", "auth-api")); !os.IsNotExist(err) {
		t.Fatalf("block.json still on disk (err=%v)", err)
	}
}

// BINDING (§11.4): the delivery is exactly once even when two deliverers hold the
// same snapshot — workflow's disclosed race, an async delivery against a
// user-triggered retry, or simply the second Loom instance.
//
// This is the test the claim-first ordering exists for. Under workflow's
// send-then-clear both callers pass the re-read and both send into a live agent's
// prompt; ClearTaskPendingSeedCAS makes the clear the claim, so exactly one can
// win it.
func TestResumeSeedIsDeliveredExactlyOnceUnderAConcurrentDeliverer(t *testing.T) {
	f := newRzFixture(t, transcript.StateIdle)
	const seed = "`account-schema` is now present at `db/0007.sql` in your worktree. Continue."
	if err := f.st.SetTaskPendingSeed(f.run.ID, "auth-api", seed, 2000); err != nil {
		t.Fatal(err)
	}
	row := f.row(t)
	sess, ok, err := f.st.Get(f.session)
	if err != nil || !ok {
		t.Fatalf("session row missing (ok=%v err=%v)", ok, err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = f.r.deliver(sess, row)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("deliver[%d] = %v, want nil (the loser no-ops, it does not fail)", i, err)
		}
	}
	if n := f.tmux.sends(); n != 1 {
		t.Fatalf("sent %d times, want exactly 1", n)
	}
	if got := f.row(t).PendingSeed; got != "" {
		t.Fatalf("pending_seed = %q, want cleared once", got)
	}
}

// §12.2's rule made concrete: a park whose child died is REPORTED, and the seed
// stays owed. Silently retrying forever renders as a healthy "seed pending" with
// nobody told that the thing it is pending on no longer exists.
func TestParkedChildWithADeadSessionIsReportedAndTheSeedStaysOwed(t *testing.T) {
	f := newRzFixture(t, transcript.StateIdle)
	if err := f.st.MarkEnded(f.session, "ended", 0, 1500); err != nil {
		t.Fatal(err)
	}
	b := Block{Version: BlockVersion, Task: "auth-api", Kind: BlockNeedsDecision}
	f.writeBlockFile(t, b)

	err := f.r.Resume(f.run, f.m, f.task, b)
	if !errors.Is(err, ErrChildGone) {
		t.Fatalf("Resume = %v, want ErrChildGone", err)
	}
	if n := f.tmux.sends(); n != 0 {
		t.Fatalf("sent %d times into a dead session", n)
	}
	row := f.row(t)
	if row.PendingSeed == "" {
		t.Fatal("pending_seed was cleared; the seed is owed until it is delivered")
	}
	if !DecodeFlags(row.Flags)[FlagSeedPending] {
		t.Fatalf("flags = %q, want seed-pending", row.Flags)
	}
	if row.State != string(StateBlocked) {
		t.Fatalf("state = %q, want blocked", row.State)
	}
	if _, err := os.Stat(f.r.Layout.BlockPath(f.run.Slug, "bankenstein", "auth-api")); err != nil {
		t.Fatalf("block.json was removed for an undelivered seed: %v", err)
	}
}

// A gate timeout is explicitly NOT a failure that clears the seed (§11.4). The
// child was mid-turn; the debt survives and §12.2 offers the retry.
func TestParkedChildMidTurnKeepsTheSeedOwedAndSendsNothing(t *testing.T) {
	f := newRzFixture(t, transcript.StateRunning)
	err := f.r.Seed(f.run, "auth-api", "Continue.")
	if !errors.Is(err, ErrSeedUndelivered) {
		t.Fatalf("Seed = %v, want ErrSeedUndelivered", err)
	}
	if n := f.tmux.sends(); n != 0 {
		t.Fatalf("sent %d times into a mid-turn child", n)
	}
	row := f.row(t)
	if row.PendingSeed != "Continue." {
		t.Fatalf("pending_seed = %q, want the seed still owed", row.PendingSeed)
	}
	if !DecodeFlags(row.Flags)[FlagSeedPending] {
		t.Fatalf("flags = %q, want seed-pending", row.Flags)
	}
}

// §11.4 step 1: the machine-checkable condition, derived from the block's Need
// and the graph — never the child's prose `resume_when`.
func TestRendezvousUnblockedIgnoresProseAndReadsTheGraph(t *testing.T) {
	m := Manifest{
		Name: "atlas",
		Tasks: []Task{
			{ID: "schema", Repo: "b", Produces: []Artifact{{ID: "account-schema", Path: "db/0007.sql"}}},
			{ID: "auth-api", Repo: "b", Needs: []string{"account-schema"}},
		},
	}
	block := Block{Task: "auth-api", Kind: BlockNeedsArtifact,
		Need:       BlockNeed{Artifact: "account-schema", From: "schema"},
		ResumeWhen: "when the schema is ready, which it definitely is"}

	tests := []struct {
		name      string
		block     Block
		state     TaskState
		published bool
		want      bool
	}{
		{"verified and published", block, StateVerified, true, true},
		{"merged counts too", block, StateMerged, true, true},
		// The four states graph.go's needsMet names, and the reason they must be
		// the same four: a producer moving verified → integrating → mergeable is
		// making PROGRESS, and a consumer's park that answers false through that
		// window is a rendezvous the producer's own progress delayed.
		{"integrating counts too — readiness is monotonic", block, StateIntegrating, true, true},
		{"mergeable counts too — readiness is monotonic", block, StateMergeable, true, true},
		{"published but the producer is still running", block, StateRunning, true, false},
		{"verified but nothing published", block, StateVerified, false, false},
		{"a failed producer does not unblock", block, StateFailed, true, false},
		{"needs-decision is never machine-unblocked",
			Block{Task: "auth-api", Kind: BlockNeedsDecision}, StateVerified, true, false},
		{"needs-scope is never machine-unblocked",
			Block{Task: "auth-api", Kind: BlockNeedsScope}, StateVerified, true, false},
		{"blocked-external is never machine-unblocked",
			Block{Task: "auth-api", Kind: BlockExternal}, StateVerified, true, false},
		{"an artifact nobody produces never unblocks",
			Block{Task: "auth-api", Kind: BlockNeedsArtifact, Need: BlockNeed{Artifact: "tenant-model"}},
			StateVerified, true, false},
	}
	var r Rendezvous
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := Effective(m, nil, map[string]Block{"auth-api": tc.block})
			e.Blocks = map[string]Block{"auth-api": tc.block}
			states := map[string]TaskState{"schema": tc.state, "auth-api": StateBlocked}
			published := map[string]bool{}
			if tc.published {
				published["account-schema"] = true
			}
			if got := r.Unblocked(e, states, published, "auth-api"); got != tc.want {
				t.Fatalf("Unblocked = %v, want %v", got, tc.want)
			}
		})
	}
}

// §11.4's load-bearing ORDER: materialize, THEN seed. The assertion runs INSIDE
// the send — at the moment the child is told the artifact is present, it must
// actually be present. Seeding first sends a statement that is false when it is
// read, and the child burns turns discovering that.
func TestResumeMaterializesTheProducerBeforeSeeding(t *testing.T) {
	f := newRzFixture(t, transcript.StateIdle)
	repo := scratchRepo(t)
	base := headSHA(t, repo)

	// The producer's branch: the artifact, committed.
	gitIn(t, repo, "checkout", "-q", "-b", BranchName(f.run.Slug, "schema"))
	write(t, filepath.Join(repo, "db", "0007.sql"), "create table account (tenant_id text);\n")
	gitIn(t, repo, "add", "db/0007.sql")
	gitIn(t, repo, "commit", "-qm", "schema: account table")
	producerSHA := headSHA(t, repo)
	gitIn(t, repo, "checkout", "-q", base)

	// The consumer's worktree, branched from the run's base without the
	// artifact, at the deterministic layout path Materialize falls back to.
	consumer := f.r.Layout.Dir(f.run.Slug, "bankenstein", "auth-api")
	if err := os.MkdirAll(filepath.Dir(consumer), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "worktree", "add", "-q", "-B", BranchName(f.run.Slug, "auth-api"), consumer, base)
	if err := f.st.SetTaskBranchHead(f.run.ID, "schema", producerSHA, 2000); err != nil {
		t.Fatal(err)
	}

	artifactInWorktree := filepath.Join(consumer, "db", "0007.sql")
	var presentAtSendTime bool
	f.tmux.onSend = func(string) {
		_, err := os.Stat(artifactInWorktree)
		presentAtSendTime = err == nil
	}

	b := Block{Version: BlockVersion, Task: "auth-api", Kind: BlockNeedsArtifact,
		Need: BlockNeed{Artifact: "account-schema", From: "schema"}}
	f.writeBlockFile(t, b)

	if err := f.r.Resume(f.run, f.m, f.task, b); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !presentAtSendTime {
		t.Fatal("the seed was sent before the artifact existed in the worktree")
	}
	if !strings.Contains(f.tmux.last(), "db/0007.sql") {
		t.Fatalf("seed = %q, want it to name the artifact's path", f.tmux.last())
	}
	if f.row(t).State != string(StateRunning) {
		t.Fatalf("state = %q, want running", f.row(t).State)
	}
}

// §11.4: if step 2 conflicts, the seed says SO and the task stays blocked. It
// does not become a silent failure and it does not become a lie.
func TestResumeKeepsTheTaskParkedWhenMaterializationConflicts(t *testing.T) {
	f := newRzFixture(t, transcript.StateIdle)
	repo := scratchRepo(t)
	base := headSHA(t, repo)

	gitIn(t, repo, "checkout", "-q", "-b", BranchName(f.run.Slug, "schema"))
	write(t, filepath.Join(repo, "db", "0007.sql"), "producer's version\n")
	gitIn(t, repo, "add", "db/0007.sql")
	gitIn(t, repo, "commit", "-qm", "schema: account table")
	producerSHA := headSHA(t, repo)
	gitIn(t, repo, "checkout", "-q", base)

	consumer := f.r.Layout.Dir(f.run.Slug, "bankenstein", "auth-api")
	if err := os.MkdirAll(filepath.Dir(consumer), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "worktree", "add", "-q", "-B", BranchName(f.run.Slug, "auth-api"), consumer, base)
	write(t, filepath.Join(consumer, "db", "0007.sql"), "consumer's version\n")
	gitIn(t, consumer, "add", "db/0007.sql")
	gitIn(t, consumer, "commit", "-qm", "auth-api: my own guess at the schema")

	if err := f.st.SetTaskBranchHead(f.run.ID, "schema", producerSHA, 2000); err != nil {
		t.Fatal(err)
	}

	b := Block{Version: BlockVersion, Task: "auth-api", Kind: BlockNeedsArtifact,
		Need: BlockNeed{Artifact: "account-schema", From: "schema"}}
	f.writeBlockFile(t, b)

	err := f.r.Resume(f.run, f.m, f.task, b)
	var conflict *ProducerConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("Resume = %v, want a *ProducerConflict", err)
	}
	row := f.row(t)
	if row.State != string(StateBlocked) {
		t.Fatalf("state = %q, want blocked", row.State)
	}
	if !strings.Contains(f.tmux.last(), "db/0007.sql") || !strings.Contains(f.tmux.last(), "CONFLICT") {
		t.Fatalf("seed = %q, want a conflict description naming the file", f.tmux.last())
	}
	if _, err := os.Stat(f.r.Layout.BlockPath(f.run.Slug, "bankenstein", "auth-api")); err != nil {
		t.Fatalf("block.json was removed for a task that is still blocked: %v", err)
	}
}

// §11.3: a needs-scope amendment is HUMAN-APPROVED and rewrites brief.md in
// place. The unapproved half is the one that matters — Loom must not widen a
// child's authorization on an agent's say-so, and the evidence is that the file
// on disk is byte-identical and nothing was sent.
func TestAmendScopeIsNeverAutoGrantedAndRewritesTheBriefInPlace(t *testing.T) {
	f := newRzFixture(t, transcript.StateIdle)
	briefPath := f.r.Layout.BriefPath(f.run.Slug, "bankenstein", "auth-api")
	const original = "# auth-api\n\n## 2. Authorization\n\nYou may modify internal/auth only.\n"
	write(t, briefPath, original)
	f.writeBlockFile(t, Block{Version: BlockVersion, Task: "auth-api", Kind: BlockNeedsScope,
		Paths: []string{"internal/db/**"}})

	proposal := Amendment{Kind: AmendScope, Task: "auth-api", Paths: []string{"internal/db/**"},
		Reason: "the session table lives outside internal/auth"}

	if err := f.r.ApplyScope(f.run, f.task, proposal); !errors.Is(err, ErrAmendmentNotApproved) {
		t.Fatalf("ApplyScope = %v, want ErrAmendmentNotApproved", err)
	}
	if got, _ := os.ReadFile(briefPath); string(got) != original {
		t.Fatalf("the brief was rewritten for an UNAPPROVED amendment:\n%s", got)
	}
	if n := f.tmux.sends(); n != 0 {
		t.Fatalf("sent %d times for an unapproved amendment", n)
	}
	if f.row(t).State != string(StateBlocked) {
		t.Fatalf("state = %q, want the task still parked", f.row(t).State)
	}

	proposal.ApprovedAt = time.Unix(3000, 0)
	if err := f.r.ApplyScope(f.run, f.task, proposal); err != nil {
		t.Fatalf("ApplyScope after approval: %v", err)
	}
	amended, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(amended), original) {
		t.Fatalf("the original authorization was not preserved:\n%s", amended)
	}
	if !strings.Contains(string(amended), "internal/db/**") {
		t.Fatalf("the grant is not in the brief:\n%s", amended)
	}
	if n := f.tmux.sends(); n != 1 {
		t.Fatalf("sends = %d, want the child re-seeded exactly once", n)
	}
	if !strings.Contains(f.tmux.last(), "internal/db/**") {
		t.Fatalf("seed = %q, want it to name the widened paths", f.tmux.last())
	}
	if f.row(t).State != string(StateRunning) {
		t.Fatalf("state = %q, want running", f.row(t).State)
	}
}

// The cross-repo unblock is a RESTART, and the restart must be a RESUME: an
// add-dir cannot be added to a live session, and a fresh launch would throw away
// the context that made parking cheaper than restarting in the first place.
func TestResumeCrossRepoRelaunchesByResumingTheSameConversation(t *testing.T) {
	f := newRzFixture(t, transcript.StateIdle)
	// ballista-client consumes account-schema from another repo.
	if err := f.st.InsertDelegationTask(store.DelegationTask{
		RunID: f.run.ID, TaskID: "ballista-client", State: string(StateBlocked),
		RepoLabel: "ballista", SessionName: f.session, UpdatedAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	consumer := f.m.Tasks[2]
	if err := os.MkdirAll(f.r.Layout.MetaDir(f.run.Slug, "ballista", "ballista-client"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := Block{Version: BlockVersion, Task: "ballista-client", Kind: BlockNeedsArtifact,
		Need: BlockNeed{Artifact: "account-schema", From: "schema"}}

	mat, err := f.r.Materialize(f.run, f.m, consumer, b)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if !mat.Relaunched || len(f.resumer.calls) != 1 {
		t.Fatalf("Relaunched = %v, resumes = %d, want a single resume", mat.Relaunched, len(f.resumer.calls))
	}
	resumed := f.resumer.calls[0]
	if resumed.ClaudeSessionID != f.claudeID {
		t.Fatalf("resumed claude_session_id = %q, want the child's own %q (the context is the point)", resumed.ClaudeSessionID, f.claudeID)
	}
	if !strings.Contains(resumed.AddDirs, "__integration") {
		t.Fatalf("add_dirs = %q, want the producer's integration worktree", resumed.AddDirs)
	}
	if len(f.tmux.killed) != 1 {
		t.Fatalf("killed = %v, want the old child ended before the new one exists", f.tmux.killed)
	}
	old, _, err := f.st.Get(f.session)
	if err != nil {
		t.Fatal(err)
	}
	if old.EndedAt == -1 {
		t.Fatal("the old session row is still live; it would count against the cap forever")
	}
	row, _, err := f.st.GetDelegationTask(f.run.ID, "ballista-client")
	if err != nil {
		t.Fatal(err)
	}
	if row.SessionName != "loom-child-resumed" || row.State != string(StateRunning) {
		t.Fatalf("row = (%q, %q), want the resumed session bound and running", row.SessionName, row.State)
	}
}
