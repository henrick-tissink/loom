# GUI Walking Skeleton — Design

**Date:** 2026-07-10
**Status:** Approved (brainstorming) → ready for implementation plan

## Context

Loom today is a Bubble Tea / Lipgloss terminal UI in Go that orchestrates real
`claude` sessions on a dedicated `tmux -L loom` server. The goal is to grow a
**real GUI window** — a beautiful graphical surface built on top of tmux — that
reuses the existing Go engine (registry, tmux control, memory index, workflows,
status detection) as its backend.

That full vision is several independent subsystems:

- **A · Wails shell + Go↔JS bridge** — native window, engine exposed to the web UI.
- **B · Embedded terminal** — a live tmux session rendered in an xterm.js pane.
- **C · Graphical chrome** — sidebar, status header, launcher, search, ported to web.
- **D · File-link click-to-open** — path detection + launch in external editor.

Each piece gets its own spec → plan → build cycle. **This document specs only
the first slice: a walking skeleton covering A + a minimal slice of B.** Its
sole purpose is to de-risk the two scariest unknowns before any aesthetic
investment:

1. Can the existing Go engine drive a web UI?
2. Can a live tmux session be embedded, interactively, in xterm.js?

Everything not needed to answer those two questions is explicitly out of scope.

## Decisions (from brainstorming)

- **Surface:** a real native GUI window (not an elevated TUI).
- **Foundation:** reuse the Go engine; **Wails v2** puts a web UI on top. One Go
  binary, native window.
- **Session view (end vision):** embedded terminal with graphical chrome. The
  skeleton delivers the embedded terminal with *no* chrome.
- **Coexistence:** the GUI ships as a **separate entry point** (`cmd/loom-gui`).
  The existing `loom` TUI is untouched and remains the daily driver while the
  GUI matures.
- **Sidebar updates:** **poll** every ~1.5s (matches the TUI's cadence, simplest
  correct option) rather than pushing snapshot diffs.
- **Toolchain:** adding the Wails + Node frontend build toolchain to the repo is
  approved.

## Architecture

### Entry point

`cmd/loom-gui/main.go` is a new binary that constructs the **same engine objects
`cmd/loom/main.go` already builds**: load config → `tmux.New()` client →
`store.Store` → `status.NewEngine(tm, st, claudeConfigDir)`. No engine code is
modified; the GUI is purely a second consumer of the existing packages.

It then starts a Wails application, binding a single `App` struct (the bridge)
and serving the `frontend/` assets.

### New dependencies

- `github.com/wailsapp/wails/v2` — window + Go↔JS bridge + asset serving.
- `github.com/creack/pty` — spawn `AttachCmd` under a PTY and stream it.
- Frontend: **xterm.js** (+ the `fit` addon) via the Wails-standard npm frontend
  build under `cmd/loom-gui/frontend/`.

### Bridge (`App`) — Go methods bound to JS

The bridge is the single, well-defined interface between the web UI and the
engine. It owns no orchestration logic of its own beyond a PTY registry.

| Method | Signature | Behavior |
|---|---|---|
| `ListSessions` | `() []SessionDTO` | Calls `engine.Poll(now)`, maps the snapshot rows to flat DTOs. |
| `AttachSession` | `(name string) error` | Starts `tmux.AttachCmd(name)` under `pty.Start`; registers the PTY keyed by `name`. Idempotent — a second call for an already-attached name is a no-op. |
| `SendInput` | `(name, data string)` | Writes `data` to the named PTY (keystrokes from xterm.js). |
| `ResizeSession` | `(name string, cols, rows int)` | `pty.Setsize` on the named PTY. |
| `CloseSession` | `(name string)` | Kills the *attach client* (the PTY process) and deregisters it. **Never** kills the underlying tmux session. |

```go
type SessionDTO struct {
    Name    string `json:"name"`
    Project string `json:"project"`
    Status  string `json:"status"` // running | needs_you | idle | done | error | unknown
}
```

`SessionDTO` is derived from the `status.Snapshot` the TUI already renders, so
the mapping is a pure, unit-testable function.

### PTY registry

The `App` holds `map[string]*ptySession` guarded by a mutex. A `ptySession`
bundles the `*exec.Cmd`, the `*os.File` PTY, and a `done` channel. On
`AttachSession`:

1. If `name` is already registered, return nil (idempotent).
2. `cmd := tmux.AttachCmd(name)`; `f, err := pty.Start(cmd)`.
3. Register, then launch a read loop goroutine: read chunks from `f`, emit a
   Wails event `pty:data:<name>` carrying the chunk.
4. When the read loop hits EOF/error, emit `pty:exit:<name>` and deregister.

`CloseSession` kills the PTY process and closes the file; the read loop unwinds
through the same deregister path.

## Data flow

```
frontend                         bridge (Go)                    engine / tmux
--------                         -----------                    -------------
sidebar poll (1.5s) ── ListSessions() ─────► engine.Poll(now) ──► tmux ListSessions
        ◄──────────── []SessionDTO ◄─────────┘

click session ──────── AttachSession(name) ─► pty.Start(AttachCmd(name)) ─► tmux attach
                                             └─ read loop ─► emit pty:data:<name>
        ◄──────────── pty:data:<name> events ── xterm.write(chunk)

keystroke ──────────── SendInput(name,data) ─► pty.Write(data)
window/pane resize ─── ResizeSession(name,…) ─► pty.Setsize
close pane / quit ──── CloseSession(name) ────► kill attach client (session survives)
```

The frontend is deliberately thin: a sidebar `<ul>` rebuilt from each poll, and
a single terminal container. Clicking a row tears down any current xterm
instance, mounts a fresh one, calls `AttachSession`, and wires the event/input
bridges. No routing, no state library.

## Error handling

- **No tmux server / no sessions:** `engine.Poll` already handles this; the
  sidebar simply renders empty. Not an error state.
- **`AttachSession` on a missing/dead session:** `pty.Start` (or an immediate
  `tmux attach` failure) returns an error surfaced to JS; the frontend shows a
  plain inline message. No crash, no partial registration (register only after
  `pty.Start` succeeds).
- **Underlying session ends while attached:** the read loop hits EOF, emits
  `pty:exit:<name>`, deregisters; the frontend shows the terminal as ended. The
  tmux session's own dead-pane behavior (`remain-on-exit`) is unchanged.
- **Window closed / app quit:** all PTY clients are killed on shutdown; tmux
  sessions survive exactly as they do when the TUI quits today.
- **Double-attach / rapid clicks:** idempotent `AttachSession` + teardown of the
  prior xterm on the frontend prevents duplicate read loops.

## Testing & success criteria

### Automated (Go)

- **Snapshot → DTO mapping:** table test that each `status.Status` maps to the
  right DTO string and that name/project carry through.
- **PTY registry lifecycle** (using a trivial stand-in command, not real tmux):
  attach registers exactly once; double-attach is a no-op; close deregisters and
  terminates the process; a read loop that hits EOF deregisters itself.

The bridge is structured so the registry logic is testable without a live Wails
runtime (the event-emit is an injected function/interface, stubbed in tests).

### Manual acceptance

The skeleton is done when:

1. `loom-gui` opens a native window.
2. A session started elsewhere (the `loom` TUI) appears in the sidebar with the
   correct status, and the list refreshes as status changes.
3. Clicking a session shows a **live, interactive** claude terminal — typing and
   claude's output both work, colors intact.
4. Resizing the window resizes the terminal (tmux reflows).
5. Closing the window and reopening `loom-gui` shows the same session still
   running and re-attachable.

Frontend logic is thin enough to verify by hand at this stage; a web test
harness is deferred to the chrome spec (C).

## Forward-compatibility seams (enable later beauty at ~zero cost)

The skeleton stays unstyled, but a few structural choices are cheap to make now
and expensive to retrofit once chrome (spec C) exists. Honoring these does **not**
add scope — it's still "one plain sidebar + one terminal" — it just routes those
few pixels through the right seams. See `2026-07-10-gui-design-language.md`.

- **Frameless window.** Configure the Wails window frameless with a custom drag
  region and the macOS traffic-light inset from the start. Retrofitting frameless
  later means reworking drag regions and title-bar insets — do it once, now.
- **Single tokens source.** All skeleton color/spacing/font values come from one
  CSS custom-property file (`--bg`, `--surface`, `--hairline`, `--font-mono`,
  `--font-ui`, and the six status tokens). Spec C then reskins by editing tokens,
  not by hunting hardcoded values.
- **Status→color in one place.** The `status → hex` mapping lives in a single
  shared module consumed by *both* the sidebar row rendering and the xterm.js
  theme, so a session's thread and its terminal never disagree.
- **Themed terminal from day one.** Wire xterm.js's `theme` (background,
  foreground, cursor, the 16 ANSI colors) and `fontFamily` from the tokens rather
  than accepting library defaults, so the embedded terminal already reads as
  native to the app instead of a foreign console.
- **Font indirection.** Reference `--font-mono` / `--font-ui` everywhere; the
  skeleton fills them with system stacks, and the real build later drops a bundled
  programming mono into the same variable with no component changes.

These are structural only — no sidebar styling, no layout chrome, no launcher.

## Out of scope (deferred to later specs)

Visual chrome & styling, the launcher, search/recall, workflows, file-link
click-to-open, multiple simultaneous embedded terminals, light/dark theming,
session creation/kill from the GUI, and any change to the existing TUI. These
are named here only to keep the skeleton honest — none are built in this slice.
