# Loom

A terminal control center for [Claude Code](https://claude.com/claude-code):
launch, monitor, and return to real `claude` sessions across a whole workspace.

**📖 New here? Read [the Loom Manual](docs/MANUAL.md)** — the complete guide to
flying the cockpit. The sections below are per-phase feature notes.

**🛠 Working on Loom? Read [the Architecture doc](docs/ARCHITECTURE.md)** — the
engineering model: invariants, data model, control flows, and the reasons
behind the non-obvious choices.

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

## Phase 4 — Wall

- **`W`** opens the wall: a read-only, 2-column grid of every live session's
  pane — header line (status · project · title/tool hint) plus a tail of
  recent output, refreshed once per poll. Order is stable (oldest-launched
  first, tie-broken by name) and never reshuffles on a status change, so the
  grid doesn't jump around while you're reading it; `↓`/`↑`/`j`/`k` move the
  selection, `↵` attaches the selected session full-screen (same hand-off as
  the dashboard), `esc` returns to the dashboard. A session whose pane
  couldn't be captured this tick still shows its cell, marked
  `(pane unavailable)`, with `↵` gated off until it either recovers or is
  reaped.
- **Limits (v1)**: wall cells are colorless and show only the left edge of
  wide panes (Claude's own chrome truncates on the right); only the visible
  page's panes are captured, so paging is a read, not a live feed of
  everything at once; no remote hosts; the wall is read-only — there's still
  no headless/scripted launch path, and there isn't going to be one.

## Requirements

- macOS, `tmux` ≥ 3.x, `claude` CLI, Go ≥ 1.22 (build only)

## Build & run

The app is the frontend:

    make gui-install        # builds and installs /Applications/loom.app
    make gui-run            # build and launch without installing

`loom` is a thin command line over the same engine, for a shell or a script:

    go build -o loom ./cmd/loom
    ./loom ls               # live sessions with their fused status
    ./loom attach <name>    # hand this terminal to the session's tmux pane
    ./loom kill <name>
    ./loom search <query>   # full-text search over indexed session history
    ./loom doctor           # how this machine's Agent Discipline install compares to the bundle loom carries
    ./loom setup            # install or upgrade the discipline layer (plan, gate, apply; asks first)
    ./loom uninstall        # remove exactly what setup installed
    ./loom discipline …     # status | diff <file> | keep <file> | reset <file> | restore <backup>

`setup`, `uninstall` and the `discipline` writes refuse inside a Claude Code
session or without a terminal: only a person installs. See `docs/DISCIPLINE.md`.

To get `loom` on your PATH:

    ln -s "$PWD/loom" ~/.local/bin/loom

## Notes

- Scrollback inside a session uses tmux copy-mode (Ctrl-b [), not the terminal's
  native scroll — a known, deliberate deviation from raw `claude`.
- State: `~/.loom/loom.db`. Transcripts remain claude's own (`~/.claude/projects/...`).
- Design: `docs/superpowers/specs/2026-07-02-cockpit-core-design.md`.
