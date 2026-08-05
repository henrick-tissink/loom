package delegate

import (
	"reflect"
	"testing"
)

// TestNextActions pins the corrected auto-runner policy: on top of what Runner.
// Tick already drives (check, integrate), the auto-runner only SPAWNS ready/
// approved tasks, MERGES mergeable ones, and ESCALATES a stuck run.
func TestNextActions(t *testing.T) {
	cases := []struct {
		name  string
		tasks []TaskView
		cap   int
		want  []AutoAction
	}{
		{"two ready under cap spawn both",
			[]TaskView{{ID: "a", State: StateReady}, {ID: "b", State: StateReady}}, 2,
			[]AutoAction{SpawnTask{"a"}, SpawnTask{"b"}}},
		{"cap full: two running plus one ready spawns nothing",
			[]TaskView{{ID: "a", State: StateRunning}, {ID: "b", State: StateRunning}, {ID: "c", State: StateReady}}, 2,
			nil},
		{"approved (partial spawn) is retried via the same spawn path",
			[]TaskView{{ID: "a", State: StateApproved}}, 2,
			[]AutoAction{SpawnTask{"a"}}},
		{"mergeable -> merge (the one step Tick leaves to the auto-runner)",
			[]TaskView{{ID: "a", State: StateMergeable}}, 2,
			[]AutoAction{MergeTask{"a"}}},
		{"verified -> nothing (Tick integrates it next iteration)",
			[]TaskView{{ID: "a", State: StateVerified}}, 2,
			nil},
		{"running with a green history but still working -> nothing (Tick checks)",
			[]TaskView{{ID: "a", State: StateRunning, ChildIdle: false, CommittedAt: 100}}, 2,
			nil},
		{"red check, child idle, no new commit -> escalate",
			[]TaskView{{ID: "a", State: StateRunning, ChildIdle: true, CheckStatus: "fail", CheckedAt: 200, CommittedAt: 100}}, 2,
			[]AutoAction{Escalate{Reason: "check failed for a (fail) and its child is idle with no new work"}}},
		{"red check but child committed a fix since -> nothing (Tick will re-check)",
			[]TaskView{{ID: "a", State: StateRunning, ChildIdle: true, CheckStatus: "fail", CheckedAt: 200, CommittedAt: 300}}, 2,
			nil},
		{"red check but child still working -> nothing (not idle yet)",
			[]TaskView{{ID: "a", State: StateRunning, ChildIdle: false, CheckStatus: "fail", CheckedAt: 200, CommittedAt: 100}}, 2,
			nil},
		{"all merged -> done",
			[]TaskView{{ID: "a", State: StateMerged}, {ID: "b", State: StateMerged}}, 2,
			[]AutoAction{Done{}}},
		{"all remaining blocked -> deadlock escalate",
			[]TaskView{{ID: "a", State: StateBlocked}, {ID: "b", State: StateMerged}}, 2,
			[]AutoAction{Escalate{Reason: "deadlock: all remaining tasks are blocked"}}},
		{"one blocked but another verified in flight -> not a deadlock",
			[]TaskView{{ID: "a", State: StateBlocked}, {ID: "b", State: StateVerified}}, 2,
			nil},
		{"mergeable merges and ready spawns in the same tick",
			[]TaskView{{ID: "a", State: StateMergeable}, {ID: "b", State: StateReady}}, 2,
			[]AutoAction{MergeTask{"a"}, SpawnTask{"b"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NextActions(c.tasks, c.cap); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("NextActions = %#v, want %#v", got, c.want)
			}
		})
	}
}
