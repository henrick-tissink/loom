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
	"github.com/henricktissink/loom/internal/delegate"
	"github.com/henricktissink/loom/internal/flashcards"
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
	if len(os.Args) > 1 && os.Args[1] == "flashcards" {
		if err := runFlashcards(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "loom:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "manifest" {
		if err := runManifest(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "loom:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loom:", err)
		os.Exit(1)
	}
}

// runFlashcards wires the flashcards CLI to config + store, using the real
// `claude` binary and the loom data dir as the child workdir.
func runFlashcards(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	return flashcards.RunCLI(args, st, "claude", cfg.LoomDir, time.Now().Unix(), os.Stdin, os.Stdout)
}

// runManifest implements `loom manifest validate <projectRoot>`: it loads every
// delegation manifest under <projectRoot>/.loom/manifests through the SAME loader
// the GUI's Start-run uses, and reports which load and which do not (with the
// exact reason). The orchestrator runs this to validate the manifest it just
// authored and self-correct before handing it off — so it never punts an
// "is this valid?" question to the human.
func runManifest(args []string) error {
	if len(args) < 2 || args[0] != "validate" {
		return fmt.Errorf("usage: loom manifest validate <projectRoot>")
	}
	root := args[1]
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	targets, err := projects.New(st).LaunchTargets()
	if err != nil {
		return fmt.Errorf("load projects: %w", err)
	}
	dir := delegate.ManifestDir(root)
	ms, errs := delegate.LoadAll(dir, delegate.NewResolver(targets))
	for _, m := range ms {
		fmt.Printf("ok   %s — %d task(s)\n", m.Name, len(m.Tasks))
		for _, w := range m.Warnings {
			if w.Task != "" {
				fmt.Printf("     warning [%s]: %s\n", w.Task, w.Text)
			} else {
				fmt.Printf("     warning: %s\n", w.Text)
			}
		}
	}
	for _, e := range errs {
		fmt.Printf("FAIL %s — %s\n", filepath.Base(e.Path), e.Err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d manifest(s) did not load in %s", len(errs), dir)
	}
	if len(ms) == 0 {
		fmt.Printf("no manifests found in %s\n", dir)
	}
	return nil
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
