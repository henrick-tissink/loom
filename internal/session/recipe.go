// Package session defines the launch recipe (the Phase-3 "workflow step" shape)
// and orchestrated session launching.
package session

import (
	"strings"
	"sync"

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

// Claude's TUI must match loom's embedded terminal theme, or its 256-color
// text (which xterm.js can't remap) is illegible — near-white text on the
// light Blush terminal, or dark text on a dark terminal. We inject the
// matching theme via --settings (merged over the user's global config, not
// mutating it). Default light; the GUI calls SetClaudeTheme when the user
// picks a dark terminal. Guarded so a launch reading the setting can't race a
// pref change.
var (
	themeMu      sync.RWMutex
	themeSetting = `{"theme":"light"}`
)

// SetClaudeTheme sets the theme injected into launched and resumed sessions.
// "dark" selects the dark theme; anything else selects light.
func SetClaudeTheme(theme string) {
	themeMu.Lock()
	defer themeMu.Unlock()
	if theme == "dark" {
		themeSetting = `{"theme":"dark"}`
	} else {
		themeSetting = `{"theme":"light"}`
	}
}

func currentThemeSetting() string {
	themeMu.RLock()
	defer themeMu.RUnlock()
	return themeSetting
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
	argv := []string{"claude", "--session-id", sessionID, "--settings", currentThemeSetting()}
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
	return shellQuote([]string{"claude", "--resume", claudeSessionID, "--settings", currentThemeSetting()})
}
