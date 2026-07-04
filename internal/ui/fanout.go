package ui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
)

// fanoutForm is fan-out's OWN form (spec §2.1) — NOT launcher reuse: a
// multi-select project checklist cannot live in launcherForm's 4-line cycle
// shape. Only modelOptions/modeOptions/optLabel/cycle (launcher.go) and a
// fresh seed textinput are shared with launcherForm; everything else here —
// the checklist, its own cursor, the toggle set — is independent state.
//
// Focus zones (spec §2.1): 0 checklist, 1 model, 2 mode, 3 seed. tab/
// shift-tab cycle the 4 zones, wrapping. Within the checklist, ↓/↑ scroll it
// internally and space toggles the hovered project; on fields 1-3, ↓/↑ are
// no-ops (tab is the only field-nav there — one dialect per zone, spec-
// stated). space on the seed field TYPES a space (launcherForm precedent,
// tested).
type fanoutForm struct {
	projects []registry.Project
	checked  map[int]bool // project index -> selected
	listCur  int          // checklist cursor
	modelIdx int
	modeIdx  int
	seed     textinput.Model
	focus    int // 0=checklist 1=model 2=mode 3=seed

	// hint is a transient inline message shown INSIDE the still-open form
	// (spec §2.1: "↵ empty selection -> no-op with inline hint") — distinct
	// from App.fanHint, the dedicated persistent field that survives the
	// view transition to viewDash once a launch actually completes (spec
	// §2.3). This one lives only as long as the form itself and is cleared
	// the moment the selection changes.
	hint string
}

func newFanoutForm(projects []registry.Project) fanoutForm {
	ti := textinput.New()
	ti.Placeholder = "optional seed prompt or /slash-command"
	ti.CharLimit = 500
	return fanoutForm{projects: projects, checked: map[int]bool{}, seed: ti}
}

func (f *fanoutForm) setFocus(n int) {
	f.focus = n
	if n == 3 {
		f.seed.Focus()
	} else {
		f.seed.Blur()
	}
}

// selectedProjects returns the checked projects in checklist (registry)
// order — stable, independent of toggle order.
func (f *fanoutForm) selectedProjects() []registry.Project {
	var out []registry.Project
	for i, p := range f.projects {
		if f.checked[i] {
			out = append(out, p)
		}
	}
	return out
}

// recipeFor builds the recipe for project p using the form's SHARED
// model/mode/seed (spec §6: "uniform recipes — no per-project seed").
func (f *fanoutForm) recipeFor(p registry.Project) session.Recipe {
	return session.Recipe{
		ProjectLabel: p.Label,
		Cwd:          p.Path,
		Model:        modelOptions[f.modelIdx],
		Mode:         modeOptions[f.modeIdx],
		Seed:         f.seed.Value(),
	}
}

// update handles a form-focused key (spec §2.1, VERBATIM): tab/shift-tab
// cycle the 4 zones, wrapping; ↓/↑ scroll the checklist when it's focused
// and do nothing on fields 1-3; ←/→ cycle the model/mode value; space
// toggles the hovered checklist row when the checklist is focused, else (on
// the seed field) is forwarded to textinput, which types a space.
func (f *fanoutForm) update(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyTab:
		f.setFocus(cycle(f.focus, 1, 4))
		return nil
	case tea.KeyShiftTab:
		f.setFocus(cycle(f.focus, -1, 4))
		return nil
	case tea.KeyDown:
		if f.focus == 0 {
			if n := len(f.projects); n > 0 && f.listCur < n-1 {
				f.listCur++
			}
		}
		return nil
	case tea.KeyUp:
		if f.focus == 0 && f.listCur > 0 {
			f.listCur--
		}
		return nil
	case tea.KeyLeft, tea.KeyRight:
		d := 1
		if msg.Type == tea.KeyLeft {
			d = -1
		}
		switch f.focus {
		case 1:
			f.modelIdx = cycle(f.modelIdx, d, len(modelOptions))
		case 2:
			f.modeIdx = cycle(f.modeIdx, d, len(modeOptions))
		}
		return nil
	}
	if f.focus == 0 && msg.String() == " " {
		if len(f.projects) > 0 {
			f.checked[f.listCur] = !f.checked[f.listCur]
			f.hint = ""
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

// view renders the form body as plain lines (the caller wraps them in
// frame(), which supplies the title/keybar — same "no title/keybar here"
// discipline as launcherForm.view, see there for why).
func (f *fanoutForm) view(inner int) []string {
	var body []string
	marker := func(i int) string {
		if f.focus == i {
			return styCursor.Render("▸ ")
		}
		return "  "
	}

	body = append(body, "projects (space toggle):")
	if len(f.projects) == 0 {
		body = append(body, styHelp.Render("  (no projects found)"))
	}
	for i, p := range f.projects {
		box := "[ ]"
		if f.checked[i] {
			box = "[x]"
		}
		m := "  "
		if f.focus == 0 && f.listCur == i {
			m = styCursor.Render("▸ ")
		}
		body = append(body, truncPlain(m+box+" "+p.Label, inner))
	}
	body = append(body, "")

	sel := func(i int, label, val string) string {
		return fmt.Sprintf("%s%-9s ‹ %s ›", marker(i), label, val)
	}
	body = append(body, sel(1, "model", optLabel(modelOptions[f.modelIdx])))
	body = append(body, sel(2, "mode", optLabel(modeOptions[f.modeIdx])))
	body = append(body, marker(3)+"seed      "+f.seed.View())

	if f.hint != "" {
		body = append(body, "", styHelp.Render(truncPlain(f.hint, inner)))
	}
	return body
}
