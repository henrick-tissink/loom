// Package session defines the launch recipe (the Phase-3 "workflow step" shape)
// and orchestrated session launching.
package session

import (
	"encoding/json"
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
	// AddDirs are the other repos of a scoped multi-repo launch (spec §5):
	// Cwd is the primary repo, AddDirs the siblings claude is granted via
	// --add-dir. Empty for a root launch or a single-repo launch.
	AddDirs []string
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
	// --add-dir last, one flag per directory: claude takes a single path per
	// occurrence, and appending after the existing flags keeps every argv the
	// pre-multi-repo tests pinned byte-identical (spec §5).
	argv = append(argv, addDirArgs(r.AddDirs)...)
	return argv
}

func addDirArgs(dirs []string) []string {
	out := make([]string, 0, len(dirs)*2)
	for _, d := range dirs {
		if d == "" {
			continue
		}
		out = append(out, "--add-dir", d)
	}
	return out
}

// EncodeAddDirs / DecodeAddDirs are the only place the store's add_dirs JSON
// string is produced or read. The store deliberately keeps the column as a raw
// string so SessionRow stays comparable, which makes the encoding this
// package's invariant, not the store's.
func EncodeAddDirs(dirs []string) string {
	if len(dirs) == 0 {
		return "" // '' not "[]": the migration-v8 default for "no add-dirs"
	}
	b, err := json.Marshal(dirs)
	if err != nil {
		return "" // unreachable for []string; degrade to "no add-dirs" rather than crash a launch
	}
	return string(b)
}

// DecodeAddDirs returns nil for anything it cannot parse. A corrupt row must
// resume as a single-repo session rather than block the resume entirely —
// losing a sibling repo is recoverable, a session you cannot restart is not.
func DecodeAddDirs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var dirs []string
	if err := json.Unmarshal([]byte(s), &dirs); err != nil {
		return nil
	}
	return dirs
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

// ResumeShellCommand takes addDirs because --resume does NOT restore the
// add-dirs of the original launch: without re-passing them the session comes
// back seeing only Cwd, and nothing surfaces that until a write to a sibling
// repo is refused mid-turn (spec §5).
func ResumeShellCommand(claudeSessionID string, addDirs []string) string {
	argv := []string{"claude", "--resume", claudeSessionID, "--settings", currentThemeSetting()}
	return shellQuote(append(argv, addDirArgs(addDirs)...))
}
