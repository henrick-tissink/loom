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
> the tree at commit `fac253a` (2026-07-14); `go test ./...` green across all
> 12 packages.

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
| **Observation is read-only** | Status comes from `~/.claude/projects/**/*.jsonl` (Claude's own transcripts) + tmux pane liveness. Loom never instruments, patches, or intercepts Claude. |
| **Two sources of truth, cleanly split** | tmux owns *liveness*; `~/.loom/loom.db` owns *history*. Reconciliation may retire a row, never delete one. |

The corollary that shapes half the codebase: **Loom's model of a session is
always inferred, never authoritative.** So every inference is written to
degrade — a bad status label, never a lost session.

---

## 2. Process & deployment shape

Two binaries over one shared engine:

```
cmd/loom      →  Bubble Tea TUI  (internal/ui)      ─┐
                                                     ├─→  internal/{status,session,store,memory,workflow,tmux,…}
cmd/loom-gui  →  Wails v2 + xterm.js (frontend/)    ─┘
```

Both `main()`s do the same wiring: check binaries → load config → ensure tmux
server → open store (migrate) → discover projects → start the indexer
goroutine → build `Launcher`, `Engine`, `Summarizer`, `Runner` → run the
frontend. The GUI adds `hydratePATH()`/`hydrateLocale()` first, because a
Finder-launched bundle inherits neither a usable `PATH` nor a locale.

**Two Loom instances against one DB is a supported state** — hence WAL,
`busy_timeout`, and the CAS discipline in §6.

### Filesystem contract

| Path | Owner | Role |
|---|---|---|
| `~/.loom/loom.db` | Loom (rw) | sessions, transcripts index, FTS, workflow runs |
| `~/.loom/workflows/*.json` | user (rw) | workflow definitions |
| `~/.loom/settings.json` | GUI (rw, atomic) | editor, notifications, auto-summarize, terminal theme |
| `~/.claude/projects/**` | Claude (Loom: **read-only**) | transcripts — main + subagent |
| `~/.claude.json` | Claude (Loom: read-only) | per-project trust flags |
| `$LOOM_WORKSPACE` (default `~/Sauce`) | user | project discovery root |

`$CLAUDE_CONFIG_DIR` overrides `~/.claude` everywhere (`internal/config`).

---

## 3. Package map

Dependency direction is strictly downward; nothing in `internal/` imports a
frontend.

```
   cmd/loom (TUI)                    cmd/loom-gui (Wails)
        │                                    │
   internal/ui  ───────────────┬──────────────┘
        │                      │
   internal/workflow ──────────┤
        │                      │
   internal/status ────────────┤        internal/memory
        │        │             │          │        │
   internal/session   internal/transcript │   internal/store
        │                                 │        │
   internal/tmux                internal/registry, trust, config
```

| Package | Responsibility | Notes |
|---|---|---|
| `tmux` | Every interaction with the `-L loom` server | Only place that shells out to `tmux`. Exact-match targets (`=name`), locale defense. |
| `session` | Launch recipe + orchestrated launch/resume | Owns tmux naming, argv construction, the ready/trust gate, seed delivery. |
| `transcript` | Locate + interpret Claude's JSONL | Path encoding, incremental `Reader`, turn-boundary `Classifier`. |
| `status` | Fuse transcript state with tmux liveness | The reconcile loop. The only writer of live-session status. |
| `store` | SQLite: schema, sessions, memory index, runs | Migrations are transactional & re-entrant. |
| `memory` | Index / extract / recall / summarize | Reads the whole Claude archive; the only package that calls out to an LLM. |
| `workflow` | Definitions + CAS-guarded runner | Multi-step chains of real sessions. |
| `registry` | Workspace project discovery | `.git` or an existing transcript dir; one nesting level. |
| `trust` | Read Claude's trust flags | Purely defensive — never answers a trust dialog. |
| `config` | Paths, env, binary preflight | |
| `ui` | Bubble Tea app: 13 views, one model | ~2.9k lines; the TUI's whole state machine. |

Test mass is deliberate: **11,573 test lines vs 10,246 source lines.**

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
throughout the codebase goes *through* `ClaudeSessionID`, not the tmux name
(see `workflow.ResolveStepSession`).

### 4.2 Launch

`session.Launcher.Launch` — create tmux session → write store row → (if
seeded) spawn `seedWhenReady` goroutine. The argv:

```
claude --session-id <uuid> --settings '{"theme":"light"}' [--model X] [--permission-mode Y]
```

The injected `--settings` theme is a GUI concern leaking down for a real
reason: xterm.js can't remap Claude's 256-color output, so Claude's theme must
match Loom's terminal theme or the text is illegible. It's merged over the
user's global config, never mutating it (`session.SetClaudeTheme`, guarded by
an RWMutex so a launch can't race a pref change).

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

- **TUI:** hands off the terminal to `tmux attach-session` (`TMUX`/`TMUX_PANE`
  stripped so it works from inside the user's own tmux). Polling pauses while
  attached — hence "no bells while attached".
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
8. Read live rows, poll each transcript, persist title/status changes, and
   compute `NewlyNeedsYou` **before** persisting the new status — which is
   what makes the notification naturally once-only.

TUI polls every 1.5s; the GUI polls via `ListSessions()` from JS.

---

## 6. Data model (`~/.loom/loom.db`)

SQLite via `modernc.org/sqlite` (pure Go, no cgo), `journal_mode=WAL`,
`busy_timeout=5000`, `synchronous=NORMAL`, **`SetMaxOpenConns(1)`**. That
single connection is not just a simplification — the FTS rowid-range trick in
§6.2 depends on per-file insert contiguity, which it guarantees.

Migrations are a versioned list applied **one transaction per migration**
(DDL + `PRAGMA user_version` together), with `IF NOT EXISTS` on every object.
A crash between the two used to brick the next `Open()`.

| v | Adds |
|---|---|
| 1 | `sessions` |
| 2 | `sessions.seed_status` |
| 3 | `sessions.title` |
| 4 | `transcripts`, `indexed_files`, `messages_fts` (FTS5) |
| 5 | `workflow_runs` |
| 6 | `idx_transcripts_project` |

### 6.1 `sessions` — Loom's own launches

Keyed by tmux name. Terminal statuses are `done`/`error`; live is
`running|needs_you|idle|unknown`. Several rows can share a
`claude_session_id` (one per resume) — `GetLatestByClaudeSessionID` picks the
newest, which is the identity primitive workflows and resume-collision
detection rely on.

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

Two entry points share it: `SearchSessions` (sanitized free text — every term
quoted, trailing `*` on the last for as-you-type prefix matching) and
`SearchSessionsRaw` (recall supplies its own expression). Both swallow FTS
syntax errors into "zero results" rather than surfacing an error.

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
| `AdvanceRunCAS` (§8) | double-press / two-instance workflow double-advance |
| `sendPendingSeed` re-read | double-delivering a seed when a 60s gate wait races a manual retry |
| `Indexer.sweepMu` | overlapping sweeps contending over rowid ranges |
| `ptyRegistry` double-attach check | two PTYs on one session; killed children are reaped |
| captured `actionTarget` in the TUI | rows reorder every 1.5s poll — kill/tag must act on the row captured at confirm-open |
| debounce generation counters | a stale search/recall result overwriting a newer one |
| `cmd.WaitDelay` in the summarizer | an orphaned grandchild wedging `Wait()` forever |

Two `ensureLocale`/`hydrateLocale` defenses exist because tmux keys UTF-8 off
`LC_ALL`/`LC_CTYPE`/`LANG`, and a Finder-launched bundle has none: in non-UTF-8
mode tmux replaces multibyte glyphs *and control characters* with `_` —
including the `\t` separators in `-F` format output, which silently mangles
every parsed session name.

---

## 8. Workflows

A workflow is a named chain of steps, each an ordinary `claude` session.
**Nothing auto-advances** — the user presses `n`, sees a confirm with the
fully substituted seed, and approves.

**Definitions** (`~/.loom/workflows/<name>.json`) are validated at load:
name matches filename stem; ≥1 step; every model/mode/relation in a known set;
step 1 names a project; every `{{…}}` token whitelisted
(`prev.outcome|title|ask`) so a typo can never ship as literal braces; newline
runs in seeds collapsed at load (a literal `\n` in `send-keys` submits early).
Step `project` is a registry **label** on disk and is rewritten in place to the
resolved **absolute path** by `LoadAll` — the `Runner` has no registry, so
resolution must happen while one is in hand. An empty project means "inherit
the previous step's cwd".

**Relations:**

| Relation | Effect |
|---|---|
| `fresh` | new session, seed as-is |
| `fork` | new session, `{{prev.*}}` substituted from the previous step's transcript extraction |
| `continue` | seed sent into the *current* session (no new session; model/mode can't change — a known v1 limit) |

Substitution reads the previous step's transcript through the same
`memory.ExtractFile` machinery, at **confirm-open time**. Missing values render
as `(unavailable)` rather than blocking; values are capped at 8 KB each and
15 KB assembled, with a visible truncation marker; stray CR/LF is stripped
(again: a newline is an Enter).

**`AdvanceRunCAS` is the concurrency primitive.** Every advance is a
compare-and-swap on `(id, step_idx, status='running')`:

- `continue`: **CAS first** (claiming the step, storing the seed in
  `pending_seed`), then an async goroutine delivers it once the transcript
  state reaches idle/needs-you — because `❯` is meaningless mid-generation, so
  the gate is transcript-derived, not a pane read. `pending_seed` is the
  durable "still owed" record; it survives restart and `n` retries it.
- `fork`/`fresh`: Launch, *then* CAS — `Launcher.Launch` mints its own session
  id and there's no API to hand it a pre-chosen one. The disclosed failure
  mode: a rejected CAS after a successful launch leaves a real, untracked
  session and the run shows its dead-step hint.

A dead `continue` target returns `ErrContinueDead`; the UI offers a one-shot
demotion (`Advance(..., forceFork=true)`). `Finish` and `Abandon` are CAS'd
too, so an abandon confirm opened against a running snapshot can't overwrite a
run that finished meanwhile. **Abandon ≠ kill** — the sessions keep running.

Runs never garbage-collect. Sessions are tagged `wf:<name>#<id>:step<N>`; a
dashboard `r`-resume of a dead step reconnects transparently because
`ResolveStepSession` resolves by claude id, not tmux name.

---

## 9. Recall — memory pulled in, never injected

The launcher's RELATED panel is a **manual pull**. Nothing is ever
automatically injected into a prompt.

`buildRecallQuery` deliberately does *not* reuse `sanitizeFTSQuery`, whose
implicit-AND + trailing-`*` shape returns zero hits for natural-sentence
seeds. Instead: tokenize on non-alphanumerics → drop tokens under 4 chars and
a stopword list → dedupe → quote → **OR**-join, no trailing `*`. Fewer than 2
surviving terms ⇒ empty expression ⇒ fall back to same-project recency.

Then the **≥2-matched-term gate**, counted client-side against the *snippet
and title only*. Excluding the `ask` field is the empirically important part:
asks are often multi-KB pasted blobs where generic vocabulary ("implement",
"settings", "mode") scatters by coincidence and clears the gate for
unrelated sessions. A snippet is a ~12-token window centered on a real match,
so co-occurrence there means something. Survivors rank same-project first,
then matched terms, then bm25 order.

**The echo-chamber guard.** Included entries are appended to the seed as
*visible, literal* text:

```
<your seed> ── Related prior work [project·title]: outcome
```

That marker (`memory.RecallMarker`) is recognized by Loom's own extractor,
which cuts user text at its first occurrence before computing docs, ask, or
the ask-fallback. Pulled-in context can therefore never re-index as the new
session's own words — recall cannot compound across generations. A seed
starting with `/` refuses includes entirely (a slash command's argument line
is no place to glue outcome text).

---

## 10. Summarization — the one outbound LLM call

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
stored on failure. It is user-triggered (`s`) in the TUI; the GUI adds an
opt-in background auto-summarize that does **one at a time, once per session
per process**.

---

## 11. The two frontends

### 11.1 TUI (`internal/ui`) — Bubble Tea

One `App` model, 13 views (`viewDash`, `viewLauncher`, `viewSearch`,
`viewDetail`, `viewWorkflows`, `viewWall`, `viewFanout`, confirms, …), one
`Update` switch. Recurring discipline throughout:

- **Captured targets.** Anything a confirm acts on is captured at open time,
  because rows reorder under the cursor on every poll.
- **Generation counters.** Every debounced/async result carries the seq it was
  issued with; stale ones are discarded.
- **Nil-safe `Deps`.** `Store`, `Summarizer`, `Runner`, `IndexerStatus` may all
  be nil — every path no-ops instead of panicking, so dashboard-only tests use
  a bare `Deps{}`.

### 11.2 GUI (`cmd/loom-gui`) — Wails v2 + xterm.js

The Go side is a thin **bridge**: `App` owns no orchestration beyond the PTY
registry; every method is a DTO-shaped call over the same engine, with
`defer recover()` on the poll paths so a half-built engine degrades to an
empty list rather than crashing the window.

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
- `SessionDiff` shells out to `git diff HEAD` for an in-app review panel.
- Settings persist atomically (temp + rename), loaded over a defaults base so
  an old file with missing keys keeps sane values.

Frontend is dependency-light: vanilla ES modules + xterm.js, a "Blush" palette
in `theme.js`/`tokens.css`, light/dark terminal themes mirrored into Claude via
`--settings`, Unicode v11 width addon so emoji measure 2 cells like tmux and
Claude's own TUI.

---

## 12. Design principles, stated plainly

1. **Never own what you can observe.** Sessions live in tmux; transcripts
   belong to Claude. Loom holds only derived state, and can rebuild it.
2. **Degrade the label, never the session.** Every inference path — status
   fusion, extraction, substitution, recall — has a defined "unknown" outcome
   that lets work continue.
3. **Nothing silently auto-advances or auto-injects.** Workflows wait for a
   keypress with a preview. Recall requires an explicit `space`. Summaries
   cost quota, so they're opt-in.
4. **Failures must be visible.** `seed_status`, `pending_seed`, `file_missing`,
   `· seed FAILED`, `(pane unavailable)`, dim-red load errors — the codebase
   consistently prefers a visible bad state to a silent drop.
5. **CAS at every multi-writer boundary.** Two instances is a supported
   configuration, so "read then write" is never left unguarded.
6. **Comments carry the *why*, including the wrong answer.** Most non-obvious
   lines cite the spec section or the red-team finding that produced them.
   This is why the code is readable years later — preserve the habit.

---

## 13. Known limits (v1)

Recall is lexical only (no embeddings — the interface is upgradeable).
Workflows have no in-app editor, no branching, no auto-advance, no scheduling,
no per-step worktree isolation, and `continue` can't change model/mode. Fan-out
recipes are uniform across the group and there's no group view beyond the tag,
marker, and summary line. The wall is read-only, colorless, left-edge-only, and
only captures the visible page. Workflow run rows never GC. macOS-only in
practice (Dock badge, `osascript`, `open`). Index freshness is bounded by the
10-minute sweep. Scrollback inside a session is tmux copy-mode, not native
scroll — the one deliberate deviation from raw `claude`.

**There is deliberately no headless or scripted launch path, and there isn't
going to be one.** Loom is a cockpit for a human who is present.

---

## 14. Where to look

| Question | File |
|---|---|
| How is a session started? | `internal/session/{recipe,launch}.go` |
| Why does a row say "running"? | `internal/transcript/classify.go` + `internal/status/status.go` |
| Why did a row disappear / come back? | `internal/status/engine.go` |
| What's in the DB? | `internal/store/store.go` (migrations), `memory.go`, `runs.go` |
| How is search built? | `internal/store/memory.go` (`searchSQL`), `internal/memory/recall.go` |
| What gets indexed, and what's filtered out? | `internal/memory/extract.go` |
| Workflow semantics | `internal/workflow/{def,run}.go` |
| Design specs & plans (historical record) | `docs/superpowers/specs/`, `docs/superpowers/plans/` |
| Empirical findings about Claude's CLI | `docs/spikes/` |
