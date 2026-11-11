# The Loom Manual

Loom is a desktop control center for real Claude Code sessions. Launch it from
your application menu (Linux: search **Loom**) or application bundle (macOS).

## Desktop quick start

- Use **New session** to choose a project, model, permissions, seed prompt and
  optional isolated worktree. Prefer worktrees for concurrent changes in one repo.
- Select a session to use its embedded terminal. Closing Loom leaves tmux sessions
  and run scripts alive; rebooting the machine does not preserve live processes.
- Open Preferences for editor, notifications, theme and workspace. Workspace changes
  apply after restart; `LOOM_WORKSPACE` overrides the saved preference.
- Linux: **Ctrl+Shift+P** opens commands; **Ctrl-click** opens terminal links;
  **Ctrl+Shift+C/V** copies/pastes in the terminal. macOS keeps **Command-K** and
  **Command-click**. Shell control keys remain terminal input.
- Outside a text field or terminal, **/** searches history and **R** opens Quick reply.
- Linux uses `notify-send` (libnotify), `xdg-open` (xdg-utils) or `gio`, and the
  desktop file-manager service. Missing openers produce visible errors. A session
  notification requires a notification daemon and an enabled preference.
- Launching Linux Loom again queues activation of the existing GUI; it does not
  change its workspace. The compositor may prevent focus stealing. Startup errors
  are recorded in `~/.loom/startup-error.log`; agent sessions are never killed.
- System theme uses the desktop appearance portal at startup and WebKit's live
  color-scheme notifications. A desktop without either can use explicit dark/light.

Native desktop QA across all compositors is still pending.

## 1. The mental model

Three facts explain almost everything:

1. **Every session is a real `claude` running inside tmux** (on Loom's own private tmux server, `tmux -L loom`). Loom never re-implements chat — when you attach, you get the genuine full-screen Claude Code, every feature intact.
2. **Sessions outlive Loom.** Quit Loom, close the terminal, reboot your shell — live sessions keep running in tmux. Loom is a *window onto* your sessions, not their owner.
3. **Loom remembers by reading Claude's own transcripts.** Every session Claude Code has ever written to `~/.claude/projects/` is indexed into `~/.loom/loom.db` — searchable, distillable, resumable, including sessions that predate Loom.

The one habit that matters: **F12 detaches** (session keeps living). Quitting claude itself (`/exit`, double Ctrl-C) *ends* the session.

## 2. Starting

Launch **Loom** from your desktop application menu. From a shell, use
`~/.local/bin/loom-gui` on Linux. The separate `loom` command is a CLI for
listing, attaching, killing, searching and reporting; it does not open the GUI.

Project discovery uses `LOOM_WORKSPACE`, the saved workspace preference, or
`~/Sauce`, in that order. Your installed launcher may supply the environment
override. History indexing runs in the background and refreshes every ten minutes.

## 3. The dashboard

The sidebar organizes projects and sessions; the attention queue highlights work waiting on you. Select a row to open its terminal.

**Reading a row:** status icon · project · *activity* (state + Claude's own session title) · context tokens · model·mode · age.

When the branch a session worked on has a pull request (GitHub, via `gh`) or merge request (GitLab, via `glab`), the row shows it: `PR #77 · ✓ checks`, `MR !55 · ● checks`, `PR #12 · merged`. Red means failing checks or changes requested. Click the pill to open it in your browser. `PR ?` means the forge couldn't be asked (hover for why: usually `gh auth login` or `glab auth login`). Status refreshes about once a minute for live sessions.

When the PR has review feedback, the pill counts it (`3 comments`) and the row gets a **Send review** button. The feedback is the unresolved diff threads plus any reviews or comments posted since the last push (bot comments such as coverage reports are left out). The button hides while the session has a permission dialog open. The button fetches it fresh and shows it as a prompt you can edit; **Send** pastes it into the session as one message. On a finished session it reads **Resume and send**, which resumes the conversation with the review as its first message. Nothing is sent until you click.

When a session's task file is open and has Done-when checks, and the session is waiting on you, the row gets **Run to goal** (the target icon). It shows a `/goal` built from the task file's checks, with a cap of 8 turns, for you to edit; **Start goal** sends it. Claude then keeps working until a check of its transcript finds every Done-when check shown passing and the task file done, or the cap is hit. The row shows `goal · 3 checks` while it runs (hover for the last check's reason), `goal paused` when claude gave up on a turn (send any message to continue), and `goal met` at the end. Type `/goal clear` in the session to stop it early.

**Spinning.** When a session hits the same tool failure three times in one turn, its row shows a red `spinning ×3` pill. That means the same error from a command (the same test failing the same way, say, even when the command is reworded) or from a file tool on one file. Hover the pill for the failing command or file, the error's telling line, and how long it has been going on. The pill goes away when a new prompt starts a turn, when the same command or file succeeds, or after the session has run 8 more tools without hitting that failure. It ignores failures that aren't the agent's doing: a tool call you declined, a hook's block, and the auto-mode classifier denying a call or being unavailable. The row and the stage header get **Nudge** (the pause icon). It shows a message naming the failure and asking the agent to stop and answer three questions before it tries again: what failed, what would fix it, and whether it is repeating an approach that already failed. You can edit the message; **Send nudge** pastes it as one message, and claude queues it if it is mid-turn. Nudge hides while the session has a dialog open. Rows don't flag sessions that have gone a while without editing anything: slow builds, consultations and edits made through the shell made that too noisy.

**Overlap.** When two live sessions have pending changes to the same file of one repository, both rows show an amber `overlap` pill: `overlap · engine.go` for one file, `overlap · 3 files` for several, and `overlap ×2` when more than one other session is involved. Hover for which sessions and files. With one other session, clicking the pill selects it. In one checkout the two sessions overwrite each other's work. On two branches they will conflict at merge. A session's pending changes depend on where it runs:

- **In its own worktree** (a linked worktree with no other live session in it), they are everything uncommitted there plus the files of the branch's commits that no other branch or remote has. A commit you have pushed to the branch's own remote copy still counts until it is merged. Edits made through the shell count here too.
- **Anywhere else** (the main checkout, a worktree two sessions share, another repository), they are the files the session wrote with its file tools (Edit, Write, MultiEdit, NotebookEdit) that are still uncommitted. Once the session's edit has been committed (or reverted), the file stops counting for it until it edits the file again, so a session that finished with a file isn't named when another one picks it up. Edits made through the shell don't count. Neither do a subagent's edits, or edits made before a `/clear`.

Git is read in the background at most every 10 seconds per checkout, so a pill can lag a commit by that much. A merge done on the remote counts only after a fetch. Sessions in hidden projects are never named.

**Control from phone** (the phone icon, on a session waiting on you) sends `/remote-control` to the session and shows its terminal. If claude asks whether to enable Remote Control (it does the first time), answer there. While it is on, the row shows a `phone` pill: click it to open the session at claude.ai/code, or open the Code tab in the Claude app. `/remote-control` in the session disconnects it. To get pinged on your phone when a session waits on you, turn on push notifications in claude's `/config`; that setting is claude's, not loom's.

| Icon | Meaning |
|---|---|
| `●` red | **Needs you** — Claude finished, a reply is waiting (floats to the top; native notifications are available when enabled) |
| `◐` amber | **Running** — the `⏺` hint shows the tool it's using |
| `○` | **Idle** — your turn |
| `✓` / `✗` | Done (exit 0) / Error — in RECENT, resumable |

**Desktop controls:** select a row to attach its terminal; use New session,
Search, Preferences and Commands in the sidebar. Running and waiting sessions
continue to be polled while another terminal is selected. Native notifications
are independent of the selected session.

## 4. Launching sessions

**New session** opens the launcher:

| Field | Notes |
|---|---|
| **project** | `←/→` cycles your discovered projects |
| **model** | default / opus / sonnet / fable |
| **mode** | default / plan / acceptEdits / auto / bypassPermissions |
| **seed** | optional first prompt — plain text or a `/slash-command` |
| **own git worktree** | `claude --worktree`: the session gets a fresh branch under `.claude/worktrees/`, so parallel sessions in one repo don't collide. The row shows the branch, changed files and unpushed commits; a finished one can **Remove worktree** (the branch is kept; refused while it holds changes, unpushed commits or a running session; gitignored files such as a copied `.env` are deleted with it). |

Use the launcher fields and Launch button. The seed is delivered only once Claude's input box is actually ready (and never fired into a first-run trust dialog).

### Best of N

Set **Attempts** to *Best of 2* or *Best of 3* in the launcher to run the same seed in that many worktrees at once. A seed is required, and the worktree option is switched on for you. Each attempt's row shows **best of N · k**. Click it to compare the attempts side by side: each one's status, branch and commit count, with every file any attempt touched in rows across the columns. This shows everything since they started: commits, uncommitted changes and new files. **Refresh** rereads them.

**Keep this one** shows what will be lost, then stops the other attempts and deletes their worktrees. Their branches stay, so anything they committed is still there (`git branch -D` removes them). The attempt you keep is left exactly as it is; merging it is up to you.

### Setup and run scripts

Right-click a project and choose **Scripts…** to give it a **setup** script and a **run** script. Both run in your login shell. They are kept in loom, not in the repository.

- **Setup** runs by itself in each new worktree as soon as Claude creates it. Use it to copy `.env`, install dependencies, and so on. `$LOOM_ROOT_PATH` is the project and `$LOOM_WORKSPACE_PATH` the worktree.
- **Run** starts from a session's row (▶) and stops from the same button (■). It gets `$LOOM_PORT`, the first of ten ports kept for that workspace. Each worktree gets its own ten, and the same ones again next time if they are free, so parallel dev servers don't collide (`npm run dev -- --port $LOOM_PORT`).

The row shows **setup…** while setup runs and **setup failed (N)** if it exits non-zero. A running run script shows as **▶ :port**; click it to open `http://localhost:port`. Click any other script pill for the scripts window: each script's output, **Run setup** again, and **Run**/**Stop**. Scripts keep running if loom quits, and a worktree with a running script is not removed.

## 5. Inside a session

It's real Claude Code — nothing changes. Remember:

- **F12** (or `Ctrl-b d`) = detach, keep alive
- Scrollback uses tmux copy-mode (`Ctrl-b [`, `q` to exit) instead of your terminal's native scroll — the one deliberate deviation from raw claude
- If claude exits (crash or `/exit`) you'll briefly see tmux's "Pane is dead" — F12 goes home; the session files under RECENT with its exit code

## 6. Memory: search & detail

Use Search or `/` outside the terminal. Type — results appear live, one row per session, best snippet highlighted, ranked by relevance. Everything is searchable: every project, every session ever, **including subagent transcripts** and sessions that predate Loom. Filenames and titles are searchable too ("which session touched reader.go").

Select a search result to open its **detail view**:

- **Ask** — what the session was asked (the real human prompt)
- **Outcome** — the last conclusion
- **Files** — every file touched, including by subagents
- **Summary** — use the summary action for an LLM summary (a small, sandboxed haiku call — costs plan quota, so regenerate only when needed)
- Matching snippets for your query

Resume returns the session to a live cockpit row — with collision protection: if it's *already* live you get a hint instead of a duplicate.

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

## 7. Monitoring several sessions

Use the attention queue and session rows to inspect activity and select terminals.
The old terminal UI's `W` wall shortcut is no longer a current GUI control.

## 7a. Usage

The gauge button in the sidebar opens **Usage**. Its dot turns amber at 70% and red at 90% of your tightest plan window.

- **Plan limits**: your 5-hour and 7-day windows, with when each resets. Claude only reports them to its status line, so the first time, click **Track plan limits**. Loom wraps your status line command in a small script that saves what claude reports and then runs your command as before; your status line looks the same. settings.json is backed up first, and **Stop tracking** puts the original back. The numbers update whenever any claude session redraws its status line.
- **Tokens**: what you spent in the last 5 hours and 7 days, by model and by project, read from the transcripts (subagents included). "out" is output, "in" is input plus cache writes, and "cache" is cache reads, which are cheap and make up most of a long session's input.

## 7b. Rules from repeated failures

The **Rules** tab of Agent Discipline lists failures that keep repeating:
- a hook holding sessions for the same reason;
- tests re-run with nothing changed;
- a tool call failing the same way, such as Write on a file that was not read first.

Each proposal shows how often it happened, in how many sessions and projects, and the evidence (click **evidence**). A failure is listed once it has happened at least 5 times in 3 or more sessions in the last 14 days.

**Adopt…** opens the rule with loom's suggested wording, which you can edit, and the day it lapses (30 days out by default). **Review** shows exactly what will change in `~/.claude/discipline/rules.md` and, for the first rule, the import line added to `~/.claude/CLAUDE.md`. **Apply** writes it after backing up both files. Each rule in the file carries why it exists and when it lapses, so the agent sees the reason too.

Adopted rules show how often their failure happened since you adopted them, against the 14 days before. When a rule lapses it is marked; **Renew…** or **Drop…** it (both are reviewed the same way). Loom never adds, changes or removes a rule on its own.

## 8. Under the hood

| Thing | Where |
|---|---|
| Loom's state (index, sessions, projects) | `~/.loom/loom.db` (SQLite) |
| Briefs | `~/.loom/briefs/<project>/<when>-<window>/` |
| Usage tap (when on) | `~/.loom/usage/` (the script and the last status line capture) |
| Adopted rules | `~/.claude/discipline/rules.md` (imported from `~/.claude/CLAUDE.md`) |
| Setup and run scripts, port blocks | `~/.loom/scripts.json`; the jobs run on tmux `-L loom` as `loomjob-…` |
| Best-of-N groups | `~/.loom/bestof.json` |
| Sessions | tmux server `-L loom` (`tmux -L loom list-sessions` to inspect raw) |
| Transcripts (Claude's own, read-only to Loom) | `~/.claude/projects/…` |

Linux allows one GUI owner per user; launching again requests activation. macOS retains its existing multi-instance behavior. If Loom crashes, relaunch to re-adopt live tmux sessions.

## 9. Troubleshooting

| Symptom | Explanation / fix |
|---|---|
| "Pane is dead" when attaching | claude exited; **F12**, the session files under RECENT (`r` resumes it) |
| Session stuck on *unknown/starting* | First poll hasn't landed or claude is still booting — give it a few seconds; attach to look |
| Seed never arrived | First launch in an untrusted folder shows Claude's trust dialog — attach, answer it; the seed sends after |
| Search misses very recent work | Index sweeps ~10 min; restart Loom or wait |
| No notification | Check Preferences, libnotify and your desktop notification daemon; polling continues while a terminal is selected |
| `tmux -L loom kill-server` | Nuclear option: all live sessions end (history survives); Loom recovers on next launch |
