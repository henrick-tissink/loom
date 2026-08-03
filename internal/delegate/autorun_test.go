package delegate

import (
	"reflect"
	"testing"
)

// TestNextActions pins the auto-runner's entire decision policy, including the
// one genuinely new signal — child-done detection (running + child idle + a
// commit since the last check).
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
		{"child idle with a commit since last check -> check",
			[]TaskView{{ID: "a", State: StateRunning, ChildIdle: true, CommittedAt: 100, CheckedAt: 0}}, 2,
			[]AutoAction{CheckTask{"a"}}},
		{"child still working -> no check",
			[]TaskView{{ID: "a", State: StateRunning, ChildIdle: false, CommittedAt: 100}}, 2,
			nil},
		{"idle but nothing committed since the last check -> no check",
			[]TaskView{{ID: "a", State: StateRunning, ChildIdle: true, CommittedAt: 100, CheckedAt: 100}}, 2,
			nil},
		{"verified -> integrate",
			[]TaskView{{ID: "a", State: StateVerified}}, 2,
			[]AutoAction{IntegrateTask{"a"}}},
		{"all merged -> done",
			[]TaskView{{ID: "a", State: StateMerged}, {ID: "b", State: StateMerged}}, 2,
			[]AutoAction{Done{}}},
		{"lapses at the ceiling -> escalate, stopping the tick",
			[]TaskView{{ID: "a", State: StateRunning, ChildIdle: true, CommittedAt: 100, CheckedAt: 50, Lapses: MaxCheckRetries}}, 2,
			[]AutoAction{Escalate{Reason: "check failed 3x: a"}}},
		{"failed below the ceiling -> retry the check",
			[]TaskView{{ID: "a", State: StateFailed, Lapses: 1}}, 2,
			[]AutoAction{CheckTask{"a"}}},
		{"failed at the ceiling -> escalate, not retry",
			[]TaskView{{ID: "a", State: StateFailed, Lapses: MaxCheckRetries}}, 2,
			[]AutoAction{Escalate{Reason: "check failed 3x: a"}}},
		{"mergeable (auto-merge refused) -> escalate",
			[]TaskView{{ID: "a", State: StateMergeable}}, 2,
			[]AutoAction{Escalate{Reason: "auto-merge refused (divergence/drift/conflict): a"}}},
		{"all remaining blocked -> deadlock escalate",
			[]TaskView{{ID: "a", State: StateBlocked}}, 2,
			[]AutoAction{Escalate{Reason: "deadlock: all remaining tasks are blocked"}}},
		{"approved (partial spawn) -> retry spawn",
			[]TaskView{{ID: "a", State: StateApproved}}, 2,
			[]AutoAction{SpawnTask{"a"}}},
		{"verified integrates and ready spawns in the same tick",
			[]TaskView{{ID: "a", State: StateVerified}, {ID: "b", State: StateReady}}, 2,
			[]AutoAction{IntegrateTask{"a"}, SpawnTask{"b"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NextActions(c.tasks, c.cap); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("NextActions = %#v, want %#v", got, c.want)
			}
		})
	}
}
