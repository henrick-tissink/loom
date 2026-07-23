package arch

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func nodesOf(ids ...string) []LayoutNode {
	out := make([]LayoutNode, 0, len(ids))
	for _, id := range ids {
		out = append(out, LayoutNode{ID: id})
	}
	return out
}

// edgesOf parses "a>b" pairs, with a trailing "!" marking a cycle edge.
func edgesOf(specs ...string) []LayoutEdge {
	out := make([]LayoutEdge, 0, len(specs))
	for _, s := range specs {
		cyc := strings.HasSuffix(s, "!")
		s = strings.TrimSuffix(s, "!")
		parts := strings.SplitN(s, ">", 2)
		out = append(out, LayoutEdge{From: parts[0], To: parts[1], Cycle: cyc})
	}
	return out
}

func ranksOf(t *testing.T, placed []Placement) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, p := range placed {
		out[p.ID] = p.Rank
	}
	return out
}

// §5.6: "Rank = longest-path depth". LONGEST, not shortest — a diamond's join
// belongs one column right of its deepest arm, or the picture shows an edge
// running backwards through a card.
func TestLayoutRanking(t *testing.T) {
	cases := []struct {
		name  string
		nodes []LayoutNode
		edges []LayoutEdge
		want  map[string]int
	}{
		{name: "a chain",
			nodes: nodesOf("a", "b", "c"), edges: edgesOf("a>b", "b>c"),
			want: map[string]int{"a": 0, "b": 1, "c": 2}},
		{name: "a diamond takes the LONGEST path, not the shortest",
			nodes: nodesOf("a", "b", "c", "d", "e"),
			edges: edgesOf("a>b", "b>c", "c>d", "a>d", "d>e"),
			want:  map[string]int{"a": 0, "b": 1, "c": 2, "d": 3, "e": 4}},
		{name: "multiple roots all sit at rank 0",
			nodes: nodesOf("a", "b", "c"), edges: edgesOf("a>c", "b>c"),
			want: map[string]int{"a": 0, "b": 0, "c": 1}},
		{name: "no edges at all is one column",
			nodes: nodesOf("a", "b", "c"),
			want:  map[string]int{"a": 0, "b": 0, "c": 0}},
		{name: "a cycle-flagged edge does not contribute to rank",
			// §5.6 routes backward edges through a band rather than letting
			// them push ranks. Without this the whole component drifts right by
			// one column per cycle edge for no reason a reader can see.
			nodes: nodesOf("a", "b"), edges: edgesOf("a>b", "b>a!"),
			want: map[string]int{"a": 0, "b": 1}},
		{name: "an edge naming an unknown node is ignored, not fatal",
			nodes: nodesOf("a", "b"), edges: edgesOf("a>b", "ghost>b", "a>nowhere"),
			want: map[string]int{"a": 0, "b": 1}},
		{name: "a self-edge carries no ordering and is dropped",
			nodes: nodesOf("a", "b"), edges: edgesOf("a>a", "a>b"),
			want: map[string]int{"a": 0, "b": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			placed, _, _ := Layout(tc.nodes, tc.edges)
			if got := ranksOf(t, placed); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ranks = %v, want %v", got, tc.want)
			}
		})
	}
}

// §9 is emphatic: the graph still DRAWS on a cycle. Layout returns, no hang, no
// panic, and every node gets a position — the alternative is a blank panel,
// which §9's degradation matrix has no row for.
func TestLayoutSurvivesCycles(t *testing.T) {
	cases := []struct {
		name  string
		nodes []LayoutNode
		edges []LayoutEdge
	}{
		{name: "2-cycle, unflagged", nodes: nodesOf("a", "b"), edges: edgesOf("a>b", "b>a")},
		{name: "3-cycle, unflagged", nodes: nodesOf("a", "b", "c"),
			edges: edgesOf("a>b", "b>c", "c>a")},
		{name: "a cycle beside a clean subgraph", nodes: nodesOf("a", "b", "c", "x", "y"),
			edges: edgesOf("a>b", "b>c", "c>a", "x>y")},
		{name: "every node in one big cycle", nodes: nodesOf("a", "b", "c", "d", "e", "f"),
			edges: edgesOf("a>b", "b>c", "c>d", "d>e", "e>f", "f>a")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The bounded relaxation is what makes this terminate; a
			// topological sort would have to answer "is there a cycle" before
			// it could answer "what rank".
			placed, w, h := Layout(tc.nodes, tc.edges)
			if len(placed) != len(tc.nodes) {
				t.Fatalf("%d placements for %d nodes — every node must get a position",
					len(placed), len(tc.nodes))
			}
			if w < NodeW || h < NodeH {
				t.Errorf("stage = %dx%d, want at least one card", w, h)
			}
			// The clamp: an unflagged cycle must not lay the component out
			// across a column per node.
			for _, p := range placed {
				if p.Rank > len(tc.nodes)-1 {
					t.Errorf("node %s at rank %d exceeds the n-1 clamp", p.ID, p.Rank)
				}
			}
		})
	}
}

// §12 BINDING: "the same manifest yields byte-identical coordinates across 100
// runs and across map-iteration reorderings of the input".
//
// This is the property the whole layout serves. Without it an unrelated status
// tick reshuffles the picture under the user's eye and golden tests are
// impossible — and it is exactly what could not be asserted while the layout
// lived in a frontend with no test runner.
func TestLayoutIsDeterministic(t *testing.T) {
	nodes := nodesOf("schema", "handlers", "client", "docs", "migrate", "seed")
	edges := edgesOf("schema>handlers", "schema>migrate", "handlers>client",
		"migrate>seed", "seed>client", "schema>docs")

	want, ww, wh := Layout(nodes, edges)
	for i := 0; i < 100; i++ {
		got, w, h := Layout(nodes, edges)
		if !reflect.DeepEqual(got, want) || w != ww || h != wh {
			t.Fatalf("run %d differed:\n got %v %dx%d\nwant %v %dx%d", i, got, w, h, want, ww, wh)
		}
	}

	// Go randomizes map iteration per run, and the internal maps (rank,
	// adjacency, barycentre) are the hazard. Shuffling the EDGE slice is the
	// input-side version of the same question: for a DAG the ranks are a
	// property of the topology, the barycentre is a mean, and every tie breaks
	// on node id — so a reordered edge list must land byte-identically.
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		shuffled := append([]LayoutEdge(nil), edges...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		got, w, h := Layout(nodes, shuffled)
		if !reflect.DeepEqual(got, want) || w != ww || h != wh {
			t.Fatalf("edge order %v changed the layout:\n got %v\nwant %v", shuffled, got, want)
		}
	}
}

// Left-to-right orientation (§5.6), and no two cards on top of each other.
// Overlap is the failure a human sees instantly and no unit test of ranks alone
// would catch.
func TestLayoutGeometry(t *testing.T) {
	nodes := nodesOf("a", "b", "c", "d", "e")
	edges := edgesOf("a>b", "a>c", "a>d", "b>e", "c>e", "d>e")
	placed, w, h := Layout(nodes, edges)

	at := map[string]Placement{}
	for _, p := range placed {
		at[p.ID] = p
	}
	// Rank decides X and nothing else does: two nodes of the same rank share a
	// column, and a deeper rank is strictly further right.
	if at["a"].X != 0 {
		t.Errorf("root x = %d, want 0", at["a"].X)
	}
	if at["b"].X != at["c"].X || at["c"].X != at["d"].X {
		t.Errorf("rank 1 does not share a column: b=%d c=%d d=%d", at["b"].X, at["c"].X, at["d"].X)
	}
	if !(at["a"].X < at["b"].X && at["b"].X < at["e"].X) {
		t.Errorf("layout is not left-to-right: a=%d b=%d e=%d", at["a"].X, at["b"].X, at["e"].X)
	}

	byCol := map[int][]Placement{}
	for _, p := range placed {
		byCol[p.X] = append(byCol[p.X], p)
	}
	for x, col := range byCol {
		sort.Slice(col, func(i, j int) bool { return col[i].Y < col[j].Y })
		for i := 1; i < len(col); i++ {
			if col[i].Y-col[i-1].Y < NodeH {
				t.Errorf("column x=%d overlaps: %s at y=%d and %s at y=%d",
					x, col[i-1].ID, col[i-1].Y, col[i].ID, col[i].Y)
			}
		}
	}
	// The stage must contain every card, or the frontend's fit-on-first-render
	// crops the graph.
	for _, p := range placed {
		if p.X+NodeW > w || p.Y+NodeH > h {
			t.Errorf("node %s at (%d,%d) falls outside the %dx%d stage", p.ID, p.X, p.Y, w, h)
		}
	}
}

// An empty graph is a real state (a run whose manifest has no tasks, or a fully
// filtered node set) and must not produce a zero-sized stage — a 0x0 viewBox
// makes the frontend's fit divide by zero.
func TestLayoutEmpty(t *testing.T) {
	placed, w, h := Layout(nil, nil)
	if len(placed) != 0 {
		t.Errorf("placements = %v, want none", placed)
	}
	if w != NodeW || h != NodeH {
		t.Errorf("stage = %dx%d, want one card's worth", w, h)
	}
}

// §12's "12-node fixture golden". A golden rather than a property because the
// point is that the PICTURE does not move: a change to the sweep, the stride or
// the tie-break is a change a human should have to look at and accept, not one
// that slips through because the ranks still happen to check out.
func TestLayoutGolden12Nodes(t *testing.T) {
	nodes := nodesOf("api", "auth", "billing", "cache", "db", "email",
		"frontend", "gateway", "hooks", "infra", "jobs", "kv")
	edges := edgesOf(
		"db>api", "db>auth", "api>billing", "auth>billing",
		"cache>api", "infra>db", "infra>cache", "infra>kv",
		"kv>jobs", "jobs>email", "billing>frontend", "api>gateway",
		"gateway>frontend", "hooks>jobs", "frontend>hooks!",
	)
	placed, w, h := Layout(nodes, edges)

	var b strings.Builder
	fmt.Fprintf(&b, "stage %dx%d\n", w, h)
	for _, p := range placed {
		fmt.Fprintf(&b, "%s r%d (%d,%d)\n", p.ID, p.Rank, p.X, p.Y)
	}
	const golden = `stage 1472x230
api r2 (624,86)
auth r2 (624,0)
billing r3 (936,0)
cache r1 (312,86)
db r1 (312,0)
email r3 (936,172)
frontend r4 (1248,86)
gateway r3 (936,86)
hooks r0 (0,129)
infra r0 (0,43)
jobs r2 (624,172)
kv r1 (312,172)
`
	if b.String() != golden {
		t.Errorf("layout moved.\n got:\n%s\nwant:\n%s\n\nIf this change is intended, look at "+
			"the picture before updating the golden — §5.6's determinism is what stops the "+
			"graph shuffling under the user.", b.String(), golden)
	}
}
