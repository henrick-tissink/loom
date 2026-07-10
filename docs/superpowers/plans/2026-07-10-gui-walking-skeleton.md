# GUI Walking Skeleton Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a `loom-gui` window that lists your real `claude` sessions from the live engine and lets you click one to attach a working, interactive embedded terminal.

**Architecture:** A new `cmd/loom-gui` binary constructs the *same* engine objects the TUI's `cmd/loom/main.go` builds, then runs a Wails v2 app that binds an `App` bridge. The bridge exposes the engine to a thin web UI (session list via polling) and streams a tmux attach through a PTY into an xterm.js terminal. No engine package is modified; the GUI is a second consumer of existing packages. The existing `loom` TUI is untouched.

**Tech Stack:** Go 1.26.4, Wails v2 (native window + Go↔JS bridge), `github.com/creack/pty` (PTY), xterm.js + `@xterm/addon-fit` (terminal), Vite (frontend build).

## Global Constraints

- Go module `github.com/henricktissink/loom`, Go **1.26.4**. Do not lower the floor.
- **Do not modify any `internal/` package.** The GUI only consumes them.
- The GUI is a **separate binary at `cmd/loom-gui`**. `cmd/loom` (the TUI) is untouched.
- Session list is obtained by **polling** `engine.Poll(now)` every **1500ms** (matches the TUI cadence).
- `CloseSession` kills the **attach client (PTY process) only — never the tmux session.**
- PTY output chunks are **base64-encoded** over Wails events (avoids UTF-8 boundary corruption); JS decodes to bytes before `term.write`.
- Event topics are exactly: data = `"pty:data:" + name`, exit = `"pty:exit:" + name`.
- Bound-method access from JS uses the runtime globals: `window.go.main.App.<Method>` and `window.runtime.EventsOn/EventsOff`.
- **Single palette source:** all colors live in `frontend/theme.js`; it writes CSS custom properties at boot and exports the xterm theme + `statusColor(status)`. No hardcoded hex anywhere else.
- macOS window uses a **hidden-inset titlebar** (`mac.TitleBarHiddenInset()`) with a CSS drag region — native traffic lights, custom bar.
- Every color/font in CSS is a `var(--…)` token; fonts go through `--font-mono` / `--font-ui`.

---

### Task 1: SessionDTO + snapshot mapping

Pure, engine-facing data mapping. No Wails, no PTY — the fully unit-testable core of "the Go engine drives the UI."

**Files:**
- Create: `cmd/loom-gui/session_dto.go`
- Test: `cmd/loom-gui/session_dto_test.go`

**Interfaces:**
- Consumes: `status.Snapshot`, `status.Row` (embeds `store.SessionRow`), `status.Status` — all existing.
- Produces:
  - `type SessionDTO struct { Name, Project, Title, Status string }` (JSON-tagged)
  - `func snapshotToDTOs(s status.Snapshot) []SessionDTO`

- [ ] **Step 1: Write the failing test**

Create `cmd/loom-gui/session_dto_test.go`:

```go
package main

import (
	"testing"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

func TestSnapshotToDTOs_mapsFieldsAndStatus(t *testing.T) {
	snap := status.Snapshot{Live: []status.Row{
		{SessionRow: store.SessionRow{Name: "api-migration", ProjectLabel: "sauce-api"},
			Status: status.NeedsYou, Title: "Migrate the API"},
		{SessionRow: store.SessionRow{Name: "nested-discovery", ProjectLabel: "loom"},
			Status: status.Running, Title: ""},
	}}

	got := snapshotToDTOs(snap)

	if len(got) != 2 {
		t.Fatalf("want 2 DTOs, got %d", len(got))
	}
	if got[0] != (SessionDTO{Name: "api-migration", Project: "sauce-api", Title: "Migrate the API", Status: "needs_you"}) {
		t.Errorf("row 0 mismatch: %+v", got[0])
	}
	if got[1].Status != "running" || got[1].Project != "loom" {
		t.Errorf("row 1 mismatch: %+v", got[1])
	}
}

func TestSnapshotToDTOs_emptyIsNonNil(t *testing.T) {
	got := snapshotToDTOs(status.Snapshot{})
	if got == nil {
		t.Fatal("want non-nil empty slice (marshals to [] not null)")
	}
	if len(got) != 0 {
		t.Fatalf("want 0, got %d", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/loom-gui/ -run TestSnapshotToDTOs -v`
Expected: FAIL — `undefined: snapshotToDTOs` / `undefined: SessionDTO` (build error).

- [ ] **Step 3: Write minimal implementation**

Create `cmd/loom-gui/session_dto.go`:

```go
package main

import "github.com/henricktissink/loom/internal/status"

// SessionDTO is the flat, JSON-friendly view of a live session the frontend renders.
type SessionDTO struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Title   string `json:"title"`
	Status  string `json:"status"` // running | needs_you | idle | done | error | unknown
}

// snapshotToDTOs flattens a status.Snapshot's live rows into SessionDTOs.
// Always returns a non-nil slice so it marshals to [] rather than null.
func snapshotToDTOs(s status.Snapshot) []SessionDTO {
	out := make([]SessionDTO, 0, len(s.Live))
	for _, r := range s.Live {
		out = append(out, SessionDTO{
			Name:    r.Name,
			Project: r.ProjectLabel,
			Title:   r.Title,
			Status:  string(r.Status),
		})
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/loom-gui/ -run TestSnapshotToDTOs -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/loom-gui/session_dto.go cmd/loom-gui/session_dto_test.go
git commit -m "feat(gui): SessionDTO + snapshot mapping for loom-gui bridge"
```

---

### Task 2: PTY registry (attach lifecycle)

The riskiest logic, made testable without tmux by injecting the command factory (tests use `cat` / `sleep` / `true` instead of `tmux attach`) and the event emitter.

**Files:**
- Create: `cmd/loom-gui/ptyreg.go`
- Test: `cmd/loom-gui/ptyreg_test.go`
- Modify: `go.mod` / `go.sum` (add `github.com/creack/pty` via `go get`)

**Interfaces:**
- Consumes: `github.com/creack/pty`.
- Produces:
  - `type emitFunc func(event string, data ...any)`
  - `type ptyRegistry struct { … }`
  - `func newPTYRegistry(start func(name string) *exec.Cmd, emit emitFunc) *ptyRegistry`
  - `(*ptyRegistry) attach(name string) error` — idempotent
  - `(*ptyRegistry) send(name, data string) error`
  - `(*ptyRegistry) resize(name string, cols, rows uint16) error`
  - `(*ptyRegistry) close(name string)`
  - `(*ptyRegistry) has(name string) bool`
  - `(*ptyRegistry) count() int`

- [ ] **Step 1: Add the PTY dependency**

Run: `go get github.com/creack/pty@latest`
Expected: `go.mod` gains a `github.com/creack/pty` require line.

- [ ] **Step 2: Write the failing test**

Create `cmd/loom-gui/ptyreg_test.go`:

```go
package main

import (
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureEmitter records events in a goroutine-safe way and lets tests wait
// for a topic prefix to appear.
type captureEmitter struct {
	mu     sync.Mutex
	events []string
}

func (c *captureEmitter) emit(event string, _ ...any) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *captureEmitter) waitFor(prefix string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, e := range c.events {
			if strings.HasPrefix(e, prefix) {
				c.mu.Unlock()
				return true
			}
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestPTYRegistry_attachIdempotentAndClose(t *testing.T) {
	em := &captureEmitter{}
	reg := newPTYRegistry(func(string) *exec.Cmd { return exec.Command("sleep", "5") }, em.emit)

	if err := reg.attach("s1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := reg.attach("s1"); err != nil {
		t.Fatalf("second attach should be a no-op, got: %v", err)
	}
	if reg.count() != 1 {
		t.Fatalf("want 1 registered pty, got %d", reg.count())
	}
	if !reg.has("s1") {
		t.Fatal("has(s1) should be true")
	}

	reg.close("s1")
	if reg.count() != 0 {
		t.Fatalf("want 0 after close, got %d", reg.count())
	}
}

func TestPTYRegistry_readLoopExitDeregisters(t *testing.T) {
	em := &captureEmitter{}
	reg := newPTYRegistry(func(string) *exec.Cmd { return exec.Command("true") }, em.emit)

	if err := reg.attach("s2"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !em.waitFor("pty:exit:s2", 2*time.Second) {
		t.Fatal("expected pty:exit:s2 after the command exits")
	}
	// Give the deregister path a beat to run after the emit.
	time.Sleep(50 * time.Millisecond)
	if reg.count() != 0 {
		t.Fatalf("want 0 after read-loop EOF, got %d", reg.count())
	}
}

func TestPTYRegistry_sendEchoesData(t *testing.T) {
	em := &captureEmitter{}
	reg := newPTYRegistry(func(string) *exec.Cmd { return exec.Command("cat") }, em.emit)

	if err := reg.attach("s3"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer reg.close("s3")

	if err := reg.send("s3", "ping\n"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !em.waitFor("pty:data:s3", 2*time.Second) {
		t.Fatal("expected pty:data:s3 after cat echoes input")
	}
}

func TestPTYRegistry_sendUnknownIsError(t *testing.T) {
	reg := newPTYRegistry(func(string) *exec.Cmd { return exec.Command("cat") }, (&captureEmitter{}).emit)
	if err := reg.send("nope", "x"); err == nil {
		t.Fatal("send to unattached name should error")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./cmd/loom-gui/ -run TestPTYRegistry -v`
Expected: FAIL — `undefined: newPTYRegistry`.

- [ ] **Step 4: Write minimal implementation**

Create `cmd/loom-gui/ptyreg.go`:

```go
package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// emitFunc pushes an event to the frontend. Backed by Wails runtime.EventsEmit
// in production; a capturing fake in tests.
type emitFunc func(event string, data ...any)

type ptyHandle struct {
	cmd *exec.Cmd
	f   *os.File
}

// ptyRegistry owns the live attach clients, keyed by session name. It never
// touches the underlying tmux session — only the PTY process wrapping the
// `tmux attach` invocation.
type ptyRegistry struct {
	mu    sync.Mutex
	ptys  map[string]*ptyHandle
	start func(name string) *exec.Cmd
	emit  emitFunc
}

func newPTYRegistry(start func(name string) *exec.Cmd, emit emitFunc) *ptyRegistry {
	return &ptyRegistry{ptys: map[string]*ptyHandle{}, start: start, emit: emit}
}

func (r *ptyRegistry) has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.ptys[name]
	return ok
}

func (r *ptyRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ptys)
}

// attach starts the command under a PTY and begins streaming. Idempotent: a
// second attach for an already-registered name is a no-op.
func (r *ptyRegistry) attach(name string) error {
	r.mu.Lock()
	if _, ok := r.ptys[name]; ok {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	cmd := r.start(name)
	f, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("attach %s: %w", name, err)
	}

	r.mu.Lock()
	// Lost a race with a concurrent attach — keep the first, drop this one.
	if _, ok := r.ptys[name]; ok {
		r.mu.Unlock()
		_ = f.Close()
		_ = cmd.Process.Kill()
		return nil
	}
	r.ptys[name] = &ptyHandle{cmd: cmd, f: f}
	r.mu.Unlock()

	go r.readLoop(name, f)
	return nil
}

func (r *ptyRegistry) readLoop(name string, f *os.File) {
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			r.emit("pty:data:"+name, base64.StdEncoding.EncodeToString(buf[:n]))
		}
		if err != nil {
			break
		}
	}
	r.emit("pty:exit:" + name)
	r.deregister(name)
}

func (r *ptyRegistry) send(name, data string) error {
	r.mu.Lock()
	h, ok := r.ptys[name]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("send: %s not attached", name)
	}
	_, err := io.WriteString(h.f, data)
	return err
}

func (r *ptyRegistry) resize(name string, cols, rows uint16) error {
	r.mu.Lock()
	h, ok := r.ptys[name]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("resize: %s not attached", name)
	}
	return pty.Setsize(h.f, &pty.Winsize{Rows: rows, Cols: cols})
}

// close terminates the attach client and deregisters it. The tmux session is
// left running.
func (r *ptyRegistry) close(name string) {
	r.mu.Lock()
	h, ok := r.ptys[name]
	if ok {
		delete(r.ptys, name)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	_ = h.f.Close()
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
}

func (r *ptyRegistry) deregister(name string) {
	r.mu.Lock()
	h, ok := r.ptys[name]
	if ok {
		delete(r.ptys, name)
	}
	r.mu.Unlock()
	if ok {
		_ = h.f.Close()
	}
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./cmd/loom-gui/ -run TestPTYRegistry -v`
Expected: PASS (all four tests).

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum cmd/loom-gui/ptyreg.go cmd/loom-gui/ptyreg_test.go
git commit -m "feat(gui): PTY registry with idempotent attach and streamed output"
```

---

### Task 3: Wails app shell + bridge wiring + session sidebar

The window opens, constructs the real engine, and renders the live session list. This is where "the Go engine drives a web UI" becomes visible. Terminal comes in Task 4.

**Files:**
- Create: `cmd/loom-gui/main.go`
- Create: `cmd/loom-gui/app.go`
- Create: `cmd/loom-gui/wails.json`
- Create: `cmd/loom-gui/frontend/index.html`
- Create: `cmd/loom-gui/frontend/package.json`
- Create: `cmd/loom-gui/frontend/vite.config.js`
- Create: `cmd/loom-gui/frontend/theme.js`
- Create: `cmd/loom-gui/frontend/tokens.css`
- Create: `cmd/loom-gui/frontend/main.js`
- Test: `cmd/loom-gui/app_test.go`
- Modify: `go.mod` / `go.sum` (add Wails v2)

**Interfaces:**
- Consumes: `snapshotToDTOs` (Task 1), `newPTYRegistry`, `emitFunc` (Task 2), `status.NewEngine`, `engine.Poll`, `tmux.New`, `tmux.AttachCmd`, `config.Load`, `store.Open`.
- Produces:
  - `type App struct { … }`
  - `func newApp(engine *status.Engine, tm *tmux.Client, now func() time.Time) *App`
  - `(*App) startup(ctx context.Context)` — captures ctx, wires the real emitter
  - `(*App) ListSessions() []SessionDTO`
  - `(*App) AttachSession(name string) error`
  - `(*App) SendInput(name, data string)`
  - `(*App) ResizeSession(name string, cols, rows int)`
  - `(*App) CloseSession(name string)`

- [ ] **Step 1: Install the Wails toolchain**

Run:
```bash
go install github.com/wailsapp/wails/v2/cmd/wails@latest
wails doctor
```
Expected: `wails doctor` reports a healthy environment (Go, npm, and platform webview deps present). Fix anything it flags before continuing.

- [ ] **Step 2: Add the Wails module dependency**

Run: `go get github.com/wailsapp/wails/v2@latest`
Expected: `go.mod` gains `github.com/wailsapp/wails/v2`.

- [ ] **Step 3: Write the failing test (bridge behavior, no window)**

Create `cmd/loom-gui/app_test.go`:

```go
package main

import (
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

func TestApp_ListSessions_pollErrorReturnsEmpty(t *testing.T) {
	// A store-less engine: Poll will error; ListSessions must degrade to [].
	eng := status.NewEngine(tmux.New(), nil, t.TempDir())
	app := newApp(eng, tmux.New(), func() time.Time { return time.Unix(0, 0) })

	got := app.ListSessions()
	if got == nil {
		t.Fatal("ListSessions must never return nil (marshals to [])")
	}
	if len(got) != 0 {
		t.Fatalf("want 0 on poll error, got %d", len(got))
	}
	_ = store.SessionRow{} // keep store import if Poll path changes
}

func TestApp_CloseUnknownIsNoop(t *testing.T) {
	app := newApp(nil, tmux.New(), time.Now)
	app.CloseSession("does-not-exist") // must not panic
}
```

> Note: if a nil-store `Poll` panics rather than errors in your engine build, wrap the `engine.Poll` call site in Task 1's `ListSessions` with a `recover` guard (see Step 5) — the test asserts the observable contract: no nil, no crash.

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./cmd/loom-gui/ -run TestApp -v`
Expected: FAIL — `undefined: newApp`.

- [ ] **Step 5: Write the App bridge**

Create `cmd/loom-gui/app.go`:

```go
package main

import (
	"context"
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
```

Add the missing import to the top of `app.go` (`os/exec`):

```go
import (
	"context"
	"os/exec"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/tmux"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./cmd/loom-gui/ -run TestApp -v`
Expected: PASS.

- [ ] **Step 7: Write the Wails entry point**

Create `cmd/loom-gui/main.go`:

```go
// loom-gui — a native window on top of the loom engine.
package main

import (
	"context"
	"embed"
	"fmt"
	"os"
	"time"

	"github.com/henricktissink/loom/internal/config"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
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

	engine := status.NewEngine(tm, st, cfg.ClaudeConfigDir)
	app := newApp(engine, tm, time.Now)

	return wails.Run(&options.App{
		Title:     "loom",
		Width:     1180,
		Height:    760,
		MinWidth:  760,
		MinHeight: 480,
		OnStartup: func(ctx context.Context) { app.startup(ctx) },
		Bind:      []interface{}{app},
		AssetServer: &options.AssetServer{Assets: assets},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarHiddenInset(),
		},
	})
}
```

- [ ] **Step 8: Create the Wails project config**

Create `cmd/loom-gui/wails.json`:

```json
{
  "$schema": "https://wails.io/schemas/config.v2.json",
  "name": "loom-gui",
  "outputfilename": "loom-gui",
  "frontend:install": "npm install",
  "frontend:build": "npm run build",
  "frontend:dev:watcher": "npm run dev",
  "frontend:dev:serverUrl": "auto",
  "wailsjsdir": "./frontend/wailsjs"
}
```

- [ ] **Step 9: Scaffold the frontend build**

Create `cmd/loom-gui/frontend/package.json`:

```json
{
  "name": "loom-gui-frontend",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "vite build"
  },
  "dependencies": {
    "@xterm/xterm": "^5.5.0",
    "@xterm/addon-fit": "^0.10.0"
  },
  "devDependencies": {
    "vite": "^5.4.0"
  }
}
```

Create `cmd/loom-gui/frontend/vite.config.js`:

```js
import { defineConfig } from "vite";

export default defineConfig({
  build: { outDir: "dist", emptyOutDir: true },
});
```

Create `cmd/loom-gui/frontend/index.html`:

```html
<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>loom</title>
    <link rel="stylesheet" href="/tokens.css" />
    <link rel="stylesheet" href="/node_modules/@xterm/xterm/css/xterm.css" />
  </head>
  <body>
    <header id="titlebar"><span class="wordmark">loom</span></header>
    <main id="body">
      <aside id="rail"><div class="rail-head">THREADS</div><ul id="threads"></ul></aside>
      <section id="stage"><div id="stage-empty">Select a session</div></section>
    </main>
    <script type="module" src="/main.js"></script>
  </body>
</html>
```

- [ ] **Step 10: Create the single palette source**

Create `cmd/loom-gui/frontend/theme.js`:

```js
// The single source of truth for loom's palette. Writes CSS custom properties
// at boot and exports the xterm theme + status→color map so the rail and the
// terminal can never disagree.
export const palette = {
  bg: "#15141B", rail: "#1A1922", surface: "#201E28", hairline: "#322E3C",
  text: "#E9E5F0", textDim: "#9C95A9", textFaint: "#645D72",
  needs_you: "#F5B14C", running: "#56C6A9", idle: "#7E8AA6",
  done: "#7FA98A", error: "#E06A5E", unknown: "#565065",
  termBg: "#121118",
};

export function statusColor(status) {
  return palette[status] || palette.unknown;
}

export function applyTokens() {
  const r = document.documentElement.style;
  r.setProperty("--bg", palette.bg);
  r.setProperty("--rail", palette.rail);
  r.setProperty("--surface", palette.surface);
  r.setProperty("--hairline", palette.hairline);
  r.setProperty("--text", palette.text);
  r.setProperty("--text-dim", palette.textDim);
  r.setProperty("--text-faint", palette.textFaint);
  r.setProperty("--needs", palette.needs_you);
  r.setProperty("--running", palette.running);
  r.setProperty("--font-ui", 'system-ui, -apple-system, "Segoe UI", sans-serif');
  r.setProperty("--font-mono", 'ui-monospace, "SF Mono", "JetBrains Mono", Menlo, monospace');
}

export const xtermTheme = {
  background: palette.termBg,
  foreground: "#C9C3D4",
  cursor: palette.needs_you,
  selectionBackground: "rgba(245,177,76,0.25)",
};
```

Create `cmd/loom-gui/frontend/tokens.css`:

```css
:root { color-scheme: dark; }
* { box-sizing: border-box; }
body {
  margin: 0; background: var(--bg); color: var(--text);
  font-family: var(--font-ui); font-size: 14px;
  height: 100vh; display: flex; flex-direction: column;
}
#titlebar {
  height: 44px; flex: 0 0 44px; display: flex; align-items: center;
  padding-left: 78px; border-bottom: 1px solid var(--hairline);
  --wails-draggable: drag;
}
.wordmark { font-family: var(--font-mono); letter-spacing: 0.34em; padding-left: 0.34em; }
#body { flex: 1; min-height: 0; display: grid; grid-template-columns: 264px 1fr; }
#rail { background: var(--rail); border-right: 1px solid var(--hairline); overflow-y: auto; }
.rail-head {
  font-size: 10.5px; letter-spacing: 0.18em; color: var(--text-faint);
  padding: 13px 16px 9px; font-weight: 600;
}
#threads { list-style: none; margin: 0; padding: 0; }
.thread {
  position: relative; padding: 9px 14px 9px 18px; cursor: pointer;
  display: flex; justify-content: space-between; gap: 10px;
}
.thread::before {
  content: ""; position: absolute; left: 0; top: 6px; bottom: 6px;
  width: 2px; border-radius: 2px; background: var(--tc, var(--text-faint));
}
.thread:hover { background: rgba(255,255,255,0.025); }
.thread.active { background: var(--surface); }
.thread .name { font-family: var(--font-mono); font-size: 12.5px; }
.thread .proj { font-size: 11px; color: var(--text-faint); }
.thread .st { font-size: 10.5px; color: var(--tc, var(--text-faint)); }
#stage { min-width: 0; display: flex; flex-direction: column; background: var(--bg); }
#stage-empty { margin: auto; color: var(--text-faint); font-family: var(--font-mono); }
#terminal { flex: 1; min-height: 0; padding: 10px 12px; background: #121118; }
```

- [ ] **Step 11: Write the sidebar frontend (no terminal yet)**

Create `cmd/loom-gui/frontend/main.js`:

```js
import { applyTokens, statusColor } from "./theme.js";

applyTokens();

const threadsEl = document.getElementById("threads");
let activeName = null;

function renderThreads(sessions) {
  threadsEl.replaceChildren();
  for (const s of sessions) {
    const li = document.createElement("li");
    li.className = "thread" + (s.name === activeName ? " active" : "");
    li.style.setProperty("--tc", statusColor(s.status));
    li.innerHTML =
      `<span><span class="name">${s.name}</span> ` +
      `<span class="proj">${s.project}</span></span>` +
      `<span class="st">${s.status.replace("_", " ")}</span>`;
    li.addEventListener("click", () => selectSession(s.name));
    threadsEl.appendChild(li);
  }
}

// Placeholder until Task 4 wires the terminal.
function selectSession(name) {
  activeName = name;
}

async function poll() {
  try {
    const sessions = await window.go.main.App.ListSessions();
    renderThreads(sessions);
  } catch (e) {
    console.error("ListSessions failed", e);
  }
}

poll();
setInterval(poll, 1500);
```

- [ ] **Step 12: Build and verify the window opens with the real session list**

First, in another terminal, launch a couple of sessions via the existing TUI (`./loom`, press `n`, start a session) so there is live data.

Then:
```bash
cd cmd/loom-gui
wails build
./build/bin/loom-gui   # macOS: open ./build/bin/loom-gui.app
```
Expected (manual): a native "loom" window opens; the left rail lists the sessions you started, each with its project and a status word, and the correct status color on the left thread edge; the list refreshes as statuses change (e.g. a session going running→needs_you recolors within ~1.5s).

- [ ] **Step 13: Commit**

```bash
git add cmd/loom-gui/main.go cmd/loom-gui/app.go cmd/loom-gui/app_test.go \
        cmd/loom-gui/wails.json cmd/loom-gui/frontend/ go.mod go.sum
git commit -m "feat(gui): Wails shell + bridge + live session sidebar"
```

> If `wails build` writes generated bindings under `frontend/wailsjs/` or a `build/` dir you don't want tracked, add them to `.gitignore` in this commit (`cmd/loom-gui/build/`, `cmd/loom-gui/frontend/dist/`, `cmd/loom-gui/frontend/node_modules/`).

---

### Task 4: Embedded terminal (attach a live tmux session)

Clicking a session mounts an xterm.js terminal wired to the bridge — the payoff task. Terminal is themed from the single palette source.

**Files:**
- Modify: `cmd/loom-gui/frontend/index.html` (add fit-addon css already covered; no change needed if Task 3 used the css link — otherwise none)
- Modify: `cmd/loom-gui/frontend/main.js` (replace the `selectSession` placeholder)

**Interfaces:**
- Consumes: `window.go.main.App.AttachSession/SendInput/ResizeSession/CloseSession`, `window.runtime.EventsOn/EventsOff`, `xtermTheme` from `theme.js`.
- Produces: no Go interface — frontend integration only.

- [ ] **Step 1: Replace the terminal placeholder in main.js**

In `cmd/loom-gui/frontend/main.js`, update the imports line:

```js
import { applyTokens, statusColor, xtermTheme } from "./theme.js";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
```

Then replace the placeholder `selectSession` function with the full attach flow:

```js
let term = null;
let fit = null;
let dataUnsub = null;
let exitUnsub = null;

function b64ToBytes(b64) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

function teardownTerminal() {
  if (dataUnsub) { dataUnsub(); dataUnsub = null; }
  if (exitUnsub) { exitUnsub(); exitUnsub = null; }
  if (activeName) { window.go.main.App.CloseSession(activeName); }
  if (term) { term.dispose(); term = null; }
}

function selectSession(name) {
  teardownTerminal();
  activeName = name;

  const stage = document.getElementById("stage");
  stage.replaceChildren();
  const host = document.createElement("div");
  host.id = "terminal";
  stage.appendChild(host);

  term = new Terminal({
    fontFamily:
      getComputedStyle(document.documentElement).getPropertyValue("--font-mono"),
    fontSize: 13,
    theme: xtermTheme,
    cursorBlink: true,
  });
  fit = new FitAddon();
  term.loadAddon(fit);
  term.open(host);
  fit.fit();

  // Wails delivers our base64 payload as the first event arg.
  dataUnsub = window.runtime.EventsOn("pty:data:" + name, (b64) => {
    term.write(b64ToBytes(b64));
  });
  exitUnsub = window.runtime.EventsOn("pty:exit:" + name, () => {
    term.write("\r\n\x1b[2m[session ended]\x1b[0m\r\n");
  });

  term.onData((data) => window.go.main.App.SendInput(name, data));

  window.go.main.App.AttachSession(name)
    .then(() => {
      fit.fit();
      window.go.main.App.ResizeSession(name, term.cols, term.rows);
    })
    .catch((e) => term.write("\r\n\x1b[31mattach failed: " + e + "\x1b[0m\r\n"));

  window.addEventListener("resize", onResize);
}

function onResize() {
  if (!term || !fit || !activeName) return;
  fit.fit();
  window.go.main.App.ResizeSession(activeName, term.cols, term.rows);
}
```

Also update `renderThreads` so re-renders don't clobber the mounted terminal: the `activeName` guard in the class list already preserves the highlight; leave `selectSession` binding as-is (clicking the same active row re-attaches, which is acceptable for the skeleton).

- [ ] **Step 2: Build and verify an interactive terminal**

Ensure a live session exists (start one via `./loom` if needed), then:
```bash
cd cmd/loom-gui
wails build
./build/bin/loom-gui    # macOS: open ./build/bin/loom-gui.app
```
Expected (manual acceptance — the skeleton's definition of done):
1. Window opens; sidebar lists sessions with correct status colors.
2. Click a session → a live claude terminal appears; its output renders with colors, and the amber cursor blinks.
3. Typing goes through — e.g. type a message and press Enter, claude responds.
4. Resize the window → the terminal reflows (tmux resizes to match).
5. Close the window and relaunch `loom-gui` → the same session is still running and re-attaches (the tmux session survived; only the attach client was killed).

- [ ] **Step 3: Commit**

```bash
git add cmd/loom-gui/frontend/main.js cmd/loom-gui/frontend/index.html
git commit -m "feat(gui): embedded xterm.js terminal attached to live tmux session"
```

---

## Self-Review

**Spec coverage** (against `2026-07-10-gui-walking-skeleton-design.md`):
- Entry point `cmd/loom-gui` constructing the same engine objects → Task 3 (main.go).
- Bridge methods `ListSessions/AttachSession/SendInput/ResizeSession/CloseSession` → Task 3 (app.go), backed by Task 1 (mapping) + Task 2 (PTY).
- `SessionDTO {name, project, status}` (+ title) → Task 1.
- Poll every ~1.5s → Task 3 (main.js `setInterval(poll, 1500)`).
- PTY streaming via `pty:data:<name>` / `pty:exit:<name>` events → Task 2 + Task 4.
- `CloseSession` never kills the tmux session → Task 2 (`close` kills PTY process only); verified by manual acceptance step 5.
- Error handling: no server/empty (ListSessions → []), attach failure surfaced, session-ends read-loop EOF → `pty:exit`, window-close leaves sessions alive → Tasks 2–4.
- Automated tests: snapshot→DTO mapping (Task 1) + PTY registry lifecycle (Task 2) + bridge contract (Task 3).
- Forward-compat seams: frameless/hidden-inset titlebar (Task 3 main.go `mac.TitleBarHiddenInset()` + `--wails-draggable` drag region), single tokens source (`theme.js`), status→color once (`statusColor`), themed terminal from tokens (`xtermTheme`, Task 4), font indirection (`--font-mono`/`--font-ui`).

**Placeholder scan:** no TBD/TODO; every code step has full content; the one "placeholder" (`selectSession` in Task 3 Step 11) is explicitly a stub replaced in Task 4 Step 1, and both versions are shown in full.

**Type consistency:** `snapshotToDTOs`, `SessionDTO`, `newPTYRegistry`, `emitFunc`, `ptyRegistry.{attach,send,resize,close,has,count}`, `newApp`, and the `App` method set are named identically across Tasks 1–4. Event topics `pty:data:<name>` / `pty:exit:<name>` and the base64 payload contract match between Task 2 (emit) and Task 4 (decode). Bound-method path `window.go.main.App.*` is consistent (package `main`, struct `App`).
