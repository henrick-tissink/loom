package delegate

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeRun is an in-memory stand-in for the store+status+git world the loop
// projects. Its `tick` models what Runner.Tick does for real — check a spawned
// child (green → verified → integrate → mergeable) — so the loop's own executors
// (Spawn, Merge) are exercised against a faithful pipeline with no I/O.
type fakeRun struct {
	state   RunState
	views   map[string]*TaskView
	spawned []string
	merged  []string
	esc     string
}

func newFakeRun(ids ...string) *fakeRun {
	f := &fakeRun{state: RunStateRunning, views: map[string]*TaskView{}}
	for _, id := range ids {
		f.views[id] = &TaskView{ID: id, State: StateReady}
	}
	return f
}

func (f *fakeRun) project() ([]TaskView, error) {
	out := make([]TaskView, 0, len(f.views))
	for _, v := range f.views {
		out = append(out, *v)
	}
	return out, nil
}

// tick models Runner.Tick: a running task whose child has committed and gone
// idle is checked green -> verified, then integrated straight to mergeable. This
// is the pipeline the real Tick drives, and the reason the loop's only executors
// are Spawn and Merge.
func (f *fakeRun) tick(context.Context) error {
	for _, v := range f.views {
		if v.State == StateRunning && v.ChildIdle && v.CommittedAt > v.CheckedAt {
			v.State = StateVerified
			v.CheckStatus = "pass"
			v.CheckedAt = v.CommittedAt
		}
		if v.State == StateVerified {
			v.State = StateMergeable
		}
	}
	return nil
}

func (f *fakeRun) runner(t *testing.T) *AutoRunner {
	t.Helper()
	return &AutoRunner{
		Cap: 2, Interval: time.Millisecond,
		State:            func() (RunState, error) { return f.state, nil },
		Advance:          func(_, to RunState) (bool, error) { f.state = to; return true, nil },
		RecordEscalation: func(r string) error { f.esc = r; return nil },
		Tick:             f.tick,
		Project:          f.project,
		// Spawn: the child starts, commits immediately, and goes idle. The next
		// tick (f.tick) checks and integrates it.
		Spawn: func(id string) error {
			f.spawned = append(f.spawned, id)
			v := f.views[id]
			v.State = StateRunning
			v.ChildIdle = true
			v.CommittedAt = 100
			return nil
		},
		// Merge: the auto-runner's one merge step -> merged.
		Merge: func(id string) error {
			f.merged = append(f.merged, id)
			f.views[id].State = StateMerged
			return nil
		},
		Sleep: func(ctx context.Context, _ time.Duration) {},
	}
}

// TestAutoRunnerDrivesRunToDone is the whole loop: two ready tasks are spawned,
// Tick checks+integrates them to mergeable, the runner merges, and the run
// reaches Done — no human action, no I/O.
func TestAutoRunnerDrivesRunToDone(t *testing.T) {
	f := newFakeRun("a", "b")
	ar := f.runner(t)

	if err := ar.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.state != RunStateDone {
		t.Fatalf("final state = %q, want done", f.state)
	}
	if len(f.spawned) != 2 || len(f.merged) != 2 {
		t.Fatalf("spawned/merged = %v/%v, want 2/2", f.spawned, f.merged)
	}
}

// TestAutoRunnerStopsOnAbort: an abort flips the control state, and the loop
// returns before its next round of actions.
func TestAutoRunnerStopsOnAbort(t *testing.T) {
	f := newFakeRun("a")
	ar := f.runner(t)
	// Abort fires as soon as the first spawn happens, from inside the Spawn seam.
	ar.Spawn = func(id string) error {
		f.spawned = append(f.spawned, id)
		f.state = RunStateAborted
		return nil
	}

	if err := ar.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.state != RunStateAborted {
		t.Fatalf("state = %q, want aborted", f.state)
	}
	if len(f.merged) != 0 {
		t.Fatalf("acted past the abort: merged=%v", f.merged)
	}
}

// TestAutoRunnerEscalatesOnFailedCheck: a red check on an idle child with no new
// commit escalates with a reason and stops the run.
func TestAutoRunnerEscalatesOnFailedCheck(t *testing.T) {
	f := newFakeRun("a")
	f.views["a"] = &TaskView{ID: "a", State: StateRunning, ChildIdle: true,
		CheckStatus: "fail", CheckedAt: 200, CommittedAt: 100}
	ar := f.runner(t)

	if err := ar.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.state != RunStateEscalated {
		t.Fatalf("state = %q, want escalated", f.state)
	}
	if f.esc == "" {
		t.Fatal("escalation recorded no reason")
	}
}

// TestAutoRunnerEscalatesOnRefusedMerge: a genuine (non-transient) merge refusal
// — a divergence a human must acknowledge — escalates the run rather than
// looping forever on the mergeable task.
func TestAutoRunnerEscalatesOnRefusedMerge(t *testing.T) {
	f := newFakeRun("a")
	f.views["a"] = &TaskView{ID: "a", State: StateMergeable}
	ar := f.runner(t)
	ar.Merge = func(id string) error { return errors.New("scope divergence needs acknowledgement") }

	if err := ar.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.state != RunStateEscalated {
		t.Fatalf("state = %q, want escalated after a refused merge", f.state)
	}
}

// TestAutoRunnerRetriesTransientMerge: a TRANSIENT merge error (the branch moved,
// another integration in flight) is retried next tick, NOT escalated — so a
// child pushing a fix while its task is mergeable does not permanently halt the
// run. The merge succeeds on the second attempt and the run completes.
func TestAutoRunnerRetriesTransientMerge(t *testing.T) {
	f := newFakeRun("a")
	f.views["a"] = &TaskView{ID: "a", State: StateMergeable}
	ar := f.runner(t)
	attempts := 0
	ar.Merge = func(id string) error {
		attempts++
		if attempts == 1 {
			return ErrBranchMoved // transient: the child pushed a commit
		}
		f.views[id].State = StateMerged
		return nil
	}

	if err := ar.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.state != RunStateDone {
		t.Fatalf("state = %q, want done — a transient merge error must retry, not escalate", f.state)
	}
	if attempts < 2 {
		t.Fatalf("merge attempted %d times, want a retry after the transient error", attempts)
	}
}

// TestAutoRunnerEscalatesOnPermanentSpawnFailure: a non-transient spawn failure
// escalates the run instead of retrying forever and silently holding a cap slot.
func TestAutoRunnerEscalatesOnPermanentSpawnFailure(t *testing.T) {
	f := newFakeRun("a")
	ar := f.runner(t)
	ar.Spawn = func(id string) error { return errors.New("worktree add failed: branch already exists") }

	if err := ar.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.state != RunStateEscalated {
		t.Fatalf("state = %q, want escalated after a permanent spawn failure", f.state)
	}
}

// TestAutoRunnerHonoursContextCancel: a cancelled context stops the loop even
// while the run is still live.
func TestAutoRunnerHonoursContextCancel(t *testing.T) {
	f := newFakeRun("a")
	ar := f.runner(t)
	// Cancel the moment the loop sleeps, so it exits at the top of the next round.
	ctx, cancel := context.WithCancel(context.Background())
	ar.Sleep = func(_ context.Context, _ time.Duration) { cancel() }

	if err := ar.Run(ctx); err != context.Canceled {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
}
