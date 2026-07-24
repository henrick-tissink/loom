package arch

import (
	"strings"
	"unicode"
)

// The markdown renderer (orchestration-view spec §4.4). It emits a TOKEN TREE,
// never an HTML string, and that is the whole security argument: there is no
// injection site by construction, because nothing downstream ever concatenates
// markup. Raw HTML in the source is not parsed at all — `<img src=x
// onerror=alert(1)>` falls through as ordinary text and the frontend paints it
// with textContent.
//
// Rejected: a markdown library. A fixed token set and a four-key front-matter
// subset do not justify a dependency whose default is raw-HTML passthrough,
// and the frontend budget for this arc is zero new dependencies either way.
//
// Unsupported constructs degrade to their source text. There is deliberately no
// failure mode where the pane is empty because of a syntax the parser did not
// know: every line of the input reaches the output as something.

type BlockKind string

const (
	BlockHeading   BlockKind = "heading"
	BlockParagraph BlockKind = "paragraph"
	BlockCode      BlockKind = "code"
	BlockList      BlockKind = "list"
	BlockQuote     BlockKind = "quote"
	BlockTable     BlockKind = "table"
	BlockRule      BlockKind = "rule"
)

// Block is one node of the safe token tree.
type Block struct {
	Kind  BlockKind
	Level int    // heading level, 1..6
	Slug  string // heading anchor, for the outline pane

	Inline []Inline // heading, paragraph

	// Code blocks. Text is verbatim source; Info is the fence info string as
	// written; Lang is its first word, lowercased, for highlighting.
	Text     string
	Info     string
	Lang     string
	Unclosed bool // fence ran to EOF — rendered, plus a visible note

	// Note is a non-fatal, user-visible remark about this block. Refusals and
	// degradations are VISIBLE, never silent: a mermaid fence shown as source
	// with no explanation is indistinguishable from a renderer that broke.
	Note string

	// Diagram is set on a `mermaid` fence and ONLY there. Non-nil always — a
	// fence outside the subset or malformed carries a Diagram whose Status says
	// which, so the renderer's decision is "draw or show source", never "did
	// the parser run". Text stays populated in every case: the source block is
	// the permanent fallback (view spec §6) and it is the same block either way.
	Diagram *Diagram

	Ordered bool // list
	Items   []ListItem

	Head []Cell   // table header row
	Rows [][]Cell // table body rows

	Blocks []Block // blockquote children
}

type ListItem struct {
	Inline []Inline // the item's own first paragraph, hoisted for flat lists
	Blocks []Block  // everything else, including nested lists
}

type Cell struct{ Inline []Inline }

type InlineKind string

const (
	InlineText   InlineKind = "text"
	InlineCode   InlineKind = "code"
	InlineEmph   InlineKind = "emph"
	InlineStrong InlineKind = "strong"
	InlineLink   InlineKind = "link"
	InlineImage  InlineKind = "image"
)

// LinkTarget says which existing, already-tested gate a link routes to. §4.4
// binds this: `file:`/relative to OpenInEditor, `http(s)` to OpenURL, and
// EVERY other scheme to inert text. The gates exist and are tested; this
// classifies, it does not re-derive them.
type LinkTarget string

const (
	LinkEditor  LinkTarget = "editor"
	LinkBrowser LinkTarget = "browser"
	LinkInert   LinkTarget = "inert"
)

type Inline struct {
	Kind     InlineKind
	Text     string // text, code span, link label, image alt
	Href     string // link/image destination — EMPTY when Target is inert
	Target   LinkTarget
	Children []Inline // emph, strong, link label
}

// Render parses markdown into the token tree. It never fails and never panics
// on malformed input: the worst case is that a construct degrades to the text
// that produced it.
func Render(src string) []Block {
	return parseBlocks(splitLines(src))
}

// OutlineEntry is one row of the auto-built section outline pinned beside a
// rendered architecture document (§4.3). h2/h3 only — h1 is the document title
// and deeper levels turn the pane into a second copy of the document.
type OutlineEntry struct {
	Level int
	Text  string
	Slug  string
}

func Outline(blocks []Block) []OutlineEntry {
	var out []OutlineEntry
	for _, b := range blocks {
		if b.Kind == BlockHeading && (b.Level == 2 || b.Level == 3) {
			out = append(out, OutlineEntry{Level: b.Level, Text: PlainText(b.Inline), Slug: b.Slug})
		}
	}
	return out
}

// PlainText flattens inline nodes back to their text, for titles, outline rows
// and the decision-card consequence line.
func PlainText(in []Inline) string {
	var sb strings.Builder
	for _, n := range in {
		if len(n.Children) > 0 {
			sb.WriteString(PlainText(n.Children))
			continue
		}
		sb.WriteString(n.Text)
	}
	return sb.String()
}

func splitLines(src string) []string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	src = strings.TrimSuffix(src, "\n")
	if src == "" {
		return nil
	}
	return strings.Split(src, "\n")
}

func parseBlocks(lines []string) []Block {
	var out []Block
	i := 0
	for i < len(lines) {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			i++
			continue
		}
		switch {
		case isFence(line):
			b, next := parseFence(lines, i)
			out = append(out, b)
			i = next
		case headingLevel(line) > 0:
			out = append(out, parseHeading(line))
			i++
		case isRule(line):
			out = append(out, Block{Kind: BlockRule})
			i++
		case strings.HasPrefix(strings.TrimLeft(line, " \t"), ">"):
			b, next := parseQuote(lines, i)
			out = append(out, b)
			i = next
		case listMarker(line) != nil:
			b, next := parseList(lines, i)
			out = append(out, b)
			i = next
		case isTableStart(lines, i):
			b, next := parseTable(lines, i)
			out = append(out, b)
			i = next
		default:
			b, next := parseParagraph(lines, i)
			out = append(out, b)
			i = next
		}
	}
	return out
}

// --- leaf blocks ---

func isFence(line string) bool {
	t := strings.TrimLeft(line, " ")
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

func parseFence(lines []string, i int) (Block, int) {
	open := strings.TrimLeft(lines[i], " ")
	ch := open[0:1]
	n := 0
	for n < len(open) && string(open[n]) == ch {
		n++
	}
	info := strings.TrimSpace(open[n:])
	b := Block{Kind: BlockCode, Info: info}
	if f := strings.Fields(info); len(f) > 0 {
		b.Lang = strings.ToLower(f[0])
	}
	var body []string
	j := i + 1
	closed := false
	for ; j < len(lines); j++ {
		t := strings.TrimLeft(lines[j], " ")
		if strings.HasPrefix(t, strings.Repeat(ch, n)) && strings.TrimSpace(strings.TrimLeft(t, ch)) == "" {
			closed = true
			j++
			break
		}
		body = append(body, lines[j])
	}
	b.Text = strings.Join(body, "\n")
	if !closed {
		// An unterminated fence is a real authoring mistake in an
		// agent-authored file. Rendering the rest of the document as prose
		// would silently reflow it; rendering it as code and SAYING the fence
		// never closed is the honest read.
		b.Unclosed = true
		b.Note = "unterminated code fence — rendered to end of file"
	}
	if b.Lang == "mermaid" {
		// An unterminated fence is not parsed at all: its body is whatever was
		// left in the file, so a "successful" parse of it would be a diagram of
		// a truncated document. The fence note is the only honest answer.
		if b.Unclosed {
			d := errDiagram("unterminated code fence", 0)
			b.Diagram = &d
			b.Note = "mermaid — shown as source; unterminated code fence"
			return b, j
		}
		d := ParseMermaid(b.Text)
		b.Diagram = &d
		// Note stays EMPTY on a drawn diagram — a chip beside a picture that
		// rendered fine reads as an apology for it.
		b.Note = d.Note()
	}
	return b, j
}

func headingLevel(line string) int {
	t := strings.TrimLeft(line, " ")
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0
	}
	if n < len(t) && (t[n] == ' ' || t[n] == '\t') {
		return n
	}
	if n == len(t) {
		return n
	}
	return 0
}

func parseHeading(line string) Block {
	t := strings.TrimLeft(line, " ")
	lvl := headingLevel(line)
	text := strings.TrimSpace(strings.TrimLeft(t[lvl:], " \t"))
	text = strings.TrimRight(text, "#")
	text = strings.TrimSpace(text)
	in := parseInline(text)
	return Block{Kind: BlockHeading, Level: lvl, Slug: Slug(PlainText(in)), Inline: in}
}

// Slug is the heading anchor. Deterministic and pure so an outline row and its
// heading agree without the frontend inventing an id.
func Slug(s string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			sb.WriteRune(r)
			dash = false
		default:
			if !dash && sb.Len() > 0 {
				sb.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(sb.String(), "-")
}

func isRule(line string) bool {
	t := strings.TrimSpace(line)
	if len(t) < 3 {
		return false
	}
	c := t[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] != c {
			return false
		}
	}
	return true
}

func parseParagraph(lines []string, i int) (Block, int) {
	var buf []string
	j := i
	for ; j < len(lines); j++ {
		l := lines[j]
		if strings.TrimSpace(l) == "" {
			break
		}
		if j > i && (isFence(l) || headingLevel(l) > 0 || isRule(l) || listMarker(l) != nil ||
			strings.HasPrefix(strings.TrimLeft(l, " \t"), ">")) {
			break
		}
		buf = append(buf, strings.TrimSpace(l))
	}
	return Block{Kind: BlockParagraph, Inline: parseInline(strings.Join(buf, "\n"))}, j
}

func parseQuote(lines []string, i int) (Block, int) {
	var inner []string
	j := i
	for ; j < len(lines); j++ {
		t := strings.TrimLeft(lines[j], " \t")
		if !strings.HasPrefix(t, ">") {
			if strings.TrimSpace(lines[j]) == "" {
				break
			}
			// Lazy continuation: a bare line under a quote belongs to it.
			inner = append(inner, lines[j])
			continue
		}
		inner = append(inner, strings.TrimPrefix(strings.TrimPrefix(t, ">"), " "))
	}
	return Block{Kind: BlockQuote, Blocks: parseBlocks(inner)}, j
}

// --- lists ---

type marker struct {
	indent  int // columns before the marker
	width   int // marker + following spaces
	ordered bool
}

func listMarker(line string) *marker {
	indent := 0
	for indent < len(line) && (line[indent] == ' ' || line[indent] == '\t') {
		indent++
	}
	rest := line[indent:]
	if rest == "" {
		return nil
	}
	if (rest[0] == '-' || rest[0] == '*' || rest[0] == '+') && len(rest) > 1 && (rest[1] == ' ' || rest[1] == '\t') {
		return &marker{indent: indent, width: 2, ordered: false}
	}
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	if n > 0 && n+1 < len(rest) && (rest[n] == '.' || rest[n] == ')') && (rest[n+1] == ' ' || rest[n+1] == '\t') {
		return &marker{indent: indent, width: n + 2, ordered: true}
	}
	return nil
}

func parseList(lines []string, i int) (Block, int) {
	first := listMarker(lines[i])
	b := Block{Kind: BlockList, Ordered: first.ordered}
	j := i
	for j < len(lines) {
		m := listMarker(lines[j])
		if m == nil || m.indent > first.indent+3 {
			break
		}
		if m.indent < first.indent || m.ordered != first.ordered {
			break
		}
		// Item body: the marker's own line, plus every following line indented
		// past the marker (that is what makes a nested list nested) or blank.
		body := []string{lines[j][m.indent+m.width:]}
		j++
		for j < len(lines) {
			l := lines[j]
			if strings.TrimSpace(l) == "" {
				// A blank line only continues the item if something indented
				// follows; otherwise the list ends here.
				if j+1 < len(lines) && indentOf(lines[j+1]) > first.indent {
					body = append(body, "")
					j++
					continue
				}
				break
			}
			if indentOf(l) <= first.indent {
				break
			}
			body = append(body, dedent(l, first.indent+first.width))
			j++
		}
		item := ListItem{Blocks: parseBlocks(body)}
		// Hoist the item's leading paragraph so a flat list is a flat list in
		// the tree too; the frontend should not have to unwrap one every time.
		if len(item.Blocks) > 0 && item.Blocks[0].Kind == BlockParagraph {
			item.Inline = item.Blocks[0].Inline
			item.Blocks = item.Blocks[1:]
		}
		b.Items = append(b.Items, item)
	}
	return b, j
}

func indentOf(line string) int {
	n := 0
	for n < len(line) && (line[n] == ' ' || line[n] == '\t') {
		n++
	}
	if n == len(line) {
		return 1 << 30 // blank line: infinitely indented, never ends a block
	}
	return n
}

func dedent(line string, n int) string {
	i := 0
	for i < n && i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return line[i:]
}

// --- tables ---

func isTableStart(lines []string, i int) bool {
	if i+1 >= len(lines) || !strings.Contains(lines[i], "|") {
		return false
	}
	return isDelimRow(lines[i+1])
}

func isDelimRow(line string) bool {
	cells := splitRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		c = strings.TrimSpace(c)
		c = strings.TrimPrefix(c, ":")
		c = strings.TrimSuffix(c, ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}

func splitRow(line string) []string {
	t := strings.TrimSpace(line)
	if !strings.Contains(t, "|") {
		return nil
	}
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	return strings.Split(t, "|")
}

func parseTable(lines []string, i int) (Block, int) {
	b := Block{Kind: BlockTable}
	for _, c := range splitRow(lines[i]) {
		b.Head = append(b.Head, Cell{Inline: parseInline(strings.TrimSpace(c))})
	}
	j := i + 2
	for ; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" || !strings.Contains(lines[j], "|") {
			break
		}
		var row []Cell
		for _, c := range splitRow(lines[j]) {
			row = append(row, Cell{Inline: parseInline(strings.TrimSpace(c))})
		}
		b.Rows = append(b.Rows, row)
	}
	return b, j
}

// --- inline ---

func parseInline(s string) []Inline {
	var out []Inline
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, Inline{Kind: InlineText, Text: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && isPunct(s[i+1]):
			lit.WriteByte(s[i+1])
			i += 2
		case c == '`':
			// Code spans bind tighter than emphasis, so `*x*` inside backticks
			// stays literal — the same rule CommonMark uses, and the reason a
			// code span is matched before anything else here.
			n := 0
			for i+n < len(s) && s[i+n] == '`' {
				n++
			}
			fence := strings.Repeat("`", n)
			end := strings.Index(s[i+n:], fence)
			if end < 0 {
				lit.WriteString(fence)
				i += n
				continue
			}
			flush()
			out = append(out, Inline{Kind: InlineCode, Text: s[i+n : i+n+end]})
			i += n + end + n
		case c == '!' && i+1 < len(s) && s[i+1] == '[':
			label, href, next, ok := parseLinkAt(s, i+1)
			if !ok {
				lit.WriteByte(c)
				i++
				continue
			}
			flush()
			// An image's src is an arbitrary agent-authored URL on a render
			// path; Loom does not fetch it. The alt text renders, and the src
			// is classified like any other link so the user can still open it
			// through a gate that already exists.
			t, clean := classifyLink(href)
			out = append(out, Inline{Kind: InlineImage, Text: label, Href: clean, Target: t})
			i = next
		case c == '[':
			label, href, next, ok := parseLinkAt(s, i)
			if !ok {
				lit.WriteByte(c)
				i++
				continue
			}
			flush()
			t, clean := classifyLink(href)
			out = append(out, Inline{Kind: InlineLink, Text: label, Href: clean, Target: t, Children: parseInline(label)})
			i = next
		case c == '*' || c == '_':
			n := 1
			if i+1 < len(s) && s[i+1] == c {
				n = 2
			}
			delim := strings.Repeat(string(c), n)
			end := strings.Index(s[i+n:], delim)
			if end <= 0 {
				lit.WriteByte(c)
				i++
				continue
			}
			flush()
			inner := s[i+n : i+n+end]
			kind := InlineEmph
			if n == 2 {
				kind = InlineStrong
			}
			out = append(out, Inline{Kind: kind, Children: parseInline(inner)})
			i += n + end + n
		default:
			lit.WriteByte(c)
			i++
		}
	}
	flush()
	return out
}

func isPunct(c byte) bool {
	return strings.IndexByte("\\`*_{}[]()#+-.!|<>~", c) >= 0
}

// parseLinkAt reads `[label](href)` starting at s[i] == '['. Returns ok=false
// for anything else, which falls back to literal text — an unmatched bracket
// is far more common than a link in prose.
func parseLinkAt(s string, i int) (label, href string, next int, ok bool) {
	depth := 0
	j := i
	for ; j < len(s); j++ {
		if s[j] == '\\' {
			j++
			continue
		}
		if s[j] == '[' {
			depth++
		} else if s[j] == ']' {
			depth--
			if depth == 0 {
				break
			}
		}
	}
	if j >= len(s) || j+1 >= len(s) || s[j+1] != '(' {
		return "", "", 0, false
	}
	label = s[i+1 : j]
	k := j + 2
	pd := 1
	start := k
	for ; k < len(s); k++ {
		if s[k] == '(' {
			pd++
		} else if s[k] == ')' {
			pd--
			if pd == 0 {
				break
			}
		}
	}
	if k >= len(s) {
		return "", "", 0, false
	}
	href = strings.TrimSpace(s[start:k])
	// A title (`[a](b "t")`) is dropped rather than rendered: it is a tooltip
	// surface with no place in the token tree.
	if sp := strings.IndexAny(href, " \t"); sp >= 0 {
		href = href[:sp]
	}
	return label, href, k + 1, true
}

// classifyLink routes a destination to an EXISTING gate, and returns the href
// the frontend is allowed to keep. An inert link's href is BLANKED: an inert
// classification the frontend could ignore is one refactor away from being a
// `javascript:` sink, so the token tree simply does not carry the payload.
func classifyLink(href string) (LinkTarget, string) {
	h := strings.TrimSpace(href)
	// Strip C0 controls and space anywhere before scheme detection: `java\n
	// script:` and `  javascript:` are the classic bypasses.
	h = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, h)
	if h == "" {
		return LinkInert, ""
	}
	if strings.HasPrefix(h, "#") {
		// In-document anchor. No routing exists for it yet, and inventing one
		// here would be a second navigation model; it renders as text.
		return LinkInert, ""
	}
	if scheme, rest, found := splitScheme(h); found {
		switch scheme {
		case "http", "https":
			return LinkBrowser, h
		case "file":
			return LinkEditor, strings.TrimPrefix(rest, "//")
		default:
			// javascript:, data:, vbscript:, mailto:, everything else. §4.4 is
			// an allowlist, deliberately: a denylist of dangerous schemes has
			// been wrong every time it has been tried.
			return LinkInert, ""
		}
	}
	return LinkEditor, h
}

// splitScheme reports an RFC-3986 scheme prefix. Written out rather than using
// net/url: url.Parse accepts and normalizes far more than this gate wants to
// reason about, and its error cases would silently become "relative path".
func splitScheme(h string) (scheme, rest string, found bool) {
	for i := 0; i < len(h); i++ {
		c := h[i]
		if c == ':' {
			if i == 0 {
				return "", "", false
			}
			return strings.ToLower(h[:i]), h[i+1:], true
		}
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '+' || c == '-' || c == '.'
		if !ok {
			return "", "", false
		}
		if i == 0 && !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return "", "", false
		}
	}
	return "", "", false
}
