// Delegation runs (slice 3a, spec docs/superpowers/specs/
// 2026-07-22-delegation-design.md §13): an orchestration run is a manifest
// snapshot, a set of tasks each owning one repo and one worktree, and the
// artifacts those tasks publish.
//
// This file owns the rows only. The manifest, its cycle detection, the
// worktree plumbing, the check runner and the scheduler live in
// internal/delegate, above the store.
//
// EVERY state transition here is a compare-and-swap — §13.3 applies
// AdvanceRunCAS's discipline per task, for the same reason: two Loom instances
// against one DB is supported, so "read the state, decide, write the state" is
// two moments and something can land in between. RowsAffected()==0 means the
// snapshot the caller acted on went stale; the row is left COMPLETELY
// untouched and the caller must not perform the side effect it was claiming.
//
// SCOPE NOTE: §§9-12 (integration worktrees, Loom-run merges, cross-repo
// checks, rendezvous, amendments, the deadlock detector) are no longer parked.
// `delegation_runs.integration` and `delegation_tasks.spawn_snapshot` — declared
// without a writer in migration v12 for exactly this moment — have writers here
// now; `delegation_amendments` arrives in v14 and `delegation_tasks.
// needs_snapshot` in v15.
package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// DelegationRun is one row of `delegation_runs` (migration v12).
//
// ManifestJSON is a SNAPSHOT, the workflow_runs.def_json precedent: a run
// replays what it was created from even if the on-disk manifest is edited or
// deleted underneath it. BaseSHAs is JSON {repoLabel: sha}, pinned once at
// creation so every child of the run branches from the same commit — that is
// what makes integration deterministic.
//
// Integration is JSON {repoLabel: {head,status,at,out}} — §10.2's per-repo
// staging result at `pre`, which is the left column of its blame table: red with
// the task merged but green at `pre` blames the task, red at `pre` too blames
// the baseline and no task is blamed. It is the run's memory of the previous
// pass, so it must survive a restart between passes.
type DelegationRun struct {
	ID           int64
	Slug         string // "<manifest-name>-<id>"; the worktree and branch component
	Name         string
	ProjectRoot  string // §3 containment: a run is scoped to exactly ONE project
	ManifestJSON string
	BaseSHAs     string
	Integration  string
	Status       string // planning|running|deadlocked|done|abandoned
	CreatedAt    int64
	UpdatedAt    int64
}

// DelegationTask is one row of `delegation_tasks` (migration v12): one unit of
// delegated work — one repo, one worktree, one branch, one child session, one
// check.
//
// Flags is a JSON set (stalled|orphaned|diverged|stale-contract|env-suspect|
// forced|block-malformed) kept deliberately OUT of State: a state machine with
// a `stalled` state has to define the cross product of stalled × everything and
// stops being testable. Flags never gate a transition on their own, which is
// why nothing in this file CASes on them.
type DelegationTask struct {
	RunID  int64
	TaskID string
	State  string // §13.2's one CAS-guarded column
	// SessionName is the store PK of the child, '' until the launch produced
	// one. §13.3: Launcher.Launch mints the session id itself, so this is
	// always written AFTER the process exists.
	SessionName   string
	RepoLabel     string
	Worktree      string
	Branch        string
	BaseSHA       string // may differ from the run base — §9.2's same-repo merge
	BaseProducers string // JSON [{task,branch,sha}] merged to build BaseSHA
	CheckStatus   string // ''|pass|fail|unpublished|env-suspect|infra-error
	CheckExit     int64
	CheckOut      string // capped head+tail
	CheckAt       int64
	BranchHead    string // last sha the check ran against (§8.2's debounce)
	BlockJSON     string
	PendingSeed   string
	Divergence    string // JSON file lists (§12.3)
	// SpawnSnapshot is §12.3.3's out-of-worktree tripwire: JSON
	// {dir: [{path,mtime,size}]} over every in-scope repo's primary working tree
	// and every add-dir'd integration worktree, captured at spawn. `--add-dir`
	// grants write and cannot be revoked (spike), so this is the only thing that
	// can see a child writing outside its worktree by absolute path.
	SpawnSnapshot string
	// NeedsSnapshot is §10.5's stale-contract baseline (migration v15): JSON
	// {artifactID: {fingerprint, commit}} for every artifact this task `needs`,
	// as those artifacts stood when the task was spawned. Recorded because
	// delegation_artifacts holds only the LATEST publication — a producer sent
	// back by §10.3 overwrites the fingerprint the consumer was built against,
	// and without a copy the alarm compares the current value with itself.
	NeedsSnapshot string
	// SeedOwedSince is when a continuation seed became owed (migration v17), and
	// it is NOT UpdatedAt. §12.2's `block-stale` row measures the age of the
	// DEBT; UpdatedAt is the age of the last write to the row, and every delivery
	// attempt is a write. Zero means no seed is owed.
	SeedOwedSince int64
	// CertifiedSHA is the task-branch commit a green §10.2 pass certified
	// (migration v18). §5.2's gate compares it against the branch head at merge
	// time so that the diff a human approved is the diff that lands.
	CertifiedSHA string
	Flags        string
	UpdatedAt    int64
}

// DelegationAmendment is one row of `delegation_amendments` (migration v14):
// one recorded change to the effective plan, §11.3.
//
// APPEND-ONLY, and that is a property of this file, not of the caller: nothing
// here updates Kind, Body, CreatedAt or Seq once written. The effective graph is
// the manifest snapshot plus these rows replayed in Seq order, so an amendment
// that could be rewritten would silently rewrite history a child is already
// parked against.
//
// Body is opaque JSON whose shape depends on Kind (§11.3's needs-artifact edge,
// re-plan request, authorization amendment). The store stays a dumb pipe for the
// same reason it does for BlockJSON: the parse — and the cycle detection every
// amendment must re-run (§11.3/§4.5) — lives in internal/delegate, above it.
type DelegationAmendment struct {
	RunID      int64
	Seq        int64
	Kind       string
	Body       string
	ApprovedAt int64 // 0 = proposed, not yet approved
	// RejectedAt is the human's NO (migration v16). Zero means no decision, not
	// "approved" — the pair is exclusive and both being zero is the only state
	// that means "still on offer". Kept as a timestamp beside ApprovedAt rather
	// than folded into a status column for the reason approved_at gives: a
	// decision is when it was made, and two spellings of one fact disagree.
	RejectedAt int64
	CreatedAt  int64
}

// DelegationArtifact is one row of `delegation_artifacts` (migration v12): a
// named, path-addressed, COMMITTED file a task publishes. Artifacts are the
// only currency between tasks — `needs` names artifact ids, never task ids, so
// the ready condition is a statement about a thing that exists on disk and
// passed a check rather than about a peer's self-declared status.
type DelegationArtifact struct {
	RunID       int64
	ArtifactID  string
	TaskID      string
	Path        string
	Fingerprint string
	CommitSHA   string
	PublishedAt int64
}

const (
	drunCols  = "id, slug, name, project_root, manifest_json, base_shas, integration, status, created_at, updated_at"
	dtaskCols = "run_id, task_id, state, session_name, repo_label, worktree, branch, base_sha, " +
		"base_producers, check_status, check_exit, check_out, check_at, branch_head, " +
		"block_json, pending_seed, divergence, spawn_snapshot, needs_snapshot, " +
		"seed_owed_since, certified_sha, flags, updated_at"
	dartCols = "run_id, artifact_id, task_id, path, fingerprint, commit_sha, published_at"
	damdCols = "run_id, seq, kind, body, approved_at, rejected_at, created_at"
)

// InsertDelegationRun creates a run row at status 'planning' and returns it
// with its id and slug filled in.
//
// The slug is `<name>-<id>` and the id is only known after the insert, so the
// row is written with an empty slug and updated in the SAME transaction. That
// is not a workaround for the UNIQUE index, it is a use of it: ” is itself a
// unique value, so two concurrent creations cannot both hold it, and the
// transaction means no other reader ever observes the placeholder. Deriving the
// slug in the caller and passing it in was the alternative and is worse — the
// caller would have to invent an id, which is the one thing AUTOINCREMENT
// exists to stop it doing.
func (s *Store) InsertDelegationRun(name, projectRoot, manifestJSON, baseSHAs string, now int64) (DelegationRun, error) {
	r := DelegationRun{
		Name: name, ProjectRoot: projectRoot, ManifestJSON: manifestJSON,
		BaseSHAs: baseSHAs, Status: "planning", CreatedAt: now, UpdatedAt: now,
	}
	err := s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO delegation_runs
			(slug, name, project_root, manifest_json, base_shas, integration, status, created_at, updated_at)
			VALUES ('', ?, ?, ?, ?, '', 'planning', ?, ?)`,
			name, projectRoot, manifestJSON, baseSHAs, now, now)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		r.ID = id
		r.Slug = fmt.Sprintf("%s-%d", name, id)
		_, err = tx.Exec("UPDATE delegation_runs SET slug=? WHERE id=?", r.Slug, id)
		return err
	})
	if err != nil {
		return DelegationRun{}, err
	}
	return r, nil
}

func (s *Store) GetDelegationRun(id int64) (DelegationRun, bool, error) {
	return scanDelegationRun(s.db.QueryRow("SELECT "+drunCols+" FROM delegation_runs WHERE id=?", id))
}

// GetDelegationRunBySlug is the reverse of the worktree/branch naming: a
// stranded worktree on disk names its run by slug and nothing else, so
// recovery has to be able to get back from the path to the row.
func (s *Store) GetDelegationRunBySlug(slug string) (DelegationRun, bool, error) {
	return scanDelegationRun(s.db.QueryRow("SELECT "+drunCols+" FROM delegation_runs WHERE slug=?", slug))
}

// DelegationRunProjectRoot is §14.1's attribution override, reduced to the one
// fact it needs. A delegation child's cwd is a worktree under ~/.loom, which
// matches no {projects.root} ∪ {project_repos.path} target, so the path-based
// resolver fails CLOSED on it and every child vanishes from the rail the moment
// anything is hidden — including when the user soloed the run's own project.
// Identity beats geometry: the session row carries "<runID>:<taskID>", and this
// resolves the run half of it.
//
// ok=false for an unknown id is the correct answer, not an error: §14.1 says a
// delegation value naming a run that no longer exists falls through to the
// prefix scan and thus to fail-closed, because a deleted run is exactly the
// case where the conservative answer is right.
func (s *Store) DelegationRunProjectRoot(id int64) (string, bool, error) {
	var root string
	err := s.db.QueryRow("SELECT project_root FROM delegation_runs WHERE id=?", id).Scan(&root)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return root, true, nil
}

// ListDelegationRuns returns a project's runs, newest first. Backed by
// idx_druns_project.
func (s *Store) ListDelegationRuns(projectRoot string) ([]DelegationRun, error) {
	return s.queryDelegationRuns(
		"SELECT "+drunCols+" FROM delegation_runs WHERE project_root=? ORDER BY created_at DESC, id DESC",
		projectRoot)
}

// ActiveDelegationRuns returns every run that has not reached a terminal
// status. 'deadlocked' counts as active on purpose: a deadlocked run is
// waiting for a human, not finished, and hiding it is how a stuck run becomes
// an invisible one.
func (s *Store) ActiveDelegationRuns() ([]DelegationRun, error) {
	return s.queryDelegationRuns("SELECT " + drunCols +
		" FROM delegation_runs WHERE status NOT IN ('done','abandoned') ORDER BY created_at DESC, id DESC")
}

// AdvanceDelegationRunCAS moves a run's status iff it still holds
// expectedStatus — §13.3's "same for run status". claimed=false means another
// writer (or another Loom instance) moved it first; the row is untouched.
func (s *Store) AdvanceDelegationRunCAS(id int64, expectedStatus, newStatus string, now int64) (claimed bool, err error) {
	return s.casExec(
		"UPDATE delegation_runs SET status=?, updated_at=? WHERE id=? AND status=?",
		newStatus, now, id, expectedStatus)
}

// SetDelegationRunIntegrationCAS replaces the per-repo integration blob only if
// it is still byte-identical to what the caller read.
//
// A CAS rather than a setter because the blob is a MAP over repos and two
// integration passes for two different repos can complete concurrently (§10.2
// serializes passes per RUN, and two Loom instances against one DB serialize
// nothing). A plain setter makes each writer round-trip the whole map, so the
// later write erases the other repo's freshly-recorded baseline — and §10.2's
// blame table then reads a missing baseline as "no previous result", which
// flips an attribution from `baseline` to `the task` and blames a child for a
// red the environment caused. Merging inside SQLite was the alternative and is
// rejected: it needs JSON1 semantics in the store, and the store does not get to
// understand a payload it is a pipe for.
//
// claimed=false means re-read and re-apply. The caller must not also re-run the
// checks: it is recording a result it already has.
func (s *Store) SetDelegationRunIntegrationCAS(id int64, expected, next string, now int64) (claimed bool, err error) {
	return s.casExec(
		"UPDATE delegation_runs SET integration=?, updated_at=? WHERE id=? AND integration=?",
		next, now, id, expected)
}

// AbandonDelegationRunCAS retires a run from any non-terminal status. Unlike
// AdvanceDelegationRunCAS it does not name an expected status, because abandon
// is the one transition the human can issue against a run in any state; what it
// still refuses is re-abandoning or overwriting a finished run, so claimed=false
// distinguishes "already gone" from "just abandoned" for the caller's message.
func (s *Store) AbandonDelegationRunCAS(id int64, now int64) (claimed bool, err error) {
	return s.casExec(
		"UPDATE delegation_runs SET status='abandoned', updated_at=? WHERE id=? AND status NOT IN ('done','abandoned')",
		now, id)
}

// InsertDelegationTask writes a task row at run creation and NEVER updates an
// existing one — the UpsertProject discipline. Run creation is re-runnable
// (a crash between the run row and the last task row leaves a partial set), and
// a re-run must not reset a task that has since been approved, spawned or
// merged back to 'pending'. Every mutable field below has its own guarded
// writer; this one only ever adds.
func (s *Store) InsertDelegationTask(t DelegationTask) error {
	_, err := s.db.Exec(`INSERT INTO delegation_tasks (`+dtaskCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id, task_id) DO NOTHING`,
		t.RunID, t.TaskID, t.State, t.SessionName, t.RepoLabel, t.Worktree, t.Branch,
		t.BaseSHA, t.BaseProducers, t.CheckStatus, t.CheckExit, t.CheckOut, t.CheckAt,
		t.BranchHead, t.BlockJSON, t.PendingSeed, t.Divergence, t.SpawnSnapshot,
		t.NeedsSnapshot, t.SeedOwedSince, t.CertifiedSHA, t.Flags, t.UpdatedAt)
	return err
}

func (s *Store) GetDelegationTask(runID int64, taskID string) (DelegationTask, bool, error) {
	t, err := scanDelegationTask(s.db.QueryRow(
		"SELECT "+dtaskCols+" FROM delegation_tasks WHERE run_id=? AND task_id=?", runID, taskID))
	if err == sql.ErrNoRows {
		return DelegationTask{}, false, nil
	}
	if err != nil {
		return DelegationTask{}, false, err
	}
	return t, true, nil
}

// ListDelegationTasks returns a run's tasks in task-id order. The order is
// alphabetical, not topological: the topological order is a property of the
// manifest graph, which internal/delegate owns and the store cannot see.
func (s *Store) ListDelegationTasks(runID int64) ([]DelegationTask, error) {
	rows, err := s.db.Query(
		"SELECT "+dtaskCols+" FROM delegation_tasks WHERE run_id=? ORDER BY task_id", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DelegationTask
	for rows.Next() {
		t, err := scanDelegationTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetDelegationTaskBySession is the reverse lookup a poll loop needs: it holds
// a session row and has to find the task it belongs to. It exists so callers do
// not have to parse `sessions.delegation`'s composite string in two places —
// that string is for attribution, this is for the runner.
func (s *Store) GetDelegationTaskBySession(sessionName string) (DelegationTask, bool, error) {
	if sessionName == "" {
		return DelegationTask{}, false, nil
	}
	t, err := scanDelegationTask(s.db.QueryRow(
		"SELECT "+dtaskCols+" FROM delegation_tasks WHERE session_name=?", sessionName))
	if err == sql.ErrNoRows {
		return DelegationTask{}, false, nil
	}
	if err != nil {
		return DelegationTask{}, false, err
	}
	return t, true, nil
}

// AdvanceTaskCAS is the plain state move: pending→ready, ready→approved,
// running→checking, and so on. It writes nothing but the state, so a caller
// that also needs to record worktree or session identity uses one of the
// specific claims below rather than a move followed by a setter — the pair
// would not be atomic, and a crash between them is precisely the stranded
// half-state §13.3 spends its length narrowing.
func (s *Store) AdvanceTaskCAS(runID int64, taskID, expectedState, newState string, now int64) (claimed bool, err error) {
	return s.casExec(
		"UPDATE delegation_tasks SET state=?, updated_at=? WHERE run_id=? AND task_id=? AND state=?",
		newState, now, runID, taskID, expectedState)
}

// AdvanceTaskFromAnyCAS is AdvanceTaskCAS with a legal SOURCE SET instead of a
// single expected state.
//
// It exists because "CAS from whatever I just read" is not a CAS at all — it is
// a read-modify-write that succeeds against every state including the one it
// must refuse. Two probes found the same defect through it: a check claimed
// `merged→checking` and resurrected a task whose work had already landed, and a
// second instance claimed `checking→checking` (SQLite counts the row as
// affected, so the claim reported success) and ran the same agent-authored argv
// a second time against the same worktree. The caller must therefore name the
// states a transition is legal FROM, in code, where a reader can check them
// against §13.2's diagram.
//
// An empty set claims nothing and returns false rather than degrading to an
// unguarded update: a caller that computed its source set to empty has a bug,
// and the safe reading of a bug in a claim is "do not claim".
//
// It returns the state it actually matched. Without that the caller is back to
// trusting the value it read before the claim, and the whole point here is that
// that value can be stale — a caller that has to restore the task afterwards
// (the check does, for `unpublished` and `infra-error`) would write a state the
// row was never in.
//
// Implemented as a loop of single-state CASes rather than one `state IN (…)`
// statement, deliberately. Each iteration is the existing, already-tested
// atomic primitive, so at most one of any number of racing callers can win and
// the winner learns which state it took — `IN (…)` is one round trip but cannot
// report that without RETURNING, and a claim is not the place to be clever.
func (s *Store) AdvanceTaskFromAnyCAS(runID int64, taskID string, expectedStates []string, newState string, now int64) (claimed bool, previous string, err error) {
	for _, st := range expectedStates {
		ok, err := s.AdvanceTaskCAS(runID, taskID, st, newState, now)
		if err != nil {
			return false, "", err
		}
		if ok {
			return true, st, nil
		}
	}
	return false, "", nil
}

// ClaimTaskSpawnCAS is §13.3 step 1: approved→spawning, recording the worktree
// identity in the same statement. The claim precedes the worktree creation and
// the launch, so pressing Approve twice — or two instances pressing it once
// each — produces exactly one spawn.
//
// The worktree, branch and base are written HERE rather than after creation
// because they are deterministic from (run, task) and are what recovery keys
// on: a crash between this claim and the launch leaves a row that names exactly
// where to look. BaseProducers records the producer branch heads §9.2 merged in
// to build BaseSHA, so a re-spawn reproduces the same tree byte-for-byte.
//
// capN is §6.6's HARD maximum, and it is enforced HERE — inside the same UPDATE
// as the state move — rather than by the caller reading a count and then
// claiming. That ordering matters and was found by a probe, not by reasoning:
// the caller's count (Worktrees.LiveChildren) is derived from live `sessions`
// rows, and a session row is only written inside Launcher.Launch, which happens
// several steps AFTER this claim. Every spawn already past its own count was
// therefore invisible to every other spawn's count, and five concurrent
// approvals against a cap of three launched five children. Both reachable ways
// are blessed by the spec: §5.1's "approve all 3 ready tasks" runs as
// concurrent Wails binding calls, and §13's two-Loom-instances-one-DB
// configuration races by construction.
//
// The predicate counts TASK STATES, not sessions, because that is the only
// number that exists inside a single SQLite statement. The rejected alternative
// — claim, then count, then release if over — livelocks: five racers all claim,
// all count five, all release, and nothing spawns. The known cost of counting
// states is staleness in one direction (a dead child leaves a `running` row
// until something notices), and staleness here only ever REFUSES a spawn, which
// a human undoes by abandoning the dead task. Under-counting is the direction
// that cannot be undone, and this cannot under-count. Worktrees.Create keeps its
// LiveChildren check as the second, session-derived backstop for the reverse
// case — sessions that exist and that no task row admits to.
//
// capN <= 0 means "no capacity predicate" and exists for the store's own tests;
// no production caller passes it.
func (s *Store) ClaimTaskSpawnCAS(runID int64, taskID, worktree, branch, baseSHA, baseProducers string, capN int, now int64) (claimed bool, err error) {
	const set = `UPDATE delegation_tasks SET state='spawning', worktree=?, branch=?, base_sha=?,
			base_producers=?, updated_at=?
		 WHERE run_id=? AND task_id=? AND state='approved'`
	args := []any{worktree, branch, baseSHA, baseProducers, now, runID, taskID}
	if capN <= 0 {
		return s.casExec(set, args...)
	}
	// The state list is spelled out rather than built from delegate.TaskState:
	// internal/store must not import internal/delegate (the dependency runs the
	// other way), and a cap is a safety property that should be readable in the
	// SQL a person runs by hand against loom.db. delegate.ActiveChildren's
	// switch is the same set, and a test asserts the two agree.
	return s.casExec(set+`
		   AND (SELECT COUNT(*) FROM delegation_tasks
		         WHERE run_id=? AND state IN ('spawning','running','blocked','checking')) < ?`,
		append(args, runID, capN)...)
}

// BindTaskSessionCAS is §13.3 step 4: spawning→running, writing the child's
// store name.
//
// claimed=false here is the residual hole §13.3 discloses and does NOT claim to
// have closed: a concurrent abandon can move the task out of 'spawning' while
// the launch is in flight, leaving a live child whose row is no longer
// spawning. The caller must surface that — the child is real and is running —
// rather than treat it as a no-op. Abandon sweeps by cwd over the run's
// deterministic worktree paths for exactly this case.
//
// The spawn-orphan recovery uses this same call: a live sessions row whose cwd
// equals physicalDir(<deterministic worktree>) is the child of a task still
// stuck in 'spawning', and completing the CAS adopts it. The 'spawning' guard
// is load-bearing there — revision 1's rule ("no tag and no commits ⇒
// re-approve") would have put a SECOND claude in the same worktree on the same
// branch, the worst outcome available in this design.
func (s *Store) BindTaskSessionCAS(runID int64, taskID, sessionName string, now int64) (claimed bool, err error) {
	return s.casExec(
		`UPDATE delegation_tasks SET state='running', session_name=?, updated_at=?
		 WHERE run_id=? AND task_id=? AND state='spawning'`,
		sessionName, now, runID, taskID)
}

// RecordTaskCheckCAS records a check result and moves the state in one
// statement — checking→verified on exit 0, checking→failed otherwise.
//
// Result and state are written together because they are one fact. Writing the
// result and then moving the state would allow a reader to see `verified` with
// a stale check output, or a green check on a task the UI still shows as
// running, and §5.2's merge gate reads both to decide whether to render the
// action at all.
//
// branchHead is the sha the check actually ran against, which is what §8.2's
// debounce compares on the next tick: without it a check re-runs forever
// against an unchanged tree, or never re-runs after a commit that landed
// mid-check.
func (s *Store) RecordTaskCheckCAS(runID int64, taskID, expectedState, newState, checkStatus string, exit int64, out, branchHead string, now int64) (claimed bool, err error) {
	return s.casExec(
		`UPDATE delegation_tasks SET state=?, check_status=?, check_exit=?, check_out=?,
			check_at=?, branch_head=?, updated_at=?
		 WHERE run_id=? AND task_id=? AND state=?`,
		newState, checkStatus, exit, out, now, branchHead, now, runID, taskID, expectedState)
}

// ClaimTaskIntegrationCAS is §10.2's serialization, enforced INSIDE the UPDATE:
// it moves one task into `integrating` only while no task of the same run is
// already there.
//
// Run-wide exclusion is not a nicety. A cross check (§10.2 step 4) reads several
// repos' integration worktrees at once and must not see one mid-merge; two
// concurrent passes in the SAME repo are worse still, because step 0's `pre` is
// captured by one and `git reset --hard <pre>` executed by the other, which
// silently discards a sibling's verified merge from the staging branch.
//
// The predicate lives in the statement for the reason ClaimTaskSpawnCAS's cap
// does: a caller that counts `integrating` tasks and then claims has two
// moments, and both racers count zero. An in-process mutex is not a candidate at
// all — two Loom instances against one DB is a supported configuration and a
// mutex is per-process.
//
// expectedStates is a source SET the caller must name in code (AdvanceTaskFromAnyCAS's
// argument): `verified` on the first attempt, plus whatever state a §10.2
// re-attempt starts from. Passing `integrating` itself is safe rather than
// clever — the exclusion predicate counts the task's own row and refuses.
func (s *Store) ClaimTaskIntegrationCAS(runID int64, taskID string, expectedStates []string, now int64) (claimed bool, previous string, err error) {
	const q = `UPDATE delegation_tasks SET state='integrating', updated_at=?
		 WHERE run_id=? AND task_id=? AND state=?
		   AND (SELECT COUNT(*) FROM delegation_tasks
		         WHERE run_id=? AND state='integrating') = 0`
	for _, st := range expectedStates {
		ok, err := s.casExec(q, now, runID, taskID, st, runID)
		if err != nil {
			return false, "", err
		}
		if ok {
			return true, st, nil
		}
	}
	return false, "", nil
}

// AbandonTaskCAS retires a task from any state except a terminal one. §13.2
// puts `abandoned` reachable "from anywhere"; refusing to re-abandon is what
// makes claimed=false mean something the caller can report.
func (s *Store) AbandonTaskCAS(runID int64, taskID string, now int64) (claimed bool, err error) {
	return s.casExec(
		`UPDATE delegation_tasks SET state='abandoned', updated_at=?
		 WHERE run_id=? AND task_id=? AND state NOT IN ('abandoned','merged')`,
		now, runID, taskID)
}

// SetTaskFlags replaces the JSON flag set. Deliberately NOT a CAS and
// deliberately not folded into any transition: §13.2 keeps flags independent of
// the state column precisely so they can be set and cleared by a watchdog
// without racing the runner, and a flag never gates a transition on its own. A
// lost flag update costs one poll interval of a stale badge.
func (s *Store) SetTaskFlags(runID int64, taskID, flags string, now int64) error {
	return s.execTask("UPDATE delegation_tasks SET flags=?, updated_at=? WHERE run_id=? AND task_id=?",
		flags, now, runID, taskID)
}

// SetTaskFlagsCAS is SetTaskFlags for a flag that is the ONLY record of a
// failure — §11.4's undelivered seed, §10.5's stale-contract withdrawal — rather
// than a badge a later poll would recompute anyway.
//
// Both exist because the two cases have opposite failure modes. Flags is one
// JSON set, so a plain write is read-modify-write over the WHOLE set: a watchdog
// clearing `stalled` at the same moment the seed deliverer records its failure
// erases the failure, and §12's whole premise is that failures are visible. The
// unconditional setter is still correct for a badge — retrying a CAS on every
// poll to re-assert `stalled` costs more than the stale badge it prevents — so
// the caller chooses, and this comment is the basis on which it chooses.
//
// claimed=false means the set moved: re-read, re-merge, re-try. It does NOT
// mean the flag is set.
func (s *Store) SetTaskFlagsCAS(runID int64, taskID, expected, next string, now int64) (claimed bool, err error) {
	return s.casExec(
		"UPDATE delegation_tasks SET flags=?, updated_at=? WHERE run_id=? AND task_id=? AND flags=?",
		next, now, runID, taskID, expected)
}

// SetTaskBlock records the child's block declaration verbatim. The parse
// happens above the store: a malformed declaration is still evidence and is
// still shown to the human (the `block-malformed` flag), so the store keeps the
// bytes it was given rather than validating them away.
func (s *Store) SetTaskBlock(runID int64, taskID, blockJSON string, now int64) error {
	return s.execTask("UPDATE delegation_tasks SET block_json=?, updated_at=? WHERE run_id=? AND task_id=?",
		blockJSON, now, runID, taskID)
}

// SetTaskPendingSeed stores an undelivered continuation seed — the same
// durable-seed shape as workflow_runs.pending_seed, whose whole point is that
// a seed which was never delivered is visible rather than lost.
//
// seed_owed_since is stamped ONLY on the transition from "nothing owed" to
// "something owed", which is why it is a CASE and not an assignment. Every
// caller of this function is either arming a new debt or RE-ARMING one it just
// failed to deliver, and the poll re-attempts every couple of seconds; a plain
// assignment would restart the clock on every attempt and §12.2's `block-stale`
// row — the only escalation a parked child has — could never reach its 5-minute
// threshold. The row's own updated_at cannot serve for the same reason: it is
// the age of the last WRITE, and an attempt is a write.
//
// The seed text is still overwritten on every call: a newer continuation
// supersedes an older one (deliver() re-reads for exactly that reason), and the
// debt those two texts represent is the same debt.
func (s *Store) SetTaskPendingSeed(runID int64, taskID, seed string, now int64) error {
	return s.execTask(`UPDATE delegation_tasks
		 SET pending_seed=?,
		     seed_owed_since = CASE WHEN seed_owed_since = 0 THEN ? ELSE seed_owed_since END,
		     updated_at=?
		 WHERE run_id=? AND task_id=?`,
		seed, now, now, runID, taskID)
}

// ClearTaskPendingSeedCAS clears the seed only if it is still the exact text
// the deliverer sent. This is a CAS and workflow's ClearPendingSeed is not,
// because the delegation path re-reads before delivering (§11.4's
// double-delivery guard): an unconditional clear here would erase a NEWER seed
// written between the read and the send, and the child would sit blocked
// forever on a continuation nothing will re-issue.
//
// It clears seed_owed_since in the SAME statement. Two writes would mean a
// window in which nothing is owed and a stale debt clock is still ticking, and
// §12.2 would offer a retry for a seed that has already been delivered.
func (s *Store) ClearTaskPendingSeedCAS(runID int64, taskID, expectedSeed string, now int64) (claimed bool, err error) {
	return s.casExec(
		"UPDATE delegation_tasks SET pending_seed='', seed_owed_since=0, updated_at=? WHERE run_id=? AND task_id=? AND pending_seed=?",
		now, runID, taskID, expectedSeed)
}

// SetTaskDivergence records the files the child touched outside its declared
// paths (§12.3). Persisted rather than recomputed because §5.2's merge gate
// requires a second explicit acknowledgement when it is non-empty — it gates a
// human decision, so it has to still be there when the human comes back.
func (s *Store) SetTaskDivergence(runID int64, taskID, divergence string, now int64) error {
	return s.execTask("UPDATE delegation_tasks SET divergence=?, updated_at=? WHERE run_id=? AND task_id=?",
		divergence, now, runID, taskID)
}

// SetTaskBranchHead records the branch sha without touching the check result —
// used when the check is skipped (§8.2's "branch moved but the child is
// mid-generation") so the next tick still sees the movement.
func (s *Store) SetTaskBranchHead(runID int64, taskID, head string, now int64) error {
	return s.execTask("UPDATE delegation_tasks SET branch_head=?, updated_at=? WHERE run_id=? AND task_id=?",
		head, now, runID, taskID)
}

// SetTaskSpawnSnapshot records §12.3.3's pre-spawn fingerprint of every
// directory the child can reach but does not own.
//
// Ungated, and safely so: it is written by whoever already holds the task's
// spawn claim (ClaimTaskSpawnCAS put the row in `spawning`), so there is exactly
// one writer by construction. Folding it into that claim was the alternative and
// was rejected for a mundane reason — the walk is O(dirty files across every
// in-scope repo) and would have to complete before the claim, widening the
// window in which two approvals both see an unclaimed task.
//
// An empty snapshot is a real value meaning "nothing dirty anywhere at spawn",
// not "not captured"; §12.3.3's comparator says "changed since spawn" either way
// and never says "the child wrote this", because a repo the human is working in
// is indistinguishable from a misbehaving child.
func (s *Store) SetTaskSpawnSnapshot(runID int64, taskID, snapshot string, now int64) error {
	return s.execTask("UPDATE delegation_tasks SET spawn_snapshot=?, updated_at=? WHERE run_id=? AND task_id=?",
		snapshot, now, runID, taskID)
}

// SetTaskNeedsSnapshot records the fingerprint+commit of every artifact this
// task `needs`, as of spawn — §10.5's stale-contract baseline (migration v15).
// Same single-writer argument as SetTaskSpawnSnapshot.
//
// Without it the alarm cannot fire: UpsertDelegationArtifact keeps only the
// newest publication, so a producer that revises its interface after the
// consumer was built against it destroys the very value the comparison needs,
// and "did the contract change?" is answered by comparing the current
// fingerprint with itself.
func (s *Store) SetTaskNeedsSnapshot(runID int64, taskID, snapshot string, now int64) error {
	return s.execTask("UPDATE delegation_tasks SET needs_snapshot=?, updated_at=? WHERE run_id=? AND task_id=?",
		snapshot, now, runID, taskID)
}

// SetTaskCertifiedSHA records the task-branch commit a green §10.2 pass
// certified (migration v18). §5.2's gate refuses when the branch has moved past
// it, which is what makes "the diff shown is the diff applied" true.
//
// Ungated, and the single-writer argument is stronger here than for the two
// snapshot setters: the only caller holds the run-wide integration claim, which
// ClaimTaskIntegrationCAS enforced inside its own UPDATE, so no second writer
// exists even across Loom instances. Folding it into the mergeable CAS was the
// alternative and is worse in one specific way — the sha must be durable BEFORE
// the state says `mergeable`, or a crash between the two leaves a gate open on
// a task with no certified sha, which is precisely the case this column exists
// to refuse.
func (s *Store) SetTaskCertifiedSHA(runID int64, taskID, sha string, now int64) error {
	return s.execTask("UPDATE delegation_tasks SET certified_sha=?, updated_at=? WHERE run_id=? AND task_id=?",
		sha, now, runID, taskID)
}

// DelegationTasksInStates returns every task in any of the given states, across
// ALL runs, newest-updated last. It backs §12.2's watchdogs, which are
// deliberately run-agnostic: `spawn-orphan` sweeps `spawning`, `no-progress`
// sweeps `running`, and a watchdog that iterated runs first would skip a task
// whose run row is momentarily unreadable — the sweep matters most exactly when
// something is wrong.
//
// An empty set returns no rows rather than every row: the caller computed its
// own filter to empty, and answering "everything" to a query for nothing is how
// a watchdog acts on a task it never meant to name.
func (s *Store) DelegationTasksInStates(states ...string) ([]DelegationTask, error) {
	if len(states) == 0 {
		return nil, nil
	}
	q := "SELECT " + dtaskCols + " FROM delegation_tasks WHERE state IN (?" +
		strings.Repeat(",?", len(states)-1) + ") ORDER BY updated_at, run_id, task_id"
	args := make([]any, len(states))
	for i, st := range states {
		args[i] = st
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DelegationTask
	for rows.Next() {
		t, err := scanDelegationTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpsertDelegationArtifact records a published artifact: tracked, committed,
// and fingerprinted at the moment §8.3 verified it. Upsert rather than
// insert-only because a task that is re-spawned republishes the same artifact
// id at a new commit, and the newest publication is the one every `needs` edge
// should resolve against.
func (s *Store) UpsertDelegationArtifact(a DelegationArtifact) error {
	_, err := s.db.Exec(`INSERT INTO delegation_artifacts (`+dartCols+`)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(run_id, artifact_id) DO UPDATE SET
			task_id=excluded.task_id, path=excluded.path,
			fingerprint=excluded.fingerprint, commit_sha=excluded.commit_sha,
			published_at=excluded.published_at`,
		a.RunID, a.ArtifactID, a.TaskID, a.Path, a.Fingerprint, a.CommitSHA, a.PublishedAt)
	return err
}

func (s *Store) GetDelegationArtifact(runID int64, artifactID string) (DelegationArtifact, bool, error) {
	var a DelegationArtifact
	err := s.db.QueryRow("SELECT "+dartCols+" FROM delegation_artifacts WHERE run_id=? AND artifact_id=?",
		runID, artifactID).Scan(&a.RunID, &a.ArtifactID, &a.TaskID, &a.Path,
		&a.Fingerprint, &a.CommitSHA, &a.PublishedAt)
	if err == sql.ErrNoRows {
		return DelegationArtifact{}, false, nil
	}
	if err != nil {
		return DelegationArtifact{}, false, err
	}
	return a, true, nil
}

// ListDelegationArtifacts returns a run's published artifacts in id order.
// §9.1's ready predicate needs the whole set at once — a per-edge lookup would
// be one query per dependency on every scheduler tick.
func (s *Store) ListDelegationArtifacts(runID int64) ([]DelegationArtifact, error) {
	rows, err := s.db.Query(
		"SELECT "+dartCols+" FROM delegation_artifacts WHERE run_id=? ORDER BY artifact_id", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DelegationArtifact
	for rows.Next() {
		var a DelegationArtifact
		if err := rows.Scan(&a.RunID, &a.ArtifactID, &a.TaskID, &a.Path,
			&a.Fingerprint, &a.CommitSHA, &a.PublishedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AppendDelegationAmendment adds one amendment to a run and returns its seq.
// §11.3: append-only, never a mutation of the on-disk manifest — Loom does not
// own that file and the author may have it open.
//
// The next seq is computed inside the INSERT rather than by SELECT-then-INSERT.
// The two-statement version reads the max, and a writer in another Loom instance
// commits between the read and the write; both then insert the same seq and one
// of them loses — either to a PK violation (visible, annoying) or, if the table
// had used an unconstrained id, silently to a duplicated ordinal that makes the
// replay order ambiguous. As one statement the read is part of the write, so
// SQLite's single-writer lock is the serialization and no second mechanism is
// needed.
//
// There is no Update and no Delete, deliberately: the effective graph is the
// manifest snapshot plus these rows replayed in order, so a rewritable amendment
// rewrites a plan a child may already be parked against. A withdrawn amendment
// is a NEW amendment that supersedes it, which leaves the history legible.
func (s *Store) AppendDelegationAmendment(runID int64, kind, body string, now int64) (int64, error) {
	var seq int64
	err := s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO delegation_amendments (`+damdCols+`)
			SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, 0, 0, ?
			  FROM delegation_amendments WHERE run_id=?`,
			runID, kind, body, now, runID); err != nil {
			return err
		}
		return tx.QueryRow("SELECT MAX(seq) FROM delegation_amendments WHERE run_id=?", runID).Scan(&seq)
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// ListDelegationAmendments returns a run's amendments in seq order — the order
// they must be replayed in to build the effective graph (§11.3). Whole-run
// rather than paged: §4.5's cycle detection re-runs over the WHOLE amended graph
// on every amendment, and a partial read there answers "no cycle" for a graph
// that has one.
func (s *Store) ListDelegationAmendments(runID int64) ([]DelegationAmendment, error) {
	rows, err := s.db.Query(
		"SELECT "+damdCols+" FROM delegation_amendments WHERE run_id=? ORDER BY seq", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DelegationAmendment
	for rows.Next() {
		a, err := scanDelegationAmendment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetDelegationAmendment(runID, seq int64) (DelegationAmendment, bool, error) {
	a, err := scanDelegationAmendment(s.db.QueryRow(
		"SELECT "+damdCols+" FROM delegation_amendments WHERE run_id=? AND seq=?", runID, seq))
	if err == sql.ErrNoRows {
		return DelegationAmendment{}, false, nil
	}
	if err != nil {
		return DelegationAmendment{}, false, err
	}
	return a, true, nil
}

// ApproveDelegationAmendmentCAS records the human's yes, once. §11.3's
// authorization amendment is "human-approved, never auto-granted", and approval
// has a side effect — it rewrites the child's brief.md and re-seeds it — so a
// second approval would re-seed a child that is already working. claimed=false
// means someone (or another Loom instance) already approved it, and the caller
// must not perform the side effect.
//
// approved_at=0 is the guard rather than a separate status column: an approval
// is a timestamp or it is nothing, and two representations of the same fact is
// how they come to disagree.
func (s *Store) ApproveDelegationAmendmentCAS(runID, seq, now int64) (claimed bool, err error) {
	// rejected_at=0 is in the guard, not only approved_at: a human who said no
	// and a human who said yes are contending for the same decision, and an
	// approval that overwrote a rejection would perform the side effect the
	// rejection existed to prevent. Enforced INSIDE the UPDATE — a check the
	// caller makes first is advisory, and this file's whole discipline is that
	// two Loom instances are supported.
	return s.casExec(
		"UPDATE delegation_amendments SET approved_at=? WHERE run_id=? AND seq=? AND approved_at=0 AND rejected_at=0",
		now, runID, seq)
}

// RejectDelegationAmendmentCAS records the human's NO, once (§11.3).
//
// The mirror of ApproveDelegationAmendmentCAS and guarded identically. It is a
// CAS and not a plain UPDATE for the same reason approval is: the two decisions
// race, the loser must be TOLD rather than silently overwriting, and a caller
// that read the row a second ago is holding a snapshot.
//
// The row is not deleted. delegation_amendments is append-only because the
// effective graph is a replay of it, and an amendment that vanished would erase
// the record of a decision a parked child is waiting on the far side of.
func (s *Store) RejectDelegationAmendmentCAS(runID, seq, now int64) (claimed bool, err error) {
	return s.casExec(
		"UPDATE delegation_amendments SET rejected_at=? WHERE run_id=? AND seq=? AND approved_at=0 AND rejected_at=0",
		now, runID, seq)
}

// casExec is the shared compare-and-swap tail: run the guarded UPDATE, report
// whether it matched. Factored out so every transition in this file is provably
// the same shape as AdvanceRunCAS — a hand-rolled one that forgot to check
// RowsAffected would silently claim every race it lost.
func (s *Store) casExec(q string, args ...any) (bool, error) {
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// execTask is the tail for the ungated setters. It does not report whether the
// row existed: these fields are all annotations on a task the caller just read,
// and turning "the run was abandoned under me" into an error on a flag write
// would make every watchdog handle a condition it cannot act on.
func (s *Store) execTask(q string, args ...any) error {
	_, err := s.db.Exec(q, args...)
	return err
}

func (s *Store) queryDelegationRuns(q string, args ...any) ([]DelegationRun, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DelegationRun
	for rows.Next() {
		r, _, err := scanDelegationRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanDelegationRun(row rowScanner) (DelegationRun, bool, error) {
	var r DelegationRun
	err := row.Scan(&r.ID, &r.Slug, &r.Name, &r.ProjectRoot, &r.ManifestJSON,
		&r.BaseSHAs, &r.Integration, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return DelegationRun{}, false, nil
	}
	if err != nil {
		return DelegationRun{}, false, err
	}
	return r, true, nil
}

func scanDelegationTask(row rowScanner) (DelegationTask, error) {
	var t DelegationTask
	err := row.Scan(&t.RunID, &t.TaskID, &t.State, &t.SessionName, &t.RepoLabel,
		&t.Worktree, &t.Branch, &t.BaseSHA, &t.BaseProducers, &t.CheckStatus,
		&t.CheckExit, &t.CheckOut, &t.CheckAt, &t.BranchHead, &t.BlockJSON,
		&t.PendingSeed, &t.Divergence, &t.SpawnSnapshot, &t.NeedsSnapshot,
		&t.SeedOwedSince, &t.CertifiedSHA, &t.Flags, &t.UpdatedAt)
	return t, err
}

func scanDelegationAmendment(row rowScanner) (DelegationAmendment, error) {
	var a DelegationAmendment
	err := row.Scan(&a.RunID, &a.Seq, &a.Kind, &a.Body, &a.ApprovedAt, &a.RejectedAt, &a.CreatedAt)
	return a, err
}
