# Loom

A terminal control center for [Claude Code](https://claude.com/claude-code):
launch, monitor, and return to real `claude` sessions across a whole workspace.

**📖 New here? Read [the Loom Manual](docs/MANUAL.md)** — the complete guide to
flying the cockpit. The sections below are per-phase feature notes.

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

## Phase 3 — Workflows

- `w` opens the workflows view: a **RUNS** section (active runs, honest
  store-backed status: `▸ name#id · step 2/3 execute · needs you` plus
  `· seed pending` / `· seed FAILED` markers) and a **WORKFLOWS** section
  (definitions loaded from `~/.loom/workflows/*.json`, malformed files listed
  dim-red with their error).
- A workflow is a named chain of steps, each a normal `claude` session.
  Starting one launches step 1; you advance (`n`) when you're ready — nothing
  auto-advances. Loom launches the next step per its **relation** and offers
  a confirm preview of exactly what will happen (including the substituted
  seed) before it fires.
- **Definition format** (`~/.loom/workflows/<name>.json`, name must match the
  filename):

  ```json
  { "name": "plan-execute-review",
    "steps": [
      { "label": "plan",    "project": "myproject", "model": "opus", "mode": "plan",
        "seed": "Plan the following work: <describe>. Write the plan to docs/plan.md.", "relation": "fresh" },
      { "label": "execute", "model": "sonnet", "mode": "acceptEdits", "relation": "fork",
        "seed": "Execute the plan just written. Prior step concluded: {{prev.outcome}}" },
      { "label": "review",  "relation": "fresh", "seed": "/code-review" } ] }
  ```

  A starter copy lives at `docs/examples/plan-execute-review.json` — copy and
  edit it:

  ```sh
  mkdir -p ~/.loom/workflows
  cp docs/examples/plan-execute-review.json ~/.loom/workflows/
  # edit the "project" field(s) and seed prompts to match your workspace
  ```

- **Relations** (ignored for step 1, which is always a fresh session):
  - `fresh` — new session, seed used as-is.
  - `fork` — new session, seed with `{{prev.*}}` templates substituted from
    the previous step.
  - `continue` — the seed is sent into the *current* step's still-running
    session (model/mode are ignored — ignoring them is a known v1 limit).
- **Template variables**: `{{prev.outcome}}`, `{{prev.title}}`,
  `{{prev.ask}}` — filled in from the previous step's extracted transcript at
  confirm-open time. Any other `{{...}}` token is rejected at load time (a
  typo never ships as literal braces). A value that can't be resolved (or an
  `ask` that fails the same filters memory search uses) renders as
  `(unavailable)` rather than blocking the advance.
- **Limits (v1)**: no in-app definition editor (hand-edit the JSON); no
  branching/conditionals; no auto-advance; no scheduling; no per-step
  worktree isolation; `continue` can't change model/mode; each substituted
  value is capped at 8KB and the assembled seed at 15KB (both truncate with a
  visible `…[truncated]` marker); a dead `continue` target offers `f` to fork
  from the transcript instead; run rows are never garbage-collected.

## Phase 2.5 — Recall

- The launcher (`n`) gains a **RELATED** panel below the form: pick a project
  and it immediately shows that project's most recent sessions; type a seed
  prompt and it live re-ranks into an actual full-text recall query —
  same-project hits first, then cross-project hits, both blended above
  unrelated noise (a 2+ matched-term relevance gate keeps stray keyword
  overlap from surfacing as a false match). This is a *manual* pull, never an
  automatic injection.
- `↓` from the seed field moves focus into the panel (`↑` moves back out);
  `↵` on a hovered entry opens its detail view to read the full session
  before deciding; `space` toggles it **included** — included entries pin to
  the top of the panel (up to 3 at once) and stay included through further
  typing or re-ranking. `esc` from anywhere in the launcher returns to the
  dashboard; opening a detail from the launcher and pressing `esc` restores
  the launcher exactly as it was (`r` resume is hidden in that detail — 
  launching another session mid-launcher-flow is a footgun).
- **At launch**, each included entry is appended to your seed as visible,
  literal context — nothing is silently injected:

  ```
  <your seed> ── Related prior work [project·title]: outcome
  ```

  A seed starting with `/` never gets blocks appended (the panel shows a
  warning instead) — a slash command's argument line isn't the place to glue
  outcome text onto.
- **The marker**: pulled-in context is recognizable by its
  ` ── Related prior work [` prefix. Loom's own indexer strips everything
  from that marker onward before computing a session's indexed text or ask,
  so context pulled into a new session is never re-indexed as that session's
  *own* words — recall can't echo or compound across generations.
- **Limits**: lexical (full-text) relevance only, no embeddings yet (same
  interface, upgradeable later); the panel can go briefly stale during an
  active background index sweep and self-heals on your next keystroke or
  project change rather than live-updating; max 3 includes per launch; in a
  small archive, same-project recency dominates simply because there isn't
  yet enough cross-project history for the query to rank against.

## Phase 4 — Fan-out + Wall

- **`N`** opens fan-out: a project checklist plus one shared model · mode ·
  optional seed. Check as many projects as you want (`space`), `tab`/
  `shift-tab` moves between the checklist / model / mode / seed fields,
  `↓`/`↑` scroll the checklist while it's focused. `↵` launches — one real
  session per checked project, all sharing the same recipe (no per-project
  seed in v1) — and stays on the form until every launch has resolved, then
  drops you back on the dashboard with a summary line:

  ```
  fan #a1b2c3: 4/5 launched · failed: tavli (bad cwd) · volar launched untagged
  ```

  Every session in the group is tagged `fan:<groupID>` (visible in the tag
  editor, `t`); a launched-but-untagged session is still counted as
  launched, never silently dropped. Rows belonging to a group get a dim
  `· fan` marker in the dashboard's activity column — that marker plus the
  tag editor and the summary line above are the only group affordance in v1,
  there's no dedicated group view yet. `r`-resume on a fanned-out session
  carries its tags forward, so a resumed session rejoins its group.
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
  everything at once; fan-out recipes are uniform across the group; no
  group view beyond the marker/tag/summary above; no remote hosts; the wall
  is read-only — there's still no headless/scripted launch path, and there
  isn't going to be one.

## Requirements

- macOS, `tmux` ≥ 3.x, `claude` CLI, Go ≥ 1.22 (build only)

## Build & run

    go build -o loom ./cmd/loom && ./loom

To get `loom` on your PATH:

    ln -s "$PWD/loom" ~/.local/bin/loom

## Notes

- Scrollback inside a session uses tmux copy-mode (Ctrl-b [), not the terminal's
  native scroll — a known, deliberate deviation from raw `claude`.
- State: `~/.loom/loom.db`. Transcripts remain claude's own (`~/.claude/projects/...`).
- Design: `docs/superpowers/specs/2026-07-02-cockpit-core-design.md`.
