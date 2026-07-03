package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

const pollInterval = 1500 * time.Millisecond

// searchDebounce is the keystroke→query delay (spec §6): fast typing only
// fires one FTS query per pause, not one per rune.
const searchDebounce = 200 * time.Millisecond

type view int

const (
	viewDash view = iota
	viewLauncher
	viewConfirmKill
	viewTag
	viewPeek
	viewSearch
	viewDetail
)

type Deps struct {
	Engine     *status.Engine
	Launcher   *session.Launcher
	Projects   []registry.Project
	Tmux       *tmux.Client
	InsideTmux bool

	// Store/IndexerStatus/Summarizer back the memory search + detail views
	// (Task 2/3). All query paths are nil-safe: a nil Store means every
	// search/status command no-ops rather than panicking, so Deps{} (as
	// used by existing dashboard-only tests) keeps working unmodified.
	Store         *store.Store
	IndexerStatus func() memory.Status
	Summarizer    *memory.Summarizer
}

// uiRow is one selectable dashboard line (live or recent).
type uiRow struct {
	name     string
	label    string
	status   string
	lastTool string
	model    string
	mode     string
	activity int64
	recent   bool
	row      store.SessionRow
	title    string
	ctx      int64
}

type App struct {
	deps   Deps
	snap   status.Snapshot
	rows   []uiRow
	cursor int
	view   view
	form   launcherForm
	tag    textinput.Model
	errStr string
	width  int
	height int
	now    time.Time

	// actionTarget is the row captured at the moment viewConfirmKill/viewTag
	// is opened. Rows reorder under the cursor every poll (1.5s) as statuses
	// change, so the kill/tag-save handlers must act on this captured target
	// rather than re-reading selected() at confirm/save time (finding 5).
	actionTarget uiRow

	// peekTarget/peekContent: peek acts on a target captured at open time,
	// same captured-target discipline as actionTarget above.
	peekTarget struct {
		name  string
		label string
	}
	peekContent string

	// Search view state (spec §6). searchInput is fresh + focused each time
	// '/' opens the view. searchSeq is the debounce generation counter: every
	// keystroke that changes the input bumps it, and a stale debounce/result
	// message (captured seq or query no longer current) is discarded — same
	// staleness discipline as peekMsg above.
	searchInput  textinput.Model
	searchSeq    int64
	searchHits   []store.SearchHit
	searchCursor int
	searchCount  int64 // cached TranscriptCount(), refreshed on open + each tick
	searchActive bool  // cached IndexerStatus().Active, ditto

	// detailTarget: the hit captured at the moment viewDetail is opened
	// (same captured-target discipline as actionTarget/peekTarget). Task 3
	// fleshes out the rest of the detail view.
	detailTarget store.SearchHit
}

type (
	tickMsg     time.Time
	pollNowMsg  struct{} // one-shot: "poll now", does NOT arm a new tick chain
	snapMsg     status.Snapshot
	errMsg      struct{ err error }
	attachedMsg struct{ err error }
	peekMsg     struct{ name, content string }

	// searchDebounceMsg fires searchDebounce after a keystroke that changed
	// the search input; seq pins it to the keystroke generation that
	// scheduled it (spec §6 debounce pattern).
	searchDebounceMsg struct{ seq int64 }

	// searchStatusMsg carries the cheap (COUNT-only, no FTS query) refresh
	// of the search frame's right annotation, fired on search-open and every
	// tick while in viewSearch.
	searchStatusMsg struct {
		active bool
		count  int64
	}

	// searchResultsMsg is the result of an actual FTS query. Applied only
	// when query still matches the live input value — any older in-flight
	// query is discarded as stale (peekMsg precedent).
	searchResultsMsg struct {
		query string
		hits  []store.SearchHit
	}
)

func NewApp(deps Deps) *App {
	ti := textinput.New()
	ti.Placeholder = "tags (comma separated)"
	return &App{deps: deps, form: newLauncherForm(deps.Projects), tag: ti}
}

func (a *App) Init() tea.Cmd { return tea.Batch(a.pollCmd(), tickAfter()) }

func tickAfter() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (a *App) pollCmd() tea.Cmd {
	eng := a.deps.Engine
	if eng == nil {
		return nil
	}
	return func() tea.Msg {
		snap, err := eng.Poll(time.Now())
		if err != nil {
			return errMsg{err}
		}
		return snapMsg(snap)
	}
}

// rebuildRows flattens the snapshot into the attention-queue order:
// NeedsYou → Running → Idle/Unknown → Recent (spec §4.1).
func (a *App) rebuildRows() {
	var needs, running, idle, recent []uiRow
	for _, r := range a.snap.Live {
		u := uiRow{name: r.Name, label: r.ProjectLabel, status: string(r.Status),
			lastTool: r.LastTool, model: r.Model, mode: r.Mode, activity: r.Activity, row: r.SessionRow,
			title: r.Title, ctx: r.CtxTokens}
		switch r.Status {
		case status.NeedsYou:
			needs = append(needs, u)
		case status.Running:
			running = append(running, u)
		default:
			idle = append(idle, u)
		}
	}
	for _, r := range a.snap.Recent {
		recent = append(recent, uiRow{name: r.Name, label: r.ProjectLabel,
			status: r.LastStatus, model: r.Model, mode: r.Mode, recent: true, row: r, title: r.Title})
	}
	a.rows = append(append(append(needs, running...), idle...), recent...)
	if a.cursor >= len(a.rows) {
		a.cursor = max(0, len(a.rows)-1)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = m.Width, m.Height
		return a, nil
	case tickMsg:
		// ONE tickAfter per tickMsg, always — any view-specific extra work
		// rides along as an additional one-shot cmd in the same batch, never
		// as a second tick chain (finding 1 precedent).
		switch a.view {
		case viewPeek:
			// live refresh: same tick chain, plus a one-shot peek re-capture.
			return a, tea.Batch(a.pollCmd(), tickAfter(), a.peekCmd())
		case viewSearch:
			// self-healing first-run (spec §6): re-check indexer status/count
			// every tick while searching; searchStatusMsg's handler decides
			// whether to re-fire the current query (active, or an
			// active→inactive edge).
			return a, tea.Batch(a.pollCmd(), tickAfter(), a.searchStatusCmd())
		}
		return a, tea.Batch(a.pollCmd(), tickAfter())
	case pollNowMsg:
		// one-shot refresh after launch/kill/resume — NOT a new tick chain.
		// Returning tickMsg here (as launch/kill/resume commands used to)
		// would permanently add another perpetual pollInterval tick loop per
		// action, stacking concurrent Engine.Poll goroutines (finding 1).
		return a, a.pollCmd()
	case snapMsg:
		a.snap = status.Snapshot(m)
		a.errStr = ""
		a.now = time.Now()
		a.rebuildRows()
		if len(m.NewlyNeedsYou) > 0 {
			return a, notifyCmd(m.NewlyNeedsYou)
		}
		return a, nil
	case errMsg:
		a.errStr = m.err.Error()
		return a, nil
	case attachedMsg:
		if m.err != nil {
			a.errStr = "attach failed: " + m.err.Error()
		}
		return a, a.pollCmd()
	case peekMsg:
		if a.view == viewPeek && m.name == a.peekTarget.name {
			a.peekContent = m.content
		}
		return a, nil
	case searchDebounceMsg:
		// stale (a newer keystroke has since bumped searchSeq) → discarded.
		if m.seq != a.searchSeq {
			return a, nil
		}
		if q := a.searchInput.Value(); q != "" {
			return a, a.searchQueryCmd(q)
		}
		return a, nil
	case searchStatusMsg:
		wasActive := a.searchActive
		a.searchActive = m.active
		a.searchCount = m.count
		if a.view != viewSearch {
			return a, nil
		}
		// re-fire the current query while the indexer is active, or once
		// more on the active→inactive edge (self-healing first-run results).
		if m.active || (wasActive && !m.active) {
			if q := a.searchInput.Value(); q != "" {
				return a, a.searchQueryCmd(q)
			}
		}
		return a, nil
	case searchResultsMsg:
		// stale (the input has changed since this query was fired) →
		// discarded — same discipline as peekMsg.
		if m.query != a.searchInput.Value() {
			return a, nil
		}
		a.searchHits = m.hits
		if a.searchCursor >= len(a.searchHits) {
			a.searchCursor = max(0, len(a.searchHits)-1)
		}
		return a, nil
	case tea.KeyMsg:
		return a.updateKeys(m)
	}
	return a, nil
}

func (a *App) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch a.view {
	case viewLauncher:
		switch msg.Type {
		case tea.KeyEsc:
			a.view = viewDash
			return a, nil
		case tea.KeyEnter:
			r, ok := a.form.Recipe()
			a.view = viewDash
			if !ok || a.deps.Launcher == nil {
				return a, nil
			}
			l := a.deps.Launcher
			w, h := a.width, a.height
			return a, func() tea.Msg {
				if _, err := l.Launch(r, w, h, time.Now()); err != nil {
					return errMsg{err}
				}
				return pollNowMsg{}
			}
		}
		return a, a.form.update(msg)

	case viewConfirmKill:
		s := msg.String()
		if s == "y" {
			a.view = viewDash
			if a.deps.Tmux != nil {
				name := a.actionTarget.name
				tm := a.deps.Tmux
				return a, func() tea.Msg {
					if err := tm.KillSession(name); err != nil {
						return errMsg{err}
					}
					return pollNowMsg{}
				}
			}
			return a, nil
		}
		if s == "n" || msg.Type == tea.KeyEsc {
			a.view = viewDash
		}
		return a, nil

	case viewTag:
		// ctrl+c must quit even from a free-text input view — checked BEFORE
		// forwarding to textinput, which would otherwise swallow it as a
		// (no-op) keystroke (red-team finding; same fix as viewSearch below).
		if msg.String() == "ctrl+c" {
			return a, tea.Quit
		}
		switch msg.Type {
		case tea.KeyEsc:
			a.view = viewDash
			return a, nil
		case tea.KeyEnter:
			a.view = viewDash
			if a.deps.Launcher != nil {
				_ = a.deps.Launcher.Store.SetTags(a.actionTarget.name, a.tag.Value())
			}
			return a, a.pollCmd()
		}
		var cmd tea.Cmd
		a.tag, cmd = a.tag.Update(msg)
		return a, cmd

	case viewPeek:
		switch msg.String() {
		case "esc", " ":
			a.view = viewDash
			return a, nil
		case "enter":
			if a.deps.Tmux != nil {
				cmd := a.deps.Tmux.AttachCmd(a.peekTarget.name)
				return a, tea.ExecProcess(cmd, func(err error) tea.Msg { return attachedMsg{err} })
			}
			return a, nil
		case "q", "ctrl+c":
			return a, tea.Quit
		}
		return a, nil

	case viewSearch:
		return a.updateSearchKeys(msg)

	case viewDetail:
		// Task 3 fleshes this out (r resume, s summarize, snippets); Task 2
		// only wires the navigation so '↵' from search has somewhere to go.
		switch msg.String() {
		case "esc":
			a.view = viewSearch
		case "q", "ctrl+c":
			return a, tea.Quit
		}
		return a, nil
	}

	// viewDash
	switch msg.String() {
	case "q", "ctrl+c":
		return a, tea.Quit
	case "j", "down":
		if a.cursor < len(a.rows)-1 {
			a.cursor++
		}
	case "k", "up":
		if a.cursor > 0 {
			a.cursor--
		}
	case "n":
		a.form = newLauncherForm(a.deps.Projects)
		a.form.setFocus(0)
		a.view = viewLauncher
	case "x":
		if r, ok := a.selected(); ok {
			a.actionTarget = r
			a.view = viewConfirmKill
		}
	case "t":
		if r, ok := a.selected(); ok {
			a.actionTarget = r
			a.tag.SetValue(r.row.Tags)
			a.tag.Focus()
			a.view = viewTag
		}
	case "/":
		return a, a.openSearch()
	case "r":
		if r, ok := a.selected(); ok && r.recent && a.deps.Launcher != nil {
			l := a.deps.Launcher
			old := r.row
			w, h := a.width, a.height
			return a, func() tea.Msg {
				if _, err := l.Resume(old, w, h, time.Now()); err != nil {
					return errMsg{err}
				}
				return pollNowMsg{}
			}
		}
	case "enter":
		if r, ok := a.selected(); ok && !r.recent && a.deps.Tmux != nil {
			cmd := a.deps.Tmux.AttachCmd(r.name)
			return a, tea.ExecProcess(cmd, func(err error) tea.Msg { return attachedMsg{err} })
		}
	case " ":
		if r, ok := a.selected(); ok && !r.recent {
			a.peekTarget.name = r.name
			a.peekTarget.label = r.label
			a.peekContent = ""
			a.view = viewPeek
			return a, a.peekCmd()
		}
	}
	return a, nil
}

// openSearch enters viewSearch with a fresh, focused input and empty
// results (spec §6), then kicks off an immediate status refresh so the
// right-annotation count isn't stale-zero until the next 1.5s tick.
func (a *App) openSearch() tea.Cmd {
	ti := textinput.New()
	ti.Placeholder = "search sessions"
	ti.Focus()
	a.searchInput = ti
	a.searchHits = nil
	a.searchCursor = 0
	a.searchSeq++
	a.view = viewSearch
	return a.searchStatusCmd()
}

// updateSearchKeys handles keys while viewSearch is open. ↓/↑ move the
// result-cursor; the input keeps focus throughout, so j/k are TYPED (not
// navigation) here — only the arrow keys are intercepted before falling
// through to textinput. ctrl+c is checked BEFORE forwarding to textinput,
// same fix as viewTag above.
func (a *App) updateSearchKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return a, tea.Quit
	}
	switch msg.Type {
	case tea.KeyEsc:
		a.view = viewDash
		return a, nil
	case tea.KeyEnter:
		if a.searchCursor >= 0 && a.searchCursor < len(a.searchHits) {
			a.detailTarget = a.searchHits[a.searchCursor]
			a.view = viewDetail
		}
		return a, nil
	case tea.KeyDown:
		if a.searchCursor < len(a.searchHits)-1 {
			a.searchCursor++
		}
		return a, nil
	case tea.KeyUp:
		if a.searchCursor > 0 {
			a.searchCursor--
		}
		return a, nil
	}

	old := a.searchInput.Value()
	var cmd tea.Cmd
	a.searchInput, cmd = a.searchInput.Update(msg)
	if a.searchInput.Value() == old {
		return a, cmd
	}
	a.searchSeq++
	if a.searchInput.Value() == "" {
		// Empty input → results cleared immediately, no query scheduled
		// (spec §6). The bumped seq also invalidates any debounce/query
		// already in flight from before the input was cleared.
		a.searchHits = nil
		a.searchCursor = 0
		return a, cmd
	}
	return a, tea.Batch(cmd, debounceCmd(a.searchSeq))
}

// peekCmd captures the target pane's contents. The target is pinned at
// peek-open time (a.peekTarget), never the live cursor — same captured-target
// discipline as kill/tag (finding 5).
func (a *App) peekCmd() tea.Cmd {
	if a.deps.Tmux == nil {
		return nil
	}
	tm, name := a.deps.Tmux, a.peekTarget.name
	return func() tea.Msg {
		out, err := tm.CapturePane(name)
		if err != nil {
			return peekMsg{name: name, content: "(pane unavailable)"}
		}
		return peekMsg{name: name, content: out}
	}
}

// debounceCmd schedules a searchDebounceMsg carrying seq, searchDebounce
// after the keystroke that scheduled it.
func debounceCmd(seq int64) tea.Cmd {
	return tea.Tick(searchDebounce, func(time.Time) tea.Msg { return searchDebounceMsg{seq: seq} })
}

// searchStatusCmd fetches the cheap status the search frame's right
// annotation needs: the indexer's Active flag and TranscriptCount(). Both
// deps are nil-safe (Deps{} keeps the search view harmlessly inert).
func (a *App) searchStatusCmd() tea.Cmd {
	getStatus, st := a.deps.IndexerStatus, a.deps.Store
	return func() tea.Msg {
		var active bool
		if getStatus != nil {
			active = getStatus().Active
		}
		var count int64
		if st != nil {
			count, _ = st.TranscriptCount()
		}
		return searchStatusMsg{active: active, count: count}
	}
}

// searchQueryCmd runs the actual FTS query in a tea.Cmd. nil Store → nil cmd
// (no-op), matching Deps' nil-safety contract.
func (a *App) searchQueryCmd(query string) tea.Cmd {
	st := a.deps.Store
	if st == nil {
		return nil
	}
	return func() tea.Msg {
		hits, err := st.SearchSessions(query, 30)
		if err != nil {
			hits = nil
		}
		return searchResultsMsg{query: query, hits: hits}
	}
}

func (a *App) selected() (uiRow, bool) {
	if a.cursor < 0 || a.cursor >= len(a.rows) {
		return uiRow{}, false
	}
	return a.rows[a.cursor], true
}

func (a *App) View() string {
	w := a.width
	if w == 0 {
		w = 80
	}
	switch a.view {
	case viewLauncher:
		return frame(w, "new session", "", strings.Split(a.form.view(), "\n"),
			"tab/↑↓ field · ←/→ value · ↵ launch · esc cancel")
	case viewConfirmKill:
		r := a.actionTarget
		return frame(w, "kill session", "",
			[]string{"", "  kill " + styNeedsYou.Render(r.label) + styMeta.Render(" ("+r.name+")") + " ?", ""},
			"y confirm · n/esc cancel")
	case viewTag:
		return frame(w, "tags", "", []string{"", "  " + a.tag.View(), ""},
			"↵ save · esc cancel")
	case viewPeek:
		inner := w - 4
		h := a.height - 2
		if h < 1 {
			h = 1
		}
		lines := strings.Split(strings.TrimRight(a.peekContent, "\n"), "\n")
		if len(lines) > h {
			lines = lines[len(lines)-h:]
		}
		body := make([]string, len(lines))
		for i, line := range lines {
			// capture-pane without -e is plain text — no ANSI to worry about.
			body[i] = truncPlain(line, inner)
		}
		if a.errStr != "" {
			body = append(body, "", styErr.Render(truncPlain("! "+a.errStr, inner)))
		}
		return frame(w, "peek · "+a.peekTarget.label, "", body,
			"space/esc back · ↵ attach · q quit")
	case viewSearch:
		return a.viewSearch(w)
	case viewDetail:
		// Task 3 fleshes this out; Task 2 only needs somewhere for '↵' to
		// land (spec §6's detailTarget capture + minimal body).
		return frame(w, "detail", "", []string{"", "  detail — task 3", ""},
			"esc back · q quit")
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
	cursorLine := 0
	for i, r := range a.rows {
		if sec := sectionFor(r); sec != section {
			if section != "" {
				body = append(body, "")
			}
			section = sec
			body = append(body, sectionRule(sec, inner, sec == "NEEDS YOU"))
		}
		if i == a.cursor {
			cursorLine = len(body)
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
		// Breathing room before the hint — but skip it if the line above is
		// already blank (e.g. the empty-state block already ends with one),
		// so populated and empty dashboards don't end up with a double gap.
		if n := len(body); n > 0 && body[n-1] != "" {
			body = append(body, "")
		}
		body = append(body, styHelp.Render(truncPlain("(inside tmux — attach nests; F12 detaches)", inner)))
	}
	if a.height > 2 {
		body = windowBody(body, cursorLine, a.height-2)
	}

	counts := fmt.Sprintf("%d live · %d needs you", live, needs)
	keybar := "↵ attach · space peek · n new · x kill · t tag · r reopen · q quit"
	if inner > lipgloss.Width(keybar)+24 {
		keybar += " · / search·soon · w workflows·soon"
	}
	return frame(w, "LOOM", counts, body, keybar)
}

// windowBody keeps at most maxH body lines with cursorLine visible,
// replacing clipped edges with dim "… N more" markers.
func windowBody(body []string, cursorLine, maxH int) []string {
	if maxH <= 2 || len(body) <= maxH {
		return body
	}
	off := cursorLine - maxH/2
	if off < 0 {
		off = 0
	}
	if off > len(body)-maxH {
		off = len(body) - maxH
	}
	out := make([]string, maxH)
	copy(out, body[off:off+maxH])
	if off > 0 {
		out[0] = styChrome.Render(fmt.Sprintf("… %d more ↑", off))
	}
	if rest := len(body) - off - maxH; rest > 0 {
		out[maxH-1] = styChrome.Render(fmt.Sprintf("… %d more ↓", rest))
	}
	return out
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

// seedFailedSuffix is appended (dim/chrome, not meta) to the activity cell
// when a session's seed prompt failed to deliver — see renderRow.
const seedFailedSuffix = " · seed failed"

// renderRow: cursor(2) icon(1)+1 project(12)+1 activity(flex)+1 ctx(4)+1 model·mode(13)+1 age(4)
func (a *App) renderRow(i int, r uiRow, inner int) string {
	actW := inner - 41
	cursor := "  "
	if i == a.cursor {
		cursor = styCursor.Render("▸ ")
	}
	proj := padPlain(truncPlain(r.label, 12), 12)
	ctx := styMeta.Render(padLeft(truncPlain(humanTokens(r.ctx), 4), 4))
	meta := padPlain(truncPlain(metaText(r.model, r.mode), 13), 13)
	age := padPlain(humanAge(a.now, ageOf(r)), 4)
	if actW <= 0 { // ultra-narrow: drop the activity column entirely
		return cursor + statusIcon(r.status) + " " + styNeedsYouIf(r, proj)
	}
	return cursor + statusIcon(r.status) + " " + styNeedsYouIf(r, proj) + " " +
		a.renderActivityCell(r, actW) + " " + ctx + " " + styMeta.Render(meta) + " " + styMeta.Render(age)
}

// renderActivityCell renders the activity column, exactly actW cells.
// Per spec the " · seed failed" note is "appended dim" — styled separately
// from the base activity text (meta) rather than folded into the same
// style. That means truncating the base text to make room for the suffix
// BEFORE styling either segment (styled strings must never be re-sliced).
// If actW is too narrow to fit the suffix at all, this degrades to the
// simplest correct rendering: the combined text truncated as one dim-meta
// cell, same as before the suffix existed.
func (a *App) renderActivityCell(r uiRow, actW int) string {
	base := activityText(r)
	suffix := ""
	if r.row.SeedStatus == "failed" {
		suffix = seedFailedSuffix
	}
	// rune count, not len(suffix): suffix contains "·" (U+00B7), which is
	// 2 bytes in UTF-8 — len() would overcount its width by one.
	suffixW := len([]rune(suffix))
	if suffix == "" || actW <= suffixW {
		return styMeta.Render(padPlain(truncPlain(base+suffix, actW), actW))
	}
	baseW := actW - suffixW
	return styMeta.Render(padPlain(truncPlain(base, baseW), baseW)) + styChrome.Render(suffix)
}

// styNeedsYouIf highlights the project name on attention rows.
func styNeedsYouIf(r uiRow, s string) string {
	if r.status == "needs_you" {
		return styNeedsYou.Bold(true).Render(s)
	}
	return s
}

// activityText composes the base state text and, when known, the session
// title: "reply ready · add vega hedge to strategy" (state first, title
// after — truncated as one plain string before styling, see
// renderActivityCell). Blank title leaves the text unchanged. The
// seed-failed suffix is NOT appended here — renderActivityCell appends and
// styles it as its own dim segment (see there).
func activityText(r uiRow) string {
	var text string
	if r.recent {
		switch {
		case r.status == "error":
			text = fmt.Sprintf("error · exit %d", r.row.ExitCode)
		case r.row.ExitCode == 0:
			text = "done"
		default:
			text = "ended"
		}
	} else {
		switch r.status {
		case "running":
			if r.lastTool != "" {
				text = "⏺ " + r.lastTool
			} else {
				text = "working"
			}
		case "needs_you":
			text = "reply ready"
		default:
			text = "your turn"
		}
	}
	if r.title != "" {
		text += " · " + r.title
	}
	return text
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

func sectionFor(r uiRow) string {
	if r.recent {
		return "RECENT"
	}
	switch r.status {
	case "needs_you":
		return "NEEDS YOU"
	case "running":
		return "RUNNING"
	default:
		return "IDLE"
	}
}

// viewSearch renders the search frame (spec §6): input line, blank, then two
// lines per hit. Right annotation is the cached count/active pair refreshed
// on open and every tick (searchStatusCmd) — never a fresh query per frame.
func (a *App) viewSearch(w int) string {
	inner := w - 4

	body := make([]string, 0, 2+2*len(a.searchHits))
	body = append(body, truncPlain(a.searchInput.View(), inner), "")
	for i, h := range a.searchHits {
		body = append(body, a.renderHit(i, h, inner)...)
	}
	if len(a.searchHits) == 0 && a.searchInput.Value() != "" {
		body = append(body, styHelp.Render("no matches"))
	}

	right := fmt.Sprintf("%d sessions", a.searchCount)
	if a.searchActive {
		right += " · indexing…"
	}
	return frame(w, "search", right, body,
		"↓/↑ select · ↵ open · esc dashboard · ctrl+c quit")
}

// renderHit renders one search result as two lines: a header line (project
// label · title-or-ask · age, highlighted when it's the selected result) and
// a dim snippet line with accent-highlighted match spans (renderSnippet).
// Per the truncate-plain-before-style invariant, the header's plain text is
// assembled and truncated BEFORE any styling is applied.
func (a *App) renderHit(i int, h store.SearchHit, inner int) []string {
	text := h.Title
	if text == "" {
		text = h.Ask
	}
	line1 := "▸ " + projectLabel(h) + " · " + text
	if age := humanAge(a.now, h.LastTS); age != "" {
		line1 += " " + age
	}
	line1 = truncPlain(line1, inner)
	if i == a.searchCursor {
		line1 = styCursor.Render(line1)
	}

	snippetMax := inner - 4 // "    " indent before the snippet
	if snippetMax < 0 {
		snippetMax = 0
	}
	line2 := "    " + renderSnippet(h.Snippet, snippetMax)
	return []string{line1, line2}
}

// projectLabel derives a short display label for a search hit: prefer
// filepath.Base(cwd) when the cwd is known (spec §6); otherwise fall back to
// the loom-encoded ProjectDir name (e.g. "-Users-h-Sauce-HappyPay" — '/'
// replaced with '-'), taking the segment after the LAST '-'. That fallback
// is ambiguous whenever the real leaf directory name itself contains a '-'
// (e.g. a project literally named "my-app" encodes to "...-my-app" and this
// yields just "app") — acceptable for a compact search-result label; the
// detail view (Task 3) shows the full cwd/project_dir untruncated.
func projectLabel(h store.SearchHit) string {
	if h.Cwd != "" {
		return filepath.Base(h.Cwd)
	}
	s := strings.TrimPrefix(h.ProjectDir, "-")
	if i := strings.LastIndex(s, "-"); i >= 0 {
		return s[i+1:]
	}
	return s
}
