# Loom — Orchestrator Session & Brief Assembly — Design

**Status:** Revision 2 (hardened after adversarial review: 3 findings folded)
**Date:** 2026-07-22
**Scope:** Slice 2 of 4 in the orchestration arc. A per-project **orchestrator session** launched at the project root, seeded from a **brief Loom assembles** out of durable on-disk artifacts plus the memory index; a fixed, small set of orchestrator-authored notes that constitute the project brain; drift detection between those notes and the repos. NO delegation, NO child sessions, NO rendezvous (slice 3). NO architecture or dependency-graph rendering (slice 4).

Builds directly on slice 1 (`2026-07-22-projects-foundation-design.md`, built and merged at `97dc269`/`9b69827`): projects as named roots owning repos, `internal/projects` as the single attribution/visibility authority, `Recipe.AddDirs` persisted, roots launchable, and the deliberately-empty `#po-arch` seam in the GUI project overview. Slice 1 §11's evidence constraints are binding here and are answered explicitly in §15.

---

## 1. What this delivers

Selecting a project and pressing **Spawn orchestrator** launches one real `claude` at the project root, with every out-of-root member repo granted via `--add-dir`, seeded with a two-line pointer to a brief Loom wrote to disk. The brief carries: the project's repo set with current branch/HEAD/dirty state, the project's own notes verbatim, the last N sessions across *all* the project's repos (title + outcome, one line each, from the existing memory index), and a drift report saying what moved since an orchestrator was last briefed.

The orchestrator writes back to exactly three files. Those files — not the session — are the project brain. Kill the session, respawn it a week later, and it reads itself back into the same position plus a list of what changed while it was gone.

Loom spends **zero LLM quota** assembling any of this. Assembly is local file reads, `git rev-parse`, and SQLite.

## 2. Model (binding)

- An **orchestrator** is an ordinary `store.SessionRow` — a real `claude` in tmux, observed the same way as any other session. It gets no special status path, no engine awareness, no privileged transcript handling. `status.Engine` still never learns about projects (slice 1 §6.2a).
- **At most one orchestrator per project at a time**, enforced by a primary key, not by a UI guard (§9). Two Loom instances is a supported state.
- **Ungrouped (`root=''`) can never have an orchestrator.** There is no directory to launch at and no coherent repo set. The spawn action is disabled and the backend rejects it.
- An **initiative is not an orchestrator either.** Slice 1 §2 binds "initiative = orchestration *run* scoped to a subset of one project's repos". Slice 2 ships the *per-project* orchestrator only; run-scoping is slice 3's, and the orchestrator is the thing a run will be requested *from*, not the run itself. Nothing in this slice may key on a run id.
- **The orchestrator is disposable.** Its transcript is a byproduct. Every durable claim it makes must land in a file (§3) or it does not exist. This is the same commitment as ARCHITECTURE.md §12.1 ("never own what you can observe"), applied one level up.
- **The orchestrator's notes are unverified prose.** Nothing checks them against the repos. Slice 3 must not treat a line in `loom-decisions.md` as a contract a child can be held to; §11's "a child's done is an executable check on a published artifact" is not satisfied by anything in this slice, and slice 2 does not pretend otherwise (§15).

## 3. Where the durable artifacts live (binding)

Slice 1 established that **Loom writes nothing into the user's workspace except on explicit request**, and deleted its own "Loom creates the directory" branch to keep that absolute. The orchestrator's notes are useful to humans and to claude and belong conceptually next to the code. Those two pull opposite ways. The resolution is a split by **author**, not by usefulness:

> **Loom writes only under `~/.loom/`. The agent writes only under the project's `notes_dir`. Neither ever writes where the other does.**

Concretely:

| Path | Written by | Contents |
|---|---|---|
| `~/.loom/projects/<key>/brief.md` | Loom | the assembled brief (§4). Overwritten every spawn. |
| `~/.loom/projects/<key>/state.json` | Loom | drift oracle + spawn ledger (§8). Machine-only. |
| `<notes_dir>/loom-map.md` | the orchestrator | what the repos are and where the seams between them run |
| `<notes_dir>/loom-decisions.md` | the orchestrator | append-only decision log: date · decision · why · repos affected |
| `<notes_dir>/loom-open.md` | the orchestrator | open questions and next steps |

`<key>` = `<sanitized basename of root>-<sha256(root)[:8]>`. The hash disambiguates two projects with the same basename; the basename keeps the directory greppable by a human.

**`notes_dir` is a new column on `projects` (§9), empty by default.** When empty, the first spawn **materializes** it to `~/.loom/projects/<key>/notes/` and writes that literal path into the row. Materializing rather than deriving-on-read is load-bearing: `RepointProject` changes `root`, and a derived default would silently relocate a project's whole brain on a directory rename. The stored path survives a re-point untouched.

**Putting notes in the repo is a gesture, never a default.** The project overview offers *Move notes…* → the existing `OpenDirectoryDialog`, typically to `<root>/docs/loom/`. That is the "explicit request" the invariant carves out — the user picked the directory in a file dialog. Moving copies the three files and rewrites `notes_dir` in one transaction; the old location is left in place, not deleted (Loom retires, never deletes — ARCHITECTURE.md §1).

**Rejected alternatives, with reasons:**

- **Write notes into `<root>/.loom/` by default.** This is Loom causing a workspace write on a gesture the user did not make. It also lands in `git status` and in slice 1's sectioned `SessionDiff`, so every review panel in a project with an orchestrator grows noise the user never asked for. Rejected.
- **Keep the brain in `loom.db` as blobs.** Rejected on three counts: claude reads files natively and needs no tool Loom would have to build; a human cannot diff, review, or PR a blob; and it makes Loom the *owner* of content it does not author, which is the inversion §12.1 exists to prevent.
- **Name the files `ARCHITECTURE.md` / `DECISIONS.md`.** Rejected: `<root>/docs/` is exactly where a human already keeps files with those names — this repo included. The `loom-` prefix makes an accidental collision with hand-written docs structurally impossible and makes "which files does the orchestrator own" answerable by `ls loom-*`.
- **Let the orchestrator choose its own file layout.** Rejected: a fixed, three-file layout is what makes §8's digest-based drift detection possible at all, and what lets a *fresh* orchestrator know where to look without being told by its predecessor.

**Pre-existing files.** If a target file already exists at `notes_dir` and is unknown to `state.json`, assembly includes it verbatim like any other note and the brief labels it `pre-existing — treat as authoritative, do not rewrite wholesale`. Loom never truncates or moves a file it did not create.

## 4. The brief — content and budget (binding)

`brief.md` is one markdown file with a **hard 64 KB cap**, sections in a fixed order, each with its own cap. Order and caps are binding: the authorization scope must be first and last, and the sections a truncation eats must be the least load-bearing.

| # | Section | Cap | Source |
|---|---|---|---|
| 0 | `LOOM-BRIEF` label line (§5.4) | 1 line | constant |
| 1 | `## Authorization scope` | 1 KB, **never truncated** | constant + `notes_dir` + repo list |
| 2 | `## Project` | 4 KB | `store.Project` + `ListProjectRepos` + `git` (§5.1) |
| 3 | `## Drift` | 4 KB | `state.json` vs now (§8) |
| 4 | `## Notes` | 24 KB | the three files at `notes_dir` |
| 5 | `## Recent work` | 16 KB, ≤40 rows | memory index (§5.2) |
| 6 | `## What to do` | 4 KB | user intent at spawn + standing instruction |
| 7 | `## Authorization scope (repeated)` | 1 KB | identical to §1 |

**§1 Authorization scope** is verbatim-fixed text plus three substituted lists. Slice 1 §11 is explicit that removing scope text measurably raises overreach, so it is present, it is first, it is repeated last (recency), and it is the one section a truncation may never touch. Its claims:

- the only directory you may write to is `<notes_dir>`, and within it only `loom-map.md`, `loom-decisions.md`, `loom-open.md`;
- these repos are readable: `<paths>`. Nothing outside them is in scope;
- you may not commit, push, rebase, or run destructive commands in any repo;
- you may not start, resume, or kill other sessions. Delegation does not exist yet — if the work needs to be split, write the split into `loom-open.md` and stop;
- if you believe you need something outside this scope, say so and stop. Do not route around it.

**§2 Project** is name, root, and one line per repo: `label · path · branch · HEAD[:8] · N dirty` (plus `missing` where slice 1 flagged it). Nothing else. No file listings, no language detection, no README.

**§4 Notes** is the three files verbatim **iff** they total ≤24 KB. Over that, each is included head-truncated to 2 KB with `…[truncated] — read the rest at <abs path>` (the workflow truncation idiom, `internal/workflow/run.go:51`). Verbatim inclusion is a latency optimization, not a necessity: the files are on disk inside an `--add-dir`, and the agent can always read them itself.

**§5 Recent work** is one line per prior session: `2026-07-14 · bankenstein · <title> — <outcome>`. See §5.2.

**§6 What to do** is the user's typed intent (≤600 chars, `memory.CleanText`'d) plus, always, the standing instruction: keep the three notes current, reconcile anything listed under Drift before starting new analysis, and record every decision in `loom-decisions.md` as it is made rather than at the end.

**Truncation is visible.** Any section that was cut says so inline, and `state.json` records `brief_bytes` and `truncated_sections`, which the overview renders (§10). A silently short brief is the failure mode this exists to prevent.

## 5. Assembly

New package `internal/orchestrator` (`assemble.go`, `state.go`, `spawn.go`). It depends on `projects`, `store`, `memory`, `session`, and shells out to `git`. It is below both frontends; `internal/projects` stays a pure resolver and gains nothing.

### 5.1 Repo state

`git rev-parse --abbrev-ref HEAD`, `git rev-parse HEAD`, and `git status --porcelain | wc -l` per repo, reusing the shell-out discipline already in `cmd/loom-gui/diff.go` (bounded context, errors degrade to a rendered `(unavailable)`, never fatal). A repo flagged `missing` by slice 1's sweep is listed as missing and not stat'ed.

### 5.2 Recent work — reuse, with one extension

The memory index already holds exactly what is wanted: per-session `Title`, `Ask`, `Outcome`, `Files`, `Cwd`, `ProjectDir`, all `CleanText`'d and single-line by construction. Assembly:

1. Build the dir set = `{project root} ∪ {member repo paths}` (slice 1's target set, minus the Ungrouped guard).
2. **If the user typed an intent at spawn**, run recall's existing ranking with that intent as the seed, over the dir set. **Otherwise** fall back to recency over the dir set — which is precisely `recall.go`'s existing fallback branch, not a new code path.
3. **Drop every `orch`-tagged session** (§5.4) — `transcripts.session_id` joins `sessions.claude_session_id`, and `store.SessionRow.Tags` (`store.go:259`) already carries the tag. This is the echo-chamber guard and it is not optional.
4. Merge, dedupe by `session_id`, sort by `last_ts` desc, cap at 40 rows and 16 KB.
5. Per row, render `Outcome`; if empty, render `Ask` **only if `memory.AskUsable(ask)`** (the existing filter that keeps `<command-…>` wrappers and `Caveat:` preambles out); otherwise render `(no outcome recorded)`.

The extension: `memory.Related` and `store.RecentTranscriptsByProjectDir` are single-dir. Add `memory.RelatedIn(st, dirs []string, seed string, limit int)` and `store.RecentTranscriptsByProjectDirs(dirs []string, limit int)`, with `RelatedHit.SameProject` true when the hit's `ProjectDir` is anywhere in the set. Single-dir callers are unchanged (`Related` becomes a one-element wrapper). **This pays down slice 1 §10's noted follow-on** — "recall's same-project boost still operates on the repo dir, not the project" — for the orchestrator path, and the launcher's RELATED panel can adopt it later for free.

**Hidden projects contribute nothing.** Rows are filtered through `projects.Resolver.Visible` before ranking, so a brief assembled while another project is hidden or solo is active cannot quote it. Fail-closed inherits from slice 1 §4: an unattributable row is dropped, not included.

**`LLMSummary` is never used in a brief.** It is multi-KB by design; forty of them would exceed the whole budget, and generating missing ones at assembly time would be new Loom-initiated LLM spend, which slice 1 §6.2b already forbids for hidden projects and which principle §12.3 ("nothing silently auto-injects; summaries cost quota, so they're opt-in") forbids generally.

### 5.3 Determinism

Assembly is a pure function of (project row, repo rows, git state, notes files, memory index, intent). Same inputs ⇒ byte-identical brief. This is what makes §13's golden-file test meaningful and what lets `state.json` store a brief digest that means something.

### 5.4 The echo-chamber guard (binding)

`brief.md` quotes prior sessions' outcomes. The orchestrator's own transcript is then indexed by `memory.Indexer` within ten minutes, and the next brief would quote *that* — recall compounding across generations, which `memory.RecallMarker` was built to prevent for the seed path (ARCHITECTURE.md §9). Revision 1 aimed this guard at the wrong path; the correction is load-bearing enough to state in full.

**The brief's own text cannot re-index, and never could.** `memory.extractUserText` (`extract.go:322-336`) returns `ok=false` for *any* user record whose content list contains a `tool_result` block — the whole record is discarded before `stripRecallMarker`, before `hasExcludedPrefix`, and before a doc is built. The brief arrives as a `Read` tool result, so it is already dropped by the pre-existing filter. **Rejected: a `LOOM-BRIEF` sentinel entry in `excludedPrefixes`.** It would guard nothing, and it cannot be "one entry alongside the existing mechanism" anyway: `hasExcludedPrefix` is `strings.HasPrefix` and backs the **exported** `AskUsable` that `internal/workflow`'s `substitute()` depends on for `{{prev.ask}}` (`extract.go:150`), so a contains-within-512-bytes variant silently widens an unrelated seam. The `LOOM-BRIEF` first line stays in `brief.md` **as a human/debug label only** — it is how a person reading a stray file knows what it is. It has no code path.

**The path that actually compounds is the orchestrator's own `Outcome`.** `feedAssistant` sets `Extraction.Outcome` to the last assistant text (`extract.go:369`) — assistant text, so no user-side filter touches it. The orchestrator's cwd is the project root, which is in §5.2's dir set, so the *next* brief's `Recent work` would quote generation N's summary of a project generation N learned about from generation N−1's brief. Nothing about that text carries a marker.

**Guard (binding): `orch`-tagged sessions are excluded from `Recent work`** — §5.2 step 3. The guarantee stated plainly: *an orchestrator's own transcript is never fed back to its successor; the notes are.* The notes (§3) are the deliberate channel between generations, and they are agent-authored on purpose; the transcript is a byproduct (§2) and stays one.

**Disclosed, not guarded:** the brief-pointer seed (§6) becomes that session's `Ask` — no excluded prefix matches it — so every spawn adds one near-identical `Read …/brief.md first` user doc to FTS. It cannot reach a brief (the tag exclusion drops the whole row before `Ask` is consulted), but it can surface as a low-value hit in the launcher's RELATED panel. Widening `excludedPrefixes` to suppress it would widen `AskUsable` for the same reason above, which is a worse trade than a boilerplate row. Accepted (§14).

## 6. Seed delivery — the brief is a pointer, not a payload

Workflows measured the tmux `send-keys` argv ceiling at ≈16.3 KB and set a **15 KB hard seed cap** with an 8 KB per-value cap; a newline in a seed is an Enter and submits it early. A multi-repo brief does not fit and must not try.

**Binding: the seed is a single line under 2 KB that names the brief file.**

```
Read <~/.loom/projects/<key>/brief.md> first — it is your assembled context for this project.
Follow its "Authorization scope" section exactly. Then: <intent, ≤600 chars, CleanText'd>
```

(rendered as one physical line; the break above is presentational). With no intent, the tail is the standing instruction from §4.6.

This choice does three things at once: it stays an order of magnitude under the 15 KB cap so the existing cap logic is never near its edge; it makes the newline hazard structurally impossible, because every substituted value goes through `memory.CleanText` exactly as workflow substitution already does; and it reuses `session.seedWhenReady` and the ready/trust gate **unchanged** — including `selectCursorPattern`, which the add-dir spike proved is the real defence against typing a seed into a dialog. No new delivery mechanism is introduced.

**Rejected: inline the brief into the seed.** It cannot fit, and paging it in over multiple `send-keys` calls would reintroduce exactly the partial-submit hazard the cap exists for.

**Rejected: reuse `workflow_runs.pending_seed` and the CAS runner.** An orchestrator has no steps, no relations, and no advance; modelling it as a one-step workflow would put a run row, a `step_idx`, and an advance CAS in front of a thing that never advances, and slice 3's real runs would then have to distinguish "run that is an orchestrator" from "run that is a run". The *durable-seed* idea is still borrowed (§7), just not the runner.

## 7. Spawn (binding order)

1. **Guard:** project exists, `root != ''`, root usable (slice 1's `dirUsable`; a `missing` project cannot spawn).
2. **Materialize `notes_dir`** if empty (§3), creating the directory under `~/.loom` only. If `notes_dir` points into the workspace and does not exist, spawn **fails with a named error** rather than creating it — Loom does not create directories in the user's workspace, full stop.
3. **Claim, then launch.** `INSERT INTO orchestrators(project_root, …) VALUES(…) ON CONFLICT(project_root) DO NOTHING`. Zero rows affected ⇒ `ErrOrchestratorExists`, naming the existing session; **no launch**. This is the same claim-before-side-effect discipline as `AdvanceRunCAS`, and it is the only thing that makes §2's singleton true across two instances.
4. **Assemble** the brief, write `brief.md` and `state.json` (atomic temp+rename, the settings idiom).
5. **Launch** via `session.Launcher.Launch` with `Recipe{Cwd: root, AddDirs: <out-of-root repos> ∪ {notes_dir if outside root}, Model: "opus", Mode: "default", Seed: <§6>, ProjectLabel: <project name>}`, tags `orch`.
6. **Update** the claim row with the minted session name and claude id.

`Launcher.Launch` mints the session id internally, so steps 3 and 6 straddle it — the same shape, and the same disclosed race, as workflows' fresh/fork path. **Disclosed failure mode:** a launch that fails after a successful claim leaves a claim row with no session name. Swept: `orchestrator.Sweep` (called from the same place `projects.Sweep` already runs) deletes any claim older than 60 s with an empty `session_name`. A claim row is cheap and recoverable; a double orchestrator is not.

**Launch shape decisions:**

- **cwd is the project root**, per the task and slice 1 §5's root launch. The add-dir spike is binding here: trust prompts **once, for the primary cwd only**, and the siblings are granted silently. So a five-repo orchestrator costs the user exactly one dialog. Add-dirs and cwd are resolved to physical paths by `session.physicalDir` before storage, or the transcript is unreadable (spike §2).
- **Model `opus`.** An orchestrator holding a multi-repo architecture is the one place in Loom where the cheap model is the wrong call. It is also, by construction, a small number of long sessions rather than many short ones.
- **Mode is `"default"`, passed explicitly — never inherited (binding).** Revision 1 said `Mode: ""` and called it "default mode". It is not: `Recipe.Argv` appends `--permission-mode` only when `Mode != ""` (`internal/session/recipe.go:77`), so `""` means *the account default*, and this account's default is **auto mode** — under which the add-dir spike recorded a `Write` to an `--add-dir`'d sibling succeeding with no prompt at all (`⎿ Allowed by auto mode classifier`, spike §3 and its method note). An orchestrator launched the revision-1 way would have had silent write access to every member repo — precisely what rejecting `acceptEdits` was supposed to prevent, with §4.1's prose as the only thing in the way. That is instruction-level ownership, which slice 1 §11 forbids, reintroduced by omission.
  Therefore: `Recipe.Mode`'s documented value set (`recipe.go:19`) gains `"default"`, and spawn sets it. `acceptEdits` is still rejected (silent cross-repo writes); `plan` is still rejected (cannot write the notes, and exiting plan mode is a keystroke Loom must not make on the user's behalf).
  **Honest statement of enforcement:** with `--permission-mode default` in argv, a write outside the accepted set raises a prompt the human answers — a mechanism, not a sentence. The *narrowing* to three files inside `notes_dir` is still **instruction-level**, and slice 1 §11 is explicit that instruction-level restriction is not enforcement. That residue is acceptable here and only here, because the orchestrator has no children and every write is adjudicated by a present human. Slice 3's children get worktree isolation; the orchestrator gets a permission mode plus prose, and this spec claims exactly that much.
  **Deferred, not rejected:** `--settings` deny rules for `Write`/`Edit` outside `notes_dir` would make the narrowing a permission rule too. `--settings` is already occupied by the theme injection (`recipe.go:69`), so this means merging two settings objects at launch — real work, and unnecessary while a human answers every prompt. Named here so slice 3 can pick it up rather than rediscover it.
- **The orchestrator session is an ordinary rail citizen.** It is attributed to the project root by slice 1's resolver, so it hides with its project, appears in Finished when it ends, and is resumable via `r`. No special-casing.
- **`maybeAutoSummarize` skips orchestrator sessions** (tag test). Its transcript is mostly quoted brief content; summarizing it spends quota to distil Loom's own output. This lands in slice 1 §6.2b's "new Loom-initiated background work" category, which exists for exactly this.

## 8. Disposability, `state.json`, and drift

**How a fresh orchestrator picks up.** It does not resume; it re-reads. Every spawn re-assembles from scratch. `claude --resume` on a dead orchestrator remains available (it is the existing dashboard gesture and Loom does not remove it), but it is **not** the spawn path and gets no brief refresh — a resumed orchestrator carries a stale window *and* a stale picture of repo state. Re-assembly is free (§11), so the default is the honest one.

`~/.loom/projects/<key>/state.json`:

```json
{ "schema": 1,
  "project_root": "/Users/h/Sauce/Innostream",
  "notes_dir": "/Users/h/.loom/projects/Innostream-9f2c1ab4/notes",
  "assembled_at": 1753200000,
  "spawn_count": 7,
  "last_session": "loom-<uuid>",
  "brief_bytes": 41233, "brief_sha256": "…", "truncated_sections": ["Recent work"],
  "repos": { "/…/bankenstein": {"branch":"main","head":"9b69827…"}, … },
  "notes": { "loom-map.md": {"sha256":"…","bytes":8112}, … } }
```

Stamped **at assembly only**. Drift is therefore always the question a fresh orchestrator actually needs answered: *what changed since an orchestrator was last briefed on this project?*

At assembly, `## Drift` reports:

- **`repos moved` (actionable):** `bankenstein: 14 commits since the last brief (9b69827 → a41f0c2)` via `git rev-list --count <old>..HEAD`. If `<old>` is unknown to the repo (rebase, force-push, shallow clone, fresh clone) the line reads `history rewritten or commit unknown` — never a fabricated number, never fatal.
- **`notes edited` (informational, expected):** a notes digest differs from the recorded one. This fires whenever the *previous orchestrator did its job*, so it is labelled as expected rather than as a problem. It also catches a human editing the notes between spawns, which is a supported and encouraged thing to do.
- **`membership changed`:** repos added to or removed from the project since the last brief (from the recorded repo key set).
- **`notes missing`:** a file recorded in `state.json` is gone. Reported, never recreated.

**Loom never resolves drift.** It states it; the brief's `What to do` instructs the orchestrator to reconcile before doing anything else. Auto-editing the notes would be Loom writing content it does not understand into files it does not author — and, in the in-repo configuration, into the user's workspace.

**First-ever spawn is not a special mode.** No `state.json` ⇒ no drift section; no notes ⇒ `## Notes` reads `none yet` and `What to do` leads with "write `loom-map.md` first". Bootstrap and steady state are one code path, which is the only way the bootstrap path stays tested.

**If `state.json` is corrupt or unparseable**, it is treated as absent and rewritten. It holds nothing that is not rederivable; the notes are the brain, and this file is a cache with a schema number.

## 9. Store — migrations v10 and v11

**Slots v1–v9 are taken.** `internal/store/store.go`'s `migrations` slice already has nine entries; the last is slice 1's `ALTER TABLE projects ADD COLUMN collapsed INTEGER NOT NULL DEFAULT 0` (v9, `store.go:182`). Revision 1 allocated v9/v10 and would have written the orchestrators table into slot index 8, silently redefining an already-applied version: `migrate()` loops `for i := v; i < len(migrations)`, so every existing DB sitting at `user_version = 9` would skip the new table entirely and open cleanly *without* it. Every store test would pass on a fresh DB and fail on a real one. Renumbered to v10/v11.

```sql
-- v10
CREATE TABLE IF NOT EXISTS orchestrators (
  project_root      TEXT PRIMARY KEY,     -- absolute, canonical; enforces §2's singleton
  session_name      TEXT NOT NULL DEFAULT '',   -- '' = claim in flight (§7.3)
  claude_session_id TEXT NOT NULL DEFAULT '',
  spawned_at        INTEGER NOT NULL,
  ended_at          INTEGER NOT NULL DEFAULT -1
);

-- v11 (standalone: ALTER is not re-entrant — the v2/v3/v8/v9 precedent)
ALTER TABLE projects ADD COLUMN notes_dir TEXT NOT NULL DEFAULT '';
```

Two migrations, not one, for the reason slice 1 §5 already paid for: `PRAGMA user_version` replay from a stale version must not re-run a non-idempotent `ALTER`. Slice 1's own v9 `collapsed` ALTER is the nearest precedent and sits in its own slot for exactly this reason.

**The table is a pointer, not a record.** It holds nothing that cannot be rebuilt by scanning live sessions for the `orch` tag and attributing their cwds — which is exactly what `orchestrator.Sweep` does when it finds a live `orch`-tagged session with no row (adopt-before-reap, the engine's own idiom). Losing `orchestrators` costs a singleton guarantee for one poll interval, never a session and never a note.

`ended_at` is set by `orchestrator.Sweep` when the session row goes terminal, not by the status engine — the engine stays project-unaware. A terminated orchestrator's row is **kept**, so the overview can say "last orchestrator ran Tuesday" and so `spawn_count` is meaningful; a new spawn overwrites it (`ON CONFLICT DO UPDATE` when `ended_at != -1`, `DO NOTHING` when live — the claim in §7.3 is exactly this predicate).

## 10. GUI seam (slice 4 must stay unblocked)

**How the GUI knows.** `ProjectDetailDTO` gains one nullable sub-object:

```go
Orchestrator *OrchestratorDTO `json:"orchestrator"`   // nil = none has ever run
```

with `{SessionName, ClaudeSessionID, Live, SpawnedAt, EndedAt, NotesDir, NotesInWorkspace, BriefPath, BriefBytes, TruncatedSections, Drift []string}`. Populated by one extra `ListOrchestrators()` query joined in memory inside the existing `ListProjectDetails` call — **no per-project IPC, no N+1**. The rail's per-project marker is derived from this field only.

Three new `App` methods: `SpawnOrchestrator(root, intent string) (sessionName string, err error)`, `SetProjectNotesDir(root, dir string) error`, `ReassembleBrief(root string) error` (refresh drift without spawning). Errors surface through the overview's existing `#po-error` node.

**The overview renders a new `#po-orch` block, positioned above `#po-arch`.** In this slice it contains: state line (`none` / `live since …` / `ended <when>`), a Spawn or Attach button, the notes location with a *Move notes…* action and an in-workspace badge, `brief.md`'s size plus any truncated sections, and the drift list as plain lines.

**Binding: slice 2 does not write into `#po-arch`.** It stays the empty, unread seam slice 1 left for slice 4's architecture view. Nothing in slice 2 renders note *content*; the notes are opened in the user's editor through the existing `OpenInEditor` path. Rich rendering is slice 4's whole job and duplicating a shabby version of it here would be work slice 4 has to delete.

**Hidden projects:** the overview is deliberately unfiltered (slice 1 §8, so a hidden project can be unhidden from its own screen), so the orchestrator block renders there. The orchestrator *session* is attributed to the project root and therefore hides on the rail like any other session. No special case, and none is wanted — a spawn is user-initiated, so §6.2b's suppression of Loom-initiated background work does not bar it.

**TUI reach:** none in this slice. The TUI sees the orchestrator as an ordinary `orch`-tagged session — it attaches, resumes, and hides correctly with zero TUI changes, which is the property the "ordinary session" decision in §2 was chosen for.

## 11. Cost budget — and what is deliberately not loaded

**Assembly costs no quota at all.** Local file reads, a few `git rev-parse` invocations, and two SQLite queries. Nothing in this slice calls out to an LLM. `memory.Summarizer` — Loom's only outbound call — is not on any path here.

**Spawn cost:** one `Read` of ≤64 KB ⇒ ≈16 k tokens worst case, ≈8 % of a 200 k window, once. Typical is far smaller: the 24 KB notes cap plus 16 KB of recent work only binds on a mature project, and a fresh one assembles at 3–6 KB.

**Steady-state cost is whatever the human asks the orchestrator to do**, which Loom neither budgets nor bounds. Loom adds zero background turns: no auto-summarize (§7), no polling prompts, no nudges (rendezvous is slice 3).

**Not loaded, ever, at spawn — each with its reason:**

| Not loaded | Why |
|---|---|
| Any source file from any repo | The orchestrator has read access via `--add-dir` and can fetch what it needs. Preloading a guess is how a 200 k window becomes 40 k of usable room. |
| `git log` beyond one HEAD sha per repo | A commit-count is the whole signal; the log is a `git log` away. |
| `transcripts.llm_summary` | Multi-KB each; forty exceed the total cap alone (§5.2). |
| Transcript bodies, FTS snippets, message docs | The one-line outcome is the distillation; that is what the index is *for*. |
| Any other project's memory | Attribution filter, plus `Resolver.Visible`. |
| Hidden/solo-suppressed rows | Slice 1 §6, fail-closed. |
| `~/.claude` config, trust flags, settings | Not the orchestrator's business, and slice 1 kept `trust` read-only-defensive. |
| A dependency graph, call graph, or module map | Slice 4. |
| Anything about child sessions | Slice 3. Not even a placeholder section — an empty "Children" heading is an invitation to hallucinate one. |

## 12. Reuse ledger

**Reused unchanged:** `session.Launcher.Launch` / `Recipe` / `AddDirs` / `physicalDir` / `validateDirs`; the ready-and-trust gate and `selectCursorPattern`; `seedWhenReady` and `seed_status`; `store.SessionRow` and its `tags` column; `projects.Resolver` (attribution, `Visible`, `Canonical`); `projects.Service` target set and `Sweep` cadence; `memory.CleanText`, `memory.AskUsable`, `memory.ExtractFile`; `store.tx` and `applyMigration`; the settings atomic temp+rename write; `cmd/loom-gui/diff.go`'s bounded git shell-out; `OpenInEditor`; `OpenDirectoryDialog`; the workflow truncation marker idiom.

**Reused with a small, backward-compatible extension:** `memory.Related` → `RelatedIn(dirs…)`, single-dir becomes a wrapper (§5.2); `store.RecentTranscriptsByProjectDir` → `…Dirs`, plus an `orch`-tag exclusion join (§5.4); `Recipe.Mode`'s documented value set → `"default"` (§7, no behaviour change for existing callers, which pass the values they already passed); `ProjectDetailDTO` → one nullable field (§10).

**Explicitly NOT extended:** `memory`'s `excludedPrefixes` / `hasExcludedPrefix`. Revision 1 proposed a sentinel entry there; §5.4 shows it would guard nothing and would widen the exported `AskUsable` that `internal/workflow` depends on.

**Added:** `internal/orchestrator` (assemble/state/spawn/sweep); migrations v10 + v11; three `App` methods; one overview block.

**Deliberately not reused, with reasons:** `workflow.Runner` and `workflow_runs` (§6 — no steps, no advance, and slice 3's real runs must not have to distinguish themselves from a fake one-step run); `memory.Summarizer` (§11 — assembly spends no quota); `status.Engine` hooks (slice 1 §6.2a — the engine never learns about projects, so `ended_at` is stamped by the orchestrator sweep instead).

## 13. Testing (binding)

- **Assembly determinism:** golden brief from a fixture project (2 repos, fixture notes, fixture transcripts, fixed clock) — byte-identical across runs; a second run with one changed input changes exactly the expected section.
- **Budget:** notes at 23 KB inline verbatim; at 25 KB each is head-truncated with the marker and the abs path; `Recent work` capped at both 40 rows and 16 KB; total never exceeds 64 KB; **`Authorization scope` is present, intact, and appears twice in every truncation scenario including a pathological 500 KB notes file.**
- **Seed:** assembled seed is single-line, <2 KB, contains no `\n`/`\r` after `CleanText` even when the user's intent is a multi-line paste; a 5 KB intent is capped at 600 chars; empty intent produces the standing tail.
- **Recent work:** hidden project's rows never appear; solo suppresses non-solo rows; unattributable row dropped (fail-closed); `Outcome`-empty falls back to `Ask` only when `AskUsable`, else `(no outcome recorded)`; dedupe across two repo dirs that indexed the same session id; intent present ⇒ ranked path, intent absent ⇒ recency path.
- **`RelatedIn`:** `SameProject` true for a hit in any dir of the set; existing single-dir `Related` tests pass unmodified (this is the wrapper's whole point).
- **Echo-chamber guard:** a completed `orch`-tagged session, fully indexed with a non-empty `Outcome` and a cwd equal to the project root, **does not appear in that project's next brief** — this is the test that pins the real path, and it must fail if the tag exclusion is removed. Plus: a non-`orch` session in the same dir *does* appear (the exclusion is not a blanket drop); a doc whose user text merely mentions `LOOM-BRIEF` is still indexed (there is no sentinel filter, by decision — §5.4); `AskUsable` behaviour is unchanged from its existing tests.
- **Singleton:** two concurrent `SpawnOrchestrator` calls on one project ⇒ exactly one launch, the loser gets `ErrOrchestratorExists` naming the winner; a claim with an empty `session_name` older than 60 s is swept; a claim younger than 60 s is not; a live `orch`-tagged session with no row is adopted.
- **notes_dir:** empty ⇒ materialized under `~/.loom` and **written back to the row**; `RepointProject` afterwards leaves `notes_dir` unchanged (the derived-default regression); a `notes_dir` inside the workspace that does not exist ⇒ spawn fails with a named error and creates nothing; *Move notes* copies all three files and leaves the source in place.
- **Drift:** repo HEAD moved ⇒ commit count; unknown old sha ⇒ "history rewritten or commit unknown", not a number and not an error; notes digest changed ⇒ labelled expected; repo added and repo removed both listed; missing notes file reported and not recreated; corrupt `state.json` ⇒ treated as absent, rewritten, spawn succeeds.
- **Store:** v10+v11 replay from a stale `user_version` on a real DB copy (this is what catches a non-idempotent ALTER); **`PRAGMA user_version` equals 11 after migration** — an assertion on the absolute number, so the next slice cannot re-collide with an occupied slot the way revision 1 did; the claim's conflict predicate (`DO NOTHING` while live, `DO UPDATE` once ended).
- **Launch shape:** `Cwd` = root, add-dirs = out-of-root repos ∪ out-of-root `notes_dir`, deduped, physical-resolved; **argv contains `--permission-mode default`, asserted on the exact argv, and a store/account default of `auto` does not change it**; no orchestrator for Ungrouped (backend rejects, button disabled); a `missing` project cannot spawn.
- **Layering:** an orchestrator session polls, transitions, hides, and resumes with zero orchestrator-specific code in `status` or `ui`; `maybeAutoSummarize` skips it without poisoning `sumTried`.
- **GUI:** `ListProjectDetails` issues one orchestrator query for N projects (no N+1); `orchestrator` is `null` for a project that never ran one; **`#po-arch` is present and empty after rendering a project with a live orchestrator** — the slice-4 seam, pinned by a test so it cannot be quietly colonized.

## 14. Accepted limits and disclosed failure modes

- **The notes are unverified prose.** Nothing checks `loom-map.md` against the repos. An orchestrator that hallucinates a seam produces a confidently wrong brief for its successor, and the successor has no way to tell. The only mitigations shipped are drift reporting (§8) and the fact that a human can read three short files. This is the single largest weakness in the slice and slice 3 must not build a delegation contract on top of it (§15).
- **In-workspace notes appear in `git status` and in slice 1's sectioned `SessionDiff`.** That is the price of the in-repo option and the reason it is not the default.
- **The narrowing to three files is instruction-level; the write gate itself is not** (§7). `--permission-mode default` is passed explicitly, so writes prompt; *which* writes are legitimate is prose. Acceptable only because there are no children yet and the human answers every prompt. A permission mode that is inherited rather than passed is not a guarantee — that is how revision 1 got this wrong.
- **The brief-pointer seed indexes as an `Ask`** (§5.4). It cannot reach a brief, but it accumulates one near-identical boilerplate doc per spawn in FTS, reachable by the launcher's RELATED panel. Suppressing it would widen the exported `AskUsable`; the boilerplate is the cheaper cost.
- **Claim-then-launch can strand a claim row** for up to 60 s after a failed launch (§7).
- **One orchestrator per project, not per initiative.** Two concurrent initiatives inside one project share one orchestrator and one set of notes. Splitting notes per initiative is slice 3's problem, deliberately not pre-solved here.
- **No cross-project orchestrator.** Exclusive membership (slice 1 §2) means a project is the widest scope that exists.
- **`state.json` drift is coarse:** a commit count, not a diff. A 200-commit refactor and a 200-commit dependency bump read identically.
- **No TUI surface** for spawn, notes location, or drift.
- **The brief is a snapshot.** Repos move while the orchestrator runs; nothing re-briefs it mid-session. `ReassembleBrief` + a manual "re-read the brief" is the whole story. Live re-briefing is a rendezvous concern (slice 3).

## 15. Slice 1 §11 constraints — how this slice answers them

| Constraint | Answer |
|---|---|
| Children get worktree/container isolation, not instruction slices | **No children in this slice.** The orchestrator's own write gate is a passed permission mode (`--permission-mode default`, §7); only the narrowing to three files is instruction-level, and §7 says so plainly rather than dressing it up. Revision 1 inherited the account default and thereby had *no* gate at all under auto mode — the one place this slice had quietly reverted to instruction-level ownership. Slice 3 inherits the constraint intact. |
| A test-gated integration step is mandatory | Not applicable yet — nothing is integrated. Recorded as still owed by slice 3, and §2 forbids treating notes as a substitute. |
| "Done" is an executable check on a published artifact | **Not satisfiable here and not claimed.** The orchestrator's artifacts are prose. §14 states this as the slice's largest weakness precisely so slice 3 cannot inherit it as a solved problem. |
| Dependency-gated scheduling primary, park-and-resume fallback | Slice 3. Nothing here schedules anything. |
| Explicit authorization scope in every brief | §4.1: first section, repeated last, never truncated, tested under pathological truncation (§13). |
| No orchestrator-reviews-children-in-prose | **Structurally impossible in this slice** — there are no children — and nothing here builds the reflection machinery that would tempt it. `loom-decisions.md` is a ledger of decisions, not a review of work. |
| Multi-agent benefit is conditioned on low inter-task cohesion; validate cheaply first | **This slice is that cheap validation.** An orchestrator with real notes, real drift reporting, and zero delegation can be run against one genuine multi-repo initiative for a week, at the cost of one session, and will answer empirically whether a per-project brain holds a multi-repo architecture usefully at all. If it does not, slice 3 should not be built, and the sunk cost is one package and two migrations. |

**Out of scope, restated:** delegation, child sessions, worktree provisioning, rendezvous, park-and-resume, dependency graphs, and all rich rendering.
