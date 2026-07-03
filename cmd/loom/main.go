// Loom — a terminal control center for claude sessions.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/henricktissink/loom/internal/config"
	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/ui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loom:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := config.CheckBinaries(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	tm := tmux.New()
	if err := tm.EnsureServer(); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	projects, err := registry.Discover(cfg.WorkspaceRoot, cfg.ClaudeConfigDir)
	if err != nil {
		return fmt.Errorf("discover projects in %s: %w", cfg.WorkspaceRoot, err)
	}

	ix := memory.NewIndexer(st, cfg.ClaudeConfigDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ix.Run(ctx, 10*time.Minute)

	deps := ui.Deps{
		Engine: status.NewEngine(tm, st, cfg.ClaudeConfigDir),
		Launcher: &session.Launcher{
			Tmux: tm, Store: st,
			ClaudeConfigDir: cfg.ClaudeConfigDir,
			ClaudeJSONPath:  cfg.ClaudeJSONPath(),
			ReadyMarker:     session.DefaultReadyMarker,
			TrustMarker:     session.DefaultTrustMarker,
			ReadyTimeout:    60 * time.Second,
			PollEvery:       500 * time.Millisecond,
		},
		Projects:      projects,
		Tmux:          tm,
		InsideTmux:    config.InsideTmux(),
		Store:         st,
		IndexerStatus: ix.Status,
		Summarizer: &memory.Summarizer{
			Store:   st,
			Binary:  "claude",
			WorkDir: cfg.LoomDir,
		},
	}
	p := tea.NewProgram(ui.NewApp(deps), tea.WithAltScreen())
	_, err = p.Run()
	return err
}
