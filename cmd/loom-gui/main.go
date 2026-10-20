// loom-gui — a native window on top of the loom engine.
package main

import (
	"context"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/henricktissink/loom/internal/brief"
	"github.com/henricktissink/loom/internal/ccsessions"
	"github.com/henricktissink/loom/internal/config"
	"github.com/henricktissink/loom/internal/diet"
	"github.com/henricktissink/loom/internal/diet/fromstore"
	"github.com/henricktissink/loom/internal/discipline"
	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/projects"
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

	// loom.db is the runtime source of truth for launch targets (§7), so a
	// failed workspace scan is no longer fatal: every project already
	// reconciled into the store stays launchable, and the sweep still runs.
	// Refusing to start because one directory became unreadable would be a
	// strictly worse outcome than starting with a stale target set.
	svc := projects.New(st)
	discovered, derr := registry.Discover(cfg.WorkspaceRoot, cfg.ClaudeConfigDir)
	if derr != nil {
		fmt.Fprintf(os.Stderr, "loom-gui: discover repos in %s: %v\n", cfg.WorkspaceRoot, derr)
	}
	if rerr := svc.Reconcile(discovered); rerr != nil {
		fmt.Fprintln(os.Stderr, "loom-gui: reconcile projects:", rerr)
	}
	launcher := &session.Launcher{
		Tmux: tm, Store: st,
		ClaudeConfigDir: cfg.ClaudeConfigDir,
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
	// claude's background sessions, including the ones its supervisor has
	// stopped, at most once per 5 s (ccsessions.AgentsCache).
	engine.SetAgents((&ccsessions.AgentsCache{Run: ccsessions.RunAgents, Every: 5 * time.Second}).List)
	app := newApp(engine, tm, st, launcher, svc, time.Now)
	app.settings = newSettingsStore(cfg.LoomDir)
	app.usage = newUsageState(cfg.ClaudeConfigDir, cfg.LoomDir, time.Now)
	if home, err := os.UserHomeDir(); err == nil {
		l := discipline.NewLayout(cfg.ClaudeConfigDir, home, os.Getenv("CLAUDE_STATE_DIR"))
		app.disc = newDisciplineState(l, time.Now)
		app.disc.diet = func(ctx context.Context) *diet.Report {
			in := diet.Inputs{ConfigDir: l.ConfigDir, ClaudeJSON: diet.ClaudeJSONPath(l.ConfigDir, l.Home), Now: time.Now(),
				Run: func(ctx context.Context, name string, args ...string) (string, error) {
					out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
					return string(out), err
				}}
			fromstore.Fill(&in, st)
			rep := diet.Build(ctx, in)
			return &rep
		}
		app.hooks = newHookState(l)
		app.rules = newRulesState(l)
	}
	app.applyClaudeTheme() // match Claude's theme to the terminal
	app.summarizer = &memory.Summarizer{Store: st, Binary: "claude", WorkDir: cfg.LoomDir}
	app.briefs = newBriefState(&brief.Service{
		Store: st, ClaudeConfigDir: cfg.ClaudeConfigDir,
		Archive: brief.Archive{Dir: filepath.Join(cfg.LoomDir, "briefs")},
		Writer:  brief.Writer{Binary: "claude", WorkDir: cfg.LoomDir},
	})

	return wails.Run(&options.App{
		Title:       "loom",
		Width:       1180,
		Height:      760,
		MinWidth:    760,
		MinHeight:   480,
		OnStartup:   func(ctx context.Context) { app.startup(ctx) },
		Bind:        []interface{}{app},
		AssetServer: &assetserver.Options{Assets: assets},
		// A transparent webview over a translucent window: the ground (the
		// strips between the islands) lets the desktop's vibrancy through,
		// tinted by tokens.css; the islands themselves stay opaque.
		BackgroundColour: &options.RGBA{R: 0, G: 0, B: 0, A: 0},
		Mac: &mac.Options{
			TitleBar:             mac.TitleBarHiddenInset(),
			WebviewIsTransparent: true,
			WindowIsTranslucent:  true,
		},
	})
}
