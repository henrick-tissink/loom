package tmux

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// throwaway server per test run
func testClient(t *testing.T) *Client {
	t.Helper()
	c := &Client{Socket: fmt.Sprintf("loomtest%d", os.Getpid())}
	t.Cleanup(func() { _ = c.KillServer() })
	if err := c.EnsureServer(); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	return c
}

func TestListSessionsNoServer(t *testing.T) {
	c := &Client{Socket: "loomtest-noserver"}
	ss, err := c.ListSessions()
	if err != nil {
		t.Fatalf("want nil error for no server, got %v", err)
	}
	if len(ss) != 0 {
		t.Fatalf("want empty, got %v", ss)
	}
}

func TestSessionLifecycle(t *testing.T) {
	c := testClient(t)
	name := "loom-11111111-1111-1111-1111-111111111111"
	if err := c.NewSession(name, t.TempDir(), "sleep 30", 120, 40); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !c.HasSession(name) {
		t.Fatal("HasSession false after create")
	}
	ss, err := c.ListSessions()
	if err != nil || len(ss) != 1 || ss[0].Name != name {
		t.Fatalf("ListSessions = %v, %v", ss, err)
	}
	ps, err := c.PaneStatus(name)
	if err != nil || ps.Dead {
		t.Fatalf("PaneStatus = %+v, %v (want alive)", ps, err)
	}
	if err := c.KillSession(name); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if c.HasSession(name) {
		t.Fatal("HasSession true after kill")
	}
}

func TestDeadPaneExitCode(t *testing.T) {
	c := testClient(t)
	name := "loom-22222222-2222-2222-2222-222222222222"
	if err := c.NewSession(name, t.TempDir(), "exit 3", 80, 24); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ps, err := c.PaneStatus(name)
		if err != nil {
			t.Fatalf("PaneStatus: %v", err)
		}
		if ps.Dead {
			if ps.ExitCode != 3 {
				t.Fatalf("ExitCode = %d, want 3", ps.ExitCode)
			}
			break // remain-on-exit kept the dead pane visible: the whole point
		}
		if time.Now().After(deadline) {
			t.Fatal("pane never went dead — is remain-on-exit on?")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSendLiteralAndCapture(t *testing.T) {
	c := testClient(t)
	name := "loom-33333333-3333-3333-3333-333333333333"
	if err := c.NewSession(name, t.TempDir(), "cat", 80, 24); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// "Enter" as literal text must NOT be interpreted as the Enter key
	if err := c.SendLiteral(name, "Enter the loop; ok"); err != nil {
		t.Fatalf("SendLiteral: %v", err)
	}
	if err := c.SendEnter(name); err != nil {
		t.Fatalf("SendEnter: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	out, err := c.CapturePane(name)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if !strings.Contains(out, "Enter the loop; ok") {
		t.Fatalf("capture missing literal text:\n%s", out)
	}
}

// unsetLocale scrubs every locale var from the test process, mimicking a
// Finder/Dock-launched app bundle (which inherits no LANG/LC_*). t.Setenv
// snapshots each var so cleanup restores the original environment.
func unsetLocale(t *testing.T) {
	t.Helper()
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// A tmux client running without a locale is put in non-UTF-8 mode by the
// server, which then vis()-sanitizes control characters in command output —
// including the \t separators our -F formats rely on — into literal '_'.
// That mangled every session name ("loom-<uuid>_1783938662"), which made
// PaneStatus fail for every live session, which made the status engine
// retire ALL sessions on every poll when the GUI was launched from the Dock.
// Control commands must parse identically with and without a host locale.
func TestListSessionsAndPaneStatusWithoutLocale(t *testing.T) {
	unsetLocale(t)
	c := testClient(t)
	name := "loom-44444444-4444-4444-4444-444444444444"
	if err := c.NewSession(name, t.TempDir(), "sleep 30", 80, 24); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ss, err := c.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(ss) != 1 || ss[0].Name != name {
		t.Fatalf("ListSessions = %+v, want one session named %q", ss, name)
	}
	if ss[0].Activity == 0 {
		t.Fatalf("Activity = 0, want the session_activity timestamp (separator not parsed?)")
	}
	ps, err := c.PaneStatus(name)
	if err != nil {
		t.Fatalf("PaneStatus: %v", err)
	}
	if ps.Dead {
		t.Fatalf("PaneStatus = %+v, want alive", ps)
	}
	if ps.CurrentPath == "" {
		t.Fatalf("CurrentPath empty, want the session cwd (separator not parsed?)")
	}
}

func TestAttachCmdStripsTMUX(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/x,123,0")
	c := &Client{Socket: "loomtest-env"}
	cmd := c.AttachCmd("loom-x")
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_PANE=") {
			t.Fatalf("env leaks %s", e)
		}
	}
	want := []string{"tmux", "-L", "loomtest-env", "attach-session", "-t", "=loom-x"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args = %v", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("args = %v, want %v", cmd.Args, want)
		}
	}
}

// envValue returns the last value for key in env ("" if absent). Last wins,
// matching how exec applies a later duplicate over an earlier one.
func envValue(env []string, key string) (string, bool) {
	val, ok := "", false
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			val, ok = strings.TrimPrefix(e, key+"="), true
		}
	}
	return val, ok
}

// When loom is launched from Finder (a GUI app bundle) the process has no TERM,
// so the attach child inherits none and `tmux attach` dies with
// "open terminal failed: terminal does not support clear". AttachCmd must
// supply a sane default so the embedded xterm.js terminal renders.
func TestAttachCmdDefaultsTERMWhenUnset(t *testing.T) {
	t.Setenv("TERM", "")
	os.Unsetenv("TERM")
	c := &Client{Socket: "loomtest-env"}
	cmd := c.AttachCmd("loom-x")
	if got, _ := envValue(cmd.Env, "TERM"); got != "xterm-256color" {
		t.Fatalf("TERM = %q, want xterm-256color", got)
	}
}

// In the TUI hand-off loom runs inside the user's real terminal, which already
// has a valid TERM. AttachCmd must not clobber it.
func TestAttachCmdPreservesExistingTERM(t *testing.T) {
	t.Setenv("TERM", "tmux-256color")
	c := &Client{Socket: "loomtest-env"}
	cmd := c.AttachCmd("loom-x")
	if got, _ := envValue(cmd.Env, "TERM"); got != "tmux-256color" {
		t.Fatalf("TERM = %q, want tmux-256color", got)
	}
}

// A Finder-launched app bundle inherits no locale vars. tmux then runs the
// attach client in non-UTF-8 mode and replaces every multibyte glyph (block
// elements, box drawing, ⚠, ’, the Claude logo) with a literal '_'. AttachCmd
// must supply a UTF-8 LC_CTYPE so tmux streams real UTF-8 to the terminal.
func TestAttachCmdDefaultsLocaleWhenUnset(t *testing.T) {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	c := &Client{Socket: "loomtest-env"}
	cmd := c.AttachCmd("loom-x")
	if got, _ := envValue(cmd.Env, "LC_CTYPE"); got != "UTF-8" {
		t.Fatalf("LC_CTYPE = %q, want UTF-8", got)
	}
}

// In the TUI hand-off the user's real terminal already has a UTF-8 locale.
// AttachCmd must not clobber it with a forced LC_CTYPE.
func TestAttachCmdPreservesExistingLocale(t *testing.T) {
	os.Unsetenv("LC_ALL")
	os.Unsetenv("LC_CTYPE")
	t.Setenv("LANG", "en_US.UTF-8")
	c := &Client{Socket: "loomtest-env"}
	cmd := c.AttachCmd("loom-x")
	if got, ok := envValue(cmd.Env, "LC_CTYPE"); ok {
		t.Fatalf("LC_CTYPE = %q, want absent (LANG already UTF-8)", got)
	}
	if got, _ := envValue(cmd.Env, "LANG"); got != "en_US.UTF-8" {
		t.Fatalf("LANG = %q, want en_US.UTF-8", got)
	}
}
