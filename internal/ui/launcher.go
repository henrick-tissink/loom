package ui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
)

var (
	modelOptions = []string{"", "opus", "sonnet", "fable"}
	modeOptions  = []string{"", "plan", "acceptEdits", "auto", "bypassPermissions"}
)

func optLabel(v string) string {
	if v == "" {
		return "default"
	}
	return v
}

// launcherForm is a minimal 4-field form: project / model / mode / seed.
// tab moves fields, ←/→ cycle selects, enter submits, esc cancels.
type launcherForm struct {
	projects []registry.Project
	projIdx  int
	modelIdx int
	modeIdx  int
	seed     textinput.Model
	focus    int // 0=project 1=model 2=mode 3=seed
}

func newLauncherForm(projects []registry.Project) launcherForm {
	ti := textinput.New()
	ti.Placeholder = "optional seed prompt or /slash-command"
	ti.CharLimit = 500
	return launcherForm{projects: projects, seed: ti}
}

func (f *launcherForm) Recipe() (session.Recipe, bool) {
	if len(f.projects) == 0 {
		return session.Recipe{}, false
	}
	p := f.projects[f.projIdx]
	return session.Recipe{
		ProjectLabel: p.Label,
		Cwd:          p.Path,
		Model:        modelOptions[f.modelIdx],
		Mode:         modeOptions[f.modeIdx],
		Seed:         f.seed.Value(),
	}, true
}

func cycle(idx, delta, n int) int { return ((idx+delta)%n + n) % n }

// update handles a form-focused key (spec §3): tab/shift-tab cycle the 4
// form fields (0..3, wrapping); left/right cycle the focused field's value.
// down/up are NOT handled here (spec §3 split from tab/shift-tab: down/up
// also cross into/out of the RELATED panel, which lives on App, not here —
// see App.launcherDown/launcherUp) — this used to share a branch with tab/
// shift-tab before the panel existed.
func (f *launcherForm) update(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyTab:
		f.setFocus(cycle(f.focus, 1, 4))
		return nil
	case tea.KeyShiftTab:
		f.setFocus(cycle(f.focus, -1, 4))
		return nil
	case tea.KeyLeft, tea.KeyRight:
		d := 1
		if msg.Type == tea.KeyLeft {
			d = -1
		}
		switch f.focus {
		case 0:
			if n := len(f.projects); n > 0 {
				f.projIdx = cycle(f.projIdx, d, n)
			}
		case 1:
			f.modelIdx = cycle(f.modelIdx, d, len(modelOptions))
		case 2:
			f.modeIdx = cycle(f.modeIdx, d, len(modeOptions))
		}
		return nil
	}
	if f.focus == 3 {
		var cmd tea.Cmd
		f.seed, cmd = f.seed.Update(msg)
		return cmd
	}
	return nil
}

func (f *launcherForm) setFocus(n int) {
	f.focus = n
	if n == 3 {
		f.seed.Focus()
	} else {
		f.seed.Blur()
	}
}

// view renders the 4-field form. active is false when focus has moved into
// the RELATED panel (spec §3) — in that case no field shows the cursor
// marker, since focus has left the form entirely (exactly one thing
// highlighted at a time, mirroring the panel's own cursor highlight).
func (f *launcherForm) view(active bool) string {
	sel := func(i int, label, val string) string {
		marker := "  "
		if active && f.focus == i {
			marker = styCursor.Render("▸ ")
		}
		return fmt.Sprintf("%s%-9s ‹ %s ›", marker, label, val)
	}
	proj := "(no projects found)"
	if len(f.projects) > 0 {
		proj = f.projects[f.projIdx].Label
	}
	seedMarker := "  "
	if active && f.focus == 3 {
		seedMarker = styCursor.Render("▸ ")
	}
	// The title and a help footer are NOT rendered here: View() wraps this
	// form in frame(), which already supplies both (frame title + keybar).
	// Emitting them here too produced a duplicated title and a keybar that
	// could drift from the frame's (finding: launcher double-title/hints).
	return sel(0, "project", proj) + "\n" +
		sel(1, "model", optLabel(modelOptions[f.modelIdx])) + "\n" +
		sel(2, "mode", optLabel(modeOptions[f.modeIdx])) + "\n" +
		seedMarker + "seed      " + f.seed.View()
}
