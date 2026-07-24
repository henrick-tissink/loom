package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

// probe_failure_test.go — the §§9-12 failure-mode probe.
//
// Everything here builds its own repo under t.TempDir(). Nothing names the loom
// repo, for worktree_test.go's reason: `git merge`, `git merge --abort` and
// `git worktree add` write into whatever repo they are pointed at.
//
// Each test states the BINDING sentence it is probing and the damage the
// absence of that behaviour does. A test in this file that fails is a defect
// report, not a flaky assertion — none of them depends on wall-clock timing
// except where a gate file makes the ordering deterministic.

// ─────────────────────────────────────────────────────────────────────────────
// §10.4 / §5.2 — the merge into a tree LOOM DOES NOT OWN. The worst class in the
// slice, because a mistake here damages work Loom did not create.

// §10.3: "merge into the user's own branch requires the target repo's working
// tree to be clean; a dirty tree is refused with the offending files named,
// because merging into a dirty tree is how a human loses work to a machine."
//
// The clean-tree predicate is `gitdiff.WorkingTree(repo).Dirty`, i.e. a non-empty
// `git diff HEAD` or an untracked file. An IN-PROGRESS MERGE whose index happens
// to match HEAD satisfies that predicate — `git status --porcelain` is empty and
// MERGE_HEAD exists — so the guard passes, `git merge` refuses ("You have not
// concluded your merge"), and integrate.go's failure path then runs
// `git merge --abort` UNCONDITIONALLY. That abort belongs to the human's merge,
// not to Loom's: it discards MERGE_HEAD and every resolution staged in it, and
// the error Loom returns says the TASK BRANCH conflicted, which is false.
//
// This is the one path found in this probe that destroys state Loom did not
// create. The correct behaviour is to refuse on any in-progress operation
// (MERGE_HEAD / CHERRY_PICK_HEAD / REVERT_HEAD / rebase dir) and to abort only a
// merge Loom itself started.
func TestProbeMergeAbortsTheHumansOwnInProgressMerge(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	tk := f.task("schema", map[string]string{"schema.sql": "create table t;\n"})
	if _, err := f.i.Integrate(context.Background(), f.run, f.m, tk); err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	f.reload()

	// The human starts a merge of their own and leaves it uncommitted. The
	// content is already present on main, so the merge is clean AND the index
	// matches HEAD — `git status` is empty and the dirty guard sees nothing.
	mustGit(t, f.repo, "checkout", "-q", "-b", "human-side", "main")
	writeFile(t, filepath.Join(f.repo, "side.txt"), "shared\n")
	mustGit(t, f.repo, "add", "side.txt")
	mustGit(t, f.repo, "commit", "-qm", "the human's side branch")
	mustGit(t, f.repo, "checkout", "-q", "main")
	writeFile(t, filepath.Join(f.repo, "side.txt"), "shared\n")
	mustGit(t, f.repo, "add", "side.txt")
	mustGit(t, f.repo, "commit", "-qm", "the same change, already on main")
	mustGit(t, f.repo, "merge", "--no-commit", "--no-ff", "human-side")

	mergeHead := filepath.Join(f.repo, ".git", "MERGE_HEAD")
	if _, err := os.Stat(mergeHead); err != nil {
		t.Fatalf("the fixture did not produce an in-progress merge, so this test proves nothing: %v", err)
	}
	if got := strings.TrimSpace(mustGit(t, f.repo, "status", "--porcelain")); got != "" {
		t.Fatalf("the fixture's tree is dirty (%q), so the dirty guard would refuse and this test proves nothing", got)
	}

	_, err := f.i.Merge(context.Background(), f.run, f.m, tk, false)
	if err == nil {
		t.Fatalf("Merge succeeded on top of an in-progress merge")
	}
	if _, statErr := os.Stat(mergeHead); statErr != nil {
		t.Errorf("Loom ran `git merge --abort` on the HUMAN's in-progress merge: MERGE_HEAD is gone (%v). "+
			"Their staged resolution is discarded and Loom reported it as %q", statErr, err)
	}
	if strings.Contains(err.Error(), "conflict") {
		t.Errorf("the refusal blames a conflict that never happened: %v", err)
	}
}

// §5.2 / §10.4: "what the gate merges into the user's branch is the task's own
// branch, so THE DIFF SHOWN IS THE DIFF APPLIED", and §5.2's precondition is a
// green integration pass over that work.
//
// Merge resolves the branch by NAME at merge time. A child that commits again
// after its task reached `mergeable` — which nothing stops it doing; Tick only
// re-checks `running` and `blocked` tasks, never `mergeable` ones — moves the
// branch head, and the merge lands commits that no check, no integration pass
// and no preview ever saw. There is no divergence signal either when the new
// commit is inside the task's declared paths.
//
// The gate must either pin the certified sha or refuse a branch that moved.
func TestProbeMergeLandsCommitsNoCheckEverCertified(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	tk := f.task("schema", map[string]string{"schema.sql": "create table t;\n"})
	if _, err := f.i.Integrate(context.Background(), f.run, f.m, tk); err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	f.reload()
	if got := f.state("schema"); got != StateMergeable {
		t.Fatalf("state = %s, want mergeable", got)
	}
	certified := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", BranchName(f.run.Slug, "schema")))

	// The child keeps working after the gate appeared.
	branch := BranchName(f.run.Slug, "schema")
	mustGit(t, f.repo, "checkout", "-q", branch)
	writeFile(t, filepath.Join(f.repo, "uncertified.sql"), "drop table t;\n")
	mustGit(t, f.repo, "add", "uncertified.sql")
	mustGit(t, f.repo, "commit", "-qm", "work nobody checked")
	mustGit(t, f.repo, "checkout", "-q", "main")

	res, err := f.i.Merge(context.Background(), f.run, f.m, tk, false)
	if err != nil {
		// A refusal is the acceptable outcome: the certified picture moved.
		return
	}
	if _, statErr := os.Stat(filepath.Join(f.repo, "uncertified.sql")); statErr == nil {
		t.Errorf("the merge landed a commit that no check and no integration pass ever ran against "+
			"(certified %s, merged %s); result stage=%s status=%s",
			short(certified), short(strings.TrimSpace(mustGit(t, f.repo, "rev-parse", branch))),
			res.Stage, res.Status)
	}
}

// §10.2: "Serialized per run — ONE INTEGRATION AT A TIME, RUN-WIDE", enforced by
// store.ClaimTaskIntegrationCAS because "a claim enforced outside the UPDATE is
// advisory".
//
// Integrator.Merge takes neither the in-process lock nor the store claim, and
// §10.4 step 2 (`rederive`) does `git reset --hard <user head>` plus
// `git clean -fd` in the SAME integration worktree a concurrent Integrate pass
// is standing in. The consequences are both silent:
//
//   - the in-flight pass's staging merge is deleted from under it, so its check
//     — and therefore its verdict — describes a tree that does not contain the
//     task being certified;
//   - the pass then records a GREEN baseline for a head whose tree never held
//     its own work, and promotes the task to `mergeable` on that evidence.
//
// The gate the whole slice rests on is then green for a combination nobody ran.
func TestProbeMergeRacesAConcurrentIntegrationOfTheSameRepo(t *testing.T) {
	gate := t.TempDir()
	script := filepath.Join(gate, "check.sh")
	// The first invocation after the switch blocks; every later one returns at
	// once, so `rederive`'s own re-run cannot deadlock the test.
	writeFile(t, script, "exit 0\n")

	f := newIntegrationFixture(t, []string{"sh", script}, nil)
	beta := f.task("beta", map[string]string{"b.txt": "beta\n"})
	alpha := f.task("alpha", map[string]string{"a.txt": "alpha\n"})

	if _, err := f.i.Integrate(context.Background(), f.run, f.m, beta); err != nil {
		t.Fatalf("Integrate(beta): %v", err)
	}
	f.reload()

	writeFile(t, script, "if [ ! -f "+gate+"/first ]; then\n"+
		"  touch "+gate+"/first "+gate+"/started\n"+
		"  while [ ! -f "+gate+"/go ]; do sleep 0.02; done\n"+
		"fi\nexit 0\n")

	done := make(chan error, 1)
	go func() {
		_, err := f.i.Integrate(context.Background(), f.run, f.m, alpha)
		done <- err
	}()
	waitForFile(t, filepath.Join(gate, "started"))

	// The human presses §5.2's gate on beta while alpha's pass is mid-check.
	if _, err := f.i.Merge(context.Background(), f.run, f.m, beta, false); err != nil {
		if errors.Is(err, ErrIntegrationBusy) {
			writeFile(t, filepath.Join(gate, "go"), "")
			<-done
			return // the correct outcome: the merge waited for the run's pass
		}
		writeFile(t, filepath.Join(gate, "go"), "")
		<-done
		t.Fatalf("Merge(beta): %v", err)
	}
	writeFile(t, filepath.Join(gate, "go"), "")
	if err := <-done; err != nil {
		t.Fatalf("Integrate(alpha): %v", err)
	}

	if got := f.state("alpha"); got == StateMergeable {
		if _, err := os.Stat(filepath.Join(f.integrationDir(), "a.txt")); err != nil {
			t.Errorf("alpha was promoted to `mergeable` on a green pass, but its work is not in the integration "+
				"worktree at all (%v): the concurrent Merge reset the staging area under the pass, "+
				"so the check that certified alpha never saw alpha", err)
		}
	}
	if _, err := os.Stat(filepath.Join(f.integrationDir(), "b.txt")); err != nil {
		t.Errorf("beta's merged work is missing from the re-derived staging area (%v): "+
			"the concurrent pass's step-R reset threw away §10.4 step 2's re-derivation", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// §10.2 — interruption, attribution and recovery.

// §10.2 step 0/R: every failure path ends in `git reset --hard <pre>`, and
// "THE INTEGRATION BRANCH ONLY EVER CONTAINS WORK THAT HAS BEEN GREEN END TO END".
// That invariant is maintained only by in-process error paths. A Loom that dies
// between step 1 and step 5 — the window is one whole check timeout wide — leaves
// the row at `integrating` and the un-green merge in the branch forever.
//
// Nothing recovers it. `integrating` is InFlight for §9.3, so the run is not
// deadlocked; Watch has no row for `integrating` or `checking`, so no watchdog
// fires; and ClaimTaskIntegrationCAS's run-wide exclusion counts that row, so
// EVERY OTHER TASK IN THE RUN can never integrate again. A crash therefore stops
// the run permanently while it renders as healthy progress — precisely the shape
// §12.1 exists to make impossible.
func TestProbeACrashMidIntegrationWedgesTheWholeRunSilently(t *testing.T) {
	f := newIntegrationFixture(t, []string{"true"}, nil)
	alpha := f.task("alpha", map[string]string{"a.txt": "alpha\n"})
	beta := f.task("beta", map[string]string{"b.txt": "beta\n"})
	_ = alpha

	// The crash: the claim landed, the sequence never finished.
	claimed, _, err := f.store.ClaimTaskIntegrationCAS(f.run.ID, "alpha", []string{string(StateVerified)}, 1000)
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	f.reload()

	if _, err := f.i.Integrate(context.Background(), f.run, f.m, beta); !errors.Is(err, ErrIntegrationBusy) {
		t.Fatalf("Integrate(beta) = %v, want ErrIntegrationBusy (alpha holds the run-wide claim)", err)
	}

	// Two hours later, with nothing else happening.
	later := epoch.Add(2 * time.Hour)
	obs := []Observation{
		{TaskID: "alpha", State: StateIntegrating, Since: epoch},
		{TaskID: "beta", State: StateVerified, Since: epoch},
	}
	if got := Watch(later, obs, Budget{StartedAt: epoch}); len(got) != 0 {
		return // something noticed; good
	}
	states := map[string]TaskState{"alpha": StateIntegrating, "beta": StateVerified}
	p := Tick(Effective(f.m, nil, nil), states, nil)
	t.Errorf("a task wedged in `integrating` for 2h produced no watchdog finding and Deadlocked=%v: "+
		"the run cannot integrate anything ever again, and nothing tells the human", p.Deadlocked)
}

// §10.2's attribution table, second row: "red with the task merged AND red at
// `pre` → the integration BASELINE — a run-level fault: the run row goes red, no
// task is blamed, spawning stops."
//
// The blame is computed correctly. What is recorded is not: `attribute` pays for
// a real check run AT `pre` and stashes its output in res.baseline, and then
// `record`'s BlameBaseline branch overwrites that entry with the output of the
// run WITH THE TASK STAGED. The `integration` column is the ONLY place §12.1's
// renderer can learn why the run is red (run.go's redRunReason reads exactly
// this), so the human is shown a failure captured from a tree that contained the
// task nobody is blaming.
func TestProbeBaselineFaultRecordsTheWrongEvidence(t *testing.T) {
	// The check names the tree it ran in. `a.txt` is alpha's file, so it is
	// present with the task staged and absent at `pre`.
	f := newIntegrationFixture(t, []string{"sh", "-c", `if [ -f a.txt ]; then echo WITH_TASK_STAGED; else echo AT_PRE_WITHOUT_THE_TASK; fi; exit 1`}, nil)
	alpha := f.task("alpha", map[string]string{"a.txt": "alpha\n"})

	res, err := f.i.Integrate(context.Background(), f.run, f.m, alpha)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if res.Blame != BlameBaseline {
		t.Fatalf("Blame = %q, want baseline (the check is red at `pre` too)", res.Blame)
	}
	b := DecodeBaselines(f.reload().Integration)["app"]
	if !strings.Contains(b.Out, "AT_PRE_WITHOUT_THE_TASK") {
		t.Errorf("the recorded baseline output is %q: the run row says no task is to blame and then shows "+
			"the failure captured WITH that task staged, which is the evidence for the opposite verdict", b.Out)
	}
}

// §10.2: a baseline fault stops spawning "until it is fixed"; §12.1's deadlock is
// permanent BY DESIGN. Both land on delegation_runs.status = 'deadlocked', and
// nothing in the tree ever moves a run OUT of it (grep: every
// AdvanceDelegationRunCAS names 'deadlocked' as the DESTINATION). So a single
// environment-shaped red at `pre` — a busy port, a stale DB, §6.4's disclosed
// non-isolation, one flake — permanently bricks the run: Approve refuses with
// ErrRunRed, Preview blocks every merge with "the run is red", and the only exit
// is Abandon. Every child's committed work is then stranded behind a gate that
// cannot be reopened.
//
// The fix is a human-pressed re-arm (or a green baseline re-run clearing the
// status); §10.2's own wording implies one exists.
func TestProbeATransientBaselineRedBricksTheRunForever(t *testing.T) {
	gate := t.TempDir()
	red := filepath.Join(gate, "red")
	writeFile(t, red, "the environment is broken\n")

	f := newIntegrationFixture(t, []string{"sh", "-c", "test ! -f " + red}, nil)
	alpha := f.task("alpha", map[string]string{"a.txt": "alpha\n"})

	res, err := f.i.Integrate(context.Background(), f.run, f.m, alpha)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if res.Blame != BlameBaseline {
		t.Fatalf("Blame = %q, want baseline", res.Blame)
	}
	if got := f.reload().Status; got != "deadlocked" {
		t.Fatalf("run status = %q, want deadlocked", got)
	}

	// The human fixes the environment. The baseline is green again and the task
	// integrates cleanly.
	if err := os.Remove(red); err != nil {
		t.Fatal(err)
	}
	res, err = f.i.Integrate(context.Background(), f.run, f.m, alpha)
	if err != nil {
		t.Fatalf("Integrate after the fix: %v", err)
	}
	if !res.Green() {
		t.Fatalf("the second pass is %s/%s, want green — the fixture no longer proves anything", res.Stage, res.Status)
	}
	if got := f.reload().Status; got != "running" {
		t.Errorf("run status = %q after a GREEN integration pass: nothing in the package can un-red a run, "+
			"so one environment blip stops every spawn and every merge for the rest of the run's life", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// §11.4 / §12.2 — a child parked forever.

// §12.2's `block-stale` row: "`blocked`, unblock condition satisfied >5m, seed
// not delivered ⇒ render `seed pending`, offer retry". It is the ONLY escalation
// a park has: nothing else fires for a blocked task whose child is alive, and
// Progress counts a blocked task as non-terminal but the run is not deadlocked
// while other tasks are in flight.
//
// run.go's watch() derives Observation.UnblockedAt from delegation_tasks.
// updated_at — and Rendezvous.Seed WRITES that column (SetTaskPendingSeed sets
// updated_at=now) on every delivery attempt. Tick re-attempts on every poll, so
// the debt's age resets to zero every 2 seconds and the 5-minute threshold is
// unreachable in exactly the case it was written for: a child that never returns
// to a prompt. The seed stays owed forever, the badge stays on, and the one
// affordance that would tell a human to go and look is never offered.
//
// The debt needs its own durable timestamp (owed_since), not the row's mtime.
func TestProbeBlockStaleNeverFiresWhileTheSeedIsRetried(t *testing.T) {
	r, run, _ := newRunFixture(t)
	now := epoch
	r.Now = func() time.Time { return now }

	// A live child that is mid-turn: no transcript at all, so the continue gate
	// never opens and every delivery attempt times out.
	const child = "loom-child-probe"
	if err := r.Store.Upsert(store.SessionRow{
		Name: child, ClaudeSessionID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		Cwd: t.TempDir(), CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "running",
	}); err != nil {
		t.Fatal(err)
	}
	parkTaskWithAChild(t, r.Store, run, "auth-api", child)

	rv := &Rendezvous{
		Store: r.Store, Layout: r.Layout, Tmux: &fakeSender{},
		ClaudeConfigDir: t.TempDir(),
		PollEvery:       time.Millisecond, Timeout: time.Millisecond,
		Now: func() time.Time { return now },
	}

	// Twenty ticks over twenty minutes — four times the threshold.
	for i := 1; i <= 20; i++ {
		now = epoch.Add(time.Duration(i) * time.Minute)
		if err := rv.Seed(run, "auth-api", "`account-schema` is now present. Continue."); !errors.Is(err, ErrSeedUndelivered) {
			t.Fatalf("tick %d: Seed = %v, want ErrSeedUndelivered (the child is mid-turn)", i, err)
		}
		st, err := r.load(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range r.watch(st, now) {
			if f.Kind == WatchBlockStale {
				return // escalated; good
			}
		}
	}
	t.Errorf("a seed owed for 20 minutes to a child that never returns to its prompt produced no `block-stale` "+
		"finding: every delivery attempt rewrites pending_seed and therefore updated_at, which is where "+
		"watch() reads UnblockedAt from, so the debt is forever 0 minutes old")
}

// §12.1: the two deadlock shapes exist "because the REMEDIES differ", and
// ShapeStarved is documented as "a fault in Loom rather than a fault in the
// plan" — the shape a human is told to report as a bug.
//
// Abandoning one task (§13.2: `abandoned` from anywhere, and Abandon is the only
// exit from a bricked run) reaches exactly that shape. The consumer can never be
// ready — needsMet requires the producer verified — nothing is in flight and no
// block is live, so DetectDeadlock returns ShapeStarved and the run view tells
// the human their tool is confused. The truth is an actionable re-plan: the
// producer of `account-schema` was abandoned.
func TestProbeAnAbandonedProducerReadsAsABugInLoom(t *testing.T) {
	_, _, m := newRunFixture(t)
	e := Effective(m, nil, nil)
	states := map[string]TaskState{"schema": StateAbandoned, "auth-api": StatePending}
	p := Tick(e, states, nil)
	if !p.Deadlocked {
		t.Fatalf("progress = %+v, want the run to have stopped", p)
	}
	d := DetectDeadlock(e, p, states)
	if d.Shape == ShapeStarved {
		t.Errorf("an abandoned producer classifies as %q, which §12.1 defines as a fault in Loom: "+
			"the human is told to report a bug when the actionable answer is `schema` was abandoned and "+
			"`auth-api` needs a re-plan", d.Shape)
	}
}

// parkTaskWithAChild walks a task pending → ready → approved → spawning →
// running → blocked through the real CAS API and binds a session to it. The row
// shape matters: session_name is written only by BindTaskSessionCAS, and a
// fixture that wrote it behind the store's back would be testing a row the
// running system cannot produce.
func parkTaskWithAChild(t *testing.T, s *store.Store, run store.DelegationRun, taskID, session string) {
	t.Helper()
	steps := [][2]string{
		{string(StatePending), string(StateReady)},
		{string(StateReady), string(StateApproved)},
	}
	for _, st := range steps {
		if ok, err := s.AdvanceTaskCAS(run.ID, taskID, st[0], st[1], 1000); err != nil || !ok {
			t.Fatalf("%s → %s: ok=%v err=%v", st[0], st[1], ok, err)
		}
	}
	if ok, err := s.ClaimTaskSpawnCAS(run.ID, taskID, "", BranchName(run.Slug, taskID), "", "", 8, 1000); err != nil || !ok {
		t.Fatalf("ClaimTaskSpawnCAS: ok=%v err=%v", ok, err)
	}
	if ok, err := s.BindTaskSessionCAS(run.ID, taskID, session, 1000); err != nil || !ok {
		t.Fatalf("BindTaskSessionCAS: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AdvanceTaskCAS(run.ID, taskID, string(StateRunning), string(StateBlocked), 1000); err != nil || !ok {
		t.Fatalf("park: ok=%v err=%v", ok, err)
	}
}

// waitForFile blocks until path exists. Used only to sequence a goroutine
// against a subprocess; every assertion in this file is about state, never about
// timing.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
