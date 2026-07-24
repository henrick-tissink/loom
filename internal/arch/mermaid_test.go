package arch

import (
	"fmt"
	"strings"
	"testing"
)

// Every test here exists to pin one of the two halves of §6: the subset parses
// to the expected graph, and EVERYTHING else falls back to source rather than
// drawing a picture that is subtly wrong.

func edgeStr(e DiagramEdge) string {
	s := e.From + "->" + e.To + ":" + e.Style
	if e.Label != "" {
		s += "|" + e.Label
	}
	if e.Cycle {
		s += "*"
	}
	return s
}

func nodeStr(n DiagramNode) string {
	s := n.ID + "[" + n.Label + "]:" + n.Shape
	if n.Subgraph != "" {
		s += "@" + n.Subgraph
	}
	return s
}

func TestParseMermaidSupportedSubset(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		kind  string
		dir   string
		nodes []string
		edges []string
	}{
		{
			name:  "bare graph defaults to TD",
			src:   "graph\nA-->B",
			kind:  "graph",
			dir:   "TD",
			nodes: []string{"A[A]:rect", "B[B]:rect"},
			edges: []string{"A->B:arrow"},
		},
		{
			name:  "flowchart LR",
			src:   "flowchart LR\n  A --> B",
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[A]:rect", "B[B]:rect"},
			edges: []string{"A->B:arrow"},
		},
		{
			name:  "direction TB",
			src:   "flowchart TB\nA --> B",
			kind:  "flowchart",
			dir:   "TB",
			nodes: []string{"A[A]:rect", "B[B]:rect"},
			edges: []string{"A->B:arrow"},
		},
		{
			name:  "direction RL",
			src:   "graph RL\nA --> B",
			kind:  "graph",
			dir:   "RL",
			nodes: []string{"A[A]:rect", "B[B]:rect"},
			edges: []string{"A->B:arrow"},
		},
		{
			name:  "header with trailing statement on the same line",
			src:   "graph TD; A-->B;",
			kind:  "graph",
			dir:   "TD",
			nodes: []string{"A[A]:rect", "B[B]:rect"},
			edges: []string{"A->B:arrow"},
		},
		{
			name: "all four node shapes",
			src: `flowchart LR
  a[Rect]
  b(Round)
  c{Diamond}
  d([Stadium])`,
			kind: "flowchart",
			dir:  "LR",
			nodes: []string{
				"a[Rect]:rect", "b[Round]:round",
				"c[Diamond]:diamond", "d[Stadium]:stadium",
			},
		},
		{
			name: "all four edge styles",
			src: `flowchart LR
  A --> B
  B --- C
  C -.-> D
  D ==> E`,
			kind: "flowchart",
			dir:  "LR",
			nodes: []string{
				"A[A]:rect", "B[B]:rect", "C[C]:rect", "D[D]:rect", "E[E]:rect",
			},
			edges: []string{
				"A->B:arrow", "B->C:open", "C->D:dotted", "D->E:thick",
			},
		},
		{
			name: "edge labels on every style",
			src: `flowchart LR
  A -->|yes| B
  B ---|plain| C
  C -.->|maybe| D
  D ==>|bulk| E`,
			kind: "flowchart",
			dir:  "LR",
			nodes: []string{
				"A[A]:rect", "B[B]:rect", "C[C]:rect", "D[D]:rect", "E[E]:rect",
			},
			edges: []string{
				"A->B:arrow|yes", "B->C:open|plain",
				"C->D:dotted|maybe", "D->E:thick|bulk",
			},
		},
		{
			name:  "label pipe separated from operator by a space",
			src:   "flowchart LR\nA --> |go| B",
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[A]:rect", "B[B]:rect"},
			edges: []string{"A->B:arrow|go"},
		},
		{
			name:  "quoted edge label",
			src:   `flowchart LR` + "\n" + `A -->|"a, b"| B`,
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[A]:rect", "B[B]:rect"},
			edges: []string{"A->B:arrow|a, b"},
		},
		{
			name:  "multi-hop chain",
			src:   "flowchart LR\nA --> B --> C",
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[A]:rect", "B[B]:rect", "C[C]:rect"},
			edges: []string{"A->B:arrow", "B->C:arrow"},
		},
		{
			name:  "no whitespace around the operator",
			src:   "flowchart LR\nA-->B-.->C",
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[A]:rect", "B[B]:rect", "C[C]:rect"},
			edges: []string{"A->B:arrow", "B->C:dotted"},
		},
		{
			name:  "hyphenated ids survive the operator scan",
			src:   "flowchart LR\nmy-node --> other-node",
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"my-node[my-node]:rect", "other-node[other-node]:rect"},
			edges: []string{"my-node->other-node:arrow"},
		},
		{
			name:  "extra dashes are only a length hint",
			src:   "flowchart LR\nA ----> B\nB ----- C",
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[A]:rect", "B[B]:rect", "C[C]:rect"},
			edges: []string{"A->B:arrow", "B->C:open"},
		},
		{
			name:  "quoted node label containing a bracket",
			src:   `flowchart LR` + "\n" + `A["a] b"] --> B`,
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[a] b]:rect", "B[B]:rect"},
			edges: []string{"A->B:arrow"},
		},
		{
			name: "comments are stripped",
			src: `%% leading note
flowchart LR
  %% a whole-line comment
  A --> B %% a trailing one`,
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[A]:rect", "B[B]:rect"},
			edges: []string{"A->B:arrow"},
		},
		{
			name:  "percent inside a label is not a comment",
			src:   "flowchart LR\nA[100%% done] --> B",
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[100%% done]:rect", "B[B]:rect"},
			edges: []string{"A->B:arrow"},
		},
		{
			name: "subgraph membership by first appearance",
			src: `flowchart LR
  A --> B
  subgraph svc[Service]
    B --> C
  end
  C --> D`,
			kind: "flowchart",
			dir:  "LR",
			nodes: []string{
				"A[A]:rect", "B[B]:rect", "C[C]:rect@svc", "D[D]:rect",
			},
			edges: []string{"A->B:arrow", "B->C:arrow", "C->D:arrow"},
		},
		{
			name: "bare subgraph title is its own id",
			src: `flowchart LR
  subgraph Back End
    A --> B
  end`,
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[A]:rect@Back End", "B[B]:rect@Back End"},
			edges: []string{"A->B:arrow"},
		},
		{
			name: "a later label upgrades a bare first reference",
			src: `flowchart LR
  A --> B
  B[Real name]`,
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[A]:rect", "B[Real name]:rect"},
			edges: []string{"A->B:arrow"},
		},
		{
			name:  "semicolon-separated statements on one line",
			src:   "flowchart LR\nA-->B; B-->C",
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[A]:rect", "B[B]:rect", "C[C]:rect"},
			edges: []string{"A->B:arrow", "B->C:arrow"},
		},
		{
			name:  "isolated node with no edges",
			src:   "flowchart LR\nA[Lonely]",
			kind:  "flowchart",
			dir:   "LR",
			nodes: []string{"A[Lonely]:rect"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ParseMermaid(tc.src)
			if d.Status != DiagramOK {
				t.Fatalf("status = %q (%s, line %d), want ok", d.Status, d.Reason, d.Line)
			}
			if d.Kind != tc.kind || d.Direction != tc.dir {
				t.Errorf("kind/dir = %q/%q, want %q/%q", d.Kind, d.Direction, tc.kind, tc.dir)
			}
			var gotN []string
			for _, n := range d.Nodes {
				gotN = append(gotN, nodeStr(n))
			}
			if strings.Join(gotN, " ") != strings.Join(tc.nodes, " ") {
				t.Errorf("nodes = %v, want %v", gotN, tc.nodes)
			}
			var gotE []string
			for _, e := range d.Edges {
				gotE = append(gotE, edgeStr(e))
			}
			if strings.Join(gotE, " ") != strings.Join(tc.edges, " ") {
				t.Errorf("edges = %v, want %v", gotE, tc.edges)
			}
			if len(d.Placements) != len(d.Nodes) {
				t.Errorf("placements = %d, want %d", len(d.Placements), len(d.Nodes))
			}
			if d.Note() != "" {
				t.Errorf("a drawn diagram must carry no chip, got %q", d.Note())
			}
		})
	}
}

// A parsed diagram must never leave a node unplaced: the frontend keys the SVG
// by placement id, so a missing one is an invisible node.
func TestParseMermaidPlacesEveryNode(t *testing.T) {
	d := ParseMermaid("flowchart LR\nA-->B\nB-->C\nA-->C\nD")
	if d.Status != DiagramOK {
		t.Fatalf("status = %q", d.Status)
	}
	seen := map[string]bool{}
	for _, p := range d.Placements {
		seen[p.ID] = true
	}
	for _, n := range d.Nodes {
		if !seen[n.ID] {
			t.Errorf("node %q has no placement", n.ID)
		}
	}
}

func TestParseMermaidUnsupported(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		kind    string
		wantSub string // substring the chip must name
	}{
		{"sequence diagram", "sequenceDiagram\n  A->>B: hi", "sequenceDiagram", "sequenceDiagram"},
		{"class diagram", "classDiagram\n  Animal <|-- Duck", "classDiagram", "classDiagram"},
		{"state diagram", "stateDiagram-v2\n  [*] --> Still", "stateDiagram-v2", "stateDiagram-v2"},
		{"er diagram", "erDiagram\n  A ||--o{ B : has", "erDiagram", "erDiagram"},
		{"gantt", "gantt\n  title A", "gantt", "gantt"},
		{"pie", "pie\n  \"a\" : 10", "pie", "pie"},
		{"journey", "journey\n  title T", "journey", "journey"},
		{"mindmap", "mindmap\n  root", "mindmap", "mindmap"},
		{"gitGraph", "gitGraph\n  commit", "gitGraph", "gitGraph"},
		{"timeline", "timeline\n  title T", "timeline", "timeline"},
		{"quadrantChart", "quadrantChart\n  title T", "quadrantChart", "quadrantChart"},
		{"requirementDiagram", "requirementDiagram\n  requirement r {}", "requirementDiagram", "requirementDiagram"},
		{"C4Context", "C4Context\n  title T", "C4Context", "C4Context"},
		{"unknown diagram type", "warpDrive TD\n  A --> B", "warpDrive", "warpDrive"},

		{"unsupported direction BT", "flowchart BT\n  A --> B", "flowchart BT", "flowchart BT"},

		{"style directive", "flowchart LR\nA-->B\nstyle A fill:#f9f", "style", "style"},
		{"classDef directive", "flowchart LR\nA-->B\nclassDef big font-size:20px", "classdef", "classdef"},
		{"class assignment statement", "flowchart LR\nA-->B\nclass A big", "class", "class"},
		{"click handler", "flowchart LR\nA-->B\nclick A \"http://x\"", "click", "click"},
		{"linkStyle", "flowchart LR\nA-->B\nlinkStyle 0 stroke:red", "linkstyle", "linkstyle"},
		{"per-subgraph direction", "flowchart LR\nsubgraph s\ndirection TB\nA-->B\nend", "direction", "direction"},
		{"init directive", "%%{init: {'theme':'dark'}}%%\nflowchart LR\nA-->B", "%%{init}%%", "init"},

		{"inline class suffix", "flowchart LR\nA:::big --> B", "", ":::"},
		{"node metadata", "flowchart LR\nA@{ shape: circle } --> B", "", "@{"},
		{"hexagon node", "flowchart LR\nA{{Hex}} --> B", "", "{{"},
		{"circle node", "flowchart LR\nA((Circle)) --> B", "", "(("},
		{"subroutine node", "flowchart LR\nA[[Sub]] --> B", "", "[["},
		{"cylinder node", "flowchart LR\nA[(DB)] --> B", "", "[("},
		{"parallelogram node", "flowchart LR\nA[/Para/] --> B", "", "[/"},
		{"asymmetric node", "flowchart LR\nA>Flag] --> B", "", ">"},

		{"ampersand fan-out", "flowchart LR\nA & B --> C", "&", "&"},
		{"circle link ending", "flowchart LR\nA --o B", "", "--o"},
		{"cross link ending", "flowchart LR\nA --x B", "", "--x"},
		{"bidirectional link", "flowchart LR\nA <--> B", "", "<--"},
		{"dotted open link", "flowchart LR\nA -.- B", "", "-.-"},
		{"thick open link", "flowchart LR\nA === B", "", "==="},
		{"nested subgraph", "flowchart LR\nsubgraph a\nsubgraph b\nA-->B\nend\nend", "nested subgraph", "nested"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ParseMermaid(tc.src)
			if d.Status != DiagramUnsupported {
				t.Fatalf("status = %q (%s), want unsupported", d.Status, d.Reason)
			}
			if tc.kind != "" && d.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", d.Kind, tc.kind)
			}
			// The whole point of "unsupported" over "error": nothing is drawn.
			if len(d.Placements) != 0 || d.Width != 0 || d.Height != 0 {
				t.Errorf("unsupported diagram must not carry a layout, got %d placements %dx%d",
					len(d.Placements), d.Width, d.Height)
			}
			note := d.Note()
			if !strings.Contains(note, "shown as source") {
				t.Errorf("note %q must say the fence is shown as source", note)
			}
			if !strings.Contains(note+" "+d.Reason, tc.wantSub) {
				t.Errorf("note %q / reason %q must name %q", note, d.Reason, tc.wantSub)
			}
		})
	}
}

func TestParseMermaidMalformed(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		wantLine int
		wantSub  string
	}{
		{"empty fence", "", 1, "empty"},
		{"only comments", "%% nothing here\n%% still nothing", 1, "empty"},
		{"header with no nodes", "flowchart LR", 1, "no nodes"},
		{"header with only blank lines", "flowchart LR\n\n   \n", 1, "no nodes"},
		{"unterminated node label", "flowchart LR\nA[Broken --> B", 2, "unterminated"},
		{"unterminated quoted label", "flowchart LR\nA[\"Broken] --> B", 2, "unterminated"},
		{"unterminated edge label", "flowchart LR\nA -->|oops B", 2, "unterminated"},
		{"link with no target", "flowchart LR\nA -->", 2, "no target"},
		{"two nodes with no link", "flowchart LR\nA B", 2, "link operator"},
		{"statement starting with punctuation", "flowchart LR\n--> B", 2, "node id"},
		{"end without subgraph", "flowchart LR\nA-->B\nend", 3, "without an open subgraph"},
		{"unclosed subgraph", "flowchart LR\nsubgraph s\nA-->B", 2, "never closed"},
		{"subgraph with no id", "flowchart LR\nsubgraph\nA-->B\nend", 2, "no id or title"},
		{"error on a later line reports that line", "flowchart LR\nA-->B\nB-->C\nC-->", 4, "no target"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ParseMermaid(tc.src)
			if d.Status != DiagramError {
				t.Fatalf("status = %q (%s), want error", d.Status, d.Reason)
			}
			if d.Line != tc.wantLine {
				t.Errorf("line = %d, want %d (reason %q)", d.Line, tc.wantLine, d.Reason)
			}
			if !strings.Contains(d.Reason, tc.wantSub) {
				t.Errorf("reason %q must contain %q", d.Reason, tc.wantSub)
			}
			if len(d.Placements) != 0 {
				t.Error("a malformed diagram must not carry a layout")
			}
			if n := d.Note(); !strings.Contains(n, "shown as source") ||
				!strings.Contains(n, fmt.Sprintf("line %d", tc.wantLine)) {
				t.Errorf("note %q must say shown-as-source and name the line", n)
			}
		})
	}
}

// Garbage in must still produce a fallback, never a panic and never a partial
// picture. These are inputs no grammar covers.
func TestParseMermaidNeverPanicsOrHalfDraws(t *testing.T) {
	inputs := []string{
		"",
		"\n\n\n",
		"flowchart",
		"flowchart LR RL TD",
		"flowchart LR\n[[[[[",
		"flowchart LR\nA[",
		"flowchart LR\nA]",
		"flowchart LR\n|||",
		"flowchart LR\n-->",
		"flowchart LR\n;;;;",
		"flowchart LR\nA --> B -->",
		"flowchart LR\nA(((",
		"flowchart LR\nsubgraph [",
		"flowchart LR\n\"\"\"",
		"%%",
		"%%{",
		strings.Repeat("-", 500),
		"flowchart LR\n" + strings.Repeat("A-->B\n", 5000),
	}
	for i, src := range inputs {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			d := ParseMermaid(src)
			switch d.Status {
			case DiagramOK:
				if len(d.Placements) != len(d.Nodes) {
					t.Fatalf("ok status with %d nodes but %d placements", len(d.Nodes), len(d.Placements))
				}
			case DiagramUnsupported, DiagramError:
				if len(d.Placements) != 0 {
					t.Fatalf("%s status carries %d placements", d.Status, len(d.Placements))
				}
				if d.Note() == "" {
					t.Fatal("a fallback with no user-visible note is a silent degradation")
				}
			default:
				t.Fatalf("unknown status %q", d.Status)
			}
		})
	}
}

// The node cap is a real ceiling, not a comment: past it the fence falls back
// rather than laying out a wall of cards.
func TestParseMermaidNodeCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	for i := 0; i < maxDiagramNodes; i++ {
		fmt.Fprintf(&b, "n%d\n", i)
	}
	if d := ParseMermaid(b.String()); d.Status != DiagramOK {
		t.Fatalf("at the cap: status = %q (%s)", d.Status, d.Reason)
	}
	fmt.Fprintf(&b, "n%d\n", maxDiagramNodes)
	d := ParseMermaid(b.String())
	if d.Status != DiagramUnsupported {
		t.Fatalf("one over the cap: status = %q, want unsupported", d.Status)
	}
	if !strings.Contains(d.Kind, "201 nodes") {
		t.Errorf("chip %q must name the node count", d.Kind)
	}
}

// A cycle must lay out, not hang, and the back edge must be flagged so it is
// excluded from ranking exactly as a delegation `park` edge is.
func TestParseMermaidCycles(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		cycle []string
	}{
		{"two cycle", "flowchart LR\nA-->B\nB-->A", []string{"B->A:arrow*"}},
		{"three cycle", "flowchart LR\nA-->B\nB-->C\nC-->A", []string{"C->A:arrow*"}},
		{"self loop", "flowchart LR\nA-->A", []string{"A->A:arrow*"}},
		{"cycle beside a clean chain", "flowchart LR\nA-->B\nB-->A\nX-->Y", []string{"B->A:arrow*"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ParseMermaid(tc.src)
			if d.Status != DiagramOK {
				t.Fatalf("status = %q (%s)", d.Status, d.Reason)
			}
			var got []string
			for _, e := range d.Edges {
				if e.Cycle {
					got = append(got, edgeStr(e))
				}
			}
			if strings.Join(got, " ") != strings.Join(tc.cycle, " ") {
				t.Errorf("cycle edges = %v, want %v", got, tc.cycle)
			}
			if len(d.Placements) != len(d.Nodes) {
				t.Errorf("placements = %d, want %d", len(d.Placements), len(d.Nodes))
			}
		})
	}
}

// GOLDEN. A representative diagram's exact coordinates, in every direction the
// subset supports. This is the test that fails if the parser's node ORDER, the
// layout's ranks or the orientation transform ever drift — all three of which
// are invisible in a status-only assertion.
func TestParseMermaidGoldenLayout(t *testing.T) {
	const src = `%% representative: fan-out, join, labelled edges
  api[API] --> auth{Auth?}
  auth -->|yes| store([Store])
  auth -->|no| deny(Deny)
  store --> done[Done]
  deny --> done
`

	tests := []struct {
		dir  string
		want string
		w, h int
	}{
		{
			dir: "LR",
			want: "api@0,43,r0 auth@312,43,r1 " +
				"deny@624,0,r2 done@936,43,r3 store@624,86,r2",
			w: 1160, h: 144,
		},
		{
			// Same ranks, mirrored on x. RL must not re-rank: reversing the
			// reading direction is a paint decision, not a topology one.
			dir: "RL",
			want: "api@936,43,r0 auth@624,43,r1 " +
				"deny@312,0,r2 done@0,43,r3 store@312,86,r2",
			w: 1160, h: 144,
		},
		{
			// Ranks now run DOWN the page and step by row height, and
			// within-rank position steps by card width — the transform that
			// exists because a plain x/y swap would overlap every row.
			dir: "TD",
			want: "api@156,0,r0 auth@156,86,r1 " +
				"deny@0,172,r2 done@156,258,r3 store@312,172,r2",
			w: 536, h: 316,
		},
	}
	for _, tc := range tests {
		t.Run(tc.dir, func(t *testing.T) {
			d := ParseMermaid("flowchart " + tc.dir + "\n" + src)
			if d.Status != DiagramOK {
				t.Fatalf("status = %q (%s, line %d)", d.Status, d.Reason, d.Line)
			}
			var parts []string
			for _, p := range d.Placements {
				parts = append(parts, fmt.Sprintf("%s@%d,%d,r%d", p.ID, p.X, p.Y, p.Rank))
			}
			got := strings.Join(parts, " ")
			if got != tc.want {
				t.Errorf("placements =\n  %s\nwant\n  %s", got, tc.want)
			}
			if d.Width != tc.w || d.Height != tc.h {
				t.Errorf("size = %dx%d, want %dx%d", d.Width, d.Height, tc.w, tc.h)
			}
		})
	}
}

// Determinism across repeated runs. §12 binds byte-identical coordinates; the
// parser is the layer that decides node ORDER, and node order is the layout's
// column seed, so the property has to be asserted from the text down.
func TestParseMermaidDeterminism(t *testing.T) {
	const src = `flowchart TD
  a --> b
  a --> c
  b --> d
  c --> d
  d --> e
  subgraph grp[Group]
    f --> g
  end
  g --> e
  e -.-> a`

	first := ParseMermaid(src)
	if first.Status != DiagramOK {
		t.Fatalf("status = %q (%s)", first.Status, first.Reason)
	}
	render := func(d Diagram) string {
		var b strings.Builder
		for _, n := range d.Nodes {
			b.WriteString(nodeStr(n) + " ")
		}
		for _, e := range d.Edges {
			b.WriteString(edgeStr(e) + " ")
		}
		for _, p := range d.Placements {
			fmt.Fprintf(&b, "%s@%d,%d,r%d ", p.ID, p.X, p.Y, p.Rank)
		}
		fmt.Fprintf(&b, "%dx%d", d.Width, d.Height)
		return b.String()
	}
	want := render(first)
	for i := 0; i < 100; i++ {
		if got := render(ParseMermaid(src)); got != want {
			t.Fatalf("run %d differs:\n got %s\nwant %s", i, got, want)
		}
	}
}

// The markdown layer must reach the parser, and the fallback must stay a
// SOURCE block in every non-ok case — that is the permanent graceful fallback
// §6 allowed 4d to be cut for, so it has to survive 4d shipping.
func TestMarkdownMermaidFence(t *testing.T) {
	tests := []struct {
		name       string
		md         string
		wantStatus DiagramStatus
		wantNote   string
		wantText   string
	}{
		{
			name:       "supported subset draws and carries no chip",
			md:         "```mermaid\nflowchart LR\nA-->B\n```",
			wantStatus: DiagramOK,
			wantNote:   "",
			wantText:   "flowchart LR\nA-->B",
		},
		{
			name:       "unsupported type names itself",
			md:         "```mermaid\nsequenceDiagram\nA->>B: hi\n```",
			wantStatus: DiagramUnsupported,
			wantNote:   "mermaid `sequenceDiagram` — shown as source",
			wantText:   "sequenceDiagram\nA->>B: hi",
		},
		{
			name:       "malformed names the line",
			md:         "```mermaid\nflowchart LR\nA -->\n```",
			wantStatus: DiagramError,
			wantNote:   "mermaid — shown as source; line 2: link has no target node",
			wantText:   "flowchart LR\nA -->",
		},
		{
			name:       "unterminated fence is never parsed",
			md:         "```mermaid\nflowchart LR\nA-->B\n",
			wantStatus: DiagramError,
			wantNote:   "mermaid — shown as source; unterminated code fence",
			wantText:   "flowchart LR\nA-->B",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			blocks := Render(tc.md)
			var code *Block
			for i := range blocks {
				if blocks[i].Kind == BlockCode {
					code = &blocks[i]
					break
				}
			}
			if code == nil {
				t.Fatal("no code block produced")
			}
			if code.Text != tc.wantText {
				t.Errorf("source text = %q, want %q", code.Text, tc.wantText)
			}
			if code.Diagram == nil {
				t.Fatal("mermaid fence produced no Diagram")
			}
			if code.Diagram.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", code.Diagram.Status, tc.wantStatus)
			}
			if code.Note != tc.wantNote {
				t.Errorf("note = %q, want %q", code.Note, tc.wantNote)
			}
		})
	}

	// A non-mermaid fence must not gain a Diagram — the field's nil-ness is how
	// the renderer tells "this is a diagram candidate" from "this is code".
	for _, b := range Render("```go\nfunc main() {}\n```") {
		if b.Kind == BlockCode && b.Diagram != nil {
			t.Error("a go fence must not carry a Diagram")
		}
	}
}
