package ui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/henricktissink/loom/internal/status"
)

// wallCapture is the cached per-session captured-pane state (spec §3.3/§3.4),
// keyed by session name on App.wallCaptures. lines is the split, per-line
// TrimRight'd (spec §6: "tails are input-box-heavy — TrimRight applied")
// content of the last CapturePane call that succeeded; err true means that
// call failed — the cell keeps rendering (peek precedent: "(pane
// unavailable)") rather than disappearing, and gates ↵ off for that cell
// specifically (spec §3.4).
type wallCapture struct {
	lines []string
	err   bool
}

// openWall enters the wall (spec §3, the `W` key): a.wallOrder/wallSelected
// are already current — applyWallOrder runs unconditionally on every snapMsg
// regardless of view (see Update), so there's nothing to rebuild here — only
// the capture generation needs bumping before firing an IMMEDIATE capture cmd
// (same "fresh state + immediate refresh cmd" shape as openSearch/
// openWorkflows, rather than waiting up to 1.5s for the first tick).
func (a *App) openWall() tea.Cmd {
	a.view = viewWall
	a.wallSeq++
	return a.wallCaptureCmd()
}

// applyWallOrder recomputes a.wallOrder (spec §3.1: STABLE CreatedAt-then-
// Name order, deterministic regardless of a.snap.Live's own slice order —
// NEVER the attention order rebuildRows computes) and resolves the name-keyed
// selection (spec §3.5): unchanged if wallSelected is still present in the
// new order; otherwise a nearest-neighbor fallback — scanning outward from
// the selected name's OLD position for the closest name that's still
// present — or the first row if there was no prior selection at all. Called
// unconditionally on every snapMsg (see Update), independent of the current
// view, so the wall is never looking at stale ordering/selection the instant
// it's opened.
func (a *App) applyWallOrder() {
	old := a.wallOrder
	oldIdx := wallIndexOf(old, a.wallSelected)
	a.wallOrder = stableWallOrder(a.snap.Live)

	// Hygiene: drop cached captures for names no longer live, so a dead
	// session's last-seen pane content can't reappear if its name is ever
	// somehow reused.
	if len(a.wallCaptures) > 0 {
		alive := make(map[string]bool, len(a.wallOrder))
		for _, r := range a.wallOrder {
			alive[r.Name] = true
		}
		for name := range a.wallCaptures {
			if !alive[name] {
				delete(a.wallCaptures, name)
			}
		}
	}

	if len(a.wallOrder) == 0 {
		a.wallSelected = ""
		return
	}
	if wallIndexOf(a.wallOrder, a.wallSelected) >= 0 {
		return // still present — nothing to do.
	}
	if oldIdx < 0 {
		a.wallSelected = a.wallOrder[0].Name
		return
	}
	for d := 1; d < len(old); d++ {
		if name := wallNameAt(old, oldIdx-d); name != "" && wallIndexOf(a.wallOrder, name) >= 0 {
			a.wallSelected = name
			return
		}
		if name := wallNameAt(old, oldIdx+d); name != "" && wallIndexOf(a.wallOrder, name) >= 0 {
			a.wallSelected = name
			return
		}
	}
	a.wallSelected = a.wallOrder[0].Name
}

// stableWallOrder sorts a COPY of rows by (CreatedAt, Name) — a full,
// deterministic ordering (Name breaks any CreatedAt tie), so the result
// never depends on rows' own input order and never changes for the same set
// of sessions regardless of status changes (spec §3.1 I4: attention order is
// the dashboard's job — reordering the wall on a status flip would "teleport
// the grid under the reader").
func stableWallOrder(rows []status.Row) []status.Row {
	out := make([]status.Row, len(rows))
	copy(out, rows)
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func wallIndexOf(rows []status.Row, name string) int {
	if name == "" {
		return -1
	}
	for i, r := range rows {
		if r.Name == name {
			return i
		}
	}
	return -1
}

func wallNameAt(rows []status.Row, i int) string {
	if i < 0 || i >= len(rows) {
		return ""
	}
	return rows[i].Name
}

// wallSelectedRow resolves a.wallSelected against a.wallOrder.
func (a *App) wallSelectedRow() (status.Row, bool) {
	if i := wallIndexOf(a.wallOrder, a.wallSelected); i >= 0 {
		return a.wallOrder[i], true
	}
	return status.Row{}, false
}

// wallMove shifts the selection by delta positions in a.wallOrder, clamping
// at either end (no wrap — same discipline as the dashboard cursor).
func (a *App) wallMove(delta int) {
	if len(a.wallOrder) == 0 {
		return
	}
	idx := wallIndexOf(a.wallOrder, a.wallSelected)
	if idx < 0 {
		a.wallSelected = a.wallOrder[0].Name
		return
	}
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx > len(a.wallOrder)-1 {
		idx = len(a.wallOrder) - 1
	}
	a.wallSelected = a.wallOrder[idx].Name
}

// clampInt clamps v to [lo, hi]; if hi < lo (a degenerate tiny-terminal
// bound), lo wins.
func clampInt(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// wallPageSize computes tailH and perPage (rows-that-fit × 2, spec §3.2) from
// the wall's available body height. tailH = clamp(6, 1, bodyH−2) (spec's
// tiny-terminal degrade — the M2 finding): a very short terminal still gets
// at least 1 tail line and at least one row-that-fits, never a divide-by-zero
// or an empty page.
func (a *App) wallPageSize() (perPage, tailH int) {
	bodyH := a.height - 2
	if bodyH < 1 {
		bodyH = 1
	}
	tailH = clampInt(6, 1, bodyH-2)
	cellH := tailH + 2 // 1 header + tailH + 1 separator
	rows := bodyH / cellH
	if rows < 1 {
		rows = 1
	}
	return rows * 2, tailH
}

// wallPageBounds returns the [start,end) slice bounds into a.wallOrder for
// the page CONTAINING the current selection (spec §3.2/§3.5: paging follows
// selection, there is no separate "next page" key) — the page the selected
// row's index falls into, at perPage sessions per page.
func (a *App) wallPageBounds(perPage int) (start, end int) {
	total := len(a.wallOrder)
	if total == 0 || perPage <= 0 {
		return 0, 0
	}
	idx := wallIndexOf(a.wallOrder, a.wallSelected)
	if idx < 0 {
		idx = 0
	}
	page := idx / perPage
	start = page * perPage
	end = start + perPage
	if end > total {
		end = total
	}
	return start, end
}

// wallColumnWidths splits inner (minus the 1-col gutter) into 2 columns; when
// the split is uneven, the extra cell goes to the RIGHT column (spec §3.2,
// "M1 corrected").
func wallColumnWidths(inner int) (colL, colR int) {
	usable := inner - 1
	if usable < 0 {
		usable = 0
	}
	colL = usable / 2
	colR = usable - colL
	return
}

// wallCaptureCmd builds the ONE one-shot tea.Cmd that captures the VISIBLE
// page's panes SEQUENTIALLY (spec §3.3, measured ~44ms for 6) — names are
// snapshotted from a.wallOrder now (captured-target discipline), and gen
// pins the result to the CURRENT a.wallSeq so Update can discard it outright
// if a newer capture has since been fired. nil Tmux or an empty page → nil
// cmd (Deps nil-safety contract, peekCmd precedent).
func (a *App) wallCaptureCmd() tea.Cmd {
	tm := a.deps.Tmux
	if tm == nil {
		return nil
	}
	perPage, _ := a.wallPageSize()
	start, end := a.wallPageBounds(perPage)
	if start >= end {
		return nil
	}
	names := make([]string, end-start)
	for i, r := range a.wallOrder[start:end] {
		names[i] = r.Name
	}
	gen := a.wallSeq
	return func() tea.Msg {
		results := make([]wallCaptureResult, 0, len(names))
		for _, name := range names {
			out, err := tm.CapturePane(name)
			if err != nil {
				results = append(results, wallCaptureResult{name: name, err: true})
				continue
			}
			results = append(results, wallCaptureResult{name: name, lines: splitCapture(out)})
		}
		return wallMsg{gen: gen, results: results}
	}
}

// splitCapture turns a raw CapturePane blob into lines: trailing newline
// trimmed (peekCmd precedent), each individual line right-trimmed of spaces
// (spec §6 accepted limit: "tails are input-box-heavy — TrimRight applied").
func splitCapture(out string) []string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	lines := strings.Split(out, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}

// lastLines returns exactly n lines: the LAST n of lines (spec §3.1: "last
// tailH lines of CapturePane"), left-padded with blanks at the TOP when
// there are fewer than n — so short content stays anchored to the bottom of
// the cell, same as a real terminal tail.
func lastLines(lines []string, n int) []string {
	out := make([]string, n)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	pad := n - len(lines)
	for i, l := range lines {
		out[pad+i] = l
	}
	return out
}

// padCells right-pads s with spaces to exactly w terminal CELLS (never
// truncates — callers that might overflow use truncPadCells instead).
func padCells(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// truncPadCells truncates PLAIN (unstyled) s to at most w terminal CELLS
// (CJK-safe — reuses snippet.go's truncCells, which measures by cell width,
// not rune count) and pads it to EXACTLY w cells. Style must be applied
// AFTER this (truncate/pad-before-style invariant, same as truncPlain
// elsewhere in this package).
func truncPadCells(s string, w int) string {
	if w <= 0 {
		return ""
	}
	text, _, truncated := truncCells([]rune(s), w)
	out := string(text)
	if truncated {
		out += "…"
	}
	return padCells(out, w)
}

// updateWallKeys handles keys while the wall is open (spec §3.5): ↓/↑/j/k
// move the name-keyed selection (no wrap); ↵ attaches the selected LIVE cell
// — gated off when its last capture is known to have errored (spec §3.4);
// esc back to the dashboard; q/ctrl+c quit. Read-only otherwise.
func (a *App) updateWallKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return a, tea.Quit
	}
	switch msg.String() {
	case "q":
		return a, tea.Quit
	case "esc":
		a.view = viewDash
		return a, nil
	case "j", "down":
		a.wallMove(1)
		return a, nil
	case "k", "up":
		a.wallMove(-1)
		return a, nil
	case "enter":
		r, ok := a.wallSelectedRow()
		if !ok || a.deps.Tmux == nil {
			return a, nil
		}
		if wc, capOK := a.wallCaptures[r.Name]; capOK && wc.err {
			return a, nil // spec §3.4: a known capture error gates ↵ off.
		}
		cmd := a.deps.Tmux.AttachCmd(r.Name)
		return a, tea.ExecProcess(cmd, func(err error) tea.Msg { return attachedMsg{err} })
	}
	return a, nil
}

// renderWall renders the wall frame (spec §3): a 2-column, 1-col-gutter grid
// of the visible page's cells, page indicator as the frame's right
// annotation. Every composed line is built to EXACTLY `inner` cells before
// being handed to frame() (frame() itself pads/clips defensively, but these
// lines are exact by construction, per the grid-composition discipline).
func (a *App) renderWall(w int) string {
	inner := w - 4
	total := len(a.wallOrder)
	perPage, tailH := a.wallPageSize()
	start, end := a.wallPageBounds(perPage)
	colL, colR := wallColumnWidths(inner)

	var body []string
	body = append(body, "")
	for i := start; i < end; i += 2 {
		left := a.wallCellRows(i, colL, tailH)
		var right []string
		if i+1 < end {
			right = a.wallCellRows(i+1, colR, tailH)
		} else {
			right = blankCell(colR, tailH)
		}
		for ln := range left {
			body = append(body, left[ln]+" "+right[ln])
		}
	}
	if total == 0 {
		msg := "no live sessions"
		pad := (inner - len([]rune(msg))) / 2
		if pad < 0 {
			pad = 0
		}
		body = append(body, "", strings.Repeat(" ", pad)+styHelp.Render(msg), "")
	}

	right := ""
	if total > 0 {
		right = fmt.Sprintf("%d–%d of %d", start+1, end, total)
	}
	return frame(w, "wall", right, body, "↓/↑ select · ↵ attach · esc dashboard · q quit")
}

// wallCellRows renders one populated cell — exactly tailH+2 lines
// (1 header + tailH + 1 separator), each exactly colW cells wide (spec
// §3.2/§3.1): a header line (icon + project + title/tool hint, cursor-
// highlighted when selected), the last tailH lines of its cached capture (or
// "(pane unavailable)" — spec §3.4 — when its last capture errored; blank
// when nothing has been captured for it yet at all), and a dim separator
// rule.
func (a *App) wallCellRows(idx, colW, tailH int) []string {
	r := a.wallOrder[idx]
	lines := make([]string, 0, tailH+2)
	lines = append(lines, wallHeaderLine(r, colW, r.Name == a.wallSelected))

	var content []string
	if wc, ok := a.wallCaptures[r.Name]; ok {
		if wc.err {
			content = make([]string, tailH)
			if tailH > 0 {
				content[0] = "(pane unavailable)"
			}
		} else {
			content = lastLines(wc.lines, tailH)
		}
	} else {
		content = make([]string, tailH) // not yet captured: blank, not an error.
	}
	for _, c := range content {
		lines = append(lines, truncPadCells(c, colW))
	}

	lines = append(lines, styChrome.Render(strings.Repeat("─", colW)))
	return lines
}

// blankCell renders the filler for a page's trailing odd cell (spec §3.2:
// "extra cell to the RIGHT column" leaves no session there when the page's
// count is odd) — same exact tailH+2 line shape, entirely blank.
func blankCell(colW, tailH int) []string {
	blank := strings.Repeat(" ", colW)
	lines := make([]string, 0, tailH+2)
	lines = append(lines, blank)
	for i := 0; i < tailH; i++ {
		lines = append(lines, blank)
	}
	lines = append(lines, styChrome.Render(strings.Repeat("─", colW)))
	return lines
}

// wallHeaderLine composes one cell's header (spec §3.1): status glyph +
// project label + title/tool hint, PLAIN text truncated/padded to EXACTLY
// colW cells BEFORE any style is applied (truncate-before-style invariant) —
// selection gets exactly one style, styCursor, over the whole finished line.
func wallHeaderLine(r status.Row, colW int, selected bool) string {
	glyph, _ := statusGlyphWord(string(r.Status))
	text := glyph + " " + r.ProjectLabel
	hint := r.Title
	if hint == "" {
		hint = r.LastTool
	}
	if hint != "" {
		text += " · " + hint
	}
	text = truncPadCells(text, colW)
	if selected {
		return styCursor.Render(text)
	}
	return text
}
