# Loom — Projects Foundation: grouping, hiding, multi-repo launch — Design

**Status:** Revision 2 (hardened after six-lens adversarial review: 75 findings raised, 67 survived double refutation — 3 Critical / 11 Important / 10 Minor, all folded; plus arc constraints from an evidence review of multi-agent coding orchestration)
**Date:** 2026-07-22
**Scope:** Slice 1 of 4 in the orchestration arc. Models *project* as an axis distinct from *repo*, fixes the discovery bug that hides nested repos, adds screen-share hiding, and makes cross-repo sessions a first-class launch target. NO orchestrator, NO delegation, NO architecture rendering — slices 2–4, whose binding constraints are recorded in §11.

## 1. What this delivers

A **project** is a named initiative owning one or more repos ("Happy Pay", "Innostream"). Orthogonal to a repo, persistent, groups live *and* historical work, creatable in-app against any directory, hideable for demos, and launchable into across several of its repos at once.

1. 16 currently-unreachable repos become visible and launchable (§3).
2. Rail, search and history group by initiative rather than by directory accident.
3. One gesture removes a client's footprint from Loom's own surfaces before a screen-share (§6 — this is screen-share hygiene, not confidentiality; see §6.5).
4. A single session works across `bankenstein` + `ballista` without dragging in the other repos.

## 2. Model (binding)

- **Repo** — a git working tree (`.git` present). Today's `registry.Project`, renamed `registry.Repo`.
- **Project** — a named set of repos plus a root directory. Both roots and repos are launch targets (§5).
- **Membership is exclusive**: one repo belongs to exactly one project. Enforced by `project_repos.path` PRIMARY KEY. *This constraint buys unambiguous attribution and a single owner per path — it does NOT by itself make hiding airtight; §4's resolver is what does that.* (Revision 1 asserted the PK as the hiding guarantee. That was a non-sequitur: the specified filter never read `project_repos`.)
- Every repo belongs to some project; a bare top-level repo is a **single-repo project of one**, so the rail has one kind of citizen.
- **Ungrouped** is a reserved project row (§7), not a computed bucket — it must live in the same space the §6 predicate keys on, or every surface grows a second branch and a surface that forgets one still passes its test.

**Exclusivity and the arc.** An initiative like "the Atlas rearchitecture across bankenstein/ballista/v-atlas" is **not** a project — it is an orchestration *run* (slice 3) scoped to a subset of one project's repos. Modelling initiatives as projects would force them to steal repos from Innostream. Recorded here because slice 3 must not assume otherwise.

**Label rule (binding).** `registry.Repo.Label` is `project/repo` for repos of a multi-repo project (matching today's `registry.go:48`) and the bare basename for single-repo projects. The project segment derives from the **root directory's basename**, never from `projects.name` — `name` is user-editable and a rename must not invalidate saved workflows. Labels are unique by construction (`UNIQUE` on `project_repos.label`; uniqueness rule on created project names). A collision must not break discovery (§3 is never fatal): the reconciler **skips** the conflicting insert and records it as a non-fatal warning surfaced on the project overview, and `workflow.loadOne` reports ambiguity through its existing per-file error channel. The set handed to `workflow.LoadAll` is **repos ∪ project roots**, so a definition naming `Innostream` still resolves. On-disk workflow JSON is unchanged.

## 3. Discovery (ordered decision list)

`registry.Discover` currently tests `isProject(path)` — `.git` **OR** an existing claude transcript dir — *before* descending, and `continue`s. Verified live: `~/Sauce/Innostream` and `~/Sauce/HappyPay` have no `.git` but do have transcript dirs, so both self-promote to leaf projects and their children are never scanned.

**Verified currently-invisible set (16):** Innostream → `ballista`, `bankenstein`, `bb-integr8`, `flux-fleet`, `quickbit`, `terraform-infra-eks`. HappyPay → `HappyCardEngine`, `HappyPay`, `HappyPayCLM`, `HappyPayCoreApi`, `HappyPayMembers`, `HappyPayMerchants`, `HappyPaySavaToolset`, `_v3-members`, `_v3-merchants`, `_v3-monolith`.

The fix separates the two predicates `isProject` conflates: **`isRepo` (has `.git`) governs descent; a transcript dir governs launchability.** Applied per workspace subdir as an ordered, first-match list:

1. **is a repo** → single-repo project; **do not descend** (preserves today's behaviour for submodules/vendored trees).
2. **else any immediate child is a repo** → project with those children — *regardless of whether the parent has a claude transcript dir*. This arm is what fixes the live bug.
3. **else has a transcript dir** → zero-repo project rooted at itself, launchable via §5 root launch.
4. **else** → not a project.

Depth stays one level. Unreadable dirs are skipped, never fatal. **Symlinked entries are skipped** — `os.ReadDir`'s `DirEntry.Type()` comes from the dirent, so `IsDir()` is false and the entry never reaches `isRepo` (this is why `Innostream/leikur`, a symlink to `bankenstein`, is not in the restored set; it is also a duplicate of a repo already restored). Stated rather than changed: following symlinks risks the same tree appearing under two paths, which §2 exclusivity forbids.

## 4. Attribution and path identity

**One resolver, one authority.** A single package under `internal/` (below both frontends, explicitly *not* in `status` or `store`, preserving §6's layering) owns attribution and visibility. Every surface calls it; nothing re-derives.

**Target set** = `{projects.root} ∪ {project_repos.path}`. A session attributes to the **longest match** over that union; a repo match resolves to its `project_root`. If a path appears as both, the project root wins — and adding an existing project's root as another project's repo is rejected at write time.

**Matching is segment-wise**, never a raw string prefix:

```go
cwd == R || strings.HasPrefix(cwd, R + string(filepath.Separator))
```

Without this, `…/HappyPay/HappyPay` is a prefix of the five real siblings `HappyPayCLM`, `HappyPayCoreApi`, `HappyPayMembers`, `HappyPayMerchants`, `HappyPaySavaToolset`.

**Canonicalization is on writes only** — `filepath.Abs` + `Clean` for discovery output, folder-picker results, and `project_repos` inserts (a trailing slash otherwise breaks both arms of the match *and* mints a second PK row for the same directory). **Stored cwds are never rewritten and never `EvalSymlinks`'d**: `transcript.ProjectDirName(cwd)` is how `status/engine.go`, `workflow/run.go` and `memory/indexer.go` locate transcripts, so rewriting a cwd breaks status polling and the memory index. Symlinks are handled at the *comparison* site — each project root is resolved once per discovery pass and matched against both its raw and resolved forms, which is O(projects), not O(sessions).

**Failure direction is fail-closed:** while any project is hidden or solo is active, a row that cannot be attributed is treated as **hidden**. (Live DB: 141 transcripts, 1 with `cwd=''`; `transcripts.cwd` is `NOT NULL DEFAULT ''`, `sessions.cwd` has no default, so the empty case is transcripts-only.)

Attribution is **computed, never stored** — zero data migration, and every historical transcript groups correctly the moment a project exists. Reassigning a repo re-attributes its history immediately *because* the resolver keys on repo paths, not only roots.

**Shape changes this requires** (neither makes the engine project-aware, so §9's engine-independence test still means what it says):
- `status.Snapshot.NewlyNeedsYou` becomes identity-bearing — `[]string` of **session names** (the store PK and the engine's own identity key), with consumers joining against `snap.Live` for label and cwd. Today it is `[]string` of pre-rendered `"ProjectLabel · Title"`, and `ProjectLabel` is `filepath.Base(cwd)` for adopted orphans, so two same-basename repos in different projects are indistinguishable — routine under the multi-repo model. Updates `notify.go`, `internal/ui/app.go`, and two tests.
- `SessionDTO`/`FinishedDTO` gain server-computed `projectRoot` and `projectName`. They currently carry `Project = ProjectLabel` and no cwd, so leak surfaces 1–2 and §8's sectioned rail have nothing to group by. `ProjectLabel` remains display-only and is never an attribution input (it is still a passthrough for `workflow/run.go` and `launch.go`).

## 5. Multi-repo launch

`session.Recipe` grows `AddDirs []string`; `Recipe.Argv` appends `--add-dir <path>` per entry. Two shapes:

- **Root launch** — `Cwd` = project root, no `AddDirs`.
- **Scoped multi-repo** — `Cwd` = primary repo, `AddDirs` = the other selected repos.

**The target list is a set keyed by absolute path**: the root row is suppressed when any member repo's path equals the root (use the path rule, not `len(repos)==1` — §3 rule 3 and out-of-root membership both produce roots that are also repos with siblings). The surviving target keeps its `{kind, path, project_root}` association so slice 2 can still launch a single-repo project *as a project*. **Primary = the topmost checked repo in list order**, matching the documented house decision in `internal/ui/fanout.go` that selection is checklist-order and independent of toggle order.

Fan-out (`N`) remains a separate modal launching one session per checked repo; its checklist enumerates repos, not projects.

**`AddDirs` must be persisted.** `ResumeShellCommand` is hardcoded to `claude --resume <id> --settings <theme>`, `Resume` forwards only `old.Cwd`, and `SessionRow` has no field — so a resumed multi-repo session silently comes back seeing one repo, invisibly, until a sibling write fails mid-turn. Reachable from the Finished list, search-resume, and the TUI. This is also slice 3's park/resume path.

**Migration hazard:** `ALTER TABLE sessions ADD COLUMN` is **not** re-entrant, and §9 requires replaying migrations from a stale `user_version`. The ALTER therefore lands as its **own migration v8** (matching the standalone-ALTER precedent of v2/v3), never inside v7. Thread through `Launch`'s upsert, `Resume`'s new row (or a second resume drops them again), and a dirs-taking `ResumeShellCommand`. At resume, filter to still-existing dirs so a moved repo degrades visibly.

**Prerequisite spike — `docs/spikes/2026-07-22-add-dir-spike.md`** (blocks §5 only). It must **construct** a genuinely untrusted sibling (fresh directory outside every trusted ancestor, scratch `CLAUDE_CONFIG_DIR`) — the previous trust spike came back inconclusive precisely because trust was inherited — and record the exact pane string. Questions: does `--add-dir` prompt, silently restrict writes, or just work? Does `claude --resume` restore add-dirs? Does the transcript still key on `Cwd` alone (§4 assumes so; the blast radius is the status reader path and resume correlation, not just grouping)? The spec must carry a branch table for each outcome — "findings binding" names an authority, not a result.

**This is a seed-corruption hazard, not only a UX question.** `launch.go` documents that `ReadyMarker` (`❯`) is unsafe alone because the trust dialog's `❯ 1. Yes, proceed` cursor contains it. A `--add-dir` dialog with *different wording* will not match `TrustMarker` but will match `❯`, and `seedWhenReady` will type the seed into it. Harden generically rather than by allowlist: treat the pane as dialog-pending on any line matching a numbered select cursor (`^\s*❯\s*\d+[.)]`), and accept ready only on a `❯` that is not that shape. This subsumes the existing special case and covers model/theme/login dialogs.

## 6. Hiding & solo

### 6.1 The predicate

**One flag, in `loom.db`.** `solo` is an `INTEGER` column on `projects` with a partial unique index (`WHERE solo=1`) — not a settings-file field, which would be `cmd/loom-gui`-local and unreadable by the TUI, making §6's "same mechanism" claim false at the storage layer.

```
if any project has solo=1 → visible ⟺ solo
else                      → visible ⟺ !hidden
```

`hidden` is untouched by solo, so exiting solo restores prior state exactly. A solo root that is `missing` degrades to **nothing hidden**, never everything hidden.

**Visibility is evaluated over the session's whole directory set — `cwd ∪ add_dirs`** — so a session whose cwd sits in a visible project while it edits a hidden project's repo is hidden. Ungrouped is suppressed by solo.

### 6.2 Layering invariant, split in two

Revision 1 said "presentation filter only". That is right for rendering and wrong for spending. The invariant splits:

- **(a) Hiding never alters in-flight behaviour.** Hidden sessions keep running, polling, and transitioning status. `status.Engine` never learns about projects.
- **(b) Hiding suppresses *new Loom-initiated background work*.** Today's sole member: `maybeAutoSummarize`, which currently runs on raw store rows before DTO mapping and would spend quota summarizing a project the user just put out of view. Skip **without** setting `sumTried[id]`, or unhiding never re-enables it. This category is what slices 2–4 (brief assembly, orchestrator spawn, rendezvous nudges) inherit — it exists now so they have somewhere to land.

### 6.3 Leak surfaces, per frontend (binding; §9 is one test per item, per frontend)

**GUI:** rail · Finished list · search results · workflow runs · workflow *definitions* list · `ListProjects` (launcher target picker) · fan-out checklist · needs-you count · window title · dock badge · notifications.

**TUI:** rail · Finished · wall · recall/RELATED panel · search · notifications.

The `wall` is TUI-only; window title and dock badge are GUI-only; notifications exist in **both**, and `ARCHITECTURE.md` declares two instances against one DB a supported state — so an unfiltered TUI banner naming a hidden client is a real leak of the feature's entire purpose. TUI hiding is therefore **in scope** for this slice; the minimum is filtering `NewlyNeedsYou` before `notifyCmd` plus the `snapMsg` handler, which covers rail, Finished and wall in one place.

**Over-fetch before filtering.** `store.Recent` and `SearchSessions` apply `LIMIT` in SQL, so a presentation-layer filter after the cap silently truncates — badly under solo. Over-fetch and trim after filtering, the pattern `memory/recall.go` already uses. Do **not** push the predicate into SQL: `Recent` feeds `Engine.Poll` (breaking §6.2a), and a `LIKE` join cannot express longest-prefix.

**Workflow runs** attribute by the **union** of every project the run touches (per-step resolved paths from the `def_json` snapshot plus the cwds of `session_names` — all derivable, no schema change). Attributing to step 1 is wrong: a run advancing into a hidden repo would keep a visible row naming that project's live session.

### 6.4 Persistence and the chip

Both flags persist. A permanent titlebar chip is mandatory: a restart mid-demo silently revealing a project is the worse failure. The chip shows the **visible** project under solo (`solo: Innostream`) or a bare count when hiding — never an identity-bearing needs-you count, which would re-leak what hiding concealed. If attention must escalate from a hidden project, degrade the notification *body* to a label-free form. Restore uses the existing armed-confirm idiom from `killButton`, so one stray click mid-share cannot undo it.

Status is level-triggered, so rail, counts and badge self-heal on unhide. `NewlyNeedsYou` is edge-triggered and `SetStatus` persists in the same pass, so **a notification suppressed while hidden is never replayed** — stated, not fixed.

### 6.5 What this does not do

Not "leak-proof", and §1.3 does not claim every surface. Explicitly out of reach: macOS Notification Centre retains already-delivered banners (fire-and-forget `osascript`, no handle to withdraw); `tmux -L loom ls`; `~/.claude/projects` JSONL; `loom.db` in plaintext, readable by any session Loom launches; and the attached terminal on screen. **The threat model is screen-share hygiene, not confidentiality.** No hard-delete "forget project" is offered — discovery re-inserts the row next launch and the indexer re-creates transcript rows within 10 minutes from JSONL Loom does not own, so it would ship a stronger false promise.

## 7. Store — migrations v7 and v8

```sql
-- v7
CREATE TABLE IF NOT EXISTS projects (
  root       TEXT PRIMARY KEY,       -- absolute, Abs+Clean canonical; '' reserved for Ungrouped
  name       TEXT NOT NULL,
  origin     TEXT NOT NULL,          -- 'discovered' | 'created' | 'reserved'
  hidden     INTEGER NOT NULL DEFAULT 0,
  solo       INTEGER NOT NULL DEFAULT 0,
  missing    INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_solo ON projects(solo) WHERE solo = 1;
CREATE TABLE IF NOT EXISTS project_repos (
  path         TEXT PRIMARY KEY,     -- absolute, canonical; enforces §2 exclusivity
  project_root TEXT NOT NULL,
  label        TEXT NOT NULL,
  missing      INTEGER NOT NULL DEFAULT 0,
  added_at     INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_repos_label ON project_repos(label);
CREATE INDEX IF NOT EXISTS idx_project_repos_project ON project_repos(project_root);

-- v8 (standalone: ALTER is not re-entrant, cf. v2/v3)
ALTER TABLE sessions ADD COLUMN add_dirs TEXT NOT NULL DEFAULT '';  -- JSON array
```

`IF NOT EXISTS` per house convention, through the existing single-transaction `applyMigration`. The **Ungrouped** row is seeded at migration with `root=''`, `origin='reserved'`; §4's prefix scan must exclude it (an empty root prefixes everything).

**`loom.db` is the runtime source of truth for launch targets.** `registry.Discover` today runs once at startup into a by-value `App.projects` slice that `buildRecipe`, `Fanout` and `workflow.LoadAll` all validate against — so a project created in-app is listed but not launchable, and hide/rename/reassign never reach it. Replace with a project/repo service in `internal/` (both binaries need it) that owns discovery + DB reconciliation and is queried read-through. Its target set includes project roots, repo paths, and `AddDirs` entries. `Discover` failure becomes non-fatal once the launcher no longer depends on it.

**Upsert-without-clobber:** discovery inserts what is absent and never overwrites a user-set `name`, `hidden`, `solo`, or membership — the discipline `UpsertTranscript` uses for `llm_summary`.

**`origin`'s job** is reconciliation: it tells the sweep whether absence from a workspace scan means "root vanished → flag `missing`" or "never expected here → leave alone". Stored roots are **not** re-run through §3's rules — that would clobber curated membership and let two scan sets fight over one path under a PK. Refreshing a created project's repo set is an explicit user gesture.

**Retirement.** `missing` is swept by stat-ing **every known row** (the way `memory/indexer.go` sweeps `file_missing`) — not by diffing against the scan set, which would wrongly flag every out-of-root member. Missing projects and repos render dimmed and non-launchable, stay in the rail, self-clear, and `hidden` wins over any missing badge (a warning chip naming a hidden client mid-demo is exactly the leak §6 prevents). **"Remove repo from project" is defined as reassignment to a single-repo project at its own path**, not deletion — the row persists so discovery never re-absorbs it, and §4 re-attributes its history for free. A **re-point action** moves a `missing` project to a new root in one transaction (updating `root`, all `project_repos.project_root`, and prefix-rewriting member paths, with a stated rule for a PK conflict at the target), because `root` is the PK and a plain `mv` otherwise mints a new row and strands the old.

Project roots must **exist at create time** (the folder picker cannot return a nonexistent path anyway), which removes Revision 1's "Loom creates the directory" branch and keeps the no-writes-into-your-workspace property absolute.

`sort_order` stays dropped. Ordering is one total order: needs-you projects by name → Ungrouped if it holds a needs-you session → remaining projects by name → Ungrouped. (Revision 1 said both "attention floats" and "Ungrouped sits last", which conflict.)

## 8. UI (GUI-first)

**Rail** — project sections, each internally preserving today's status ordering, ordered per §7. Rationale unchanged: strict project-then-status nesting buries an urgent session inside a collapsed group, and attention-first is Loom's reason for existing. Collapse state persists in `loom.db` alongside the other project flags, not in a third store.

**Project overview** — clicking a header replaces the stage: name, root, repos (with missing badges), live and finished sessions, hide/solo toggles, edit, re-point. Deliberately a shell — slice 2's architecture view renders into it.

**Create project** — `+ New project` → `OpenDirectoryDialog` → name → repo checklist prefilled from children, plus add-repo-outside-root → create.

**TUI reach** — inherits §2's rename, §3's discovery fix, §4's resolver, and §6's hiding for the surfaces it owns. The two-level rail, project overview, folder picker and solo *gesture* are GUI-only; the solo *flag* is honoured by both because it lives in the DB.

**`SessionDiff` becomes sectioned** — `Repos []RepoDiff{Path, Label, Stat, Patch, Untracked, Dirty, Error}` — because `gitDiff` runs against `row.Cwd` only, so a scoped multi-repo session shows an authoritative-looking diff covering just the primary repo. Sectioned rather than concatenated: the frontend splits on `/\n(?=diff --git )/` and injected headers would corrupt the parse. Reviewing a cross-repo change is what slices 3–4 lean on.

## 9. Testing (binding)

- **Discovery**: regression pinned to the verified live shape (parent with transcript dir *and* child repos yields the 16); table tests for each §3 rule in order, including rule-1-beats-rule-2 precedence, transcript-only leaf, transcript-only child, non-git/non-transcript leaf (not discovered), symlinked entry (skipped), nested group at depth 2 (not discovered).
- **Attribution**: sibling-prefix (`HappyPay/HappyPay` vs `HappyPayCoreApi`), trailing-slash root, symlinked root, empty-cwd transcript, adopted-orphan cwd, cwd == root exactly, nested roots, out-of-root repo, path present as both root and repo (rejected at write).
- **Hiding**: one test per §6.3 surface **per frontend**; out-of-root repo hides with its project; a cross-project `AddDirs` session hides; fail-closed on unattributable rows; Finished and search lengths unchanged when a project is hidden (over-fetch); solo ↔ hidden round-trip; solo root missing → nothing hidden; workflow run touching a hidden project.
- **Layering**: hidden sessions still poll and transition (§6.2a); auto-summarize suppressed and re-enabled on unhide without a poisoned `sumTried` (§6.2b).
- **Store**: v7+v8 re-entrancy from a stale `user_version` on a real DB copy (this is what catches a non-idempotent ALTER); upsert-without-clobber after rename/hide; `missing` sweep over every known row; remove-repo-as-reassign not re-absorbed after restart; directory rename → re-point → one row, `hidden` preserved.
- **Recipe**: `AddDirs` → argv order and quoting; empty `AddDirs` leaves existing tests untouched; launch → resume → argv round-trips; resume filters vanished dirs.
- **Launch safety**: cwd and each add-dir validated before `tmux new-session`.
- **Workflow compat**: a definition naming a promoted group dir (`Innostream`) resolves to the same absolute path; the existing `plan-execute-review.json` naming `loom` is unaffected; a `parent/child` label loads.

## 10. Accepted limits

Discovery depth stays one level — **nested groups such as `Innostream/albedo/{back-office-website,voucher-api,voucher-websites}` are not auto-discovered** and are reachable only via `+ New project`. Symlinked workspace entries are skipped. Exclusive membership only. Project metadata is local, not synced. Hiding is screen-share hygiene, not confidentiality (§6.5), and suppressed notifications are not replayed. Fan-out stays one-session-per-repo. The TUI does not get the two-level rail or the solo gesture. Recall's same-project boost still operates on the repo dir, not the project — a noted follow-on.

**Scope.** Five workstreams (rename, discovery, migration + reconciliation, hiding/solo, multi-repo launch + spike) plus GUI work. The rename is ~21 non-test references. If this must be split, split on **persistence, not the model**: 1a = §2 rename + §3 + §4 + grouping + root launch, zero schema; 1b = tables, create-project, hiding/solo, `AddDirs`. Do not defer `AddDirs` or membership-as-data past 1b — slice 3's children need both.

## 11. Out of scope — and binding constraints for slices 2–4

Slice 2: orchestrator session, brief assembly. Slice 3: delegation manifest, spawning, rendezvous. Slice 4: architecture and dependency-graph rendering.

Constraints established by the evidence review, recorded so those slices do not inherit Revision 1's assumptions:

- **Children get worktree (or container) isolation, not instruction-level file slices.** A controlled ablation put shared-tree + declared-ownership *below* the single-agent baseline (55.5% vs 57.2% on PaperBench) while worktree isolation scored 63.3%. Write-path restriction must be enforced out-of-band plus checked pre-merge, never trusted to the brief.
- **A test-gated integration step is mandatory** and is currently absent from the arc; it is the load-bearing component in every system that measured a win. The primary human gate belongs at merge, not only at spawn.
- **A child's "done" is an executable check on its published artifact, never a message.** Self-reports are unreliable.
- **Dependency-gated scheduling is primary; mid-task park-and-resume is the fallback** for dependencies the plan did not foresee.
- **Keep explicit authorization-scope text in every child brief** — removing it measurably raises scope overreach.
- **Do not build orchestrator-reviews-children-in-prose**; reflection-style review measures worse than doing nothing.
- **Open risk:** the multi-agent benefit is conditioned on *low* inter-task cohesion, and a multi-repo re-architecture with declared handoff contracts is high-cohesion by construction. Cheap empirical validation on one real initiative should precede building slice 3 in full. Multi-repo evidence is essentially absent from the literature; a cross-repo integration-test harness may be the actual hard part.

## 12. Pre-existing defects in scope

Two live bugs sit inside this slice's blast radius and are fixed here:

- **`waitReady` can pause forever** — on a trust-marker match it `continue`s without incrementing `waited`, so the timeout clock never advances. Cap trust-pending time.
- **`tmux new-session -c <nonexistent dir>` exits 0 and silently falls back to `$HOME`** (verified: `pane_current_path` = home). `Launch` and `Resume` pass cwd straight through with no stat, so a stale path starts a real agent in the wrong directory — contradicting "failures must be visible". Validate before creating the session (§9).
