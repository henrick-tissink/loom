# The Loom Manual

Loom is a terminal control center for [Claude Code](https://claude.com/claude-code): launch, monitor, remember, and chain real `claude` sessions across your whole workspace. This manual covers everything the cockpit does and how to fly it.

---

## 1. The mental model

Three facts explain almost everything:

1. **Every session is a real `claude` running inside tmux** (on Loom's own private tmux server, `tmux -L loom`). Loom never re-implements chat — when you attach, you get the genuine full-screen Claude Code, every feature intact.
2. **Sessions outlive Loom.** Quit Loom, close the terminal, reboot your shell — live sessions keep running in tmux. Loom is a *window onto* your sessions, not their owner.
3. **Loom remembers by reading Claude's own transcripts.** Every session Claude Code has ever written to `~/.claude/projects/` is indexed into `~/.loom/loom.db` — searchable, distillable, resumable, including sessions that predate Loom.

The one habit that matters: **F12 detaches** (session keeps living). Quitting claude itself (`/exit`, double Ctrl-C) *ends* the session.

## 2. Starting

```bash
loom            # if symlinked to your PATH, else: cd ~/Sauce/loom && ./loom
```

On first launch the indexer sweeps your entire archive in the background (~10s for hundreds of MB); search works immediately on whatever is indexed so far and re-sweeps every 10 minutes. Projects are auto-discovered from your workspace root — `$LOOM_WORKSPACE` if set, else `~/Sauce` (anything with a `.git` or existing Claude transcripts).

## 3. The dashboard

```
╭─ LOOM ──────────────────────────────── 3 live · 1 needs you ─╮
│ NEEDS YOU ─────────────────────────────────────────────────── │
│ ▸ ● tavli      reply ready · fix booking race    82k  sonnet·auto   2m │
│ RUNNING ───────────────────────────────────────────────────── │
│   ◐ parallax   ⏺ Edit · add vega hedge          140k  opus·plan   12s │
│ IDLE ──────────────────────────────────────────────────────── │
│   ○ loom       your turn                          9k  fable·auto   1h │
│ RECENT ────────────────────────────────────────────────────── │
│   ✓ gloom      done · monster AI                       opus        3h │
╰─ ↵ attach · space peek · n new · x kill · t tag · r reopen · q quit … ─╯
```

**Reading a row:** status icon · project · *activity* (state + Claude's own session title) · context tokens · model·mode · age.

| Icon | Meaning |
|---|---|
| `●` red | **Needs you** — Claude finished, a reply is waiting (floats to the top; you also get a macOS notification with sound) |
| `◐` amber | **Running** — the `⏺` hint shows the tool it's using |
| `○` | **Idle** — your turn |
| `✓` / `✗` | Done (exit 0) / Error — in RECENT, resumable |

**Dashboard keys**

| Key | Action |
|---|---|
| `↵` | Attach full-screen (F12 to come back) |
| `space` | **Peek** — live read-only view of the pane without attaching (`↵` attaches from peek, `esc` backs out) |
| `n` | New session (launcher) |
| `N` | **Fan-out** — same recipe across many projects (§8) |
| `/` | **Search** everything you've ever done (§6) |
| `w` | **Workflows** (§7) |
| `W` | **Wall** — live grid of all running sessions (§8) |
| `x`→`y` | Kill (confirm) |
| `t` | Edit tags |
| `r` | Reopen a RECENT session (`claude --resume` — same conversation) |
| `j/k` or `↓/↑` | Move · `q`/`ctrl+c` quit Loom (sessions keep running) |

**The bell:** when any session flips to *needs you*, you get a notification — walk away freely. Caveat: while you're attached full-screen, Loom's polling pauses, so bells for *other* sessions arrive the moment you detach.

## 4. Launching sessions

`n` opens the launcher:

| Field | Notes |
|---|---|
| **project** | `←/→` cycles your discovered projects |
| **model** | default / opus / sonnet / fable |
| **mode** | default / plan / acceptEdits / auto / bypassPermissions |
| **seed** | optional first prompt — plain text or a `/slash-command` |

`tab`/`shift-tab` move fields · `↵` launches · `esc` cancels. The seed is delivered only once Claude's input box is actually ready (and never fired into a first-run trust dialog).

### The RELATED panel (recall)

As you pick a project and type a seed, related past sessions appear below the form — recent work from that project, plus cross-project matches on your seed words.

- `↓` from the seed field enters the panel; `↑` from the top returns to the form
- `space` toggles an entry **into your seed** (✓, max 3) — its distilled outcome is appended at launch as explicit context: `── Related prior work [project·title]: outcome…`
- `↵` on an entry opens its full detail view to read first (`esc` returns, everything intact)
- Included context is marked so it never re-indexes as the new session's own words (no echo chamber)
- Changing project clears the includes; a `/slash-command` seed can't take includes (you'll see a warning)

## 5. Inside a session

It's real Claude Code — nothing changes. Remember:

- **F12** (or `Ctrl-b d`) = detach, keep alive
- Scrollback uses tmux copy-mode (`Ctrl-b [`, `q` to exit) instead of your terminal's native scroll — the one deliberate deviation from raw claude
- If claude exits (crash or `/exit`) you'll briefly see tmux's "Pane is dead" — F12 goes home; the session files under RECENT with its exit code

## 6. Memory: search & detail

`/` from the dashboard. Type — results appear live, one row per session, best snippet highlighted, ranked by relevance. Everything is searchable: every project, every session ever, **including subagent transcripts** and sessions that predate Loom. Filenames and titles are searchable too ("which session touched reader.go").

`↓/↑` select · `↵` opens the **detail view**:

- **Ask** — what the session was asked (the real human prompt)
- **Outcome** — the last conclusion
- **Files** — every file touched, including by subagents
- **Summary** — press `s` for an LLM summary (a small, sandboxed haiku call — costs plan quota, so regenerating asks you to press `s` twice)
- Matching snippets for your query

`r` resumes the session into a live cockpit row — with collision protection: if it's *already* live you get a hint instead of a duplicate. `esc` back to search · `q` quit.

Notes: new content appears in search up to one sweep (~10 min) late; deleted transcripts stay searchable forever (the index is the memory); search is text-match, not semantic.

## 7. Workflows

Saved multi-step chains of real sessions. Definitions are JSON files in `~/.loom/workflows/`:

```json
{ "name": "plan-execute-review",
  "steps": [
    { "label": "plan",    "project": "parallax", "model": "opus", "mode": "plan",
      "seed": "Plan the following work: <describe>. Write the plan to docs/plan.md.", "relation": "fresh" },
    { "label": "execute", "model": "sonnet", "mode": "acceptEdits", "relation": "fork",
      "seed": "Execute the plan just written. Prior step concluded: {{prev.outcome}}" },
    { "label": "review",  "relation": "fresh", "seed": "/code-review" } ] }
```

(Starter copy: `cp docs/examples/plan-execute-review.json ~/.loom/workflows/` — filename must match `"name"`.)

**Relations** — how each step gets its context:

| Relation | Meaning |
|---|---|
| `fresh` | Brand-new session, seed as-is (step 1 is always fresh) |
| `fork` | New session; `{{prev.outcome}}`, `{{prev.title}}`, `{{prev.ask}}` in the seed are replaced with the previous step's distilled values |
| `continue` | No new session — the seed is sent into the current step's session (delivered only when it's idle; model/mode can't change mid-session) |

Steps without a `project` inherit the previous step's. Unknown template variables are rejected when the file loads.

**Running:** `w` → select a definition → `↵` starts step 1. You work in that session as long as you like (it's a normal dashboard row, tagged `wf:name#id:stepN`). When ready:

| Key (on a run) | Action |
|---|---|
| `n` | **Advance** — a confirm shows the next step + a preview of the substituted seed → `y` |
| `f` | If the current session died, demote this advance to a fork from its transcript (offered in the confirm) |
| `↵` | Attach to the current step's session |
| `x` | Abandon the run (its sessions stay alive as normal sessions) |

Advancing past the last step finishes the run. Runs survive restarts; an undelivered `continue` seed shows as *seed pending* — `n` retries it. Resuming a dead step session from the dashboard reconnects it to its run automatically.

## 8. Fan-out & the Wall

**`N` — fan-out:** a checklist of projects (`space` toggles, `↓/↑` scroll) + one shared model/mode/seed. `↵` launches one session per checked project, all tagged `fan:<group-id>`; a summary line reports successes and failures. The dashboard shows a dim `· fan` marker on group members; the swarm sorts itself through the normal attention queue.

**`W` — the wall:** a read-only 2-column grid of every live session's pane tail, refreshing live. `↓/↑` select · `↵` attach · `esc` back. Order is stable (oldest first) so the grid never shuffles under you. Cells are plain-text tails — for full fidelity, attach.

## 9. Under the hood

| Thing | Where |
|---|---|
| Loom's state (index, sessions, runs) | `~/.loom/loom.db` (SQLite) |
| Workflow definitions | `~/.loom/workflows/*.json` |
| Sessions | tmux server `-L loom` (`tmux -L loom list-sessions` to inspect raw) |
| Transcripts (Claude's own, read-only to Loom) | `~/.claude/projects/…` |

Two Loom instances can run against the same state safely. If Loom ever crashes, nothing is lost — relaunch and it re-adopts everything from tmux.

## 10. Troubleshooting

| Symptom | Explanation / fix |
|---|---|
| "Pane is dead" when attaching | claude exited; **F12**, the session files under RECENT (`r` resumes it) |
| Session stuck on *unknown/starting* | First poll hasn't landed or claude is still booting — give it a few seconds; attach to look |
| Seed never arrived | First launch in an untrusted folder shows Claude's trust dialog — attach, answer it; the seed sends after |
| Search misses very recent work | Index sweeps ~10 min; restart Loom or wait |
| No bell while attached | By design — polling pauses during attach; bells land on detach |
| `tmux -L loom kill-server` | Nuclear option: all live sessions end (history survives); Loom recovers on next launch |
