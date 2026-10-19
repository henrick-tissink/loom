package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/henricktissink/loom/internal/taskfile"
	"github.com/henricktissink/loom/internal/transcript"
)

// GoalDTO is a session's /goal as its transcript records it.
type GoalDTO struct {
	State     string `json:"state"` // active | paused | met | cleared
	Condition string `json:"condition"`
	Checks    int    `json:"checks"` // evaluator checks since it was set
	Reason    string `json:"reason"` // the last check's reason, or the pause notice
	At        int64  `json:"at"`
}

func goalDTO(g transcript.Goal) *GoalDTO {
	if g.State == "" {
		return nil
	}
	return &GoalDTO{State: g.State, Condition: g.Condition, Checks: g.Checks, Reason: g.Reason, At: g.At}
}

// GoalPrompt composes a /goal for a live session from its task file: the
// Done-when checks verbatim plus a turn cap. Nothing is sent: the operator
// reads and edits it first, then SendPrompt delivers it, which is the
// explicit ask a goal loop needs.
func (a *App) GoalPrompt(name string) (string, error) {
	if a.st == nil || a.launcher == nil {
		return "", fmt.Errorf("store unavailable")
	}
	row, ok, err := a.st.Get(name)
	if err != nil || !ok {
		return "", fmt.Errorf("no session %q", name)
	}
	if row.ClaudeSessionID == "" {
		return "", fmt.Errorf("the session has no claude session id yet")
	}
	path := filepath.Join(taskfile.Dir(a.launcher.ClaudeConfigDir), "tasks", row.ClaudeSessionID+".md")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the session has no task file yet (%s)", path)
	}
	t, ok := taskfile.ParseTask(string(b))
	switch {
	case !ok || len(t.Checks) == 0:
		return "", fmt.Errorf("the task file has no Done-when checks to aim at")
	case t.Status != "open":
		return "", fmt.Errorf("the task file says Status: %s", t.Status)
	}
	cond, ok := taskfile.GoalCondition(t, path, taskfile.GoalTurns)
	if !ok {
		return "", fmt.Errorf("the task file has too many checks for one /goal (claude reads at most %d characters as a command)", taskfile.GoalMaxLen)
	}
	return "/goal " + cond, nil
}
