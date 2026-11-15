package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

// A capture of claude 2.1.325's plan dialog (trimmed), steps numbered too.
const planPane = `  ⎿  /plan to preview
  ──────────────────────────────────────────────
   Ready to code?
   Here is Claude's plan:
  ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
   1. Write the task file (Outcome: hello.txt with "hi").
   2. Create hello.txt in the repo root containing hi.
   3. Verify, tick the check, and set Status: done.
  ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
  ──────────────────────────────────────────────
   Claude has written up a plan and is ready to execute. Would you like to proceed?
   ❯ 1. Yes, and use auto mode
     2. Yes, manually approve edits
     3. Tell Claude what to change
        shift+tab to approve with this feedback
   ctrl+g to edit in Vim · ~/.claude/plans/plan-how-to-add-fizzy-emerson.md
`

func TestParsePlanDialog(t *testing.T) {
	d, ok := parsePlanDialog(planPane)
	if !ok || d.file != "~/.claude/plans/plan-how-to-add-fizzy-emerson.md" || len(d.options) != 3 {
		t.Fatalf("parse = %+v, %v", d, ok)
	}
	if d.options[0] != (PlanOption{N: 1, Label: "Yes, and use auto mode"}) || d.options[1].Input || !d.options[2].Input {
		t.Fatalf("options = %+v", d.options)
	}
	// a permission dialog, the plan steps alone, no cursor: not a plan dialog
	for _, p := range []string{
		"────\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n Esc to cancel\n",
		strings.Split(planPane, "  ──────────────────────────────────────────────\n   Claude has")[0],
		strings.ReplaceAll(planPane, "❯", " "),
	} {
		if _, ok := parsePlanDialog(p); ok {
			t.Fatalf("parsed as a plan dialog:\n%s", p)
		}
	}
}

func TestPlanCardAnswers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plans := filepath.Join(home, ".claude", "plans")
	os.MkdirAll(plans, 0o755)
	os.WriteFile(filepath.Join(plans, "plan-how-to-add-fizzy-emerson.md"), []byte("1. Do it.\n"), 0o644)

	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tm := &tmux.Client{Socket: fmt.Sprintf("loomtest-plan%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	a := newApp(nil, tm, st, nil, nil, time.Now)
	a.planDir = filepath.Join(t.TempDir(), "plans")
	name := "loom-55555555"
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: "sid-5", Cwd: home, CreatedAt: 1, EndedAt: -1, LastStatus: "needs_you"})
	// a pane that draws the dialog; a digit redraws it with the cursor on
	// that option (as claude does), then it echoes what it is sent
	dir := t.TempDir()
	draw := filepath.Join(dir, "pane.txt")
	os.WriteFile(draw, []byte(planPane), 0o644)
	script := filepath.Join(dir, "dialog.sh")
	os.WriteFile(script, []byte(`#!/bin/bash
cat "$1"
IFS= read -rsn1 k
clear
sed -e 's/❯ 1\./  1./' -e "s/^\(  *\)$k\./\1❯ $k./" "$1"
stty -echo 2>/dev/null; cat
`), 0o755)
	if err := tm.NewSession(name, home, script+" "+draw, 150, 40); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	for end := time.Now().Add(3 * time.Second); !a.planReady(name); {
		if time.Now().After(end) {
			out, _ := tm.CapturePane(name)
			t.Fatalf("dialog not seen:\n%s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	c, err := a.PlanCard(name)
	if err != nil || c.Plan != "1. Do it.\n" || len(c.Options) != 3 || c.PlanErr != "" {
		t.Fatalf("card = %+v, %v", c, err)
	}
	if err := a.AnswerPlan(name, 2, "Yes, and use auto mode", ""); err == nil {
		t.Fatal("a label that does not match must be refused")
	}
	if err := a.AnswerPlan(name, 3, "Tell Claude what to change", ""); err == nil {
		t.Fatal("feedback option without text must be refused")
	}
	if err := a.AnswerPlan(name, 1, "Yes, and use auto mode", "x"); err == nil {
		t.Fatal("approve with feedback must be refused")
	}
	if err := a.AnswerPlan(name, 3, "Tell Claude what to change", "add a\nREADME line"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	out, _ := tm.CapturePane(name)
	if d, _ := parsePlanDialog(out); d.cursor != 3 || !strings.Contains(out, "add a README line") {
		t.Fatalf("feedback not typed into the opened field:\n%s", out)
	}
	if err := a.AnswerPlan(name, 2, "Yes, manually approve edits", ""); err != nil {
		t.Fatal(err)
	}
	if got := a.approvedPlan("sid-5"); got != "1. Do it.\n" {
		t.Fatalf("approved plan = %q", got)
	}
}

func TestReadPlanFileStaysInPlans(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.WriteFile(filepath.Join(home, "secret.md"), []byte("x"), 0o644)
	a := &App{}
	for _, p := range []string{"~/secret.md", "~/.claude/plans/../../secret.md", "/etc/x/plans/a.md"} {
		if _, err := a.readPlanFile(p); err == nil {
			t.Fatalf("%s read", p)
		}
	}
}

// Text is never typed while the cursor stays off the feedback option: in
// claude, it would land in the option list, where a digit submits.
func TestPlanFeedbackWaitsForTheField(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tm := &tmux.Client{Socket: fmt.Sprintf("loomtest-planw%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	a := newApp(nil, tm, st, nil, nil, time.Now)
	name := "loom-66666666"
	draw := filepath.Join(t.TempDir(), "pane.txt")
	os.WriteFile(draw, []byte(planPane), 0o644)
	// a pane that never moves its cursor, and swallows keys unechoed
	if err := tm.NewSession(name, t.TempDir(), "cat "+draw+"; stty -echo; cat > /dev/null", 150, 40); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(3 * time.Second); !a.planReady(name); {
		if time.Now().After(end) {
			t.Fatal("dialog not seen")
		}
		time.Sleep(100 * time.Millisecond)
	}
	err = a.AnswerPlan(name, 3, "Tell Claude what to change", "1 more step")
	if err == nil || !strings.Contains(err.Error(), "nothing was typed") {
		t.Fatalf("typed without the field open: %v", err)
	}
}
