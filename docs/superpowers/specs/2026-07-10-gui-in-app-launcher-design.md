# GUI In-App Launcher — Design

**Date:** 2026-07-10
**Status:** Approved (user: "yes please — execute") → plan + build

## Context

`loom-gui` (the Wails GUI) can view and drive sessions but cannot *create* one,
so it still depends on the `loom` TUI to start a session. This slice adds an
in-window launcher so the GUI is **self-contained**: open loom, launch a
session, and work in it — the terminal TUI is no longer required.

The engine already has all the machinery. The TUI's `cmd/loom/main.go`
constructs `registry.Discover(...)` (project list) and a `session.Launcher{...}`;
the GUI simply builds the same objects and exposes two bridge methods.

## Ground truth (existing engine — do not modify)

- `session.Launcher.Launch(r session.Recipe, w, h int, now time.Time) (string, error)`
  — **non-blocking**: creates the tmux session, upserts the store row, starts a
  seed goroutine if `r.Seed != ""`, returns the tmux session name immediately.
- `session.Recipe{ ProjectLabel, Cwd, Model, Mode, Seed string }`.
- Option sets (from the TUI launcher, mirrored verbatim):
  - Models: `["", "opus", "sonnet", "fable"]` (`""` = claude default).
  - Modes: `["", "plan", "acceptEdits", "auto", "bypassPermissions"]` (`""` = default).
- `registry.Project{ Label, Path string }`; `registry.Discover(workspaceRoot, claudeConfigDir) ([]Project, error)`.
- Launcher fields (as the TUI sets them): `Tmux, Store, ClaudeConfigDir,
  ClaudeJSONPath: cfg.ClaudeJSONPath(), ReadyMarker: session.DefaultReadyMarker,
  TrustMarker: session.DefaultTrustMarker, ReadyTimeout: 60s, PollEvery: 500ms`.

## Decisions

- **Trigger:** a `+ New` button in the titlebar (marked `--wails-draggable:
  no-drag` so it's clickable inside the draggable bar).
- **Form:** a modal overlay — Project (`<select>`, value = path, text = label),
  Model (`<select>`), Mode (`<select>`), Seed (`<textarea>`, optional), then
  Launch / Cancel.
- **Auto-attach:** on a successful launch, immediately select+attach the new
  session's terminal, so the user lands in the live session. The 1.5s poll also
  surfaces it in the rail.
- **Validation:** `buildRecipe` rejects unknown project path / model / mode, so a
  malformed request never reaches the launcher.
- **Launch size:** pass `w=120, h=32`; the xterm attach immediately
  `ResizeSession`s to the real dimensions, so the initial size is just a seed.

## Bridge additions (`App`)

- `type ProjectDTO struct { Label, Path string }` (JSON-tagged).
- `ListProjects() []ProjectDTO` — projects discovered at startup, mapped; non-nil.
- `LaunchSession(projectPath, model, mode, seed string) (string, error)` —
  `buildRecipe` then `launcher.Launch(recipe, 120, 32, now())`; returns the new
  session name. Errors (nil launcher, unknown project/model/mode, tmux failure)
  surface to JS and are shown inline in the modal.
- `newApp` gains `launcher *session.Launcher` and `projects []registry.Project`;
  `cmd/loom-gui/main.go` constructs both and passes them in.

## Error handling

- No launcher (defensive / tests): `LaunchSession` returns an error; no crash.
- Unknown project/model/mode: `buildRecipe` returns an error; nothing launched.
- `registry.Discover` error at startup: fail fast in `main.go` (same as the TUI).
- Launch failure (tmux): the launcher's error surfaces to the modal; the modal
  stays open with the message so the user can retry.

## Testing

- **Automated (Go, headless):** `projectsToDTOs` mapping (+ empty non-nil);
  `buildRecipe` valid case (fields resolved from the matched project) and each
  rejection (unknown project/model/mode); `LaunchSession` nil-launcher error;
  `ListProjects` non-nil. The real launch is integration (needs claude/tmux) and
  is covered by manual acceptance.
- **Manual acceptance:** `+ New` opens the modal; the project list matches the
  workspace; Launch starts a claude session that appears in the rail and
  auto-attaches; an invalid/edge case shows an inline error, not a crash.

## Out of scope (later)

Recall/RELATED panel in the form, resume-from-history, editing a running
session's model/mode, project-list refresh without restart, workflows. Named
only to keep this slice honest.
