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
// busy/generating states). Critically, "❯" is NOT unique to the ready
// prompt: the trust dialog's own "❯ 1. Yes, proceed" select-cursor line also
// contains it. That means ReadyMarker is NOT safe by construction on its
// own — waitReady MUST test TrustMarker before ReadyMarker on every poll
// iteration (see below), or a still-open trust dialog gets misread as ready
// and the seed fires into it, mashing "1" or worse into the trust prompt.
// DefaultTrustMarker could not be triggered in the spike's environment
// (trust is inherited from an already-trusted ancestor directory) and
// remains a defensive, unverified candidate; ordering it first is still the
// safe choice even though it wasn't observed live.
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
		// TrustMarker must be checked BEFORE ReadyMarker: the trust dialog's
		// "❯ 1. Yes, proceed" cursor line contains the ReadyMarker glyph too,
		// so testing ReadyMarker first could fire the seed into an unanswered
		// trust prompt (finding 3).
		if strings.Contains(out, l.TrustMarker) {
			time.Sleep(poll)
			continue // trust pending: clock paused
		}
		if strings.Contains(out, l.ReadyMarker) {
			return true
		}
		time.Sleep(poll)
		waited += poll
	}
	return false
}

// seedWhenReady is best-effort about the SESSION (never fatal to it), but the
// seed's own outcome must not be silently dropped (finding 4): the store
// records 'sent' or 'failed' so the UI can surface a missed seed instead of
// it vanishing without a trace.
func (l *Launcher) seedWhenReady(name, seed string) {
	if !l.waitReady(name) {
		_ = l.Store.SetSeedStatus(name, "failed") // timed out or session gone
		return
	}
	if err := l.Tmux.SendLiteral(name, seed); err != nil {
		_ = l.Store.SetSeedStatus(name, "failed")
		return
	}
	if err := l.Tmux.SendEnter(name); err != nil {
		_ = l.Store.SetSeedStatus(name, "failed")
		return
	}
	_ = l.Store.SetSeedStatus(name, "sent")
}
