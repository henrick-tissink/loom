# Mission Control UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restyle Loom's dashboard into the approved "Mission Control" framed panel (spec: `docs/superpowers/specs/2026-07-03-mission-control-ui-design.md`) with columns and an age column — zero behavior changes.

**Architecture:** A hand-composed rounded frame (lipgloss can't embed text in borders) wraps every view; rows are built from plain-text segments truncated per-segment BEFORE styling; ages come from tmux `session_activity` newly threaded through `status.Row.Activity`.

**Tech Stack:** Existing: Go, lipgloss, bubbletea. No new dependencies.

## Global Constraints

- Zero behavior change: keybindings, messages, polling, engine semantics untouched except the additive `status.Row.Activity int64` field.
- ALL existing tests must keep passing UNMODIFIED except where a task explicitly says to extend one.
- Truncate plain text before styling — never slice a styled string (ANSI corruption).
- All width math via `lipgloss.Width` (ANSI-aware); frame invariant: every rendered line is exactly `App.width` cells when width is set.
- Palette constants (the only colors allowed): accent `219`, alert `203`, running `214`, done `71`, meta `245`, chrome `240`.
- gofmt -w before every commit; conventional commits; `go vet ./... && go test ./...` green at each task end.

## File Structure

```
internal/status/engine.go      — add Activity to Row (Task A)
internal/status/engine_test.go — extend one assertion (Task A)
internal/ui/text.go            — NEW: humanAge, truncPlain, padPlain (Task A)
internal/ui/text_test.go       — NEW (Task A)
internal/ui/frame.go           — NEW: the panel renderer (Task B)
internal/ui/frame_test.go      — NEW (Task B)
internal/ui/styles.go          — palette consts + restyle (Task B)
internal/ui/app.go             — View/renderRow rewrite; snapMsg sets a.now (Task B)
internal/ui/app_test.go        — extend: width invariant, age wiring (Task B)
internal/ui/launcher.go        — unchanged except view() body returned frameless (Task B wraps it)
```

---

### Task A: Activity plumbing + text helpers

**Files:**
- Modify: `internal/status/engine.go` (Row struct + live-row build)
- Modify: `internal/status/engine_test.go` (extend TestPollLiveNeedsYou)
- Create: `internal/ui/text.go`, `internal/ui/text_test.go`

**Interfaces:**
- Produces: `status.Row.Activity int64` (unix seconds of last tmux activity; 0 = unknown), `ui.humanAge(now time.Time, unix int64) string`, `ui.truncPlain(s string, max int) string`, `ui.padPlain(s string, w int) string`.

- [ ] **Step 1: Write failing tests**

`internal/ui/text_test.go`:
```go
package ui

import (
	"testing"
	"time"
)

func TestHumanAge(t *testing.T) {
	now := time.Unix(100_000, 0)
	cases := map[int64]string{
		0:                "",      // unset → blank
		-5:               "",      // negative source → blank
		100_000 - 4:      "4s",
		100_000 - 59:     "59s",
		100_000 - 60:     "1m",
		100_000 - 3599:   "59m",
		100_000 - 3600:   "1h",
		100_000 - 86399:  "23h",
		100_000 - 86400:  "1d",
		100_000 + 50:     "0s", // future timestamp clamps to zero
	}
	for unix, want := range cases {
		if got := humanAge(now, unix); got != want {
			t.Errorf("humanAge(%d) = %q, want %q", unix, got, want)
		}
	}
}

func TestTruncPad(t *testing.T) {
	if got := truncPlain("parallax", 12); got != "parallax" {
		t.Errorf("no-trunc = %q", got)
	}
	if got := truncPlain("trend-wood-consult", 12); got != "trend-wood-…" {
		t.Errorf("trunc = %q", got)
	}
	if got := truncPlain("abc", 1); got != "…" {
		t.Errorf("trunc1 = %q", got)
	}
	if got := truncPlain("abc", 0); got != "" {
		t.Errorf("trunc0 = %q", got)
	}
	if got := padPlain("ab", 4); got != "ab  " {
		t.Errorf("pad = %q", got)
	}
	if got := padPlain("abcde", 4); got != "abcde" {
		t.Errorf("pad-over = %q (padPlain never truncates)", got)
	}
}
```

In `internal/status/engine_test.go`, extend `TestPollLiveNeedsYou`: after the existing Status assertion, add:
```go
	if snap.Live[0].Activity <= 0 {
		t.Fatalf("Activity not threaded: %d", snap.Live[0].Activity)
	}
```

- [ ] **Step 2: Run to verify failures**

Run: `go test ./internal/ui/ ./internal/status/ -run 'HumanAge|TruncPad|PollLiveNeedsYou' -v`
Expected: FAIL (undefined humanAge/truncPlain/padPlain; Activity field missing).

- [ ] **Step 3: Implement**

`internal/ui/text.go`:
```go
package ui

import (
	"fmt"
	"strings"
	"time"
)

// humanAge renders a compact age ("4s","2m","1h","2d"); blank when unset.
func humanAge(now time.Time, unix int64) string {
	if unix <= 0 {
		return ""
	}
	d := now.Unix() - unix
	if d < 0 {
		d = 0
	}
	switch {
	case d < 60:
		return fmt.Sprintf("%ds", d)
	case d < 3600:
		return fmt.Sprintf("%dm", d/60)
	case d < 86400:
		return fmt.Sprintf("%dh", d/3600)
	default:
		return fmt.Sprintf("%dd", d/86400)
	}
}

// truncPlain truncates PLAIN (unstyled) text to max runes, ellipsizing.
// Styling must happen after truncation — never slice styled strings.
func truncPlain(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// padPlain right-pads plain text to w runes; never truncates.
func padPlain(s string, w int) string {
	if n := w - len([]rune(s)); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
```

`internal/status/engine.go`: add field to Row:
```go
type Row struct {
	store.SessionRow
	Status   Status
	LastTool string
	Activity int64 // unix seconds of last tmux session activity; 0 = unknown
}
```
and in the live-row build inside `Poll`, where the Row is appended, add `Activity: activity[r.Name],` to the literal.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/ui/ ./internal/status/ -v` — Expected: PASS (status integration tests take ~8s).

- [ ] **Step 5: Commit**

```bash
git add internal/ui/text.go internal/ui/text_test.go internal/status/
git commit -m "feat: thread tmux activity into status rows; add age/trunc/pad text helpers"
```

---

### Task B: Frame renderer + dashboard restyle

**Files:**
- Create: `internal/ui/frame.go`, `internal/ui/frame_test.go`
- Rewrite: `internal/ui/styles.go`
- Modify: `internal/ui/app.go` (View, renderRow, uiRow, rebuildRows, snapMsg handler)
- Modify: `internal/ui/app_test.go` (extend fixture + add invariant test; existing assertions unchanged)

**Interfaces:**
- Consumes: Task A's helpers and `Row.Activity`.
- Produces: `frame(width int, title, right string, body []string, keybar string) string` — every output line exactly `width` cells.

- [ ] **Step 1: Write failing tests**

`internal/ui/frame_test.go`:
```go
package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestFrameWidthExact(t *testing.T) {
	for _, w := range []int{40, 60, 100} {
		out := frame(w, "LOOM", "4 live · 1 needs you", []string{"hello", ""}, "q quit")
		for i, line := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(line); lw != w {
				t.Errorf("width %d line %d: got %d cells: %q", w, i, lw, line)
			}
		}
	}
}

func TestFrameContainsParts(t *testing.T) {
	out := frame(60, "LOOM", "counts", []string{"body-line"}, "keybar-text")
	for _, want := range []string{"LOOM", "counts", "body-line", "keybar-text", "╭", "╰", "╮", "╯"} {
		if !strings.Contains(out, want) {
			t.Errorf("frame missing %q", want)
		}
	}
}

func TestFrameOverlongBodyLineIsHardClipped(t *testing.T) {
	long := strings.Repeat("x", 500)
	out := frame(40, "T", "", []string{long}, "")
	for _, line := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(line); lw != 40 {
			t.Errorf("overlong body not clipped: %d cells", lw)
		}
	}
}
```

Extend `internal/ui/app_test.go` — add (do NOT modify existing tests):
```go
func TestViewFrameInvariantAllViews(t *testing.T) {
	a := fixtureApp()
	views := []func(){
		func() {},                                    // dashboard
		func() { a.Update(key("n")) },                // launcher
		func() { a.Update(tea.KeyMsg{Type: tea.KeyEsc}); a.Update(key("x")) }, // confirm
		func() { a.Update(key("n")); a.Update(tea.KeyMsg{Type: tea.KeyEsc}); a.Update(key("t")) }, // tag
	}
	for i, setup := range views {
		setup()
		for j, line := range strings.Split(a.View(), "\n") {
			if lw := lipgloss.Width(line); lw != a.width {
				t.Fatalf("view %d line %d: %d cells (want %d): %q", i, j, lw, a.width, line)
			}
		}
	}
}

func TestViewNarrowNoPanic(t *testing.T) {
	a := fixtureApp()
	a.width, a.height = 40, 12
	_ = a.View() // must not panic; invariant checked by frame tests
}

func TestRowShowsAge(t *testing.T) {
	a := fixtureApp()
	a.now = time.Unix(2000, 0)
	a.snap.Live[1].Activity = 2000 - 120 // parallax row: 2m ago
	a.rebuildRows()
	if !strings.Contains(a.View(), "2m") {
		t.Fatal("age column missing")
	}
}
```
(Add `"strings"`, `"time"`, and `"github.com/charmbracelet/lipgloss"` to the test imports.)

- [ ] **Step 2: Run to verify failures**

Run: `go test ./internal/ui/ -v` — Expected: FAIL (frame undefined, a.now undefined).

- [ ] **Step 3: Implement frame.go**

```go
package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// frame hand-composes a rounded panel (lipgloss borders can't embed text):
// top border carries the title + right-aligned annotation, bottom the keybar.
// Every returned line is exactly `width` terminal cells.
func frame(width int, title, right string, body []string, keybar string) string {
	if width < 20 {
		width = 20
	}
	inner := width - 4 // "│ " … " │"

	var b strings.Builder
	b.WriteString(frameEdge(width, "╭", "╮", styTitle.Render(title), styMeta.Render(right)))
	b.WriteByte('\n')
	for _, line := range body {
		w := lipgloss.Width(line)
		if w > inner {
			// Defensive hard clip: builders should pre-fit lines; a styled
			// overflow is clipped unstyled rather than corrupting the frame.
			line = truncPlain(stripAnsi(line), inner)
			w = lipgloss.Width(line)
		}
		b.WriteString(styChrome.Render("│ ") + line + strings.Repeat(" ", inner-w) + styChrome.Render(" │"))
		b.WriteByte('\n')
	}
	b.WriteString(frameEdge(width, "╰", "╯", styHelp.Render(keybar), ""))
	return b.String()
}

// frameEdge builds "╭─ <left> ──…── <right> ─╮" at exactly `width` cells.
func frameEdge(width int, open, close, left, right string) string {
	var mid strings.Builder
	used := 2 // open+close corners
	if lw := lipgloss.Width(left); lw > 0 {
		mid.WriteString(styChrome.Render("─ ") + left + styChrome.Render(" "))
		used += lw + 3
	} else {
		mid.WriteString(styChrome.Render("─"))
		used++
	}
	rw := lipgloss.Width(right)
	fill := width - used - rw
	if rw > 0 {
		fill -= 2 // " <right> ─" spacing
	}
	if fill < 0 {
		fill = 0
	}
	mid.WriteString(styChrome.Render(strings.Repeat("─", fill)))
	if rw > 0 {
		mid.WriteString(" " + right + styChrome.Render(" ─"))
	}
	line := styChrome.Render(open) + mid.String() + styChrome.Render(close)
	// Exactness guard: pad or clip stray cells (styled-width math edge cases).
	if d := width - lipgloss.Width(line); d > 0 {
		line = strings.TrimSuffix(line, styChrome.Render(close)) +
			styChrome.Render(strings.Repeat("─", d)+close)
	}
	return line
}

// stripAnsi removes SGR sequences (defensive clip path only).
func stripAnsi(s string) string {
	var out strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
```

- [ ] **Step 4: Rewrite styles.go**

```go
// Package ui is Loom's Bubble Tea TUI: the mission-control dashboard.
package ui

import "github.com/charmbracelet/lipgloss"

// The Mission Control palette (spec 2026-07-03) — the only colors allowed.
var (
	colAccent = lipgloss.Color("219") // wordmark, cursor
	colAlert  = lipgloss.Color("203") // needs-you
	colRun    = lipgloss.Color("214") // running
	colDone   = lipgloss.Color("71")  // done
	colMeta   = lipgloss.Color("245") // secondary text
	colChrome = lipgloss.Color("240") // frame, rules, help
)

var (
	styTitle    = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styCursor   = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styNeedsYou = lipgloss.NewStyle().Foreground(colAlert)
	styRunning  = lipgloss.NewStyle().Foreground(colRun)
	styIdle     = lipgloss.NewStyle().Foreground(colMeta)
	styDone     = lipgloss.NewStyle().Foreground(colDone)
	styErr      = lipgloss.NewStyle().Foreground(colAlert).Bold(true)
	styMeta     = lipgloss.NewStyle().Foreground(colMeta)
	styChrome   = lipgloss.NewStyle().Foreground(colChrome)
	styHelp     = lipgloss.NewStyle().Foreground(colChrome)
)

func statusIcon(status string) string {
	switch status {
	case "needs_you":
		return styNeedsYou.Render("●")
	case "running":
		return styRunning.Render("◐")
	case "idle":
		return styIdle.Render("○")
	case "done":
		return styDone.Render("✓")
	case "error":
		return styErr.Render("✗")
	default:
		return styIdle.Render("·")
	}
}
```

- [ ] **Step 5: Modify app.go**

Changes (keep everything else byte-identical):

1. `uiRow` gains `activity int64`; `rebuildRows` sets `activity: r.Activity` for live rows (recent rows keep 0 — age uses EndedAt).
2. `App` gains `now time.Time`; the `snapMsg` case sets `a.now = time.Now()` before `rebuildRows`.
3. Replace `View()`, delete old `sectionFor`/`trimMeta` inline layout, add `renderRow`/`activityText`/`ageOf`/`sectionRule`:

```go
func (a *App) View() string {
	w := a.width
	if w == 0 {
		w = 80
	}
	switch a.view {
	case viewLauncher:
		return frame(w, "new session", "", strings.Split(a.form.view(), "\n"),
			"tab field · ←/→ value · ↵ launch · esc cancel")
	case viewConfirmKill:
		r := a.actionTarget
		return frame(w, "kill session", "",
			[]string{"", "  kill " + styNeedsYou.Render(r.label) + styMeta.Render(" ("+r.name+")") + " ?", ""},
			"y confirm · n/esc cancel")
	case viewTag:
		return frame(w, "tags", "", []string{"", "  " + a.tag.View(), ""},
			"↵ save · esc cancel")
	}

	inner := w - 4
	live, needs := 0, 0
	for _, r := range a.snap.Live {
		live++
		if r.Status == status.NeedsYou {
			needs++
		}
	}

	var body []string
	body = append(body, "")
	section := ""
	for i, r := range a.rows {
		if sec := sectionFor(r); sec != section {
			if section != "" {
				body = append(body, "")
			}
			section = sec
			body = append(body, sectionRule(sec, inner, sec == "NEEDS YOU"))
		}
		body = append(body, a.renderRow(i, r, inner))
	}
	if len(a.rows) == 0 {
		pad := (inner - lipgloss.Width("no sessions — press n to launch one")) / 2
		if pad < 0 {
			pad = 0
		}
		body = append(body, "", strings.Repeat(" ", pad)+styHelp.Render("no sessions — press n to launch one"), "")
	}
	if a.errStr != "" {
		body = append(body, "", styErr.Render(truncPlain("! "+a.errStr, inner)))
	}
	if a.deps.InsideTmux {
		body = append(body, styHelp.Render(truncPlain("(inside tmux — attach nests; F12 detaches)", inner)))
	}

	counts := fmt.Sprintf("%d live · %d needs you", live, needs)
	keybar := "↵ attach · n new · x kill · t tag · r reopen · q quit"
	if inner > lipgloss.Width(keybar)+24 {
		keybar += " · / search·soon · w workflows·soon"
	}
	return frame(w, "LOOM", counts, body, keybar)
}

func sectionRule(label string, inner int, alert bool) string {
	sty := styChrome
	if alert {
		sty = styNeedsYou
	}
	fill := inner - len([]rune(label)) - 1
	if fill < 0 {
		fill = 0
	}
	return sty.Bold(true).Render(label) + " " + styChrome.Render(strings.Repeat("─", fill))
}

// renderRow: cursor(2) icon(1)+1 project(12)+1 activity(flex)+1 model·mode(13)+1 age(4)
func (a *App) renderRow(i int, r uiRow, inner int) string {
	actW := inner - 36
	cursor := "  "
	if i == a.cursor {
		cursor = styCursor.Render("▸ ")
	}
	proj := padPlain(truncPlain(r.label, 12), 12)
	act := padPlain(truncPlain(activityText(r), actW), actW)
	meta := padPlain(truncPlain(metaText(r.model, r.mode), 13), 13)
	age := padPlain(humanAge(a.now, ageOf(r)), 4)
	if actW <= 0 { // ultra-narrow: drop the activity column entirely
		return cursor + statusIcon(r.status) + " " + styNeedsYouIf(r, proj)
	}
	return cursor + statusIcon(r.status) + " " + styNeedsYouIf(r, proj) + " " +
		styMeta.Render(act) + " " + styMeta.Render(meta) + " " + styMeta.Render(age)
}

// styNeedsYouIf highlights the project name on attention rows.
func styNeedsYouIf(r uiRow, s string) string {
	if r.status == "needs_you" {
		return styNeedsYou.Bold(true).Render(s)
	}
	return s
}

func activityText(r uiRow) string {
	if r.recent {
		switch {
		case r.status == "error":
			return fmt.Sprintf("error · exit %d", r.row.ExitCode)
		case r.row.ExitCode == 0:
			return "done"
		default:
			return "ended"
		}
	}
	hint := ""
	switch r.status {
	case "running":
		if r.lastTool != "" {
			hint = "⏺ " + r.lastTool
		} else {
			hint = "working"
		}
	case "needs_you":
		hint = "reply ready"
	default:
		hint = "your turn"
	}
	if r.row.SeedStatus == "failed" {
		hint += " · seed failed"
	}
	return hint
}

func ageOf(r uiRow) int64 {
	if r.recent {
		return r.row.EndedAt
	}
	return r.activity
}

func metaText(model, mode string) string {
	if model == "" {
		model = "default"
	}
	if mode == "" {
		mode = "normal"
	}
	return model + "·" + mode
}
```
Keep `sectionFor` as-is. Delete the old `trimMeta`. Add `"github.com/charmbracelet/lipgloss"` to app.go imports. NOTE: the confirm-kill view uses the captured `a.actionTarget` (exists since the final-review fix) — verify the field name in the current code and use it exactly.

- [ ] **Step 6: Run all tests**

Run: `go test ./internal/ui/ -v -count=1` then `go test ./... && go vet ./...`
Expected: ALL PASS including the pre-existing app tests unmodified.

- [ ] **Step 7: Commit**

```bash
git add internal/ui/
git commit -m "feat: mission-control framed dashboard with columns and age"
```

---

### Task C: Visual verification + polish

**Files:** whatever nits the eyeball pass surfaces (small diffs only).

- [ ] **Step 1: Capture real renders**

```bash
go build -o loom ./cmd/loom
for W in 80 120 46; do
  tmux -L loomviz new-session -d -s viz -x $W -y 30 './loom' && sleep 3
  echo "=== width $W ==="; tmux -L loomviz capture-pane -p -t '=viz:' | grep -v '^ *$'
  tmux -L loomviz send-keys -t '=viz:' n; sleep 1
  echo "--- launcher ---"; tmux -L loomviz capture-pane -p -t '=viz:' | grep -v '^ *$'
  tmux -L loomviz kill-server
done
```

- [ ] **Step 2: Compare against the spec reference**

Check: frame corners/edges continuous at every width; title + counts on the top border; keybar on the bottom border; columns aligned across rows; section rules run edge to edge; NEEDS YOU rule red only when populated (launch nothing — verify via test if no live needs-you row exists); no wrapped/overflowing line anywhere. Fix any nit found (small, targeted diffs), re-run the capture and full test suite after each fix.

- [ ] **Step 3: Full suite + commit**

```bash
gofmt -l . && go vet ./... && go test -count=1 ./...
git add -A && git commit -m "polish: visual nits from mission-control eyeball pass"
```
(If zero nits: skip the commit and say so.)

---

## Self-Review (performed during plan writing)

1. **Spec coverage:** frame+wordmark+counts (B), keybar-in-border (B), section rules w/ conditional red (B `sectionRule`), columns+truncate-before-style (B `renderRow`), age via Activity (A), activity texts incl. seed-failed (B `activityText`), palette consts (B styles), dialogs framed (B View cases), empty state centered (B), narrow degrade (B actW guard + tests), invariant tests (B), humanAge tests (A), no behavior change (constraint).
2. **Placeholders:** none; all code complete.
3. **Type consistency:** `frame` signature matches all call sites; `Row.Activity`→`uiRow.activity`→`ageOf` consistent; `a.actionTarget` flagged for verification against current code.
