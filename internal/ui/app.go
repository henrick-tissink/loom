package ui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
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
			lastTool: r.LastTool, model: r.Model, mode: r.Mode, row: r.SessionRow}
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
	switch a.view {
	case viewLauncher:
		return a.form.view()
	case viewConfirmKill:
		r := a.actionTarget
		return fmt.Sprintf("kill %s (%s)? %s",
			r.label, r.name, styHelp.Render("y/n"))
	case viewTag:
		return styTitle.Render("tags") + "\n\n" + a.tag.View() + "\n\n" +
			styHelp.Render("enter save · esc cancel")
	}

	live, needs := 0, 0
	for _, r := range a.snap.Live {
		live++
		if r.Status == status.NeedsYou {
			needs++
		}
	}
	out := styTitle.Render("LOOM") +
		styMeta.Render(fmt.Sprintf("   %d live · %d needs you", live, needs)) + "\n\n"

	section := ""
	for i, r := range a.rows {
		sec := sectionFor(r)
		if sec != section {
			section = sec
			out += stySection.Render(section) + "\n"
		}
		cursor := "  "
		if i == a.cursor {
			cursor = styCursor.Render("▸ ")
		}
		hint := r.lastTool
		if hint != "" {
			hint = "⏺ " + hint
		}
		meta := trimMeta(r.model, r.mode)
		if r.row.SeedStatus == "failed" {
			meta += " · seed failed"
		}
		out += fmt.Sprintf("%s%s %-14s %-18s %s\n",
			cursor, statusIcon(r.status), r.label, hint,
			styMeta.Render(meta))
	}
	if len(a.rows) == 0 {
		out += styHelp.Render("no sessions — press n to launch one") + "\n"
	}
	if a.errStr != "" {
		out += "\n" + styErr.Render("! "+a.errStr) + "\n"
	}
	if a.deps.InsideTmux {
		out += styHelp.Render("(running inside tmux — attach opens a nested client; F12 detaches)") + "\n"
	}
	out += "\n" + styHelp.Render("[↵]attach [n]ew [x]kill [t]ag [r]eopen [q]uit  ·  [/]search·soon [w]orkflows·soon")
	return out
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

func trimMeta(model, mode string) string {
	if model == "" {
		model = "default"
	}
	if mode == "" {
		mode = "normal"
	}
	return model + " · " + mode
}
