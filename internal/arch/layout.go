package arch

import "sort"

// Layered DAG layout (orchestration-view §5.6), computed HERE and not in the
// frontend.
//
// The spec's reasons, restated because they are the ones that decided it: this
// is pure, it is the single most test-sensitive piece of the slice, this repo's
// test mass lives in Go, a future TUI view can reuse the ranks, and it keeps
// the frontend at zero new dependencies. The first shipped version put rank and
// order in graph.js and declared the deviation honestly — but §12 binds "the
// same manifest yields byte-identical coordinates across 100 runs and across
// map-iteration reorderings of the input", the frontend has no test runner and
// may not gain a dependency to get one, so that determinism claim was a
// comment. It is a test now.
//
// The frontend paints inline SVG from these coordinates and computes no
// positions of its own. One layout, one place.

// Card geometry. Wide-and-short because §5.6 chose LEFT-TO-RIGHT orientation: a
// node card carries a title, a repo chip and a badge, and a top-down layout
// spends the stage's horizontal budget on nothing.
//
// Exported and shipped in the payload rather than duplicated as a CSS or JS
// constant: the frontend needs the card size to draw the rectangle and to aim
// an edge at its midpoint, and two copies of a number that must agree is how a
// graph ends up with arrows landing beside their nodes.
const (
	NodeW = 224
	NodeH = 58
	GapX  = 88
	GapY  = 28
)

const (
	strideX = NodeW + GapX
	strideY = NodeH + GapY
)

// LayoutNode is the layout's input: an id and nothing else. Deliberately not
// the render DTO — layout depends on topology alone, and a function that could
// see a title could start deciding with it.
type LayoutNode struct{ ID string }

// LayoutEdge is one directed edge. Cycle marks an edge the caller already
// identified as part of a dependency cycle; those are excluded from ranking but
// are still laid out and still drawn.
//
// Band marks §5.1's `park` edge: a dependency discovered mid-task, drawn dashed
// "in the back-edge band above the ranks". It is excluded from ranking for that
// reason and not as an approximation — a park edge is by construction absent
// from the plan, so letting it push a rank would re-shape the planned picture
// every time a child parked and un-shape it when the park resolved, which is the
// re-layout-under-the-cursor §7.3 forbids arriving through the data instead of
// through the render.
//
// Band edges DO still feed the barycenter sweeps (see adjacency, which filters
// neither kind): they do not decide which column a node lands in, but two nodes
// that wait on each other are better drawn near each other, and the sweep is the
// only thing that can say so.
type LayoutEdge struct {
	From, To string
	Cycle    bool
	Band     bool
}

// Placement is one node's computed position.
type Placement struct {
	ID   string
	X    int
	Y    int
	Rank int
}

// Layout is the whole of §5.6, and it is a PURE FUNCTION of (nodes, edges).
//
// Determinism is the property everything else here serves. The same topology
// must produce byte-identical coordinates on every call and under any input
// reordering that preserves the node slice — otherwise an unrelated status tick
// reshuffles the picture under the user's eye, and golden tests are impossible.
// Every map in this function is therefore read in a deterministic order, and
// every tie breaks on node id rather than on iteration order.
func Layout(nodes []LayoutNode, edges []LayoutEdge) (placed []Placement, width, height int) {
	if len(nodes) == 0 {
		return []Placement{}, NodeW, NodeH
	}
	rank := rankOf(nodes, edges)

	maxRank := 0
	for _, n := range nodes {
		if r := rank[n.ID]; r > maxRank {
			maxRank = r
		}
	}
	columns := make([][]string, maxRank+1)
	// Input order seeds every column, so a graph with no edges keeps the
	// manifest's own task order instead of an alphabetical one nobody asked
	// for. The caller's slice is the ordering authority; nothing here sorts it.
	for _, n := range nodes {
		columns[rank[n.ID]] = append(columns[rank[n.ID]], n.ID)
	}

	preds, succs := adjacency(nodes, edges)

	// Two barycenter sweeps (§5.6). More sweeps buy nothing measurable at the
	// node counts §9 caps this view at, and each additional one is another
	// chance for the ordering to oscillate between two equally good
	// arrangements — which a user sees as the graph twitching on a manifest
	// rewrite.
	index := map[string]int{}
	reindex := func() {
		for _, col := range columns {
			for i, id := range col {
				index[id] = i
			}
		}
	}
	reindex()
	sweep := func(cols [][]string, neighbours map[string][]string) {
		for _, col := range cols {
			bary := make(map[string]float64, len(col))
			for _, id := range col {
				sum, n := 0.0, 0
				for _, nb := range neighbours[id] {
					if i, ok := index[nb]; ok {
						sum += float64(i)
						n++
					}
				}
				if n == 0 {
					bary[id] = float64(index[id])
					continue
				}
				bary[id] = sum / float64(n)
			}
			sort.SliceStable(col, func(i, j int) bool {
				if bary[col[i]] != bary[col[j]] {
					return bary[col[i]] < bary[col[j]]
				}
				// Ties break on NODE ID, never on the slice's incoming order and
				// never on a map walk. This is the line that makes the layout a
				// function of the topology alone.
				return col[i] < col[j]
			})
			for i, id := range col {
				index[id] = i
			}
		}
	}
	if len(columns) > 1 {
		sweep(columns[1:], preds)
		rev := make([][]string, 0, len(columns)-1)
		for i := len(columns) - 2; i >= 0; i-- {
			rev = append(rev, columns[i])
		}
		sweep(rev, succs)
	}
	reindex()

	tallest := 1
	for _, col := range columns {
		if len(col) > tallest {
			tallest = len(col)
		}
	}
	height = tallest*strideY - GapY
	placed = make([]Placement, 0, len(nodes))
	for r, col := range columns {
		// Columns are centred vertically against the tallest one, so a diamond
		// reads as a diamond rather than as a flag hanging off the top edge.
		top := (height - (len(col)*strideY - GapY)) / 2
		for i, id := range col {
			placed = append(placed, Placement{ID: id, X: r * strideX, Y: top + i*strideY, Rank: r})
		}
	}
	// Sorted by id so the RETURNED SLICE is a function of the set, independent
	// of column iteration. Callers key by id; the order here is only ever a
	// determinism hazard.
	sort.Slice(placed, func(i, j int) bool { return placed[i].ID < placed[j].ID })

	width = (maxRank+1)*strideX - GapX
	if width < NodeW {
		width = NodeW
	}
	if height < NodeH {
		height = NodeH
	}
	return placed, width, height
}

// rankOf is longest-path depth over the edge set, computed by BOUNDED
// RELAXATION rather than by a topological sort.
//
// A topological sort has to answer "is there a cycle" before it can answer
// "what rank", and §9 requires a rank even when the answer is yes. Relaxation
// capped at len(nodes) passes returns the exact longest-path depth for a DAG
// (it converges in at most depth passes) and a bounded, deterministic value
// inside a strongly connected component, with no special case and no
// possibility of a hang.
//
// Edges the caller flagged as part of a cycle — or as a back-edge band member —
// are skipped, which usually leaves the remainder acyclic; the clamp exists for
// the case where it does not.
func rankOf(nodes []LayoutNode, edges []LayoutEdge) map[string]int {
	rank := make(map[string]int, len(nodes))
	for _, n := range nodes {
		rank[n.ID] = 0
	}
	live := make([]LayoutEdge, 0, len(edges))
	for _, e := range edges {
		if e.Cycle || e.Band || e.From == e.To {
			continue
		}
		if _, ok := rank[e.From]; !ok {
			continue
		}
		if _, ok := rank[e.To]; !ok {
			continue
		}
		live = append(live, e)
	}
	// In a DAG the longest path is at most n-1 edges, so this clamp is a no-op
	// on every well-formed manifest. It binds only inside an UNFLAGGED cycle,
	// where relaxation would otherwise keep pushing ranks up and lay the
	// component out across a hundred columns of empty stage.
	cap := len(nodes) - 1
	if cap < 0 {
		cap = 0
	}
	for pass := 0; pass < len(nodes); pass++ {
		changed := false
		// `live` preserves the caller's edge order, so relaxation visits edges
		// in a fixed sequence. Iterating a map here would make the intermediate
		// ranks — and, inside a clamped cycle, the final ones — depend on Go's
		// randomized map order.
		for _, e := range live {
			want := rank[e.From] + 1
			if want > cap {
				want = cap
			}
			if rank[e.To] < want {
				rank[e.To] = want
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return rank
}

// adjacency builds the predecessor and successor lists the barycenter sweeps
// read. Built from the edge slice in order — the lists are iterated during
// layout, so their order is part of the determinism contract.
func adjacency(nodes []LayoutNode, edges []LayoutEdge) (preds, succs map[string][]string) {
	preds = make(map[string][]string, len(nodes))
	succs = make(map[string][]string, len(nodes))
	known := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		known[n.ID] = true
	}
	for _, e := range edges {
		if !known[e.From] || !known[e.To] || e.From == e.To {
			continue
		}
		preds[e.To] = append(preds[e.To], e.From)
		succs[e.From] = append(succs[e.From], e.To)
	}
	return preds, succs
}
