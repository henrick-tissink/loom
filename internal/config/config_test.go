package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ClaudeConfigDir != filepath.Join(home, ".claude") {
		t.Errorf("ClaudeConfigDir = %q", c.ClaudeConfigDir)
	}
	if c.DBPath() != filepath.Join(home, ".loom", "loom.db") {
		t.Errorf("DBPath = %q", c.DBPath())
	}
	if c.ClaudeJSONPath() != filepath.Join(home, ".claude.json") {
		t.Errorf("ClaudeJSONPath = %q", c.ClaudeJSONPath())
	}
	if _, err := os.Stat(filepath.Join(home, ".loom")); err != nil {
		t.Errorf(".loom dir not created: %v", err)
	}
}

func TestLoadRespectsClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ClaudeConfigDir != "/custom/claude" {
		t.Errorf("ClaudeConfigDir = %q", c.ClaudeConfigDir)
	}
}

func TestInsideTmux(t *testing.T) {
	t.Setenv("TMUX", "/tmp/x,1,0")
	if !InsideTmux() {
		t.Error("expected true when TMUX set")
	}
	t.Setenv("TMUX", "")
	if InsideTmux() {
		t.Error("expected false when TMUX empty")
	}
}
