package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

const pollInterval = 1500 * time.Millisecond

type view int

const (
	viewDash view = iota
	viewLauncher
	viewConfirmKill
	viewTag
)

type Deps struct {
	Engine     *status.Engine
	Launcher   *session.Launcher
	Projects   []registry.Project
	Tmux       *tmux.Client
	InsideTmux bool
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
}

type (
	tickMsg     time.Time
	pollNowMsg  struct{} // one-shot: "poll now", does NOT arm a new tick chain
	snapMsg     status.Snapshot
	errMsg      struct{ err error }
	attachedMsg struct{ err error }
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
			lastTool: r.LastTool, model: r.Model, mode: r.Mode, activity: r.Activity, row: r.SessionRow}
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
			status: r.LastStatus, model: r.Model, mode: r.Mode, recent: true, row: r})
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
		return a, nil
	case errMsg:
		a.errStr = m.err.Error()
		return a, nil
	case attachedMsg:
		if m.err != nil {
			a.errStr = "attach failed: " + m.err.Error()
		}
		return a, a.pollCmd()
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
	}
	return a, nil
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
