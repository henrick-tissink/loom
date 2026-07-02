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

// paneTarget returns the exact-match pane target for session commands that require
// a full pane specification (e.g., send-keys, capture-pane). Uses the first pane (0.0).
func paneTarget(name string) string { return "=" + name + ":0.0" }

func (c *Client) run(args ...string) (string, error) {
	full := append([]string{"-L", c.Socket}, args...)
	out, err := exec.Command("tmux", full...).CombinedOutput()
	return string(out), err
}

// EnsureServer starts the loom server (idempotent) and applies global options:
// remain-on-exit → dead panes keep exit codes for Done/Error classification (spec §6);
// status off → native-claude fidelity (spec §3.4); F12 detaches without a prefix.
func (c *Client) EnsureServer() error {
	// On macOS, start-server alone doesn't create a persistent background server.
	// We check if the server is already running (by listing sessions) and only create
	// a temporary session if needed. This makes EnsureServer truly idempotent.
	tempName := fmt.Sprintf("loom-ensure-%d", os.Getpid())
	serverRunning := false

	// Check if the server is already running by trying to list sessions.
	if out, err := c.run("list-sessions"); err == nil {
		serverRunning = true
	} else if !strings.Contains(out, "no server running") && !strings.Contains(out, "error connecting") {
		// Some other error occurred (not "server not running" or "connection error").
		return fmt.Errorf("tmux list-sessions: %s: %w", strings.TrimSpace(out), err)
	}

	// If server is not running, create a temporary session to start it.
	// It must stay alive while we set options, so we use 'sleep 86400' (24h).
	// The session is left running; callers will kill the server via KillServer().
	if !serverRunning {
		_, _ = c.run("new-session", "-d", "-s", tempName, "-c", os.TempDir(), "sleep", "86400")
	}

	opts := [][]string{
		{"set-option", "-g", "remain-on-exit", "on"},
		{"set-option", "-g", "status", "off"},
		{"set-option", "-g", "history-limit", "50000"},
		{"set-option", "-g", "window-size", "latest"},
		{"bind-key", "-n", "F12", "detach-client"},
	}
	for _, o := range opts {
		if out, err := c.run(o...); err != nil {
			return fmt.Errorf("tmux %v: %s: %w", o, strings.TrimSpace(out), err)
		}
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
		if strings.Contains(out, "no server running") || strings.Contains(out, "error connecting") {
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
		// Skip internal temporary sessions created by EnsureServer.
		if strings.HasPrefix(name, "loom-ensure-") {
			continue
		}
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
	out, err := c.run("list-panes", "-t", target(name), "-F",
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
func (c *Client) AttachCmd(name string) *exec.Cmd {
	cmd := exec.Command("tmux", "-L", c.Socket, "attach-session", "-t", target(name))
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_PANE=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = env
	return cmd
}
