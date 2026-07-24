package delegate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

// §9's scheduler and §12.1's predicate, over the EFFECTIVE graph. Every test in
// this file is a literal: no DB, no git, no processes. That is not a
// convenience, it is the property that makes the deadlock logic checkable at
// all — manifest_test.go's cycle table is the precedent.

// emanifest builds a manifest whose task `x` produces artifact `x`, the same
// convention manifest_test.go's graphOf uses, so a `needs` list reads as the
// producing task ids.
func emanifest(order []string, needs map[string][]string) Manifest {
	m := Manifest{Name: "atlas"}
	for _, id := range order {
		m.Tasks = append(m.Tasks, Task{
			ID: id, Repo: "r", Needs: needs[id], Produces: []Artifact{{ID: id}},
		})
	}
	return m
}

// approved is an accepted AmendEdge: consumer `to` gains a dependency on
// artifact `art`, produced by `from`.
func approved(seq int64, to, from, art string) Amendment {
	return Amendment{
		Seq: seq, Kind: AmendEdge, Task: to, From: from, Artifact: art,
		CreatedAt: time.Unix(1, 0), ApprovedAt: time.Unix(2, 0),
	}
}

// proposed is the same amendment before a human touched it.
func proposed(seq int64, to, from, art string) Amendment {
	a := approved(seq, to, from, art)
	a.ApprovedAt = time.Time{}
	return a
}

func artBlock(task, art, from string) Block {
	return Block{Version: BlockVersion, Task: task, Kind: BlockNeedsArtifact,
		Need: BlockNeed{Artifact: art, From: from}, Summary: "needs " + art}
}

// ---------------------------------------------------------------- Ready

// TestEffectiveReady is §9.1 over the three terms of the effective graph. The
// declared-only rows restate 3a's contract (this function must not have lost
// it); the amendment and block rows are what §§9-12 adds.
func TestEffectiveReady(t *testing.T) {
	cases := []struct {
		name       string
		m          Manifest
		amendments []Amendment
		blocks     map[string]Block
		states     map[string]TaskState
		published  map[string]bool
		want       string
	}{
		{
			name: "declared only: nothing done, only the task with no needs",
			m:    emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}}),
			want: "[a]",
		},
		{
			name:      "declared only: producer verified AND published",
			m:         emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}}),
			states:    map[string]TaskState{"a": StateVerified},
			published: map[string]bool{"a": true},
			want:      "[b]",
		},
		{
			name:      "declared only: published but not verified is not ready",
			m:         emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}}),
			states:    map[string]TaskState{"a": StateRunning},
			published: map[string]bool{"a": true},
			want:      "[]",
		},
		{
			name:      "declared only: verified but not published is not ready",
			m:         emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}}),
			states:    map[string]TaskState{"a": StateVerified},
			published: map[string]bool{},
			want:      "[]",
		},
		{
			name:       "accepted amendment gates exactly like a declared edge",
			m:          emanifest([]string{"a", "b"}, nil), // b declares NOTHING
			amendments: []Amendment{approved(1, "b", "a", "a")},
			want:       "[a]", // b is now gated on a, which is not verified
		},
		{
			name:       "accepted amendment satisfied",
			m:          emanifest([]string{"a", "b"}, nil),
			amendments: []Amendment{approved(1, "b", "a", "a")},
			states:     map[string]TaskState{"a": StateVerified},
			published:  map[string]bool{"a": true},
			want:       "[b]",
		},
		{
			name:       "a PROPOSED amendment is inert until a human accepts",
			m:          emanifest([]string{"a", "b"}, nil),
			amendments: []Amendment{proposed(1, "b", "a", "a")},
			want:       "[a b]",
		},
		{
			name:       "a scope amendment contributes no edge",
			m:          emanifest([]string{"a", "b"}, nil),
			amendments: []Amendment{{Seq: 1, Kind: AmendScope, Task: "b", Paths: []string{"x/**"}, ApprovedAt: time.Unix(2, 0)}},
			want:       "[a b]",
		},
		{
			name: "a re-plan amendment contributes no edge: an unproduced need is a hole, not a dependency",
			m:    emanifest([]string{"a", "b"}, nil),
			amendments: []Amendment{{Seq: 1, Kind: AmendReplan, Task: "b", From: "a",
				Artifact: "ghost", ApprovedAt: time.Unix(2, 0)}},
			want: "[a b]",
		},
		{
			name:   "a live block disqualifies a task whatever its edges say",
			m:      emanifest([]string{"a", "b"}, nil),
			blocks: map[string]Block{"b": artBlock("b", "ghost", "a")},
			want:   "[a]",
		},
		{
			name:   "a CLEARED block is not a block",
			m:      emanifest([]string{"a", "b"}, nil),
			blocks: map[string]Block{"b": {}},
			want:   "[a b]",
		},
		{
			name:       "amendment naming a task outside the snapshot contributes nothing",
			m:          emanifest([]string{"a", "b"}, nil),
			amendments: []Amendment{approved(1, "b", "stranger", "stranger")},
			want:       "[a b]",
		},
		{
			name:      "producer mid-integration keeps the consumer ready",
			m:         emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}}),
			states:    map[string]TaskState{"a": StateIntegrating},
			published: map[string]bool{"a": true},
			want:      "[b]",
		},
		{
			name:      "producer at the merge gate keeps the consumer ready",
			m:         emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}}),
			states:    map[string]TaskState{"a": StateMergeable},
			published: map[string]bool{"a": true},
			want:      "[b]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Effective(tc.m, tc.amendments, tc.blocks)
			got := e.Ready(tc.states, tc.published)
			if fmt.Sprint(got) != tc.want {
				t.Fatalf("Ready = %v, want %s", got, tc.want)
			}
		})
	}
}

// TestEffectiveReadyIsMonotonicInTheProducer is the deviation from §9.1's
// literal "verified" stated as a property: once a consumer is offered, the
// producer's own progress must never withdraw the offer. A ready action that
// vanishes under the cursor is worse than one that is late.
func TestEffectiveReadyIsMonotonicInTheProducer(t *testing.T) {
	e := Effective(emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}}), nil, nil)
	published := map[string]bool{"a": true}
	for _, s := range []TaskState{StateVerified, StateIntegrating, StateMergeable, StateMerged} {
		got := e.Ready(map[string]TaskState{"a": s}, published)
		if fmt.Sprint(got) != "[b]" {
			t.Fatalf("producer %s: Ready = %v, want [b]", s, got)
		}
	}
}

// TestEffectiveReadyIsPure: no input is mutated and the answer does not drift.
func TestEffectiveReadyIsPure(t *testing.T) {
	m := emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}})
	e := Effective(m, []Amendment{approved(1, "b", "a", "a")}, map[string]Block{"a": artBlock("a", "z", "b")})
	states := map[string]TaskState{"a": StateVerified}
	published := map[string]bool{"a": true}
	first := fmt.Sprint(e.Ready(states, published))
	for range 20 {
		if got := fmt.Sprint(e.Ready(states, published)); got != first {
			t.Fatalf("not pure: %s then %s", first, got)
		}
	}
	if len(states) != 1 || len(published) != 1 {
		t.Fatal("Ready mutated its inputs")
	}
	if len(e.declared.Edges) != 1 {
		t.Fatalf("the DECLARED graph was mutated: %v", e.declared.Edges)
	}
}

// TestDeclaredIsNotAmended: Declared() and Merged() must not be the same value,
// or every caller that asked the wrong one still got the right answer by
// accident and the distinction rots.
func TestDeclaredIsNotAmended(t *testing.T) {
	e := Effective(emanifest([]string{"a", "b"}, nil), []Amendment{approved(1, "b", "a", "a")}, nil)
	if len(e.Declared().Edges) != 0 {
		t.Fatalf("Declared() carries amended edges: %v", e.Declared().Edges)
	}
	g := e.Merged()
	if fmt.Sprint(g.Edges) != "[{a b a}]" {
		t.Fatalf("Merged().Edges = %v", g.Edges)
	}
	if fmt.Sprint(g.Needs["b"]) != "[a]" {
		t.Fatalf("Merged().Needs[b] = %v — an amended dependency invisible to Ready", g.Needs["b"])
	}
	if len(e.Declared().Needs["b"]) != 0 {
		t.Fatal("Merged() wrote through into the declared graph's Needs map")
	}
}

// ---------------------------------------------------------------- amendments

// TestAmendmentCycleRejected is §11.3's last rule, BINDING: an amendment that
// closes a loop is REJECTED, and the finding names the ACTUAL cycle — every task
// and every artifact in it — because the remedy is a human re-plan and a boolean
// cannot be re-planned against.
func TestAmendmentCycleRejected(t *testing.T) {
	cases := []struct {
		name    string
		m       Manifest
		already []Amendment
		amend   Amendment
		wantMsg string // "" means: must be accepted
		// wantNoAdd: accepted, and STILL contributes no edge — an amendment whose
		// endpoints are not in the snapshot is dropped rather than inventing a
		// task the human never approved one for.
		wantNoAdd bool
	}{
		{
			name:    "length 1: the amendment makes a task its own producer",
			m:       emanifest([]string{"a"}, nil),
			amend:   approved(1, "a", "a", "a"),
			wantMsg: "a → (a) → a",
		},
		{
			name:    "length 2: the amendment reverses a declared edge",
			m:       emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}}),
			amend:   approved(1, "a", "b", "b"),
			wantMsg: "a → (a) → b → (b) → a",
		},
		{
			name:    "length 3: the amendment closes a chain",
			m:       emanifest([]string{"a", "b", "c"}, map[string][]string{"b": {"a"}, "c": {"b"}}),
			amend:   approved(1, "a", "c", "c"),
			wantMsg: "a → (a) → b → (b) → c → (c) → a",
		},
		{
			name:    "the loop is closed against an EARLIER amendment, not a declared edge",
			m:       emanifest([]string{"a", "b"}, nil),
			already: []Amendment{approved(1, "b", "a", "a")},
			amend:   approved(2, "a", "b", "b"),
			wantMsg: "a → (a) → b → (b) → a",
		},
		{
			name:  "an amendment that does NOT close a loop is accepted",
			m:     emanifest([]string{"a", "b", "c"}, map[string][]string{"b": {"a"}}),
			amend: approved(1, "c", "b", "b"),
		},
		{
			name:  "a diamond is not a cycle",
			m:     emanifest([]string{"top", "l", "r", "bot"}, map[string][]string{"l": {"top"}, "r": {"top"}, "bot": {"l"}}),
			amend: approved(1, "bot", "r", "r"),
		},
		{
			name:      "an amendment naming a task outside the snapshot cannot close anything",
			m:         emanifest([]string{"a"}, nil),
			amend:     approved(1, "a", "stranger", "stranger"),
			wantNoAdd: true, // …and it contributes no edge either: totality, not silence
		},
		{
			name:  "a scope amendment over an acyclic graph is fine",
			m:     emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}}),
			amend: Amendment{Seq: 1, Kind: AmendScope, Task: "b", Paths: []string{"x/**"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Effective(tc.m, tc.already, nil)
			ce := AmendmentCycle(e, tc.amend)
			if tc.wantMsg == "" {
				if ce != nil {
					t.Fatalf("amendment rejected for a cycle that is not there: %v", ce)
				}
				// Accepting it must actually add the edge.
				ne := e.With(tc.amend)
				_, want := tc.amend.Edge()
				want = want && !tc.wantNoAdd
				if want && len(ne.Added) != len(e.Added)+1 {
					t.Fatalf("With() did not add the edge: %v", ne.Added)
				}
				if tc.wantNoAdd && len(ne.Added) != len(e.Added) {
					t.Fatalf("With() invented an edge over a task outside the run: %v", ne.Added)
				}
				return
			}
			if ce == nil {
				t.Fatal("want a cycle, got nil — a loud block just became a silent deadlock")
			}
			if !strings.Contains(ce.Error(), tc.wantMsg) {
				t.Fatalf("cycle = %q, want it to name %q", ce.Error(), tc.wantMsg)
			}
			if !strings.HasPrefix(ce.Error(), `manifest "atlas": dependency cycle:`) {
				t.Errorf("runtime cycle does not render like the loader's: %q", ce.Error())
			}
			for i := 1; i < len(ce.Path); i++ {
				if ce.Path[i-1].To != ce.Path[i].From {
					t.Fatalf("path is not connected at %d: %+v", i, ce.Path)
				}
			}
			if ce.Path[len(ce.Path)-1].To != ce.Path[0].From {
				t.Fatalf("path does not close: %+v", ce.Path)
			}
		})
	}
}

// TestAmendmentCycleIgnoresLiveBlocks: a transiently parked pair of children
// must not veto a human's edit. Mutual wait through live blocks is a §12.1
// FINDING over a stopped run, and accepting the amendment is what UNPARKS them.
func TestAmendmentCycleIgnoresLiveBlocks(t *testing.T) {
	m := emanifest([]string{"a", "b"}, nil)
	e := Effective(m, nil, map[string]Block{"a": artBlock("a", "b", "b")})
	if ce := AmendmentCycle(e, approved(1, "b", "a", "a")); ce != nil {
		t.Fatalf("amendment refused because of a live block: %v", ce)
	}
	// …and the wait-for graph, which is where that pair IS a finding, sees it.
	e2 := e.With(approved(1, "b", "a", "a"))
	if ce := e2.WaitCycle(); ce == nil {
		t.Fatal("WaitCycle must see the mutual wait the amendment check ignored")
	}
}

// TestWithReplacesTheSameSeq: approval is an UPDATE of an existing row, not a
// second row. Appending would render the amendment twice and double its edge.
func TestWithReplacesTheSameSeq(t *testing.T) {
	e := Effective(emanifest([]string{"a", "b"}, nil), []Amendment{proposed(7, "b", "a", "a")}, nil)
	if len(e.Added) != 0 {
		t.Fatalf("a proposal is inert: Added = %v", e.Added)
	}
	ne := e.With(approved(7, "b", "a", "a"))
	if len(ne.Amendments) != 1 {
		t.Fatalf("approval appended a second row: %+v", ne.Amendments)
	}
	if fmt.Sprint(ne.Added) != "[{a b a}]" {
		t.Fatalf("Added = %v", ne.Added)
	}
	if len(e.Amendments) != 1 || e.Amendments[0].Accepted() {
		t.Fatal("With mutated the graph it was called on")
	}
}

// TestAddedIsAFunctionOfTheSet: two Looms holding the same amendments in
// different orders must draw the same graph.
func TestAddedIsAFunctionOfTheSet(t *testing.T) {
	m := emanifest([]string{"a", "b", "c"}, nil)
	fwd := Effective(m, []Amendment{approved(1, "b", "a", "a"), approved(2, "c", "b", "b")}, nil)
	rev := Effective(m, []Amendment{approved(2, "c", "b", "b"), approved(1, "b", "a", "a")}, nil)
	if fmt.Sprint(fwd.Added) != fmt.Sprint(rev.Added) {
		t.Fatalf("order-dependent: %v vs %v", fwd.Added, rev.Added)
	}
	if fmt.Sprint(fwd.Merged().Edges) != fmt.Sprint(rev.Merged().Edges) {
		t.Fatalf("order-dependent edges: %v vs %v", fwd.Merged().Edges, rev.Merged().Edges)
	}
	// The same edge approved twice is one dependency.
	dup := Effective(m, []Amendment{approved(1, "b", "a", "a"), approved(2, "b", "a", "a")}, nil)
	if len(dup.Added) != 1 {
		t.Fatalf("duplicate approval doubled the edge: %v", dup.Added)
	}
}

func TestAmendmentBodyRoundTrip(t *testing.T) {
	in := Amendment{Kind: AmendScope, Task: "b", Artifact: "x", From: "a",
		Paths: []string{"internal/**", "cmd/x.go"}, Reason: "the boundary was drawn wrong"}
	out, ok := DecodeAmendmentBody(AmendScope, EncodeAmendmentBody(in))
	if !ok {
		t.Fatal("round trip reported a decode failure")
	}
	if fmt.Sprint(out) != fmt.Sprint(in) {
		t.Fatalf("round trip: %+v -> %+v", in, out)
	}
}

// TestAmendmentBodyDegrades: a body that will not parse must still RENDER as an
// amendment of its kind. An invisible amendment is an edge the human cannot see
// and cannot revoke — and, critically, it must contribute no edge either.
func TestAmendmentBodyDegrades(t *testing.T) {
	a, ok := DecodeAmendmentBody(AmendEdge, "{not json")
	if ok {
		t.Fatal("want ok=false")
	}
	if a.Kind != AmendEdge {
		t.Fatalf("kind lost: %+v", a)
	}
	a.ApprovedAt = time.Unix(2, 0)
	if _, has := a.Edge(); has {
		t.Fatal("a malformed amendment contributed an edge")
	}
}

// ---------------------------------------------------------------- wait-for

// TestWaitCycle is §12.1(a): the mutual wait among children, over the effective
// graph, reported as the actual wait-for cycle.
func TestWaitCycle(t *testing.T) {
	cases := []struct {
		name       string
		m          Manifest
		amendments []Amendment
		blocks     map[string]Block
		wantMsg    string
	}{
		{
			name:    "length 2, entirely from live blocks over UNDECLARED artifacts",
			m:       emanifest([]string{"a", "b"}, nil),
			blocks:  map[string]Block{"a": artBlock("a", "b-thing", "b"), "b": artBlock("b", "a-thing", "a")},
			wantMsg: "a → (a-thing) → b → (b-thing) → a",
		},
		{
			name:    "length 3 through blocks",
			m:       emanifest([]string{"a", "b", "c"}, nil),
			blocks:  map[string]Block{"a": artBlock("a", "cx", "c"), "b": artBlock("b", "ax", "a"), "c": artBlock("c", "bx", "b")},
			wantMsg: "a → (ax) → b → (bx) → c → (cx) → a",
		},
		{
			name:    "length 1: a child parked on its own output",
			m:       emanifest([]string{"a"}, nil),
			blocks:  map[string]Block{"a": artBlock("a", "a", "a")},
			wantMsg: "a → (a) → a",
		},
		{
			name:    "a declared edge plus one block closes the loop",
			m:       emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}}),
			blocks:  map[string]Block{"a": artBlock("a", "late", "b")},
			wantMsg: "a → (a) → b → (late) → a",
		},
		{
			name:       "an accepted amendment plus one block closes the loop",
			m:          emanifest([]string{"a", "b"}, nil),
			amendments: []Amendment{approved(1, "b", "a", "a")},
			blocks:     map[string]Block{"a": artBlock("a", "late", "b")},
			wantMsg:    "a → (a) → b → (late) → a",
		},
		{
			name:   "a needs-decision block is a wait on a HUMAN and draws no edge",
			m:      emanifest([]string{"a", "b"}, nil),
			blocks: map[string]Block{"a": {Kind: BlockNeedsDecision, Task: "a"}, "b": {Kind: BlockNeedsDecision, Task: "b"}},
		},
		{
			name:   "two children blocked on DIFFERENT peers is not a cycle",
			m:      emanifest([]string{"a", "b", "c"}, nil),
			blocks: map[string]Block{"a": artBlock("a", "cx", "c"), "b": artBlock("b", "cx", "c")},
		},
		{
			name:   "a block naming nobody is a hole, not an edge",
			m:      emanifest([]string{"a"}, nil),
			blocks: map[string]Block{"a": artBlock("a", "ghost", "")},
		},
		{
			name:   "a block naming a task outside the run draws no edge",
			m:      emanifest([]string{"a"}, nil),
			blocks: map[string]Block{"a": artBlock("a", "ghost", "stranger")},
		},
		{
			name: "the declared graph alone is acyclic (§4.5 made it impossible)",
			m:    emanifest([]string{"a", "b", "c"}, map[string][]string{"b": {"a"}, "c": {"a", "b"}}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Effective(tc.m, tc.amendments, tc.blocks)
			ce := e.WaitCycle()
			if tc.wantMsg == "" {
				if ce != nil {
					t.Fatalf("phantom deadlock: %v", ce)
				}
				return
			}
			if ce == nil {
				t.Fatal("want a wait-for cycle, got nil")
			}
			if !strings.Contains(ce.Error(), tc.wantMsg) {
				t.Fatalf("cycle = %q, want it to name %q", ce.Error(), tc.wantMsg)
			}
			// Every task AND artifact in the loop is named, which is the whole
			// requirement: a boolean cannot be re-planned against.
			for _, ed := range ce.Path {
				if ed.From == "" || ed.To == "" || ed.Artifact == "" {
					t.Fatalf("unnamed element in the cycle: %+v", ce.Path)
				}
			}
		})
	}
}

// TestWaitCycleIsDeterministic: two Looms rendering one deadlock must name the
// same loop, and a map iteration over Blocks is exactly how that stops being
// true.
func TestWaitCycleIsDeterministic(t *testing.T) {
	e := Effective(emanifest([]string{"a", "b", "c", "d"}, nil), nil, map[string]Block{
		"a": artBlock("a", "bx", "b"), "b": artBlock("b", "ax", "a"),
		"c": artBlock("c", "dx", "d"), "d": artBlock("d", "cx", "c"),
	})
	first := e.WaitCycle().Error()
	for range 50 {
		if got := e.WaitCycle().Error(); got != first {
			t.Fatalf("nondeterministic: %q then %q", first, got)
		}
	}
}

// ---------------------------------------------------------------- progress

// TestTickBuckets is §9.3. The buckets must PARTITION the run — a task in none
// of them is a deadlock verdict computed from an incomplete picture.
func TestTickBuckets(t *testing.T) {
	m := emanifest([]string{"a", "b", "c", "d", "e", "f"}, map[string][]string{"b": {"a"}})
	blocks := map[string]Block{"c": artBlock("c", "ghost", "")}
	states := map[string]TaskState{
		"a": StateVerified,  // in flight: §10.2 is about to pick it up
		"c": StateRunning,   // the block file beats the state column
		"d": StateMergeable, // terminal: waiting on a human at §5.2
		"e": StateFailed,    // terminal
		"f": StateApproved,  // in flight: the human pressed, the spawn is owed
	}
	p := Tick(Effective(m, nil, blocks), states, map[string]bool{"a": true})

	if fmt.Sprint(p.Ready) != "[b]" {
		t.Errorf("Ready = %v", p.Ready)
	}
	if fmt.Sprint(p.InFlight) != "[a f]" {
		t.Errorf("InFlight = %v, want [a f] (verified and approved are in flight)", p.InFlight)
	}
	if fmt.Sprint(p.Blocked) != "[c]" {
		t.Errorf("Blocked = %v", p.Blocked)
	}
	if fmt.Sprint(p.Terminal) != "[d e]" {
		t.Errorf("Terminal = %v", p.Terminal)
	}
	if len(p.Waiting) != 0 || len(p.Unclassified) != 0 {
		t.Errorf("Waiting = %v, Unclassified = %v", p.Waiting, p.Unclassified)
	}
	if p.Deadlocked {
		t.Error("a run with a ready task is not deadlocked")
	}

	total := len(p.Ready) + len(p.Waiting) + len(p.InFlight) + len(p.Blocked) +
		len(p.Terminal) + len(p.Unclassified)
	if total != len(m.Tasks) {
		t.Fatalf("buckets do not partition the run: %d of %d", total, len(m.Tasks))
	}
}

// TestTickDeadlockPredicate is §9.3's condition, one row per way a run can and
// cannot be stopped.
func TestTickDeadlockPredicate(t *testing.T) {
	chain := emanifest([]string{"a", "b"}, map[string][]string{"b": {"a"}})
	cases := []struct {
		name      string
		m         Manifest
		blocks    map[string]Block
		states    map[string]TaskState
		published map[string]bool
		want      bool
		wantIn    string // bucket the stuck task must land in
	}{
		{
			name: "a fresh run is not deadlocked", m: chain, want: false,
		},
		{
			name: "producer running: the consumer waits and that is progress",
			m:    chain, states: map[string]TaskState{"a": StateRunning}, want: false,
			wantIn: "waiting",
		},
		{
			name: "producer failed: nothing ready, nothing in flight, b is non-terminal",
			m:    chain, states: map[string]TaskState{"a": StateFailed}, want: true,
			wantIn: "waiting",
		},
		{
			name: "producer verified but never published: the consumer is stuck",
			m:    chain, states: map[string]TaskState{"a": StateVerified}, want: false, // verified is in flight
		},
		{
			name:   "everything blocked is the state that LOOKS like progress",
			m:      chain,
			states: map[string]TaskState{"a": StateBlocked, "b": StateBlocked},
			blocks: map[string]Block{
				"a": {Kind: BlockExternal, Task: "a"}, "b": {Kind: BlockNeedsDecision, Task: "b"},
			},
			want: true, wantIn: "blocked",
		},
		{
			name: "every task terminal is a finished run, not a deadlock",
			m:    chain, states: map[string]TaskState{"a": StateMerged, "b": StateMerged}, want: false,
		},
		{
			name: "every task at the merge gate is waiting on a HUMAN, which is the design working",
			m:    chain, states: map[string]TaskState{"a": StateMergeable, "b": StateMergeable}, want: false,
		},
		{
			name: "an empty run is not deadlocked",
			m:    Manifest{Name: "empty"}, want: false,
		},
		{
			name: "a state no bucket claims stops the run AND is rendered",
			m:    chain, states: map[string]TaskState{"a": "teleporting", "b": StateMerged},
			want: true, wantIn: "unclassified",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Tick(Effective(tc.m, nil, tc.blocks), tc.states, tc.published)
			if p.Deadlocked != tc.want {
				t.Fatalf("Deadlocked = %v, want %v (%+v)", p.Deadlocked, tc.want, p)
			}
			switch tc.wantIn {
			case "waiting":
				if len(p.Waiting) == 0 {
					t.Fatalf("Waiting is empty: %+v", p)
				}
			case "blocked":
				if len(p.Blocked) != 2 {
					t.Fatalf("Blocked = %v", p.Blocked)
				}
			case "unclassified":
				if fmt.Sprint(p.Unclassified) != "[a]" {
					t.Fatalf("Unclassified = %v", p.Unclassified)
				}
			}
		})
	}
}

// TestTickCountsBlockedSeparatelyFromInFlight: a blocked child holds a process
// and a worktree, and counting it as in-flight would make the deadlock predicate
// never fire — which is the entire hazard §12.1 exists for.
func TestTickCountsBlockedSeparatelyFromInFlight(t *testing.T) {
	m := emanifest([]string{"a"}, nil)
	p := Tick(Effective(m, nil, map[string]Block{"a": {Kind: BlockExternal, Task: "a"}}),
		map[string]TaskState{"a": StateRunning}, nil)
	if len(p.InFlight) != 0 {
		t.Fatalf("InFlight = %v, want none: the child is parked", p.InFlight)
	}
	if !p.Deadlocked {
		t.Fatal("a run whose only child is parked on an outage has stopped")
	}
}

// ---------------------------------------------------------------- §9.2 plan

func TestPlanBase(t *testing.T) {
	m := Manifest{Name: "atlas", Tasks: []Task{
		{ID: "schema", Repo: "bank", Produces: []Artifact{{ID: "account-schema"}}},
		{ID: "config", Repo: "bank", Produces: []Artifact{{ID: "cfg"}}},
		{ID: "types", Repo: "ballista", Produces: []Artifact{{ID: "ts"}}},
		{ID: "api", Repo: "bank", Needs: []string{"cfg", "account-schema", "ts"}},
	}}
	tasks := map[string]store.DelegationTask{
		"schema": {TaskID: "schema", Branch: "loom/atlas/schema", BranchHead: "aaa"},
		"config": {TaskID: "config", Branch: "loom/atlas/config", BranchHead: "bbb"},
		"types":  {TaskID: "types", Branch: "loom/atlas/types", BranchHead: "ccc"},
	}
	bases := map[string]string{"bank": "base-bank", "ballista": "base-ballista"}
	integration := map[string]string{"bank": "/i/bank", "ballista": "/i/ballista"}

	p := PlanBase(Effective(m, nil, nil), m, m.Tasks[3], tasks, bases, integration)
	if p.Base != "base-bank" {
		t.Errorf("Base = %q", p.Base)
	}
	// ASCENDING TASK ID, not manifest order and not `needs` order: a re-spawn
	// must reproduce the tree byte-for-byte.
	want := "[{config loom/atlas/config bbb} {schema loom/atlas/schema aaa}]"
	if fmt.Sprint(p.Merge) != want {
		t.Errorf("Merge = %v, want %s", p.Merge, want)
	}
	// Cross-repo contributes no merge — it arrives as the producer repo's
	// INTEGRATION worktree add-dir.
	if fmt.Sprint(p.AddDirs) != "[/i/ballista]" {
		t.Errorf("AddDirs = %v", p.AddDirs)
	}
}

// TestPlanBaseIncludesAmendedProducers: an accepted amendment is a real
// dependency, so the consumer's tree must contain it. A base built from the
// declared needs alone hands the child a tree missing a dependency it was told
// it has — the revision-1 defect, one level up.
func TestPlanBaseIncludesAmendedProducers(t *testing.T) {
	m := Manifest{Name: "atlas", Tasks: []Task{
		{ID: "schema", Repo: "bank", Produces: []Artifact{{ID: "account-schema"}}},
		{ID: "api", Repo: "bank"},
	}}
	e := Effective(m, []Amendment{approved(1, "api", "schema", "account-schema")}, nil)
	p := PlanBase(e, m, m.Tasks[1],
		map[string]store.DelegationTask{"schema": {Branch: "b", BranchHead: "aaa"}},
		map[string]string{"bank": "base"}, nil)
	if fmt.Sprint(p.Merge) != "[{schema b aaa}]" {
		t.Fatalf("Merge = %v", p.Merge)
	}
}

// TestPlanBaseProducerWithNoRecordedHead: the ref is kept with an empty SHA so
// MergeProducers refuses loudly. Dropping it would silently hand the child a
// tree missing a declared dependency, which is the failure this whole function
// exists to prevent.
func TestPlanBaseProducerWithNoRecordedHead(t *testing.T) {
	m := Manifest{Name: "atlas", Tasks: []Task{
		{ID: "p", Repo: "r", Produces: []Artifact{{ID: "x"}}},
		{ID: "c", Repo: "r", Needs: []string{"x"}},
	}}
	p := PlanBase(Effective(m, nil, nil), m, m.Tasks[1],
		map[string]store.DelegationTask{}, map[string]string{"r": "base"}, nil)
	if len(p.Merge) != 1 || p.Merge[0].SHA != "" || p.Merge[0].Task != "p" {
		t.Fatalf("Merge = %v, want one ref with an empty sha", p.Merge)
	}
}

// TestPlanBaseDeduplicates: two artifacts from one producer is one merge, and
// two cross-repo producers in one repo is one add-dir.
func TestPlanBaseDeduplicates(t *testing.T) {
	m := Manifest{Name: "atlas", Tasks: []Task{
		{ID: "p", Repo: "r", Produces: []Artifact{{ID: "x"}, {ID: "y"}}},
		{ID: "q1", Repo: "far", Produces: []Artifact{{ID: "f1"}}},
		{ID: "q2", Repo: "far", Produces: []Artifact{{ID: "f2"}}},
		{ID: "c", Repo: "r", Needs: []string{"x", "y", "f1", "f2"}},
	}}
	p := PlanBase(Effective(m, nil, nil), m, m.Tasks[3],
		map[string]store.DelegationTask{"p": {Branch: "b", BranchHead: "aaa"}},
		map[string]string{"r": "base"}, map[string]string{"far": "/i/far"})
	if len(p.Merge) != 1 {
		t.Errorf("Merge = %v, want one", p.Merge)
	}
	if fmt.Sprint(p.AddDirs) != "[/i/far]" {
		t.Errorf("AddDirs = %v, want one", p.AddDirs)
	}
}

func TestProducersCodec(t *testing.T) {
	refs := []ProducerRef{{Task: "a", Branch: "loom/x/a", SHA: "aaa"}}
	if got := EncodeProducers(nil); got != "" {
		t.Errorf("the empty slice must encode as the column default, got %q", got)
	}
	if got := DecodeProducers(""); got != nil {
		t.Errorf("DecodeProducers(\"\") = %v", got)
	}
	if got := DecodeProducers(EncodeProducers(refs)); fmt.Sprint(got) != fmt.Sprint(refs) {
		t.Errorf("round trip: %v", got)
	}
	if got := DecodeProducers("{not json"); got != nil {
		t.Errorf("a stray byte must degrade to nil, got %v", got)
	}
}

// TestPublishedSet: §8.3 publishes only after verifying the artifact is
// committed, so a row with no commit sha is a half-written publish.
func TestPublishedSet(t *testing.T) {
	got := PublishedSet([]store.DelegationArtifact{
		{ArtifactID: "good", CommitSHA: "aaa"},
		{ArtifactID: "half"},
		{CommitSHA: "bbb"},
	})
	if !got["good"] || got["half"] || len(got) != 1 {
		t.Fatalf("PublishedSet = %v", got)
	}
	if PublishedSet(nil) != nil {
		t.Error("no rows is no set")
	}
}
