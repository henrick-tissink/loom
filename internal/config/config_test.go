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

func TestLoadRespectsLoomWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOOM_WORKSPACE", "/custom/workspace")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkspaceRoot != "/custom/workspace" {
		t.Errorf("WorkspaceRoot = %q", c.WorkspaceRoot)
	}
}

func TestLoadWorkspaceDefaultsToSauce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOOM_WORKSPACE", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkspaceRoot != filepath.Join(home, "Sauce") {
		t.Errorf("WorkspaceRoot = %q", c.WorkspaceRoot)
	}
}
