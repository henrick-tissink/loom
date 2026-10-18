package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/henricktissink/loom/internal/session"
)

// Settings are user preferences persisted to ~/.loom/settings.json. Kept small
// and additive: unknown/absent fields fall back to defaults so old files and
// new binaries interoperate.
type Settings struct {
	Editor        string `json:"editor"`        // "" = auto-detect; else "cursor" | "code" | "zed"
	Notifications bool   `json:"notifications"` // fire native needs-you notifications
	AutoSummarize bool   `json:"autoSummarize"` // background-summarize finished sessions (uses claude quota)
	TerminalTheme string `json:"terminalTheme"` // "dark" (default) | "light" | "system"; themes the chrome, the terminal and Claude
}

func defaultSettings() Settings {
	return Settings{Editor: "", Notifications: true, AutoSummarize: false, TerminalTheme: "dark"}
}

// settingsStore is a concurrency-safe, file-backed view of Settings.
type settingsStore struct {
	path string
	mu   sync.RWMutex
	cur  Settings
}

func newSettingsStore(loomDir string) *settingsStore {
	s := &settingsStore{path: filepath.Join(loomDir, "settings.json"), cur: defaultSettings()}
	s.load()
	return s
}

// load reads the file over a defaults base, so a partial or missing file still
// yields sane values (a missing "notifications" stays true, not false).
func (s *settingsStore) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	v := defaultSettings()
	if err := json.Unmarshal(b, &v); err != nil {
		return
	}
	s.mu.Lock()
	s.cur = normalize(v)
	s.mu.Unlock()
}

func (s *settingsStore) get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// set normalizes, updates the in-memory value, and persists atomically
// (write-temp-then-rename) so a crash mid-write can't corrupt the file.
func (s *settingsStore) set(v Settings) error {
	v = normalize(v)
	s.mu.Lock()
	s.cur = v
	s.mu.Unlock()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// normalize clamps the editor and terminal theme to known values.
func normalize(v Settings) Settings {
	switch v.Editor {
	case "cursor", "code", "zed":
	default:
		v.Editor = ""
	}
	switch v.TerminalTheme {
	case "light", "system":
	default:
		v.TerminalTheme = "dark"
	}
	return v
}

// GetPrefs returns the current settings (defaults if the store is unset, e.g.
// in tests).
func (a *App) GetPrefs() Settings {
	if a.settings == nil {
		return defaultSettings()
	}
	return a.settings.get()
}

// SetPrefs persists the given settings, applies the Claude theme so newly
// launched/resumed sessions match, and returns any write error.
func (a *App) SetPrefs(s Settings) error {
	if a.settings == nil {
		return nil
	}
	if err := a.settings.set(s); err != nil {
		return err
	}
	a.applyClaudeTheme()
	return nil
}

// applyClaudeTheme sets the Claude theme for new sessions from the setting.
// "system" resolves to what macOS shows now; the frontend corrects it live
// through SystemAppearance when the appearance changes.
func (a *App) applyClaudeTheme() {
	t := a.settings.get().TerminalTheme
	if t == "system" {
		t = systemAppearance()
	}
	session.SetClaudeTheme(t)
}

// SystemAppearance is called by the frontend whenever the macOS appearance
// resolves or changes ("dark" | "light"). It only matters under "system":
// a fixed theme ignores it.
func (a *App) SystemAppearance(mode string) {
	if a.settings == nil || a.settings.get().TerminalTheme != "system" {
		return
	}
	session.SetClaudeTheme(mode)
}

// systemAppearance reads the macOS appearance: AppleInterfaceStyle is "Dark"
// in dark mode and absent (the read fails) in light mode.
func systemAppearance() string {
	out, err := exec.Command("defaults", "read", "-g", "AppleInterfaceStyle").Output()
	if err == nil && strings.TrimSpace(string(out)) == "Dark" {
		return "dark"
	}
	return "light"
}
