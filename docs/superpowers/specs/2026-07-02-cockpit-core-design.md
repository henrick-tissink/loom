# Loom — Phase 1: Cockpit Core — Design

**Status:** Draft for review
**Date:** 2026-07-02
**Author:** Henrick + Claude (brainstorming session)

---

## 1. What Loom is

Loom is a **terminal control center you live inside** — a daily driver that replaces raw `claude` as the primary way its author launches, drives, and remembers Claude Code work across a whole workspace of projects (the `~/Sauce` portfolio: parallax, tavli, volar, HappyPay, gloom, and ~14 others).

Three pillars, built in phases:

1. **Cockpit Core** (this spec) — a multiplexer around *real* `claude` sessions: launch, attach, detach, reattach; a beautiful home dashboard; live session status; a project switcher.
2. **Memory** (Phase 2) — index the session archive → instant search (L1) → auto-distilled per-session summaries/decision logs (L2) → manual "related work" recall (L3).
3. **Workflows** (Phase 3) — saved "session recipes" chained into multi-step, interactive pipelines with `continue` / `fork` / `fresh` context topology (a guide-rail, not rails).

A later **Phase 4** adds unattended background fan-out + a read-only monitoring wall.

### Design decisions already settled (context for reviewers)

- **Cockpit, not overseer.** Loom is the daily driver, not a side dashboard. The interactive experience must be *at least as good* as raw `claude`.
- **Multiplex the real thing (option A), don't rebuild chat.** Loom wraps the actual `claude` CLI. It never re-implements Claude Code's chat UI; it orchestrates real sessions and layers its value around them.
- **Model 1 layout: full-screen hand-off.** A focused session takes the *whole* terminal as native `claude` (zero compromise). One keybind pops back to Loom's dashboard. Loom is therefore **not** a full terminal emulator drawing a child TUI in a sub-pane.
- **Build order after Core: Memory (Phase 2) → Workflows (Phase 3).** Memory's L1 search is nearly free and makes workflow runs more valuable; each memory level is the substrate for the next.
- **Stack: Go + Charm (Bubble Tea / Lipgloss).** Best beauty-to-effort ratio for a session-manager TUI; single static binary; solid ecosystem.

---

## 2. Phase 1 scope

**In scope:**
- Launch a new real `claude` session from a launcher form (project, model, running mode, optional seed prompt/command).
- Sessions persist across detach and across quitting Loom (and terminal close).
- Attach (full-screen hand-off) and detach back to the dashboard.
- Home dashboard: live session list grouped by status, with an **attention queue** (sessions needing you float to top).
- Reopen/resume a finished session.
- Kill / tag a session.
- Project registry auto-discovered from `~/Sauce`.
- Best-effort live status derived from the session transcript + tmux.

**Explicitly out of scope for Phase 1** (designed-for, not built):
- Memory search / distillation / recall (Phase 2).
- Saved & chained workflows (Phase 3).
- Background/unattended fan-out and monitoring wall (Phase 4).
- Any re-rendering of `claude`'s own chat UI.

---

## 3. Core architecture: the session model

Loom does **not** implement PTY persistence itself. It **orchestrates `tmux`** as the session backend.

```
┌─────────────────────────────────────────────────────────┐
│  LOOM  (Bubble Tea TUI)  ← the beautiful part            │
│  • home dashboard   • session list + live status         │
│  • project switcher • launcher (project/model/mode/seed) │
└───────────────┬─────────────────────────────────────────┘
                │  creates / lists / attaches / send-keys
                ▼
┌─────────────────────────────────────────────────────────┐
│  tmux  (the session backend — battle-tested)             │
│   loom/parallax/<uuid>  →  cd parallax && claude --flags  │
│   loom/tavli/<uuid>     →  cd tavli    && claude --flags  │
│   … each real claude, full-screen, native, persistent    │
└───────────────┬─────────────────────────────────────────┘
                │  writes live
                ▼
   $CLAUDE_CONFIG_DIR/projects/<enc-cwd>/<uuid>.jsonl
     ← Loom *tails* these to derive status
```

**Why tmux:** persistence, reattach, PTY correctness, and even multi-terminal attach are solved by a battle-tested tool. Loom's own code stays focused on the dashboard, launcher, and status layer.

**Model-1 hand-off:** on `↵`, Loom uses Bubble Tea's `tea.Exec` to hand the whole terminal to `tmux attach -t <name>`. The user is now in the real, full-screen `claude`. A detach keybind returns to Loom. The session keeps running in tmux regardless of Loom's lifecycle.

**tmux is the source of truth** for session existence. Loom's own store holds only *metadata*. On startup Loom reconciles: `tmux list-sessions` filtered to `loom/*`, adopt orphans, prune dead metadata.

### 3.1 Session identity & JSONL correlation (verified)

The sharp edge: Loom names the tmux session, but `claude` mints its own session UUID and JSONL filename. Solution: **Loom generates the UUID and passes `--session-id <uuid>` at launch**, so the transcript path is deterministic:

```
$CLAUDE_CONFIG_DIR/projects/<cwd with non-alphanumerics → '-'>/<uuid>.jsonl
```
(`CLAUDE_CONFIG_DIR` defaults to `~/.claude`.)

**Verified `claude` capabilities (from `claude --help`, 2026-07-02):**
- `--session-id <uuid>` — works for interactive sessions. *Open risk: confirm a fresh UUID starts a clean new session (vs. colliding with resume). Spike before relying on it.*
- `--permission-mode <default|acceptEdits|plan|auto|bypassPermissions>` (+ `--dangerously-skip-permissions`). "Auto-mode" = `--permission-mode auto`.
- `--model <opus|sonnet|fable|full-id>`.
- Positional `claude "prompt"` starts an interactive session seeded with that prompt. **Slash commands are NOT documented as launch-seedable** via the positional arg.
- `claude --resume <session-id>` reopens a specific past session.

### 3.2 Seeding input

Because slash commands can't be seeded via the positional arg, Loom seeds **all** initial input the universal way: after the pane is live, inject it with `tmux send-keys -t <name> "<seed>" Enter`. This handles plain prompts and `/slash-commands` identically, and is the same mechanism a Phase-3 workflow will use to drive step transitions. (Plain-prompt seeding via the positional arg remains a possible optimization but is not required.)

---

## 4. Dashboard, launcher, and the live-status engine

### 4.1 Home dashboard

Concentrates Loom's visual design and doubles as the situational-awareness surface. The **attention queue** floats sessions needing you to the top.

```
┌─ LOOM ───────────────────────────────────────── 4 live · 1 needs you ─┐
│  NEEDS YOU ─────────────────────────────────────────────────────────  │
│   ● tavli      permission: Edit booking.ts?        sonnet · auto  2m   │
│  RUNNING ───────────────────────────────────────────────────────────  │
│   ◐ parallax   ⏺ Edit strategy.py                  opus   · plan  12s  │
│   ◐ volar      ⏺ Bash: pytest -q                   sonnet · edits 4s   │
│  IDLE ──────────────────────────────────────────────────────────────  │
│   ○ HappyPay   your turn · 41k ctx                 opus   · norm  8m   │
│  RECENT ────────────────────────────────────────────────────────────  │
│   ✓ gloom      finished · monster AI               sonnet        1h    │
│  [↵]attach [n]ew [x]kill [t]ag  [/]search·soon [w]orkflows·soon        │
└────────────────────────────────────────────────────────────────────────┘
```

Interaction: `j/k` or arrows to move, `↵` attach, `n` new (launcher), `x` kill (confirm), `t` tag, `r` reopen a Recent session (`claude --resume`). `/` and `w` are visible-but-disabled affordances reserved for Phases 2–3.

### 4.2 Launcher

A small form that *is* the "session recipe" (used one-off in Phase 1, savable in Phase 3):
- **project** (cwd) — from the registry
- **model** — opus / sonnet / fable / custom
- **running mode** — plan / normal(default) / accept-edits / auto
- optional **seed** — a prompt or a `/slash-command`

Loom builds the exact `claude` invocation from these fields, creates a detached tmux session (`tmux new-session -d -s loom/<project>/<uuid> -c <cwd> 'claude <flags>'`), then seeds input via `send-keys`.

### 4.3 Live-status engine

Status is a small **state machine fusing two signals**:
1. **JSONL tail** (fsnotify on the transcript): last event is a pending `tool_use` → *Running*; a completed assistant turn with nothing pending → *Needs you*; last turn was the user's / nothing pending → *Idle*.
2. **tmux pane activity** (to distinguish live streaming from a genuine wait, and to catch permission prompts the JSONL doesn't cleanly mark).

**v1 state set (deliberately modest):** `Running · Needs-you · Idle · Done · Error`.

**Refresh inside Bubble Tea:** background goroutines emit messages into the Elm loop — one fsnotify watcher per active transcript (`SessionActivityMsg`), plus a periodic `tmux list-sessions` poll (~1–2s) for liveness/reconciliation (`SessionListMsg`). Redraws are throttled/debounced.

---

## 5. State store

**SQLite from day one** (pure-Go `modernc.org/sqlite`, no cgo → clean static binary). Phase 1 barely needs it, but Phase 2 Memory indexes thousands of messages and needs real queries; starting on SQLite avoids a migration.

Holds:
- **Project registry** — auto-discovered `~/Sauce` subdirs (heuristic: has `.git`, or already has a `claude` transcript dir).
- **Session metadata** — keyed by tmux session name: `{project, cwd, model, mode, seed, tags, claude_session_id, created_at, last_status}`.

Single DB at `~/.loom/loom.db` (path configurable).

---

## 6. Failure handling

**Load-bearing principle: persistence and attach are load-bearing; status is best-effort.** If JSONL correlation or event interpretation fails, status degrades to "unknown/alive" — but the user can still launch, attach, and never lose a session, because tmux is the source of truth.

- **Loom crash = non-event.** Sessions survive in tmux; restart reconciles `loom/*`.
- **Two Loom instances** reflect the same tmux backend; metadata writes are idempotent, keyed by session name.
- **tmux / claude not installed** → detected at startup; clear message + install hint; graceful refusal.
- **Partial JSONL line** (mid-write) → parse only complete newline-terminated lines.
- **claude exits/crashes in a pane** → detected via `list-sessions` → moved to *Recent* as Done/Error. (`remain-on-exit` off so panes close on claude exit.)

---

## 7. Testing strategy

- **Integration:** spawn real tmux sessions running a **fake `claude`** (a tiny script that writes a known JSONL and idles); assert Loom lists / attaches / reconciles / reopens. tmux is fully scriptable and CI-friendly.
- **Status inference:** unit tests over recorded JSONL fixtures → assert the state machine.
- **Launcher:** unit test that a recipe → exact `claude` command string (flags in the right order/values).
- **Bubble Tea model:** unit-test `Update` with synthetic messages via Charm's `teatest`.

---

## 8. Forward-compatibility (why Phases 2–3 aren't retrofits)

- **Session metadata + SQLite** are the substrate Phase 2 Memory indexes.
- **The launcher's recipe shape** is exactly a Phase-3 workflow step; chaining adds an ordered list + a `context relation` (`continue`/`fork`/`fresh`) per step.
- **`send-keys` seeding** is the same mechanism Phase-3 uses to drive step transitions.
- **Dashboard affordances** (`/`, `w`) are already reserved.

---

## 9. Open risks / spikes before/within implementation

1. **`--session-id` new-session behavior** — confirm a fresh UUID starts a clean session and produces the expected JSONL path (do this first; the whole status layer depends on it).
2. **Status precision** — validate the JSONL + pane-activity fusion against real sessions; be ready to simplify states if the "Needs-you vs Idle" distinction proves unreliable.
3. **tmux version differences** — confirm the `send-keys`, `list-sessions -F`, and pane-activity flags behave across common tmux versions.
4. **Claude Code version drift** — the verified flags are a snapshot; pin behavior with a small capability check at startup.
```
