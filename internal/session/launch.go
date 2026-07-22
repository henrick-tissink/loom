package session

import (
	"fmt"
	"os"
	"regexp"
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

// selectCursorPattern builds the generic dialog-pending test spec §5 requires:
// any line that is the ready glyph followed by a numbered option ("❯ 1. Yes,
// proceed", "❯ 2) Sonnet") is a SELECT CURSOR, not a prompt.
//
// The allowlist this replaces — match the trust dialog's exact wording — was
// only ever safe for the one dialog whose wording we had seen. `--add-dir`
// (§5), model, theme and login dialogs all render the same numbered-select
// shape with different wording, so each would have failed the TrustMarker test,
// passed the bare-"❯" test, and had the seed typed into it: seed corruption,
// not merely a cosmetic misread. Keying on the SHAPE covers every dialog claude
// ever adds, including ones that do not exist yet.
//
// [ \t] rather than \s because Go's \s matches newlines and (?m)^ would then
// let a glyph on one line pair with a number on the next. An empty marker
// yields no pattern at all: `^\s*\d+[.)]` would call any numbered list a
// dialog.
func selectCursorPattern(marker string) *regexp.Regexp {
	if marker == "" {
		return nil
	}
	return regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(marker) + `[ \t]*\d+[.)]`)
}

type Launcher struct {
	Tmux            *tmux.Client
	Store           *store.Store
	ClaudeConfigDir string
	ClaudeJSONPath  string
	ReadyMarker     string
	TrustMarker     string
	ReadyTimeout    time.Duration // default 60s
	PollEvery       time.Duration // default 500ms
	// TrustTimeout bounds how long waitReady will sit on an unanswered trust
	// dialog. It is separate from (and much larger than) ReadyTimeout because
	// answering the dialog needs a human to attach, which is minutes-scale
	// work, not the seconds-scale wait for a TUI to mount. Default 15m.
	TrustTimeout time.Duration
}

// dirUsable reports whether p exists and is a directory. `tmux new-session -c
// <nonexistent>` exits 0 and SILENTLY falls back to $HOME (verified:
// pane_current_path becomes the home dir), so without this stat a stale path
// starts a real agent in the wrong repo with no error anywhere — the exact
// invisible failure this codebase refuses to ship (spec §12).
func dirUsable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// validateDirs checks cwd and every add-dir BEFORE the tmux session exists, so
// a bad path yields an error and no half-made session to clean up.
func validateDirs(cwd string, addDirs []string) error {
	if cwd == "" {
		return fmt.Errorf("session: empty cwd")
	}
	if !dirUsable(cwd) {
		return fmt.Errorf("session: cwd %q is not an existing directory", cwd)
	}
	for _, d := range addDirs {
		if !dirUsable(d) {
			return fmt.Errorf("session: --add-dir %q is not an existing directory", d)
		}
	}
	return nil
}

// Launch creates the tmux session running claude per the recipe, records the
// row, and (if seeded) starts the gated seed goroutine (spec §3.2, §4.2).
func (l *Launcher) Launch(r Recipe, w, h int, now time.Time) (string, error) {
	// A launch is explicit user intent with fresh paths: an unusable path is a
	// hard error, not something to filter away silently (contrast Resume).
	if err := validateDirs(r.Cwd, r.AddDirs); err != nil {
		return "", err
	}
	id := NewSessionID()
	name := TmuxName(id)
	if err := l.Tmux.NewSession(name, r.Cwd, r.ShellCommand(id), w, h); err != nil {
		return "", err
	}
	if err := l.Store.Upsert(store.SessionRow{
		Name: name, ClaudeSessionID: id, ProjectLabel: r.ProjectLabel,
		Cwd: r.Cwd, Model: r.Model, Mode: r.Mode, Seed: r.Seed,
		AddDirs:   EncodeAddDirs(r.AddDirs),
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
	// Add-dirs are re-passed on resume (--resume does not restore them) but are
	// filtered to those that still exist: a repo moved or deleted since the
	// original launch must not make an otherwise fine conversation unresumable.
	// The surviving set is written back to the row, so the loss shows up in the
	// UI's add-dir list instead of only in a mid-turn write refusal. Cwd itself
	// is NOT filtered — a session without its primary repo is not resumable at
	// all, and tmux would silently start it in $HOME.
	dirs := existingDirs(DecodeAddDirs(old.AddDirs))
	if err := validateDirs(old.Cwd, dirs); err != nil {
		return "", err
	}
	id := NewSessionID() // for the tmux name only
	name := TmuxName(id)
	if err := l.Tmux.NewSession(name, old.Cwd, ResumeShellCommand(old.ClaudeSessionID, dirs), w, h); err != nil {
		return "", err
	}
	if err := l.Store.Upsert(store.SessionRow{
		Name: name, ClaudeSessionID: old.ClaudeSessionID,
		ProjectLabel: old.ProjectLabel, Cwd: old.Cwd,
		Model: old.Model, Mode: old.Mode, Tags: old.Tags,
		AddDirs:   EncodeAddDirs(dirs),
		CreatedAt: now.Unix(), EndedAt: -1, ExitCode: -1,
		LastStatus: "unknown",
	}); err != nil {
		return name, err
	}
	return name, nil
}

func existingDirs(dirs []string) []string {
	kept := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if dirUsable(d) {
			kept = append(kept, d)
		}
	}
	return kept
}

// waitReady polls the pane until the ready marker shows on a line that is not
// a numbered select cursor (selectCursorPattern). While ANY dialog is pending
// the ready clock is paused — the user must attach and answer it; we never
// answer for them (spec §3.2 trust gate) — but the pause is itself bounded by
// TrustTimeout. Before that bound existed the trust
// branch `continue`d without advancing any clock, so a dialog nobody ever
// answered left the goroutine polling forever and the seed's outcome
// permanently unrecorded: neither 'sent' nor 'failed', which is precisely the
// silent limbo finding 4 exists to prevent (spec §12).
func (l *Launcher) waitReady(name string) bool {
	poll := l.PollEvery
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	timeout := l.ReadyTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	trustTimeout := l.TrustTimeout
	if trustTimeout <= 0 {
		trustTimeout = 15 * time.Minute
	}
	dialog := selectCursorPattern(l.ReadyMarker)
	waited, trustWaited := time.Duration(0), time.Duration(0)
	for waited < timeout && trustWaited < trustTimeout {
		out, err := l.Tmux.CapturePane(name)
		if err != nil {
			return false // session gone
		}
		// Dialog-pending must be tested BEFORE ReadyMarker: a select cursor
		// line contains the ReadyMarker glyph too, so testing ReadyMarker first
		// could fire the seed into an unanswered prompt (finding 3).
		//
		// Both tests are kept. The shape test (§5) is the general one and
		// subsumes the wording test for every dialog that has rendered its
		// cursor; TrustMarker still earns its place for the window in which the
		// question is on screen but the option list is not — capture-pane can
		// catch a half-drawn frame, and the trust dialog is the one whose
		// wording is verified.
		if strings.Contains(out, l.TrustMarker) || (dialog != nil && dialog.MatchString(out)) {
			time.Sleep(poll)
			trustWaited += poll // ready clock paused, but the wait is still bounded
			continue
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
