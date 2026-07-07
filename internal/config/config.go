// Package config resolves Loom's filesystem paths and environment.
package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type Config struct {
	ClaudeConfigDir string // $CLAUDE_CONFIG_DIR or $HOME/.claude
	LoomDir         string // $HOME/.loom
	WorkspaceRoot   string // $LOOM_WORKSPACE or $HOME/Sauce
	Home            string
}

func Load() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}
	ccd := os.Getenv("CLAUDE_CONFIG_DIR")
	if ccd == "" {
		ccd = filepath.Join(home, ".claude")
	}
	ws := os.Getenv("LOOM_WORKSPACE")
	if ws == "" {
		ws = filepath.Join(home, "Sauce")
	}
	c := &Config{
		ClaudeConfigDir: ccd,
		LoomDir:         filepath.Join(home, ".loom"),
		WorkspaceRoot:   ws,
		Home:            home,
	}
	if err := os.MkdirAll(c.LoomDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", c.LoomDir, err)
	}
	return c, nil
}

func (c *Config) DBPath() string         { return filepath.Join(c.LoomDir, "loom.db") }
func (c *Config) ClaudeJSONPath() string { return filepath.Join(c.Home, ".claude.json") }

// CheckBinaries verifies tmux and claude are on PATH.
func CheckBinaries() error {
	for _, bin := range []string{"tmux", "claude"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found on PATH — Loom requires it (install and retry)", bin)
		}
	}
	return nil
}

func InsideTmux() bool { return os.Getenv("TMUX") != "" }
