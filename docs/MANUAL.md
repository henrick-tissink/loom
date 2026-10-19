# The Loom Manual

> **Status (2026-10-14):** the key-driven terminal UI this manual describes was
> retired; the Loom app (`make gui-install`) is the frontend, and `loom` on the
> command line is now `loom ls | attach <name> | kill <name> | search <query>`.
> The mental model, "inside a session", "under the hood" and troubleshooting
> sections still hold. The keybindings do not; the app exposes the same
> actions as buttons and a command palette. The RELATED (recall) panel no
> longer exists.

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

On first launch the indexer sweeps your entire archive in the background (~10s for hundreds of MB); search works immediately on whatever is indexed so far and re-sweeps every 10 minutes. Projects are auto-discovered from your workspace root — `$LOOM_WORKSPACE` if set, else `~/Sauce` (anything with a `.git` or existing Claude transcripts). A workspace subfolder that isn't itself a project is treated as a group: repos one level inside it are discovered too and labeled `group/repo`.

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
| `/` | **Search** everything you've ever done (§6) |
| `W` | **Wall** — live grid of all running sessions (§7) |
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
| **own git worktree** | `claude --worktree`: the session gets a fresh branch under `.claude/worktrees/`, so parallel sessions in one repo don't collide. The row shows the branch, changed files and unpushed commits; a finished one can **Remove worktree** (the branch is kept; refused while it holds changes, unpushed commits or a running session; gitignored files such as a copied `.env` are deleted with it). |

`tab`/`shift-tab` move fields · `↵` launches · `esc` cancels. The seed is delivered only once Claude's input box is actually ready (and never fired into a first-run trust dialog).

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

## 6a. Briefs: what did I actually do?

A project's **Brief** button (also on the project's right-click menu and in the palette) opens the brief view.

1. **Pick a window:** 24h, 3d, 5d or 1w. Loom collects the evidence and shows what it found before anything runs: the sessions, your commits, others' commits, and roughly how many tokens it will use. This step costs nothing.
2. **Write brief.** Opus writes a deep, per-task account from that evidence. It takes a few minutes and uses plan quota.
   - The brief opens with an overview: shipped / in flight / needs you / blocked / surprises.
   - Each task then gets its own section: outcome, status (taken from git, including squash-merged PRs), the problem, how it works now, plan vs what shipped, proof, risks, how to explain it, and likely questions.
   - Claims cite their evidence (`S3:L120` = session 3, line 120; or a commit) and say whether they are `[git]`, `[transcript]`, `[inferred]` or `[unverified]`.
3. **Each task has two buttons:**
   - **Ask about this** opens a session about that task, so you can keep asking until you understand it.
   - **Review** opens an independent review of the task's change.

   Both sessions are fenced. They can read the project's code and the brief, and nothing else: no shell, no edits, no web, no MCP or connectors.
4. **Make artefact** turns the brief into a polished standalone page in your browser. It is the same text, so you can print it or keep it.

Saved briefs are listed under the window chips. They live in `~/.loom/briefs/` (**Files** reveals the folder): `brief.md`, `evidence.md` (exactly what Claude was given), and the page.

From the terminal: `loom brief <project> --window 3d` writes a brief. `--evidence` prints the facts with no Claude call. `--list` shows the saved briefs.

## 7. The Wall

**`W` — the wall:** a read-only 2-column grid of every live session's pane tail, refreshing live. `↓/↑` select · `↵` attach · `esc` back. Order is stable (oldest first) so the grid never shuffles under you. Cells are plain-text tails — for full fidelity, attach.

## 8. Under the hood

| Thing | Where |
|---|---|
| Loom's state (index, sessions, projects) | `~/.loom/loom.db` (SQLite) |
| Briefs | `~/.loom/briefs/<project>/<when>-<window>/` |
| Sessions | tmux server `-L loom` (`tmux -L loom list-sessions` to inspect raw) |
| Transcripts (Claude's own, read-only to Loom) | `~/.claude/projects/…` |

Two Loom instances can run against the same state safely. If Loom ever crashes, nothing is lost — relaunch and it re-adopts everything from tmux.

## 9. Troubleshooting

| Symptom | Explanation / fix |
|---|---|
| "Pane is dead" when attaching | claude exited; **F12**, the session files under RECENT (`r` resumes it) |
| Session stuck on *unknown/starting* | First poll hasn't landed or claude is still booting — give it a few seconds; attach to look |
| Seed never arrived | First launch in an untrusted folder shows Claude's trust dialog — attach, answer it; the seed sends after |
| Search misses very recent work | Index sweeps ~10 min; restart Loom or wait |
| No bell while attached | By design — polling pauses during attach; bells land on detach |
| `tmux -L loom kill-server` | Nuclear option: all live sessions end (history survives); Loom recovers on next launch |
