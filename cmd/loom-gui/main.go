// loom-gui — a native window on top of the loom engine.
package main

import (
	"context"
	"embed"
	"fmt"
	"os"
	"time"

	"github.com/henricktissink/loom/internal/config"
	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loom-gui:", err)
		os.Exit(1)
	}
}

func run() error {
	// GUI apps launched from Finder/Dock get a minimal PATH; make tmux/claude
	// resolve the same as they do in a terminal before we check for them.
	hydratePATH()
	hydrateLocale()
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
	launcher := &session.Launcher{
		Tmux: tm, Store: st,
		ClaudeConfigDir: cfg.ClaudeConfigDir,
		ClaudeJSONPath:  cfg.ClaudeJSONPath(),
		ReadyMarker:     session.DefaultReadyMarker,
		TrustMarker:     session.DefaultTrustMarker,
		ReadyTimeout:    60 * time.Second,
		PollEvery:       500 * time.Millisecond,
	}
	// Keep the memory index fresh (search + summaries read it): sweep at
	// startup and every 10 min, like the TUI. Without this a GUI-only user's
	// index goes stale and freshly-finished sessions can't be summarized.
	ix := memory.NewIndexer(st, cfg.ClaudeConfigDir)
	ixCtx, cancelIx := context.WithCancel(context.Background())
	defer cancelIx()
	go ix.Run(ixCtx, 10*time.Minute)

	engine := status.NewEngine(tm, st, cfg.ClaudeConfigDir)
	app := newApp(engine, tm, st, launcher, projects, time.Now)
	app.settings = newSettingsStore(cfg.LoomDir)
	app.summarizer = &memory.Summarizer{Store: st, Binary: "claude", WorkDir: cfg.LoomDir}

	return wails.Run(&options.App{
		Title:       "loom",
		Width:       1180,
		Height:      760,
		MinWidth:    760,
		MinHeight:   480,
		OnStartup:   func(ctx context.Context) { app.startup(ctx) },
		Bind:        []interface{}{app},
		AssetServer: &assetserver.Options{Assets: assets},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarHiddenInset(),
		},
	})
}
