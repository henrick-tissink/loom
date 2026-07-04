package ui

import (
	"encoding/json"
	"errors"
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
	"github.com/henricktissink/loom/internal/workflow"
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
	viewWorkflows
	viewWFConfirm
	viewWFConfirmAbandon
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

	// Runner/WorkflowsDir back the workflows view (Task 3, the `w` view).
	// Both are nil-safe: a nil Runner means the workflows view still opens
	// (LoadAll needs only Projects/WorkflowsDir) but shows an empty RUNS
	// section and no-ops every run action; an empty WorkflowsDir makes
	// LoadAll's os.ReadDir("") fail with IsNotExist, which LoadAll already
	// treats as "no definitions" rather than an error. Projects (existing
	// field above) doubles as the registry LoadAll validates step-1 projects
	// against — the brief's "Registry" field is this one, reused.
	Runner       *workflow.Runner
	WorkflowsDir string
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
	// (same captured-target discipline as actionTarget/peekTarget).
	// detailTranscript is fetched once at that same moment (and re-fetched
	// after a successful summarize) — r/s below act on detailTarget's
	// SessionID/detailTranscript, never on any live selection.
	detailTarget     store.SearchHit
	detailTranscript store.Transcript

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

	// --- Task 3: workflows view (`w`) ---------------------------------

	// wfRuns/wfDefs/wfLoadErrs are the two RUNS/WORKFLOWS sections,
	// refreshed by wfLoadCmd on every view-open and after every action
	// (spec §4: "RUNS rows render honestly from store rows"). wfCursor
	// indexes the concatenation wfRuns++wfDefs (LoadErrors are informational
	// only, never selectable) — same clamp discipline as the dashboard's
	// cursor/rows in rebuildRows.
	wfRuns     []wfRunRow
	wfDefs     []workflow.Definition
	wfLoadErrs []workflow.LoadError
	wfCursor   int

	// wfHint is a transient dim message in the workflows body — currently
	// only the dead-attach hint (spec §2.8), same captured/transient
	// discipline as detailHint.
	wfHint string

	// wfTarget is the run row captured at the moment 'n'/'x' opens
	// viewWFConfirm/viewWFConfirmAbandon (captured-target discipline, same
	// as actionTarget/peekTarget/detailTarget elsewhere). wfPreview is the
	// StepPreview fetched (via a tea.Cmd — Preview reads a transcript file)
	// at confirm-open time (spec §2.11); wfPreviewLoading/wfPreviewErr cover
	// the in-flight/failed states before it arrives; wfContinueDead is set
	// when an advance attempt comes back ErrContinueDead (spec §2.8),
	// arming the one-shot 'f' fork-demotion recovery key.
	wfTarget         store.RunRow
	wfPreview        workflow.StepPreview
	wfPreviewLoading bool
	wfPreviewErr     string
	wfContinueDead   bool

	// wfInFlight (keyed by run id) / wfStartInFlight (keyed by definition
	// path) are the in-flight guards (spec §2.6/§4, the detailSummarizing
	// precedent): a second 'n'/'x'/↵ on the same target while its previous
	// action is still resolving is a no-op rather than firing a second
	// launch/CAS.
	wfInFlight      map[int64]bool
	wfStartInFlight map[string]bool
}

// wfRunRow is one RUNS-section entry: a store.RunRow plus the display/action
// facts wfLoadCmd resolves for it up front (parsing def_json, resolving the
// current step's session BY IDENTITY via Runner.ResolveStepSession — spec
// §2.5) so rendering and attach/hint decisions never need to touch the
// Runner or re-parse JSON themselves.
type wfRunRow struct {
	run store.RunRow

	// stepLabel is "step N/M label" (1-based) built from the parsed
	// def_json snapshot; defErr is true when def_json failed to parse
	// (spec §2.12: "renders dim-red, abandonable, never panics") — in that
	// case stepLabel is empty and the renderer substitutes a fixed message.
	stepLabel string
	defErr    bool

	// resolved/resolvedOK/live are ResolveStepSession's result: ok=false
	// only when the pinned session name has no store row at all (the
	// documented Launch-failed-after-CAS accepted failure mode). live
	// mirrors isLiveRow(resolved) and gates attach (spec §2.8: attach only
	// when resolved-live, else the dead-attach hint).
	resolved   store.SessionRow
	resolvedOK bool
	live       bool
}

// wfEntryKind distinguishes a RUNS-section row from a WORKFLOWS-section row
// in the shared cursor space (spec §4: "↓/↑ across sections").
type wfEntryKind int

const (
	wfEntryRun wfEntryKind = iota
	wfEntryDef
)

// wfEntry is what a.wfSelected() returns: exactly one of run/def is
// meaningful, discriminated by kind.
type wfEntry struct {
	kind wfEntryKind
	run  wfRunRow
	def  workflow.Definition
}

// wfActionKind discriminates wfActionMsg's four possible sources — needed
// only so the ErrContinueDead→"offer f" recovery applies to advance and not
// to finish/abandon/retry (spec §2.8 is specifically about advancing into a
// dead continue target).
type wfActionKind int

const (
	wfActionAdvance wfActionKind = iota
	wfActionFinish
	wfActionAbandon
	wfActionRetry
)

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

	// resumeBlockedMsg reports the resume-collision guard (spec §6 P0) found
	// a live row for sessionID and — critically — did NOT call
	// Launcher.Resume. Same sessionID-staleness discipline as summaryMsg.
	resumeBlockedMsg struct{ sessionID string }

	// wfLoadedMsg is the result of wfLoadCmd (spec §4: LoadAll + ActiveRuns,
	// both file/db IO, always run in a tea.Cmd — fired on every workflows-
	// view open). Applied unconditionally (idempotent background refresh);
	// harmless even if the user has since navigated away.
	wfLoadedMsg struct {
		runs     []wfRunRow
		defs     []workflow.Definition
		loadErrs []workflow.LoadError
		err      error
	}

	// wfPreviewMsg is Runner.Preview's result for a confirm dialog (spec
	// §2.11). Stale (view no longer viewWFConfirm, or runID no longer
	// matches the captured wfTarget) is discarded — same discipline as
	// peekMsg/summaryMsg.
	wfPreviewMsg struct {
		runID   int64
		preview workflow.StepPreview
		err     error
	}

	// wfStartMsg is Runner.Start's result (spec §2.10): path identifies
	// which definition's in-flight guard to release.
	wfStartMsg struct {
		path string
		err  error
	}

	// wfActionMsg is the shared result type for advance/finish/abandon/
	// retry (spec §2.6/§2.7/§2.9/§2.12) — all four are CAS-guarded or
	// otherwise idempotent store writes, all release the per-run-id
	// in-flight guard the same way. runName is the target run's Name,
	// captured at command-fire time — used only to label a STALE result's
	// errStr (see the wfActionMsg case), since a stale result must not touch
	// a.wfTarget to read it back off.
	wfActionMsg struct {
		kind    wfActionKind
		runID   int64
		runName string
		err     error
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
	case wfLoadedMsg:
		if m.err != nil {
			a.errStr = m.err.Error()
		}
		a.wfRuns = m.runs
		a.wfDefs = m.defs
		a.wfLoadErrs = m.loadErrs
		if n := len(a.wfRuns) + len(a.wfDefs); a.wfCursor >= n {
			a.wfCursor = max(0, n-1)
		}
		return a, nil
	case wfPreviewMsg:
		// stale: confirm was cancelled, or a different run's confirm has
		// since been opened (same discipline as peekMsg/summaryMsg).
		if a.view != viewWFConfirm || m.runID != a.wfTarget.ID {
			return a, nil
		}
		a.wfPreviewLoading = false
		if m.err != nil {
			a.wfPreviewErr = m.err.Error()
			return a, nil
		}
		a.wfPreview = m.preview
		return a, nil
	case wfStartMsg:
		delete(a.wfStartInFlight, m.path)
		if m.err != nil {
			a.errStr = m.err.Error()
			return a, nil
		}
		// "run appears, stay in view" (spec §4): reload RUNS/WORKFLOWS so
		// the new run shows up, rather than hand-splicing a synthetic row.
		return a, a.wfLoadCmd()
	case wfActionMsg:
		a.clearWFInFlight(m.runID) // own-runID guard release: correct regardless of staleness
		// Staleness gate (same discipline as wfPreviewMsg above, spec
		// regression fix): this result belongs to the CURRENTLY open confirm
		// only if that confirm's view matches the kind AND its captured
		// wfTarget is still this run. Reachable bug this closes: run A's
		// advance confirm is cancelled (esc) while its advance is still in
		// flight, run B's confirm is then opened, and A's delayed result
		// (e.g. ErrContinueDead) arrives — it must NOT arm B's 'f' recovery
		// or otherwise touch B's open confirm. A stale result only clears its
		// own in-flight guard (above) and surfaces a run-name-qualified
		// errStr; it never mutates a.view/wfContinueDead.
		fresh := m.runID == a.wfTarget.ID &&
			((m.kind == wfActionAbandon && a.view == viewWFConfirmAbandon) ||
				((m.kind == wfActionAdvance || m.kind == wfActionFinish) && a.view == viewWFConfirm))
		if !fresh {
			if m.err != nil {
				a.errStr = fmt.Sprintf("run %s#%d: %s", m.runName, m.runID, m.err.Error())
			}
			return a, a.wfLoadCmd()
		}
		if m.err == nil {
			a.view = viewWorkflows
			return a, a.wfLoadCmd()
		}
		if m.kind == wfActionAdvance && errors.Is(m.err, workflow.ErrContinueDead) {
			// spec §2.8: stay in the confirm, arm the one-shot 'f' recovery
			// (demote this advance to fork) instead of surfacing a dead end.
			a.wfContinueDead = true
			return a, nil
		}
		a.errStr = m.err.Error()
		a.view = viewWorkflows
		return a, a.wfLoadCmd()
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
		return a.updateDetailKeys(msg)

	case viewWorkflows:
		return a.updateWorkflowsKeys(msg)

	case viewWFConfirm:
		return a.updateWFConfirmKeys(msg)

	case viewWFConfirmAbandon:
		return a.updateWFAbandonKeys(msg)
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
	case "w":
		return a, a.openWorkflows()
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
			a.openDetail(a.searchHits[a.searchCursor])
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

// openDetail captures hit as detailTarget (spec §6 captured-target
// discipline) and fetches its full Transcript once, at open time — a plain
// synchronous Store call (same precedent as viewTag's SetTags in
// updateKeys), not a tea.Cmd round trip: it's a single indexed row lookup,
// nowhere near the cost that would justify async staleness handling. A nil
// Store (Deps{} in dashboard-only tests) leaves detailTranscript zeroed,
// which resumeDisabled() correctly reads as "no cwd, disabled".
func (a *App) openDetail(hit store.SearchHit) {
	a.detailTarget = hit
	a.detailTranscript = store.Transcript{}
	a.detailHint = ""
	a.detailConfirmRegen = false
	a.detailSummarizing = false
	if st := a.deps.Store; st != nil {
		if t, ok, err := st.GetTranscript(hit.SessionID); err == nil && ok {
			a.detailTranscript = t
		}
	}
	a.view = viewDetail
}

// updateDetailKeys handles keys while viewDetail is open (spec §6): r
// resume (collision-guarded), s summarize (press-twice-to-regenerate), esc
// back to search (results/input untouched — viewSearch's own state isn't
// touched by any of this), q/ctrl+c quit.
func (a *App) updateDetailKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return a, tea.Quit
	case "esc":
		a.view = viewSearch
		return a, nil
	case "r":
		if a.resumeDisabled() {
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

// --- Task 3: workflows view (`w`) ---------------------------------------

// openWorkflows resets the workflows view to a fresh, empty state (spec §4)
// and kicks off the initial load — same "fresh state + immediate refresh
// cmd" shape as openSearch.
func (a *App) openWorkflows() tea.Cmd {
	a.view = viewWorkflows
	a.wfCursor = 0
	a.errStr = ""
	a.wfHint = ""
	a.wfRuns = nil
	a.wfDefs = nil
	a.wfLoadErrs = nil
	return a.wfLoadCmd()
}

// wfLoadCmd loads both workflows-view sections (spec §4/§2.1): WORKFLOWS via
// workflow.LoadAll(WorkflowsDir, Projects) — the registry Definitions §3
// validates step-1 projects against — and RUNS via Runner.Store.ActiveRuns,
// each resolved into a wfRunRow up front (buildWFRunRow). Both deps are
// nil-safe: a nil Runner (or nil Runner.Store) yields an empty RUNS section
// rather than panicking, matching Deps' nil-safety contract elsewhere.
func (a *App) wfLoadCmd() tea.Cmd {
	dir := a.deps.WorkflowsDir
	projects := a.deps.Projects
	runner := a.deps.Runner
	return func() tea.Msg {
		defs, loadErrs := workflow.LoadAll(dir, projects)
		var rows []wfRunRow
		if runner != nil && runner.Store != nil {
			runs, err := runner.Store.ActiveRuns()
			if err != nil {
				return wfLoadedMsg{defs: defs, loadErrs: loadErrs, err: err}
			}
			for _, run := range runs {
				rows = append(rows, buildWFRunRow(runner, run))
			}
		}
		return wfLoadedMsg{runs: rows, defs: defs, loadErrs: loadErrs}
	}
}

// buildWFRunRow resolves the display/action facts for one active run (spec
// §2.5/§2.12): parses its def_json snapshot (a corrupt snapshot sets defErr
// rather than panicking — the run still renders, dim-red, abandonable) and
// resolves its current step's session BY IDENTITY (never the pinned tmux
// name alone — spec §2.5) via ResolveStepSession, which itself tolerates an
// empty/dead pin (ok=false — the Launch-failed-after-CAS accepted failure
// mode).
func buildWFRunRow(runner *workflow.Runner, run store.RunRow) wfRunRow {
	w := wfRunRow{run: run}
	var def workflow.Definition
	if err := json.Unmarshal([]byte(run.DefJSON), &def); err != nil ||
		len(def.Steps) == 0 || run.StepIdx < 0 || int(run.StepIdx) >= len(def.Steps) {
		w.defErr = true
	} else {
		w.stepLabel = fmt.Sprintf("step %d/%d %s", run.StepIdx+1, len(def.Steps), def.Steps[run.StepIdx].Label)
	}
	if row, ok := runner.ResolveStepSession(run); ok {
		w.resolved = row
		w.resolvedOK = true
		w.live = isLiveRow(row)
	}
	return w
}

// wfSelected returns the entry under wfCursor in the shared RUNS++WORKFLOWS
// cursor space, or ok=false when nothing is selectable (empty/out of range —
// same shape as selected() above).
func (a *App) wfSelected() (wfEntry, bool) {
	n := len(a.wfRuns) + len(a.wfDefs)
	if a.wfCursor < 0 || a.wfCursor >= n {
		return wfEntry{}, false
	}
	if a.wfCursor < len(a.wfRuns) {
		return wfEntry{kind: wfEntryRun, run: a.wfRuns[a.wfCursor]}, true
	}
	return wfEntry{kind: wfEntryDef, def: a.wfDefs[a.wfCursor-len(a.wfRuns)]}, true
}

// markInFlight/clearWFInFlight guard per-run-id actions (spec §2.6 in-flight
// guard, the detailSummarizing precedent) against a double press firing a
// second launch/CAS while the first is still resolving.
func (a *App) markInFlight(id int64) {
	if a.wfInFlight == nil {
		a.wfInFlight = map[int64]bool{}
	}
	a.wfInFlight[id] = true
}

func (a *App) clearWFInFlight(id int64) {
	delete(a.wfInFlight, id)
}

// updateWorkflowsKeys handles keys while viewWorkflows is open (spec §4):
// ↓/↑ move across BOTH sections, ↵ starts a definition or attaches a
// resolved-live run, n opens the advance confirm (or retries a pending
// seed directly), x opens the abandon confirm, esc back to the dashboard.
func (a *App) updateWorkflowsKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		if n := len(a.wfRuns) + len(a.wfDefs); a.wfCursor < n-1 {
			a.wfCursor++
		}
		return a, nil
	case "k", "up":
		if a.wfCursor > 0 {
			a.wfCursor--
		}
		return a, nil
	case "enter":
		a.wfHint = ""
		return a.wfActivate()
	case "n":
		a.wfHint = ""
		return a.wfPressN()
	case "x":
		a.wfHint = ""
		return a.wfPressX()
	}
	return a, nil
}

// wfActivate handles ↵ (spec §4): a definition row starts a new run (guarded
// per definition path against a double-launch); a run row attaches ONLY
// when its current step resolved live — otherwise the dead-attach hint
// (spec §2.8), never a raw tmux error.
func (a *App) wfActivate() (tea.Model, tea.Cmd) {
	e, ok := a.wfSelected()
	if !ok {
		return a, nil
	}
	switch e.kind {
	case wfEntryDef:
		if a.deps.Runner == nil || a.wfStartInFlight[e.def.Path] {
			return a, nil
		}
		if a.wfStartInFlight == nil {
			a.wfStartInFlight = map[string]bool{}
		}
		a.wfStartInFlight[e.def.Path] = true
		w, h := a.width, a.height
		return a, a.wfStartCmd(e.def, w, h)
	case wfEntryRun:
		if e.run.resolvedOK && e.run.live {
			if a.deps.Tmux == nil {
				return a, nil
			}
			cmd := a.deps.Tmux.AttachCmd(e.run.resolved.Name)
			return a, tea.ExecProcess(cmd, func(err error) tea.Msg { return attachedMsg{err} })
		}
		a.wfHint = "step session ended — n advance (f fork) · x abandon"
		return a, nil
	}
	return a, nil
}

// wfPressN handles 'n' on a run row (spec §2.9/§2.11): a run with a
// pending_seed retries delivery directly (guarded — this fires the action
// immediately, no confirm step); otherwise it opens the advance confirm and
// fetches its Preview (a tea.Cmd — Preview reads a transcript file, never
// inline in Update). Opening the confirm itself is NOT guarded/marked
// in-flight: fetching a preview launches nothing, and once the confirm is
// open a second 'n' press routes to updateWFConfirmKeys (which reads 'n' as
// cancel, not "reopen") — so the double-press race this guards against is
// entirely at the 'y'/'f' press, handled there.
func (a *App) wfPressN() (tea.Model, tea.Cmd) {
	e, ok := a.wfSelected()
	if !ok || e.kind != wfEntryRun || a.deps.Runner == nil {
		return a, nil
	}
	run := e.run.run
	if run.PendingSeed != "" {
		if a.wfInFlight[run.ID] {
			return a, nil
		}
		a.markInFlight(run.ID)
		return a, a.retryCmd(run)
	}
	a.wfTarget = run
	a.wfPreview = workflow.StepPreview{}
	a.wfPreviewLoading = true
	a.wfPreviewErr = ""
	a.wfContinueDead = false
	a.view = viewWFConfirm
	return a, a.previewCmd(run)
}

// wfPressX opens the abandon confirm for the selected run row (spec §2.12).
func (a *App) wfPressX() (tea.Model, tea.Cmd) {
	e, ok := a.wfSelected()
	if !ok || e.kind != wfEntryRun || a.deps.Runner == nil {
		return a, nil
	}
	run := e.run.run
	if a.wfInFlight[run.ID] {
		return a, nil
	}
	a.wfTarget = run
	a.view = viewWFConfirmAbandon
	return a, nil
}

// updateWFConfirmKeys handles keys while viewWFConfirm is open (spec §2.6/
// §2.8/§2.11): 'y' fires the advance/finish (re-verified against a fresh
// read first — see advanceCmd/finishCmd), guarded per run id against a
// double press firing two launches before the first result returns; 'f' is
// the one-shot fork-demotion recovery, only accepted once wfContinueDead is
// armed, guarded the same way; 'n'/'esc' cancel (nothing is in-flight yet at
// that point — see wfPressN's doc comment — so there is nothing to
// release).
func (a *App) updateWFConfirmKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return a, tea.Quit
	}
	switch msg.String() {
	case "q":
		return a, tea.Quit
	case "esc", "n":
		a.view = viewWorkflows
		return a, nil
	case "y":
		a.wfHint = ""
		if a.wfPreviewLoading || a.wfPreviewErr != "" || a.wfContinueDead || a.deps.Runner == nil {
			return a, nil // not (yet/anymore) a valid confirm state
		}
		target := a.wfTarget
		if a.wfInFlight[target.ID] {
			return a, nil // double-press guard: an advance/finish is already in flight
		}
		a.markInFlight(target.ID)
		if a.wfPreview.Finish {
			return a, a.finishCmd(target)
		}
		w, h := a.width, a.height
		return a, a.advanceCmd(target, false, w, h)
	case "f":
		a.wfHint = ""
		if !a.wfContinueDead || a.deps.Runner == nil {
			return a, nil
		}
		target := a.wfTarget
		if a.wfInFlight[target.ID] {
			return a, nil // double-press guard, same as 'y' above
		}
		a.wfContinueDead = false
		a.markInFlight(target.ID)
		w, h := a.width, a.height
		return a, a.advanceCmd(target, true, w, h)
	}
	return a, nil
}

// updateWFAbandonKeys handles keys while viewWFConfirmAbandon is open.
func (a *App) updateWFAbandonKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return a, tea.Quit
	}
	switch msg.String() {
	case "q":
		return a, tea.Quit
	case "n", "esc":
		a.view = viewWorkflows
		return a, nil
	case "y":
		// Stays on viewWFConfirmAbandon until the wfActionMsg result lands
		// (same discipline as updateWFConfirmKeys' advance/finish 'y'
		// above) rather than switching to viewWorkflows here: the Update
		// staleness gate (spec regression fix) requires a.view still be
		// viewWFConfirmAbandon AND m.kind==wfActionAbandon for the result
		// to be treated as "fresh" — flipping the view early made that
		// branch unreachable and every abandon result take the stale path
		// instead (still correct end state, but via the wrong branch and
		// with an unintended run-name-qualified error string).
		if a.deps.Runner == nil {
			return a, nil
		}
		target := a.wfTarget
		if a.wfInFlight[target.ID] {
			return a, nil
		}
		a.markInFlight(target.ID)
		return a, a.abandonCmd(target)
	}
	return a, nil
}

// previewCmd fetches Runner.Preview(target) for the advance confirm dialog
// (spec §2.11: substitution runs at confirm-OPEN time).
func (a *App) previewCmd(target store.RunRow) tea.Cmd {
	runner := a.deps.Runner
	if runner == nil {
		return nil
	}
	return func() tea.Msg {
		p, err := runner.Preview(target)
		return wfPreviewMsg{runID: target.ID, preview: p, err: err}
	}
}

// advanceCmd fires Runner.Advance for target (spec §2.6/§2.8). Per the
// spec's "the confirm re-verifies the captured step_idx against a fresh
// read before firing": it re-reads the run FIRST and refuses to advance at
// all — surfacing the same ErrRunAdvancedElsewhere the CAS itself would —
// if the row is no longer 'running' or its step_idx has moved since the
// confirm opened (the preview the user is looking at would otherwise be
// stale). Only once that check passes does it call Advance with the FRESH
// row (which still has its own CAS as the final word against any race in
// between).
func (a *App) advanceCmd(target store.RunRow, forceFork bool, w, h int) tea.Cmd {
	runner := a.deps.Runner
	if runner == nil {
		return nil
	}
	return func() tea.Msg {
		fresh, ok, err := runner.Store.GetRun(target.ID)
		if err != nil {
			return wfActionMsg{kind: wfActionAdvance, runID: target.ID, runName: target.Name, err: err}
		}
		if !ok || fresh.Status != "running" || fresh.StepIdx != target.StepIdx {
			return wfActionMsg{kind: wfActionAdvance, runID: target.ID, runName: target.Name, err: workflow.ErrRunAdvancedElsewhere}
		}
		err = runner.Advance(fresh, forceFork, w, h, time.Now())
		return wfActionMsg{kind: wfActionAdvance, runID: target.ID, runName: target.Name, err: err}
	}
}

// finishCmd marks target done (spec §2.7: terminal-step confirm, no launch,
// no append) via Runner.Finish, which itself CAS-conditions the actual write
// on run.StepIdx/status='running' (FinishRunCAS) — this fresh-read is a
// re-verification for a prompt error message, not the sole gate; the write
// is guarded again, independently, at the point it happens.
func (a *App) finishCmd(target store.RunRow) tea.Cmd {
	runner := a.deps.Runner
	if runner == nil {
		return nil
	}
	return func() tea.Msg {
		fresh, ok, err := runner.Store.GetRun(target.ID)
		if err != nil {
			return wfActionMsg{kind: wfActionFinish, runID: target.ID, runName: target.Name, err: err}
		}
		if !ok || fresh.Status != "running" || fresh.StepIdx != target.StepIdx {
			return wfActionMsg{kind: wfActionFinish, runID: target.ID, runName: target.Name, err: workflow.ErrRunAdvancedElsewhere}
		}
		err = runner.Finish(fresh, time.Now())
		return wfActionMsg{kind: wfActionFinish, runID: target.ID, runName: target.Name, err: err}
	}
}

// abandonCmd marks target abandoned (spec §2.12: abandon ≠ kill — the
// step's session is left running untouched).
func (a *App) abandonCmd(target store.RunRow) tea.Cmd {
	runner := a.deps.Runner
	if runner == nil {
		return nil
	}
	return func() tea.Msg {
		err := runner.Abandon(target, time.Now())
		return wfActionMsg{kind: wfActionAbandon, runID: target.ID, runName: target.Name, err: err}
	}
}

// retryCmd re-attempts a run's pending_seed delivery (spec §2.9), re-reading
// the run first so a slow retry acts on current state.
func (a *App) retryCmd(target store.RunRow) tea.Cmd {
	runner := a.deps.Runner
	if runner == nil {
		return nil
	}
	return func() tea.Msg {
		fresh, ok, err := runner.Store.GetRun(target.ID)
		if err != nil {
			return wfActionMsg{kind: wfActionRetry, runID: target.ID, runName: target.Name, err: err}
		}
		if !ok {
			return wfActionMsg{kind: wfActionRetry, runID: target.ID, runName: target.Name, err: errors.New("workflow: run not found")}
		}
		err = runner.RetryPendingSeed(fresh)
		return wfActionMsg{kind: wfActionRetry, runID: target.ID, runName: target.Name, err: err}
	}
}

// wfStartCmd launches a brand-new run of def (spec §2.10).
func (a *App) wfStartCmd(def workflow.Definition, w, h int) tea.Cmd {
	runner := a.deps.Runner
	if runner == nil {
		return nil
	}
	return func() tea.Msg {
		_, err := runner.Start(def, w, h, time.Now())
		return wfStartMsg{path: def.Path, err: err}
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
		return a.renderDetail(w)
	case viewWorkflows:
		return a.renderWorkflows(w)
	case viewWFConfirm:
		return a.renderWFConfirm(w)
	case viewWFConfirmAbandon:
		return a.renderWFAbandon(w)
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
		keybar += " · / search · w workflows"
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
	if !a.resumeDisabled() {
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

// --- Task 3: workflows view rendering -----------------------------------

// renderWorkflows renders the two-section RUNS/WORKFLOWS frame (spec §4).
func (a *App) renderWorkflows(w int) string {
	inner := w - 4
	var body []string
	cursorLine := 0
	body = append(body, "")

	body = append(body, sectionRule("RUNS", inner, false))
	if len(a.wfRuns) == 0 {
		body = append(body, styHelp.Render(truncPlain("no active runs", inner)))
	} else {
		for i, e := range a.wfRuns {
			if i == a.wfCursor {
				cursorLine = len(body)
			}
			body = append(body, a.renderWFRunLine(i, e, inner))
		}
	}

	body = append(body, "", sectionRule("WORKFLOWS", inner, false))
	if len(a.wfDefs) == 0 && len(a.wfLoadErrs) == 0 {
		body = append(body, styHelp.Render(truncPlain("no workflow definitions found", inner)))
	} else {
		for j, d := range a.wfDefs {
			idx := len(a.wfRuns) + j
			if idx == a.wfCursor {
				cursorLine = len(body)
			}
			body = append(body, a.renderWFDefLine(idx, d, inner))
		}
		for _, le := range a.wfLoadErrs {
			body = append(body, renderWFLoadErr(le, inner))
		}
	}

	if a.wfHint != "" {
		body = append(body, "", styHelp.Render(truncPlain(a.wfHint, inner)))
	}
	if a.errStr != "" {
		body = append(body, "", styErr.Render(truncPlain("! "+a.errStr, inner)))
	}
	if a.height > 2 {
		body = windowBody(body, cursorLine, a.height-2)
	}
	return frame(w, "workflows", "", body, "↵ start/attach · n advance · x abandon · esc dash · q quit")
}

// renderWFRunLine renders one RUNS row (spec §4): "name#id · step N/M label
// · <glyph> <status word>" plus the pending/failed seed markers. The whole
// line is composed as PLAIN text and truncated BEFORE any styling is
// applied (truncate-before-style invariant), then the fully-truncated line
// gets exactly one style: cursor accent when selected, dim-red when the
// run's def_json failed to parse (spec §2.12), otherwise unstyled.
func (a *App) renderWFRunLine(i int, e wfRunRow, inner int) string {
	cursor := "  "
	if i == a.wfCursor {
		cursor = styCursor.Render("▸ ")
	}
	label := e.stepLabel
	if e.defErr {
		label = "corrupt run definition"
	}
	glyph, word := "✗", "dead"
	if e.resolvedOK {
		glyph, word = statusGlyphWord(e.resolved.LastStatus)
	}
	text := fmt.Sprintf("%s#%d · %s · %s %s", e.run.Name, e.run.ID, label, glyph, word)
	if e.run.PendingSeed != "" {
		text += " · seed pending"
	}
	if e.resolvedOK && e.resolved.SeedStatus == "failed" {
		text += " · seed FAILED"
	}
	avail := inner - 2
	if avail < 0 {
		avail = 0
	}
	text = truncPlain(text, avail)
	switch {
	case i == a.wfCursor:
		text = styCursor.Render(text)
	case e.defErr:
		text = styNeedsYou.Render(text)
	}
	return cursor + text
}

// renderWFDefLine renders one WORKFLOWS row: "name · N steps · project"
// (project = filepath.Base(step 1's resolved path) — Step.Project holds a
// registry-resolved absolute path post-LoadAll, never a bare label).
func (a *App) renderWFDefLine(idx int, d workflow.Definition, inner int) string {
	cursor := "  "
	if idx == a.wfCursor {
		cursor = styCursor.Render("▸ ")
	}
	proj := ""
	if len(d.Steps) > 0 {
		proj = filepath.Base(d.Steps[0].Project)
	}
	text := fmt.Sprintf("%s · %d step%s · %s", d.Name, len(d.Steps), plural(len(d.Steps)), proj)
	avail := inner - 2
	if avail < 0 {
		avail = 0
	}
	text = truncPlain(text, avail)
	if idx == a.wfCursor {
		text = styCursor.Render(text)
	}
	return cursor + text
}

// renderWFLoadErr renders one malformed-definition line, dim-red (spec
// §2.1: "malformed files listed dim-red with their error"), never
// selectable (no cursor marker — LoadErrors aren't part of the cursor
// space).
func renderWFLoadErr(e workflow.LoadError, inner int) string {
	avail := inner - 2
	if avail < 0 {
		avail = 0
	}
	text := truncPlain(filepath.Base(e.Path)+": "+e.Err, avail)
	return "  " + styNeedsYou.Render(text)
}

// statusGlyphWord maps a store row's last_status to a plain (unstyled)
// glyph+word pair for renderWFRunLine — the same states statusIcon()
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

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// renderWFConfirm renders the advance/finish confirm dialog (spec §2.11):
// substitution snippet (~60 chars) + relation wording (continue reads
// "sends into current session"), the finish variant, the unavailable
// warning, and the dead-continue recovery state (offering 'f').
func (a *App) renderWFConfirm(w int) string {
	inner := w - 4
	name := a.wfTarget.Name
	var body []string
	body = append(body, "")

	switch {
	case a.wfPreviewLoading:
		body = append(body, truncPlain("computing preview for "+name+"…", inner))
	case a.wfPreviewErr != "":
		body = append(body, styErr.Render(truncPlain("! "+a.wfPreviewErr, inner)))
	case a.wfContinueDead:
		body = append(body, truncPlain("step session ended — cannot continue", inner))
		body = append(body, styHelp.Render(truncPlain("f fork from transcript instead · esc cancel", inner)))
	case a.wfPreview.Finish:
		body = append(body, truncPlain(fmt.Sprintf("finish run %s?", name), inner))
	default:
		p := a.wfPreview
		nextN := a.wfTarget.StepIdx + 2 // 1-based display of the NEXT step
		var verb string
		if p.Relation == "continue" {
			verb = fmt.Sprintf("advance to step %d %s (continue) — sends into current session", nextN, p.Label)
		} else {
			verb = fmt.Sprintf("advance to step %d %s (%s)", nextN, p.Label, p.Relation)
		}
		line := verb + fmt.Sprintf(" · seed: %q", truncPlain(p.Seed, 60))
		body = append(body, truncPlain(line, inner))
		if p.Unavailable {
			body = append(body, styHelp.Render(truncPlain("⚠ some template values unavailable", inner)))
		}
	}
	body = append(body, "")

	if a.errStr != "" {
		body = append(body, styErr.Render(truncPlain("! "+a.errStr, inner)))
	}

	keybar := "y confirm · n/esc cancel"
	switch {
	case a.wfContinueDead:
		keybar = "f fork instead · esc cancel"
	case a.wfPreviewErr != "":
		keybar = "esc cancel"
	}
	return frame(w, "advance "+name, "", body, keybar)
}

// renderWFAbandon renders the abandon confirm dialog (spec §2.12).
func (a *App) renderWFAbandon(w int) string {
	inner := w - 4
	r := a.wfTarget
	label := styNeedsYou.Render(fmt.Sprintf("%s#%d", r.Name, r.ID))
	body := []string{"", "  abandon " + label + " ?", ""}
	if a.errStr != "" {
		body = append(body, styErr.Render(truncPlain("! "+a.errStr, inner)))
	}
	return frame(w, "abandon run", "", body, "y confirm · n/esc cancel")
}
