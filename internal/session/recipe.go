// Package session defines the launch recipe (the Phase-3 "workflow step" shape)
// and orchestrated session launching.
package session

import (
	"strings"

	"github.com/google/uuid"
)

// Recipe is everything needed to launch a configured claude session.
// In Phase 3 a saved workflow step IS this struct plus a context relation.
type Recipe struct {
	ProjectLabel string
	Cwd          string
	Model        string // "", "opus", "sonnet", "fable"
	Mode         string // "", "plan", "acceptEdits", "auto", "bypassPermissions"
	Seed         string // optional initial prompt or /slash-command
}

func NewSessionID() string { return uuid.NewString() }

const tmuxPrefix = "loom-"

// TmuxName builds the tmux-safe session name. NEVER embed the project label:
// '.'/':' break tmux -t targeting (spec §4.2).
func TmuxName(sessionID string) string { return tmuxPrefix + sessionID }

func SessionIDFromTmuxName(name string) (string, bool) {
	if !strings.HasPrefix(name, tmuxPrefix) {
		return "", false
	}
	return strings.TrimPrefix(name, tmuxPrefix), true
}

func (r Recipe) Argv(sessionID string) []string {
	argv := []string{"claude", "--session-id", sessionID}
	if r.Model != "" {
		argv = append(argv, "--model", r.Model)
	}
	if r.Mode != "" {
		argv = append(argv, "--permission-mode", r.Mode)
	}
	return argv
}

func shellQuote(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

func (r Recipe) ShellCommand(sessionID string) string {
	return shellQuote(r.Argv(sessionID))
}

func ResumeShellCommand(claudeSessionID string) string {
	return shellQuote([]string{"claude", "--resume", claudeSessionID})
}
