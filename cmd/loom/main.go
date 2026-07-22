// Loom — a terminal control center for claude sessions.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/henricktissink/loom/internal/config"
	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/ui"
	"github.com/henricktissink/loom/internal/workflow"
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

	// loom.db is the runtime source of truth for launch targets (§7), so a
	// failed workspace scan is no longer fatal — it used to abort startup.
	// Everything already reconciled into the store stays launchable, which is
	// strictly better than refusing to start because one directory became
	// unreadable. This matches cmd/loom-gui/main.go; both binaries reconcile
	// into the same DB, and reconciliation never clobbers user-set state.
	projectSvc := projects.New(st)
	discovered, derr := registry.Discover(cfg.WorkspaceRoot, cfg.ClaudeConfigDir)
	if derr != nil {
		fmt.Fprintf(os.Stderr, "loom: discover repos in %s: %v\n", cfg.WorkspaceRoot, derr)
	}
	if rerr := projectSvc.Reconcile(discovered); rerr != nil {
		fmt.Fprintln(os.Stderr, "loom: reconcile projects:", rerr)
	}
	// The TUI still consumes a flat repo list, but it now comes from the DB's
	// target set rather than the scan: a project created in the GUI is a real
	// launch target here, and registry.Discover alone cannot conjure one.
	repos := launchRepos(projectSvc)

	ix := memory.NewIndexer(st, cfg.ClaudeConfigDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ix.Run(ctx, 10*time.Minute)

	launcher := &session.Launcher{
		Tmux: tm, Store: st,
		ClaudeConfigDir: cfg.ClaudeConfigDir,
		ClaudeJSONPath:  cfg.ClaudeJSONPath(),
		ReadyMarker:     session.DefaultReadyMarker,
		TrustMarker:     session.DefaultTrustMarker,
		ReadyTimeout:    60 * time.Second,
		PollEvery:       500 * time.Millisecond,
	}

	workflowsDir := filepath.Join(cfg.LoomDir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", workflowsDir, err)
	}

	deps := ui.Deps{
		Engine:        status.NewEngine(tm, st, cfg.ClaudeConfigDir),
		Launcher:      launcher,
		Repos:         repos,
		Tmux:          tm,
		InsideTmux:    config.InsideTmux(),
		Store:         st,
		IndexerStatus: ix.Status,
		Summarizer: &memory.Summarizer{
			Store:   st,
			Binary:  "claude",
			WorkDir: cfg.LoomDir,
		},
		Runner: &workflow.Runner{
			Store:           st,
			Launcher:        launcher,
			ClaudeConfigDir: cfg.ClaudeConfigDir,
		},
		WorkflowsDir: workflowsDir,
		Projects:     projectSvc,
	}
	p := tea.NewProgram(ui.NewApp(deps), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// launchRepos flattens the project service's target set into the flat repo
// list the TUI's launcher, fan-out checklist and workflow loader validate
// against. Target.Label is used verbatim: it is the stable directory-derived
// identifier saved workflow definitions resolve through (§2), never the
// user-editable project name.
//
// Missing targets are dropped here rather than dimmed — the TUI has no
// non-launchable row, and offering a path that no longer exists ends in tmux
// silently starting the session in $HOME (§12).
//
// A failure degrades to an empty list rather than aborting startup: the rail,
// search and history all still work without a launch surface.
func launchRepos(svc *projects.Service) []registry.Repo {
	ts, err := svc.LaunchTargets()
	if err != nil {
		fmt.Fprintln(os.Stderr, "loom: launch targets:", err)
		return nil
	}
	out := make([]registry.Repo, 0, len(ts))
	for _, t := range ts {
		if t.Missing {
			continue
		}
		out = append(out, registry.Repo{Label: t.Label, Path: t.Path})
	}
	return out
}
