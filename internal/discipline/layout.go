package discipline

import (
	"os"
	"path/filepath"
	"strings"
)

// Layout is where the bundle lives on one machine. Everything is derived
// from the Claude config dir, the home dir and the hooks' state dir; see
// docs/DISCIPLINE.md §2 for the picture.
type Layout struct {
	ConfigDir string // $CLAUDE_CONFIG_DIR or ~/.claude
	Home      string
	StateDir  string // $CLAUDE_STATE_DIR or <ConfigDir>/state
}

// NewLayout resolves a layout. stateDir may be empty.
func NewLayout(configDir, home, stateDir string) Layout {
	if stateDir == "" {
		stateDir = filepath.Join(configDir, "state")
	}
	return Layout{ConfigDir: configDir, Home: home, StateDir: stateDir}
}

// StateSubdirs are the directories the hooks write under the state dir.
var StateSubdirs = []string{"tasks", "checkpoints", "ctxbudget", "testlog", "by-pid"}

func (l Layout) DisciplineDir() string    { return filepath.Join(l.ConfigDir, "discipline") }
func (l Layout) RevDir(rev string) string { return filepath.Join(l.DisciplineDir(), rev) }
func (l Layout) CurrentLink() string      { return filepath.Join(l.DisciplineDir(), "current") }
func (l Layout) HooksDir() string         { return filepath.Join(l.ConfigDir, "hooks", "enforce") }
func (l Layout) ScriptsDir() string       { return filepath.Join(l.ConfigDir, "scripts") }
func (l Layout) ContextBar() string       { return filepath.Join(l.ScriptsDir(), "context-bar.sh") }
func (l Layout) ClaudeMD() string         { return filepath.Join(l.ConfigDir, "CLAUDE.md") }
func (l Layout) SettingsPath() string     { return filepath.Join(l.ConfigDir, "settings.json") }
func (l Layout) LogDir() string           { return filepath.Join(l.StateDir, "log") }
func (l Layout) LegacyLogDir() string     { return l.HooksDir() }
func (l Layout) Deadline() string         { return filepath.Join(l.Home, ".local", "bin", "deadline") }

// Kind of install found on the machine.
const (
	LayoutRev    = "rev"    // discipline/current -> r<N>, hooks/enforce is a symlink into it
	LayoutLegacy = "legacy" // hooks/enforce is a real directory (the page's bootstrap layout)
	LayoutNone   = "none"
)

// Installed reports what is on the machine: the layout kind and the
// installed revision ("r3" from the REV file in the revision layout,
// "local" for a legacy install, "" for none).
func (l Layout) Installed() (kind, rev string) {
	if target, err := os.Readlink(l.CurrentLink()); err == nil {
		dir := target
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(l.DisciplineDir(), target)
		}
		if b, err := os.ReadFile(filepath.Join(dir, revName)); err == nil {
			if r := strings.TrimSpace(string(b)); r != "" {
				return LayoutRev, r
			}
		}
		return LayoutRev, "local"
	}
	if fi, err := os.Lstat(l.HooksDir()); err == nil && fi.IsDir() {
		if _, err := os.Stat(filepath.Join(l.HooksDir(), "stop_guard.py")); err == nil {
			return LayoutLegacy, "local"
		}
	}
	return LayoutNone, ""
}

// InstalledPath is where a bundle file is expected on this machine under
// the given layout kind. ok is false when the layout has no place for it
// (the legacy layout never had the examples, REV or the manifest).
func (l Layout) InstalledPath(f ManifestFile, kind string) (p string, ok bool) {
	if f.Target != "rev" {
		return l.expand(f.Target), true
	}
	switch kind {
	case LayoutRev:
		return filepath.Join(l.CurrentLink(), filepath.FromSlash(f.Path)), true
	case LayoutLegacy:
		switch Dir(f.Path) {
		case "enforce":
			return filepath.Join(l.HooksDir(), filepath.Base(f.Path)), true
		case "tests":
			return filepath.Join(l.HooksDir(), "tests", filepath.Base(f.Path)), true
		case "scripts":
			return filepath.Join(l.ScriptsDir(), filepath.Base(f.Path)), true
		}
		if f.Path == rulesName {
			return l.ClaudeMD(), true
		}
	}
	return "", false
}

// expand turns "~" and "<ccd>" prefixes into this layout's paths.
func (l Layout) expand(p string) string {
	switch {
	case p == "~":
		return l.Home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(l.Home, p[2:])
	case strings.HasPrefix(p, "<ccd>/"):
		return filepath.Join(l.ConfigDir, p[len("<ccd>/"):])
	}
	return p
}

// NormalizeCommand reduces a hook command string from settings.json to the
// script path it runs, so "~/.claude/hooks/enforce/x.py",
// "/Users/me/.claude/hooks/enforce/x.py", "$HOME/.claude/…" and
// "python3 -I <path>" all compare equal. Non-script commands come back
// trimmed but otherwise untouched.
func (l Layout) NormalizeCommand(cmd string) string {
	s := strings.TrimSpace(cmd)
	// Drop an interpreter prefix ("python3 -I", "/usr/bin/python3", "bash").
	if fields := strings.Fields(s); len(fields) > 1 {
		switch filepath.Base(fields[0]) {
		case "python3", "python", "bash", "sh", "zsh":
			rest := fields[1:]
			for len(rest) > 1 && strings.HasPrefix(rest[0], "-") {
				rest = rest[1:]
			}
			s = strings.Join(rest, " ")
		}
	}
	s = strings.Trim(s, `"'`)
	s = strings.ReplaceAll(s, "$CLAUDE_CONFIG_DIR", l.ConfigDir)
	s = strings.ReplaceAll(s, "${CLAUDE_CONFIG_DIR}", l.ConfigDir)
	s = strings.ReplaceAll(s, "$HOME", l.Home)
	s = strings.ReplaceAll(s, "${HOME}", l.Home)
	s = l.expand(s)
	if filepath.IsAbs(s) {
		s = filepath.Clean(s)
	}
	return s
}
