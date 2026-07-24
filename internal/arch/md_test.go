package arch

import (
	"strings"
	"testing"
)

func kinds(bs []Block) []BlockKind {
	out := make([]BlockKind, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Kind)
	}
	return out
}

func TestRenderBlocks(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		want  []BlockKind
		check func(t *testing.T, bs []Block)
	}{
		{
			name: "heading levels and slug",
			src:  "# Title\n\n## A Section, Here\n",
			want: []BlockKind{BlockHeading, BlockHeading},
			check: func(t *testing.T, bs []Block) {
				if bs[0].Level != 1 || bs[1].Level != 2 {
					t.Fatalf("levels = %d,%d", bs[0].Level, bs[1].Level)
				}
				if bs[1].Slug != "a-section-here" {
					t.Fatalf("slug = %q", bs[1].Slug)
				}
			},
		},
		{
			name: "paragraph joins soft-wrapped lines",
			src:  "one\ntwo\n\nthree\n",
			want: []BlockKind{BlockParagraph, BlockParagraph},
			check: func(t *testing.T, bs []Block) {
				if got := PlainText(bs[0].Inline); got != "one\ntwo" {
					t.Fatalf("para = %q", got)
				}
			},
		},
		{
			name: "fenced code keeps info string",
			src:  "```go\nfunc main() {}\n```\n",
			want: []BlockKind{BlockCode},
			check: func(t *testing.T, bs []Block) {
				if bs[0].Lang != "go" || bs[0].Text != "func main() {}" || bs[0].Unclosed {
					t.Fatalf("code = %#v", bs[0])
				}
			},
		},
		{
			name: "mermaid fence outside the subset falls back with a chip",
			src:  "```mermaid\nsequenceDiagram\n  a ->> b: hi\n```\n",
			want: []BlockKind{BlockCode},
			check: func(t *testing.T, bs []Block) {
				if bs[0].Diagram == nil || bs[0].Diagram.Status != DiagramUnsupported {
					t.Fatalf("diagram = %#v", bs[0].Diagram)
				}
				if !strings.Contains(bs[0].Note, "shown as source") {
					t.Fatalf("note = %q", bs[0].Note)
				}
			},
		},
		{
			name: "unterminated fence renders to EOF and says so",
			src:  "intro\n\n```sh\nls -l\nstill code\n",
			want: []BlockKind{BlockParagraph, BlockCode},
			check: func(t *testing.T, bs []Block) {
				if !bs[1].Unclosed || bs[1].Note == "" {
					t.Fatalf("expected an unclosed fence with a note: %#v", bs[1])
				}
				if bs[1].Text != "ls -l\nstill code" {
					t.Fatalf("text = %q", bs[1].Text)
				}
			},
		},
		{
			// 4d ships the parser, so a fence INSIDE the subset now draws and
			// carries no chip. The source text is still preserved verbatim: the
			// fallback is permanent and the renderer may need it at any time.
			name: "mermaid fence in the subset parses, keeping its source",
			src:  "```mermaid\nflowchart LR\n  a --> b\n```\n",
			want: []BlockKind{BlockCode},
			check: func(t *testing.T, bs []Block) {
				if bs[0].Lang != "mermaid" {
					t.Fatalf("lang = %q", bs[0].Lang)
				}
				if bs[0].Text != "flowchart LR\n  a --> b" {
					t.Fatalf("source not preserved: %q", bs[0].Text)
				}
				if bs[0].Diagram == nil || bs[0].Diagram.Status != DiagramOK {
					t.Fatalf("diagram = %#v", bs[0].Diagram)
				}
				if bs[0].Note != "" {
					t.Fatalf("a drawn diagram carries no chip, got %q", bs[0].Note)
				}
			},
		},
		{
			name: "flat unordered list",
			src:  "- one\n- two\n- three\n",
			want: []BlockKind{BlockList},
			check: func(t *testing.T, bs []Block) {
				if bs[0].Ordered || len(bs[0].Items) != 3 {
					t.Fatalf("list = %#v", bs[0])
				}
				if PlainText(bs[0].Items[2].Inline) != "three" {
					t.Fatalf("item = %q", PlainText(bs[0].Items[2].Inline))
				}
			},
		},
		{
			name: "ordered list",
			src:  "1. one\n2. two\n",
			want: []BlockKind{BlockList},
			check: func(t *testing.T, bs []Block) {
				if !bs[0].Ordered || len(bs[0].Items) != 2 {
					t.Fatalf("list = %#v", bs[0])
				}
			},
		},
		{
			name: "nested list becomes a child block",
			src:  "- outer\n  - inner a\n  - inner b\n- second\n",
			want: []BlockKind{BlockList},
			check: func(t *testing.T, bs []Block) {
				if len(bs[0].Items) != 2 {
					t.Fatalf("items = %d", len(bs[0].Items))
				}
				sub := bs[0].Items[0].Blocks
				if len(sub) != 1 || sub[0].Kind != BlockList || len(sub[0].Items) != 2 {
					t.Fatalf("nested = %#v", sub)
				}
				if PlainText(sub[0].Items[1].Inline) != "inner b" {
					t.Fatalf("nested item = %q", PlainText(sub[0].Items[1].Inline))
				}
			},
		},
		{
			name: "table with header and rows",
			src:  "| Kind | Renders |\n|---|---|\n| decision | card |\n| contract | card |\n",
			want: []BlockKind{BlockTable},
			check: func(t *testing.T, bs []Block) {
				if len(bs[0].Head) != 2 || len(bs[0].Rows) != 2 {
					t.Fatalf("table = %#v", bs[0])
				}
				if PlainText(bs[0].Rows[1][0].Inline) != "contract" {
					t.Fatalf("cell = %q", PlainText(bs[0].Rows[1][0].Inline))
				}
			},
		},
		{
			name: "blockquote nests blocks",
			src:  "> quoted\n>\n> - a\n",
			want: []BlockKind{BlockQuote},
			check: func(t *testing.T, bs []Block) {
				if len(bs[0].Blocks) != 2 || bs[0].Blocks[1].Kind != BlockList {
					t.Fatalf("quote = %#v", bs[0].Blocks)
				}
			},
		},
		{
			name: "thematic break",
			src:  "a\n\n---\n\nb\n",
			want: []BlockKind{BlockParagraph, BlockRule, BlockParagraph},
		},
		{
			name: "raw HTML is literal text, never markup",
			src:  "before <img src=x onerror=alert(1)> after\n",
			want: []BlockKind{BlockParagraph},
			check: func(t *testing.T, bs []Block) {
				got := PlainText(bs[0].Inline)
				if got != "before <img src=x onerror=alert(1)> after" {
					t.Fatalf("html not literal: %q", got)
				}
				for _, n := range bs[0].Inline {
					if n.Kind != InlineText {
						t.Fatalf("html produced a non-text token: %#v", n)
					}
				}
			},
		},
		{
			name: "empty input yields no blocks and does not panic",
			src:  "",
			want: []BlockKind{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bs := Render(tc.src)
			got := kinds(bs)
			if len(got) != len(tc.want) {
				t.Fatalf("blocks = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("blocks = %v, want %v", got, tc.want)
				}
			}
			if tc.check != nil {
				tc.check(t, bs)
			}
		})
	}
}

func TestRenderInline(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []Inline
	}{
		{
			name: "strong and emph",
			src:  "**bold** and *soft*",
			want: []Inline{
				{Kind: InlineStrong, Children: []Inline{{Kind: InlineText, Text: "bold"}}},
				{Kind: InlineText, Text: " and "},
				{Kind: InlineEmph, Children: []Inline{{Kind: InlineText, Text: "soft"}}},
			},
		},
		{
			name: "code span wins over emphasis",
			src:  "`a *b* c`",
			want: []Inline{{Kind: InlineCode, Text: "a *b* c"}},
		},
		{
			name: "unmatched asterisk stays literal",
			src:  "2 * 3 = 6",
			want: []Inline{{Kind: InlineText, Text: "2 * 3 = 6"}},
		},
		{
			name: "backslash escape",
			src:  `\*not emph\*`,
			want: []Inline{{Kind: InlineText, Text: "*not emph*"}},
		},
		{
			name: "unmatched bracket stays literal",
			src:  "[oops",
			want: []Inline{{Kind: InlineText, Text: "[oops"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseInline(tc.src)
			if len(got) != len(tc.want) {
				t.Fatalf("inline = %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i].Kind != tc.want[i].Kind || got[i].Text != tc.want[i].Text {
					t.Fatalf("inline[%d] = %#v, want %#v", i, got[i], tc.want[i])
				}
				if PlainText(got[i:i+1]) != PlainText(tc.want[i:i+1]) {
					t.Fatalf("inline[%d] text = %q, want %q", i, PlainText(got[i:i+1]), PlainText(tc.want[i:i+1]))
				}
			}
		})
	}
}

// Link routing is §4.4's gate. Every scheme that is not http(s) or file is
// INERT, and an inert link carries no href at all — the token tree must not be
// able to hand a `javascript:` payload to a frontend that forgets to check.
func TestLinkRouting(t *testing.T) {
	tests := []struct {
		name       string
		href       string
		wantTarget LinkTarget
		wantHref   string
	}{
		{"relative path", "docs/ARCHITECTURE.md", LinkEditor, "docs/ARCHITECTURE.md"},
		{"absolute path", "/Users/h/x.md", LinkEditor, "/Users/h/x.md"},
		{"file scheme", "file:///Users/h/x.md", LinkEditor, "/Users/h/x.md"},
		{"http", "http://example.com/a", LinkBrowser, "http://example.com/a"},
		{"https", "https://example.com/a", LinkBrowser, "https://example.com/a"},
		{"https uppercase scheme", "HTTPS://example.com", LinkBrowser, "HTTPS://example.com"},
		{"javascript", "javascript:alert(1)", LinkInert, ""},
		{"javascript mixed case", "JaVaScRiPt:alert(1)", LinkInert, ""},
		{"javascript with control chars", "java\x0ascript:alert(1)", LinkInert, ""},
		{"javascript with leading space", "   javascript:alert(1)", LinkInert, ""},
		{"data uri", "data:text/html;base64,PHNjcmlwdD4=", LinkInert, ""},
		{"vbscript", "vbscript:msgbox", LinkInert, ""},
		{"mailto", "mailto:a@b.c", LinkInert, ""},
		{"in-document anchor", "#section", LinkInert, ""},
		{"empty", "", LinkInert, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target, href := classifyLink(tc.href)
			if target != tc.wantTarget || href != tc.wantHref {
				t.Fatalf("classify(%q) = %q,%q; want %q,%q", tc.href, target, href, tc.wantTarget, tc.wantHref)
			}
		})
	}
}

func TestLinkTokensInDocument(t *testing.T) {
	bs := Render("see [spec](docs/x.md), [site](https://loom.dev) and [bad](javascript:alert(1))\n")
	if len(bs) != 1 {
		t.Fatalf("blocks = %v", kinds(bs))
	}
	var links []Inline
	for _, n := range bs[0].Inline {
		if n.Kind == InlineLink {
			links = append(links, n)
		}
	}
	if len(links) != 3 {
		t.Fatalf("links = %d (%#v)", len(links), bs[0].Inline)
	}
	want := []struct {
		target LinkTarget
		href   string
		label  string
	}{
		{LinkEditor, "docs/x.md", "spec"},
		{LinkBrowser, "https://loom.dev", "site"},
		{LinkInert, "", "bad"},
	}
	for i, w := range want {
		if links[i].Target != w.target || links[i].Href != w.href || links[i].Text != w.label {
			t.Fatalf("link[%d] = %#v, want %+v", i, links[i], w)
		}
	}
}

func TestImageIsNotFetched(t *testing.T) {
	bs := Render("![a diagram](https://evil.example/pixel.png)\n")
	if len(bs) != 1 || len(bs[0].Inline) != 1 {
		t.Fatalf("blocks = %#v", bs)
	}
	img := bs[0].Inline[0]
	if img.Kind != InlineImage || img.Text != "a diagram" {
		t.Fatalf("image = %#v", img)
	}
	if img.Target != LinkBrowser {
		// http(s) routes to the existing OpenURL gate — a click, never a fetch.
		t.Fatalf("image target = %q", img.Target)
	}
}

func TestOutlineIsH2H3Only(t *testing.T) {
	bs := Render("# Doc\n\n## One\n\n### Deep\n\n#### Deeper\n\n## Two\n")
	got := Outline(bs)
	if len(got) != 3 {
		t.Fatalf("outline = %#v", got)
	}
	if got[0].Text != "One" || got[1].Level != 3 || got[2].Slug != "two" {
		t.Fatalf("outline = %#v", got)
	}
}

// A megabyte of markdown must render without pathological behaviour: this is
// the render path of a poll, and an O(n²) scanner would be discovered in the
// field rather than here.
func TestRenderLargeDocument(t *testing.T) {
	var sb strings.Builder
	for sb.Len() < 1<<20 {
		sb.WriteString("## Section\n\nSome **prose** with `code` and a [link](docs/x.md).\n\n- a\n- b\n\n")
	}
	bs := Render(sb.String())
	if len(bs) < 100 {
		t.Fatalf("blocks = %d", len(bs))
	}
}
