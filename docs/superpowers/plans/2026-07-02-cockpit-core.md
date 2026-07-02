# Loom Phase 1 — Cockpit Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go + Bubble Tea TUI ("Loom") that launches, lists, attaches, and remembers real `claude` sessions orchestrated on a dedicated `tmux -L loom` server, with a live-status dashboard backed by SQLite and claude's JSONL transcripts.

**Architecture:** Loom shells out to a dedicated tmux server (`-L loom`) for all session lifecycle (create/list/attach/kill — tmux is the source of truth for LIVE sessions; SQLite owns HISTORY). Each session runs the real `claude` CLI with a Loom-minted `--session-id <uuid>`, making the transcript path deterministic; a poll loop incrementally reads transcripts and fuses their turn-boundary state with tmux pane liveness into dashboard statuses. Attach is a full-screen `tea.ExecProcess` hand-off to `tmux attach` (with `$TMUX` stripped for nested launches).

**Tech Stack:** Go ≥1.22 · charmbracelet/bubbletea + lipgloss + bubbles(textinput) · modernc.org/sqlite (pure Go) · google/uuid · tmux 3.7b · claude CLI 2.1.198.

**Spec:** `docs/superpowers/specs/2026-07-02-cockpit-core-design.md` (Revision 2 — read it before starting a task if anything here seems ambiguous).

## Global Constraints

- Go module path: `github.com/henricktissink/loom`. Go ≥ 1.22.
- Every tmux invocation MUST use the dedicated socket: `tmux -L loom …`. Never touch the user's default tmux server. Tests use throwaway sockets (`-L loomtest<N>`) and `kill-server` in cleanup.
- tmux `-t` targets MUST use exact-match form `=<name>` (tmux prefix-matches otherwise).
- tmux session names are ALWAYS `loom-<uuid>` — never a project label (`.`/`:` in labels break `-t` targeting).
- Verified claude flags (v2.1.198): `--session-id <uuid>`, `--permission-mode <default|acceptEdits|plan|auto|bypassPermissions>`, `--model <opus|sonnet|fable|full-id>`, `--resume <session-id>`. Seed injection is NEVER via positional arg — always gated `send-keys -l --` + separate `Enter`.
- Transcript path: `$CLAUDE_CONFIG_DIR` (default `~/.claude`) + `/projects/<cwd with every non-[a-zA-Z0-9] rune → '-'>/<session-id>.jsonl`.
- Trust flags live in `~/.claude.json` (or `$CLAUDE_CONFIG_DIR/../.claude.json` is WRONG — it is always `$HOME/.claude.json` unless spike says otherwise) under `.projects["<abs cwd>"].hasTrustDialogAccepted`.
- SQLite DSN pragmas are mandatory: `journal_mode(WAL)`, `busy_timeout(5000)`, `synchronous(NORMAL)`; `db.SetMaxOpenConns(1)`.
- Status strings (DB + UI): `running`, `needs_you`, `idle`, `done`, `error`, `unknown`.
- Never classify status on the physical last JSONL line — only on the last record with `type ∈ {assistant, user}` and `isSidechain == false`.
- Commit style: conventional commits (`feat:`, `test:`, `chore:`, `docs:`). Commit at the end of every task at minimum.
- One deliberate deviation from the spec: transcript reading is a **polling incremental reader inside the status-engine poll loop** (1.5s cadence) instead of per-file fsnotify watchers. Same observable behavior (status freshness ≤ poll interval), far fewer goroutines/races. fsnotify remains a documented future optimization.

## File Structure

```
loom/
  go.mod / go.sum
  cmd/loom/main.go                     — wiring + startup checks
  internal/config/config.go            — paths, env resolution, binary checks        (Task 3)
  internal/tmux/tmux.go                — tmux -L loom wrapper                        (Task 4)
  internal/session/recipe.go           — Recipe, session IDs, claude argv            (Task 5)
  internal/transcript/path.go          — cwd→transcript-dir encoding, NewestSince    (Task 6)
  internal/transcript/classify.go      — streaming turn-boundary state machine       (Task 7)
  internal/transcript/reader.go        — incremental polling JSONL reader            (Task 8)
  internal/trust/trust.go              — ~/.claude.json trust check                  (Task 9)
  internal/store/store.go              — SQLite store + migrations                   (Task 10)
  internal/registry/registry.go        — ~/Sauce project discovery                   (Task 11)
  internal/status/status.go            — Status type + fusion                        (Task 12)
  internal/status/engine.go            — poll/reconcile loop                         (Task 12)
  internal/session/launch.go           — orchestrated launch/seed/resume             (Task 13)
  internal/ui/styles.go                — lipgloss styles                             (Task 14)
  internal/ui/launcher.go              — launcher form                               (Task 14)
  internal/ui/app.go                   — top-level model, dashboard, attach          (Task 14)
  docs/spikes/2026-07-02-session-id-spike.md                                         (Task 2)
```

Each `internal/X/Y.go` has a sibling `Y_test.go`.

---

### Task 1: Toolchain + project scaffold

**Files:**
- Create: `go.mod`, `.gitignore`

**Interfaces:**
- Produces: a compiling Go module `github.com/henricktissink/loom` with all dependencies in `go.mod`.

- [ ] **Step 1: Install Go (not currently on this machine)**

Run: `brew install go`
Then: `go version`
Expected: `go version go1.2x.x darwin/arm64` (any ≥1.22)

- [ ] **Step 2: Init module and fetch dependencies**

Run from `/Users/henricktissink/Sauce/loom`:
```bash
go mod init github.com/henricktissink/loom
go get github.com/charmbracelet/bubbletea@latest \
       github.com/charmbracelet/lipgloss@latest \
       github.com/charmbracelet/bubbles@latest \
       modernc.org/sqlite@latest \
       github.com/google/uuid@latest
```
Expected: `go.mod` lists all five; no errors.

- [ ] **Step 3: Add .gitignore**

```gitignore
loom
*.db
*.db-wal
*.db-shm
.DS_Store
```

- [ ] **Step 4: Sanity-compile**

Create a throwaway `cmd/loom/main.go`:
```go
package main

import "fmt"

func main() { fmt.Println("loom") }
```
Run: `go build ./... && go run ./cmd/loom`
Expected: prints `loom`.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "chore: scaffold Go module with charm/sqlite/uuid deps"
```

---

### Task 2: Spike — `--session-id`, readiness/trust markers, resume behavior

This spike gates the whole status layer (spec §11 #1). It produces a findings doc, not code. **Do not skip; do not guess.**

**Files:**
- Create: `docs/spikes/2026-07-02-session-id-spike.md`

**Interfaces:**
- Produces: verified constants used later — `READY_MARKER` (capture-pane substring proving claude's input box is live; expected candidate: `? for shortcuts`), `TRUST_MARKER` (expected candidate: `Do you trust the files in this folder?`), confirmation that `--session-id <fresh-uuid>` creates `<uuid>.jsonl`, and the observed `--resume` transcript behavior (same file vs new file).

- [ ] **Step 1: Fresh-uuid interactive launch**

```bash
UUID=$(uuidgen | tr 'A-Z' 'a-z')
echo "UUID=$UUID"
tmux -L loomspike new-session -d -s spike -x 200 -y 50 \
  -c /Users/henricktissink/Sauce/loom "claude --session-id $UUID"
sleep 10
tmux -L loomspike capture-pane -p -t =spike | tail -40
```
Record in the findings doc: (a) did claude start a NEW clean session or error/collide? (b) EXACT text visible when the input box is ready (candidate `READY_MARKER`), (c) if the trust dialog appeared (loom/ is untrusted, so it should), its EXACT text (candidate `TRUST_MARKER`).

- [ ] **Step 2: Trust-dialog flow + JSONL path**

Attach interactively, answer the trust dialog, verify readiness, then check the transcript:
```bash
tmux -L loomspike attach -t =spike     # answer trust dialog, then detach: Ctrl-b d
ls ~/.claude/projects/-Users-henricktissink-Sauce-loom/ | grep "$UUID"
```
Expected: `<UUID>.jsonl` exists. Record: exact encoded dir name (confirms the `non-[a-zA-Z0-9] → '-'` rule).

- [ ] **Step 3: Gated seed injection**

```bash
tmux -L loomspike send-keys -t =spike -l -- "reply with exactly: pong"
tmux -L loomspike send-keys -t =spike Enter
sleep 15
tail -c 2000 ~/.claude/projects/-Users-henricktissink-Sauce-loom/$UUID.jsonl
```
Expected: transcript contains the seeded prompt and a response. Record any surprises.

- [ ] **Step 4: Resume behavior**

```bash
tmux -L loomspike kill-session -t =spike
tmux -L loomspike new-session -d -s spike2 -x 200 -y 50 \
  -c /Users/henricktissink/Sauce/loom "claude --resume $UUID"
sleep 10
ls -t ~/.claude/projects/-Users-henricktissink-Sauce-loom/*.jsonl | head -3
```
Record: does the resumed session append to `$UUID.jsonl` or create a NEW `<newuuid>.jsonl`? This decides whether `Launcher.Resume` (Task 13) needs the `NewestSince` fallback (plan assumes it does — if resume reuses the same file, simplify Task 13 accordingly and note it in the findings doc).

- [ ] **Step 5: Idle-activity probe (for the fusion heuristic)**

With the resumed session idle at its input box:
```bash
A1=$(tmux -L loomspike list-sessions -F "#{session_name} #{session_activity}"); sleep 5
A2=$(tmux -L loomspike list-sessions -F "#{session_name} #{session_activity}")
echo "$A1"; echo "$A2"
```
Record: does `session_activity` advance while claude idles (spinner/animation)? If yes, note that `paneActive` (Task 12) must use a larger threshold or be dropped.

- [ ] **Step 6: Cleanup + write findings + commit**

```bash
tmux -L loomspike kill-server 2>/dev/null || true
```
Write `docs/spikes/2026-07-02-session-id-spike.md` with every recorded answer (exact marker strings verbatim). Then:
```bash
git add docs/spikes && git commit -m "docs: record session-id/readiness/resume spike findings"
```

**Decision matrix:** if `--session-id` does NOT cleanly create a fresh session, STOP and revise: correlation falls back to `transcript.NewestSince` everywhere (Task 6 builds it anyway) and Tasks 5/13 drop the flag. If it works (expected), proceed unchanged.

---

### Task 3: Config package — paths, env, binary checks

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces:
  - `type Config struct { ClaudeConfigDir, LoomDir, WorkspaceRoot, Home string }`
  - `func Load() (*Config, error)` — resolves `$CLAUDE_CONFIG_DIR` (default `$HOME/.claude`), `LoomDir` = `$HOME/.loom` (mkdir -p), `WorkspaceRoot` = `$HOME/Sauce`.
  - `func (c *Config) DBPath() string` — `<LoomDir>/loom.db`
  - `func (c *Config) ClaudeJSONPath() string` — `<Home>/.claude.json`
  - `func CheckBinaries() error` — errors mentioning the missing binary if `tmux` or `claude` not on PATH.
  - `func InsideTmux() bool` — `os.Getenv("TMUX") != ""`.

- [ ] **Step 1: Write the failing test**

`internal/config/config_test.go`:
```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ClaudeConfigDir != filepath.Join(home, ".claude") {
		t.Errorf("ClaudeConfigDir = %q", c.ClaudeConfigDir)
	}
	if c.DBPath() != filepath.Join(home, ".loom", "loom.db") {
		t.Errorf("DBPath = %q", c.DBPath())
	}
	if c.ClaudeJSONPath() != filepath.Join(home, ".claude.json") {
		t.Errorf("ClaudeJSONPath = %q", c.ClaudeJSONPath())
	}
	if _, err := os.Stat(filepath.Join(home, ".loom")); err != nil {
		t.Errorf(".loom dir not created: %v", err)
	}
}

func TestLoadRespectsClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ClaudeConfigDir != "/custom/claude" {
		t.Errorf("ClaudeConfigDir = %q", c.ClaudeConfigDir)
	}
}

func TestInsideTmux(t *testing.T) {
	t.Setenv("TMUX", "/tmp/x,1,0")
	if !InsideTmux() {
		t.Error("expected true when TMUX set")
	}
	t.Setenv("TMUX", "")
	if InsideTmux() {
		t.Error("expected false when TMUX empty")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -v`
Expected: FAIL (package does not exist / undefined symbols).

- [ ] **Step 3: Implement**

`internal/config/config.go`:
```go
// Package config resolves Loom's filesystem paths and environment.
package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type Config struct {
	ClaudeConfigDir string // $CLAUDE_CONFIG_DIR or $HOME/.claude
	LoomDir         string // $HOME/.loom
	WorkspaceRoot   string // $HOME/Sauce
	Home            string
}

func Load() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}
	ccd := os.Getenv("CLAUDE_CONFIG_DIR")
	if ccd == "" {
		ccd = filepath.Join(home, ".claude")
	}
	c := &Config{
		ClaudeConfigDir: ccd,
		LoomDir:         filepath.Join(home, ".loom"),
		WorkspaceRoot:   filepath.Join(home, "Sauce"),
		Home:            home,
	}
	if err := os.MkdirAll(c.LoomDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", c.LoomDir, err)
	}
	return c, nil
}

func (c *Config) DBPath() string          { return filepath.Join(c.LoomDir, "loom.db") }
func (c *Config) ClaudeJSONPath() string  { return filepath.Join(c.Home, ".claude.json") }

// CheckBinaries verifies tmux and claude are on PATH.
func CheckBinaries() error {
	for _, bin := range []string{"tmux", "claude"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found on PATH — Loom requires it (install and retry)", bin)
		}
	}
	return nil
}

func InsideTmux() bool { return os.Getenv("TMUX") != "" }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/config && git commit -m "feat: config package (paths, env, binary checks)"
```

---

### Task 4: tmux wrapper package

All Loom↔tmux interaction goes through this package. Integration-tested against a real throwaway tmux server.

**Files:**
- Create: `internal/tmux/tmux.go`, `internal/tmux/tmux_test.go`

**Interfaces:**
- Produces:
  - `type Client struct { Socket string }` · `func New() *Client` (Socket `"loom"`)
  - `func (c *Client) EnsureServer() error` — start-server + global options (remain-on-exit on, status off, history-limit 50000, window-size latest, `bind-key -n F12 detach-client`).
  - `func (c *Client) NewSession(name, cwd, shellCmd string, w, h int) error`
  - `type Session struct { Name string; Activity int64 }` · `func (c *Client) ListSessions() ([]Session, error)` — empty slice (nil error) when the server isn't running.
  - `type PaneStatus struct { Dead bool; ExitCode int; CurrentPath string }` · `func (c *Client) PaneStatus(name string) (PaneStatus, error)`
  - `func (c *Client) SendLiteral(name, text string) error` · `func (c *Client) SendEnter(name string) error`
  - `func (c *Client) CapturePane(name string) (string, error)`
  - `func (c *Client) HasSession(name string) bool` · `func (c *Client) KillSession(name string) error` · `func (c *Client) KillServer() error`
  - `func (c *Client) AttachCmd(name string) *exec.Cmd` — env = `os.Environ()` minus `TMUX`/`TMUX_PANE`.

- [ ] **Step 1: Write the failing integration test**

`internal/tmux/tmux_test.go`:
```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tmux/ -v`
Expected: FAIL (undefined symbols).

- [ ] **Step 3: Implement**

`internal/tmux/tmux.go`:
```go
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

func (c *Client) run(args ...string) (string, error) {
	full := append([]string{"-L", c.Socket}, args...)
	out, err := exec.Command("tmux", full...).CombinedOutput()
	return string(out), err
}

// EnsureServer starts the loom server (idempotent) and applies global options:
// remain-on-exit → dead panes keep exit codes for Done/Error classification (spec §6);
// status off → native-claude fidelity (spec §3.4); F12 detaches without a prefix.
func (c *Client) EnsureServer() error {
	if _, err := c.run("start-server"); err != nil {
		return fmt.Errorf("tmux start-server: %w", err)
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
		s := Session{Name: parts[0]}
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
	out, err := c.run("send-keys", "-t", target(name), "-l", "--", text)
	if err != nil {
		return fmt.Errorf("tmux send-keys -l %s: %s: %w", name, strings.TrimSpace(out), err)
	}
	return nil
}

func (c *Client) SendEnter(name string) error {
	out, err := c.run("send-keys", "-t", target(name), "Enter")
	if err != nil {
		return fmt.Errorf("tmux send-keys Enter %s: %s: %w", name, strings.TrimSpace(out), err)
	}
	return nil
}

func (c *Client) CapturePane(name string) (string, error) {
	out, err := c.run("capture-pane", "-p", "-t", target(name))
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tmux/ -v`
Expected: PASS (5 tests, ~6s due to the dead-pane wait).

- [ ] **Step 5: Commit**

```bash
git add internal/tmux && git commit -m "feat: tmux wrapper on dedicated loom socket (exact-match targets, remain-on-exit, TMUX-stripped attach)"
```

---

### Task 5: Session recipe — IDs, names, claude argv

**Files:**
- Create: `internal/session/recipe.go`, `internal/session/recipe_test.go`

**Interfaces:**
- Produces:
  - `type Recipe struct { ProjectLabel, Cwd, Model, Mode, Seed string }` — `Model ∈ {"", "opus", "sonnet", "fable"}` ("" = claude default, no flag); `Mode ∈ {"", "plan", "acceptEdits", "auto", "bypassPermissions"}` ("" = default, no flag).
  - `func NewSessionID() string` — lowercase UUIDv4.
  - `func TmuxName(sessionID string) string` — `"loom-" + sessionID`.
  - `func SessionIDFromTmuxName(name string) (string, bool)` — inverse; false if not a `loom-` name.
  - `func (r Recipe) Argv(sessionID string) []string` — e.g. `["claude", "--session-id", id, "--model", "opus", "--permission-mode", "plan"]`.
  - `func (r Recipe) ShellCommand(sessionID string) string` — single-quoted argv join for tmux's shell command.
  - `func ResumeShellCommand(claudeSessionID string) string` — `claude --resume <id>` quoted the same way.

- [ ] **Step 1: Write the failing test**

`internal/session/recipe_test.go`:
```go
package session

import (
	"reflect"
	"regexp"
	"testing"
)

func TestNewSessionIDAndTmuxName(t *testing.T) {
	id := NewSessionID()
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("not a lowercase uuid: %q", id)
	}
	name := TmuxName(id)
	if name != "loom-"+id {
		t.Fatalf("TmuxName = %q", name)
	}
	got, ok := SessionIDFromTmuxName(name)
	if !ok || got != id {
		t.Fatalf("SessionIDFromTmuxName = %q, %v", got, ok)
	}
	if _, ok := SessionIDFromTmuxName("notloom-x"); ok {
		t.Fatal("accepted non-loom name")
	}
}

func TestArgv(t *testing.T) {
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	cases := []struct {
		r    Recipe
		want []string
	}{
		{Recipe{}, []string{"claude", "--session-id", id}},
		{Recipe{Model: "opus"}, []string{"claude", "--session-id", id, "--model", "opus"}},
		{Recipe{Mode: "plan"}, []string{"claude", "--session-id", id, "--permission-mode", "plan"}},
		{Recipe{Model: "sonnet", Mode: "auto"},
			[]string{"claude", "--session-id", id, "--model", "sonnet", "--permission-mode", "auto"}},
	}
	for _, c := range cases {
		if got := c.r.Argv(id); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Argv(%+v) = %v, want %v", c.r, got, c.want)
		}
	}
}

func TestShellCommand(t *testing.T) {
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	r := Recipe{Model: "opus", Mode: "plan"}
	want := "'claude' '--session-id' '" + id + "' '--model' 'opus' '--permission-mode' 'plan'"
	if got := r.ShellCommand(id); got != want {
		t.Errorf("ShellCommand = %q, want %q", got, want)
	}
	if got := ResumeShellCommand(id); got != "'claude' '--resume' '"+id+"'" {
		t.Errorf("ResumeShellCommand = %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/session/ -v`
Expected: FAIL (undefined symbols).

- [ ] **Step 3: Implement**

`internal/session/recipe.go`:
```go
// Package session defines the launch recipe (the Phase-3 "workflow step" shape)
// and orchestrated session launching.
package session

import (
	"strings"

	"github.com/google/uuid"
)

// Recipe is everything needed to launch a configured claude session.
// In Phase 3 a saved workflow step IS this struct plus a context relation.
type Recipe struct {
	ProjectLabel string
	Cwd          string
	Model        string // "", "opus", "sonnet", "fable"
	Mode         string // "", "plan", "acceptEdits", "auto", "bypassPermissions"
	Seed         string // optional initial prompt or /slash-command
}

func NewSessionID() string { return uuid.NewString() }

const tmuxPrefix = "loom-"

// TmuxName builds the tmux-safe session name. NEVER embed the project label:
// '.'/':' break tmux -t targeting (spec §4.2).
func TmuxName(sessionID string) string { return tmuxPrefix + sessionID }

func SessionIDFromTmuxName(name string) (string, bool) {
	if !strings.HasPrefix(name, tmuxPrefix) {
		return "", false
	}
	return strings.TrimPrefix(name, tmuxPrefix), true
}

func (r Recipe) Argv(sessionID string) []string {
	argv := []string{"claude", "--session-id", sessionID}
	if r.Model != "" {
		argv = append(argv, "--model", r.Model)
	}
	if r.Mode != "" {
		argv = append(argv, "--permission-mode", r.Mode)
	}
	return argv
}

func shellQuote(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

func (r Recipe) ShellCommand(sessionID string) string {
	return shellQuote(r.Argv(sessionID))
}

func ResumeShellCommand(claudeSessionID string) string {
	return shellQuote([]string{"claude", "--resume", claudeSessionID})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/session/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/session && git commit -m "feat: session recipe with verified claude argv construction"
```

---

### Task 6: Transcript path mapping

**Files:**
- Create: `internal/transcript/path.go`, `internal/transcript/path_test.go`

**Interfaces:**
- Consumes: nothing (pure functions).
- Produces:
  - `func ProjectDirName(cwd string) string` — every rune outside `[a-zA-Z0-9]` becomes `-`.
  - `func Path(claudeConfigDir, cwd, sessionID string) string` — `<ccd>/projects/<enc>/<id>.jsonl`.
  - `func NewestSince(claudeConfigDir, cwd string, since time.Time) (sessionID string, err error)` — newest `*.jsonl` in the project dir modified after `since`; `("", nil)` if none. Fallback correlation for `--resume` (Task 13) and the spike's failure branch.

- [ ] **Step 1: Write the failing test**

`internal/transcript/path_test.go`:
```go
package transcript

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProjectDirName(t *testing.T) {
	cases := map[string]string{
		"/Users/henricktissink/Sauce/HappyPay": "-Users-henricktissink-Sauce-HappyPay",
		"/a/b.c/d_e":                           "-a-b-c-d-e",
		"/x/HappyPay.Web":                      "-x-HappyPay-Web",
	}
	for in, want := range cases {
		if got := ProjectDirName(in); got != want {
			t.Errorf("ProjectDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPath(t *testing.T) {
	got := Path("/home/u/.claude", "/w/proj", "abc-123")
	want := filepath.Join("/home/u/.claude", "projects", "-w-proj", "abc-123.jsonl")
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestNewestSince(t *testing.T) {
	ccd := t.TempDir()
	dir := filepath.Join(ccd, "projects", "-w-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, mod time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	write("old.jsonl", now.Add(-time.Hour))
	write("new.jsonl", now.Add(time.Minute))
	id, err := NewestSince(ccd, "/w/proj", now)
	if err != nil || id != "new" {
		t.Fatalf("NewestSince = %q, %v (want new)", id, err)
	}
	id, err = NewestSince(ccd, "/w/proj", now.Add(2*time.Minute))
	if err != nil || id != "" {
		t.Fatalf("NewestSince (none) = %q, %v (want empty)", id, err)
	}
	id, err = NewestSince(ccd, "/no/such", now)
	if err != nil || id != "" {
		t.Fatalf("NewestSince (missing dir) = %q, %v (want empty, nil)", id, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transcript/ -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/transcript/path.go`:
```go
// Package transcript locates and interprets claude's JSONL session transcripts.
package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProjectDirName applies claude's encoding: every non-[a-zA-Z0-9] rune → '-'.
func ProjectDirName(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func Path(claudeConfigDir, cwd, sessionID string) string {
	return filepath.Join(claudeConfigDir, "projects", ProjectDirName(cwd), sessionID+".jsonl")
}

// NewestSince returns the session ID of the newest transcript in cwd's project
// dir modified after `since` — the fallback correlation when the session ID
// isn't deterministic (claude --resume mints a new one).
func NewestSince(claudeConfigDir, cwd string, since time.Time) (string, error) {
	dir := filepath.Join(claudeConfigDir, "projects", ProjectDirName(cwd))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var best string
	var bestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(since) && info.ModTime().After(bestMod) {
			best = strings.TrimSuffix(e.Name(), ".jsonl")
			bestMod = info.ModTime()
		}
	}
	return best, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/transcript/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/transcript && git commit -m "feat: transcript path encoding and newest-since fallback correlation"
```

---

### Task 7: Transcript classifier — streaming turn-boundary state machine

The P0-corrected status logic (spec §4.3). Streaming (feed line-by-line) so giant transcripts never need to be held in memory.

**Files:**
- Create: `internal/transcript/classify.go`, `internal/transcript/classify_test.go`

**Interfaces:**
- Produces:
  - `type State int` with `const (StateUnknown State = iota; StateRunning; StateNeedsYou; StateIdle)`
  - `type Classifier struct { /* unexported */ }`
  - `func (c *Classifier) Feed(line []byte)` — parses one complete JSONL line; ignores sidecars (`mode`, `permission-mode`, `last-prompt`, `ai-title`, `file-history-snapshot`, `attachment`, `queue-operation`, `system`, anything else) and sidechain records; updates state only on real turn records.
  - `func (c *Classifier) State() State`
  - `func (c *Classifier) LastTool() string` — name of the most recent `tool_use` block (dashboard hint, e.g. `Edit`).

Classification (on the last non-sidechain `assistant`/`user` record):
- assistant containing a `tool_use` block → `StateRunning` (a matching `tool_result` would arrive as a LATER user record, so "last turn = assistant tool_use" means the tool is pending)
- assistant without `tool_use` → `StateNeedsYou`
- user containing `tool_result` → `StateRunning` (claude is processing the result)
- user otherwise (a human prompt) → `StateIdle` (the pane-activity fusion in Task 12 upgrades this to Running while claude is actually streaming)

- [ ] **Step 1: Write the failing test**

`internal/transcript/classify_test.go`:
```go
package transcript

import "testing"

// Line shapes verified against real ~/.claude transcripts on 2026-07-02.
const (
	lineUserPrompt   = `{"type":"user","message":{"role":"user","content":"add a vega hedge"}}`
	lineAsstToolUse  = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{}}],"stop_reason":"tool_use"}}`
	lineToolResult   = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	lineAsstEndTurn  = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn"}}`
	linePermMode     = `{"type":"permission-mode","permissionMode":"default","sessionId":"x"}`
	lineMode         = `{"type":"mode","mode":"normal","sessionId":"x"}`
	lineSnapshot     = `{"type":"file-history-snapshot","messageId":"m1","snapshot":{},"isSnapshotUpdate":false}`
	lineSystem       = `{"type":"system","subtype":"hook","content":"x"}`
	lineSidechainAst = `{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"tool_use","id":"s1","name":"Bash","input":{}}]}}`
)

func feed(t *testing.T, lines ...string) *Classifier {
	t.Helper()
	c := &Classifier{}
	for _, l := range lines {
		c.Feed([]byte(l))
	}
	return c
}

func TestEmptyIsUnknown(t *testing.T) {
	if s := (&Classifier{}).State(); s != StateUnknown {
		t.Fatalf("state = %v, want Unknown", s)
	}
}

func TestUserPromptIsIdle(t *testing.T) {
	if s := feed(t, lineUserPrompt).State(); s != StateIdle {
		t.Fatalf("state = %v, want Idle", s)
	}
}

func TestPendingToolUseIsRunning(t *testing.T) {
	c := feed(t, lineUserPrompt, lineAsstToolUse)
	if c.State() != StateRunning {
		t.Fatalf("state = %v, want Running", c.State())
	}
	if c.LastTool() != "Edit" {
		t.Fatalf("LastTool = %q, want Edit", c.LastTool())
	}
}

func TestToolResultIsRunning(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstToolUse, lineToolResult).State(); s != StateRunning {
		t.Fatalf("state = %v, want Running", s)
	}
}

func TestEndTurnIsNeedsYou(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstToolUse, lineToolResult, lineAsstEndTurn).State(); s != StateNeedsYou {
		t.Fatalf("state = %v, want NeedsYou", s)
	}
}

// P0 regression guard (spec §7): sidecar tail must NOT change classification.
func TestSidecarTailDoesNotReclassify(t *testing.T) {
	c := feed(t, lineUserPrompt, lineAsstEndTurn, linePermMode, lineMode, lineSnapshot, lineSystem)
	if c.State() != StateNeedsYou {
		t.Fatalf("state = %v, want NeedsYou (permission-mode is NOT a permission prompt)", c.State())
	}
}

func TestSidechainRecordsIgnored(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstEndTurn, lineSidechainAst).State(); s != StateNeedsYou {
		t.Fatalf("state = %v, want NeedsYou (sidechain ignored)", s)
	}
}

func TestGarbageLineIgnored(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstEndTurn, `{"type":`).State(); s != StateNeedsYou {
		t.Fatalf("state = %v, want NeedsYou (partial line ignored)", s)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transcript/ -run 'Unknown|Idle|Running|NeedsYou|Sidecar|Sidechain|Garbage' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/transcript/classify.go`:
```go
package transcript

import "encoding/json"

type State int

const (
	StateUnknown State = iota
	StateRunning
	StateNeedsYou
	StateIdle
)

func (s State) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateNeedsYou:
		return "needs_you"
	case StateIdle:
		return "idle"
	default:
		return "unknown"
	}
}

type record struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Message     *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// Classifier folds JSONL lines into the session's turn-boundary state.
// It NEVER classifies on sidecar records (mode, permission-mode, last-prompt,
// ai-title, file-history-snapshot, attachment, queue-operation, system) — real
// transcripts flush those after the final turn (spec §4.3, P0).
type Classifier struct {
	state    State
	lastTool string
}

func (c *Classifier) Feed(line []byte) {
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		return // partial/garbage line: ignore, keep prior state
	}
	if r.IsSidechain || (r.Type != "assistant" && r.Type != "user") {
		return // sidecar or subagent record: not a turn boundary
	}
	blocks := parseBlocks(r)
	switch r.Type {
	case "assistant":
		if name, ok := findBlock(blocks, "tool_use"); ok {
			c.lastTool = name
			c.state = StateRunning // tool pending: its result would be a LATER user record
			return
		}
		c.state = StateNeedsYou
	case "user":
		if _, ok := findBlock(blocks, "tool_result"); ok {
			c.state = StateRunning // claude is consuming the result
			return
		}
		c.state = StateIdle // human prompt; fusion upgrades to Running while streaming
	}
}

func parseBlocks(r record) []block {
	if r.Message == nil {
		return nil
	}
	var bs []block
	// content is either a plain string (user prompt) or a block array
	if err := json.Unmarshal(r.Message.Content, &bs); err != nil {
		return nil
	}
	return bs
}

func findBlock(bs []block, typ string) (name string, ok bool) {
	for _, b := range bs {
		if b.Type == typ {
			return b.Name, true
		}
	}
	return "", false
}

func (c *Classifier) State() State     { return c.state }
func (c *Classifier) LastTool() string { return c.lastTool }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/transcript/ -v`
Expected: PASS (all path + classify tests).

- [ ] **Step 5: Commit**

```bash
git add internal/transcript && git commit -m "feat: streaming turn-boundary classifier (P0: sidecar-tail immune)"
```

---

### Task 8: Incremental transcript reader

**Files:**
- Create: `internal/transcript/reader.go`, `internal/transcript/reader_test.go`

**Interfaces:**
- Consumes: `Classifier` (Task 7).
- Produces:
  - `type Reader struct { /* unexported: path, offset, partial buffer, Classifier */ }`
  - `func NewReader(path string) *Reader`
  - `func (r *Reader) Poll() (State, string, error)` — reads bytes appended since last call, feeds COMPLETE lines to the classifier (buffers a trailing partial line), returns `(state, lastTool, nil)`. Missing file → `(StateUnknown, "", nil)`. Truncated/replaced file (size < offset) → resets offset and classifier, re-reads.

- [ ] **Step 1: Write the failing test**

`internal/transcript/reader_test.go`:
```go
package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReaderMissingFile(t *testing.T) {
	r := NewReader(filepath.Join(t.TempDir(), "nope.jsonl"))
	s, _, err := r.Poll()
	if err != nil || s != StateUnknown {
		t.Fatalf("Poll = %v, %v (want Unknown, nil)", s, err)
	}
}

func TestReaderIncremental(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := NewReader(p)

	f.WriteString(lineUserPrompt + "\n")
	if s, _, _ := r.Poll(); s != StateIdle {
		t.Fatalf("after prompt: %v, want Idle", s)
	}

	f.WriteString(lineAsstToolUse + "\n")
	if s, tool, _ := r.Poll(); s != StateRunning || tool != "Edit" {
		t.Fatalf("after tool_use: %v/%q, want Running/Edit", s, tool)
	}

	// partial line: state must hold until the newline arrives
	half := lineAsstEndTurn[:20]
	f.WriteString(half)
	if s, _, _ := r.Poll(); s != StateRunning {
		t.Fatalf("after partial: %v, want Running (unchanged)", s)
	}
	f.WriteString(lineAsstEndTurn[20:] + "\n")
	if s, _, _ := r.Poll(); s != StateNeedsYou {
		t.Fatalf("after completion: %v, want NeedsYou", s)
	}
}

func TestReaderTruncationResets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(lineUserPrompt+"\n"+lineAsstEndTurn+"\n"), 0o644)
	r := NewReader(p)
	if s, _, _ := r.Poll(); s != StateNeedsYou {
		t.Fatalf("initial: %v", s)
	}
	// replace with a shorter file
	os.WriteFile(p, []byte(lineUserPrompt+"\n"), 0o644)
	if s, _, _ := r.Poll(); s != StateIdle {
		t.Fatalf("after truncate: %v, want Idle (reset+reread)", s)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transcript/ -run Reader -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/transcript/reader.go`:
```go
package transcript

import (
	"bytes"
	"io"
	"os"
)

// Reader incrementally consumes a growing JSONL transcript. Only complete
// newline-terminated lines are parsed (spec §6); a trailing partial line is
// buffered until its newline arrives.
type Reader struct {
	path    string
	offset  int64
	partial []byte
	cls     Classifier
}

func NewReader(path string) *Reader { return &Reader{path: path} }

func (r *Reader) Poll() (State, string, error) {
	f, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return StateUnknown, "", nil // not written yet: fine
		}
		return r.cls.State(), r.cls.LastTool(), err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return r.cls.State(), r.cls.LastTool(), err
	}
	if info.Size() < r.offset {
		// truncated/replaced: start over
		r.offset = 0
		r.partial = nil
		r.cls = Classifier{}
	}
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return r.cls.State(), r.cls.LastTool(), err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return r.cls.State(), r.cls.LastTool(), err
	}
	r.offset += int64(len(data))

	buf := append(r.partial, data...)
	for {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			break
		}
		line := buf[:nl]
		buf = buf[nl+1:]
		if len(bytes.TrimSpace(line)) > 0 {
			r.cls.Feed(line)
		}
	}
	r.partial = buf
	return r.cls.State(), r.cls.LastTool(), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/transcript/ -v` — Expected: PASS (all transcript tests).

- [ ] **Step 5: Commit**

```bash
git add internal/transcript && git commit -m "feat: incremental partial-line-safe transcript reader"
```

---

### Task 9: Trust gate

**Files:**
- Create: `internal/trust/trust.go`, `internal/trust/trust_test.go`

**Interfaces:**
- Consumes: `config.Config.ClaudeJSONPath()` (Task 3) — callers pass the path.
- Produces:
  - `func IsTrusted(claudeJSONPath, cwd string) (bool, error)` — reads `~/.claude.json`, returns `.projects[cwd].hasTrustDialogAccepted == true`. Missing file, missing project entry, or missing flag → `(false, nil)`. Malformed JSON → `(false, err)`.

- [ ] **Step 1: Write the failing test**

`internal/trust/trust_test.go`:
```go
package trust

import (
	"os"
	"path/filepath"
	"testing"
)

func writeJSON(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIsTrusted(t *testing.T) {
	p := writeJSON(t, `{"projects":{
		"/w/trusted":  {"hasTrustDialogAccepted": true},
		"/w/declined": {"hasTrustDialogAccepted": false},
		"/w/other":    {"someOtherKey": 1}
	}}`)
	cases := map[string]bool{
		"/w/trusted":  true,
		"/w/declined": false,
		"/w/other":    false,
		"/w/unknown":  false,
	}
	for cwd, want := range cases {
		got, err := IsTrusted(p, cwd)
		if err != nil {
			t.Fatalf("IsTrusted(%q): %v", cwd, err)
		}
		if got != want {
			t.Errorf("IsTrusted(%q) = %v, want %v", cwd, got, want)
		}
	}
}

func TestIsTrustedMissingFile(t *testing.T) {
	got, err := IsTrusted(filepath.Join(t.TempDir(), "nope.json"), "/w/x")
	if err != nil || got {
		t.Fatalf("= %v, %v (want false, nil)", got, err)
	}
}

func TestIsTrustedMalformed(t *testing.T) {
	p := writeJSON(t, `{not json`)
	if _, err := IsTrusted(p, "/w/x"); err == nil {
		t.Fatal("want error for malformed json")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/trust/ -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/trust/trust.go`:
```go
// Package trust reads claude's per-project trust flags so Loom never fires a
// seed into the first-run trust dialog (spec §3.2 trust gate).
package trust

import (
	"encoding/json"
	"fmt"
	"os"
)

type projectEntry struct {
	HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
}

type claudeJSON struct {
	Projects map[string]projectEntry `json:"projects"`
}

func IsTrusted(claudeJSONPath, cwd string) (bool, error) {
	data, err := os.ReadFile(claudeJSONPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var cj claudeJSON
	if err := json.Unmarshal(data, &cj); err != nil {
		return false, fmt.Errorf("parse %s: %w", claudeJSONPath, err)
	}
	return cj.Projects[cwd].HasTrustDialogAccepted, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/trust/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/trust && git commit -m "feat: trust gate reading ~/.claude.json project flags"
```

---

### Task 10: SQLite store

**Files:**
- Create: `internal/store/store.go`, `internal/store/store_test.go`

**Interfaces:**
- Produces:
  - `type Store struct { /* unexported db */ }` · `func Open(path string) (*Store, error)` · `func (s *Store) Close() error`
  - `type SessionRow struct { Name, ClaudeSessionID, ProjectLabel, Cwd, Model, Mode, Seed, Tags, LastStatus string; CreatedAt, EndedAt, ExitCode int64 }` — `EndedAt`/`ExitCode` are `-1` when unset (avoids sql.Null* leaking into the UI).
  - `func (s *Store) Upsert(r SessionRow) error` — `INSERT ... ON CONFLICT(name) DO UPDATE`.
  - `func (s *Store) SetStatus(name, status string) error`
  - `func (s *Store) MarkEnded(name, status string, exitCode int64, endedAt int64) error` — status `done`/`error`; exitCode `-1` = unknown.
  - `func (s *Store) SetClaudeSessionID(name, id string) error` · `func (s *Store) SetTags(name, tags string) error`
  - `func (s *Store) Get(name string) (SessionRow, bool, error)`
  - `func (s *Store) Live() ([]SessionRow, error)` — `last_status ∈ {running, needs_you, idle, unknown}`, newest first.
  - `func (s *Store) Recent(limit int) ([]SessionRow, error)` — `last_status ∈ {done, error}`, newest-ended first.
  - `func (s *Store) MarkLiveOrphansEnded(liveTmuxNames []string, endedAt int64) error` — every LIVE row whose name is NOT in the list becomes `done` with exitCode `-1` ("ended"). **History is never deleted** (spec §6: prune = retire to history, never lose a resumable row).

- [ ] **Step 1: Write the failing test**

`internal/store/store_test.go`:
```go
package store

import (
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func row(name string) SessionRow {
	return SessionRow{
		Name: name, ClaudeSessionID: name[5:], ProjectLabel: "parallax",
		Cwd: "/w/parallax", Model: "opus", Mode: "plan",
		CreatedAt: 1000, EndedAt: -1, ExitCode: -1, LastStatus: "unknown",
	}
}

func TestUpsertGetRoundtrip(t *testing.T) {
	s := open(t)
	r := row("loom-aaa")
	if err := s.Upsert(r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get("loom-aaa")
	if err != nil || !ok {
		t.Fatalf("Get: %v %v", ok, err)
	}
	if got != r {
		t.Fatalf("got %+v want %+v", got, r)
	}
	// upsert same name updates, no duplicate
	r.Model = "sonnet"
	if err := s.Upsert(r); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Get("loom-aaa")
	if got.Model != "sonnet" {
		t.Fatalf("update lost: %+v", got)
	}
}

func TestLiveRecentAndMarkEnded(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-aaa"))
	s.Upsert(row("loom-bbb"))
	s.SetStatus("loom-aaa", "running")

	live, err := s.Live()
	if err != nil || len(live) != 2 {
		t.Fatalf("Live = %d rows, %v", len(live), err)
	}
	if err := s.MarkEnded("loom-bbb", "error", 3, 2000); err != nil {
		t.Fatal(err)
	}
	live, _ = s.Live()
	if len(live) != 1 || live[0].Name != "loom-aaa" {
		t.Fatalf("Live after end = %+v", live)
	}
	rec, err := s.Recent(10)
	if err != nil || len(rec) != 1 || rec[0].LastStatus != "error" || rec[0].ExitCode != 3 {
		t.Fatalf("Recent = %+v, %v", rec, err)
	}
}

func TestMarkLiveOrphansEnded(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-aaa")) // still in tmux
	s.Upsert(row("loom-bbb")) // vanished from tmux
	s.MarkEnded("loom-ccc-precreate", "done", 0, 500)
	s.Upsert(SessionRow{Name: "loom-ccc", ClaudeSessionID: "ccc", ProjectLabel: "x",
		Cwd: "/x", CreatedAt: 1, EndedAt: 400, ExitCode: 0, LastStatus: "done"}) // history

	if err := s.MarkLiveOrphansEnded([]string{"loom-aaa"}, 3000); err != nil {
		t.Fatal(err)
	}
	live, _ := s.Live()
	if len(live) != 1 || live[0].Name != "loom-aaa" {
		t.Fatalf("Live = %+v (want only loom-aaa)", live)
	}
	// history row untouched (never pruned/re-ended)
	ccc, _, _ := s.Get("loom-ccc")
	if ccc.EndedAt != 400 {
		t.Fatalf("history row mutated: %+v", ccc)
	}
	bbb, _, _ := s.Get("loom-bbb")
	if bbb.LastStatus != "done" || bbb.ExitCode != -1 || bbb.EndedAt != 3000 {
		t.Fatalf("orphan not retired: %+v", bbb)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/store/store.go`:
```go
// Package store owns Loom's SQLite state. Two sources of truth (spec §6):
// tmux for LIVE sessions, this store for HISTORY — terminal rows are never
// deleted by reconciliation.
package store

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

// Open applies the mandatory concurrency pragmas (spec §5): WAL for
// cross-process safety, busy_timeout against SQLITE_BUSY, and a single
// connection so one process never self-contends.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	migrations := []string{
		// v1
		`CREATE TABLE sessions (
			name              TEXT PRIMARY KEY,
			claude_session_id TEXT NOT NULL,
			project_label     TEXT NOT NULL,
			cwd               TEXT NOT NULL,
			model             TEXT NOT NULL DEFAULT '',
			mode              TEXT NOT NULL DEFAULT '',
			seed              TEXT NOT NULL DEFAULT '',
			tags              TEXT NOT NULL DEFAULT '',
			created_at        INTEGER NOT NULL,
			ended_at          INTEGER NOT NULL DEFAULT -1,
			exit_code         INTEGER NOT NULL DEFAULT -1,
			last_status       TEXT NOT NULL DEFAULT 'unknown'
		)`,
	}
	for i := v; i < len(migrations); i++ {
		if _, err := s.db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return err
		}
	}
	return nil
}

type SessionRow struct {
	Name            string
	ClaudeSessionID string
	ProjectLabel    string
	Cwd             string
	Model           string
	Mode            string
	Seed            string
	Tags            string
	CreatedAt       int64
	EndedAt         int64 // -1 = still live
	ExitCode        int64 // -1 = unknown
	LastStatus      string
}

const cols = "name, claude_session_id, project_label, cwd, model, mode, seed, tags, created_at, ended_at, exit_code, last_status"

func (s *Store) Upsert(r SessionRow) error {
	_, err := s.db.Exec(`INSERT INTO sessions (`+cols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			claude_session_id=excluded.claude_session_id,
			project_label=excluded.project_label, cwd=excluded.cwd,
			model=excluded.model, mode=excluded.mode, seed=excluded.seed,
			tags=excluded.tags, created_at=excluded.created_at,
			ended_at=excluded.ended_at, exit_code=excluded.exit_code,
			last_status=excluded.last_status`,
		r.Name, r.ClaudeSessionID, r.ProjectLabel, r.Cwd, r.Model, r.Mode,
		r.Seed, r.Tags, r.CreatedAt, r.EndedAt, r.ExitCode, r.LastStatus)
	return err
}

func (s *Store) SetStatus(name, status string) error {
	_, err := s.db.Exec("UPDATE sessions SET last_status=? WHERE name=?", status, name)
	return err
}

func (s *Store) MarkEnded(name, status string, exitCode, endedAt int64) error {
	_, err := s.db.Exec(
		"UPDATE sessions SET last_status=?, exit_code=?, ended_at=? WHERE name=?",
		status, exitCode, endedAt, name)
	return err
}

func (s *Store) SetClaudeSessionID(name, id string) error {
	_, err := s.db.Exec("UPDATE sessions SET claude_session_id=? WHERE name=?", id, name)
	return err
}

func (s *Store) SetTags(name, tags string) error {
	_, err := s.db.Exec("UPDATE sessions SET tags=? WHERE name=?", tags, name)
	return err
}

func (s *Store) Get(name string) (SessionRow, bool, error) {
	r, err := scanOne(s.db.QueryRow("SELECT "+cols+" FROM sessions WHERE name=?", name))
	if err == sql.ErrNoRows {
		return SessionRow{}, false, nil
	}
	return r, err == nil, err
}

const liveSet = "('running','needs_you','idle','unknown')"

func (s *Store) Live() ([]SessionRow, error) {
	return s.query("SELECT " + cols + " FROM sessions WHERE last_status IN " + liveSet +
		" ORDER BY created_at DESC")
}

func (s *Store) Recent(limit int) ([]SessionRow, error) {
	return s.query(fmt.Sprintf("SELECT "+cols+" FROM sessions WHERE last_status IN ('done','error')"+
		" ORDER BY ended_at DESC LIMIT %d", limit))
}

// MarkLiveOrphansEnded retires live rows with no tmux backing to history as
// 'done' (exit unknown). NEVER deletes — history survives restarts (spec §6).
func (s *Store) MarkLiveOrphansEnded(liveTmuxNames []string, endedAt int64) error {
	placeholders := make([]string, len(liveTmuxNames))
	args := []any{endedAt}
	for i, n := range liveTmuxNames {
		placeholders[i] = "?"
		args = append(args, n)
	}
	q := "UPDATE sessions SET last_status='done', ended_at=? WHERE last_status IN " + liveSet
	if len(liveTmuxNames) > 0 {
		q += " AND name NOT IN (" + strings.Join(placeholders, ",") + ")"
	}
	_, err := s.db.Exec(q, args...)
	return err
}

func (s *Store) query(q string, args ...any) ([]SessionRow, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(&r.Name, &r.ClaudeSessionID, &r.ProjectLabel, &r.Cwd,
			&r.Model, &r.Mode, &r.Seed, &r.Tags, &r.CreatedAt, &r.EndedAt,
			&r.ExitCode, &r.LastStatus); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanOne(row rowScanner) (SessionRow, error) {
	var r SessionRow
	err := row.Scan(&r.Name, &r.ClaudeSessionID, &r.ProjectLabel, &r.Cwd,
		&r.Model, &r.Mode, &r.Seed, &r.Tags, &r.CreatedAt, &r.EndedAt,
		&r.ExitCode, &r.LastStatus)
	return r, err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -v` — Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/store && git commit -m "feat: SQLite store (WAL, migrations, history-safe orphan retirement)"
```

---

### Task 11: Project registry

**Files:**
- Create: `internal/registry/registry.go`, `internal/registry/registry_test.go`

**Interfaces:**
- Consumes: `transcript.ProjectDirName` (Task 6).
- Produces:
  - `type Project struct { Label, Path string }`
  - `func Discover(workspaceRoot, claudeConfigDir string) ([]Project, error)` — non-hidden subdirs of `workspaceRoot` that have a `.git` entry OR an existing claude transcript dir; sorted by Label.

- [ ] **Step 1: Write the failing test**

`internal/registry/registry_test.go`:
```go
package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/henricktissink/loom/internal/transcript"
)

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	ccd := t.TempDir()
	mk := func(parts ...string) {
		if err := os.MkdirAll(filepath.Join(parts...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk(root, "gitproj", ".git")                 // include: has .git
	mk(root, "plaindir")                        // exclude: nothing
	mk(root, ".hidden", ".git")                 // exclude: hidden
	mk(root, "clauded")                         // include: has transcripts
	mk(ccd, "projects", transcript.ProjectDirName(filepath.Join(root, "clauded")))
	os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o644) // exclude: file

	ps, err := Discover(root, ccd)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Label != "clauded" || ps[1].Label != "gitproj" {
		t.Fatalf("Discover = %+v", ps)
	}
	if ps[1].Path != filepath.Join(root, "gitproj") {
		t.Fatalf("Path = %q", ps[1].Path)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/registry/ -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/registry/registry.go`:
```go
// Package registry discovers workspace projects (spec §5).
package registry

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henricktissink/loom/internal/transcript"
)

type Project struct {
	Label string // directory basename, shown in the UI
	Path  string // absolute path, used as cwd
}

// Discover lists workspace subdirs that look like projects: they have .git or
// an existing claude transcript directory.
func Discover(workspaceRoot, claudeConfigDir string) ([]Project, error) {
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return nil, err
	}
	var ps []Project
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(workspaceRoot, e.Name())
		hasGit := false
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			hasGit = true
		}
		hasTranscripts := false
		tdir := filepath.Join(claudeConfigDir, "projects", transcript.ProjectDirName(path))
		if _, err := os.Stat(tdir); err == nil {
			hasTranscripts = true
		}
		if hasGit || hasTranscripts {
			ps = append(ps, Project{Label: e.Name(), Path: path})
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Label < ps[j].Label })
	return ps, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/registry/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry && git commit -m "feat: workspace project registry (git or transcript heuristic)"
```

---

### Task 12: Status fusion + reconcile engine

The heart of the dashboard. `Fuse` is pure logic (unit-tested); `Engine.Poll` is one reconcile pass (integration-tested with a fake claude script in real tmux).

**Files:**
- Create: `internal/status/status.go`, `internal/status/status_test.go`, `internal/status/engine.go`, `internal/status/engine_test.go`

**Interfaces:**
- Consumes: `tmux.Client` (Task 4), `store.Store` (Task 10), `transcript.Reader/State/Path` (Tasks 6–8), `session.SessionIDFromTmuxName` (Task 5).
- Produces:
  - `type Status string` · `const (Running Status = "running"; NeedsYou Status = "needs_you"; Idle Status = "idle"; Done Status = "done"; Error Status = "error"; Unknown Status = "unknown")`
  - `func Fuse(t transcript.State, paneActive bool) Status` — live-session fusion:
    - `StateRunning` → `Running`
    - `StateNeedsYou` → `NeedsYou`
    - `StateIdle` + paneActive → `Running` (claude is streaming; JSONL just hasn't caught up)
    - `StateIdle` → `Idle`
    - `StateUnknown` + paneActive → `Running`; else `Unknown`
  - `type Row struct { store.SessionRow; Status Status; LastTool string }`
  - `type Snapshot struct { Live []Row; Recent []store.SessionRow }`
  - `type Engine struct { /* unexported: tmux, store, claudeConfigDir, readers map[string]*transcript.Reader, lastActivity map[string]int64 */ }`
  - `func NewEngine(tm *tmux.Client, st *store.Store, claudeConfigDir string) *Engine`
  - `func (e *Engine) Poll(now time.Time) (Snapshot, error)` — one full reconcile pass:
    1. `tm.ListSessions()`; keep only `loom-*` names.
    2. Adopt orphans: tmux session with no store row → `Upsert` a row (label = basename of `PaneStatus.CurrentPath`, cwd = CurrentPath, claude_session_id from the name).
    3. Dead panes: `PaneStatus.Dead` → `MarkEnded(done|error by exit code)` + `KillSession` (reap).
    4. `MarkLiveOrphansEnded(aliveNames)` — store rows that vanished from tmux retire to history.
    5. For each alive session: transcript `Reader.Poll()` (readers cached per name) + `paneActive = now-Activity ≤ 3s` → `Fuse` → `SetStatus` → `Row`.
    6. `Recent(10)` from store.

- [ ] **Step 1: Write the failing fusion unit test**

`internal/status/status_test.go`:
```go
package status

import (
	"testing"

	"github.com/henricktissink/loom/internal/transcript"
)

func TestFuse(t *testing.T) {
	cases := []struct {
		ts     transcript.State
		active bool
		want   Status
	}{
		{transcript.StateRunning, false, Running},
		{transcript.StateRunning, true, Running},
		{transcript.StateNeedsYou, false, NeedsYou},
		{transcript.StateNeedsYou, true, NeedsYou},
		{transcript.StateIdle, true, Running}, // streaming: JSONL lags the pane
		{transcript.StateIdle, false, Idle},
		{transcript.StateUnknown, true, Running},
		{transcript.StateUnknown, false, Unknown},
	}
	for _, c := range cases {
		if got := Fuse(c.ts, c.active); got != c.want {
			t.Errorf("Fuse(%v, %v) = %v, want %v", c.ts, c.active, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Write the failing engine integration test**

`internal/status/engine_test.go`:
```go
package status

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
)

const fakeTranscript = `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"yo"}],"stop_reason":"end_turn"}}
{"type":"permission-mode","permissionMode":"default","sessionId":"x"}
`

func testEnv(t *testing.T) (*tmux.Client, *store.Store, string) {
	t.Helper()
	tm := &tmux.Client{Socket: fmt.Sprintf("loomeng%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return tm, st, t.TempDir() // third value = fake CLAUDE_CONFIG_DIR
}

// fakeClaude writes a transcript like claude would, then idles.
func launchFake(t *testing.T, tm *tmux.Client, ccd, cwd, id string) string {
	t.Helper()
	tpath := transcript.Path(ccd, cwd, id)
	if err := os.MkdirAll(filepath.Dir(tpath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tpath, []byte(fakeTranscript), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "loom-" + id
	if err := tm.NewSession(name, cwd, "sleep 60", 80, 24); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestPollLiveNeedsYou(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	name := launchFake(t, tm, ccd, cwd, id)
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id, ProjectLabel: "p",
		Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "unknown"})

	e := NewEngine(tm, st, ccd)
	// far-future "now" so session_activity from creation doesn't read as active
	snap, err := e.Poll(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Live) != 1 {
		t.Fatalf("Live = %+v", snap.Live)
	}
	// sidecar tail must not break NeedsYou (P0 guard at engine level)
	if snap.Live[0].Status != NeedsYou {
		t.Fatalf("Status = %v, want NeedsYou", snap.Live[0].Status)
	}
}

func TestPollDeadPaneClassifiesAndReaps(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	name := "loom-" + id
	if err := tm.NewSession(name, cwd, "exit 4", 80, 24); err != nil {
		t.Fatal(err)
	}
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id, ProjectLabel: "p",
		Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "running"})

	e := NewEngine(tm, st, ccd)
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, err := e.Poll(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Recent) == 1 {
			r := snap.Recent[0]
			if r.LastStatus != "error" || r.ExitCode != 4 {
				t.Fatalf("Recent = %+v (want error/4)", r)
			}
			if tm.HasSession(name) {
				t.Fatal("dead session not reaped")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never classified; snap=%+v", snap)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func TestPollAdoptsOrphanAndRetiresVanished(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	launchFake(t, tm, ccd, cwd, id) // tmux session, NO store row → adopt
	st.Upsert(store.SessionRow{Name: "loom-gone", ClaudeSessionID: "gone",
		ProjectLabel: "p", Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1,
		LastStatus: "running"}) // store row, NO tmux → retire

	e := NewEngine(tm, st, ccd)
	if _, err := e.Poll(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	adopted, ok, _ := st.Get("loom-" + id)
	if !ok || adopted.ClaudeSessionID != id {
		t.Fatalf("orphan not adopted: %+v %v", adopted, ok)
	}
	gone, _, _ := st.Get("loom-gone")
	if gone.LastStatus != "done" {
		t.Fatalf("vanished row not retired: %+v", gone)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/status/ -v` — Expected: FAIL (undefined symbols).

- [ ] **Step 4: Implement**

`internal/status/status.go`:
```go
// Package status fuses transcript state with tmux liveness (spec §4.3, §6).
package status

import "github.com/henricktissink/loom/internal/transcript"

type Status string

const (
	Running  Status = "running"
	NeedsYou Status = "needs_you"
	Idle     Status = "idle"
	Done     Status = "done"
	Error    Status = "error"
	Unknown  Status = "unknown"
)

// Fuse combines the JSONL turn-boundary state with pane activity. Best-effort
// by design (spec §6): wrong fusion degrades a status label, never a session.
func Fuse(t transcript.State, paneActive bool) Status {
	switch t {
	case transcript.StateRunning:
		return Running
	case transcript.StateNeedsYou:
		return NeedsYou
	case transcript.StateIdle:
		if paneActive {
			return Running // streaming: the pane is moving, JSONL lags
		}
		return Idle
	default:
		if paneActive {
			return Running
		}
		return Unknown
	}
}
```

`internal/status/engine.go`:
```go
package status

import (
	"path/filepath"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
)

const activeWindow = 3 * time.Second

type Row struct {
	store.SessionRow
	Status   Status
	LastTool string
}

type Snapshot struct {
	Live   []Row
	Recent []store.SessionRow
}

// Engine performs one reconcile pass per Poll. tmux is the source of truth
// for live sessions; the store owns history (spec §6).
type Engine struct {
	tm      *tmux.Client
	st      *store.Store
	ccd     string
	readers map[string]*transcript.Reader
}

func NewEngine(tm *tmux.Client, st *store.Store, claudeConfigDir string) *Engine {
	return &Engine{tm: tm, st: st, ccd: claudeConfigDir,
		readers: map[string]*transcript.Reader{}}
}

func (e *Engine) Poll(now time.Time) (Snapshot, error) {
	sessions, err := e.tm.ListSessions()
	if err != nil {
		return Snapshot{}, err
	}

	var aliveNames []string
	activity := map[string]int64{}
	for _, s := range sessions {
		if _, ok := session.SessionIDFromTmuxName(s.Name); !ok {
			continue // not ours
		}
		ps, err := e.tm.PaneStatus(s.Name)
		if err != nil {
			continue // raced a kill; next poll settles it
		}
		if ps.Dead {
			st := "done"
			if ps.ExitCode != 0 {
				st = "error"
			}
			_ = e.st.MarkEnded(s.Name, st, int64(ps.ExitCode), now.Unix())
			_ = e.tm.KillSession(s.Name) // reap after recording (spec §6)
			delete(e.readers, s.Name)
			continue
		}
		if _, ok, _ := e.st.Get(s.Name); !ok {
			// adopt orphan: rebuild what we can from tmux alone (spec §3)
			id, _ := session.SessionIDFromTmuxName(s.Name)
			_ = e.st.Upsert(store.SessionRow{
				Name: s.Name, ClaudeSessionID: id,
				ProjectLabel: filepath.Base(ps.CurrentPath), Cwd: ps.CurrentPath,
				CreatedAt: now.Unix(), EndedAt: -1, ExitCode: -1,
				LastStatus: string(Unknown),
			})
		}
		aliveNames = append(aliveNames, s.Name)
		activity[s.Name] = s.Activity
	}

	// store rows that claim live but have no tmux backing → history (never deleted)
	if err := e.st.MarkLiveOrphansEnded(aliveNames, now.Unix()); err != nil {
		return Snapshot{}, err
	}

	liveRows, err := e.st.Live()
	if err != nil {
		return Snapshot{}, err
	}
	var live []Row
	for _, r := range liveRows {
		rd, ok := e.readers[r.Name]
		if !ok {
			rd = transcript.NewReader(transcript.Path(e.ccd, r.Cwd, r.ClaudeSessionID))
			e.readers[r.Name] = rd
		}
		ts, tool, _ := rd.Poll() // read errors degrade to prior state: best-effort
		paneActive := now.Unix()-activity[r.Name] <= int64(activeWindow/time.Second)
		st := Fuse(ts, paneActive)
		if string(st) != r.LastStatus {
			_ = e.st.SetStatus(r.Name, string(st))
			r.LastStatus = string(st)
		}
		live = append(live, Row{SessionRow: r, Status: st, LastTool: tool})
	}

	recent, err := e.st.Recent(10)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Live: live, Recent: recent}, nil
}

// GC drops cached readers for names not in the given set (call rarely; cheap).
func (e *Engine) GC(names []string) {
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	for n := range e.readers {
		if !keep[n] {
			delete(e.readers, n)
		}
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/status/ -v`
Expected: PASS (fusion table + 3 engine integration tests; ~8s).

- [ ] **Step 6: Commit**

```bash
git add internal/status && git commit -m "feat: status fusion and tmux/store reconcile engine"
```

---

### Task 13: Launch orchestration (gated seed, resume)

**Files:**
- Create: `internal/session/launch.go`, `internal/session/launch_test.go`

**Interfaces:**
- Consumes: `tmux.Client` (Task 4), `store.Store` (Task 10), `trust.IsTrusted` (Task 9), `transcript.Path/NewestSince` (Task 6), `Recipe/TmuxName/NewSessionID/ResumeShellCommand` (Task 5).
- Produces:
  - `type Launcher struct { Tmux *tmux.Client; Store *store.Store; ClaudeConfigDir, ClaudeJSONPath string; ReadyMarker, TrustMarker string; ReadyTimeout time.Duration; PollEvery time.Duration }`
  - Marker defaults (UPDATE FROM SPIKE FINDINGS, Task 2): `DefaultReadyMarker = "? for shortcuts"`, `DefaultTrustMarker = "Do you trust the files in this folder?"`.
  - `func (l *Launcher) Launch(r Recipe, w, h int, now time.Time) (name string, err error)` — creates the tmux session + store row (status `unknown`); if `r.Seed != ""` starts `go l.seedWhenReady(name, r.Seed, trusted)`.
  - `func (l *Launcher) Resume(old store.SessionRow, w, h int, now time.Time) (string, error)` — new uuid/tmux name, command `ResumeShellCommand(old.ClaudeSessionID)`, new store row (Model/Mode/Tags copied, `Seed` empty); starts a goroutine that, once ready, corrects `claude_session_id` via `transcript.NewestSince(ccd, cwd, launchTime)` (claude --resume mints a NEW id — spike Task 2 Step 4).
  - `func (l *Launcher) seedWhenReady(name, seed string, trusted bool) ` — polls `CapturePane` every `PollEvery`; while `TrustMarker` visible, keeps waiting (no timeout clock while trust is pending — the user must attach and answer); once `ReadyMarker` visible → `SendLiteral(seed)` + `SendEnter`. Gives up silently after `ReadyTimeout` of non-trust waiting (status stays best-effort; the session itself is unharmed).

- [ ] **Step 1: Write the failing test**

`internal/session/launch_test.go` (uses a fake-claude shell script so no real claude is needed):
```go
package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

// fakeClaudeScript prints a trust dialog for 1s, then the ready marker, then cats input.
const fakeClaudeScript = `#!/bin/sh
echo "Do you trust the files in this folder?"
sleep 1
clear 2>/dev/null || printf '\033[2J'
echo "? for shortcuts"
exec cat > "$1"
`

func testLauncher(t *testing.T) (*Launcher, string) {
	t.Helper()
	tm := &tmux.Client{Socket: fmt.Sprintf("loomln%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	l := &Launcher{
		Tmux: tm, Store: st,
		ClaudeConfigDir: t.TempDir(),
		ClaudeJSONPath:  filepath.Join(t.TempDir(), ".claude.json"),
		ReadyMarker:     DefaultReadyMarker,
		TrustMarker:     DefaultTrustMarker,
		ReadyTimeout:    10 * time.Second,
		PollEvery:       100 * time.Millisecond,
	}
	return l, dir
}

func TestLaunchCreatesSessionAndRow(t *testing.T) {
	l, dir := testLauncher(t)
	r := Recipe{ProjectLabel: "p", Cwd: dir, Model: "opus", Mode: "plan"}
	name, err := l.Launch(r, 120, 40, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !l.Tmux.HasSession(name) {
		t.Fatal("no tmux session")
	}
	row, ok, _ := l.Store.Get(name)
	if !ok || row.Model != "opus" || row.Mode != "plan" || row.LastStatus != "unknown" {
		t.Fatalf("row = %+v %v", row, ok)
	}
	id, _ := SessionIDFromTmuxName(name)
	if row.ClaudeSessionID != id {
		t.Fatalf("id mismatch: %q vs %q", row.ClaudeSessionID, id)
	}
}

// The launch command is what the recipe says — verified via a stub command.
func TestSeedWaitsForTrustThenReady(t *testing.T) {
	l, dir := testLauncher(t)
	sink := filepath.Join(dir, "sink.txt")
	script := filepath.Join(dir, "fake-claude.sh")
	os.WriteFile(script, []byte(fakeClaudeScript), 0o755)

	// launch manually with the fake command, then drive seedWhenReady directly
	id := NewSessionID()
	name := TmuxName(id)
	if err := l.Tmux.NewSession(name, dir, "'"+script+"' '"+sink+"'", 80, 24); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { l.seedWhenReady(name, "hello seed", false); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("seed goroutine never finished")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(sink)
		if string(b) == "hello seed\n" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink = %q, want seed after ready marker", b)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestResumeCreatesFreshTmuxSession(t *testing.T) {
	l, dir := testLauncher(t)
	old := store.SessionRow{Name: "loom-old", ClaudeSessionID: "dddddddd-dddd-dddd-dddd-dddddddddddd",
		ProjectLabel: "p", Cwd: dir, Model: "opus", CreatedAt: 1, EndedAt: 5, ExitCode: 0, LastStatus: "done"}
	l.Store.Upsert(old)
	name, err := l.Resume(old, 80, 24, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if name == "loom-old" {
		t.Fatal("resume must mint a fresh tmux name")
	}
	if !l.Tmux.HasSession(name) {
		t.Fatal("no tmux session (claude missing is fine — the pane may die, but the session was created)")
	}
	row, ok, _ := l.Store.Get(name)
	if !ok || row.Model != "opus" || row.Cwd != dir {
		t.Fatalf("row = %+v %v", row, ok)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/session/ -run 'Launch|Seed|Resume' -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/session/launch.go`:
```go
package session

import (
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
	"github.com/henricktissink/loom/internal/trust"
)

// Markers verified by the Task-2 spike against claude 2.1.198. If the spike
// recorded different strings, change THESE constants, nothing else.
const (
	DefaultReadyMarker = "? for shortcuts"
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
		trusted, _ := trust.IsTrusted(l.ClaudeJSONPath, r.Cwd)
		go l.seedWhenReady(name, r.Seed, trusted)
	}
	return name, nil
}

// Resume relaunches a finished session via `claude --resume`. claude mints a
// NEW session id for the resumed conversation, so after readiness we correct
// claude_session_id from the newest transcript (spike Task 2 / spec §11).
func (l *Launcher) Resume(old store.SessionRow, w, h int, now time.Time) (string, error) {
	id := NewSessionID() // for the tmux name only
	name := TmuxName(id)
	if err := l.Tmux.NewSession(name, old.Cwd, ResumeShellCommand(old.ClaudeSessionID), w, h); err != nil {
		return "", err
	}
	if err := l.Store.Upsert(store.SessionRow{
		Name: name, ClaudeSessionID: old.ClaudeSessionID, // corrected below
		ProjectLabel: old.ProjectLabel, Cwd: old.Cwd,
		Model: old.Model, Mode: old.Mode, Tags: old.Tags,
		CreatedAt: now.Unix(), EndedAt: -1, ExitCode: -1,
		LastStatus: "unknown",
	}); err != nil {
		return name, err
	}
	go func() {
		if !l.waitReady(name, true) {
			return
		}
		if newID, err := transcript.NewestSince(l.ClaudeConfigDir, old.Cwd, now.Add(-time.Second)); err == nil && newID != "" {
			_ = l.Store.SetClaudeSessionID(name, newID)
		}
	}()
	return name, nil
}

// waitReady polls the pane until the ready marker shows. While the trust
// dialog is visible the timeout clock is paused — the user must attach and
// answer it; we never answer for them (spec §3.2 trust gate).
func (l *Launcher) waitReady(name string, trusted bool) bool {
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

func (l *Launcher) seedWhenReady(name, seed string, trusted bool) {
	if !l.waitReady(name, trusted) {
		return // best-effort: session unharmed, seed skipped
	}
	if err := l.Tmux.SendLiteral(name, seed); err != nil {
		return
	}
	_ = l.Tmux.SendEnter(name)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/session/ -v` — Expected: PASS (recipe + launch tests; ~5s).

- [ ] **Step 5: Commit**

```bash
git add internal/session && git commit -m "feat: orchestrated launch with trust-aware gated seeding and resume"
```

---

### Task 14: TUI — styles, launcher form, dashboard app

**Files:**
- Create: `internal/ui/styles.go`, `internal/ui/launcher.go`, `internal/ui/app.go`, `internal/ui/app_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces:
  - `func NewApp(deps Deps) *App` where `type Deps struct { Engine *status.Engine; Launcher *session.Launcher; Projects []registry.Project; Tmux *tmux.Client; InsideTmux bool }`
  - `App` implements `tea.Model`. Keys: `j/k/↑/↓` move · `↵` attach · `n` launcher · `x`+`y` kill · `t` tag · `r` resume (Recent row) · `q` quit.
  - Internal messages: `type snapMsg status.Snapshot`, `type tickMsg time.Time`, `type errMsg struct{ err error }`, `type attachedMsg struct{ err error }`.

- [ ] **Step 1: Write the failing Update-logic test**

`internal/ui/app_test.go`:
```go
package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

func fixtureApp() *App {
	a := NewApp(Deps{})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{
		Live: []status.Row{
			{SessionRow: store.SessionRow{Name: "loom-b", ProjectLabel: "tavli"}, Status: status.NeedsYou},
			{SessionRow: store.SessionRow{Name: "loom-a", ProjectLabel: "parallax"}, Status: status.Running, LastTool: "Edit"},
			{SessionRow: store.SessionRow{Name: "loom-c", ProjectLabel: "volar"}, Status: status.Idle},
		},
		Recent: []store.SessionRow{
			{Name: "loom-d", ProjectLabel: "gloom", LastStatus: "done"},
		},
	}
	a.rebuildRows()
	return a
}

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// Attention queue: NeedsYou floats above Running above Idle; Recent last.
func TestRowOrdering(t *testing.T) {
	a := fixtureApp()
	got := make([]string, len(a.rows))
	for i, r := range a.rows {
		got[i] = r.name
	}
	want := []string{"loom-b", "loom-a", "loom-c", "loom-d"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestCursorMovesAndClamps(t *testing.T) {
	a := fixtureApp()
	a.Update(key("j"))
	a.Update(key("j"))
	if a.cursor != 2 {
		t.Fatalf("cursor = %d, want 2", a.cursor)
	}
	for i := 0; i < 10; i++ {
		a.Update(key("j"))
	}
	if a.cursor != 3 {
		t.Fatalf("cursor clamped = %d, want 3", a.cursor)
	}
	for i := 0; i < 10; i++ {
		a.Update(key("k"))
	}
	if a.cursor != 0 {
		t.Fatalf("cursor floor = %d, want 0", a.cursor)
	}
}

func TestNOpensLauncherAndEscCloses(t *testing.T) {
	a := fixtureApp()
	a.Update(key("n"))
	if a.view != viewLauncher {
		t.Fatalf("view = %v, want launcher", a.view)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewDash {
		t.Fatalf("view = %v, want dash after esc", a.view)
	}
}

func TestKillNeedsConfirm(t *testing.T) {
	a := fixtureApp()
	a.Update(key("x"))
	if a.view != viewConfirmKill {
		t.Fatalf("view = %v, want confirm", a.view)
	}
	a.Update(key("n")) // decline
	if a.view != viewDash {
		t.Fatalf("view = %v, want dash after decline", a.view)
	}
}

func TestViewRendersSections(t *testing.T) {
	a := fixtureApp()
	out := a.View()
	for _, want := range []string{"NEEDS YOU", "RUNNING", "IDLE", "RECENT", "parallax", "tavli", "Edit"} {
		if !contains(out, want) {
			t.Fatalf("View() missing %q:\n%s", want, out)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool { return indexOf(s, sub) >= 0 }())
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ui/ -v` — Expected: FAIL.

- [ ] **Step 3: Implement styles**

`internal/ui/styles.go`:
```go
// Package ui is Loom's Bubble Tea TUI: the home dashboard and launcher.
package ui

import "github.com/charmbracelet/lipgloss"

var (
	styTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("219"))
	stySection  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	styCursor   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("219"))
	styNeedsYou = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styRunning  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styIdle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styDone     = lipgloss.NewStyle().Foreground(lipgloss.Color("71"))
	styErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	styHelp     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styMeta     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func statusIcon(status string) string {
	switch status {
	case "needs_you":
		return styNeedsYou.Render("●")
	case "running":
		return styRunning.Render("◐")
	case "idle":
		return styIdle.Render("○")
	case "done":
		return styDone.Render("✓")
	case "error":
		return styErr.Render("✗")
	default:
		return styIdle.Render("·")
	}
}
```

- [ ] **Step 4: Implement the launcher form**

`internal/ui/launcher.go`:
```go
package ui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
)

var (
	modelOptions = []string{"", "opus", "sonnet", "fable"}
	modeOptions  = []string{"", "plan", "acceptEdits", "auto", "bypassPermissions"}
)

func optLabel(v string) string {
	if v == "" {
		return "default"
	}
	return v
}

// launcherForm is a minimal 4-field form: project / model / mode / seed.
// tab moves fields, ←/→ cycle selects, enter submits, esc cancels.
type launcherForm struct {
	projects   []registry.Project
	projIdx    int
	modelIdx   int
	modeIdx    int
	seed       textinput.Model
	focus      int // 0=project 1=model 2=mode 3=seed
}

func newLauncherForm(projects []registry.Project) launcherForm {
	ti := textinput.New()
	ti.Placeholder = "optional seed prompt or /slash-command"
	ti.CharLimit = 500
	return launcherForm{projects: projects, seed: ti}
}

func (f *launcherForm) Recipe() (session.Recipe, bool) {
	if len(f.projects) == 0 {
		return session.Recipe{}, false
	}
	p := f.projects[f.projIdx]
	return session.Recipe{
		ProjectLabel: p.Label,
		Cwd:          p.Path,
		Model:        modelOptions[f.modelIdx],
		Mode:         modeOptions[f.modeIdx],
		Seed:         f.seed.Value(),
	}, true
}

func cycle(idx, delta, n int) int { return ((idx+delta)%n + n) % n }

func (f *launcherForm) update(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyTab, tea.KeyDown:
		f.setFocus(cycle(f.focus, 1, 4))
		return nil
	case tea.KeyShiftTab, tea.KeyUp:
		f.setFocus(cycle(f.focus, -1, 4))
		return nil
	case tea.KeyLeft, tea.KeyRight:
		d := 1
		if msg.Type == tea.KeyLeft {
			d = -1
		}
		switch f.focus {
		case 0:
			if n := len(f.projects); n > 0 {
				f.projIdx = cycle(f.projIdx, d, n)
			}
		case 1:
			f.modelIdx = cycle(f.modelIdx, d, len(modelOptions))
		case 2:
			f.modeIdx = cycle(f.modeIdx, d, len(modeOptions))
		}
		return nil
	}
	if f.focus == 3 {
		var cmd tea.Cmd
		f.seed, cmd = f.seed.Update(msg)
		return cmd
	}
	return nil
}

func (f *launcherForm) setFocus(n int) {
	f.focus = n
	if n == 3 {
		f.seed.Focus()
	} else {
		f.seed.Blur()
	}
}

func (f *launcherForm) view() string {
	sel := func(i int, label, val string) string {
		marker := "  "
		if f.focus == i {
			marker = styCursor.Render("▸ ")
		}
		return fmt.Sprintf("%s%-9s ‹ %s ›", marker, label, val)
	}
	proj := "(no projects found)"
	if len(f.projects) > 0 {
		proj = f.projects[f.projIdx].Label
	}
	seedMarker := "  "
	if f.focus == 3 {
		seedMarker = styCursor.Render("▸ ")
	}
	return styTitle.Render("new session") + "\n\n" +
		sel(0, "project", proj) + "\n" +
		sel(1, "model", optLabel(modelOptions[f.modelIdx])) + "\n" +
		sel(2, "mode", optLabel(modeOptions[f.modeIdx])) + "\n" +
		seedMarker + "seed      " + f.seed.View() + "\n\n" +
		styHelp.Render("tab/↑↓ field · ←/→ value · enter launch · esc cancel")
}
```

- [ ] **Step 5: Implement the app model**

`internal/ui/app.go`:
```go
package ui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

const pollInterval = 1500 * time.Millisecond

type view int

const (
	viewDash view = iota
	viewLauncher
	viewConfirmKill
	viewTag
)

type Deps struct {
	Engine     *status.Engine
	Launcher   *session.Launcher
	Projects   []registry.Project
	Tmux       *tmux.Client
	InsideTmux bool
}

// uiRow is one selectable dashboard line (live or recent).
type uiRow struct {
	name     string
	label    string
	status   string
	lastTool string
	model    string
	mode     string
	recent   bool
	row      store.SessionRow
}

type App struct {
	deps   Deps
	snap   status.Snapshot
	rows   []uiRow
	cursor int
	view   view
	form   launcherForm
	tag    textinput.Model
	errStr string
	width  int
	height int
}

type (
	tickMsg     time.Time
	snapMsg     status.Snapshot
	errMsg      struct{ err error }
	attachedMsg struct{ err error }
)

func NewApp(deps Deps) *App {
	ti := textinput.New()
	ti.Placeholder = "tags (comma separated)"
	return &App{deps: deps, form: newLauncherForm(deps.Projects), tag: ti}
}

func (a *App) Init() tea.Cmd { return tea.Batch(a.pollCmd(), tickAfter()) }

func tickAfter() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (a *App) pollCmd() tea.Cmd {
	eng := a.deps.Engine
	if eng == nil {
		return nil
	}
	return func() tea.Msg {
		snap, err := eng.Poll(time.Now())
		if err != nil {
			return errMsg{err}
		}
		return snapMsg(snap)
	}
}

// rebuildRows flattens the snapshot into the attention-queue order:
// NeedsYou → Running → Idle/Unknown → Recent (spec §4.1).
func (a *App) rebuildRows() {
	var needs, running, idle, recent []uiRow
	for _, r := range a.snap.Live {
		u := uiRow{name: r.Name, label: r.ProjectLabel, status: string(r.Status),
			lastTool: r.LastTool, model: r.Model, mode: r.Mode, row: r.SessionRow}
		switch r.Status {
		case status.NeedsYou:
			needs = append(needs, u)
		case status.Running:
			running = append(running, u)
		default:
			idle = append(idle, u)
		}
	}
	for _, r := range a.snap.Recent {
		recent = append(recent, uiRow{name: r.Name, label: r.ProjectLabel,
			status: r.LastStatus, model: r.Model, mode: r.Mode, recent: true, row: r})
	}
	a.rows = append(append(append(needs, running...), idle...), recent...)
	if a.cursor >= len(a.rows) {
		a.cursor = max(0, len(a.rows)-1)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = m.Width, m.Height
		return a, nil
	case tickMsg:
		return a, tea.Batch(a.pollCmd(), tickAfter())
	case snapMsg:
		a.snap = status.Snapshot(m)
		a.errStr = ""
		a.rebuildRows()
		return a, nil
	case errMsg:
		a.errStr = m.err.Error()
		return a, nil
	case attachedMsg:
		if m.err != nil {
			a.errStr = "attach failed: " + m.err.Error()
		}
		return a, a.pollCmd()
	case tea.KeyMsg:
		return a.updateKeys(m)
	}
	return a, nil
}

func (a *App) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch a.view {
	case viewLauncher:
		switch msg.Type {
		case tea.KeyEsc:
			a.view = viewDash
			return a, nil
		case tea.KeyEnter:
			r, ok := a.form.Recipe()
			a.view = viewDash
			if !ok || a.deps.Launcher == nil {
				return a, nil
			}
			l := a.deps.Launcher
			w, h := a.width, a.height
			return a, func() tea.Msg {
				if _, err := l.Launch(r, w, h, time.Now()); err != nil {
					return errMsg{err}
				}
				return tickMsg(time.Now())
			}
		}
		return a, a.form.update(msg)

	case viewConfirmKill:
		s := msg.String()
		if s == "y" {
			a.view = viewDash
			if r, ok := a.selected(); ok && a.deps.Tmux != nil {
				name := r.name
				tm := a.deps.Tmux
				return a, func() tea.Msg {
					if err := tm.KillSession(name); err != nil {
						return errMsg{err}
					}
					return tickMsg(time.Now())
				}
			}
			return a, nil
		}
		if s == "n" || msg.Type == tea.KeyEsc {
			a.view = viewDash
		}
		return a, nil

	case viewTag:
		switch msg.Type {
		case tea.KeyEsc:
			a.view = viewDash
			return a, nil
		case tea.KeyEnter:
			a.view = viewDash
			if r, ok := a.selected(); ok && a.deps.Launcher != nil {
				_ = a.deps.Launcher.Store.SetTags(r.name, a.tag.Value())
			}
			return a, a.pollCmd()
		}
		var cmd tea.Cmd
		a.tag, cmd = a.tag.Update(msg)
		return a, cmd
	}

	// viewDash
	switch msg.String() {
	case "q", "ctrl+c":
		return a, tea.Quit
	case "j", "down":
		if a.cursor < len(a.rows)-1 {
			a.cursor++
		}
	case "k", "up":
		if a.cursor > 0 {
			a.cursor--
		}
	case "n":
		a.form = newLauncherForm(a.deps.Projects)
		a.form.setFocus(0)
		a.view = viewLauncher
	case "x":
		if _, ok := a.selected(); ok {
			a.view = viewConfirmKill
		}
	case "t":
		if r, ok := a.selected(); ok {
			a.tag.SetValue(r.row.Tags)
			a.tag.Focus()
			a.view = viewTag
		}
	case "r":
		if r, ok := a.selected(); ok && r.recent && a.deps.Launcher != nil {
			l := a.deps.Launcher
			old := r.row
			w, h := a.width, a.height
			return a, func() tea.Msg {
				if _, err := l.Resume(old, w, h, time.Now()); err != nil {
					return errMsg{err}
				}
				return tickMsg(time.Now())
			}
		}
	case "enter":
		if r, ok := a.selected(); ok && !r.recent && a.deps.Tmux != nil {
			cmd := a.deps.Tmux.AttachCmd(r.name)
			return a, tea.ExecProcess(cmd, func(err error) tea.Msg { return attachedMsg{err} })
		}
	}
	return a, nil
}

func (a *App) selected() (uiRow, bool) {
	if a.cursor < 0 || a.cursor >= len(a.rows) {
		return uiRow{}, false
	}
	return a.rows[a.cursor], true
}

func (a *App) View() string {
	switch a.view {
	case viewLauncher:
		return a.form.view()
	case viewConfirmKill:
		r, _ := a.selected()
		return fmt.Sprintf("kill %s (%s)? %s",
			r.label, r.name, styHelp.Render("y/n"))
	case viewTag:
		return styTitle.Render("tags") + "\n\n" + a.tag.View() + "\n\n" +
			styHelp.Render("enter save · esc cancel")
	}

	live, needs := 0, 0
	for _, r := range a.snap.Live {
		live++
		if r.Status == status.NeedsYou {
			needs++
		}
	}
	out := styTitle.Render("LOOM") +
		styMeta.Render(fmt.Sprintf("   %d live · %d needs you", live, needs)) + "\n\n"

	section := ""
	for i, r := range a.rows {
		sec := sectionFor(r)
		if sec != section {
			section = sec
			out += stySection.Render(section) + "\n"
		}
		cursor := "  "
		if i == a.cursor {
			cursor = styCursor.Render("▸ ")
		}
		hint := r.lastTool
		if hint != "" {
			hint = "⏺ " + hint
		}
		out += fmt.Sprintf("%s%s %-14s %-18s %s\n",
			cursor, statusIcon(r.status), r.label, hint,
			styMeta.Render(trimMeta(r.model, r.mode)))
	}
	if len(a.rows) == 0 {
		out += styHelp.Render("no sessions — press n to launch one") + "\n"
	}
	if a.errStr != "" {
		out += "\n" + styErr.Render("! "+a.errStr) + "\n"
	}
	if a.deps.InsideTmux {
		out += styHelp.Render("(running inside tmux — attach opens a nested client; F12 detaches)") + "\n"
	}
	out += "\n" + styHelp.Render("[↵]attach [n]ew [x]kill [t]ag [r]eopen [q]uit  ·  [/]search·soon [w]orkflows·soon")
	return out
}

func sectionFor(r uiRow) string {
	if r.recent {
		return "RECENT"
	}
	switch r.status {
	case "needs_you":
		return "NEEDS YOU"
	case "running":
		return "RUNNING"
	default:
		return "IDLE"
	}
}

func trimMeta(model, mode string) string {
	if model == "" {
		model = "default"
	}
	if mode == "" {
		mode = "normal"
	}
	return model + " · " + mode
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/ui/ -v` — Expected: PASS (6 tests).

- [ ] **Step 7: Commit**

```bash
git add internal/ui && git commit -m "feat: dashboard TUI with attention queue, launcher form, attach/kill/tag/resume"
```

---

### Task 15: Wiring, README, end-to-end verification

**Files:**
- Modify: `cmd/loom/main.go` (replace the Task-1 stub)
- Create: `README.md`

**Interfaces:**
- Consumes: everything.
- Produces: the `loom` binary.

- [ ] **Step 1: Implement main.go**

```go
// Loom — a terminal control center for claude sessions.
package main

import (
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/henricktissink/loom/internal/config"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/ui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loom:", err)
		os.Exit(1)
	}
}

func run() error {
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

	deps := ui.Deps{
		Engine: status.NewEngine(tm, st, cfg.ClaudeConfigDir),
		Launcher: &session.Launcher{
			Tmux: tm, Store: st,
			ClaudeConfigDir: cfg.ClaudeConfigDir,
			ClaudeJSONPath:  cfg.ClaudeJSONPath(),
			ReadyMarker:     session.DefaultReadyMarker,
			TrustMarker:     session.DefaultTrustMarker,
			ReadyTimeout:    60 * time.Second,
			PollEvery:       500 * time.Millisecond,
		},
		Projects:   projects,
		Tmux:       tm,
		InsideTmux: config.InsideTmux(),
	}
	p := tea.NewProgram(ui.NewApp(deps), tea.WithAltScreen())
	_, err = p.Run()
	return err
}
```

- [ ] **Step 2: Full build + test sweep**

Run: `go vet ./... && go test ./...`
Expected: vet clean; ALL packages PASS.

- [ ] **Step 3: Write README.md**

```markdown
# Loom

A terminal control center for [Claude Code](https://claude.com/claude-code):
launch, monitor, and return to real `claude` sessions across a whole workspace.

## Phase 1 — Cockpit Core

- Real `claude` sessions on a dedicated `tmux -L loom` server — they survive
  detach, Loom quitting, and terminal close.
- Full-screen attach (Enter) / detach (F12 or Ctrl-b d) hand-off.
- Live dashboard: needs-you / running / idle attention queue, recent history.
- Launcher: project · model · permission-mode · optional seed prompt.
- Resume finished sessions (`r`).

## Requirements

- macOS, `tmux` ≥ 3.x, `claude` CLI, Go ≥ 1.22 (build only)

## Build & run

    go build -o loom ./cmd/loom && ./loom

## Notes

- Scrollback inside a session uses tmux copy-mode (Ctrl-b [), not the terminal's
  native scroll — a known, deliberate deviation from raw `claude`.
- State: `~/.loom/loom.db`. Transcripts remain claude's own (`~/.claude/projects/...`).
- Design: `docs/superpowers/specs/2026-07-02-cockpit-core-design.md`.
```

- [ ] **Step 4: Manual end-to-end smoke test (the real thing)**

Run `./loom` in a full terminal and verify, in order:
1. Dashboard opens; project list discovered (`n` shows parallax, tavli, volar, …).
2. Launch a real session in `loom` itself: `n` → project `loom`, model `sonnet`, mode default, seed `reply with exactly: pong` → Enter.
3. If the trust dialog appears (first launch in loom/): dashboard row exists; attach (`↵`), answer trust, detach (F12) — the seed should be delivered after trust is accepted; verify `pong` arrives.
4. Status transitions: row shows running (◐) while claude answers → needs-you (●) after.
5. Detach → dashboard; quit loom entirely (`q`); relaunch `./loom` → session still there (tmux survived), status intact.
6. Attach from INSIDE tmux (run loom in a tmux window; `↵` must still attach — TMUX stripped).
7. Kill the session (`x` → `y`) → appears under RECENT; `r` resumes it via `claude --resume`.
8. `tmux -L loom kill-server` while loom runs → rows retire to RECENT as "done" (ended), nothing crashes, history survives restart.

Record any deviation as a bug BEFORE calling the task complete.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: wire loom binary (startup checks, engine, TUI) + README"
```

---

## Self-Review (performed during plan writing)

1. **Spec coverage:** launch/attach/detach/persist (T4/T13/T14/T15) · nested-tmux attach (T4 `AttachCmd` strips TMUX; T15 smoke #6) · gated seeding readiness+trust+literal (T13, markers spiked in T2) · P0 classifier w/ sidecar-tail regression fixture (T7) · tmux-safe naming (T5) · WAL/busy-timeout/MaxOpenConns (T10) · `remain-on-exit on` + pane_dead exit codes + reap (T4/T12) · history-safe reconcile (T10 `MarkLiveOrphansEnded` never deletes; tested) · status-off/history-limit/window-size/F12 (T4 `EnsureServer`) · `-x/-y` sizing (T13 passes App dims) · attention queue + dashboard + launcher + kill/tag/resume (T14) · registry (T11) · spike-first for `--session-id` (T2, decision matrix) · migrations via `user_version` (T10). **Gap check:** spec §3.3 "distinct prefix for the loom server" — F12 no-prefix detach covers the intent; deliberate simplification, noted in README.
2. **Placeholder scan:** clean — every code step has complete code; the two intentionally-spike-dependent constants (`DefaultReadyMarker`/`DefaultTrustMarker`) are real values with an explicit "update from spike findings" instruction.
3. **Type consistency:** `store.SessionRow` fields match usage in T12/T13/T14; `transcript.State/Reader.Poll` signatures match engine usage; `status.Row/Snapshot` match UI usage; `tmux.Client` methods match all call sites; `Launcher` field names match T15 wiring.
