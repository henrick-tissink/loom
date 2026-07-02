# Loom — Phase 1: Cockpit Core — Design

**Status:** Draft for review · **Revision 2** (hardened after adversarial red-team, 2026-07-02)
**Date:** 2026-07-02
**Author:** Henrick + Claude (brainstorming session)

> **Revision 2 note:** A 7-lens adversarial review (40 findings → 14 verified → 3 P0 / 5 P1 / 2 P2)
> found no showstopper. Verdict: *"Fundamentally sound; the core bet is the right architecture for a
> single-user cockpit; every surviving finding is a localized spec fix, not rework."* All P0/P1 fixes
> are folded in below. P2s and residual risks are listed in §10–11.

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
- **Model 1 layout: full-screen hand-off.** A focused session takes the *whole* terminal as native `claude` (see §3.4 for the honest caveats to "zero compromise"). One keybind pops back to Loom's dashboard. Loom is therefore **not** a full terminal emulator drawing a child TUI in a sub-pane.
- **Build order after Core: Memory (Phase 2) → Workflows (Phase 3).** Memory's L1 search is nearly free and makes workflow runs more valuable; each memory level is the substrate for the next.
- **Stack: Go + Charm (Bubble Tea / Lipgloss).** Best beauty-to-effort ratio for a session-manager TUI; single static binary; solid ecosystem.

---

## 2. Phase 1 scope

**In scope:**
- Launch a new real `claude` session from a launcher form (project, model, running mode, optional seed prompt/command).
- Sessions persist across detach and across quitting Loom (and terminal close).
- Attach (full-screen hand-off) and detach back to the dashboard — **including when Loom itself is launched from inside tmux** (§3.3).
- Home dashboard: live session list grouped by status, with an **attention queue** (sessions needing you float to top).
- Reopen/resume a finished session (survives Loom restarts — §6).
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

Loom does **not** implement PTY persistence itself. It **orchestrates `tmux`** as the session backend, on a **dedicated tmux server socket** (`tmux -L loom`) so Loom's sessions are fully isolated from the user's personal tmux server.

```
┌─────────────────────────────────────────────────────────┐
│  LOOM  (Bubble Tea TUI)  ← the beautiful part            │
│  • home dashboard   • session list + live status         │
│  • project switcher • launcher (project/model/mode/seed) │
└───────────────┬─────────────────────────────────────────┘
                │  tmux -L loom  {new-session|list|send-keys|attach|kill}
                ▼
┌─────────────────────────────────────────────────────────┐
│  tmux -L loom  (dedicated server — isolated from user's) │
│   loom-<uuid>  →  cd <project> && claude --flags          │
│   loom-<uuid>  →  cd <project> && claude --flags          │
│   … each real claude, full-screen, native, persistent    │
└───────────────┬─────────────────────────────────────────┘
                │  writes live
                ▼
   $CLAUDE_CONFIG_DIR/projects/<enc-cwd>/<uuid>.jsonl
     ← Loom *tails* these to derive status
```

**Why tmux:** persistence, reattach, PTY correctness, and even multi-terminal attach are solved by a battle-tested tool. Loom's own code stays focused on the dashboard, launcher, and status layer.

**Every tmux invocation uses `-L loom`** (the dedicated socket) — creation, listing, send-keys, attach, and kill. This isolates Loom from the user's ambient tmux and is a prerequisite for the nested-attach handling in §3.3.

**tmux is the source of truth for the existence of LIVE sessions** (see §6 for the sharp distinction between live-session truth and history truth). Loom's own store holds metadata. On startup Loom reconciles: `tmux -L loom list-sessions` filtered to `loom-*`, adopt live orphans, prune only *orphaned live* metadata (never terminal-state history — §6).

### 3.1 Session identity & JSONL correlation (verified)

The sharp edge: Loom names the tmux session, but `claude` mints its own session UUID and JSONL filename. Solution: **Loom generates the UUID and passes `--session-id <uuid>` at launch**, so the transcript path is deterministic:

```
$CLAUDE_CONFIG_DIR/projects/<cwd with non-alphanumerics → '-'>/<uuid>.jsonl
```
(`CLAUDE_CONFIG_DIR` defaults to `~/.claude`.)

The same UUID is the trailing component of the tmux session name (`loom-<uuid>`), so **tmux name ⇒ claude_session_id ⇒ JSONL filename** are all derivable from one another. This is what makes orphan adoption and `claude --resume` work from the tmux name alone.

**Verified `claude` capabilities (from `claude --help`, 2026-07-02):**
- `--session-id <uuid>` — works for interactive sessions. **Open risk / spike #1: confirm a fresh UUID starts a clean new session (vs. colliding with resume). This gates the whole status layer — do it first (§11).**
- `--permission-mode <default|acceptEdits|plan|auto|bypassPermissions>` (+ `--dangerously-skip-permissions`). "Auto-mode" = `--permission-mode auto`.
- `--model <opus|sonnet|fable|full-id>`.
- Positional `claude "prompt"` starts an interactive session seeded with that prompt. **Slash commands are NOT documented as launch-seedable** via the positional arg → seed via `send-keys` (§3.2).
- `claude --resume <session-id>` reopens a specific past session.

### 3.2 Seeding input (hardened)

Because slash commands can't be seeded via the positional arg, Loom seeds **all** initial input via `send-keys` after launch. This path is load-bearing (it is also reused to drive Phase-3 step transitions), so it is gated, not blind:

1. **Readiness gate.** After `new-session -d`, do **not** send immediately — a detached session returns as soon as the child is *forked*, long before Claude's Ink TUI enters raw mode (multi-second under this user's heavy MCP load). Poll `tmux -L loom capture-pane -p -t <name>` for Claude's input-box marker, bounded by a timeout with a visible error state.
2. **Trust gate.** Before seeding, check `hasTrustDialogAccepted` / `hasTrustDialogHooksAccepted` for the target cwd in `~/.claude.json` (or resolved `CLAUDE_CONFIG_DIR`). If untrusted, Claude's first-run *"Do you trust the files in this folder?"* modal will render and swallow/misanswer the seed. Handle it: pre-write the trust flag, surface a one-time "trust this project?" step in Loom's launcher, or detect/answer the dialog via `capture-pane` before sending. (Verified: ~20 of ~55 `~/Sauce` projects are currently untrusted, incl. parallax/volar/tavli/gloom/peron — so this is a common first-launch path, not an edge case.)
3. **Literal send.** `tmux -L loom send-keys -t <name> -l -- "<seed>"` (literal, end-of-options), then send `Enter` as a **separate** call. This prevents a bare-keyword seed (e.g. seed == `Enter`) or shell-meta chars from being reinterpreted as key names.

### 3.3 Attach / hand-off — nested-tmux handling (P0)

`tea.Exec` hands the whole terminal to the loom session. But `tmux attach` refuses when `$TMUX` is set (Loom launched from inside tmux) — it errors *"sessions should be nested with care, unset $TMUX to force"* and no-ops. The target user is a terminal power-user who **will** launch Loom from inside tmux, so this must be handled:

- **`$TMUX` unset:** `tea.Exec(tmux -L loom attach -t <name>)` — works as-is.
- **`$TMUX` set:** the dedicated `-L loom` socket alone does **not** bypass the nested check. In `tea.Exec`, **unset `TMUX` for the child** before `tmux -L loom attach -t <name>` — this gives a clean nested client on the loom socket while preserving `tea.Exec`'s suspend/resume contract. (`switch-client` is rejected: it returns instantly and is incompatible with `tea.Exec`'s blocking suspend/resume.)
- Give the loom server a **distinct prefix** to reduce (not eliminate — see §11) the double-prefix collision when nested inside the user's own tmux.
- **Startup check:** warn when Loom is running inside another multiplexer.

Surface a non-zero attach exit as an explicit error state in the dashboard (don't silently bounce back).

### 3.4 Native-fidelity config ("zero compromise", qualified — P1)

Attaching through tmux is not free by default. Configure the loom server/sessions so the attached `claude` is as close to native as possible, and document the one deviation we can't erase:

- **`status off`** on every loom session — otherwise tmux's status bar steals Claude's bottom row and visibly signals "you're in tmux" on every attach.
- **Generous `history-limit`.**
- **Scrollback story (documented deviation):** Claude renders inline to the primary buffer, so under tmux the user loses native terminal scrollback / mouse-wheel for long tool outputs (copy-mode required). Decide one: enable tmux mouse mode so the wheel enters copy-mode, **or** rely on the host terminal's scroll with mouse-mode off. This is a **known, deliberate deviation** from native `claude` — called out here so it's a design choice, not a surprise. (See §11 residual risk.)

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

Interaction: `j/k` or arrows to move, `↵` attach, `n` new (launcher), `x` kill (confirm), `t` tag, `r` reopen a Recent session (`claude --resume`). `/` and `w` are visible-but-disabled affordances reserved for Phases 2–3. The project label shown here is the human name from metadata (not the tmux session name — §4.2).

### 4.2 Launcher

A small form that *is* the "session recipe" (used one-off in Phase 1, savable in Phase 3):
- **project** (cwd) — from the registry
- **model** — opus / sonnet / fable / custom
- **running mode** — plan / normal(default) / accept-edits / auto
- optional **seed** — a prompt or a `/slash-command`

Loom builds the exact `claude` invocation from these fields and creates a detached session:

```
tmux -L loom new-session -d -s loom-<uuid> -x <cols> -y <rows> -c <cwd> 'claude <flags>'
tmux -L loom set-option -t loom-<uuid> remain-on-exit on      # §6 Done/Error classification
tmux -L loom set-option -t loom-<uuid> status off             # §3.4
# then: readiness gate → trust gate → literal send-keys seed  (§3.2)
```

- **Session name is a tmux-safe token only: `loom-<uuid>`.** The raw human project label is **never** put in the tmux name — `.` and `:` are tmux window/pane target separators, so a name like `loom/HappyPay.Web/<uuid>` is *created* fine but every `-t` operation against it silently fails (`can't find pane`). The project/cwd/label live in SQLite, keyed by uuid. Always quote `-t` targets. (The spec already sanitizes the JSONL cwd path; apply the same discipline to the session name.)
- **Size to the launching terminal** (`-x <cols> -y <rows>` from Loom's viewport; set `window-size latest`) so the banner + seeded prompt + first response aren't hard-wrapped at the detached default of 80×24 (P2, but cheap — folded in here).

### 4.3 Live-status engine (corrected classification — P0)

Status is a small **state machine fusing two signals**:

1. **JSONL turn-boundary scan (corrected).** Do **NOT** classify on the physical last line — real transcripts flush non-turn *sidecar* records at the end (`mode`, `permission-mode`, `last-prompt`, `ai-title`, `file-history-snapshot`, `attachment`, `queue-operation`, `system`). Verified: in 6 of 8 recent HappyPay transcripts the last physical line is a `permission-mode` record; none end on a turn. **Scan backward to the most recent record whose `type ∈ {assistant, user}`** (the real turn boundary) and classify on that:
   - pending `tool_use` (no matching `tool_result`) → **Running**
   - completed assistant turn, nothing pending → **Needs you**
   - last turn was the user's / nothing pending → **Idle**
   - ⚠️ A `permission-mode` record merely logs the current mode string — it is **NOT** a permission prompt and must never map to Needs-you.
2. **tmux pane activity** — to distinguish live streaming from a genuine wait, and to help catch mid-session permission prompts the JSONL doesn't cleanly mark (best-effort — §11).

**v1 state set:** `Running · Needs-you · Idle · Done · Error` (Done/Error from tmux `pane_dead_status` — §6).

**Refresh inside Bubble Tea:** background goroutines emit messages into the Elm loop — one fsnotify watcher per active transcript (`SessionActivityMsg`, tolerating partial mid-write lines by parsing only complete newline-terminated records), plus a periodic `tmux -L loom list-sessions` poll (~1–2s) for liveness/reconciliation (`SessionListMsg`). Redraws are throttled/debounced.

---

## 5. State store

**SQLite from day one** (pure-Go `modernc.org/sqlite`, no cgo → clean static binary). Phase 1 barely needs it, but Phase 2 Memory indexes thousands of messages and needs real queries; starting on SQLite avoids a migration.

**Concurrency-safe DSN (P1):** open with `?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)`. `modernc.org/sqlite` defaults to a rollback journal with `busy_timeout=0`, so without this a second writer — *two Loom instances, or even one instance's poll goroutine + fsnotify + launch insert on the default connection pool* — hits immediate `SQLITE_BUSY` → "database is locked". Additionally: funnel all writes through a **single writer goroutine** per process, make every write `INSERT ... ON CONFLICT(session_name) DO UPDATE`, and wrap in a short retry loop.

Holds:
- **Project registry** — auto-discovered `~/Sauce` subdirs (heuristic: has `.git`, or already has a `claude` transcript dir).
- **Session metadata** — keyed by tmux session name (`loom-<uuid>`): `{project_label, cwd, model, mode, seed, tags, claude_session_id, created_at, last_status}`. `last_status` persists terminal states (Done/Error) so history survives (§6).

Single DB at `~/.loom/loom.db` (path configurable).

---

## 6. Failure handling & the two sources of truth

**Load-bearing principle: persistence and attach are load-bearing; status is best-effort.** If JSONL correlation or event interpretation fails, status degrades to "unknown/alive" — but the user can still launch, attach, and never lose a session.

**Two sources of truth (P1 clarification):**
- **tmux `-L loom` is the source of truth for LIVE sessions only.**
- **The SQLite store is the source of truth for HISTORY (Done/Error/Recent).**

Reconciliation therefore **prunes only orphaned *live-state* metadata that has no tmux backing; it NEVER prunes terminal-state (Done/Error) rows.** (Without this, because finished sessions have no live tmux backing, a naive "prune dead metadata" would empty the Recent panel and break `r`/`claude --resume` on every restart.)

**Exit classification (P1 — resolves the `remain-on-exit` contradiction):** loom sessions are launched with **`remain-on-exit on`**. When `claude` exits, the pane persists as *dead*; Loom reads `pane_dead` and `pane_dead_status` (exit code) via `tmux -L loom list-panes -F` to classify **Done (0) vs Error (non-zero)**, records the terminal state in SQLite, then explicitly `kill-session`. (A fast session that dies between polls is still caught because the dead pane persists until Loom reaps it.)

Concrete failure handling:
- **Loom crash = non-event.** Live sessions survive on the loom socket; restart reconciles `loom-*`. Terminal history is in SQLite regardless.
- **Two Loom instances** reflect the same tmux backend; DB access is WAL + busy-timeout + single-writer (§5), so concurrent status writes don't error out.
- **tmux / claude not installed** → detected at startup; clear message + install hint; graceful refusal.
- **Launched inside another multiplexer** → detected; attach uses the unset-`TMUX` path (§3.3); warn the user.
- **Untrusted project on first launch** → trust gate (§3.2) prevents the seed from being swallowed/misanswered.
- **Partial JSONL line** (mid-write) → parse only complete newline-terminated lines.

---

## 7. Testing strategy

- **Integration:** spawn real `tmux -L loom` sessions running a **fake `claude`** (a tiny script that writes a known JSONL and idles); assert Loom lists / attaches / reconciles / reopens, and that **terminal-state rows survive a simulated restart** (history not pruned — §6).
- **Status inference:** unit tests over recorded JSONL fixtures → assert the backward-scan state machine. **Required fixture: a transcript whose tail is a `permission-mode` record — assert it does NOT classify as Needs-you** (P0 regression guard).
- **Session naming:** unit test that a project label containing `.`/`:`/space produces a tmux-safe `loom-<uuid>` name and that `-t` operations resolve.
- **Launcher:** unit test that a recipe → exact `claude` command string (flags in the right order/values).
- **Bubble Tea model:** unit-test `Update` with synthetic messages via Charm's `teatest`.

---

## 8. Forward-compatibility (why Phases 2–3 aren't retrofits)

- **Session metadata + SQLite** are the substrate Phase 2 Memory indexes.
- **The launcher's recipe shape** is exactly a Phase-3 workflow step; chaining adds an ordered list + a `context relation` (`continue`/`fork`/`fresh`) per step.
- **The gated `send-keys` seeding mechanism** (§3.2) is the same one Phase-3 uses to drive step transitions — so the readiness/trust gates are built once, here.
- **Dashboard affordances** (`/`, `w`) are already reserved.

---

## 9. Data-model / schema-evolution note

SQLite schema will grow across phases (Memory adds message/summary tables; Workflows add recipe/run tables). Adopt a simple **migration mechanism** (versioned `PRAGMA user_version` + ordered migration steps) from the first release so later phases don't require manual DB surgery.

---

## 10. P2 / nice-to-haves (not blocking implementation)

1. **Orphan-adoption robustness** — optionally store recipe fields (model/mode/seed/tags) in a tmux user option at launch (`set-option -t <name> @loom_meta ...`) so reconciliation can rebuild *display* metadata from tmux alone if a DB row is missing. Core correlation already works without this (name ⇒ uuid ⇒ resume/JSONL); only best-effort display fields are at risk, and true orphans are rare.
2. (Detached-session sizing was P2 but is cheap, so it's folded into §4.2.)

---

## 11. Open risks / spikes before/within implementation

These cannot be fully closed on paper — validate empirically, spike #1 **first**:

1. **`--session-id` fresh-session behavior** — confirm a fresh UUID starts a clean session and produces the expected JSONL path (vs. colliding with resume). The entire deterministic-JSONL status layer depends on it.
2. **Status precision** — even with the corrected backward-scan, Needs-you-vs-Idle leans on a tmux pane-activity heuristic that's unproven against real sessions; be ready to collapse states.
3. **Screen-scraping fragility** — the readiness gate (`capture-pane` input-box marker) and the trust gate (`~/.claude.json` internals) depend on undocumented Claude Code UI/config surfaces that can break on version drift. Add a startup capability check + live re-validation.
4. **Scrollback UX** — the deviation from native `claude` (§3.4) is inherent to multiplexing through tmux; mitigate + document, but "does it feel at least as good as raw claude" can only be judged by living in it.
5. **Nested-multiplexer prefix collision** — even after the unset-`TMUX` attach fix, Loom-inside-your-tmux means two prefix layers; needs a hands-on feel-test.
6. **tmux version/flag differences** — confirm `capture-pane` markers, `list-panes -F pane_dead_status`, `send-keys -l`, `-x/-y`, and per-session options across the tmux versions actually in use.
