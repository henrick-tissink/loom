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
