package delegate

import (
	"context"
	"testing"
	"time"
)

// fakeRun is an in-memory stand-in for the store+status+git world the loop
// projects. It advances one task through its lifecycle in response to the
// executor calls, so a whole autonomous run can be driven with no I/O.
type fakeRun struct {
	state   RunState
	views   map[string]*TaskView
	spawned []string
	checked []string
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

func (f *fakeRun) runner(t *testing.T) *AutoRunner {
	t.Helper()
	return &AutoRunner{
		RunID: 1, Cap: 2, Interval: time.Millisecond,
		State:            func() (RunState, error) { return f.state, nil },
		Advance:          func(_, to RunState) (bool, error) { f.state = to; return true, nil },
		RecordEscalation: func(r string) error { f.esc = r; return nil },
		Project:          f.project,
		// Spawn: the child starts working (not idle). A later tick marks it done.
		Spawn: func(id string) error {
			f.spawned = append(f.spawned, id)
			v := f.views[id]
			v.State = StateRunning
			v.ChildIdle = true // the fake child finishes immediately
			v.CommittedAt = 100
			return nil
		},
		// Check: a passing check verifies the task.
		Check: func(id string) error {
			f.checked = append(f.checked, id)
			f.views[id].State = StateVerified
			return nil
		},
		// Integrate: green gates, full-auto-merge.
		Integrate: func(id string) error {
			f.merged = append(f.merged, id)
			f.views[id].State = StateMerged
			return nil
		},
		// No real sleeping in tests.
		Sleep: func(ctx context.Context, _ time.Duration) {},
	}
}

// TestAutoRunnerDrivesRunToDone is the whole loop: two ready tasks are spawned,
// checked when their children finish, integrated, and the run reaches Done — no
// human action, no I/O.
func TestAutoRunnerDrivesRunToDone(t *testing.T) {
	f := newFakeRun("a", "b")
	ar := f.runner(t)

	if err := ar.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.state != RunStateDone {
		t.Fatalf("final state = %q, want done", f.state)
	}
	if len(f.spawned) != 2 || len(f.checked) != 2 || len(f.merged) != 2 {
		t.Fatalf("spawned/checked/merged = %v/%v/%v, want 2/2/2", f.spawned, f.checked, f.merged)
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
	if len(f.checked) != 0 || len(f.merged) != 0 {
		t.Fatalf("acted past the abort: checked=%v merged=%v", f.checked, f.merged)
	}
}

// TestAutoRunnerEscalatesAtLapseCeiling: a task stuck at the retry ceiling
// escalates with a reason and stops the run.
func TestAutoRunnerEscalatesAtLapseCeiling(t *testing.T) {
	f := newFakeRun("a")
	f.views["a"].State = StateRunning
	f.views["a"].Lapses = MaxCheckRetries
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

// TestAutoRunnerHonoursContextCancel: a cancelled context stops the loop even
// while the run is still live.
func TestAutoRunnerHonoursContextCancel(t *testing.T) {
	f := newFakeRun("a")
	ar := f.runner(t)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the moment the loop sleeps, so it exits at the top of the next round.
	ar.Sleep = func(_ context.Context, _ time.Duration) { cancel() }

	if err := ar.Run(ctx); err != context.Canceled {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
}
