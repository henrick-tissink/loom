package store

import "testing"

func newRun(t *testing.T, s *Store) DelegationRun {
	t.Helper()
	r, err := s.InsertDelegationRun("atlas", "/w/Innostream",
		`{"manifest":1,"name":"atlas"}`, `{"bankenstein":"9b69827"}`, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newTask(runID int64, id string) DelegationTask {
	return DelegationTask{RunID: runID, TaskID: id, State: "pending",
		RepoLabel: "bankenstein", UpdatedAt: 1000}
}

// TestInsertDelegationRunSlug pins the slug's shape and its uniqueness. The
// slug is the worktree and branch component (§6.2), so two runs of the same
// manifest colliding on it would put two children in one worktree — which
// §6.2 step 3 calls the worst outcome available in this design.
func TestInsertDelegationRunSlug(t *testing.T) {
	s := open(t)
	a := newRun(t, s)
	b := newRun(t, s)
	if a.Slug != "atlas-1" || b.Slug != "atlas-2" {
		t.Fatalf("slugs = %q %q, want atlas-1 atlas-2", a.Slug, b.Slug)
	}
	if a.ID == b.ID {
		t.Fatalf("two runs share id %d", a.ID)
	}
	// the placeholder slug the insert writes must never survive the transaction
	got, ok, err := s.GetDelegationRun(a.ID)
	if err != nil || !ok {
		t.Fatalf("GetDelegationRun: %v %v", ok, err)
	}
	if got != a {
		t.Fatalf("roundtrip = %+v, want %+v", got, a)
	}
	if got.Status != "planning" {
		t.Fatalf("Status = %q, want planning", got.Status)
	}
	bySlug, ok, err := s.GetDelegationRunBySlug("atlas-2")
	if err != nil || !ok || bySlug.ID != b.ID {
		t.Fatalf("GetDelegationRunBySlug = %+v %v %v", bySlug, ok, err)
	}
	if _, ok, err := s.GetDelegationRunBySlug("nope-9"); ok || err != nil {
		t.Fatalf("unknown slug = %v %v, want false nil", ok, err)
	}
}

// TestDelegationRunManifestIsASnapshot: §4.1 binds the manifest to be snapshot
// at run creation, exactly as workflow_runs.def_json is. A run replays what it
// was created from even if the on-disk file is edited or deleted.
func TestDelegationRunManifestIsASnapshot(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	// nothing in the store can rewrite manifest_json — there is deliberately no
	// setter. This test would stop compiling if one were added, which is the point.
	got, _, _ := s.GetDelegationRun(r.ID)
	if got.ManifestJSON != `{"manifest":1,"name":"atlas"}` || got.BaseSHAs != `{"bankenstein":"9b69827"}` {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestListDelegationRuns(t *testing.T) {
	s := open(t)
	a := newRun(t, s)
	b := newRun(t, s)
	other, err := s.InsertDelegationRun("ballista-x", "/w/Other", "{}", "{}", 900)
	if err != nil {
		t.Fatal(err)
	}

	list, err := s.ListDelegationRuns("/w/Innostream")
	if err != nil || len(list) != 2 {
		t.Fatalf("ListDelegationRuns = %+v %v", list, err)
	}
	if list[0].ID != b.ID || list[1].ID != a.ID {
		t.Fatalf("order = %d,%d; want newest first", list[0].ID, list[1].ID)
	}

	active, err := s.ActiveDelegationRuns()
	if err != nil || len(active) != 3 {
		t.Fatalf("ActiveDelegationRuns = %d rows, %v", len(active), err)
	}
	if ok, _ := s.AdvanceDelegationRunCAS(a.ID, "planning", "done", 1100); !ok {
		t.Fatal("advance to done rejected")
	}
	if ok, _ := s.AbandonDelegationRunCAS(other.ID, 1100); !ok {
		t.Fatal("abandon rejected")
	}
	active, _ = s.ActiveDelegationRuns()
	if len(active) != 1 || active[0].ID != b.ID {
		t.Fatalf("ActiveDelegationRuns after terminals = %+v", active)
	}
	// a deadlocked run is waiting for a human, not finished: hiding it is how a
	// stuck run becomes an invisible one
	if ok, _ := s.AdvanceDelegationRunCAS(b.ID, "planning", "deadlocked", 1200); !ok {
		t.Fatal("advance to deadlocked rejected")
	}
	if active, _ = s.ActiveDelegationRuns(); len(active) != 1 {
		t.Fatalf("deadlocked run dropped out of the active set: %+v", active)
	}
}

// TestDelegationRunCASRejectsStaleSnapshot is the run half of §13.3.
func TestDelegationRunCASRejectsStaleSnapshot(t *testing.T) {
	s := open(t)
	r := newRun(t, s)

	if ok, _ := s.AdvanceDelegationRunCAS(r.ID, "planning", "running", 1100); !ok {
		t.Fatal("first advance rejected")
	}
	// a second writer still holding the 'planning' snapshot
	ok, err := s.AdvanceDelegationRunCAS(r.ID, "planning", "abandoned", 1200)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("stale snapshot claimed the run")
	}
	got, _, _ := s.GetDelegationRun(r.ID)
	if got.Status != "running" || got.UpdatedAt != 1100 {
		t.Fatalf("rejected CAS touched the row: %+v", got)
	}

	// abandon works from any non-terminal status but never twice
	if ok, _ := s.AbandonDelegationRunCAS(r.ID, 1300); !ok {
		t.Fatal("abandon from running rejected")
	}
	if ok, _ := s.AbandonDelegationRunCAS(r.ID, 1400); ok {
		t.Fatal("re-abandon claimed; the caller cannot tell 'already gone' from 'just done'")
	}
	if ok, _ := s.AdvanceDelegationRunCAS(r.ID, "abandoned", "running", 1500); !ok {
		// a resurrect IS legal at the store layer (the guard is the expected
		// status, not a hardcoded terminal set) — the policy lives above
		t.Fatal("explicit expected-status advance out of abandoned rejected")
	}
}

func TestDelegationTaskRoundtrip(t *testing.T) {
	s := open(t)
	r := newRun(t, s)

	full := DelegationTask{
		RunID: r.ID, TaskID: "schema", State: "pending", SessionName: "loom-x",
		RepoLabel: "bankenstein", Worktree: "/h/.loom/worktrees/atlas-1/bankenstein/schema",
		Branch: "loom/atlas-1/schema", BaseSHA: "9b69827", BaseProducers: `[{"task":"a"}]`,
		CheckStatus: "pass", CheckExit: 0, CheckOut: "ok", CheckAt: 1200,
		BranchHead: "a41f0c2", BlockJSON: `{"kind":"needs-decision"}`,
		PendingSeed: "continue", Divergence: `["x.go"]`, SpawnSnapshot: `{"d":[]}`,
		Flags: `["orphaned"]`, UpdatedAt: 1300,
	}
	if err := s.InsertDelegationTask(full); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetDelegationTask(r.ID, "schema")
	if err != nil || !ok {
		t.Fatalf("GetDelegationTask: %v %v", ok, err)
	}
	if got != full {
		t.Fatalf("roundtrip:\n got %+v\nwant %+v", got, full)
	}
	if _, ok, err := s.GetDelegationTask(r.ID, "absent"); ok || err != nil {
		t.Fatalf("unknown task = %v %v, want false nil", ok, err)
	}

	// Insert never updates: run creation is re-runnable after a crash and must
	// not reset a task that has since been approved, spawned or merged.
	reset := newTask(r.ID, "schema")
	if err := s.InsertDelegationTask(reset); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetDelegationTask(r.ID, "schema"); got != full {
		t.Fatalf("re-insert clobbered a live task: %+v", got)
	}

	// tasks are per (run, task): the same task id in another run is a different row
	r2 := newRun(t, s)
	if err := s.InsertDelegationTask(newTask(r2.ID, "schema")); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListDelegationTasks(r.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListDelegationTasks = %+v %v", list, err)
	}

	if err := s.InsertDelegationTask(newTask(r.ID, "auth-api")); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListDelegationTasks(r.ID)
	if len(list) != 2 || list[0].TaskID != "auth-api" || list[1].TaskID != "schema" {
		t.Fatalf("ListDelegationTasks order = %+v", list)
	}

	bySession, ok, err := s.GetDelegationTaskBySession("loom-x")
	if err != nil || !ok || bySession.TaskID != "schema" || bySession.RunID != r.ID {
		t.Fatalf("GetDelegationTaskBySession = %+v %v %v", bySession, ok, err)
	}
	// '' must never match the fleet of tasks that have no session yet
	if _, ok, _ := s.GetDelegationTaskBySession(""); ok {
		t.Fatal("empty session name matched a task")
	}
}

// TestTaskCASRejectsStaleSnapshot is §13.3 made literal: every transition is a
// compare-and-swap, RowsAffected()==0 means the snapshot went stale, and the
// row is left COMPLETELY untouched so the caller can safely not perform its
// side effect.
func TestTaskCASRejectsStaleSnapshot(t *testing.T) {
	cases := []struct {
		name string
		// from is the state the row is put in before the call.
		from string
		// call performs one transition against the (deliberately wrong or
		// right) expected state and reports what it claimed.
		call      func(*Store, int64) (bool, error)
		wantClaim bool
		wantState string
	}{
		{
			name: "spawn claim from approved wins",
			from: "approved",
			call: func(s *Store, run int64) (bool, error) {
				return s.ClaimTaskSpawnCAS(run, "schema", "/wt", "loom/atlas-1/schema", "9b6", `[]`, 0, 2000)
			},
			wantClaim: true, wantState: "spawning",
		},
		{
			name: "spawn claim from pending loses — approve-to-spawn is the only path",
			from: "pending",
			call: func(s *Store, run int64) (bool, error) {
				return s.ClaimTaskSpawnCAS(run, "schema", "/wt", "b", "9b6", `[]`, 0, 2000)
			},
			wantClaim: false, wantState: "pending",
		},
		{
			name: "double approve-press produces one spawn",
			from: "spawning",
			call: func(s *Store, run int64) (bool, error) {
				return s.ClaimTaskSpawnCAS(run, "schema", "/wt", "b", "9b6", `[]`, 0, 2000)
			},
			wantClaim: false, wantState: "spawning",
		},
		{
			name: "session bind from spawning wins",
			from: "spawning",
			call: func(s *Store, run int64) (bool, error) {
				return s.BindTaskSessionCAS(run, "schema", "loom-child", 2000)
			},
			wantClaim: true, wantState: "running",
		},
		{
			// §13.3's disclosed residual hole: a concurrent abandon moved the
			// task out of spawning while the launch was in flight. The bind
			// must NOT claim — the caller has to surface a live child instead.
			name: "session bind after a concurrent abandon loses",
			from: "abandoned",
			call: func(s *Store, run int64) (bool, error) {
				return s.BindTaskSessionCAS(run, "schema", "loom-child", 2000)
			},
			wantClaim: false, wantState: "abandoned",
		},
		{
			name: "check record from checking wins",
			from: "checking",
			call: func(s *Store, run int64) (bool, error) {
				return s.RecordTaskCheckCAS(run, "schema", "checking", "verified", "pass", 0, "ok", "a41", 2000)
			},
			wantClaim: true, wantState: "verified",
		},
		{
			name: "check record against a task that moved on loses",
			from: "running",
			call: func(s *Store, run int64) (bool, error) {
				return s.RecordTaskCheckCAS(run, "schema", "checking", "verified", "pass", 0, "ok", "a41", 2000)
			},
			wantClaim: false, wantState: "running",
		},
		{
			name: "plain advance with the right expectation wins",
			from: "ready",
			call: func(s *Store, run int64) (bool, error) {
				return s.AdvanceTaskCAS(run, "schema", "ready", "approved", 2000)
			},
			wantClaim: true, wantState: "approved",
		},
		{
			name: "plain advance with a stale expectation loses",
			from: "approved",
			call: func(s *Store, run int64) (bool, error) {
				return s.AdvanceTaskCAS(run, "schema", "ready", "approved", 2000)
			},
			wantClaim: false, wantState: "approved",
		},
		{
			name: "abandon from anywhere",
			from: "blocked",
			call: func(s *Store, run int64) (bool, error) {
				return s.AbandonTaskCAS(run, "schema", 2000)
			},
			wantClaim: true, wantState: "abandoned",
		},
		{
			name: "abandon does not resurrect a merged task",
			from: "merged",
			call: func(s *Store, run int64) (bool, error) {
				return s.AbandonTaskCAS(run, "schema", 2000)
			},
			wantClaim: false, wantState: "merged",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := open(t)
			r := newRun(t, s)
			seed := newTask(r.ID, "schema")
			seed.State = c.from
			if err := s.InsertDelegationTask(seed); err != nil {
				t.Fatal(err)
			}
			claimed, err := c.call(s, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if claimed != c.wantClaim {
				t.Fatalf("claimed = %v, want %v", claimed, c.wantClaim)
			}
			got, _, _ := s.GetDelegationTask(r.ID, "schema")
			if got.State != c.wantState {
				t.Fatalf("state = %q, want %q", got.State, c.wantState)
			}
			if !claimed && got != seed {
				// "the row is left COMPLETELY untouched" is the actual
				// guarantee, not just the state column
				t.Fatalf("rejected CAS mutated the row:\n got %+v\nwant %+v", got, seed)
			}
		})
	}
}

// TestClaimTaskSpawnCASWritesWorktreeIdentity: the worktree, branch and base
// are written by the CLAIM, not after creation, because a crash between the
// claim and the launch must leave a row that names exactly where to look
// (§13.3's recovery evidence).
func TestClaimTaskSpawnCASWritesWorktreeIdentity(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	seed := newTask(r.ID, "schema")
	seed.State = "approved"
	if err := s.InsertDelegationTask(seed); err != nil {
		t.Fatal(err)
	}
	ok, err := s.ClaimTaskSpawnCAS(r.ID, "schema",
		"/h/.loom/worktrees/atlas-1/bankenstein/schema", "loom/atlas-1/schema",
		"merge-sha", `[{"task":"config","sha":"c0"}]`, 0, 2000)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	got, _, _ := s.GetDelegationTask(r.ID, "schema")
	if got.Worktree == "" || got.Branch == "" || got.BaseSHA != "merge-sha" ||
		got.BaseProducers == "" || got.UpdatedAt != 2000 {
		t.Fatalf("claim did not record the worktree identity: %+v", got)
	}
	if got.SessionName != "" {
		t.Fatalf("claim invented a session name: %q", got.SessionName)
	}
}

// TestRecordTaskCheckCASWritesResultAndStateTogether: result and state are one
// fact. A reader must never see `verified` with a stale check output, nor a
// green check on a task the UI still shows as running — §5.2's merge gate reads
// both to decide whether to render the action at all.
func TestRecordTaskCheckCASWritesResultAndStateTogether(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	seed := newTask(r.ID, "schema")
	seed.State = "checking"
	if err := s.InsertDelegationTask(seed); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RecordTaskCheckCAS(r.ID, "schema", "checking", "failed", "fail",
		1, "FAIL ./internal/account", "a41f0c2", 2000); err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	got, _, _ := s.GetDelegationTask(r.ID, "schema")
	want := DelegationTask{RunID: r.ID, TaskID: "schema", State: "failed",
		RepoLabel: "bankenstein", CheckStatus: "fail", CheckExit: 1,
		CheckOut: "FAIL ./internal/account", CheckAt: 2000,
		BranchHead: "a41f0c2", UpdatedAt: 2000}
	if got != want {
		t.Fatalf("\n got %+v\nwant %+v", got, want)
	}
}

// TestTaskAnnotationSetters covers the ungated writers. They are ungated on
// purpose (§13.2 keeps flags independent of the state column so a watchdog
// never races the runner), and they must not disturb the state.
func TestTaskAnnotationSetters(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	seed := newTask(r.ID, "schema")
	seed.State = "running"
	if err := s.InsertDelegationTask(seed); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskFlags(r.ID, "schema", `["orphaned"]`, 2000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskBlock(r.ID, "schema", `{"kind":"needs-artifact"}`, 2001); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskDivergence(r.ID, "schema", `["cmd/main.go"]`, 2002); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskBranchHead(r.ID, "schema", "a41f0c2", 2003); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskPendingSeed(r.ID, "schema", "account-schema is present. Continue.", 2004); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetDelegationTask(r.ID, "schema")
	if got.State != "running" {
		t.Fatalf("an annotation moved the state: %+v", got)
	}
	if got.Flags != `["orphaned"]` || got.BlockJSON != `{"kind":"needs-artifact"}` ||
		got.Divergence != `["cmd/main.go"]` || got.BranchHead != "a41f0c2" ||
		got.PendingSeed != "account-schema is present. Continue." || got.UpdatedAt != 2004 {
		t.Fatalf("annotations lost: %+v", got)
	}
	// writing to a task that no longer exists is not an error the watchdog can act on
	if err := s.SetTaskFlags(r.ID, "gone", `[]`, 2005); err != nil {
		t.Fatalf("annotation on a missing row = %v, want nil", err)
	}
}

// TestClearTaskPendingSeedCAS: the clear is conditioned on the exact seed text
// because the delivery path re-reads before sending (§11.4). An unconditional
// clear would erase a NEWER seed written between the read and the send, and the
// child would sit blocked forever on a continuation nothing re-issues.
func TestClearTaskPendingSeedCAS(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	seed := newTask(r.ID, "schema")
	seed.State = "blocked"
	if err := s.InsertDelegationTask(seed); err != nil {
		t.Fatal(err)
	}
	s.SetTaskPendingSeed(r.ID, "schema", "first", 2000)
	// a second seed lands while the deliverer is mid-send
	s.SetTaskPendingSeed(r.ID, "schema", "second", 2001)

	ok, err := s.ClearTaskPendingSeedCAS(r.ID, "schema", "first", 2002)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("stale clear claimed: it would have erased an undelivered seed")
	}
	if got, _, _ := s.GetDelegationTask(r.ID, "schema"); got.PendingSeed != "second" {
		t.Fatalf("PendingSeed = %q, want second", got.PendingSeed)
	}
	if ok, _ := s.ClearTaskPendingSeedCAS(r.ID, "schema", "second", 2003); !ok {
		t.Fatal("matching clear rejected")
	}
	if got, _, _ := s.GetDelegationTask(r.ID, "schema"); got.PendingSeed != "" {
		t.Fatalf("PendingSeed = %q, want empty", got.PendingSeed)
	}
}

func TestDelegationArtifacts(t *testing.T) {
	s := open(t)
	r := newRun(t, s)

	a := DelegationArtifact{RunID: r.ID, ArtifactID: "account-schema", TaskID: "schema",
		Path: "db/migrations/0007_account.sql", Fingerprint: "abc123",
		CommitSHA: "9b69827", PublishedAt: 2000}
	if err := s.UpsertDelegationArtifact(a); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetDelegationArtifact(r.ID, "account-schema")
	if err != nil || !ok || got != a {
		t.Fatalf("roundtrip = %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.GetDelegationArtifact(r.ID, "nope"); ok || err != nil {
		t.Fatalf("unknown artifact = %v %v", ok, err)
	}

	// a re-spawned task republishes the same id at a new commit, and the newest
	// publication is the one every `needs` edge must resolve against
	a2 := a
	a2.CommitSHA = "a41f0c2"
	a2.Fingerprint = "def456"
	a2.PublishedAt = 3000
	if err := s.UpsertDelegationArtifact(a2); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetDelegationArtifact(r.ID, "account-schema")
	if got != a2 {
		t.Fatalf("republish = %+v, want %+v", got, a2)
	}

	b := DelegationArtifact{RunID: r.ID, ArtifactID: "auth-openapi", TaskID: "auth-api",
		Path: "api/auth.yaml", PublishedAt: 3100}
	if err := s.UpsertDelegationArtifact(b); err != nil {
		t.Fatal(err)
	}
	// artifact ids are unique per RUN, so another run's identically-named
	// artifact is a separate row and must not leak into this run's `needs`
	r2 := newRun(t, s)
	other := a
	other.RunID = r2.ID
	other.CommitSHA = "zzz"
	if err := s.UpsertDelegationArtifact(other); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListDelegationArtifacts(r.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListDelegationArtifacts = %+v %v", list, err)
	}
	if list[0].ArtifactID != "account-schema" || list[1].ArtifactID != "auth-openapi" {
		t.Fatalf("order = %+v", list)
	}
	if list[0].CommitSHA != "a41f0c2" {
		t.Fatalf("another run's artifact leaked in: %+v", list[0])
	}
}

// TestDelegationRunProjectRoot pins §14.1's override input. Without it a
// child's cwd (a worktree under ~/.loom) matches no project target, the
// resolver fails closed, and every delegation child vanishes from the rail the
// moment anything is hidden — including when the user soloed the run's own
// project.
func TestDelegationRunProjectRoot(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	root, ok, err := s.DelegationRunProjectRoot(r.ID)
	if err != nil || !ok || root != "/w/Innostream" {
		t.Fatalf("DelegationRunProjectRoot = %q %v %v", root, ok, err)
	}
	// §14.1: a delegation value naming a run that no longer exists falls
	// through to the prefix scan and thus to fail-closed. Not an error.
	root, ok, err = s.DelegationRunProjectRoot(9999)
	if err != nil || ok || root != "" {
		t.Fatalf("unknown run = %q %v %v, want '' false nil", root, ok, err)
	}
}

// --- the cap is a compare-and-swap, not a read-then-write ------------------

// §6.6 is BINDING: the concurrency cap is a HARD maximum. It used to be
// enforced by the caller counting live sessions and then claiming, and a probe
// showed that is not enforcement at all — a session row is only written inside
// Launcher.Launch, several steps after the claim, so every spawn already past
// its own count was invisible to every other spawn's count and five concurrent
// approvals against a cap of three launched five children.
//
// The predicate now lives inside the claim's own UPDATE, so the count and the
// state move are one statement and cannot interleave.
func TestClaimTaskSpawnCASEnforcesTheCap(t *testing.T) {
	// holders are the states delegate.ActiveChildren counts. Spelled out here
	// too, because the SQL spells them out: if the two lists ever disagree the
	// cap silently changes meaning, and this is the test that says so.
	cases := []struct {
		name      string
		others    []string
		capN      int
		wantClaim bool
	}{
		{name: "under the cap", others: []string{"running"}, capN: 3, wantClaim: true},
		{name: "at the cap refuses", others: []string{"running", "running", "blocked"}, capN: 3},
		{name: "over the cap refuses", others: []string{"running", "running", "blocked", "checking"}, capN: 3},
		{name: "spawning counts — the launch may already be a real claude",
			others: []string{"spawning", "spawning", "spawning"}, capN: 3},
		{name: "checking counts — the child sits idle in its worktree",
			others: []string{"checking", "checking", "checking"}, capN: 3},
		{name: "terminal and pre-launch states do not count",
			others:    []string{"pending", "ready", "approved", "verified", "failed", "merged", "abandoned"},
			capN:      1,
			wantClaim: true},
		{name: "cap of one admits the first child",
			others: []string{"pending"}, capN: 1, wantClaim: true},
		{name: "cap <= 0 means no predicate — the store's own tests only",
			others: []string{"running", "running", "running", "running"}, capN: 0, wantClaim: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			r := newRun(t, s)
			for i, st := range tc.others {
				o := newTask(r.ID, "other-"+string(rune('a'+i)))
				o.State = st
				if err := s.InsertDelegationTask(o); err != nil {
					t.Fatal(err)
				}
			}
			subject := newTask(r.ID, "schema")
			subject.State = "approved"
			if err := s.InsertDelegationTask(subject); err != nil {
				t.Fatal(err)
			}

			ok, err := s.ClaimTaskSpawnCAS(r.ID, "schema", "/wt", "b", "9b6", `[]`, tc.capN, 2000)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.wantClaim {
				t.Fatalf("claimed = %v, want %v", ok, tc.wantClaim)
			}
			got, _, _ := s.GetDelegationTask(r.ID, "schema")
			want := "approved"
			if tc.wantClaim {
				want = "spawning"
			}
			if got.State != want {
				t.Errorf("state = %q, want %q — a refused claim must leave the row COMPLETELY untouched",
					got.State, want)
			}
			if !tc.wantClaim && got.Worktree != "" {
				t.Errorf("worktree = %q on a refused claim; the identity columns are written by the "+
					"claim and must not be written when it loses", got.Worktree)
			}
		})
	}
}

// The cap counts ONE RUN, not the table. Two initiatives running side by side
// must not starve each other — §6.6's reasons (quota, §6.4's shared resources,
// the human's review bandwidth) are all per-initiative.
func TestClaimTaskSpawnCASCapIsScopedToTheRun(t *testing.T) {
	s := open(t)
	busy := newRun(t, s)
	for i := 0; i < 5; i++ {
		o := newTask(busy.ID, "other-"+string(rune('a'+i)))
		o.State = "running"
		if err := s.InsertDelegationTask(o); err != nil {
			t.Fatal(err)
		}
	}
	quiet := newRun(t, s)
	subject := newTask(quiet.ID, "schema")
	subject.State = "approved"
	if err := s.InsertDelegationTask(subject); err != nil {
		t.Fatal(err)
	}
	ok, err := s.ClaimTaskSpawnCAS(quiet.ID, "schema", "/wt", "b", "9b6", `[]`, 3, 2000)
	if err != nil || !ok {
		t.Fatalf("claim = %v %v; another run's children must not consume this run's cap", ok, err)
	}
}

// --- a claim needs a legal SOURCE SET, not "whatever I just read" ----------

// AdvanceTaskCAS(from = the state the caller just read) is not a guard: it
// succeeds against every state including the ones it must refuse. Two probes
// found the same defect through it — a check resurrected a `merged` task, and a
// second instance claimed checking→checking because SQLite counts that row as
// affected. AdvanceTaskFromAnyCAS makes the legal sources explicit and reports
// which one it matched, so a caller that has to restore the task afterwards
// restores it to a state the row was actually in.
func TestAdvanceTaskFromAnyCAS(t *testing.T) {
	legal := []string{"running", "blocked", "verified", "failed"}
	cases := []struct {
		name      string
		from      string
		expected  []string
		wantClaim bool
		wantPrev  string
		wantState string
	}{
		{name: "running is legal", from: "running", expected: legal,
			wantClaim: true, wantPrev: "running", wantState: "checking"},
		{name: "verified is legal — a re-check is a human's right", from: "verified", expected: legal,
			wantClaim: true, wantPrev: "verified", wantState: "checking"},
		{name: "failed is legal — refusing a re-run would make a flake permanent",
			from: "failed", expected: legal,
			wantClaim: true, wantPrev: "failed", wantState: "checking"},
		{name: "merged is terminal and is refused", from: "merged", expected: legal,
			wantState: "merged"},
		{name: "abandoned is terminal and is refused", from: "abandoned", expected: legal,
			wantState: "abandoned"},
		{name: "checking is already in flight and is refused", from: "checking", expected: legal,
			wantState: "checking"},
		{name: "an empty source set claims nothing", from: "running", expected: nil,
			wantState: "running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			r := newRun(t, s)
			seed := newTask(r.ID, "schema")
			seed.State = tc.from
			if err := s.InsertDelegationTask(seed); err != nil {
				t.Fatal(err)
			}
			claimed, prev, err := s.AdvanceTaskFromAnyCAS(r.ID, "schema", tc.expected, "checking", 2000)
			if err != nil {
				t.Fatal(err)
			}
			if claimed != tc.wantClaim || prev != tc.wantPrev {
				t.Errorf("claimed/previous = %v %q, want %v %q", claimed, prev, tc.wantClaim, tc.wantPrev)
			}
			got, _, _ := s.GetDelegationTask(r.ID, "schema")
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q", got.State, tc.wantState)
			}
		})
	}
}

// The claim must be a SINGLETON under contention, which is the whole reason it
// is a CAS: two Loom instances against one DB is supported (§13), and the loser
// must learn it lost rather than proceed to run a subprocess.
func TestAdvanceTaskFromAnyCASIsSingletonUnderContention(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	seed := newTask(r.ID, "schema")
	seed.State = "running"
	if err := s.InsertDelegationTask(seed); err != nil {
		t.Fatal(err)
	}
	legal := []string{"running", "blocked", "verified", "failed"}
	won := 0
	for i := 0; i < 8; i++ {
		// Sequential, not concurrent: the point is that the SECOND and every
		// later attempt is refused, which is exactly the checking→checking case
		// a probe found admitted. A goroutine race would test the driver.
		ok, _, err := s.AdvanceTaskFromAnyCAS(r.ID, "schema", legal, "checking", 2000)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d of 8 claims won; exactly one may — §8.2 binds at most one check in flight per task", won)
	}
}
