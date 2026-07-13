// Package tmux wraps every Loom interaction with the dedicated `tmux -L loom` server.
package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type Client struct{ Socket string }

func New() *Client { return &Client{Socket: "loom"} }

// target returns the exact-match -t form (tmux prefix-matches bare names).
func target(name string) string { return "=" + name }

// paneTarget returns the exact-match target for session commands that require a
// pane specification (e.g., send-keys, capture-pane, list-panes). The trailing
// colon leaves the window/pane unspecified so tmux resolves the session's
// active window/pane, which is correct regardless of a user's base-index /
// pane-base-index settings (a hardcoded ":0.0" breaks when those are non-zero).
func paneTarget(name string) string { return "=" + name + ":" }

// isNoServerErr reports whether out (from a failed tmux invocation) indicates
// there is simply no server/session yet, as opposed to a real error.
func isNoServerErr(out string) bool {
	return strings.Contains(out, "no server running") || strings.Contains(out, "error connecting")
}

// ensureLocale returns env with LC_CTYPE defaulted to UTF-8 when no locale
// var is set (empty values count as unset). A Finder-launched app bundle
// inherits no LANG/LC_*, and tmux puts a locale-less client in non-UTF-8
// mode: attach clients get every multibyte glyph replaced with '_', and —
// worse — command output gets control characters sanitized the same way,
// turning the \t separators in our -F formats into '_' and mangling every
// parsed session name. Existing locales are left untouched.
func ensureLocale(env []string) []string {
	for _, e := range env {
		for _, k := range []string{"LC_ALL=", "LC_CTYPE=", "LANG="} {
			if strings.HasPrefix(e, k) && e != k {
				return env
			}
		}
	}
	return append(env, "LC_CTYPE=UTF-8")
}

func (c *Client) run(args ...string) (string, error) {
	full := append([]string{"-L", c.Socket}, args...)
	cmd := exec.Command("tmux", full...)
	cmd.Env = ensureLocale(os.Environ())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// EnsureServer starts the loom server (idempotent) and applies global options:
// exit-empty off → the server survives with zero sessions instead of exiting
// the instant the last session closes (tmux's default), so callers can rely
// on a long-lived server without a phantom keep-alive session;
// remain-on-exit → dead panes keep exit codes for Done/Error classification (spec §6);
// status off → native-claude fidelity (spec §3.4); F12 detaches without a prefix.
//
// start-server and the option calls are issued as a single tmux invocation
// (commands joined with ";") rather than separate exec.Command calls. tmux's
// default exit-empty is "on", so a freshly started server with zero sessions
// exits immediately once its client detaches; a second, separate process
// connecting afterward to flip exit-empty off loses that race almost every
// time. Chaining the commands means one client connection issues them all
// against the server it just started, before that server ever has a chance
// to see zero sessions/clients and exit.
func (c *Client) EnsureServer() error {
	args := []string{"start-server", ";"}
	opts := [][]string{
		{"set-option", "-g", "exit-empty", "off"},
		{"set-option", "-g", "remain-on-exit", "on"},
		{"set-option", "-g", "status", "off"},
		{"set-option", "-g", "history-limit", "50000"},
		{"set-option", "-g", "window-size", "latest"},
		{"bind-key", "-n", "F12", "detach-client"},
	}
	for i, o := range opts {
		args = append(args, o...)
		if i != len(opts)-1 {
			args = append(args, ";")
		}
	}
	if out, err := c.run(args...); err != nil {
		return fmt.Errorf("tmux %v: %s: %w", args, strings.TrimSpace(out), err)
	}
	return nil
}

func (c *Client) NewSession(name, cwd, shellCmd string, w, h int) error {
	out, err := c.run("new-session", "-d", "-s", name,
		"-x", strconv.Itoa(w), "-y", strconv.Itoa(h), "-c", cwd, shellCmd)
	if err != nil {
		return fmt.Errorf("tmux new-session %s: %s: %w", name, strings.TrimSpace(out), err)
	}
	return nil
}

type Session struct {
	Name     string
	Activity int64 // #{session_activity}, unix seconds
}

func (c *Client) ListSessions() ([]Session, error) {
	out, err := c.run("list-sessions", "-F", "#{session_name}\t#{session_activity}")
	if err != nil {
		// "no server running" / "error connecting" == zero sessions, not an error
		if isNoServerErr(out) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-sessions: %s: %w", strings.TrimSpace(out), err)
	}
	var ss []Session
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		name := parts[0]
		s := Session{Name: name}
		if len(parts) == 2 {
			s.Activity, _ = strconv.ParseInt(parts[1], 10, 64)
		}
		ss = append(ss, s)
	}
	return ss, nil
}

type PaneStatus struct {
	Dead        bool
	ExitCode    int
	CurrentPath string
}

func (c *Client) PaneStatus(name string) (PaneStatus, error) {
	out, err := c.run("list-panes", "-t", paneTarget(name), "-F",
		"#{pane_dead}\t#{pane_dead_status}\t#{pane_current_path}")
	if err != nil {
		return PaneStatus{}, fmt.Errorf("tmux list-panes %s: %s: %w", name, strings.TrimSpace(out), err)
	}
	line := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	parts := strings.SplitN(line, "\t", 3)
	ps := PaneStatus{}
	if len(parts) > 0 && parts[0] == "1" {
		ps.Dead = true
	}
	if len(parts) > 1 && parts[1] != "" {
		ps.ExitCode, _ = strconv.Atoi(parts[1])
	}
	if len(parts) > 2 {
		ps.CurrentPath = parts[2]
	}
	return ps, nil
}

// SendLiteral sends text with -l -- so key names ("Enter") and leading dashes
// are never reinterpreted (spec §3.2).
func (c *Client) SendLiteral(name, text string) error {
	out, err := c.run("send-keys", "-t", paneTarget(name), "-l", "--", text)
	if err != nil {
		return fmt.Errorf("tmux send-keys -l %s: %s: %w", name, strings.TrimSpace(out), err)
	}
	return nil
}

func (c *Client) SendEnter(name string) error {
	out, err := c.run("send-keys", "-t", paneTarget(name), "Enter")
	if err != nil {
		return fmt.Errorf("tmux send-keys Enter %s: %s: %w", name, strings.TrimSpace(out), err)
	}
	return nil
}

func (c *Client) CapturePane(name string) (string, error) {
	out, err := c.run("capture-pane", "-p", "-t", paneTarget(name))
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane %s: %s: %w", name, strings.TrimSpace(out), err)
	}
	return out, nil
}

func (c *Client) HasSession(name string) bool {
	_, err := c.run("has-session", "-t", target(name))
	return err == nil
}

func (c *Client) KillSession(name string) error {
	out, err := c.run("kill-session", "-t", target(name))
	if err != nil {
		return fmt.Errorf("tmux kill-session %s: %s: %w", name, strings.TrimSpace(out), err)
	}
	return nil
}

func (c *Client) KillServer() error {
	_, err := c.run("kill-server")
	return err
}

// AttachCmd builds the full-screen hand-off command. TMUX/TMUX_PANE are stripped
// so attach works when Loom itself runs inside the user's tmux (spec §3.3).
//
// TERM is defaulted to xterm-256color when the process has none: a GUI app
// bundle launched from Finder inherits no TERM, and `tmux attach` with an unset
// TERM dies with "open terminal failed: terminal does not support clear",
// leaving the embedded xterm.js terminal blank. xterm.js emulates
// xterm-256color, so that is the correct fallback. An existing TERM (the TUI
// hand-off runs inside the user's real terminal) is left untouched.
//
// LC_CTYPE is defaulted to UTF-8 for the same reason: a Finder-launched bundle
// inherits no locale, so tmux runs the attach client in non-UTF-8 mode (tmux
// keys UTF-8 off LC_ALL/LC_CTYPE/LANG — the `-u` flag was removed in tmux 2.2)
// and replaces every multibyte glyph — block elements, box drawing, ⚠, ’, the
// Claude logo — with a literal '_'. An existing locale (the TUI hand-off) is
// left untouched.
func (c *Client) AttachCmd(name string) *exec.Cmd {
	cmd := exec.Command("tmux", "-L", c.Socket, "attach-session", "-t", target(name))
	var env []string
	hasTERM := false
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_PANE=") {
			continue
		}
		if strings.HasPrefix(e, "TERM=") && e != "TERM=" {
			hasTERM = true
		}
		env = append(env, e)
	}
	if !hasTERM {
		env = append(env, "TERM=xterm-256color")
	}
	cmd.Env = ensureLocale(env)
	return cmd
}
