package store

import (
	"reflect"
	"strings"
	"testing"
)

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
		NeedsSnapshot: `{"account-schema":{"fingerprint":"f1","commit":"c1"}}`,
		Flags:         `["orphaned"]`, UpdatedAt: 1300,
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

// TestDelegationAmendmentsAreAppendOnly is §11.3 made literal: an amendment is
// an append-only row on the run, never a mutation of the on-disk manifest and
// never a mutation of a row already written. The effective graph is the manifest
// snapshot replayed with these rows in seq order, so an amendment that could be
// rewritten would rewrite a plan a child is already parked against.
func TestDelegationAmendmentsAreAppendOnly(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	other := newRun(t, s)

	seq1, err := s.AppendDelegationAmendment(r.ID, "needs-artifact",
		`{"consumer":"auth-api","artifact":"account-schema"}`, 1100)
	if err != nil {
		t.Fatal(err)
	}
	seq2, err := s.AppendDelegationAmendment(r.ID, "needs-scope", `{"paths":["internal/db"]}`, 1200)
	if err != nil {
		t.Fatal(err)
	}
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("seqs = %d %d, want 1 2", seq1, seq2)
	}
	// seq is per RUN, not global: the replay order that builds the effective
	// graph is per run, and a global counter makes "the first amendment on this
	// run" a query rather than a value.
	oseq, err := s.AppendDelegationAmendment(other.ID, "needs-artifact", `{}`, 1300)
	if err != nil {
		t.Fatal(err)
	}
	if oseq != 1 {
		t.Fatalf("first amendment of a second run got seq %d, want 1", oseq)
	}

	list, err := s.ListDelegationAmendments(r.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListDelegationAmendments = %+v %v", list, err)
	}
	want := []DelegationAmendment{
		{RunID: r.ID, Seq: 1, Kind: "needs-artifact",
			Body: `{"consumer":"auth-api","artifact":"account-schema"}`, CreatedAt: 1100},
		{RunID: r.ID, Seq: 2, Kind: "needs-scope", Body: `{"paths":["internal/db"]}`, CreatedAt: 1200},
	}
	for i, w := range want {
		if list[i] != w {
			t.Fatalf("amendment %d = %+v, want %+v", i, list[i], w)
		}
	}

	// Approval touches approved_at and nothing else: the recorded proposal is
	// evidence, and evidence that changes when someone agrees with it is not.
	if ok, err := s.ApproveDelegationAmendmentCAS(r.ID, 1, 1400); err != nil || !ok {
		t.Fatalf("approve: %v %v", ok, err)
	}
	got, ok, err := s.GetDelegationAmendment(r.ID, 1)
	if err != nil || !ok {
		t.Fatalf("GetDelegationAmendment: %v %v", ok, err)
	}
	frozen := want[0]
	frozen.ApprovedAt = 1400
	if got != frozen {
		t.Fatalf("approval rewrote the amendment: %+v, want %+v", got, frozen)
	}
	if _, ok, err := s.GetDelegationAmendment(r.ID, 99); ok || err != nil {
		t.Fatalf("unknown seq = %v %v, want false nil", ok, err)
	}

	// A superseding amendment is a NEW row. Appending the same (kind, body)
	// again must never collapse onto the existing one — that would silently
	// erase the intervening history the replay depends on.
	seq3, err := s.AppendDelegationAmendment(r.ID, "needs-artifact",
		`{"consumer":"auth-api","artifact":"account-schema"}`, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if seq3 != 3 {
		t.Fatalf("re-appending an identical amendment got seq %d, want a new row at 3", seq3)
	}
	if list, _ := s.ListDelegationAmendments(r.ID); len(list) != 3 {
		t.Fatalf("%d amendments after three appends", len(list))
	}
}

// The append-only property has to hold against the API SURFACE, not merely
// against today's callers: a later slice that adds SetAmendmentBody or
// DeleteAmendment breaks §11.3 without failing any behavioural test above,
// because the defect is the existence of the method. Pinned here so unparking
// that work is a deliberate edit to this list rather than an accident.
func TestNoAmendmentMutatorsExist(t *testing.T) {
	allowed := map[string]bool{
		"AppendDelegationAmendment":     true,
		"ListDelegationAmendments":      true,
		"GetDelegationAmendment":        true,
		"ApproveDelegationAmendmentCAS": true, // approved_at only, guarded on 0
		// v16's mirror. It writes rejected_at and nothing else, guarded on both
		// decision columns being 0, so it is a DECISION and not a mutation: kind,
		// body, seq and created_at stay untouchable and the row stays in the
		// replay. §11.3 needs a durable NO — without one a refused amendment is
		// indistinguishable from an offer nobody has looked at yet, and comes
		// back on every poll forever.
		"RejectDelegationAmendmentCAS": true,
	}
	typ := reflect.TypeOf(&Store{})
	for i := 0; i < typ.NumMethod(); i++ {
		n := typ.Method(i).Name
		if strings.Contains(n, "Amendment") && !allowed[n] {
			t.Errorf("%s: amendments are append-only (§11.3); a mutator needs a spec change, not a method", n)
		}
	}
}

// ApproveDelegationAmendmentCAS is a CAS because approval has a SIDE EFFECT —
// §11.3's authorization amendment rewrites brief.md and re-seeds the child — and
// a second approval would re-seed a child that is already working.
func TestApproveDelegationAmendmentCASIsSingleton(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	if _, err := s.AppendDelegationAmendment(r.ID, "needs-scope", `{}`, 1000); err != nil {
		t.Fatal(err)
	}
	won := 0
	for i := 0; i < 5; i++ {
		ok, err := s.ApproveDelegationAmendmentCAS(r.ID, 1, int64(2000+i))
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d of 5 approvals won; exactly one may", won)
	}
	if got, _, _ := s.GetDelegationAmendment(r.ID, 1); got.ApprovedAt != 2000 {
		t.Fatalf("ApprovedAt = %d, want the FIRST approval's timestamp 2000", got.ApprovedAt)
	}
	// An amendment that does not exist is not an approval that failed to race:
	// claimed=false, no error, nothing written.
	if ok, err := s.ApproveDelegationAmendmentCAS(r.ID, 7, 3000); ok || err != nil {
		t.Fatalf("approving a missing amendment = %v %v, want false nil", ok, err)
	}
}

// §11.3's NO is durable, exclusive with the YES, and decided INSIDE the UPDATE.
//
// Before v16 the only durable answer was yes: approved_at=0 meant "proposed", so
// a rejection was indistinguishable from an offer nobody had read, and the offer
// came back on every poll forever. Table-driven over the four orders two Loom
// instances can produce, because "the loser is told" is the whole property.
func TestAmendmentDecisionIsExclusiveAndDurable(t *testing.T) {
	tests := []struct {
		name string
		// decide returns (claimed, whichDecision) for each press in order.
		presses   []string // "approve" | "reject"
		wantWon   []bool
		wantAppr  int64
		wantRejec int64
	}{
		{"reject once", []string{"reject"}, []bool{true}, 0, 2000},
		{"reject twice: the second is told", []string{"reject", "reject"}, []bool{true, false}, 0, 2000},
		{"a rejection blocks a later approval", []string{"reject", "approve"}, []bool{true, false}, 0, 2000},
		{"an approval blocks a later rejection", []string{"approve", "reject"}, []bool{true, false}, 2000, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			r := newRun(t, s)
			if _, err := s.AppendDelegationAmendment(r.ID, "needs-scope", `{}`, 1000); err != nil {
				t.Fatal(err)
			}
			for i, press := range tc.presses {
				var ok bool
				var err error
				// Every press carries the SAME timestamp deliberately: the
				// assertion below is that the loser wrote nothing, and a distinct
				// timestamp per press would let a lost write pass unnoticed.
				if press == "approve" {
					ok, err = s.ApproveDelegationAmendmentCAS(r.ID, 1, 2000)
				} else {
					ok, err = s.RejectDelegationAmendmentCAS(r.ID, 1, 2000)
				}
				if err != nil {
					t.Fatal(err)
				}
				if ok != tc.wantWon[i] {
					t.Fatalf("press %d (%s) claimed = %v, want %v", i+1, press, ok, tc.wantWon[i])
				}
			}
			got, found, err := s.GetDelegationAmendment(r.ID, 1)
			if err != nil || !found {
				t.Fatalf("GetDelegationAmendment: %v %v", found, err)
			}
			if got.ApprovedAt != tc.wantAppr || got.RejectedAt != tc.wantRejec {
				t.Fatalf("approved_at/rejected_at = %d/%d, want %d/%d",
					got.ApprovedAt, got.RejectedAt, tc.wantAppr, tc.wantRejec)
			}
			// Append-only: the decision touched nothing else.
			if got.Kind != "needs-scope" || got.Body != `{}` || got.CreatedAt != 1000 {
				t.Fatalf("the decision rewrote the amendment: %+v", got)
			}
		})
	}
}

// Rejecting an amendment that does not exist is not a race that was lost.
func TestRejectDelegationAmendmentCASOnAMissingRow(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	if ok, err := s.RejectDelegationAmendmentCAS(r.ID, 7, 3000); ok || err != nil {
		t.Fatalf("rejecting a missing amendment = %v %v, want false nil", ok, err)
	}
}

// SetDelegationRunIntegrationCAS guards a MAP over repos: two integration passes
// for two different repos can complete concurrently, and a plain setter would
// have the later writer erase the earlier repo's baseline. §10.2's blame table
// then reads a missing baseline as "no previous result" and blames a child for a
// red the environment caused.
func TestSetDelegationRunIntegrationCASRejectsStaleSnapshot(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	if r.Integration != "" {
		t.Fatalf("fresh run Integration = %q, want empty", r.Integration)
	}
	first := `{"bankenstein":{"head":"a1","status":"pass","at":1100}}`
	ok, err := s.SetDelegationRunIntegrationCAS(r.ID, "", first, 1100)
	if err != nil || !ok {
		t.Fatalf("first write: %v %v", ok, err)
	}
	// a writer holding the pre-first snapshot must be refused, not merged over
	stale := `{"ballista":{"head":"b1","status":"fail","at":1200}}`
	ok, err = s.SetDelegationRunIntegrationCAS(r.ID, "", stale, 1200)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a stale snapshot claimed the write; the other repo's baseline would be gone")
	}
	got, _, _ := s.GetDelegationRun(r.ID)
	if got.Integration != first {
		t.Fatalf("Integration = %q, want %q untouched", got.Integration, first)
	}
	if got.UpdatedAt != 1100 {
		t.Fatalf("UpdatedAt = %d; a refused CAS must leave the row COMPLETELY untouched", got.UpdatedAt)
	}
	both := `{"bankenstein":{"head":"a1","status":"pass","at":1100},"ballista":{"head":"b1","status":"fail","at":1200}}`
	if ok, err := s.SetDelegationRunIntegrationCAS(r.ID, first, both, 1300); err != nil || !ok {
		t.Fatalf("re-read-and-reapply: %v %v", ok, err)
	}
	if got, _, _ := s.GetDelegationRun(r.ID); got.Integration != both {
		t.Fatalf("Integration = %q, want %q", got.Integration, both)
	}
}

// ClaimTaskIntegrationCAS enforces §10.2's "one integration at a time, run-wide"
// INSIDE the UPDATE. Two concurrent passes in one repo means one captures `pre`
// and the other runs `git reset --hard <pre>`, discarding a sibling's verified
// merge from the staging branch.
func TestClaimTaskIntegrationCASSerializesPerRun(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	for _, id := range []string{"schema", "auth-api", "web"} {
		task := newTask(r.ID, id)
		task.State = "verified"
		if err := s.InsertDelegationTask(task); err != nil {
			t.Fatal(err)
		}
	}
	from := []string{"verified", "integration_blocked"}
	ok, prev, err := s.ClaimTaskIntegrationCAS(r.ID, "schema", from, 2000)
	if err != nil || !ok || prev != "verified" {
		t.Fatalf("first claim = %v %q %v", ok, prev, err)
	}
	// every sibling is refused while one is integrating
	for _, id := range []string{"auth-api", "web"} {
		ok, _, err := s.ClaimTaskIntegrationCAS(r.ID, id, from, 2100)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("%s claimed a second concurrent integration", id)
		}
		if got, _, _ := s.GetDelegationTask(r.ID, id); got.State != "verified" {
			t.Fatalf("%s state = %q; a refused claim must leave the row untouched", id, got.State)
		}
	}
	// including the holder itself: a double-claim is a refusal, not a no-op
	// that would run the same merge sequence twice.
	if ok, _, _ := s.ClaimTaskIntegrationCAS(r.ID, "schema", []string{"integrating"}, 2200); ok {
		t.Fatal("the integrating task re-claimed its own integration")
	}
	// a run's exclusion must not reach across runs — §3 scopes a run to one
	// project's repos and two runs stage into different worktrees.
	r2 := newRun(t, s)
	t2 := newTask(r2.ID, "schema")
	t2.State = "verified"
	if err := s.InsertDelegationTask(t2); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := s.ClaimTaskIntegrationCAS(r2.ID, "schema", from, 2300); err != nil || !ok {
		t.Fatalf("another run's integration was blocked by this one: %v %v", ok, err)
	}
	// released, the next sibling gets its turn
	if ok, err := s.AdvanceTaskCAS(r.ID, "schema", "integrating", "mergeable", 2400); err != nil || !ok {
		t.Fatalf("release: %v %v", ok, err)
	}
	if ok, _, err := s.ClaimTaskIntegrationCAS(r.ID, "auth-api", from, 2500); err != nil || !ok {
		t.Fatalf("claim after release: %v %v", ok, err)
	}
	// an empty source set claims nothing rather than degrading to an unguarded
	// update, exactly as AdvanceTaskFromAnyCAS does
	if ok, _, err := s.ClaimTaskIntegrationCAS(r.ID, "web", nil, 2600); ok || err != nil {
		t.Fatalf("empty source set = %v %v, want false nil", ok, err)
	}
}

// The two spawn-time snapshots are the inputs §§10.5 and 12.3.3 compare against
// LATER, so they have to survive as written. NeedsSnapshot exists at all because
// UpsertDelegationArtifact keeps only the newest publication: this test performs
// the republication that destroys the old fingerprint and shows the snapshot is
// the only surviving copy.
func TestSpawnTimeSnapshotsSurviveRepublication(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	if err := s.InsertDelegationTask(newTask(r.ID, "auth-api")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDelegationArtifact(DelegationArtifact{RunID: r.ID,
		ArtifactID: "account-schema", TaskID: "schema", Path: "db/0007.sql",
		Fingerprint: "fp1", CommitSHA: "c1", PublishedAt: 1100}); err != nil {
		t.Fatal(err)
	}
	snap := `{"account-schema":{"fingerprint":"fp1","commit":"c1"}}`
	if err := s.SetTaskNeedsSnapshot(r.ID, "auth-api", snap, 1200); err != nil {
		t.Fatal(err)
	}
	dirty := `{"/w/bankenstein":[{"path":"a.go","mtime":9,"size":3}]}`
	if err := s.SetTaskSpawnSnapshot(r.ID, "auth-api", dirty, 1200); err != nil {
		t.Fatal(err)
	}

	// §10.3 sends the producer back; it republishes the same artifact id at a
	// new commit with a new fingerprint, overwriting the row.
	if err := s.UpsertDelegationArtifact(DelegationArtifact{RunID: r.ID,
		ArtifactID: "account-schema", TaskID: "schema", Path: "db/0007.sql",
		Fingerprint: "fp2", CommitSHA: "c2", PublishedAt: 1400}); err != nil {
		t.Fatal(err)
	}
	art, _, _ := s.GetDelegationArtifact(r.ID, "account-schema")
	if art.Fingerprint != "fp2" {
		t.Fatalf("artifact fingerprint = %q, want the republished fp2", art.Fingerprint)
	}
	got, _, _ := s.GetDelegationTask(r.ID, "auth-api")
	if got.NeedsSnapshot != snap {
		t.Fatalf("NeedsSnapshot = %q, want %q — §10.5's alarm has nothing to compare against without it",
			got.NeedsSnapshot, snap)
	}
	if got.SpawnSnapshot != dirty {
		t.Fatalf("SpawnSnapshot = %q, want %q", got.SpawnSnapshot, dirty)
	}
	if got.UpdatedAt != 1200 {
		t.Fatalf("UpdatedAt = %d, want 1200", got.UpdatedAt)
	}
	// both are replaceable wholesale (a re-spawn re-snapshots), and an empty
	// value is a real value meaning "nothing dirty", not "not captured"
	if err := s.SetTaskSpawnSnapshot(r.ID, "auth-api", "", 1500); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetDelegationTask(r.ID, "auth-api"); got.SpawnSnapshot != "" {
		t.Fatalf("SpawnSnapshot = %q, want cleared", got.SpawnSnapshot)
	}
}

// DelegationTasksInStates backs §12.2's watchdogs, which are run-agnostic on
// purpose: the sweep matters most when something is wrong, and iterating runs
// first skips a task whose run row is unreadable.
func TestDelegationTasksInStates(t *testing.T) {
	s := open(t)
	r1, r2 := newRun(t, s), newRun(t, s)
	seed := func(runID int64, id, state string, at int64) {
		task := newTask(runID, id)
		task.State, task.UpdatedAt = state, at
		if err := s.InsertDelegationTask(task); err != nil {
			t.Fatal(err)
		}
	}
	seed(r1.ID, "a", "spawning", 300)
	seed(r1.ID, "b", "running", 200)
	seed(r2.ID, "c", "spawning", 100)
	seed(r2.ID, "d", "merged", 400)

	for _, tc := range []struct {
		name   string
		states []string
		want   []string
	}{
		{"one state, across runs, oldest first", []string{"spawning"}, []string{"c", "a"}},
		{"several states", []string{"spawning", "running"}, []string{"c", "b", "a"}},
		{"no matches", []string{"blocked"}, nil},
		// A watchdog that computed its filter to empty has a bug, and the safe
		// reading of a bug in a sweep is "name nothing", never "name everything".
		{"empty set names nothing", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.DelegationTasksInStates(tc.states...)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, g := range got {
				ids = append(ids, g.TaskID)
			}
			if len(ids) != len(tc.want) {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
			for i := range ids {
				if ids[i] != tc.want[i] {
					t.Fatalf("ids = %v, want %v", ids, tc.want)
				}
			}
		})
	}
}

// A flag that is the only record of a failure cannot be written with a
// read-modify-write over the whole JSON set: a watchdog clearing `stalled` at
// the same moment erases it, and §12's premise is that failures stay VISIBLE.
func TestSetTaskFlagsCASRejectsStaleSnapshot(t *testing.T) {
	s := open(t)
	r := newRun(t, s)
	if err := s.InsertDelegationTask(newTask(r.ID, "schema")); err != nil {
		t.Fatal(err)
	}
	// the watchdog writes first, from the empty set it read
	if ok, err := s.SetTaskFlagsCAS(r.ID, "schema", "", `["stalled"]`, 1100); err != nil || !ok {
		t.Fatalf("first flag write: %v %v", ok, err)
	}
	// the seed deliverer, still holding the empty set, must be refused rather
	// than allowed to write `["seed-failed"]` over `["stalled"]`
	ok, err := s.SetTaskFlagsCAS(r.ID, "schema", "", `["seed-failed"]`, 1200)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a stale flag set claimed the write; the other writer's flag would be gone")
	}
	got, _, _ := s.GetDelegationTask(r.ID, "schema")
	if got.Flags != `["stalled"]` || got.UpdatedAt != 1100 {
		t.Fatalf("refused CAS touched the row: %+v", got)
	}
	if ok, err := s.SetTaskFlagsCAS(r.ID, "schema", `["stalled"]`,
		`["stalled","seed-failed"]`, 1300); err != nil || !ok {
		t.Fatalf("re-read-and-merge: %v %v", ok, err)
	}
	// the unconditional setter is still there for a badge a later poll would
	// recompute anyway, and it still wins unconditionally
	if err := s.SetTaskFlags(r.ID, "schema", `["diverged"]`, 1400); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetDelegationTask(r.ID, "schema"); got.Flags != `["diverged"]` {
		t.Fatalf("Flags = %q, want the unconditional write to land", got.Flags)
	}
}
