package arch

import (
	"fmt"
	"sort"
	"strings"
)

// A mermaid SUBSET parser (orchestration-view §6), feeding the SAME layout
// engine as the delegation graph (layout.go). One layout implementation, two
// callers — this file produces `Layout`'s input and never computes a
// coordinate of its own beyond re-orienting the result.
//
// §6 is binding that mermaid.js is not vendored, so the honest consequence is
// that most of mermaid is not understood here. The design that follows from
// that: EVERY input produces a Diagram, never an error, and a Diagram that is
// not `DiagramOK` carries the reason the user needs to see. There are exactly
// three outcomes and the difference between the last two is the message, not
// the fallback:
//
//   - ok          — parsed; Nodes/Edges/Placements are populated and drawable.
//   - unsupported — valid mermaid outside the subset (a sequence diagram, a
//     hexagon node, `classDef`). Renders as source with a chip naming the
//     construct. This is a legible outcome, not a failure.
//   - error       — inside the subset and malformed. Renders as source plus the
//     line number and the parse message.
//
// The rule that decides every ambiguous case: a half-drawn diagram is worse
// than no diagram. Anything this parser is not certain it understood becomes a
// fallback, because the fence is still shown verbatim either way and a source
// block is never *wrong* — whereas silently dropping an `&` fan-out or reading
// `A --o B` as a plain arrow would draw a picture that lies about the system.
// The rejected alternative was best-effort recovery (parse what you can, skip
// what you cannot), which is exactly how you get a diagram missing one edge
// that nobody notices.

// DiagramStatus is which of the three outcomes above applies.
type DiagramStatus string

const (
	DiagramOK          DiagramStatus = "ok"
	DiagramUnsupported DiagramStatus = "unsupported"
	DiagramError       DiagramStatus = "error"
)

// Node shapes in the subset. The wire values are the names the frontend paints
// with; they are not mermaid's own vocabulary because mermaid's is much larger
// than what is drawable here, and a shape string the painter does not know
// would silently become a rectangle.
const (
	ShapeRect    = "rect"
	ShapeRound   = "round"
	ShapeDiamond = "diamond"
	ShapeStadium = "stadium"
)

// Edge line styles in the subset: `-->`, `---`, `-.->`, `==>`.
const (
	EdgeArrow  = "arrow"  // -->
	EdgeOpen   = "open"   // ---
	EdgeDotted = "dotted" // -.->
	EdgeThick  = "thick"  // ==>
)

// maxDiagramNodes matches the view spec's ">200 nodes degrades" ceiling. A
// document diagram past it is not a diagram, it is a data dump; source is the
// more useful rendering and it costs no layout time.
const maxDiagramNodes = 200

// DiagramNode is one declared or referenced node, in FIRST-APPEARANCE order.
// Appearance order is load-bearing: layout.go seeds its columns from the input
// slice, so this order is what keeps a hand-written mermaid file laying out the
// way its author wrote it instead of alphabetically.
type DiagramNode struct {
	ID       string
	Label    string
	Shape    string
	Subgraph string // subgraph id, or "" — assigned on first appearance
}

// DiagramEdge is one link, in source order.
type DiagramEdge struct {
	From, To string
	Label    string
	Style    string
	Cycle    bool // back edge; excluded from ranking, still drawn
}

// DiagramSubgraph is one `subgraph … end` band. One nesting level only (§6);
// a nested subgraph is unsupported rather than flattened, because flattening
// draws a band around the wrong set of nodes.
type DiagramSubgraph struct {
	ID    string
	Title string
	Nodes []string
}

// Diagram is the whole result. It is always returned, whatever the input.
type Diagram struct {
	Status DiagramStatus

	// Kind is the construct being named in the chip: "flowchart"/"graph" when
	// parsed, and for `unsupported` the thing that was not understood —
	// "sequenceDiagram", "flowchart BT", "classDef", "A --o B".
	Kind      string
	Direction string // TD, TB, LR, RL — normalised, empty only on failure

	// Reason is the bare human message; Line is 1-based within the fence body
	// (0 when the reason is not about a particular line). Compose the chip with
	// Note().
	Reason string
	Line   int

	Nodes     []DiagramNode
	Edges     []DiagramEdge
	Subgraphs []DiagramSubgraph

	// Placements is layout.go's output, already oriented for Direction. Width
	// and Height bound the stage. Empty unless Status is DiagramOK.
	Placements    []Placement
	Width, Height int
}

// Note is the exact user-visible chip text. Built here rather than in the
// frontend so the two fallback shapes read identically everywhere and so the
// string is covered by this package's tests.
func (d Diagram) Note() string {
	switch d.Status {
	case DiagramOK:
		return ""
	case DiagramUnsupported:
		if d.Kind == "" {
			return "mermaid — shown as source: " + d.Reason
		}
		return fmt.Sprintf("mermaid `%s` — shown as source", d.Kind)
	default:
		if d.Line > 0 {
			return fmt.Sprintf("mermaid — shown as source; line %d: %s", d.Line, d.Reason)
		}
		return "mermaid — shown as source; " + d.Reason
	}
}

// unsupportedDiagram / errDiagram keep every failure exit one line long, which
// is the only way a parser this branchy stays honest about always returning a
// complete result.
func unsupportedDiagram(kind, reason string, line int) Diagram {
	return Diagram{Status: DiagramUnsupported, Kind: kind, Reason: reason, Line: line}
}

func errDiagram(reason string, line int) Diagram {
	return Diagram{Status: DiagramError, Reason: reason, Line: line}
}

// knownDiagramKinds are the mermaid diagram types deliberately outside the
// subset (§6). Listed explicitly so the chip can NAME the type — "mermaid
// `sequenceDiagram` — shown as source" tells the user the file is fine and
// Loom simply does not draw that; a generic "unsupported" does not.
//
// The list is a courtesy, not a gate: an unrecognised header is also
// unsupported, named by its own first word.
var knownDiagramKinds = map[string]bool{
	"sequencediagram":    true,
	"classdiagram":       true,
	"classdiagram-v2":    true,
	"statediagram":       true,
	"statediagram-v2":    true,
	"erdiagram":          true,
	"journey":            true,
	"gantt":              true,
	"pie":                true,
	"quadrantchart":      true,
	"requirementdiagram": true,
	"gitgraph":           true,
	"mindmap":            true,
	"timeline":           true,
	"zenuml":             true,
	"sankey-beta":        true,
	"xychart-beta":       true,
	"block-beta":         true,
	"packet-beta":        true,
	"c4context":          true,
	"c4container":        true,
	"c4component":        true,
	"c4dynamic":          true,
	"c4deployment":       true,
}

// statementKeywords are flowchart statements that are valid mermaid and change
// what the picture MEANS, so they cannot be skipped: styling and interaction
// directives, and per-subgraph direction. Meeting one falls the whole fence
// back to source.
var statementKeywords = map[string]bool{
	"style":     true,
	"classdef":  true,
	"class":     true,
	"click":     true,
	"linkstyle": true,
	"direction": true,
	"accdescr":  true,
	"acctitle":  true,
}

// ParseMermaid turns a fence body into a Diagram. It never returns an error and
// never panics on malformed input — the fence is shown as source in every
// non-ok case, so the only job of a failure path is to say WHY.
func ParseMermaid(src string) Diagram {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")

	// Header. Everything before it is blank or comment; the first real line
	// decides whether this fence is even a candidate for the subset.
	//
	// `graph TD; A-->B` is a real and common spelling, so the header line is
	// split on `;` and only its FIRST part is the header — the rest are
	// ordinary statements that happened to share a line.
	hdr, hdrLine := "", 0
	var hdrTail []string
	for i, raw := range lines {
		l := stripComment(raw)
		if strings.TrimSpace(l) == "" {
			if strings.HasPrefix(strings.TrimSpace(raw), "%%{") {
				// An init directive re-themes or re-configures the renderer.
				// Honouring it is out of scope and ignoring it would draw a
				// diagram the author explicitly configured away from.
				return unsupportedDiagram("%%{init}%%", "mermaid init directive", i+1)
			}
			continue
		}
		parts := splitOutsideBrackets(l, ';')
		hdr, hdrLine, hdrTail = strings.TrimSpace(parts[0]), i+1, parts[1:]
		break
	}
	if hdr == "" {
		return errDiagram("empty mermaid block", 1)
	}

	kind, dir, ok := parseHeader(hdr)
	if !ok {
		word := strings.TrimSpace(strings.SplitN(hdr, " ", 2)[0])
		word = strings.TrimSuffix(word, ";")
		if knownDiagramKinds[strings.ToLower(word)] {
			return unsupportedDiagram(word, "diagram type outside the supported subset", hdrLine)
		}
		if kind != "" {
			// A flowchart/graph header we recognised but a direction we do not
			// lay out (BT, LR_, …). Named in full so the chip is actionable.
			return unsupportedDiagram(hdr, "unsupported flowchart direction", hdrLine)
		}
		return unsupportedDiagram(word, "unrecognised mermaid diagram type", hdrLine)
	}

	p := &mermaidParser{kind: kind, dir: dir}
	for _, stmt := range hdrTail {
		if stmt = strings.TrimSpace(stmt); stmt == "" {
			continue
		}
		if d, bad := p.statement(stmt, hdrLine); bad {
			return d
		}
	}
	if d, bad := p.body(lines, hdrLine); bad {
		return d
	}
	if p.open != "" {
		return errDiagram("subgraph `"+p.open+"` is never closed", p.openLine)
	}
	if len(p.nodes) == 0 {
		return errDiagram("flowchart declares no nodes", hdrLine)
	}
	if len(p.nodes) > maxDiagramNodes {
		return unsupportedDiagram(
			fmt.Sprintf("%s with %d nodes", kind, len(p.nodes)),
			fmt.Sprintf("over the %d-node diagram limit", maxDiagramNodes), 0)
	}

	d := Diagram{
		Status:    DiagramOK,
		Kind:      kind,
		Direction: dir,
		Nodes:     p.nodes,
		Edges:     p.edges,
		Subgraphs: p.subgraphs,
	}
	markCycles(d.Nodes, d.Edges)

	ln := make([]LayoutNode, len(d.Nodes))
	for i, n := range d.Nodes {
		ln[i] = LayoutNode{ID: n.ID}
	}
	le := make([]LayoutEdge, len(d.Edges))
	for i, e := range d.Edges {
		le[i] = LayoutEdge{From: e.From, To: e.To, Cycle: e.Cycle}
	}
	placed, w, h := Layout(ln, le)
	d.Placements, d.Width, d.Height = orient(dir, placed, w, h)
	return d
}

// parseHeader accepts `flowchart`/`graph` with an optional direction. kind is
// returned even when ok is false so the caller can tell "wrong direction" from
// "wrong diagram type" — two different chips.
func parseHeader(hdr string) (kind, dir string, ok bool) {
	hdr = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(hdr), ";"))
	f := strings.Fields(hdr)
	if len(f) == 0 {
		return "", "", false
	}
	switch strings.ToLower(f[0]) {
	case "flowchart", "graph":
		kind = strings.ToLower(f[0])
	default:
		return "", "", false
	}
	if len(f) == 1 {
		// mermaid's own default for a bare `graph`.
		return kind, "TD", true
	}
	if len(f) > 2 {
		return kind, "", false
	}
	switch strings.ToUpper(f[1]) {
	case "TD", "TB", "LR", "RL":
		return kind, strings.ToUpper(f[1]), true
	}
	return kind, "", false
}

type mermaidParser struct {
	kind, dir string

	nodes []DiagramNode
	index map[string]int
	edges []DiagramEdge

	subgraphs []DiagramSubgraph
	open      string // id of the currently open subgraph, "" at top level
	openLine  int
}

// body walks the statements. It returns (Diagram, true) on the FIRST thing it
// does not understand — there is no error accumulation because there is no
// partial rendering to accumulate errors for.
func (p *mermaidParser) body(lines []string, hdrLine int) (Diagram, bool) {
	for i := hdrLine; i < len(lines); i++ {
		raw := lines[i]
		if strings.HasPrefix(strings.TrimSpace(raw), "%%{") {
			return unsupportedDiagram("%%{init}%%", "mermaid init directive", i+1), true
		}
		// Statements may be `;`-separated on one line; the split is
		// bracket-aware so a label containing `;` survives.
		for _, stmt := range splitOutsideBrackets(stripComment(raw), ';') {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if d, bad := p.statement(stmt, i+1); bad {
				return d, true
			}
		}
	}
	return Diagram{}, false
}

func (p *mermaidParser) statement(stmt string, line int) (Diagram, bool) {
	head := strings.ToLower(strings.SplitN(stmt, " ", 2)[0])
	head = strings.TrimSuffix(head, ":")
	switch {
	case head == "end":
		if p.open == "" {
			return errDiagram("`end` without an open subgraph", line), true
		}
		p.open, p.openLine = "", 0
		return Diagram{}, false
	case head == "subgraph":
		return p.subgraph(stmt, line)
	case statementKeywords[head]:
		return unsupportedDiagram(head, "flowchart directive outside the supported subset", line), true
	}
	return p.chain(stmt, line)
}

func (p *mermaidParser) subgraph(stmt string, line int) (Diagram, bool) {
	if p.open != "" {
		// §6 is explicit: one nesting level. Flattening would draw a band
		// around a set of nodes the author did not group.
		return unsupportedDiagram("nested subgraph", "subgraph nesting deeper than one level", line), true
	}
	rest := strings.TrimSpace(stmt[len("subgraph"):])
	if rest == "" {
		return errDiagram("subgraph has no id or title", line), true
	}
	id, title := rest, rest
	// `subgraph id[Title]` / `subgraph id["Title"]`.
	if b := strings.IndexAny(rest, "[("); b > 0 {
		ref, _, err, unsup := readNodeRef(rest, 0)
		if err != "" {
			if unsup {
				return unsupportedDiagram("subgraph "+rest, err, line), true
			}
			return errDiagram(err, line), true
		}
		id, title = ref.ID, ref.Label
	} else {
		id = strings.Trim(id, `"`)
		title = id
	}
	p.subgraphs = append(p.subgraphs, DiagramSubgraph{ID: id, Title: title})
	p.open, p.openLine = id, line
	return Diagram{}, false
}

// chain parses `A`, `A[Label]`, `A --> B`, `A -->|label| B --> C`. Anything
// left over after the walk is a construct outside the subset, and the leftover
// text goes into the message so the user can see WHICH part lost us.
func (p *mermaidParser) chain(stmt string, line int) (Diagram, bool) {
	i := 0
	ref, n, err, unsup := readNodeRef(stmt, i)
	if err != "" {
		return p.problem(stmt, err, unsup, line), true
	}
	i = n
	p.declare(ref)
	prev := ref.ID

	for {
		j := skipSpace(stmt, i)
		if j >= len(stmt) {
			return Diagram{}, false
		}
		if stmt[j] == '&' {
			// `A & B --> C` fans out; reading it as anything else drops edges.
			return unsupportedDiagram("&", "`&` node lists", line), true
		}
		if stmt[j] != '-' && stmt[j] != '=' && stmt[j] != '<' {
			// Two node references with nothing between them is not a mermaid
			// construct we are declining — it is broken input.
			return errDiagram(fmt.Sprintf("expected a link operator, found %q", string(stmt[j])), line), true
		}
		op, label, k, e, eUnsup := readEdge(stmt, j)
		if e != "" {
			if eUnsup {
				return unsupportedDiagram(strings.TrimSpace(stmt[j:]), e, line), true
			}
			return errDiagram(e, line), true
		}
		i = k
		i = skipSpace(stmt, i)
		if i >= len(stmt) {
			return errDiagram("link has no target node", line), true
		}
		ref, n, err, unsup = readNodeRef(stmt, i)
		if err != "" {
			return p.problem(stmt, err, unsup, line), true
		}
		i = n
		p.declare(ref)
		p.edges = append(p.edges, DiagramEdge{From: prev, To: ref.ID, Label: label, Style: op})
		prev = ref.ID
	}
}

// problem routes a node-reference failure to the right one of the two
// fallbacks, naming the whole statement when the construct is merely outside
// the subset — the statement is what the user has to look at.
func (p *mermaidParser) problem(stmt, msg string, unsup bool, line int) Diagram {
	if unsup {
		return unsupportedDiagram(strings.TrimSpace(stmt), msg, line)
	}
	return errDiagram(msg, line)
}

// declare records first appearance. A later reference may add a label the first
// one lacked (mermaid lets you write `A --> B` then `B[Real name]` further
// down), but never OVERWRITES one — the first spelling wins, so the picture
// does not depend on how far down the file you read.
func (p *mermaidParser) declare(ref nodeRef) {
	if p.index == nil {
		p.index = map[string]int{}
	}
	if at, ok := p.index[ref.ID]; ok {
		if ref.Explicit && !p.nodes[at].explicit() {
			p.nodes[at].Label, p.nodes[at].Shape = ref.Label, ref.Shape
		}
		return
	}
	p.index[ref.ID] = len(p.nodes)
	p.nodes = append(p.nodes, DiagramNode{
		ID: ref.ID, Label: ref.Label, Shape: ref.Shape, Subgraph: p.open,
	})
	if p.open != "" {
		for i := range p.subgraphs {
			if p.subgraphs[i].ID == p.open {
				p.subgraphs[i].Nodes = append(p.subgraphs[i].Nodes, ref.ID)
			}
		}
	}
}

// explicit reports whether this node ever carried a bracketed label. A node
// that only ever appeared bare has Label == ID and the default shape.
func (n DiagramNode) explicit() bool { return n.Label != n.ID || n.Shape != ShapeRect }

type nodeRef struct {
	ID       string
	Label    string
	Shape    string
	Explicit bool
}

// readNodeRef reads `id`, `id[L]`, `id(L)`, `id{L}`, `id([L])` starting at i.
//
// The id scan is where a subset parser most easily draws a WRONG diagram, so
// the dash rule is deliberate: a `-` joins the id only when the next character
// is not one that could begin an edge operator. `my-node --> b` keeps its
// hyphenated id; `A-->B` and `A-.->B` break at the operator. The rejected
// alternative — forbidding `-` in ids — silently split real ids into two nodes
// and an edge, which is exactly the class of lie this parser refuses to tell.
//
// The string result is a problem message; the bool says whether the problem is
// "valid mermaid we do not draw" (unsupported, chip) rather than "malformed"
// (error, red line). Collapsing the two would report a hexagon node as a syntax
// error in a file that has none.
func readNodeRef(s string, i int) (nodeRef, int, string, bool) {
	i = skipSpace(s, i)
	if i >= len(s) {
		return nodeRef{}, i, "expected a node id", false
	}
	if !isIDStart(s[i]) {
		return nodeRef{}, i, fmt.Sprintf("expected a node id, found %q", string(s[i])), false
	}
	start := i
	for i < len(s) {
		c := s[i]
		if c == '-' {
			if i+1 < len(s) && (s[i+1] == '-' || s[i+1] == '.' || s[i+1] == '>' || s[i+1] == '=') {
				break
			}
			i++
			continue
		}
		if !isIDChar(c) {
			break
		}
		i++
	}
	ref := nodeRef{ID: s[start:i]}
	ref.Label, ref.Shape = ref.ID, ShapeRect

	if i < len(s) {
		switch {
		case strings.HasPrefix(s[i:], ":::"):
			return ref, i, "node class assignment `:::`", true
		case s[i] == '@':
			return ref, i, "`@{ … }` node metadata", true
		}
	}

	open, close, shape, bad := shapeAt(s, i)
	if bad != "" {
		return ref, i, bad, true
	}
	if open == "" {
		return ref, i, "", false
	}
	j := i + len(open)
	label, k, err := readLabel(s, j, close)
	if err != "" {
		return ref, i, err, false
	}
	ref.Label, ref.Shape, ref.Explicit = label, shape, true
	return ref, k, "", false
}

// shapeAt classifies the bracket at i. The doubled forms are checked FIRST:
// `A[[x]]` read as `A[` + `[x]` would draw a subroutine box as a rectangle
// whose label starts with a bracket.
func shapeAt(s string, i int) (open, close, shape, bad string) {
	if i >= len(s) {
		return "", "", "", ""
	}
	switch {
	case strings.HasPrefix(s[i:], "(["):
		return "([", "])", ShapeStadium, ""
	case strings.HasPrefix(s[i:], "[["), strings.HasPrefix(s[i:], "[("),
		strings.HasPrefix(s[i:], "[/"), strings.HasPrefix(s[i:], `[\`),
		strings.HasPrefix(s[i:], "(("), strings.HasPrefix(s[i:], "(-"),
		strings.HasPrefix(s[i:], "{{"), strings.HasPrefix(s[i:], ">"):
		end := i + 2
		if end > len(s) {
			end = len(s)
		}
		return "", "", "", "node shape `" + s[i:end] + "` outside the supported subset"
	case s[i] == '[':
		return "[", "]", ShapeRect, ""
	case s[i] == '(':
		return "(", ")", ShapeRound, ""
	case s[i] == '{':
		return "{", "}", ShapeDiamond, ""
	}
	return "", "", "", ""
}

// readLabel reads a label body up to close, honouring one level of double
// quotes so `A["a] b"]` does not terminate at the bracket inside the string.
func readLabel(s string, i int, close string) (string, int, string) {
	if i < len(s) && s[i] == '"' {
		end := strings.IndexByte(s[i+1:], '"')
		if end < 0 {
			return "", i, "unterminated quoted node label"
		}
		lit := s[i+1 : i+1+end]
		rest := i + end + 2
		if !strings.HasPrefix(s[rest:], close) {
			return "", i, "unterminated node label"
		}
		return lit, rest + len(close), ""
	}
	end := strings.Index(s[i:], close)
	if end < 0 {
		return "", i, "unterminated node label"
	}
	return strings.TrimSpace(s[i : i+end]), i + end + len(close), ""
}

// readEdge reads one link operator plus an optional `|label|`.
//
// Extra dashes are accepted (`---->` is mermaid's way of asking for a LONGER
// arrow, not a different one) because length is a hint to a layout engine that
// does not take hints; the topology is identical and refusing it would fall
// back a diagram that is entirely inside the subset. Every operator that
// changes MEANING — `--o`, `--x`, `<-->`, `-.-`, `===` — is unsupported.
func readEdge(s string, i int) (op, label string, next int, bad string, unsup bool) {
	if i >= len(s) {
		return "", "", i, "expected a link", false
	}
	if s[i] == '<' {
		return "", "", i, "bidirectional links", true
	}
	if s[i] != '-' && s[i] != '=' {
		return "", "", i, "expected a link operator", false
	}
	start := i
	for i < len(s) && strings.IndexByte("-.=>", s[i]) >= 0 {
		i++
	}
	tok := s[start:i]
	if i < len(s) && (s[i] == 'o' || s[i] == 'x') && strings.HasSuffix(tok, "-") {
		return "", "", start, "circle/cross link endings", true
	}
	switch {
	case isDashes(tok) && len(tok) >= 3:
		op = EdgeOpen
	case strings.HasSuffix(tok, ">") && isDashes(tok[:len(tok)-1]) && len(tok) >= 3:
		op = EdgeArrow
	case strings.HasPrefix(tok, "-") && strings.HasSuffix(tok, ".->") && isDashes(strings.TrimSuffix(tok, ".->")):
		op = EdgeDotted
	case strings.HasSuffix(tok, ">") && isThick(tok[:len(tok)-1]) && len(tok) >= 3:
		op = EdgeThick
	default:
		return "", "", start, "link operator `" + tok + "` outside the supported subset", true
	}

	j := skipSpace(s, i)
	if j < len(s) && s[j] == '|' {
		end := strings.IndexByte(s[j+1:], '|')
		if end < 0 {
			return "", "", start, "unterminated edge label", false
		}
		label = strings.TrimSpace(strings.Trim(s[j+1:j+1+end], `"`))
		j = j + end + 2
	}
	return op, label, j, "", false
}

func isDashes(s string) bool { return s != "" && strings.Trim(s, "-") == "" }
func isThick(s string) bool  { return s != "" && strings.Trim(s, "=") == "" }

func isIDStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

func isIDChar(c byte) bool { return isIDStart(c) || c == '.' }

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// stripComment removes a `%%` comment. Bracket- and quote-aware so a `%%`
// inside a node label is text, not a comment.
func stripComment(s string) string {
	depth, quoted := 0, false
	for i := 0; i+1 < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case '[', '(', '{':
			if !quoted {
				depth++
			}
		case ']', ')', '}':
			if !quoted && depth > 0 {
				depth--
			}
		case '%':
			if !quoted && depth == 0 && s[i+1] == '%' {
				return s[:i]
			}
		}
	}
	return s
}

// splitOutsideBrackets splits on sep at bracket depth zero and outside quotes.
func splitOutsideBrackets(s string, sep byte) []string {
	var out []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case '[', '(', '{':
			if !quoted {
				depth++
			}
		case ']', ')', '}':
			if !quoted && depth > 0 {
				depth--
			}
		case sep:
			if !quoted && depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// markCycles flags back edges so rankOf can skip them, exactly as the
// delegation graph flags its `park` edges. A mermaid flowchart is allowed to
// cycle and layout.go survives one either way (its relaxation is clamped), but
// an unflagged cycle spreads the component across as many columns as it has
// nodes; flagging keeps the picture tight.
//
// DFS in node-appearance order with the successor lists in edge order, so the
// SET of edges marked is a function of the source text and not of a map walk.
func markCycles(nodes []DiagramNode, edges []DiagramEdge) {
	succ := make(map[string][]int, len(nodes))
	for i, e := range edges {
		succ[e.From] = append(succ[e.From], i)
	}
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := make(map[string]int, len(nodes))
	type frame struct {
		id string
		at int
	}
	for _, n := range nodes {
		if color[n.ID] != white {
			continue
		}
		stack := []frame{{id: n.ID}}
		color[n.ID] = grey
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			if top.at >= len(succ[top.id]) {
				color[top.id] = black
				stack = stack[:len(stack)-1]
				continue
			}
			ei := succ[top.id][top.at]
			top.at++
			to := edges[ei].To
			switch color[to] {
			case grey:
				edges[ei].Cycle = true
			case white:
				color[to] = grey
				stack = append(stack, frame{id: to})
			}
		}
	}
}

// orient re-maps layout.go's left-to-right coordinates onto the direction the
// author asked for. This is a COORDINATE TRANSFORM, not a second layout: ranks
// and within-rank ordering are whatever Layout decided, so a TD diagram and its
// LR transpose have identical topology-derived structure and the determinism
// tests over Layout cover both.
//
// The rejected alternative was passing the direction into Layout. That would
// put a rendering concern inside the pure function two callers share, and would
// have made the delegation graph's layout depend on a mermaid concept.
func orient(dir string, placed []Placement, w, h int) ([]Placement, int, int) {
	switch dir {
	case "LR":
		return placed, w, h
	case "RL":
		out := make([]Placement, len(placed))
		for i, p := range placed {
			p.X = w - p.X - NodeW
			out[i] = p
		}
		return out, w, h
	}

	// TD / TB. Ranks run down the page, so the strides swap roles: rank steps
	// by row height, within-rank position steps by card width. Reusing the LR
	// coordinates directly (a plain x/y swap) would step columns by strideY,
	// which is smaller than a card is wide, and overlap every row.
	byRank := map[int][]Placement{}
	maxRank := 0
	for _, p := range placed {
		byRank[p.Rank] = append(byRank[p.Rank], p)
		if p.Rank > maxRank {
			maxRank = p.Rank
		}
	}
	widest := 1
	for r := 0; r <= maxRank; r++ {
		row := byRank[r]
		// Y then ID: Y is Layout's own ordering decision, ID is the same tie
		// break Layout uses, so this sort adds no new source of nondeterminism.
		sort.Slice(row, func(i, j int) bool {
			if row[i].Y != row[j].Y {
				return row[i].Y < row[j].Y
			}
			return row[i].ID < row[j].ID
		})
		byRank[r] = row
		if len(row) > widest {
			widest = len(row)
		}
	}
	width := widest*strideX - GapX
	height := (maxRank+1)*strideY - GapY
	out := make([]Placement, 0, len(placed))
	for r := 0; r <= maxRank; r++ {
		row := byRank[r]
		left := (width - (len(row)*strideX - GapX)) / 2
		for i, p := range row {
			p.X, p.Y = left+i*strideX, r*strideY
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if width < NodeW {
		width = NodeW
	}
	if height < NodeH {
		height = NodeH
	}
	return out, width, height
}
