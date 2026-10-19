# Loom — Architecture

> **Living document.** This is the "how Loom actually works" reference: the
> invariants, the data model, the control flows, and the hard-won reasons
> behind the non-obvious choices. `README.md` is per-phase feature notes;
> `docs/MANUAL.md` is the user guide; this file is the engineering model.
>
> **Keeping it current:** when you change a package's responsibility, the DB
> schema, a cross-package invariant, or one of the concurrency guards in §7,
> update the matching section here in the same commit. Feature-level detail
> (keybindings, flags) belongs in the MANUAL, not here. Last verified against
> the tree on branch `cut-parked-arcs` (2026-10-14); `go test ./...` green
> across all 16 packages.

---

## 1. What Loom is

Loom is a **control plane for real `claude` processes**. It does not
re-implement Claude Code, wrap its API, or proxy its I/O semantics. It
launches genuine `claude` CLI sessions inside a private tmux server, watches
them by reading the transcripts Claude writes anyway, and gives you a cockpit
for many of them at once.

Everything follows from three structural commitments:

| Commitment | Consequence |
|---|---|
| **A session is a real `claude` in tmux** (`tmux -L loom`) | Full fidelity on attach; sessions outlive Loom, the terminal, and crashes. Loom is a *window*, never an owner. |
| **Observation is read-only** | Status comes from `~/.claude/projects/**/*.jsonl` (Claude's own transcripts), Claude's own session registry, and tmux pane liveness. Loom never instruments, patches, or intercepts Claude. |
| **Two sources of truth, cleanly split** | tmux owns *liveness*; `~/.loom/loom.db` owns *history*. Reconciliation may retire a row, never delete one. |

The corollary that shapes half the codebase: **Loom's model of a session is
always inferred, never authoritative.** So every inference is written to
degrade — a bad status label, never a lost session.

Five earlier arcs (multi-step chains, same-prompt launches across many
projects, a per-project agent with its own notes, task graphs run in
worktrees, and a study deck built from transcripts) were cut from the tree.
Their last state is reachable at the git tag `parked-arcs`; their tables
remain in the schema (§6.4) so an existing `loom.db` opens unchanged. The
Bubble Tea TUI and the launcher's recall panel followed on 2026-10-14: the GUI
is the one frontend, and `cmd/loom` is a thin command line over the same
engine.

---

## 2. Process & deployment shape

One frontend and one command line over a shared engine:

```
cmd/loom-gui  →  Wails v2 + xterm.js (frontend/)    ─┐
                                                     ├─→  internal/{status,session,store,memory,projects,tmux,…}
cmd/loom      →  `loom ls|attach|kill|search`       ─┘
```

The GUI's `main()` does the wiring: `hydratePATH()`/`hydrateLocale()` first
(a Finder-launched bundle inherits neither a usable `PATH` nor a locale) →
check binaries → load config → ensure tmux server → open store (migrate) →
discover and reconcile projects → start the indexer goroutine → build
`Launcher`, `Engine`, `Summarizer` → run the frontend. The CLI opens the same
store and runs one engine poll per `ls` (so it prints exactly what the GUI
shows, retirements included), hands the terminal to `tmux attach` for
`attach`, and reads the FTS index for `search`. It launches nothing.

**Two Loom instances against one DB is a supported state** — hence WAL,
`busy_timeout`, and the single-connection discipline in §6.

### Filesystem contract

| Path | Owner | Role |
|---|---|---|
| `~/.loom/loom.db` | Loom (rw) | sessions, projects, transcripts index, FTS |
| `~/.loom/settings.json` | GUI (rw, atomic) | editor, notifications, auto-summarize, theme (dark default) |
| `~/.claude/projects/**` | Claude (Loom: **read-only**) | transcripts — main + subagent |
| `~/.claude/sessions/<pid>.json` | Claude (Loom: read-only) | per-process session registry: current session id, tmux target, status, kind and background job id |
| `claude agents --json --all` | Claude (Loom: runs it, read-only) | background sessions, including the ones the supervisor has stopped |
| `~/.claude/state/{tasks,checkpoints}/<sid>.md` | the operator's hooks (Loom: read-only) | a session's pinned finish line and its hand-over checkpoint |
| `~/.claude/discipline/r<N>/`, `~/.claude/hooks/enforce`, `~/.local/bin/deadline`, `~/.claude/settings.json` hook entries, `~/.claude/CLAUDE.md` | the Agent Discipline bundle. `internal/discipline` is the one writer (`loom setup`, `uninstall`, `discipline keep|reset|restore`), by explicit human action only: every writing command refuses inside a Claude session or without a terminal. The doctor reads. Receipt, ledger and backups live under `~/.loom/{discipline,backups}` | hooks, rules, helpers; see `docs/DISCIPLINE.md` |
| `~/.claude.json` | Claude (Loom: read-only) | per-project trust flags |
| `$LOOM_WORKSPACE` (default `~/Sauce`) | user | project discovery root |

`$CLAUDE_CONFIG_DIR` overrides `~/.claude` everywhere (`internal/config`).

---

## 3. Package map

Dependency direction is strictly downward; nothing in `internal/` imports a
frontend.

```
   cmd/loom (CLI)                    cmd/loom-gui (Wails)
        │                                    │
        └──────────────────────┬─────────────┤
                               │             │
   internal/status ────────────┤     internal/{gitdiff,cleanup}
        │        │             │
   internal/session   internal/transcript   internal/memory
        │                │                     │
   internal/tmux   internal/ccsessions   internal/store ── internal/projects
                                               │
                              internal/registry, config
```

| Package | Responsibility | Notes |
|---|---|---|
| `tmux` | Every interaction with the `-L loom` server | Only place that shells out to `tmux`. Exact-match targets (`=name`), locale defense. |
| `session` | Launch recipe + the launch/resume sequence | Owns tmux naming, argv construction, the ready/trust gate, seed delivery, multi-repo `--add-dir`. |
| `transcript` | Locate + interpret Claude's JSONL | Path encoding, incremental `Reader`, turn-boundary `Classifier`. |
| `ccsessions` | Read Claude's own session registry and its background sessions | The only way to learn a pane's *current* session id after `/clear` or an in-pane resume. Also lists background sessions (`claude agents --json --all`, throttled) and stops one (`claude stop`). |
| `taskfile` | Read the hooks' task file and checkpoint | Finish line (status, Done-when ticks, outcome, `Started:` time, the stop guard's `.verified` marker) and hand-over age per session; stat-cached per poll. |
| `diet` | The context-diet report: plugins at user scope, MCP servers and connectors per project, commands to copy | Pure and read-only over Claude's own files; `diet/fromstore` is its only tie to loom.db. Nothing is applied. |
| `hooklog` | Read the hooks' decision and allow logs | Incremental `(inode, offset)` reader with truncation restart; legacy dirs read once. Per session: holds, nudges, the held group, the last stop verdict. Per hook: rates split at change marks (ledger, or inferred from `rev` transitions and old edit times), never summed across a mark. Test re-runs per project. |

**External sessions.** Every live registry entry that is not in a loom pane (a
`claude` started in some terminal) is shown as an `ext-<id>` row: status from
its transcript and the registry, title, ask, task pill, gauge. Never stored —
the registry file is its liveness, so there is nothing to retire — and with
no pane there is nothing to attach, reply into, or kill; the engine keeps the
last status per external row only so the needs-you bell stays once-only. A
conversation a loom row already tails is not shown twice.

**Background sessions.** A session Claude's own supervisor runs (`claude --bg`,
agent view) is a `bg-<job id>` row, never stored either. While a process runs
it, it is a registry entry with `kind: "bg"` and a `jobId`, and its status is
fused like an external row's. The supervisor stops a finished or idle one after
about an hour; it then has no registry entry and is known only to
`claude agents --json --all` (the supported interface; `~/.claude/jobs/*` is
documented as unstable), which `ccsessions.AgentsCache` runs at most every 5 s,
keeping the last good list when a run fails. Such a row's status is the
supervisor's state: blocked → needs you, done/stopped → idle, failed → error.
Naming the row by job id keeps it the same row across the stop and restart.
Attach runs `claude attach <id>` under the stage pty (which restarts a stopped
one) and kill runs `claude stop <id>`, which keeps the conversation; there is
no pane to reply into. Removing one (`claude rm`) stays in Claude's hands.
| `status` | Fuse transcript state with tmux liveness | The reconcile loop. The only writer of live-session status. |
| `store` | SQLite: schema, sessions, projects, memory index | Migrations are transactional & re-entrant. |
| `memory` | Index / extract / summarize | Reads the whole Claude archive; one of the two packages that call out to an LLM (the other is `brief`, §10a). Also records each indexed file's `mcp__` calls (`indexed_files.mcp`) for the diet report, backfilling older files a bounded batch per sweep. |
| `projects` | Discovery reconciliation, attribution, hide/solo visibility | `loom.db` is the runtime source of truth for launch targets; queried read-through. Removing a project (`App.RemoveProject`) kills its live sessions, forgets its rows, deletes the project and records the root in `ignored_roots`; "+ New project" at that root un-ignores it. |
| `registry` | Workspace project discovery | `.git` or an existing transcript dir; one nesting level. |
| `gitdiff` | `git diff` capture for the review panel | Shells out; never writes to a repo. |
| `cleanup` | Remove git-ignored build junk | Only paths that are *both* ignored and junk-patterned. |
| `config` | Paths, env, binary preflight | |
| `discipline` | The embedded Agent Discipline bundle (rules, hooks, suites, helpers), its generated manifest, the on-machine layout, the read-only doctor, and the installer: plan → stage + gate → commit, uninstall from the receipt, keep/reset of local edits | Source of truth for the handbook from rev 3 on. Revisions are immutable once in the manifest history. Only a human installs (`RefuseIfAgent`). Plan: `docs/DISCIPLINE.md`. |
| `brief` | Project study briefs: evidence ledger from transcripts + git, the Opus writer, the archive under `~/.loom/briefs`, the HTML page | Go decides what is true (sessions in the window by per-record cwd, commits deduped by SHA and matched by author email, branch state with the forge asked about squash merges); Claude only writes it up, citing evidence ids. §10a. |
| `jsonedit` | Order-preserving JSON editing for `settings.json` | Keeps every key and byte loom does not own; atomic write through symlinks (temp + fsync + rename in the real file's directory). |

Test mass is deliberate: roughly **8k test lines vs 7.5k source lines.**

---

## 4. The session lifecycle

### 4.1 Naming and identity

```go
sessionID := uuid.NewString()          // == claude's --session-id
tmuxName  := "loom-" + sessionID       // tmux-safe: no '.' or ':'
```

The project label is **never** embedded in the tmux name — `.` and `:` break
`tmux -t` targeting. The label lives in the store row.

Because Loom passes `--session-id`, the transcript path is deterministic:
`$CLAUDE_CONFIG_DIR/projects/<encoded-cwd>/<sessionID>.jsonl`, where the
encoding maps every non-alphanumeric rune to `-`. This is what makes
observation possible without any cooperation from Claude.

`claude --resume <id>` **appends to the same file under the same id** (spike
verified, `docs/spikes/2026-07-02-session-id-spike.md`). So a resume mints a
new tmux name but keeps the claude id — which is why identity resolution
throughout the codebase goes *through* `ClaudeSessionID`, not the tmux name.

**The id can change under a live pane.** `/clear`, or `claude --resume` typed
into the pane, mints a new session id and a new transcript file, and nothing
in the old file says so. Claude's own registry (`~/.claude/sessions/<pid>.json`)
is the only place the pane's current id is written, keyed by the tmux target
Claude observed itself in. `status.Engine.Poll` reads it once per pass and
rebinds the row (`SetClaudeSessionID`, drop the cached reader) when it drifts;
the registry's cwd is deliberately *not* adopted, so a `/clear` never moves a
session between projects.

### 4.2 Launch

`session.Launcher.Launch` — create tmux session → write store row → (if
seeded) spawn `seedWhenReady` goroutine. The argv:

```
claude --session-id <uuid> --settings '{"theme":"light"}' [--model X] [--permission-mode Y] [--add-dir …]
```

The injected `--settings` theme is a GUI concern leaking down for a real
reason: xterm.js can't remap Claude's 256-color output, so Claude's theme must
match Loom's terminal theme or the text is illegible. It's merged over the
user's global config, never mutating it (`session.SetClaudeTheme`, guarded by
an RWMutex so a launch can't race a pref change).

A multi-repo launch (§5 of the projects spec) keeps the topmost selected repo
as cwd and passes the rest as `--add-dir`; the row stores them in `add_dirs`,
and visibility (§8) is evaluated over the whole set.

### 4.3 The seed gate

A seed is typed into the pane by tmux `send-keys -l --` **only once Claude's
input box exists**. `waitReady` polls the pane for `❯`, but:

> **`❯` is not unique to the ready prompt.** The first-run trust dialog's
> `❯ 1. Yes, proceed` cursor line contains it too. So `TrustMarker` is tested
> *before* `ReadyMarker` on every iteration, and while the trust dialog is up
> the timeout clock is **paused** — the user must answer it; Loom never
> answers for them.

Seed outcome is never silently dropped: `seed_status` is written `sent` or
`failed`, and the UI surfaces `· seed failed`.

### 4.4 Attach

- **CLI:** `loom attach <name>` hands the terminal to `tmux attach-session`
  (`TMUX`/`TMUX_PANE` stripped so it works from inside the user's own tmux).
- **GUI:** `ptyRegistry` runs the same `tmux attach` command under a PTY
  (`creack/pty`), streams output base64-encoded over Wails events
  (`pty:data:<name>`), and pipes keystrokes back. The registry is idempotent,
  race-guarded on double-attach, and **never touches the tmux session** —
  closing a tab kills only the attach client.

---

## 5. Status: how Loom knows what a session is doing

Two independent signals, fused.

**Signal A — the transcript classifier** (`transcript/classify.go`) folds
JSONL lines into a turn-boundary state. The critical rule: **never classify
on a sidecar record.** Real transcripts flush `mode`, `permission-mode`,
`last-prompt`, `ai-title`, `file-history-snapshot`, `system` records *after*
the final turn; treating one as a turn boundary corrupts the state.
`isSidechain` records (subagent output) are likewise not boundaries.

| Record | State |
|---|---|
| assistant with a `tool_use` block | `running` (its result arrives as a *later* user record) |
| assistant, text only | `needs_you` |
| user with a `tool_result` block | `running` (Claude is consuming it) |
| user prompt | `idle` |

It also harvests the `aiTitle` sidecar (persisted, so titles survive restart)
and sums the last turn's `usage` into `CtxTokens` for the context gauge.

**Signal B — tmux pane activity**: `#{session_activity}` within a 3-second
window.

**Fusion** (`status/status.go`) resolves the lag between them:

| Transcript | pane active | Result | Why |
|---|---|---|---|
| running | — | `running` | |
| needs_you | yes | `running` | turn ended in JSONL but the pane still moves — a background task, or mid-render. Debounces a premature bell. |
| needs_you | no | `needs_you` | |
| idle | yes | `running` | streaming; JSONL lags |
| idle | no | `idle` | |
| unknown | yes/no | `running`/`unknown` | |

### 5.1 The reconcile loop (`status.Engine.Poll`)

One pass, mutex-serialized end to end:

1. `tmux list-sessions` → filter to `loom-*`.
2. Per session, probe the pane. **A failed probe counts the session alive** —
   a transient failure previously retired *every* session on that poll.
3. **Adopt orphans before branching on dead** — a tmux session with no store
   row is recorded first, so it can never be reaped unrecorded ("record before
   reap").
4. **Resurrect** a row stuck in a terminal status whose pane is demonstrably
   alive — tmux is the liveness truth.
5. Dead pane (`remain-on-exit` keeps it around with its exit code) → mark
   ended `done`/`error`, *then* kill the session.
6. GC cached transcript `Reader`s for anything not seen alive.
7. `MarkLiveOrphansEnded` retires live rows with no tmux backing — protected
   by a **5s grace window** so a poll racing a session's own launch can't
   retire a newborn.
8. Read Claude's session registry once; rebind any live row whose pane now
   runs a different session id (§4.1).
9. Read live rows, poll each transcript, persist title/status changes, and
   compute `NewlyNeedsYou` **before** persisting the new status — which is
   what makes the notification naturally once-only.

The GUI polls every 1.5s via `ListSessions()` from JS; `loom ls` polls once.

---

## 6. Data model (`~/.loom/loom.db`)

SQLite via `modernc.org/sqlite` (pure Go, no cgo), `journal_mode=WAL`,
`busy_timeout=5000`, `synchronous=NORMAL`, **`SetMaxOpenConns(1)`**. That
single connection is not just a simplification — the FTS rowid-range trick in
§6.2 depends on per-file insert contiguity, which it guarantees.

Migrations are a versioned list applied **one transaction per migration**
(DDL + `PRAGMA user_version` together), with `IF NOT EXISTS` on every CREATE
and an "already applied" check on every ALTER, so a replay from a stale
`user_version` is safe. A crash between the two used to brick the next
`Open()`.

| v | Adds |
|---|---|
| 1 | `sessions` |
| 2 | `sessions.seed_status` |
| 3 | `sessions.title` |
| 4 | `transcripts`, `indexed_files`, `messages_fts` (FTS5) |
| 6 | `idx_transcripts_project` |
| 7 | `projects`, `project_repos` (hidden / solo / missing) |
| 8 | `sessions.add_dirs` |
| 9 | `projects.collapsed` |
| 5, 10–25 | tables and columns of the parked arcs (§6.4) |
| 26 | `ignored_roots` — roots removed from the dock; the reconciler skips them, `UpsertProject` clears them |

### 6.1 `sessions` — Loom's own launches

Keyed by tmux name. Terminal statuses are `done`/`error`; live is
`running|needs_you|idle|unknown`. Several rows can share a
`claude_session_id` (one per resume) — `GetLatestByClaudeSessionID` picks the
newest, which is the identity primitive resume-collision detection relies on.

### 6.2 The memory index — three tables, one idea

`transcripts` (one row per *claude* session — the distillation),
`indexed_files` (per source **file**: size/mtime fingerprint + the contiguous
FTS rowid range it owns), `messages_fts` (FTS5 over doc text).

Per-file rather than per-session, because **subagent files arrive while the
parent session is still live**. Re-indexing a changed file is:
delete its old rowid range → insert new docs → record the new range, all in
one transaction. That's an indexed range delete instead of a full FTS scan.

Attribution: main files own `title/ask/outcome/first_ts/msg_count/cwd`;
subagent files (attributed to the parent — their containing directory name)
merge **only** `files` (parent's first, deduped) and extend `last_ts`.
`msg_count` stays main-file-only so the UI's message count isn't inflated.

A synthetic `role="meta"` doc at the pseudo-path `loom://meta/<session_id>`
carries title + touched-file list into FTS — otherwise "which session touched
`reader.go`" would never match, since titles and file lists are distillation
fields, not docs. The pseudo-path is never stat'ed (the `file_missing` sweep
iterates `transcripts`, not `indexed_files`).

Rows whose main file has vanished are **kept** and flagged `file_missing` —
the index *is* the memory, and it outlives Claude's own pruning.

### 6.3 Search SQL — the one clever query

```sql
WITH hits AS MATERIALIZED (
  SELECT session_id, snippet(messages_fts, 0, char(1), char(2), '…', 12) AS snip, rank AS r
  FROM messages_fts WHERE messages_fts MATCH ?
)
SELECT h.session_id, h.snip, min(h.r) AS best, t.title, …
FROM hits h JOIN transcripts t ON t.session_id = h.session_id
GROUP BY h.session_id ORDER BY best LIMIT ?
```

The naive `GROUP BY` + `snippet()`/`bm25()` is rejected by SQLite.
`MATERIALIZED` is load-bearing: it forces the CTE into a temp table before the
outer aggregate, which makes `snippet()` legal. The bare `h.snip` alongside
`min(h.r)` rides SQLite's documented **argmin guarantee** — with exactly one
bare `min()`, other bare columns come from that same row. So each session
yields its single best-ranked snippet.

The one entry point takes sanitized free text — every term quoted, trailing
`*` on the last for as-you-type prefix matching — and
(`SearchSessions`) swallows FTS syntax errors into "zero results" rather than
surfacing an error.

### 6.4 Parked tables

Migrations 5 and 10–25 create tables and columns that no code reads any more
(one extra column each on `sessions` and `projects`, and ten tables of their own).
They stay for two reasons: migrations are append-only, so a DB at head must
keep opening without a rewrite, and the arcs they served are parked at the
tag `parked-arcs`, not abandoned. `TestMigrationsReplayFromEveryStaleVersion`
still probes them with raw SQL. A future migration may drop them; nothing
should be built on them.

---

## 7. Concurrency & failure ledger

The comments in this codebase encode red-team findings. The guards worth
knowing:

| Guard | Protects against |
|---|---|
| `Engine.mu` around all of `Poll` | concurrent-map-write crash on `e.readers` |
| `orphanGrace` (5s) | a poll racing a launch retiring a newborn session |
| resurrection branch | a launch/reconcile race stranding a live session in a terminal status forever |
| alive-on-probe-failure | a transient `list-panes` failure retiring *every* session |
| adopt-before-reap | a tmux session dying unrecorded |
| registry rebind (§4.1) | a row tailing an abandoned transcript forever after `/clear` |
| `sendPendingSeed` re-read | double-delivering a seed when a 60s gate wait races a manual retry |
| `Indexer.sweepMu` | overlapping sweeps contending over rowid ranges |
| `ptyRegistry` double-attach check | two PTYs on one session; killed children are reaped |
| debounce generation counters | a stale search result overwriting a newer one |
| `cmd.WaitDelay` in the summarizer | an orphaned grandchild wedging `Wait()` forever |
| `lastRes` fallback in the GUI resolver | a transient DB error un-hiding a project mid-screen-share |

Two `ensureLocale`/`hydrateLocale` defenses exist because tmux keys UTF-8 off
`LC_ALL`/`LC_CTYPE`/`LANG`, and a Finder-launched bundle has none: in non-UTF-8
mode tmux replaces multibyte glyphs *and control characters* with `_` —
including the `\t` separators in `-F` format output, which silently mangles
every parsed session name.

---

## 8. Projects, attribution and hiding

A **project** is a root directory plus member repos (`projects`,
`project_repos`). `registry.Discover` finds candidates under
`$LOOM_WORKSPACE`; `projects.Service.Reconcile` folds them into `loom.db`
without clobbering user-set state (name, hidden, solo, collapsed), and a
failed scan is non-fatal — everything already reconciled stays launchable.
`Ungrouped` is a real reserved row, not a computed bucket, so every frontend
has one code path.

**Attribution is computed from paths, never stored.** A session belongs to the
project whose root or repo contains its cwd. The display label on the row
(`ProjectLabel`, `filepath.Base(cwd)` for adopted orphans) is never an
attribution input — two same-basename repos in different projects are
routine in a multi-repo workspace.

**Hiding fails closed.** `projects.Resolver.Visible(dirs…)` evaluates a
session's whole directory set (cwd ∪ `add_dirs`): if *any* lands in a hidden
project, or none can be attributed while anything is hidden, the session is
not shown. Solo is "hide everything else". The predicate is applied at the
DTO/row layer of each surface — rail, Finished, search, needs-you count,
title, dock badge, notifications, launcher targets, palette, auto-summarize —
not in the engine, which keeps polling hidden sessions (§6.2a of the spec).
The GUI keeps the last resolver that built successfully and falls back to it
on a read error, so a transient DB hiccup cannot reveal a hidden client.

---

## 9. The recall marker

The launcher's recall panel (a manual pull of related past sessions into the
seed) was cut with the TUI. What remains is its echo-chamber guard: seeds it
appended carry the ` ── Related prior work [` marker (`memory.RecallMarker`),
and the extractor still cuts user text at its first occurrence before
computing docs, ask, or the ask-fallback, so transcripts seeded that way never
index the pulled-in context as the session's own words. The last recall
implementation is at the git tag `parked-arcs`.

---

## 10. Summarization — the first outbound LLM call

`memory.Summarizer` runs a **hardened** `claude -p` child over one session's
indexed docs. The argv is spike-verified and treated as binding
(`docs/spikes/2026-07-03-summarizer-flags-spike.md`):

```
claude -p <prompt> --model haiku --no-session-persistence --tools ""
       --strict-mcp-config --mcp-config '{"mcpServers":{}}'
       --disable-slash-commands --setting-sources ""
       --exclude-dynamic-system-prompt-sections
```

The threat model is explicit: **the transcript content is untrusted input**,
so the child is disarmed of tools, MCP, slash commands, settings, hooks,
plugins, and per-machine dynamic prompt sections; `CLAUDECODE`/`CLAUDE_CODE_*`
are scrubbed from its env; content travels on **stdin** while the `-p` prompt
frames it as data to summarize, never instructions to follow.

Payload budget (40k chars): all user docs first, then assistant docs sampled
at an even stride to fill the remainder; agent docs excluded; head+tail 3:1
fallback if user docs alone overflow; UTF-8-safe cuts everywhere. Nothing is
stored on failure. It is user-triggered from a session's detail; the GUI adds
an opt-in background auto-summarize that does **one at a time, once per
session per process**, and skips hidden projects without marking them tried.


## 10a. Briefs — the second outbound LLM call

A **brief** is a deep, per-task account of what was done in a project over
24h / 3d / 5d / 1w, written to be studied at the end of a day (GUI: a project's
**Brief**; CLI: `loom brief <project> [--window …] [--evidence | --list]`).

The split of work is the point. **Go establishes the facts; Claude writes them up.**

1. **Evidence (Go, no LLM).** `brief.Collect` builds a ledger:
   - Transcripts: every main and subagent transcript with a record in the
     window. A record belongs to the project if *its own* `cwd` is under the
     project (the session row's launch dir is not used).
   - What is kept per session: typed asks, delegated prompts, every version of
     the task file, notable commands with their result tails, lava emits,
     Claude's prose and compact summaries.
   - Each entry gets an id `S<n>:L<line>`.
   - Git facts: commits in the window from every repo and worktree, deduped
     by SHA across clones, split into "yours" and "others" by author email,
     with stat and patch excerpts; branches whose tip moved, with one honest
     state each; dirty worktrees.
   - Merge state: ancestry says "merged" only for merges and fast-forwards. A
     squash merge is detected by asking the forge (`gh pr list` / `glab mr
     list`, read-only, at most 20 lookups). Any failure leaves the state
     unknown, never "unmerged". loom never fetches.
   - The ledger is trimmed to a ~420k-char budget by water-filling: anchors
     (task files, commands, commits) are kept, and each session keeps its head
     and tail.
2. **Writing (Claude).** `brief.Writer` runs the summarizer's hardened argv
   (§10), but with `--model opus` and a 15-minute timeout. The ledger goes on
   stdin.
   - The prompt requires a fixed structure: Overview, then one
     `## Task:` section per task (Outcome, Status, problem, how it works now,
     plan vs shipped, proof, risks, how to explain it, likely questions,
     provenance).
   - Every claim cites ids and is tagged `[git]`, `[transcript]`,
     `[inferred]` or `[unverified]`. Status comes only from git facts.
3. **Archive.** Each brief lives in
   `~/.loom/briefs/<name>-<hash of root>/<stamp>-<window>/`:
   - `brief.md`: the text, followed by the git facts and the evidence index,
     both verbatim;
   - `evidence.md`;
   - `meta.json`;
   - `brief.html`, written on demand.

   These are files, not a table. Ids are validated before any path is built.
4. **Reading.** goldmark renders the text with raw HTML escaped; model text
   never becomes markup. The reader splits the text at `## Task:` headings,
   ignoring headings inside code fences. **Make artefact** re-renders the same
   text as a self-contained page with light and dark styles, print styles and
   a table of contents. No second model call.
5. **Ask about this / Review** opens an interactive study session for one
   task, with the **fence** (`Recipe.Fenced`, spike-verified in
   `docs/spikes/2026-10-18-brief-fence-spike.md`):
   - Tools: `--tools Read,Grep,Glob`.
   - MCP: `--strict-mcp-config` with no servers, which removes the claude.ai
     connectors that `--tools` alone leaves in place.
   - Settings: `--setting-sources ""`, so no hooks and no settings-defined MCP
     servers.
   - Secrets: deny rules in `--settings` for `.env*`, keys, cloud and forge
     credentials.
   - Reach: the project's repos plus the brief's directory (`--add-dir`).
   - Resume: the row is tagged `fenced`, so a resume keeps the fence.
   - Self-exclusion: the seed carries `[loom brief]`. Sessions whose first ask
     carries the marker are left out of later briefs, so studying a day does
     not become part of it.

Hidden projects fail closed: no resolver, or not visible, means no brief and no
reading of a saved one. The GUI keeps an estimate's bundle for 5 minutes so
Write uses exactly the evidence the estimate counted, and refuses a second run
of the same project and window while one is writing.

---

## 11. The two frontends

### 11.1 CLI (`cmd/loom`)

`loom ls | attach <name> | kill <name> | search <query>`. One file, no
state: `ls` builds an `Engine` over the shared store and runs a single `Poll`,
`attach` execs `tmux.AttachCmd`, `kill` is `tmux.KillSession` (the GUI's next
poll retires the row), `search` is `store.SearchSessions`. It exists so a
shell or a script can see and reach sessions without the app; it does not
launch sessions, and there is no plan for it to.

### 11.2 GUI (`cmd/loom-gui`) — Wails v2 + xterm.js

The Go side is a thin **bridge**: `App` owns nothing beyond the PTY registry
and the settings store; every bound method is a DTO-shaped call over the same
engine, with `defer recover()` on the poll paths so a half-built engine
degrades to an empty list rather than crashing the window. Surfaces: the
rail (projects · threads · stage), the launcher with multi-repo targets, the
project overview (rename, re-point, add/remove repo, hide/solo, clean repo),
search, the diff panel, preferences, the ⌘K palette, and the Agent
Discipline island behind the shield token on the dock footer (`discipline.go`:
the doctor's report cached by an mtime signature, the gate sheet that
stages, runs the gate and commits only on a click after PASS, Diff / Keep /
Reset for local edits, uninstall from the receipt; see `docs/DISCIPLINE.md`; `hooks.go`:
the per-session holds and held chip on the rail, the ✓/✗ on the finish-line
pill, the hooks panel, and the island's Rates and Re-runs tabs over
`internal/hooklog`; the Checks tab's Context diet section over
`internal/diet`).

Native touches, each with a real reason:

- `hydratePATH` merges the login shell's `PATH` (bounded 3s) + Homebrew +
  `~/.local/bin` — Finder gives an app `/usr/bin:/bin:/usr/sbin:/sbin`, which
  has neither `tmux` nor `claude`.
- Dock badge via cgo/AppKit, dispatched to the main queue (`dockbadge_darwin.go`,
  with a no-op sibling for other platforms).
- Notifications via `osascript`, with AppleScript-literal escaping of session
  labels so a title can't break out of the script.
- ⌘-click on terminal text: file paths resolve against the session cwd and open
  in Cursor/Code/Zed (plus an `open -a` to actually raise the window, since
  macOS won't transfer foreground activation); URLs are gated to `http(s)` only.
- `SessionDiff` shells out to `git diff HEAD` (`internal/gitdiff`) for an
  in-app review panel, sectioned per repo for multi-repo sessions.
- `RepoCleanupPreview`/`RepoCleanupRun` (`internal/cleanup`) delete only paths
  that are both git-ignored and junk-patterned; tracked files and `.env`-style
  configs are structurally unreachable.
- Settings persist atomically (temp + rename), loaded over a defaults base so
  an old file with missing keys keeps sane values.

Frontend is dependency-light: vanilla ES modules + xterm.js, the "Islands"
look (peach on warm plum, dark first; borderless islands on a ground, an icon
dock for projects) as tokens in `tokens.css` with status colours as `--st-*`
variables that `theme.js` hands out, one theme preference for chrome, terminal
and Claude (mirrored into Claude via `--settings`), Unicode v11 width addon so emoji measure 2 cells like tmux and
Claude's own TUI. Vite empties `dist/` on every build and rewrites
`dist/.gitkeep`, so `//go:embed all:frontend/dist` never bakes a stale chunk
into the binary and still compiles on a checkout that has not built the
frontend.

---

## 12. Design principles, stated plainly

1. **Never own what you can observe.** Sessions live in tmux; transcripts
   belong to Claude. Loom holds only derived state, and can rebuild it.
2. **Degrade the label, never the session.** Every inference path — status
   fusion, extraction — has a defined "unknown" outcome that lets work
   continue.
3. **Nothing silently auto-injects.** Seeds are what the user typed.
   Summaries cost quota, so they're opt-in.
4. **Failures must be visible.** `seed_status`, `file_missing`,
   `· seed FAILED`, `(pane unavailable)` — the codebase consistently prefers a
   visible bad state to a silent drop.
5. **Hiding fails closed.** A session that cannot be attributed is not shown
   while anything is hidden; a read error keeps the last good resolver.
6. **Comments carry the *why*, including the wrong answer.** Most non-obvious
   lines cite the spec section or the red-team finding that produced them.
   This is why the code is readable years later — preserve the habit.

---

## 13. Known limits (v1)

Search is lexical only (no embeddings). macOS-only in practice (Dock badge, `osascript`, `open`). Index
freshness is bounded by the 10-minute sweep. Scrollback inside a session is
tmux copy-mode, not native scroll — the one deliberate deviation from raw
`claude`. Claude's session registry is undocumented; the reader is tolerant,
and a missing registry simply means no rebind.

**There is deliberately no headless or scripted launch path, and there isn't
going to be one.** Loom is a cockpit for a human who is present.

---

## 14. Where to look

| Question | File |
|---|---|
| How is a session started? | `internal/session/{recipe,launch}.go` |
| Why does a row say "running"? | `internal/transcript/classify.go` + `internal/status/status.go` |
| Why did a row disappear / come back / change id? | `internal/status/engine.go`, `internal/ccsessions/ccsessions.go` |
| What's in the DB? | `internal/store/store.go` (migrations), `memory.go`, `projects.go` |
| How is search built? | `internal/store/memory.go` (`searchSQL`) |
| What gets indexed, and what's filtered out? | `internal/memory/extract.go` |
| Who can see what? | `internal/projects/resolver.go`, `cmd/loom-gui/session_dto.go` |
| Design specs & plans (historical record) | `docs/superpowers/specs/`, `docs/superpowers/plans/` |
| Bundling the Agent Discipline Handbook (installer, doctor, GUI surfaces) | `docs/DISCIPLINE.md` (plan, not yet built) |
| Empirical findings about Claude's CLI | `docs/spikes/` |
| The parked arcs | `git show parked-arcs` |
