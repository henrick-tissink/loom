package main

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Go↔JS bridge. It owns no orchestration logic beyond the PTY
// registry; session state comes from the shared engine.
type App struct {
	ctx      context.Context
	engine   *status.Engine
	tm       *tmux.Client
	st       *store.Store
	launcher *session.Launcher
	projects []registry.Project
	now      func() time.Time
	reg      *ptyRegistry
	notifier *notifier
}

func newApp(engine *status.Engine, tm *tmux.Client, st *store.Store, launcher *session.Launcher, projects []registry.Project, now func() time.Time) *App {
	a := &App{engine: engine, tm: tm, st: st, launcher: launcher, projects: projects, now: now, notifier: newNotifier()}
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
	out = snapshotToDTOs(snap)
	a.onSnapshot(snap, out)
	return out
}

// onSnapshot runs the attention side effects of a poll: a native notification
// for sessions that just flipped to needs-you (once-only, from the engine),
// and the window title reflecting the current needs-you count.
func (a *App) onSnapshot(snap status.Snapshot, dtos []SessionDTO) {
	if a.notifier != nil {
		a.notifier.needsYou(snap.NewlyNeedsYou)
	}
	if a.ctx == nil {
		return
	}
	n := 0
	for _, d := range dtos {
		if d.Status == "needs_you" {
			n++
		}
	}
	title := "loom"
	if n > 0 {
		title = fmt.Sprintf("loom — %d need you", n)
	}
	wruntime.WindowSetTitle(a.ctx, title)
}

// ListProjects returns the workspace projects discovered at startup.
func (a *App) ListProjects() []ProjectDTO { return projectsToDTOs(a.projects) }

// LaunchSession starts a new claude session from the form inputs and returns
// its tmux session name. The session then appears in the next poll; the
// frontend auto-attaches it.
func (a *App) LaunchSession(projectPath, model, mode, seed string) (string, error) {
	if a.launcher == nil {
		return "", fmt.Errorf("launcher unavailable")
	}
	r, err := buildRecipe(a.projects, projectPath, model, mode, seed)
	if err != nil {
		return "", err
	}
	return a.launcher.Launch(r, 120, 32, a.now())
}

func (a *App) AttachSession(name string) error {
	if a.tm == nil {
		return fmt.Errorf("tmux unavailable")
	}
	return a.reg.attach(name)
}
func (a *App) SendInput(name, data string)     { _ = a.reg.send(name, data) }
func (a *App) ResizeSession(name string, cols, rows int) {
	_ = a.reg.resize(name, uint16(cols), uint16(rows))
}
func (a *App) CloseSession(name string) { a.reg.close(name) }

// ListRecent returns the most recent finished sessions for the Finished group.
func (a *App) ListRecent() []FinishedDTO {
	out := []FinishedDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	rows, err := a.st.Recent(30)
	if err != nil {
		return out
	}
	return recentToDTOs(rows)
}

// KillSession terminates a live tmux session (a running/needs-you agent).
func (a *App) KillSession(name string) error {
	if a.tm == nil {
		return fmt.Errorf("tmux unavailable")
	}
	return a.tm.KillSession(name)
}

// DismissSession removes a finished session from history (does not touch tmux).
func (a *App) DismissSession(name string) error {
	if a.st == nil {
		return fmt.Errorf("store unavailable")
	}
	return a.st.DeleteSession(name)
}

// ResumeSession relaunches a finished session via `claude --resume` and returns
// the new tmux session name.
func (a *App) ResumeSession(name string) (string, error) {
	if a.launcher == nil || a.st == nil {
		return "", fmt.Errorf("resume unavailable")
	}
	row, ok, err := a.st.Get(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("session %q not found", name)
	}
	return a.launcher.Resume(row, 120, 32, a.now())
}
