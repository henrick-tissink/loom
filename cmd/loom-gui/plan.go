package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The plan card answers claude's own plan-approval dialog from the rail.
// Nothing here invents a parallel plan system: the plan is the file the
// dialog names (~/.claude/plans/<name>.md) and the choices are the dialog's
// own options, read from the pane each time. While the dialog is open the
// pending ExitPlanMode call is not in the transcript yet (verified on
// Claude Code 2.1.325), so the pane is the only witness.
//
// The dialog, as drawn on 2.1.325 (the plan's own steps above it are
// numbered too, which is why options are read only below the last rule):
//
//	──────────────────────────────────────────
//	 Claude has written up a plan and is ready to execute. Would you like to proceed?
//	 ❯ 1. Yes, and use auto mode
//	   2. Yes, manually approve edits
//	   3. Tell Claude what to change
//	      shift+tab to approve with this feedback
//	 ctrl+g to edit in Vim · ~/.claude/plans/plan-how-to-add-fizzy-emerson.md
//
// A digit on a plain option submits it; a digit on the feedback option
// opens its text field, which literal text fills and Enter sends.

// PlanOption is one choice of the dialog.
type PlanOption struct {
	N     int    `json:"n"`
	Label string `json:"label"`
	Input bool   `json:"input"` // takes feedback text ("Tell Claude what to change")
}

type planDialog struct {
	options []PlanOption
	file    string // as the pane shows it, maybe ~-relative
	cursor  int    // the option under ❯
}

var (
	planOptRe  = regexp.MustCompile(`^\s*(❯\s*)?(\d+)\.\s+(.+?)\s*$`)
	planFileRe = regexp.MustCompile(`·\s*(\S*/plans/[^\s/]+\.md)\s*$`)
)

// parsePlanDialog finds claude's plan dialog in a pane capture: a plan file
// under plans/ named on the hint line, and two or more numbered options,
// one under the cursor, below the last horizontal rule.
func parsePlanDialog(pane string) (planDialog, bool) {
	lines := strings.Split(pane, "\n")
	fileAt := -1
	var d planDialog
	for i := len(lines) - 1; i >= 0; i-- {
		if m := planFileRe.FindStringSubmatch(lines[i]); m != nil {
			d.file, fileAt = m[1], i
			break
		}
	}
	if fileAt < 0 {
		return d, false
	}
	rule := -1
	for i := fileAt - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" && strings.Trim(t, "─") == "" {
			rule = i
			break
		}
	}
	if rule < 0 {
		return d, false
	}
	cursor := false
	for i := rule + 1; i < fileAt; i++ {
		m := planOptRe.FindStringSubmatch(lines[i])
		if m == nil {
			// a hint under an option ("shift+tab to approve with this
			// feedback") marks that option as the one taking text
			if n := len(d.options); n > 0 && strings.Contains(lines[i], "feedback") {
				d.options[n-1].Input = true
			}
			continue
		}
		n, _ := strconv.Atoi(m[2])
		if n != len(d.options)+1 {
			return d, false // not one run of 1, 2, 3…
		}
		if m[1] != "" {
			cursor, d.cursor = true, n
		}
		label := m[3]
		d.options = append(d.options, PlanOption{N: n, Label: label,
			Input: strings.Contains(strings.ToLower(label), "what to change")})
	}
	return d, cursor && len(d.options) >= 2
}

// planReady reports whether a session's pane shows the plan dialog.
func (a *App) planReady(name string) bool {
	if a.tm == nil {
		return false
	}
	out, err := a.tm.CapturePaneJoined(name)
	if err != nil {
		return false
	}
	_, ok := parsePlanDialog(out)
	return ok
}

// planDialogOf reads the dialog off a live session's pane now.
func (a *App) planDialogOf(name string) (planDialog, error) {
	if a.tm == nil {
		return planDialog{}, errors.New("tmux unavailable")
	}
	out, err := a.tm.CapturePaneJoined(name)
	if err != nil {
		return planDialog{}, err
	}
	d, ok := parsePlanDialog(out)
	if !ok {
		return d, errors.New("the plan dialog is no longer open")
	}
	return d, nil
}

// readPlanFile reads the plan the dialog names, only from a plans directory
// under the home or the claude config directory.
func (a *App) readPlanFile(shown string) (string, error) {
	home, _ := os.UserHomeDir()
	p := shown
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, p[2:])
	}
	p = filepath.Clean(p)
	dirs := []string{filepath.Join(home, ".claude", "plans")}
	if a.launcher != nil && a.launcher.ClaudeConfigDir != "" {
		dirs = append(dirs, filepath.Join(a.launcher.ClaudeConfigDir, "plans"))
	}
	ok := false
	for _, d := range dirs {
		if filepath.Dir(p) == filepath.Clean(d) {
			ok = true
		}
	}
	if !ok {
		return "", fmt.Errorf("%s is not in a claude plans directory", shown)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if fi.Size() > 512<<10 {
		return "", fmt.Errorf("%s is too large to show", shown)
	}
	b, err := os.ReadFile(p)
	return string(b), err
}

// PlanCardDTO is what the plan card shows.
type PlanCardDTO struct {
	Plan     string       `json:"plan"`
	PlanFile string       `json:"planFile"`
	PlanErr  string       `json:"planErr,omitempty"`
	Options  []PlanOption `json:"options"`
}

// PlanCard reads a waiting session's plan dialog and its plan.
func (a *App) PlanCard(name string) (PlanCardDTO, error) {
	d, err := a.planDialogOf(name)
	if err != nil {
		return PlanCardDTO{}, err
	}
	c := PlanCardDTO{PlanFile: d.file, Options: d.options}
	if c.Plan, err = a.readPlanFile(d.file); err != nil {
		c.PlanErr = err.Error()
	}
	return c, nil
}

// AnswerPlan picks option n of the plan dialog, which must still carry the
// label the card showed: the pane is read again first, so a dialog that
// changed or closed is never answered blind. The feedback option takes
// text (one line: Enter sends it); the others take none. Approving saves
// the plan for the review sheet.
func (a *App) AnswerPlan(name string, n int, label, feedback string) error {
	d, err := a.planDialogOf(name)
	if err != nil {
		return err
	}
	var opt *PlanOption
	for i := range d.options {
		if d.options[i].N == n {
			opt = &d.options[i]
		}
	}
	if opt == nil || opt.Label != label {
		return errors.New("the plan dialog changed; look again")
	}
	feedback = strings.Join(strings.Fields(feedback), " ")
	if opt.Input {
		if feedback == "" {
			return errors.New("write what Claude should change")
		}
		if err := a.tm.SendKeys(name, strconv.Itoa(n)); err != nil {
			return err
		}
		// Type only once the cursor is on the feedback option: text that
		// reached the option list instead would select, and a digit in it
		// would submit, a plain option.
		if !a.waitPlanCursor(name, n, 2*time.Second) {
			return errors.New("the feedback field did not open; nothing was typed")
		}
		if err := a.tm.SendLiteral(name, feedback); err != nil {
			return err
		}
		return a.tm.SendEnter(name)
	}
	if feedback != "" {
		return fmt.Errorf("%q takes no feedback", label)
	}
	plan, planErr := a.readPlanFile(d.file)
	if err := a.tm.SendKeys(name, strconv.Itoa(n)); err != nil {
		return err
	}
	if planErr == nil && strings.HasPrefix(strings.ToLower(label), "yes") {
		_ = a.savePlan(name, plan) // best-effort: the answer matters more
	}
	return nil
}

// waitPlanCursor waits for the plan dialog's cursor to sit on option n.
func (a *App) waitPlanCursor(name string, n int, within time.Duration) bool {
	end := time.Now().Add(within)
	for {
		if d, err := a.planDialogOf(name); err == nil && d.cursor == n {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// savePlan keeps an approved plan as ~/.loom/plans/<claude session id>.md.
func (a *App) savePlan(name, plan string) error {
	if a.planDir == "" || a.st == nil {
		return errors.New("no plan directory")
	}
	row, ok, err := a.st.Get(name)
	if err != nil || !ok || row.ClaudeSessionID == "" {
		return fmt.Errorf("no session id for %s", name)
	}
	if err := os.MkdirAll(a.planDir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(a.planDir, row.ClaudeSessionID+".md")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(plan), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// approvedPlan is the plan approved in a session, or "".
func (a *App) approvedPlan(sid string) string {
	if a.planDir == "" || sid == "" || strings.ContainsAny(sid, `/\`) {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(a.planDir, sid+".md"))
	if err != nil {
		return ""
	}
	return string(b)
}
