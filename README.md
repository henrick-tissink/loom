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

## Phase 2 — Memory

- Loom indexes your Claude session history into a local, full-text-searchable
  archive so you can find and return to past work: `~/.claude/projects/**`
  transcripts (main sessions and their subagent files) are swept into
  `~/.loom/loom.db` in the background, starting immediately on launch and then
  every 10 minutes. Each session's ask, outcome, and touched files are
  extracted; nothing leaves your machine except the optional summary call
  below.
- The first sweep of a fresh or large archive takes roughly 10 seconds; the
  search view's `N sessions` annotation shows `· indexing…` while it's still
  running and updates live.
- `/` from the dashboard opens search — type to filter, `↵` on a hit opens
  its detail (ask/outcome/files/snippets), `r` resumes it, `esc` steps back.
- `s` in detail generates an on-demand AI summary of that one session via a
  single `claude -p --model haiku` call — this consumes your Claude plan
  quota (a few seconds, one small request), and only runs when you press it.
  Pressing `s` again regenerates and replaces the stored summary.

## Requirements

- macOS, `tmux` ≥ 3.x, `claude` CLI, Go ≥ 1.22 (build only)

## Build & run

    go build -o loom ./cmd/loom && ./loom

## Notes

- Scrollback inside a session uses tmux copy-mode (Ctrl-b [), not the terminal's
  native scroll — a known, deliberate deviation from raw `claude`.
- State: `~/.loom/loom.db`. Transcripts remain claude's own (`~/.claude/projects/...`).
- Design: `docs/superpowers/specs/2026-07-02-cockpit-core-design.md`.
