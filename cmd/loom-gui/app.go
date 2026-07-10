package main

import (
	"context"
	"os/exec"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/tmux"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Go↔JS bridge. It owns no orchestration logic beyond the PTY
// registry; session state comes from the shared engine.
type App struct {
	ctx    context.Context
	engine *status.Engine
	tm     *tmux.Client
	now    func() time.Time
	reg    *ptyRegistry
}

func newApp(engine *status.Engine, tm *tmux.Client, now func() time.Time) *App {
	a := &App{engine: engine, tm: tm, now: now}
	// Until startup() wires the real emitter, events go nowhere (safe for tests).
	a.reg = newPTYRegistry(
		func(name string) *exec.Cmd { return tm.AttachCmd(name) },
		func(event string, data ...any) {
			if a.ctx != nil {
				wruntime.EventsEmit(a.ctx, event, data...)
			}
		},
	)
	return a
}

// startup is called by Wails with the app context once the window is ready.
func (a *App) startup(ctx context.Context) { a.ctx = ctx }

// ListSessions polls the engine and returns the live sessions as DTOs.
// Any error (or panic from a half-built engine) degrades to an empty list.
func (a *App) ListSessions() (out []SessionDTO) {
	out = []SessionDTO{}
	defer func() { _ = recover() }()
	if a.engine == nil {
		return out
	}
	snap, err := a.engine.Poll(a.now())
	if err != nil {
		return out
	}
	return snapshotToDTOs(snap)
}

func (a *App) AttachSession(name string) error { return a.reg.attach(name) }
func (a *App) SendInput(name, data string)     { _ = a.reg.send(name, data) }
func (a *App) ResizeSession(name string, cols, rows int) {
	_ = a.reg.resize(name, uint16(cols), uint16(rows))
}
func (a *App) CloseSession(name string) { a.reg.close(name) }
