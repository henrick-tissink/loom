package delegate

import (
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
)

// §12.1–12.2's tests. The one property every case here defends is that NOTHING
// IN THIS FILE KILLS ANYTHING: the pure pass is asserted to emit only the four
// enumerated actions, and the store-backed half is asserted against a REAL live
// process that is still breathing afterwards.

var epoch = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return epoch.Add(-d) }

func TestWatchdogTable(t *testing.T) {
	tests := []struct {
		name    string
		obs     Observation
		budget  Budget
		want    []Finding // TaskID/Kind/Action/Flag only; Detail is asserted separately
		wantSub string    // a substring the detail must carry
	}{
		{
			name: "running, unmoved branch and silent transcript for 20m: stalled",
			obs: Observation{
				TaskID: "schema", State: StateRunning,
				BranchHead: "abc", LastBranchHead: "abc",
				TranscriptAt: ago(34 * time.Minute), TranscriptState: transcript.StateRunning,
			},
			want:    []Finding{{TaskID: "schema", Kind: WatchNoProgress, Action: ActionFlag, Flag: FlagStalled}},
			wantSub: "34m",
		},
		{
			name: "a child that committed is not stalled, however quiet",
			obs: Observation{
				TaskID: "schema", State: StateRunning,
				BranchHead: "def", LastBranchHead: "abc",
				TranscriptAt: ago(3 * time.Hour),
			},
		},
		{
			name: "a child whose transcript is advancing is not stalled, however still the branch",
			obs: Observation{
				TaskID: "schema", State: StateRunning,
				BranchHead: "abc", LastBranchHead: "abc",
				TranscriptAt: ago(19 * time.Minute),
			},
		},
		{
			name: "no transcript observed at all measures from the state entry",
			obs: Observation{
				TaskID: "schema", State: StateRunning, Since: ago(40 * time.Minute),
				BranchHead: "abc", LastBranchHead: "abc",
			},
			want:    []Finding{{TaskID: "schema", Kind: WatchNoProgress, Action: ActionFlag, Flag: FlagStalled}},
			wantSub: "40m",
		},
		{
			name: "already flagged: a watchdog that re-fires every tick gets muted",
			obs: Observation{
				TaskID: "schema", State: StateRunning,
				BranchHead: "abc", LastBranchHead: "abc",
				TranscriptAt: ago(time.Hour),
				Flags:        Flags{FlagStalled: true},
			},
		},
		{
			name: "blocked with an undelivered seed past 5m offers a retry",
			obs: Observation{
				TaskID: "api", State: StateBlocked,
				PendingSeed: "account-schema is present at db/0007.sql. Continue.",
				UnblockedAt: ago(9 * time.Minute),
			},
			want:    []Finding{{TaskID: "api", Kind: WatchBlockStale, Action: ActionOfferRetry}},
			wantSub: "retry is offered, never automatic",
		},
		{
			name: "the retry affordance keeps being offered once the flag is on",
			obs: Observation{
				TaskID: "api", State: StateBlocked,
				PendingSeed: "seed", UnblockedAt: ago(9 * time.Minute),
				Flags: Flags{FlagSeedPending: true},
			},
			want: []Finding{{TaskID: "api", Kind: WatchBlockStale, Action: ActionOfferRetry}},
		},
		{
			name: "blocked with nothing owed is just blocked",
			obs:  Observation{TaskID: "api", State: StateBlocked, UnblockedAt: ago(time.Hour)},
		},
		{
			name: "a seed owed but not yet unblockable is not stale",
			obs:  Observation{TaskID: "api", State: StateBlocked, PendingSeed: "seed"},
		},
		{
			name: "spawning past 60s resolves by cwd",
			obs:  Observation{TaskID: "web", State: StateSpawning, Since: ago(90 * time.Second)},
			want: []Finding{{TaskID: "web", Kind: WatchSpawnOrphan, Action: ActionResolve}},
			// The rule this sentence exists to defend: revision 1's tag-based
			// recovery would have double-spawned into a live worktree.
			wantSub: "never by tag",
		},
		{
			name: "spawning inside the window is just spawning",
			obs:  Observation{TaskID: "web", State: StateSpawning, Since: ago(30 * time.Second)},
		},
		{
			name: "an adjudicated orphan is not re-resolved forever",
			obs: Observation{
				TaskID: "web", State: StateSpawning, Since: ago(time.Hour),
				Flags: Flags{FlagOrphaned: true},
			},
		},
		{
			name:    "the child budget stops new spawns and touches nothing",
			obs:     Observation{TaskID: "schema", State: StateRunning, BranchHead: "a", LastBranchHead: "a", TranscriptAt: ago(time.Minute)},
			budget:  Budget{MaxChildren: 4, Spawned: 4},
			want:    []Finding{{Kind: WatchRunBudget, Action: ActionStopSpawns}},
			wantSub: "nothing running is touched",
		},
		{
			name:    "the wall-clock budget stops new spawns and touches nothing",
			obs:     Observation{TaskID: "schema", State: StateVerified},
			budget:  Budget{MaxWall: time.Hour, StartedAt: ago(3 * time.Hour)},
			want:    []Finding{{Kind: WatchRunBudget, Action: ActionStopSpawns}},
			wantSub: "nothing running is touched",
		},
		{
			// Zero fields mean unlimited: Loom does not know what a run should
			// cost, and a default cap would be a number trusted for no reason.
			name:   "a zero budget is unlimited",
			obs:    Observation{TaskID: "schema", State: StateVerified},
			budget: Budget{Spawned: 99, StartedAt: ago(100 * time.Hour)},
		},
		{
			name: "terminal states are not watched",
			obs:  Observation{TaskID: "schema", State: StateMerged, Since: ago(100 * time.Hour)},
		},

		// §6.3, the row that fires at more than one state. Nothing is killed and
		// nothing advances: the flag is the whole action.
		{
			name:    "a dead child while running is flagged, not killed",
			obs:     Observation{TaskID: "schema", State: StateRunning, Child: LivenessDead},
			want:    []Finding{{TaskID: "schema", Kind: WatchChildGone, Action: ActionFlag, Flag: FlagOrphaned}},
			wantSub: "NOTHING was killed",
		},
		{
			name: "a dead child while blocked is flagged too",
			obs:  Observation{TaskID: "api", State: StateBlocked, Child: LivenessDead},
			want: []Finding{{TaskID: "api", Kind: WatchChildGone, Action: ActionFlag, Flag: FlagOrphaned}},
		},
		{
			// Loom is running the check itself and the verdict is minutes away.
			name: "a dead child while checking is not flagged",
			obs:  Observation{TaskID: "schema", State: StateChecking, Child: LivenessDead},
		},
		{
			name: "a dead child after the work is verified is not flagged",
			obs:  Observation{TaskID: "schema", State: StateVerified, Child: LivenessDead},
		},
		{
			// The reason Liveness is three-valued: the zero value is "I could not
			// tell", and a failed sessions read must not badge a live child.
			name: "unknown liveness is evidence of nothing",
			obs:  Observation{TaskID: "schema", State: StateRunning, Child: LivenessUnknown},
		},
		{
			name: "an orphan badge already on is not re-reported",
			obs: Observation{
				TaskID: "schema", State: StateRunning, Child: LivenessDead,
				Flags: Flags{FlagOrphaned: true},
			},
		},
		{
			// A badge that survives a successful re-spawn sends the human looking
			// for a dead child that is sitting there working.
			name: "a live child again clears the badge",
			obs: Observation{
				TaskID: "schema", State: StateRunning, Child: LivenessAlive,
				Flags: Flags{FlagOrphaned: true},
			},
			want:    []Finding{{TaskID: "schema", Kind: WatchChildGone, Action: ActionUnflag, Flag: FlagOrphaned}},
			wantSub: "cleared",
		},
		{
			name: "a live child with no badge is silent",
			obs:  Observation{TaskID: "schema", State: StateRunning, Child: LivenessAlive},
		},
		{
			// Both facts, both remedies: a stalled child that has also died must
			// not have one finding swallow the other.
			name: "a dead child that was also stalled reports both",
			obs: Observation{
				TaskID: "schema", State: StateRunning, Child: LivenessDead,
				BranchHead: "abc", LastBranchHead: "abc", TranscriptAt: ago(time.Hour),
			},
			want: []Finding{
				{TaskID: "schema", Kind: WatchChildGone, Action: ActionFlag, Flag: FlagOrphaned},
				{TaskID: "schema", Kind: WatchNoProgress, Action: ActionFlag, Flag: FlagStalled},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Watch(epoch, []Observation{tc.obs}, tc.budget)
			if len(got) != len(tc.want) {
				t.Fatalf("Watch = %+v, want %d finding(s)", got, len(tc.want))
			}
			for i, w := range tc.want {
				g := got[i]
				if g.TaskID != w.TaskID || g.Kind != w.Kind || g.Action != w.Action || g.Flag != w.Flag {
					t.Fatalf("finding %d = %+v, want %+v", i, g, w)
				}
				if g.Detail == "" {
					t.Fatal("a watchdog that says only its name makes the human go and find out what it meant")
				}
				if !g.At.Equal(epoch) {
					t.Fatalf("At = %v, want the passed clock %v", g.At, epoch)
				}
			}
			if tc.wantSub != "" && !strings.Contains(got[0].Detail, tc.wantSub) {
				t.Fatalf("detail %q must carry %q", got[0].Detail, tc.wantSub)
			}
		})
	}
}

// TestWatchdogNeverKills is the BINDING property of §12.2, asserted structurally
// rather than trusted: over every observation shape the table can produce, the
// only actions Watch may ever emit are the enumerated ones, and none of them is
// a kill.
//
// The allowlist is written out here rather than derived from the const block on
// purpose — deriving it would make the test agree with whatever anyone adds. A
// new action has to be added HERE, by a hand that has read this comment and can
// say why it is not a kill. ActionRecover is: it releases a row a dead process
// left claimed and resets a staging worktree Loom itself owns; it touches no
// session, no child worktree and no branch of the child's.
func TestWatchdogNeverKills(t *testing.T) {
	allowed := map[WatchdogAction]bool{
		ActionFlag: true, ActionUnflag: true, ActionOfferRetry: true,
		ActionResolve: true, ActionStopSpawns: true, ActionRecover: true,
	}
	var obs []Observation
	for _, st := range []TaskState{
		StatePending, StateReady, StateApproved, StateSpawning, StateRunning, StateBlocked,
		StateChecking, StateVerified, StateFailed, StateIntegrating, StateMergeable,
		StateMerged, StateAbandoned,
	} {
		// Every liveness value at every state, because §6.3's row is the one that
		// writes and CLEARS a flag and the worst case must include both limbs.
		for _, live := range []Liveness{LivenessUnknown, LivenessAlive, LivenessDead} {
			obs = append(obs, Observation{
				TaskID: string(st), State: st, Since: ago(48 * time.Hour),
				BranchHead: "same", LastBranchHead: "same",
				TranscriptAt: ago(48 * time.Hour),
				PendingSeed:  "owed", UnblockedAt: ago(48 * time.Hour),
				Child: live,
				Flags: Flags{FlagOrphaned: true},
			})
		}
	}
	got := Watch(epoch, obs, Budget{MaxChildren: 1, Spawned: 9, MaxWall: time.Minute, StartedAt: ago(time.Hour)})
	if len(got) == 0 {
		t.Fatal("the worst-case observation produced no findings at all")
	}
	for _, f := range got {
		if !allowed[f.Action] {
			t.Fatalf("finding %+v carries an action outside the enumerated four — there is no kill", f)
		}
	}
}

// TestWatchdogDeadlockShapes: the shapes are distinguished because the REMEDIES
// differ, so each one is asserted to carry the evidence its remedy needs.
func TestWatchdogDeadlockShapes(t *testing.T) {
	cyclic := Graph{
		TaskIDs:  []string{"a", "b"},
		Needs:    map[string][]string{"a": {"y"}, "b": {"x"}},
		Producer: map[string]string{"x": "a", "y": "b"},
		Edges: []Edge{
			{From: "a", To: "b", Artifact: "x"},
			{From: "b", To: "a", Artifact: "y"},
		},
	}
	acyclic := Graph{TaskIDs: []string{"a", "b"}}
	states := map[string]TaskState{"a": StateBlocked, "b": StateBlocked}

	t.Run("progressing runs are not diagnosed", func(t *testing.T) {
		if d := DetectDeadlock(EffectiveGraph{declared: acyclic}, Progress{Deadlocked: false}, states); d != nil {
			t.Fatalf("DetectDeadlock on a live run = %+v, want nil", d)
		}
	})

	t.Run("a cycle wins over the owed list", func(t *testing.T) {
		e := EffectiveGraph{declared: cyclic, Blocks: map[string]Block{
			"a": {Kind: BlockNeedsDecision, Summary: "pick one", At: ago(time.Hour)},
		}}
		d := DetectDeadlock(e, Progress{Deadlocked: true}, states)
		if d == nil || d.Shape != ShapeMutualWait {
			t.Fatalf("want mutual-wait, got %+v", d)
		}
		if len(d.Cycle) == 0 {
			t.Fatal("mutual wait must name the actual wait-for cycle")
		}
		if strings.Join(d.Stuck, ",") != "a,b" {
			t.Fatalf("Stuck = %v, want every non-terminal task", d.Stuck)
		}
	})

	t.Run("live blocks become an owed list, oldest first", func(t *testing.T) {
		e := EffectiveGraph{declared: acyclic, Blocks: map[string]Block{
			"b": {Kind: BlockExternal, Summary: "vault is down", At: ago(time.Hour)},
			"a": {Kind: BlockNeedsScope, Summary: "needs internal/auth", At: ago(3 * time.Hour)},
		}}
		d := DetectDeadlock(e, Progress{Deadlocked: true}, states)
		if d == nil || d.Shape != ShapeExternal {
			t.Fatalf("want external, got %+v", d)
		}
		if len(d.Owed) != 2 || d.Owed[0].TaskID != "a" || d.Owed[1].TaskID != "b" {
			t.Fatalf("Owed = %+v, want the oldest decision first", d.Owed)
		}
		if d.Owed[0].Kind != BlockNeedsScope || d.Owed[0].Summary != "needs internal/auth" {
			t.Fatalf("an owed decision must be actionable, got %+v", d.Owed[0])
		}
	})

	t.Run("a needs-artifact block in a stopped run is also owed", func(t *testing.T) {
		// The commonest deadlock in the design: the predicate says nothing is
		// ready and nothing is in flight, so no task can ever publish it. It is a
		// re-plan owed to a human, not a fault in Loom.
		e := EffectiveGraph{declared: acyclic, Blocks: map[string]Block{
			"a": {Kind: BlockNeedsArtifact, Summary: "needs account-schema", At: ago(time.Minute)},
		}}
		d := DetectDeadlock(e, Progress{Deadlocked: true}, states)
		if d == nil || d.Shape != ShapeExternal || len(d.Owed) != 1 {
			t.Fatalf("want one owed external decision, got %+v", d)
		}
	})

	// §12.1(a)'s graph is "declared edges + accepted amendments + LIVE BLOCKS",
	// and this is the case that separates it from Merged(): §11.1's commonest
	// unforeseen dependency names an artifact no manifest declares, so two
	// children each parked on an artifact the other was going to write is a
	// mutual wait with zero declared edges. Diagnosed over Merged() alone it
	// reads as ShapeExternal — "two decisions are owed" — and a human who grants
	// both finds the loop still closed.
	t.Run("a mutual wait made only of live blocks is a cycle, not an owed list", func(t *testing.T) {
		e := EffectiveGraph{
			declared: Graph{TaskIDs: []string{"a", "b"}},
			Blocks: map[string]Block{
				"a": {Task: "a", Kind: BlockNeedsArtifact, Summary: "needs y",
					Need: BlockNeed{Artifact: "y", From: "b"}, At: ago(time.Hour)},
				"b": {Task: "b", Kind: BlockNeedsArtifact, Summary: "needs x",
					Need: BlockNeed{Artifact: "x", From: "a"}, At: ago(time.Hour)},
			},
		}
		d := DetectDeadlock(e, Progress{Deadlocked: true}, states)
		if d == nil || d.Shape != ShapeMutualWait {
			t.Fatalf("want mutual-wait from block edges alone, got %+v", d)
		}
		if len(d.Cycle) == 0 {
			t.Fatal("mutual wait must name the actual wait-for cycle, however it was formed")
		}
	})

	t.Run("nothing ready, nothing running, nothing blocked is a fault in Loom", func(t *testing.T) {
		d := DetectDeadlock(EffectiveGraph{declared: acyclic}, Progress{Deadlocked: true}, states)
		if d == nil || d.Shape != ShapeStarved {
			t.Fatalf("want starved, got %+v", d)
		}
	})

	t.Run("terminal tasks are not stuck", func(t *testing.T) {
		d := DetectDeadlock(EffectiveGraph{declared: acyclic}, Progress{Deadlocked: true},
			map[string]TaskState{"a": StateMerged, "b": StateFailed, "c": StateRunning})
		if strings.Join(d.Stuck, ",") != "c" {
			t.Fatalf("Stuck = %v, want only the non-terminal task", d.Stuck)
		}
	})
}

// --- the store-backed half -------------------------------------------------

func newWatchdogFixture(t *testing.T) (*Watchdogs, *store.Store, store.DelegationRun) {
	t.Helper()
	s := newTestStore(t)
	run, err := s.InsertDelegationRun("atlas", t.TempDir(), "{}", "{}", epoch.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	w := &Watchdogs{
		Store:     s,
		Worktrees: &Worktrees{Layout: NewLayout(t.TempDir()), Store: s},
		Now:       func() time.Time { return epoch },
	}
	w.Layout = w.Worktrees.Layout
	return w, s, run
}

func insertSpawningTask(t *testing.T, s *store.Store, runID int64, taskID, worktree, branch, base string) {
	t.Helper()
	err := s.InsertDelegationTask(store.DelegationTask{
		RunID: runID, TaskID: taskID, State: string(StateSpawning), RepoLabel: "bankenstein",
		Worktree: worktree, Branch: branch, BaseSHA: base, UpdatedAt: epoch.UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestWatchdogSpawnOrphanAdoptsAndDoesNotKill is the "demonstrably does not
// kill" case, and it is deliberately built on a REAL process: a fake row would
// let a future implementation call something fatal and still pass.
func TestWatchdogSpawnOrphanAdoptsAndDoesNotKill(t *testing.T) {
	w, s, run := newWatchdogFixture(t)
	dir := w.Layout.Dir(run.Slug, "bankenstein", "schema")
	insertSpawningTask(t, s, run.ID, "schema", dir, "", "")

	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })
	occupyWorktree(t, s, "loom-child", dir)

	outcome, err := w.ResolveSpawnOrphan(run, "schema", "bankenstein")
	if err != nil {
		t.Fatalf("ResolveSpawnOrphan: %v", err)
	}
	if outcome != OutcomeAdopted {
		t.Fatalf("outcome = %q, want %q — a live row at the worktree cwd is the child", outcome, OutcomeAdopted)
	}

	// The process is untouched. This is the whole rule.
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the watchdog killed the child: %v", err)
	}
	live, err := s.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Name != "loom-child" {
		t.Fatalf("the session row must still be live: %+v", live)
	}
	if !strings.Contains(live[0].Tags, DelegationTag(run.Slug, "schema")) {
		t.Fatalf("adoption must re-apply the dlg: tag, got %q", live[0].Tags)
	}

	task, _, err := s.GetDelegationTask(run.ID, "schema")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != string(StateRunning) || task.SessionName != "loom-child" {
		t.Fatalf("adoption must complete the CAS to running with the session name: %+v", task)
	}
}

// TestWatchdogSpawnOrphanReapprovesOnlyWhenNothingWasDone: the no-commits case
// hands the decision back; the with-commits case preserves the work. Revision
// 1's rule got this backwards and would have put a second claude in one worktree.
func TestWatchdogSpawnOrphanReapprovesOnlyWhenNothingWasDone(t *testing.T) {
	t.Run("no session, no commits: back to approved", func(t *testing.T) {
		w, s, run := newWatchdogFixture(t)
		repo := newScratchRepo(t)
		base := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD"))
		dir := filepath.Join(t.TempDir(), "wt")
		branch := BranchName(run.Slug, "schema")
		mustGit(t, repo, "worktree", "add", "-q", "-b", branch, dir, base)
		insertSpawningTask(t, s, run.ID, "schema", dir, branch, base)

		outcome, err := w.ResolveSpawnOrphan(run, "schema", "bankenstein")
		if err != nil {
			t.Fatalf("ResolveSpawnOrphan: %v", err)
		}
		if outcome != OutcomeReapproved {
			t.Fatalf("outcome = %q, want %q", outcome, OutcomeReapproved)
		}
		task, _, _ := s.GetDelegationTask(run.ID, "schema")
		if task.State != string(StateApproved) {
			t.Fatalf("state = %q, want approved", task.State)
		}
	})

	t.Run("no session, commits present: orphaned and preserved", func(t *testing.T) {
		w, s, run := newWatchdogFixture(t)
		repo := newScratchRepo(t)
		base := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD"))
		dir := filepath.Join(t.TempDir(), "wt")
		branch := BranchName(run.Slug, "schema")
		mustGit(t, repo, "worktree", "add", "-q", "-b", branch, dir, base)
		writeFile(t, filepath.Join(dir, "work.txt"), "an hour of irreplaceable context\n")
		mustGit(t, dir, "add", "work.txt")
		mustGit(t, dir, "commit", "-qm", "child work")
		insertSpawningTask(t, s, run.ID, "schema", dir, branch, base)

		outcome, err := w.ResolveSpawnOrphan(run, "schema", "bankenstein")
		if err != nil {
			t.Fatalf("ResolveSpawnOrphan: %v", err)
		}
		if outcome != OutcomeOrphaned {
			t.Fatalf("outcome = %q, want %q", outcome, OutcomeOrphaned)
		}
		task, _, _ := s.GetDelegationTask(run.ID, "schema")
		if !DecodeFlags(task.Flags)[FlagOrphaned] {
			t.Fatalf("the orphan must be FLAGGED, not silent: flags=%q", task.Flags)
		}
		if task.State == string(StateApproved) {
			t.Fatal("re-approving over committed work is how two claudes end up in one worktree")
		}
		// The work is not garbage: nothing removed the worktree or the branch.
		if _, err := gitOut(dir, "rev-parse", "--verify", branch); err != nil {
			t.Fatalf("the branch must survive: %v", err)
		}
	})

	t.Run("a task that moved is not clobbered", func(t *testing.T) {
		w, s, run := newWatchdogFixture(t)
		insertSpawningTask(t, s, run.ID, "schema", filepath.Join(t.TempDir(), "gone"), "", "")
		if _, err := s.AbandonTaskCAS(run.ID, "schema", epoch.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		if _, err := w.ResolveSpawnOrphan(run, "schema", "bankenstein"); err != ErrTaskMovedElsewhere {
			t.Fatalf("err = %v, want ErrTaskMovedElsewhere", err)
		}
	})
}

// TestWatchdogSweepAbandonReconcilesWithoutKilling: the disclosed residual of
// §13.3. A child stranded by a lost CAS is found by cwd — the one identity that
// exists the instant the child does — tagged so it is dashboard-visible, and
// left running.
func TestWatchdogSweepAbandonReconcilesWithoutKilling(t *testing.T) {
	w, s, run := newWatchdogFixture(t)
	m := Manifest{Tasks: []Task{
		{ID: "schema", Repo: "bankenstein"},
		{ID: "api", Repo: "bankenstein"},
	}}
	dir := w.Layout.Dir(run.Slug, "bankenstein", "api")
	occupyWorktree(t, s, "loom-stranded", dir)

	found, err := w.SweepAbandon(run, m)
	if err != nil {
		t.Fatalf("SweepAbandon: %v", err)
	}
	if len(found) != 1 || found[0].Name != "loom-stranded" {
		t.Fatalf("found = %+v, want the stranded child", found)
	}
	live, err := s.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].EndedAt != -1 {
		t.Fatalf("the sweep must RECONCILE, never end the session: %+v", live)
	}
	if !strings.Contains(live[0].Tags, DelegationTag(run.Slug, "api")) {
		t.Fatalf("the sweep must leave the child findable by its dlg: tag, got %q", live[0].Tags)
	}
}
