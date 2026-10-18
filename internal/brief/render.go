package brief

import (
	"bytes"
	"fmt"
	"html"
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

// Part is a slice of a brief: the text before the first task, one task, or
// the rest after the last task.
type Part struct {
	Task  int    // 1-based task number, 0 for a non-task part
	Title string // the task title
	MD    string
}

// Split cuts a brief at its "## Task: " headings. A task runs until the next
// level-1 or level-2 heading, so "## Decisions and consultations" and the
// appendix are not swallowed into the last task.
func Split(src string) []Part {
	lines := strings.Split(src, "\n")
	var parts []Part
	cur := Part{}
	var buf []string
	n := 0
	flush := func() {
		cur.MD = strings.Join(buf, "\n")
		if strings.TrimSpace(cur.MD) != "" {
			parts = append(parts, cur)
		}
		buf = nil
	}
	inFence := false
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
		}
		if !inFence {
			switch {
			case strings.HasPrefix(l, TaskHeading):
				flush()
				n++
				cur = Part{Task: n, Title: strings.TrimSpace(strings.TrimPrefix(l, TaskHeading))}
			case cur.Task > 0 && (strings.HasPrefix(l, "## ") || strings.HasPrefix(l, "# ")):
				flush()
				cur = Part{}
			}
		}
		buf = append(buf, l)
	}
	flush()
	return parts
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

// Page renders a saved brief as a self-contained, printable HTML page: the
// "proper artefact". It is the same text as the brief, re-rendered, never
// re-summarized.
func Page(m Meta, src string) string {
	title := html.EscapeString(m.ProjectName + " brief · " + time.Unix(m.CreatedAt, 0).Local().Format("2 Jan 2006"))
	var body strings.Builder
	for _, p := range Split(src) {
		if p.Task > 0 {
			fmt.Fprintf(&body, "<section class=\"task\" id=\"task-%d\">%s</section>\n", p.Task, HTML(p.MD))
		} else {
			fmt.Fprintf(&body, "<section>%s</section>\n", HTML(p.MD))
		}
	}
	var toc strings.Builder
	for _, p := range Split(src) {
		if p.Task > 0 {
			fmt.Fprintf(&toc, "<li><a href=\"#task-%d\">%s</a></li>", p.Task, html.EscapeString(p.Title))
		}
	}
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + title + `</title>
<style>` + pageCSS + `</style></head>
<body><div class="wrap">
<nav><div class="eyebrow">Tasks</div><ol>` + toc.String() + `</ol></nav>
<main>` + body.String() + `</main>
</div></body></html>
`
}

const pageCSS = `
:root{--bg:#f7f6f3;--surface:#ffffff;--text:#1d1d1f;--muted:#6b6b70;--line:#e4e2dc;--accent:#3d5afe;--code:#f1efe9}
@media (prefers-color-scheme: dark){:root{--bg:#141416;--surface:#1c1c20;--text:#ececef;--muted:#9a9aa2;--line:#2c2c33;--accent:#8c9eff;--code:#24242a}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:16px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Inter,sans-serif}
.wrap{display:grid;grid-template-columns:240px minmax(0,1fr);gap:32px;max-width:1180px;margin:0 auto;padding:40px 24px}
nav{position:sticky;top:24px;align-self:start;font-size:14px}
nav ol{padding-left:18px;margin:8px 0}
nav li{margin:6px 0}
nav a{color:var(--muted);text-decoration:none}
nav a:hover{color:var(--accent)}
.eyebrow{text-transform:uppercase;letter-spacing:.08em;font-size:12px;color:var(--muted)}
main section{background:var(--surface);border:1px solid var(--line);border-radius:14px;padding:8px 32px 20px;margin-bottom:24px}
main section.task{border-left:4px solid var(--accent)}
h1{font-size:30px;line-height:1.2;margin:24px 0 8px}
h2{font-size:22px;margin:24px 0 8px}
h3{font-size:13px;text-transform:uppercase;letter-spacing:.07em;color:var(--muted);margin:22px 0 6px}
a{color:var(--accent)}
code{background:var(--code);padding:1px 5px;border-radius:5px;font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace}
pre{background:var(--code);padding:12px 14px;border-radius:10px;overflow:auto}
pre code{padding:0;background:none}
table{border-collapse:collapse;width:100%;font-size:14px}
th,td{border-bottom:1px solid var(--line);padding:6px 8px;text-align:left;vertical-align:top}
hr{border:0;border-top:1px solid var(--line);margin:32px 0}
@media (max-width:820px){.wrap{grid-template-columns:1fr;padding:20px 16px}nav{position:static}main section{padding:4px 18px 14px}}
@media print{nav{display:none}.wrap{display:block;padding:0}main section{break-inside:avoid-page;border:0}}
`
