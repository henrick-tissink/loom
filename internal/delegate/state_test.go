package delegate

import (
	"testing"
)

// The state predicates are tiny and have no callers inside the package yet
// (§9.3's deadlock detector is deferred), which is exactly why they are pinned
// here: a predicate with no caller and no test is a predicate that will be wrong
// on the day someone finally calls it. HoldsAChild returning false for
// everything — its skeleton value — is a cap that never caps.

func TestTaskStateHoldsAChild(t *testing.T) {
	tests := []struct {
		state TaskState
		want  bool
	}{
		{StatePending, false},
		{StateReady, false},
		{StateApproved, false},
		// The launch is in flight and the session row is written AFTER the
		// process exists (§13.3), so a spawning task may already be a claude.
		{StateSpawning, true},
		{StateRunning, true},
		// §6.6, BINDING: "a blocked child holds its worktree and its context".
		// Not counting it is how the cap is exceeded by every parked child.
		{StateBlocked, true},
		{StateChecking, true},
		{StateVerified, false},
		{StateFailed, false},
		{StateMerged, false},
		{StateAbandoned, false},
		{TaskState("nonsense-from-a-future-loom"), false},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := tc.state.HoldsAChild(); got != tc.want {
				t.Errorf("HoldsAChild(%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// The cap is enforced by ActiveChildren's own switch, which duplicates
// HoldsAChild's set deliberately (see both comments). Duplication is only safe
// if something notices when the two drift, so this is that something.
func TestActiveChildrenAgreesWithHoldsAChild(t *testing.T) {
	// Every declared TaskState must appear here. A state missing from this list
	// does not fail the test — it silently stops being compared, which is how
	// `integrating` and `mergeable` were added to HoldsAChild while
	// ActiveChildren's switch went on ignoring them and this test stayed green.
	// The length assertion below is what turns that silence into a failure.
	all := []TaskState{
		StatePending, StateReady, StateApproved, StateSpawning, StateRunning,
		StateBlocked, StateChecking, StateVerified, StateFailed,
		StateIntegrating, StateMergeable, StateMerged, StateAbandoned,
	}
	if len(all) != numTaskStates {
		t.Fatalf("all covers %d states but %d are declared; add the new state here", len(all), numTaskStates)
	}
	for _, st := range all {
		want := 0
		if st.HoldsAChild() {
			want = 1
		}
		if got := ActiveChildren(map[string]TaskState{"t": st}); got != want {
			t.Errorf("ActiveChildren(%q) = %d but HoldsAChild = %v", st, got, st.HoldsAChild())
		}
	}
	// And it counts, rather than merely detects. `integrating` and `mergeable`
	// are in the mix because §6.6's cap is what they threaten: a queue of
	// mergeable tasks waiting on a human must keep consuming slots.
	n := ActiveChildren(map[string]TaskState{
		"a": StateRunning, "b": StateBlocked, "c": StateReady, "d": StateChecking,
		"e": StateIntegrating, "f": StateMergeable, "g": StateVerified,
	})
	if n != 5 {
		t.Errorf("ActiveChildren = %d, want 5", n)
	}
}

func TestTaskStateTerminal(t *testing.T) {
	tests := []struct {
		state TaskState
		want  bool
	}{
		{StatePending, false},
		{StateReady, false},
		{StateApproved, false},
		{StateSpawning, false},
		{StateRunning, false},
		{StateBlocked, false},
		{StateChecking, false},
		// 3a does not merge (§10 deferred), so the successor of `verified` is an
		// act a human performs in their own tree.
		{StateVerified, true},
		{StateFailed, true},
		{StateMerged, true},
		{StateAbandoned, true},
		{TaskState(""), false},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := tc.state.Terminal(); got != tc.want {
				t.Errorf("Terminal(%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestDecodeFlags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Flag
	}{
		{"empty column", "", nil},
		{"whitespace", "  \n", nil},
		{"empty array", "[]", nil},
		{"one", `["orphaned"]`, []Flag{FlagOrphaned}},
		{"several", `["diverged","env-suspect"]`, []Flag{FlagDiverged, FlagEnvSuspect}},
		// A flag this Loom has never heard of must survive being read. Filtering
		// to the known set is how a two-instance setup silently downgrades rows
		// a newer Loom wrote (§13.2 lists four flags that belong to §§10-12).
		{"unknown flag is kept", `["stale-contract"]`, []Flag{Flag("stale-contract")}},
		// Degrade, never block: a corrupt flag column costs a badge, not a run.
		{"malformed json", `{"orphaned":true}`, nil},
		{"not an array of strings", `[1,2]`, nil},
		{"truncated", `["orph`, nil},
		{"empty element dropped", `["",  "orphaned"]`, []Flag{FlagOrphaned}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecodeFlags(tc.in)
			if got == nil {
				t.Fatalf("DecodeFlags(%q) returned a nil map; With on it would be a panic waiting", tc.in)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("DecodeFlags(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for _, f := range tc.want {
				if !got[f] {
					t.Errorf("DecodeFlags(%q) missing %q", tc.in, f)
				}
			}
		})
	}
}

func TestEncodeFlags(t *testing.T) {
	tests := []struct {
		name string
		in   Flags
		want string
	}{
		// The EMPTY STRING and not "[]": it is the migration's column default,
		// so an untouched row and a cleared row are byte-identical.
		{"nil", nil, ""},
		{"empty", Flags{}, ""},
		{"all false is empty", Flags{FlagOrphaned: false}, ""},
		{"one", Flags{FlagOrphaned: true}, `["orphaned"]`},
		// Sorted: map order is randomized per run, and an unsorted encoding
		// would have two Looms rewriting the same column back and forth.
		{"sorted", Flags{FlagOrphaned: true, FlagDiverged: true, FlagEnvSuspect: true},
			`["diverged","env-suspect","orphaned"]`},
		{"unknown flag round-trips", Flags{Flag("forced"): true}, `["forced"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EncodeFlags(tc.in); got != tc.want {
				t.Errorf("EncodeFlags(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFlagsRoundTrip(t *testing.T) {
	// The property that matters for a two-Loom setup: a row written by a Loom
	// that knows about `stale-contract` survives a read/modify/write by one that
	// does not.
	const stored = `["orphaned","stale-contract"]`
	f := DecodeFlags(stored).With(FlagDiverged).Without(FlagOrphaned)
	if got, want := EncodeFlags(f), `["diverged","stale-contract"]`; got != want {
		t.Errorf("round trip = %q, want %q", got, want)
	}
	if got := EncodeFlags(DecodeFlags(stored)); got != stored {
		t.Errorf("identity round trip = %q, want %q", got, stored)
	}
}

func TestFlagsWithWithoutCopy(t *testing.T) {
	// Copies, not mutation: a Flags read off a task row is shared with whatever
	// rendered it, and a renderer that watched its own set change under it is a
	// bug that only shows up under a redraw.
	base := DecodeFlags(`["orphaned"]`)
	added := base.With(FlagDiverged)
	removed := base.Without(FlagOrphaned)
	if base[FlagDiverged] {
		t.Error("With mutated the receiver")
	}
	if !base[FlagOrphaned] {
		t.Error("Without mutated the receiver")
	}
	if !added[FlagOrphaned] || !added[FlagDiverged] {
		t.Errorf("With dropped an existing flag: %v", added)
	}
	if len(removed) != 0 {
		t.Errorf("Without = %v, want empty", removed)
	}
	// A zero Flags is what a caller gets from a struct it never populated;
	// assigning into a nil map panics, so With must not be handing one out.
	var zero Flags
	if got := EncodeFlags(zero.With(FlagOrphaned)); got != `["orphaned"]` {
		t.Errorf("With on a zero Flags = %q", got)
	}
}
