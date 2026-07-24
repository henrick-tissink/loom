# Loom GUI — Three-pane navigation (projects · threads · stage) — Design

**Status:** Revision 1 (pre-review)
**Date:** 2026-07-24
**Scope:** GUI only. Restructure the left navigation from one accordion rail (projects with nested session dropdowns) into a **three-column** layout: a projects pane, a threads pane, and the existing stage. No backend, store, or DTO change — all the data already exists (slice 1). This is a frontend render restructure plus view state.

## 1. What this delivers

```
┌──────────┬──────────────────┬────────────────────────┐
│ PROJECTS │  THREADS in the  │        STAGE           │
│          │  selected project│  (overview / terminal) │
│ ● Attn 4 │  ── NEEDS YOU ──  │                        │
│ Innostrm3│  ▸ fix wallet…    │   ❯ the live claude    │
│ HappyPay1│  ▸ Atlas contract │      (unchanged)       │
│ loom    1│  ── RUNNING ──    │                        │
│ …        │  ▸ v-atlas reader │                        │
└──────────┴──────────────────┴────────────────────────┘
```

Pick an initiative on the far left → its threads fill the middle → pick one → it opens on the right. A pinned **Attention** entry keeps loom's cross-project walk-away triage: select it and the middle pane becomes the global needs-you queue across every project. On a narrow window the projects pane collapses to icon tokens so the terminal stays usable.

## 2. Layout — the `#body` grid (binding)

`#body` becomes a **three-column grid** replacing today's `292px 1fr`:

```
grid-template-columns: <projects> <threads> minmax(0, 1fr);
grid-template-rows: minmax(0, 1fr);   /* kept from the stage-scroll fix */
```

- **Projects pane** — 190px expanded, **54px** collapsed.
- **Threads pane** — 290px, fixed.
- **Stage** — the rest (`minmax(0,1fr)`), unchanged internally (the `#term-inner` fit fix, the `.po` overview scroll, etc. all carry over).

The titlebar stays full-width above `#body` (unchanged). Each pane is its own scroll container (`min-height: 0; overflow-y: auto`), same discipline as the current rail.

**Panes are visually distinct but within Blush:** projects on `--rail`, threads on `--bg` (a hair lighter), stage on `--stage` (near-white), separated by `--edge` hairlines — verified legible in the mock.

## 3. The projects pane

A quiet project switcher. Contents, top to bottom:

1. **Header** — `PROJECTS` label + a `+ New` (project) button (today's `+ New project`, relocated here).
2. **Attention entry (pinned, binding)** — a permanent row above the project list: a softly-glowing needs-you dot + "Attention" + the **total** needs-you count across all visible projects. A hairline divider separates it from the projects. It is a selectable pseudo-project (§6), not a real one.
3. **Project rows** — one per visible project. Each shows: the project name; a **red needs-you count badge** when it has needs-you sessions; and a **small green "running" dot** (presence, not a count) when it has running sessions and no needs-you. `Ungrouped` renders last, dim/italic. Selected project gets the `--stage` background + accent left-bar (today's `.thread.active` treatment).
4. Hidden projects do **not** appear here (that is what hiding means); they remain reachable through the existing titlebar chip / "Hidden projects" modal. Solo is respected (only the soloed project and Attention show).

**Ordering (binding):** Attention pinned first; then projects with needs-you (by count desc); then projects with running work; then the rest alphabetical; `Ungrouped` last. Attention-first, matching today's rail identity.

**Counts are computed client-side** from the already-server-filtered `latestSessions` (each carries `projectRoot`), grouped by project. No new binding.

## 4. The threads pane

Two modes, driven by the projects-pane selection.

**Project mode** (a real project selected):
- **Header** — the project name; a `+ New` (session, scoped to this project) button; a sub-line `N repos · overview` where **overview** is a link that returns the stage to the project overview (§5).
- **Thread list** — that project's live + finished sessions grouped by status, exactly today's rail groups (`NEEDS YOU / RUNNING / IDLE / FINISHED`), same row rendering (`.thread`: glyph · title (2-line clamp) · repo sub-label · ctx bar · status word · hover actions). Filtered to `session.projectRoot === selected` over `latestSessions ∪ latestRecent`.
- Empty state: "No sessions yet. Press + New to start one."

**Attention mode** (the Attention entry selected):
- **Header** — glowing dot + "Needs you" + "N across all projects".
- **List** — every needs-you session across all visible projects, each row showing its **project** as the sub-label (not its repo, since they're mixed), most-recently-flipped first. Same `.thread` rendering. Selecting one attaches it (§6).

## 5. The stage (unchanged surface, new triggers)

The stage keeps both existing surfaces and both existing code paths:
- **Project overview** (`openProject`) — repos, sessions, architecture, orchestration — shown when a project is selected and no session is (i.e. right after selecting a project, or via the header's `overview` link).
- **Terminal** (`selectSession`) — shown when a thread is selected. The `#term-inner` fit fix and all terminal wiring carry over untouched.
- **Attention + no session** — the stage shows a quiet prompt ("Pick a thread on the left to attend to it"), since Attention has no single overview.

Switching surfaces reuses the existing teardown/attach: selecting a project tears down the terminal PTY (the tmux session survives, as always) and renders the overview; selecting a thread attaches. No change to session lifetime semantics.

## 6. Interaction & selection model (binding)

Two selection axes, held in frontend view state:
- `selProject`: a project root, or the sentinel `"__attention__"`.
- `selSession`: a session name, or `null`.

Transitions:
- Click a **project** → `selProject = root`, `selSession = null` → threads pane fills; stage shows that project's overview.
- Click **Attention** → `selProject = "__attention__"`, `selSession = null` → threads pane shows the global queue; stage shows the attend-prompt.
- Click a **thread** → `selSession = name` (project unchanged) → stage attaches the terminal; the threads list stays put so the next thread is one click away.
- Click the header **overview** link → `selSession = null` → stage returns to the overview.

**Default on launch (binding):** `selProject = "__attention__"`, `selSession = null` — loom opens on the triage view, true to its attention-first identity. If the last-selected project is persisted and still visible, restore it instead (§7).

**Poll behavior:** the 1.5s poll refreshes the projects-pane counts and the threads list in place (never rebuilding a field being typed into, same guard as today's rail). A session that flips to needs-you updates its project's badge and, if Attention is selected, appears in the queue — the level-triggered self-heal already in place.

## 7. Collapse & persistence

- **Manual toggle** — a `‹›` control (in the titlebar's left, by the wordmark) collapses/expands the projects pane. The choice persists.
- **Automatic** — below a window-width threshold (**~980px**, tuned so the stage keeps ≥ ~600px), the projects pane auto-collapses; above it, it restores the manual preference. Auto-collapse never overrides an explicit expand at a width that can hold it.
- **Collapsed rendering** — a 54px strip: Attention as the glowing dot + count badge on top; each project as a rounded **initials token** (`I`, `HP`, `L`, `UE`, …) with its needs-you count as a corner badge and the running dot when applicable; `title` tooltips give the full name; selected token highlighted. `Ungrouped` is a dim `·`.
- **Persistence:** collapse preference and last-selected project live in `localStorage` (pure view state, per-machine, no backend). This mirrors the precedent that section-collapse persisted view state, but avoids a Go round-trip for something this ephemeral. (Project-section collapse — today's `projects.collapsed` column — is obsolete under this design and its writes are dropped from the GUI; the column stays, unused, rather than a migration to remove it.)

## 8. Data & reuse (no backend change)

- **Projects:** `ListProjectDetails()` — existing, unchanged.
- **Sessions / recent:** `ListSessions()` / `ListRecent()` — existing; already carry `projectRoot`/`projectName` and are already visibility-filtered server-side.
- **Overview / orchestration / documents:** `openProject` and its `OrchestrationSnapshot`/`ProjectDocuments` calls — existing, unchanged; they fire only while a project overview is on the stage, preserving the §7.5 cost ceiling from the orchestration-view spec.
- **Launch / attach / diff:** `LaunchSession`, `AttachSession`, `SessionDiff`, etc. — existing, unchanged.

**Everything the three panes render already exists client-side** (`latestSessions`, `latestRecent`, `latestProjects`). This is a re-layout of data the poll already has. No new Go binding, DTO, or migration.

## 9. Hiding & solo

- Hidden projects are absent from the projects pane; their sessions are absent from every threads-pane view (already server-filtered). Reach/unhide them via the existing titlebar chip and "Hidden projects" modal.
- Solo: only the soloed project and the Attention entry render; the Attention queue narrows to the soloed project's needs-you.
- The projects-pane counts derive from filtered `latestSessions`, so a hidden project can never contribute to the Attention total or a badge.

## 10. Reuse vs new code

`main.js` (the busiest frontend file) is the blast radius. Changes are contained to the rail-rendering region:
- **Replace** `renderRail` (today's single-pane accordion) with `renderProjectsPane` + `renderThreadsPane`, both pure of the data the poll holds.
- **Add** the projects/threads/collapse view-state and the selection transitions (§6).
- **Reuse unchanged:** `openProject` (overview), `selectSession`/`teardownTerminal` (terminal), the `.thread` row renderer and its helpers (`displayName`, `ctxGaugeHtml`, kill/summarize action buttons), the poll loop, hiding filters.
- **tokens.css:** the `#body` grid, the two new panes, the collapsed strip, the Attention entry. The existing `.thread`, `.po`, `#terminal`/`#term-inner`, titlebar rules carry over.
- If `renderProjectsPane`/`renderThreadsPane` grow past a few hundred lines they move to their own module; the goal is focused, testable render functions, not one giant `renderRail`.

## 11. Testing

No Go test suite change (no Go change). Frontend has no unit runner, so the discipline is: **each behavior below is verified in the faithful browser mock/harness before ship, and the checks are listed here so the reviewer can re-run them.**

- Three-pane grid at wide, mid, and ~760px widths — stage never below its usable floor; auto-collapse fires under the threshold and restores above it.
- Projects pane: ordering (attention-first → running → alpha → Ungrouped); needs-you count and running dot correct against the session set; selected highlight; hidden projects absent; solo narrows.
- Attention entry: total count = sum of visible needs-you; selecting it shows the global queue with project sub-labels; empty state when nothing needs you.
- Threads pane: project mode groups match today's rail; Attention mode shows cross-project rows; empty states; the poll updates counts/lists in place without losing a typed field.
- Selection transitions (§6) incl. the overview link and default-on-launch; terminal attaches on thread click and the tmux session survives a project switch.
- Collapse: manual toggle persists; initials tokens + badges render; tooltips; selected token highlighted.
- Regression: the terminal fit (`#term-inner`), the overview scroll (`.po`), hiding across all surfaces, and the status-bar all still behave.

## 12. Accepted limits

Threads pane is a fixed 290px (not user-draggable) in v1 — draggable dividers are a later nicety, not now. The projects pane collapse is width-threshold + manual, not a full responsive redesign. Attention mode has no per-project sub-grouping in v1 (a flat, recency-ordered list) — grouping the queue by project is a possible follow-on. No change to the TUI (this is GUI-only; the TUI keeps its single rail). Section-collapse state (`projects.collapsed`) is abandoned by this design but not migrated away.
