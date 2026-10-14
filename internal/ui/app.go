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
	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
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
	viewConfirmClear
	viewTag
	viewPeek
	viewSearch
	viewDetail
	viewWall
)

type Deps struct {
	Engine     *status.Engine
	Launcher   *session.Launcher
	Repos      []registry.Repo
	Tmux       *tmux.Client
	InsideTmux bool

	// Store/IndexerStatus/Summarizer back the memory search + detail views
	// (Task 2/3). All query paths are nil-safe: a nil Store means every
	// search/status command no-ops rather than panicking, so Deps{} (as
	// used by existing dashboard-only tests) keeps working unmodified.
	Store         *store.Store
	IndexerStatus func() memory.Status
	Summarizer    *memory.Summarizer

	// Projects is the §4 attribution/visibility authority. Nil-safe like
	// every other dep: a nil authority means nothing is ever hidden, so
	// Deps{} (and any caller predating this slice) behaves unchanged.
	//
	// The TUI owns the §6.3 surfaces listed for it — rail, Finished, wall,
	// recall/RELATED, search, notifications — because notifications exist in
	// BOTH binaries and ARCHITECTURE.md declares two instances against one DB
	// a supported state: an unfiltered terminal banner naming a hidden client
	// mid-screen-share defeats the feature outright.
	Projects Projects
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

	clearCount int // finished-row count captured when the clear confirm opens

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
	// (same captured-target discipline as actionTarget/peekTarget).
	// detailTranscript is fetched once at that same moment (and re-fetched
	// after a successful summarize) — r/s below act on detailTarget's
	// SessionID/detailTranscript, never on any live selection.
	detailTarget     store.SearchHit
	detailTranscript store.Transcript

	// detailReturn is the view esc returns to from viewDetail (spec §5):
	// viewSearch (existing search-origin flow) or viewLauncher (new,
	// Task 2). Launcher-origin details hide the 'r' resume action (spec
	// §5: spawning sessions mid-launcher-flow is a footgun) — see
	// updateDetailKeys/renderDetail.
	detailReturn view

	// --- Task 2: launcher RELATED panel (spec §3-§6) ---------------------

	// panelHits is the launcher's current recall query result (display-
	// limited — spec §6). panelFocused/panelCursor are the panel half of
	// the launcher's focus model (spec §3): panelCursor indexes the
	// COMBINED display list panelRows() builds (pinned includes first,
	// then non-included panelHits — spec §4).
	panelHits    []memory.RelatedHit
	panelFocused bool
	panelCursor  int

	// includes/includeOrder: the SessionID-keyed pull-in set (spec §4) —
	// map, never positional, so a re-rank never silently drops a toggled-in
	// entry; includeOrder is insertion (toggle) order, both for rendering
	// pinned rows in a stable order and for the marker-append order at
	// launch (buildSeedWithRecall consumes includeSnapshot()).
	includes     map[string]store.Transcript
	includeOrder []string

	// panelWarn is the live slash-seed-with-includes warning (spec §4),
	// recomputed on every seed keystroke and include toggle.
	panelWarn string

	// panelSeq is the panel query's debounce generation counter (spec §6,
	// same discipline as searchSeq): bumped on every seed keystroke and
	// project-field change; a panelDebounceMsg/panelResultsMsg carrying a
	// stale generation (or a (seed,projectDir) pair that no longer matches
	// the live launcher state — spec §6 I6) is discarded.
	panelSeq int64

	// detailHint: a transient dim message shown in the body (currently only
	// the resume-collision hint — spec §6 P0). Cleared whenever a fresh
	// detail view is opened.
	detailHint string

	// detailConfirmRegen/detailSummarizing: the summarize press-twice-to-
	// regenerate state machine (spec §5/§6). detailConfirmRegen arms after a
	// first 's' press when a summary already exists (next 's' actually
	// regenerates); detailSummarizing is true only while the tea.Cmd calling
	// Summarizer.Summarize is in flight (further 's' presses no-op).
	detailConfirmRegen bool
	detailSummarizing  bool

	// --- Wall (`W`) — spec §3 ----------------------------------------------

	// wallOrder is the wall's STABLE display order (spec §3.1: CreatedAt
	// then Name — deterministic regardless of a.snap.Live's own slice order,
	// and NEVER the attention order rebuildRows computes — attention is the
	// dashboard's job, reordering it here would teleport the grid under the
	// reader). Recomputed on every snapMsg (applyWallOrder, called
	// unconditionally alongside rebuildRows — cheap, and keeps the wall
	// current the instant it's opened rather than only after its own first
	// poll). wallSelected is the NAME-keyed selection (spec §3.5): it
	// survives a.wallOrder being rebuilt/reordered/paginated out from under
	// it, and applyWallOrder resolves the nearest-neighbor fallback the
	// moment the selected name is no longer present.
	wallOrder    []status.Row
	wallSelected string

	// wallCaptures is the per-session captured-pane cache (spec §3.3/§3.4),
	// keyed by session name — populated by wallMsg, pruned of any name no
	// longer in wallOrder on every applyWallOrder pass. A name absent from
	// this map simply renders as a blank (not-yet-captured) cell, never an
	// error.
	//
	// Deliberately NOT cleared on close (esc just flips a.view back to
	// viewDash) — it persists across reopen, so a cell can render
	// immediately from cache rather than blank. The tradeoff: openWall's
	// immediate recapture cmd is async, so the window between reopening and
	// that result landing shows STALE content (the last capture from before
	// the wall was closed), not a blank cell.
	wallCaptures map[string]wallCapture

	// wallSeq is the wall capture command's generation counter (spec §3.3
	// "stale... discard", same debounce-generation discipline as
	// searchSeq/panelSeq): bumped every time a NEW wallCaptureCmd is fired
	// (view-open, and once per tick while the wall is open) — a wallMsg
	// carrying any other generation is a stale, fully-discarded result (an
	// overlapping capture from a slower, earlier tick), never applied.
	wallSeq int64

	// res is the last successfully-read visibility authority (§6.1), kept so
	// a transient store error cannot un-hide a project mid-screen-share. It
	// is refreshed read-through by App.visibility(), read only on the Update
	// loop, and captured BY POINTER into query commands — a Resolver is
	// immutable once built, so sharing it with a tea.Cmd goroutine is safe
	// where sharing App is not.
	res *projects.Resolver
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

	// summaryMsg is the result of a Summarizer.Summarize tea.Cmd (spec §5).
	// Applied only when sessionID still matches the live detailTarget — an
	// in-flight summarize for a session the user has since navigated away
	// from (opened a DIFFERENT detail) is discarded, same staleness
	// discipline as searchResultsMsg/peekMsg above.
	summaryMsg struct {
		sessionID string
		text      string
		err       error
	}

	// panelDebounceMsg fires searchDebounce after a launcher seed keystroke
	// (spec §6, same debounce pattern as searchDebounceMsg); seq pins it to
	// the keystroke generation that scheduled it.
	panelDebounceMsg struct{ seq int64 }

	// panelResultsMsg is a launcher RELATED-panel recall query's result
	// (spec §6). Applied only when BOTH seed and projectDir still match the
	// launcher's LIVE state (spec §6 I6: the staleness key is
	// (seed,projectDir), not seed alone — a fast project switch must not
	// apply the old project's panel to the new one).
	panelResultsMsg struct {
		seed       string
		projectDir string
		hits       []memory.RelatedHit
	}

	// resumeBlockedMsg reports the resume-collision guard (spec §6 P0) found
	// a live row for sessionID and — critically — did NOT call
	// Launcher.Resume. Same sessionID-staleness discipline as summaryMsg.
	resumeBlockedMsg struct{ sessionID string }

	// wallCaptureResult is one session's CapturePane outcome within a
	// wallMsg (spec §3.3/§3.4): err true means CapturePane itself failed
	// (peek precedent — the cell keeps rendering, as "(pane unavailable)",
	// rather than disappearing outright); lines is nil for an error result.
	wallCaptureResult struct {
		name  string
		lines []string
		err   bool
	}

	// wallMsg is the result of one wall capture cmd (spec §3.3): gen pins it
	// to the generation that fired it — Update's wallMsg case discards the
	// WHOLE message when gen no longer matches a.wallSeq (a newer capture
	// has since been fired: the peekCmd/searchDebounceMsg staleness
	// discipline, generalized to a per-tick refresh rather than a keystroke
	// debounce). Even a fresh-generation message drops any individual result
	// whose name is no longer in a.wallOrder (spec: "stale/vanished entries
	// dropped") — the session ended between the cmd firing and landing.
	wallMsg struct {
		gen     int64
		results []wallCaptureResult
	}
)

func NewApp(deps Deps) *App {
	ti := textinput.New()
	ti.Placeholder = "tags (comma separated)"
	return &App{deps: deps, form: newLauncherForm(deps.Repos), tag: ti}
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

// needsYouLabels renders the display strings for the sessions that just
// flipped to needs-you. The engine reports session names (spec §4) because a
// pre-rendered label carries no identity; the label is joined back here, from
// the same snapshot's Live rows — the poll that produced a name always
// produced its Live row, so an unmatched name means the snapshot was
// hand-built and is silently dropped rather than announced as an empty
// banner.
func needsYouLabels(snap status.Snapshot) []string {
	var out []string
	for _, name := range snap.NewlyNeedsYou {
		for _, r := range snap.Live {
			if r.Name != name {
				continue
			}
			label := r.ProjectLabel
			if r.Title != "" {
				label += " · " + r.Title
			}
			out = append(out, label)
			break
		}
	}
	return out
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
		case viewWall:
			// spec §3.3: "per poll tick while open, ONE one-shot tea.Cmd
			// captures the VISIBLE page's panes" — bumping wallSeq here
			// (before firing the new capture cmd) is what makes any
			// still-in-flight PRIOR tick's capture stale the instant this
			// one is fired, per the wallMsg staleness discipline.
			a.wallSeq++
			return a, tea.Batch(a.pollCmd(), tickAfter(), a.wallCaptureCmd())
		case viewConfirmClear:
			// keep the shown count honest while the dialog is open (F1): the
			// bulk delete removes every finished row, so the number must track
			// new sessions finishing under it. CountEnded is a cheap indexed
			// count, run synchronously like the 'X' handler does.
			if st := a.deps.Store; st != nil {
				if n, err := st.CountEnded(); err == nil {
					a.clearCount = int(n)
					if a.clearCount == 0 {
						a.view = viewDash
					}
				}
			}
			return a, tea.Batch(a.pollCmd(), tickAfter())
		}
		return a, tea.Batch(a.pollCmd(), tickAfter())
	case pollNowMsg:
		// one-shot refresh after launch/kill/resume — NOT a new tick chain.
		// Returning tickMsg here (as launch/kill/resume commands used to)
		// would permanently add another perpetual pollInterval tick loop per
		// action, stacking concurrent Engine.Poll goroutines (finding 1).
		return a, a.pollCmd()
	case snapMsg:
		// §6.3: the filter lands HERE, on the way in, so every surface fed by
		// a.snap — rail, Finished, wall — is filtered by construction rather
		// than each remembering to. The engine's own snapshot is untouched:
		// hidden sessions were polled and their transitions persisted before
		// this line ran (§6.2a).
		snap, suppressed := a.filterSnapshot(status.Snapshot(m))
		a.snap = snap
		a.errStr = ""
		a.now = time.Now()
		a.rebuildRows()
		a.applyWallOrder()
		// suppressed transitions still raise a signal, just a label-free one
		// (§6.4) — see filterSnapshot.
		if labels := needsYouLabels(snap); len(labels)+suppressed > 0 {
			return a, notifyCmd(labels, suppressed)
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
	case panelDebounceMsg:
		// stale (a newer keystroke has since bumped panelSeq) → discarded.
		if m.seq != a.panelSeq {
			return a, nil
		}
		return a, a.panelQueryCmd(a.form.seed.Value(), a.currentProjectDir())
	case panelResultsMsg:
		// stale: staleness key is (seed,projectDir) — spec §6 I6 — not seed
		// alone, so a fast project switch discards the OLD project's result
		// even if the seed text happens to still match.
		if m.seed != a.form.seed.Value() || m.projectDir != a.currentProjectDir() {
			return a, nil
		}
		a.panelHits = m.hits
		a.clampPanelCursor()
		return a, nil
	case summaryMsg:
		// stale: the user has since opened a different session's detail.
		if m.sessionID != a.detailTarget.SessionID {
			return a, nil
		}
		a.detailSummarizing = false
		if m.err != nil {
			a.errStr = m.err.Error()
			return a, nil
		}
		// success: re-fetch the transcript so the new llm_summary/summary_at
		// show immediately (SetLLMSummary already persisted it).
		if st := a.deps.Store; st != nil {
			if t, ok, err := st.GetTranscript(m.sessionID); err == nil && ok {
				a.detailTranscript = t
			}
		}
		return a, nil
	case resumeBlockedMsg:
		if m.sessionID == a.detailTarget.SessionID {
			a.detailHint = "already live — attach from the dashboard"
		}
		return a, nil
	case wallMsg:
		// stale: a newer capture cmd has since been fired (gen mismatch), or
		// the wall isn't even open anymore — discard the WHOLE message (spec
		// §3.3/§3.4).
		if a.view != viewWall || m.gen != a.wallSeq {
			return a, nil
		}
		if a.wallCaptures == nil {
			a.wallCaptures = map[string]wallCapture{}
		}
		alive := make(map[string]bool, len(a.wallOrder))
		for _, r := range a.wallOrder {
			alive[r.Name] = true
		}
		for _, res := range m.results {
			if !alive[res.name] {
				continue // vanished between cmd-fire and landing — dropped.
			}
			a.wallCaptures[res.name] = wallCapture{lines: res.lines, err: res.err}
		}
		return a, nil
	case tea.KeyMsg:
		return a.updateKeys(m)
	}
	return a, nil
}

// reapDeadElseSkipLive decides whether name's finished row is safe to delete.
// It returns skip=true when the row must be kept: the tmux pane is genuinely
// live (a resurrection race — not really finished), OR a dead pane's reap
// failed (deleting anyway would let the status engine re-adopt a zero-metadata
// zombie; leaving the row lets the engine re-reap the pane and a later dismiss
// succeed). It returns skip=false when it is safe to delete: no tmux session
// exists, a dead pane was successfully reaped, or Tmux is not wired.
func (a *App) reapDeadElseSkipLive(name string) bool {
	tm := a.deps.Tmux
	if tm == nil {
		return false
	}
	ps, err := tm.PaneStatus(name)
	if err != nil {
		return false // no session — nothing to reap, safe to delete
	}
	if !ps.Dead {
		return true // genuinely live — don't dismiss a live session
	}
	if err := tm.KillSession(name); err != nil {
		return true // dead but reap failed — keep the row, engine will re-reap
	}
	return false // dead pane reaped — safe to delete
}

// confirmCancel handles the shared non-'y' keys of the confirm dialogs
// (viewConfirmKill / viewConfirmClear): n/esc return to the dashboard, ctrl+c
// quits, any other key is a no-op while a confirm is open.
func (a *App) confirmCancel(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := msg.String()
	if s == "n" || msg.Type == tea.KeyEsc {
		a.view = viewDash
		return a, nil
	}
	if s == "ctrl+c" {
		return a, tea.Quit
	}
	return a, nil // any other key is a no-op while a confirm is open
}

func (a *App) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch a.view {
	case viewLauncher:
		return a.updateLauncherKeys(msg)

	case viewConfirmKill:
		s := msg.String()
		if s == "y" {
			a.view = viewDash
			name := a.actionTarget.name
			if a.actionTarget.recent {
				st := a.deps.Store
				if st == nil {
					return a, nil
				}
				return a, func() tea.Msg {
					// Close the reap window (adversarial finding F2): if a
					// lingering tmux session still exists for this "finished"
					// row, a bare row delete lets the next poll re-adopt it as
					// a zero-metadata zombie. Reap a DEAD pane first; leave a
					// genuinely LIVE one alone (a resurrection race — it isn't
					// really finished, so don't dismiss it).
					if a.reapDeadElseSkipLive(name) {
						return pollNowMsg{}
					}
					if err := st.DeleteSession(name); err != nil {
						return errMsg{err}
					}
					return pollNowMsg{}
				}
			}
			if a.deps.Tmux != nil {
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
		return a.confirmCancel(msg)

	case viewConfirmClear:
		s := msg.String()
		if s == "y" {
			a.view = viewDash
			st := a.deps.Store
			if st == nil {
				return a, nil
			}
			tm := a.deps.Tmux
			return a, func() tea.Msg {
				if tm == nil {
					if _, err := st.DeleteEnded(); err != nil {
						return errMsg{err}
					}
					return pollNowMsg{}
				}
				names, err := st.EndedNames()
				if err != nil {
					return errMsg{err}
				}
				for _, n := range names {
					if a.reapDeadElseSkipLive(n) {
						continue
					}
					if err := st.DeleteSession(n); err != nil {
						return errMsg{err}
					}
				}
				return pollNowMsg{}
			}
		}
		return a.confirmCancel(msg)

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
		return a.updateDetailKeys(msg)

	case viewWall:
		return a.updateWallKeys(msg)
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
		return a, a.openLauncher()
	case "W":
		return a, a.openWall()
	case "x":
		if r, ok := a.selected(); ok {
			if r.recent && a.deps.Store == nil {
				break // no store to dismiss from — stay consistent with 'X'
			}
			a.actionTarget = r
			a.view = viewConfirmKill
		}
	case "X":
		if len(a.snap.Recent) > 0 && a.deps.Store != nil {
			if n, err := a.deps.Store.CountEnded(); err == nil && n > 0 {
				a.clearCount = int(n)
				a.view = viewConfirmClear
			}
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
			a.openDetail(a.searchHits[a.searchCursor], viewSearch)
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

// --- Task 2: launcher RELATED panel (spec §3-§6) ------------------------

// openLauncher resets the launcher to a fresh state (spec §3/§4: form at
// field 0, panel unfocused, no includes/warning) and kicks off an immediate
// (non-debounced — this is a view-open, not a keystroke burst) panel query
// for the default-selected project's recent sessions, same "fresh state +
// immediate refresh cmd" shape as openSearch.
// The repo list is project-scoped on open (§6.3, launcher target picker): the
// form is rebuilt from scratch here every time, so scoping it at open is
// enough — a project hidden while the launcher is already up is caught the
// next time it opens, and the launcher is modal.
func (a *App) openLauncher() tea.Cmd {
	a.form = newLauncherForm(a.visibleRepos())
	a.form.setFocus(0)
	a.view = viewLauncher
	a.panelFocused = false
	a.panelCursor = 0
	a.panelHits = nil
	a.includes = nil
	a.includeOrder = nil
	a.panelWarn = ""
	a.panelSeq++
	return a.panelQueryCmd("", a.currentProjectDir())
}

// currentProjectDir returns the store's project_dir encoding (spec §6:
// transcript.ProjectDirName) for the launcher form's currently-selected
// project, or "" when there are no projects (panelQueryCmd treats "" as
// "nothing to query" — matches Deps' nil-safety contract).
func (a *App) currentProjectDir() string {
	if len(a.form.repos) == 0 {
		return ""
	}
	return transcript.ProjectDirName(a.form.repos[a.form.repoIdx].Path)
}

// panelQueryCmd runs internal/memory.Related in a tea.Cmd for the launcher
// RELATED panel (spec §6). seed/projectDir are captured by value (captured-
// target discipline) so the result carries the EXACT staleness key it was
// fired for — Update's panelResultsMsg handler compares both against the
// LIVE launcher state, not against anything stashed here. nil Store or no
// project selected → nil cmd (Deps nil-safety contract, same as
// searchQueryCmd).
func (a *App) panelQueryCmd(seed, projectDir string) tea.Cmd {
	st := a.deps.Store
	if st == nil || projectDir == "" {
		return nil
	}
	// Recall crosses projects by design, so its hits are filtered
	// independently of the (already scoped) seed project — §6.3 lists the
	// RELATED panel as its own leak surface for exactly that reason. Asked
	// for panelFetchLimit, trimmed to panelDisplayLimit after filtering.
	res := a.visibility()
	return func() tea.Msg {
		hits, err := memory.Related(st, projectDir, seed, panelFetchLimit)
		if err != nil {
			hits = nil
		}
		return panelResultsMsg{seed: seed, projectDir: projectDir,
			hits: visibleRelated(res, hits, panelDisplayLimit)}
	}
}

// launcherDebounceCmd schedules a panelDebounceMsg carrying seq,
// searchDebounce after the seed keystroke that scheduled it (spec §6, same
// debounce delay/pattern as debounceCmd above).
func launcherDebounceCmd(seq int64) tea.Cmd {
	return tea.Tick(searchDebounce, func(time.Time) tea.Msg { return panelDebounceMsg{seq: seq} })
}

// launcherDown/launcherUp implement the ↓/↑ half of the launcher focus
// model (spec §3, VERBATIM): within the form, ↓/↑ behave like tab/shift-tab
// but WITHOUT wrapping — ↓ from seed(3) instead enters panel[0] (a no-op
// when the panel is empty: nothing to focus), and ↑ from project(0) is a
// no-op (does not wrap to seed). Within the panel, ↓/↑ move the cursor; ↓ at
// the bottom is a no-op (no wrap); ↑ at panel[0] returns focus to seed(3).
func (a *App) launcherDown() {
	if a.panelFocused {
		if n := a.panelLen(); a.panelCursor < n-1 {
			a.panelCursor++
		}
		return
	}
	if a.form.focus < 3 {
		a.form.setFocus(a.form.focus + 1)
		return
	}
	if a.panelLen() == 0 {
		return
	}
	a.panelFocused = true
	a.panelCursor = 0
	a.form.seed.Blur()
}

func (a *App) launcherUp() {
	if a.panelFocused {
		if a.panelCursor > 0 {
			a.panelCursor--
			return
		}
		a.panelFocused = false
		a.form.setFocus(3)
		return
	}
	if a.form.focus == 0 {
		return
	}
	a.form.setFocus(a.form.focus - 1)
}

// changeProjectField handles ←/→ while the project field (form.focus==0) is
// focused: delegates the actual index cycling to form.update, then — spec
// §4 "project-field change CLEARS includes" (different context, disclosed
// decision) — clears includes and refires the panel query immediately
// (not debounced: this is a discrete field change, not a keystroke burst)
// whenever the selected project actually changed.
func (a *App) changeProjectField(msg tea.KeyMsg) tea.Cmd {
	old := a.currentProjectDir()
	a.form.update(msg)
	pd := a.currentProjectDir()
	if pd == old {
		return nil
	}
	a.includes = nil
	a.includeOrder = nil
	a.panelCursor = 0
	a.panelFocused = false
	a.panelSeq++
	a.recomputeWarn()
	return a.panelQueryCmd(a.form.seed.Value(), pd)
}

// updateLauncherKeys is the launcher's focus-zone dispatch (spec §3,
// VERBATIM, every transition): tab/shift-tab always cycle the 4 FORM
// fields only, wrapping, and never enter the panel; ↓/↑ are the dual
// form<->panel navigation (launcherDown/launcherUp); enter launches when
// form-focused or opens detail on the hovered row when panel-focused; space
// toggles an include when panel-focused, or is typed into the seed
// textinput when seed-focused (space-in-seed-types-a-space); esc always
// returns to the dashboard, from anywhere in the launcher.
func (a *App) updateLauncherKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return a, tea.Quit
	}
	switch msg.Type {
	case tea.KeyEsc:
		a.view = viewDash
		return a, nil
	case tea.KeyEnter:
		if a.panelFocused {
			if row, ok := a.panelSelected(); ok {
				a.openDetailFromLauncher(row)
			}
			return a, nil
		}
		return a.launch()
	case tea.KeyTab, tea.KeyShiftTab:
		d := 1
		if msg.Type == tea.KeyShiftTab {
			d = -1
		}
		a.panelFocused = false
		a.form.setFocus(cycle(a.form.focus, d, 4))
		return a, nil
	case tea.KeyDown:
		a.launcherDown()
		return a, nil
	case tea.KeyUp:
		a.launcherUp()
		return a, nil
	}
	if msg.String() == " " && a.panelFocused {
		a.togglePanelInclude()
		return a, nil
	}
	if a.panelFocused {
		// panel-focused: no other key reaches the form (spec §3 — a stray
		// keystroke here must not leak into the seed textinput).
		return a, nil
	}
	if a.form.focus == 0 && (msg.Type == tea.KeyLeft || msg.Type == tea.KeyRight) {
		return a, a.changeProjectField(msg)
	}

	oldSeed := a.form.seed.Value()
	cmd := a.form.update(msg)
	if a.form.focus == 3 && a.form.seed.Value() != oldSeed {
		a.panelSeq++
		a.recomputeWarn()
		return a, tea.Batch(cmd, launcherDebounceCmd(a.panelSeq))
	}
	return a, cmd
}

// launch fires the actual session launch (spec §3 enter-on-form-focused):
// same recipe-and-Launcher-call shape as before this task, except the seed
// is now buildSeedWithRecall'd against the current includes first (spec
// §4) — a plain seed with zero includes passes through unchanged.
func (a *App) launch() (tea.Model, tea.Cmd) {
	r, ok := a.form.Recipe()
	a.view = viewDash
	if !ok || a.deps.Launcher == nil {
		return a, nil
	}
	seed, _ := buildSeedWithRecall(r.Seed, a.includeSnapshot(), a.deps.Repos)
	r.Seed = seed
	l := a.deps.Launcher
	w, h := a.width, a.height
	return a, func() tea.Msg {
		if _, err := l.Launch(r, w, h, time.Now()); err != nil {
			return errMsg{err}
		}
		return pollNowMsg{}
	}
}

// openDetailFromLauncher opens the detail view for a hovered RELATED-panel
// row (spec §3 enter-on-panel-focused, §5 round-trip): builds a
// store.SearchHit-shaped view from the row's Transcript — Snippet carries
// the row's own snippet (empty for a pinned include or a recency-fallback
// hit, M4 — renderDetail already handles an empty Snippet) — and reuses
// openDetail's existing captured-target + origin machinery. Launcher state
// (form fields, panel entries/cursor, includes) lives entirely on App and
// is untouched by this call — esc later returns to an intact launcher (spec
// §5).
func (a *App) openDetailFromLauncher(row panelRow) {
	t := row.t
	hit := store.SearchHit{
		SessionID:  t.SessionID,
		Snippet:    row.snippet,
		Title:      t.Title,
		ProjectDir: t.ProjectDir,
		Cwd:        t.Cwd,
		Ask:        t.Ask,
		LastTS:     t.LastTS,
	}
	a.openDetail(hit, viewLauncher)
}

// openDetail captures hit as detailTarget (spec §6 captured-target
// discipline) and fetches its full Transcript once, at open time — a plain
// synchronous Store call (same precedent as viewTag's SetTags in
// updateKeys), not a tea.Cmd round trip: it's a single indexed row lookup,
// nowhere near the cost that would justify async staleness handling. A nil
// Store (Deps{} in dashboard-only tests) leaves detailTranscript zeroed,
// which resumeDisabled() correctly reads as "no cwd, disabled".
//
// origin (spec §5) is where esc returns to: viewSearch (the existing flow)
// or viewLauncher (Task 2, RELATED panel's ↵-on-panel-focused). Launcher
// state (form fields, panel entries/cursor, includes) lives entirely on App
// and is untouched by opening/closing a detail — round-tripping back to
// viewLauncher finds it exactly as it was (spec §5).
func (a *App) openDetail(hit store.SearchHit, origin view) {
	a.detailTarget = hit
	a.detailTranscript = store.Transcript{}
	a.detailHint = ""
	a.detailConfirmRegen = false
	a.detailSummarizing = false
	a.detailReturn = origin
	if st := a.deps.Store; st != nil {
		if t, ok, err := st.GetTranscript(hit.SessionID); err == nil && ok {
			a.detailTranscript = t
		}
	}
	a.view = viewDetail
}

// updateDetailKeys handles keys while viewDetail is open (spec §6): r
// resume (collision-guarded, HIDDEN entirely for a launcher-origin detail —
// spec §5: spawning sessions mid-launcher-flow is a footgun), s summarize
// (press-twice-to-regenerate), esc back to the origin view (search or
// launcher — that view's own state isn't touched by any of this), q/ctrl+c
// quit.
func (a *App) updateDetailKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return a, tea.Quit
	case "esc":
		a.view = a.detailReturn
		return a, nil
	case "r":
		if a.detailReturn == viewLauncher || a.resumeDisabled() {
			return a, nil
		}
		return a, a.resumeCmd()
	case "s":
		if a.deps.Summarizer == nil || a.detailSummarizing {
			return a, nil // no-op: no summarizer configured, or already in flight
		}
		if a.detailTranscript.LLMSummary != "" && !a.detailConfirmRegen {
			// first press with an existing summary: arm the confirm, don't
			// call Summarize yet (spec §5 press-twice-to-regenerate).
			a.detailConfirmRegen = true
			return a, nil
		}
		a.detailConfirmRegen = false
		a.detailSummarizing = true
		return a, a.summarizeCmd(a.detailTarget.SessionID)
	}
	return a, nil
}

// resumeDisabled: r is disabled (spec §6) when the transcript has no known
// cwd (can't launch tmux into it) or its source file has vanished.
func (a *App) resumeDisabled() bool {
	return a.detailTranscript.Cwd == "" || a.detailTranscript.FileMissing
}

// resumeCmd implements the resume-collision guard (spec §6, THE P0 for this
// task): GetLatestByClaudeSessionID is checked FIRST, inside the returned
// cmd, and a live row short-circuits to resumeBlockedMsg WITHOUT ever
// calling Launcher.Resume — a terminal row is resumed as-is (preserving its
// label/model/mode/tags); no row at all synthesizes a minimal SessionRow
// from the transcript. sessionID/cwd/launcher/w/h are captured by value
// here (before the cmd runs) — same captured-target discipline as
// elsewhere, and it means the closure never re-reads a.* fields that could
// have moved on by the time this cmd executes.
func (a *App) resumeCmd() tea.Cmd {
	st := a.deps.Store
	if st == nil {
		return nil
	}
	sessionID := a.detailTarget.SessionID
	cwd := a.detailTranscript.Cwd
	launcher := a.deps.Launcher
	w, h := a.width, a.height
	return func() tea.Msg {
		now := time.Now()
		row, ok, err := st.GetLatestByClaudeSessionID(sessionID)
		if err != nil {
			return errMsg{err}
		}
		if ok && isLiveRow(row) {
			// THE guard: do NOT call Launcher.Resume on a live row.
			return resumeBlockedMsg{sessionID: sessionID}
		}
		if launcher == nil {
			return nil
		}
		target := row
		if !ok {
			target = store.SessionRow{
				ClaudeSessionID: sessionID,
				Cwd:             cwd,
				ProjectLabel:    filepath.Base(cwd),
				CreatedAt:       now.Unix(),
				EndedAt:         -1,
				ExitCode:        -1,
				LastStatus:      "unknown",
			}
		}
		if _, err := launcher.Resume(target, w, h, now); err != nil {
			return errMsg{err}
		}
		return pollNowMsg{}
	}
}

// isLiveRow mirrors store.Live()'s definition of "live" (last_status in
// running/needs_you/idle/unknown) vs. store.Recent()'s terminal set
// (done/error) — the same distinction the resume-collision guard needs.
func isLiveRow(r store.SessionRow) bool {
	return r.LastStatus != "done" && r.LastStatus != "error"
}

// summarizeCmd runs the (up to 90s) Summarizer.Summarize call in a tea.Cmd —
// it MUST never run inline in Update, which would freeze the whole UI for
// the duration of the child process.
func (a *App) summarizeCmd(sessionID string) tea.Cmd {
	sm := a.deps.Summarizer
	return func() tea.Msg {
		text, err := sm.Summarize(sessionID, time.Now())
		return summaryMsg{sessionID: sessionID, text: text, err: err}
	}
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
// Results are project-scoped (§6.3): the resolver is read HERE, on the Update
// loop, and captured by pointer — the command goroutine must not touch App.
// searchFetchLimit over-fetches so hiding shortens the result list only when
// there genuinely are fewer than searchDisplayLimit visible matches.
func (a *App) searchQueryCmd(query string) tea.Cmd {
	st := a.deps.Store
	if st == nil {
		return nil
	}
	res := a.visibility()
	return func() tea.Msg {
		hits, err := st.SearchSessions(query, searchFetchLimit)
		if err != nil {
			hits = nil
		}
		return searchResultsMsg{query: query, hits: visibleHits(res, hits, searchDisplayLimit)}
	}
}

// --- Fan-out (`N`) -------------------------------------------------------

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
		return a.viewLauncher(w)
	case viewConfirmKill:
		r := a.actionTarget
		title, verb := "kill session", "kill "
		if r.recent {
			title, verb = "dismiss session", "dismiss "
		}
		return frame(w, title, "",
			[]string{"", "  " + verb + styNeedsYou.Render(r.label) + styMeta.Render(" ("+r.name+")") + " ?", ""},
			"y confirm · n/esc cancel")
	case viewConfirmClear:
		msg := fmt.Sprintf("  clear %d finished session", a.clearCount)
		if a.clearCount != 1 {
			msg += "s"
		}
		return frame(w, "clear finished", "",
			[]string{"", styNeedsYou.Render(msg) + " ?", ""},
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
		return a.renderDetail(w)
	case viewWall:
		return a.renderWall(w)
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
	keybar := "↵ attach · space peek · n new · x kill/dismiss · t tag · r reopen · q quit"
	suffix := " · X clear · / search · W wall"
	if inner > lipgloss.Width(keybar)+lipgloss.Width(suffix)+2 {
		keybar += suffix
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
		suffix += seedFailedSuffix
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

// viewLauncher renders the launcher frame (spec §3/§4/§6): the 4-field form
// (dimmed of its own cursor marker when focus has moved into the panel —
// see launcherForm.view), a RELATED section of panel rows (pinned includes
// first, spec §4), the live slash-seed warning and include count, and a
// keybar that's contextual per focus zone (spec §3).
func (a *App) viewLauncher(w int) string {
	inner := w - 4
	body := strings.Split(a.form.view(!a.panelFocused), "\n")
	body = append(body, "", sectionRule("RELATED", inner, false))

	rows := a.panelRows()
	if len(rows) == 0 {
		body = append(body, styHelp.Render(truncPlain("no related sessions", inner)))
	} else {
		for i, row := range rows {
			body = append(body, a.renderPanelRow(i, row, inner)...)
		}
	}

	if a.panelWarn != "" {
		body = append(body, "", styHelp.Render(truncPlain("⚠ "+a.panelWarn, inner)))
	} else if len(a.includes) > 0 {
		body = append(body, "", styHelp.Render(fmt.Sprintf("%d/%d included", len(a.includes), includeCap)))
	}

	keybar := "tab/↑↓ field · ←/→ value · ↵ launch · esc cancel"
	if a.panelFocused {
		keybar = "↵ detail · space include · ↑ back to form · esc cancel"
	}
	return frame(w, "new session", "", body, keybar)
}

// renderPanelRow renders one RELATED-panel row (spec §4/§6/M3/M4): a header
// line (include checkbox, M3 project label, title-or-ask, age — cursor-
// highlighted when hovered while panel-focused) and a dim preview line: the
// FTS snippet when this row came from an actual query match (renderSnippet,
// with its accent-highlighted spans), or — M4 — an outcome preview when it
// didn't (a pinned include or a recency-fallback hit both have no snippet).
// Per the truncate-plain-before-style invariant, line1's plain text is
// assembled and truncated BEFORE any styling is applied.
func (a *App) renderPanelRow(i int, row panelRow, inner int) []string {
	t := row.t
	box := "[ ]"
	if row.included {
		box = "[x]"
	}
	text := t.Title
	if text == "" {
		text = t.Ask
	}
	line1 := box + " " + relatedLabel(a.deps.Repos, t) + " · " + text
	if age := humanAge(a.now, t.LastTS); age != "" {
		line1 += " " + age
	}
	line1 = truncPlain(line1, inner)
	if a.panelFocused && i == a.panelCursor {
		line1 = styCursor.Render(line1)
	}

	previewMax := inner - 4 // "    " indent before the preview
	if previewMax < 0 {
		previewMax = 0
	}
	var line2 string
	if row.snippet != "" {
		line2 = "    " + renderSnippet(row.snippet, previewMax)
	} else {
		line2 = "    " + styMeta.Render(truncPlain(memory.CleanText(t.Outcome), previewMax))
	}
	return []string{line1, line2}
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
// detail view shows the full cwd/project_dir untruncated.
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

// renderDetail renders the session detail frame (spec §6): heading,
// project · cwd, date range + msg count (+ file_missing hint), Ask/Outcome/
// Files, the LLM summary (or a hint to generate one, or the summarizing/
// confirm-regenerate transient states), the resume-collision hint if one is
// armed, and finally the snippet already carried on detailTarget itself —
// detailTarget IS the SearchHit the current query produced for this
// session, so its Snippet field already IS "the current-query snippet" with
// no second query needed. Every line is memory.CleanText'd (LLMSummary in
// particular is un-sanitized model output — the only field here NOT already
// cleaned at index time — and truncated before styling, per the
// truncate-plain-before-style invariant.
func (a *App) renderDetail(w int) string {
	inner := w - 4
	h := a.detailTarget
	t := a.detailTranscript

	heading := memory.CleanText(h.Title)
	if heading == "" {
		heading = memory.CleanText(h.Ask)
	}
	if heading == "" {
		heading = "(untitled session)"
	}

	cwd := t.Cwd
	if cwd == "" {
		cwd = h.Cwd
	}

	var body []string
	body = append(body, truncPlain(heading, inner), "")
	body = append(body, truncPlain(projectLabel(h)+" · "+cwd, inner))

	var dateRange string
	if first := humanAge(a.now, t.FirstTS); first != "" {
		dateRange = first
		if last := humanAge(a.now, t.LastTS); last != "" && last != first {
			dateRange += " – " + last
		}
	}
	msgLine := fmt.Sprintf("%d messages", t.MsgCount)
	if t.FileMissing {
		msgLine += " · file missing"
	}
	if dateRange != "" {
		msgLine = dateRange + " · " + msgLine
	}
	body = append(body, truncPlain(msgLine, inner), "")

	body = append(body, truncPlain("Ask: "+memory.CleanText(t.Ask), inner))
	body = append(body, truncPlain("Outcome: "+memory.CleanText(t.Outcome), inner))
	body = append(body, truncPlain("Files: "+filesSummary(t.Files), inner), "")

	switch {
	case a.detailSummarizing:
		body = append(body, styHelp.Render(truncPlain("Summary: summarizing…", inner)))
	case a.detailConfirmRegen:
		body = append(body, truncPlain("Summary: "+memory.CleanText(t.LLMSummary), inner))
		body = append(body, styHelp.Render(truncPlain("press s again — uses plan quota", inner)))
	case t.LLMSummary != "":
		body = append(body, truncPlain("Summary: "+memory.CleanText(t.LLMSummary), inner))
	default:
		body = append(body, styHelp.Render(truncPlain("Summary: press s to summarize (uses plan quota)", inner)))
	}

	if a.detailHint != "" {
		body = append(body, "", styHelp.Render(truncPlain(a.detailHint, inner)))
	}

	if h.Snippet != "" {
		body = append(body, "", renderSnippet(h.Snippet, inner))
	}

	keybar := "s summarize · esc back · q quit"
	if a.detailReturn != viewLauncher && !a.resumeDisabled() {
		keybar = "r resume · " + keybar
	}
	return frame(w, "detail", "", body, keybar)
}

// filesSummary renders Transcript.Files (a "\n"-joined path list — see
// indexer.go's joinFiles) as a compact comma list: the first ~8 entries,
// each memory.CleanText'd for defense-in-depth (paths are ordinary indexer
// output, not untrusted model text, but a stray control byte here would
// still break the frame's exact-width invariant), plus a "+N more" tail
// when there are more. Splitting BEFORE cleaning matters: CleanText
// collapses "\n" runs to a single space, which would destroy the very
// separator this function needs to split on.
func filesSummary(files string) string {
	if files == "" {
		return "(none)"
	}
	list := strings.Split(files, "\n")
	shown, more := list, 0
	if len(list) > 8 {
		shown, more = list[:8], len(list)-8
	}
	cleaned := make([]string, len(shown))
	for i, f := range shown {
		cleaned[i] = memory.CleanText(f)
	}
	out := strings.Join(cleaned, ", ")
	if more > 0 {
		out += fmt.Sprintf(" +%d more", more)
	}
	return out
}

// statusGlyphWord maps a store row's last_status to a plain (unstyled)
// glyph+word pair for the wall — the same states statusIcon()
// recognizes, but returned as bare text so the caller can truncate the
// composed line before applying any style (truncate-before-style
// invariant), rather than embedding a pre-styled glyph mid-line.
func statusGlyphWord(status string) (string, string) {
	switch status {
	case "needs_you":
		return "●", "needs you"
	case "running":
		return "◐", "running"
	case "idle":
		return "○", "idle"
	case "done":
		return "✓", "done"
	case "error":
		return "✗", "error"
	case "unknown":
		// A freshly-started run's step session hasn't had its first status
		// poll land yet (session/launch.go seeds LastStatus="unknown") — show
		// "starting" rather than the more alarming-looking "unknown", glyph
		// unchanged.
		return "·", "starting"
	default:
		return "·", "unknown"
	}
}
