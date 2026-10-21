package brief

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// md renders without raw HTML (goldmark's default escapes it and drops
// dangerous link schemes): a brief's text came from a model that read
// untrusted transcripts, so it must never become markup or script.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// HTML renders markdown to an HTML fragment.
func HTML(src string) string {
	var b bytes.Buffer
	if err := md.Convert([]byte(src), &b); err != nil {
		return "<pre>" + html.EscapeString(src) + "</pre>"
	}
	return b.String()
}

// Part kinds. A brief reads head, sections and tasks, then the appendix.
const (
	KindHead     = "head"     // the "# project · window" title and its stamp line
	KindSection  = "section"  // a level-2 section that is not a task (Overview, Decisions…)
	KindTask     = "task"     // a "## Task: " section
	KindAppendix = "appendix" // "# Appendix" to the end: git facts and the evidence index
)

// Part is a slice of a brief: its head, one section, one task, or the
// appendix.
type Part struct {
	Task  int    // 1-based task number, 0 for a non-task part
	Kind  string // KindHead, KindSection, KindTask or KindAppendix
	Title string // the heading's text (for a task, without "Task: ")
	MD    string // the part's markdown, its heading included
}

// Split cuts a brief at its level-1 and level-2 headings. A task runs until
// the next such heading, so "## Decisions and consultations" and the
// appendix are not swallowed into the last task; the appendix runs to the
// end, its own headings included.
func Split(src string) []Part {
	var parts []Part
	cur := Part{Kind: KindHead}
	var buf []string
	n := 0
	flush := func() {
		for len(buf) > 0 && (strings.TrimSpace(buf[len(buf)-1]) == "" || strings.TrimSpace(buf[len(buf)-1]) == "---") {
			buf = buf[:len(buf)-1] // a trailing rule belongs to the break, not the part
		}
		cur.MD = strings.Join(buf, "\n")
		if strings.TrimSpace(cur.MD) != "" {
			parts = append(parts, cur)
		}
		buf = nil
	}
	inFence := false
	for i, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
		}
		if !inFence && cur.Kind != KindAppendix {
			switch {
			case strings.HasPrefix(l, TaskHeading):
				flush()
				n++
				cur = Part{Task: n, Kind: KindTask, Title: plainTitle(strings.TrimPrefix(l, TaskHeading))}
			case strings.HasPrefix(l, "## "):
				flush()
				cur = Part{Kind: KindSection, Title: plainTitle(strings.TrimPrefix(l, "## "))}
			case strings.HasPrefix(l, "# ") && i > 0:
				flush()
				cur = Part{Kind: KindAppendix, Title: plainTitle(strings.TrimPrefix(l, "# "))}
			case strings.HasPrefix(l, "# "):
				cur.Title = plainTitle(strings.TrimPrefix(l, "# "))
			}
		}
		buf = append(buf, l)
	}
	flush()
	return parts
}

// plainTitle is a heading as plain text: titles are drawn as text (outline,
// card headers), so inline markdown would show as literal backticks.
func plainTitle(h string) string {
	return strings.TrimSpace(strings.NewReplacer("`", "", "**", "", "__", "").Replace(h))
}

// Body is the part's markdown without what a reader draws itself: the
// heading of a section, task or appendix, and, for the head, the title and
// the stamp line Assemble writes under it.
func (p Part) Body() string {
	lines := strings.Split(p.MD, "\n")
	out := lines[:0:0]
	stamp := p.Kind == KindHead
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case i == 0 && strings.HasPrefix(l, "#"):
			continue
		case stamp && strings.HasPrefix(t, "*") && strings.Contains(t, "written by Claude"):
			stamp = false
			continue
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// Task returns the markdown of task n (1-based).
func Task(src string, n int) (Part, bool) {
	for _, p := range Split(src) {
		if p.Task == n {
			return p, true
		}
	}
	return Part{}, false
}

// Index maps an evidence id ("S3") to what the evidence index says it is,
// so a citation can say which session it points at.
type Index map[string]string

var indexLine = regexp.MustCompile("^- \\*\\*(S\\d+)\\*\\* · (session|subagent) \\S+ · (.*) · `")

// EvidenceIndex reads the index Assemble appends to every brief.
func EvidenceIndex(src string) Index {
	idx := Index{}
	for _, l := range strings.Split(src, "\n") {
		if m := indexLine.FindStringSubmatch(l); m != nil {
			t := m[3]
			if m[2] == "subagent" && !strings.HasPrefix(t, "subagent") {
				t = "subagent: " + t
			}
			idx[m[1]] = t
		}
	}
	return idx
}

var (
	// A citation: [S1:L20], [git; S1:L5], [git `abc1234`], [inferred]… Its
	// text is already escaped HTML, so the only markup it may hold is code.
	citeRe   = regexp.MustCompile(`\[((?:git|transcript|inferred|unverified|S\d+)\b[^\[\]\n]{0,200}?)\]`)
	citeIDRe = regexp.MustCompile(`\bS\d+\b`)
	// The overview's lead-ins: "- **Shipped:**", "- **Needs Henrick:**".
	bucketRe = regexp.MustCompile(`<li>(<p>)?<strong>(Shipped|In flight|Needs [^<:]{1,40}|Blocked[^<:]{0,40}|Surprises):</strong>:?`)
)

func bucketKey(label string) string {
	switch {
	case label == "Shipped":
		return "shipped"
	case label == "In flight":
		return "flight"
	case strings.HasPrefix(label, "Needs"):
		return "needs"
	case strings.HasPrefix(label, "Blocked"):
		return "blocked"
	}
	return "surprises"
}

// decorate marks citations and the overview's buckets in rendered HTML so a
// reader can quiet the one and colour the other. It only wraps text the
// renderer already escaped; it adds no content of the model's.
func decorate(h string, idx Index) string {
	h = citeRe.ReplaceAllStringFunc(h, func(m string) string {
		inner := m[1 : len(m)-1]
		if strings.Contains(strings.NewReplacer("<code>", "", "</code>", "").Replace(inner), "<") {
			return m // spans other markup: leave it as written
		}
		cls := "cite"
		switch {
		case strings.Contains(inner, "unverified"):
			cls += " cite-unv"
		case strings.Contains(inner, "inferred"):
			cls += " cite-inf"
		}
		var tips []string
		seen := map[string]bool{}
		for _, id := range citeIDRe.FindAllString(inner, -1) {
			if t, ok := idx[id]; ok && !seen[id] {
				seen[id] = true
				tips = append(tips, id+" · "+t)
			}
		}
		title := ""
		if len(tips) > 0 {
			title = ` title="` + html.EscapeString(strings.Join(tips, "\n")) + `"`
		}
		return `<span class="` + cls + `"` + title + `>` + inner + `</span>`
	})
	return bucketRe.ReplaceAllStringFunc(h, func(m string) string {
		s := bucketRe.FindStringSubmatch(m)
		return `<li class="bucket b-` + bucketKey(s[2]) + `">` + s[1] + `<strong class="bucket-label">` + s[2] + `</strong>`
	})
}

// Render is a part's body as HTML, with citations and buckets marked.
func Render(p Part, idx Index) string {
	return decorate(HTML(p.Body()), idx)
}

// Page renders a saved brief as a self-contained, printable HTML page: the
// "proper artefact". It is the same text as the brief, re-rendered, never
// re-summarized.
func Page(m Meta, src string) string {
	created := time.Unix(m.CreatedAt, 0).Local()
	title := html.EscapeString(m.ProjectName + " brief · " + created.Format("2 Jan 2006"))
	idx := EvidenceIndex(src)
	parts := Split(src)
	var body, toc strings.Builder
	for _, p := range parts {
		inner := Render(p, idx)
		t := html.EscapeString(p.Title)
		switch p.Kind {
		case KindHead:
			if strings.TrimSpace(inner) != "" {
				fmt.Fprintf(&body, "<section class=\"sec\">%s</section>\n", inner)
			}
		case KindTask:
			fmt.Fprintf(&body, "<section class=\"task\" id=\"task-%d\"><header><span class=\"num\">%02d</span><h2>%s</h2></header>%s</section>\n", p.Task, p.Task, t, inner)
			fmt.Fprintf(&toc, "<li class=\"t\"><a href=\"#task-%d\"><span class=\"num\">%02d</span>%s</a></li>", p.Task, p.Task, t)
		case KindAppendix:
			fmt.Fprintf(&body, "<section class=\"appx\" id=\"appendix\"><h2>%s</h2>%s</section>\n", t, inner)
			fmt.Fprintf(&toc, "<li><a href=\"#appendix\">%s</a></li>", t)
		default:
			id := "sec-" + slug(p.Title)
			cls := "sec"
			if strings.EqualFold(p.Title, "Overview") {
				cls += " overview"
			}
			fmt.Fprintf(&body, "<section class=\"%s\" id=\"%s\"><h2>%s</h2>%s</section>\n", cls, id, t, inner)
			fmt.Fprintf(&toc, "<li><a href=\"#%s\">%s</a></li>", id, t)
		}
	}
	stats := []string{
		plural(m.Sessions, "session"),
		plural(m.Commits, "commit") + " of yours",
	}
	if m.OtherCommits > 0 {
		stats = append(stats, plural(m.OtherCommits, "commit")+" by others")
	}
	stats = append(stats, "written by "+html.EscapeString(m.Model)+" on "+created.Format("Mon 2 Jan 15:04"))
	hero := `<header class="hero"><div class="eyebrow">` + html.EscapeString(m.ProjectName) + ` · ` + html.EscapeString(windowLabel(m.Window)) + `</div>` +
		`<h1>` + time.Unix(m.Since, 0).Local().Format("Mon 2 Jan") + ` → ` + time.Unix(m.Until, 0).Local().Format("Mon 2 Jan") + `</h1>` +
		`<p class="stats">` + strings.Join(stats, ` <i>·</i> `) + `</p></header>`
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + title + `</title>
<style>` + pageCSS + `</style></head>
<body><div class="wrap">
<nav><div class="eyebrow">In this brief</div><ol>` + toc.String() + `</ol></nav>
<main>` + hero + body.String() + `</main>
</div></body></html>
`
}

func windowLabel(key string) string {
	if w, err := ParseWindow(key); err == nil {
		return w.Label
	}
	return key
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func slug(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// pageCSS is loom's own palette ("Islands": plum ground, peach accent), light
// by default and dark when the system is, so the artefact looks like loom.
const pageCSS = `
:root{--bg:#F3E9EC;--surface:#FFFBFB;--inset:#FCF7F5;--text:#3E3038;--dim:#75636C;--faint:#8E7A85;--line:rgba(70,56,66,.12);--accent:#EE9E86;--ink:#9C4E38;--soft:#FBE4DC;
--shipped:#2F8F75;--flight:#7F85BE;--needs:#C25F40;--blocked:#C5604C;--surprises:#A0783A}
@media (prefers-color-scheme: dark){:root{--bg:#171114;--surface:#241D21;--inset:#1A1417;--text:#F2E8EB;--dim:#BDABB4;--faint:#92818B;--line:rgba(255,255,255,.08);--ink:#F7BFAC;--soft:rgba(238,158,134,.14);
--shipped:#6FD3B0;--flight:#A5ABE0;--needs:#EE9E86;--blocked:#E8897A;--surprises:#E2C27A}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:16px/1.65 "Manrope",-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;-webkit-font-smoothing:antialiased}
.wrap{display:grid;grid-template-columns:250px minmax(0,1fr);gap:36px;max-width:1200px;margin:0 auto;padding:44px 28px}
nav{position:sticky;top:28px;align-self:start;font-size:13.5px;max-height:calc(100vh - 56px);overflow:auto}
nav ol{list-style:none;padding:0;margin:10px 0}
nav li{margin:2px 0}
nav a{display:flex;gap:8px;padding:5px 8px;border-radius:8px;color:var(--dim);text-decoration:none;line-height:1.35}
nav a:hover{background:var(--surface);color:var(--text)}
nav .num{font:600 11px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--ink)}
.eyebrow{text-transform:uppercase;letter-spacing:.12em;font-size:11px;font-weight:800;color:var(--faint)}
.hero{padding:6px 4px 22px}
.hero .eyebrow{color:var(--ink)}
.hero h1{font-size:34px;line-height:1.15;margin:8px 0 10px;letter-spacing:-.01em}
.stats{margin:0;color:var(--dim);font-size:14px}.stats i{font-style:normal;color:var(--faint);padding:0 4px}
main section{background:var(--surface);border:1px solid var(--line);border-radius:18px;padding:6px 34px 22px;margin-bottom:22px}
section.task{border-left:4px solid var(--accent)}
section.task header{display:flex;align-items:baseline;gap:12px;margin:22px 0 4px}
section.task header h2{margin:0}
.num{font:600 12px/1 ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--ink)}
section.task header .num{background:var(--soft);padding:5px 7px;border-radius:7px}
h2{font-size:22px;line-height:1.3;margin:24px 0 8px}
h3{font-size:11.5px;text-transform:uppercase;letter-spacing:.1em;color:var(--faint);font-weight:800;margin:24px 0 6px}
p,ul,ol{margin:8px 0}li{margin:4px 0}
a{color:var(--ink)}
em{color:var(--dim)}
code{background:var(--inset);border:1px solid var(--line);padding:0 5px;border-radius:5px;font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace}
pre{background:var(--inset);padding:12px 14px;border-radius:10px;overflow:auto}
pre code{padding:0;background:none;border:0}
table{border-collapse:collapse;width:100%;font-size:14px}
th,td{border-bottom:1px solid var(--line);padding:6px 8px;text-align:left;vertical-align:top}
hr{border:0;border-top:1px solid var(--line);margin:28px 0}
.cite{font:500 11px/1.4 ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--faint);white-space:nowrap;padding:0 2px}
.cite code{font-size:11px;background:none;border:0;padding:0;color:inherit}
.cite[title]{cursor:help;text-decoration:underline dotted;text-underline-offset:3px}
.cite-inf{font-style:italic}
.cite-unv{color:var(--blocked)}
.overview>ul{list-style:none;padding:0}
li.bucket{position:relative;padding:12px 0 12px 18px;border-top:1px solid var(--line);margin:0}
li.bucket:first-child{border-top:0}
li.bucket::before{content:"";position:absolute;left:0;top:21px;width:8px;height:8px;border-radius:50%;background:var(--c)}
.bucket-label{display:inline-block;margin-right:6px;font-size:11.5px;text-transform:uppercase;letter-spacing:.1em;color:var(--c)}
.b-shipped{--c:var(--shipped)}.b-flight{--c:var(--flight)}.b-needs{--c:var(--needs)}.b-blocked{--c:var(--blocked)}.b-surprises{--c:var(--surprises)}
.appx{font-size:14px}
@media (max-width:860px){.wrap{grid-template-columns:1fr;padding:22px 16px}nav{position:static;max-height:none}main section{padding:4px 18px 16px}.hero h1{font-size:26px}}
@media print{body{background:#fff;font-size:12pt}nav{display:none}.wrap{display:block;padding:0}main section{border:0;border-radius:0;padding:0;margin:0 0 18pt}section.task{border-left:0;break-before:page}h2,h3,header{break-after:avoid}li,p{break-inside:avoid}.appx{break-before:page}}
`
