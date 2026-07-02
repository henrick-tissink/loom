package session

import (
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

// Markers verified by the Task-2 spike against claude 2.1.198 ("Fable 5"
// branding). The brief's "? for shortcuts" candidate does not exist in this
// build — the spike found the ready state is a bare "❯" input prompt glyph
// (pre-mount, before claude's TUI renders, there is no "❯" at all, so its
// appearance is a safe mount signal even though it also persists through
// busy/generating states). DefaultTrustMarker could not be triggered in the
// spike's environment (trust is inherited from an already-trusted ancestor
// directory) and remains a defensive, unverified candidate — safe by
// construction because the seed only ever fires once ReadyMarker is seen.
const (
	DefaultReadyMarker = "❯"
	DefaultTrustMarker = "Do you trust the files in this folder?"
)

type Launcher struct {
	Tmux            *tmux.Client
	Store           *store.Store
	ClaudeConfigDir string
	ClaudeJSONPath  string
	ReadyMarker     string
	TrustMarker     string
	ReadyTimeout    time.Duration // default 60s
	PollEvery       time.Duration // default 500ms
}

// Launch creates the tmux session running claude per the recipe, records the
// row, and (if seeded) starts the gated seed goroutine (spec §3.2, §4.2).
func (l *Launcher) Launch(r Recipe, w, h int, now time.Time) (string, error) {
	id := NewSessionID()
	name := TmuxName(id)
	if err := l.Tmux.NewSession(name, r.Cwd, r.ShellCommand(id), w, h); err != nil {
		return "", err
	}
	if err := l.Store.Upsert(store.SessionRow{
		Name: name, ClaudeSessionID: id, ProjectLabel: r.ProjectLabel,
		Cwd: r.Cwd, Model: r.Model, Mode: r.Mode, Seed: r.Seed,
		CreatedAt: now.Unix(), EndedAt: -1, ExitCode: -1,
		LastStatus: "unknown",
	}); err != nil {
		return name, err // session exists; row failed — surfaced, not fatal
	}
	if r.Seed != "" {
		go l.seedWhenReady(name, r.Seed)
	}
	return name, nil
}

// Resume relaunches a finished session via `claude --resume`. Spike-verified
// (2026-07-02): --resume appends to the SAME <uuid>.jsonl under the SAME
// sessionId — claude does NOT mint a new id for a resumed conversation — so,
// unlike the brief's original draft, no post-ready correction goroutine is
// needed or started here; ClaudeSessionID carries over unchanged.
func (l *Launcher) Resume(old store.SessionRow, w, h int, now time.Time) (string, error) {
	id := NewSessionID() // for the tmux name only
	name := TmuxName(id)
	if err := l.Tmux.NewSession(name, old.Cwd, ResumeShellCommand(old.ClaudeSessionID), w, h); err != nil {
		return "", err
	}
	if err := l.Store.Upsert(store.SessionRow{
		Name: name, ClaudeSessionID: old.ClaudeSessionID,
		ProjectLabel: old.ProjectLabel, Cwd: old.Cwd,
		Model: old.Model, Mode: old.Mode, Tags: old.Tags,
		CreatedAt: now.Unix(), EndedAt: -1, ExitCode: -1,
		LastStatus: "unknown",
	}); err != nil {
		return name, err
	}
	return name, nil
}

// waitReady polls the pane until the ready marker shows. While the trust
// dialog is visible the timeout clock is paused — the user must attach and
// answer it; we never answer for them (spec §3.2 trust gate).
func (l *Launcher) waitReady(name string) bool {
	poll := l.PollEvery
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	timeout := l.ReadyTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	waited := time.Duration(0)
	for waited < timeout {
		out, err := l.Tmux.CapturePane(name)
		if err != nil {
			return false // session gone
		}
		if strings.Contains(out, l.ReadyMarker) {
			return true
		}
		if strings.Contains(out, l.TrustMarker) {
			time.Sleep(poll)
			continue // trust pending: clock paused
		}
		time.Sleep(poll)
		waited += poll
	}
	return false
}

func (l *Launcher) seedWhenReady(name, seed string) {
	if !l.waitReady(name) {
		return // best-effort: session unharmed, seed skipped
	}
	if err := l.Tmux.SendLiteral(name, seed); err != nil {
		return
	}
	_ = l.Tmux.SendEnter(name)
}
