# Loom — Orchestration View: architecture, decisions, and the live delegation graph — Design

**Status:** Revision 2 (hardened after adversarial review: 1 finding folded)
**Date:** 2026-07-22
**Scope:** Slice 4 of 4 in the orchestration arc — the **visual layer**. Fills the project-overview shell slice 1 deliberately left empty (`#po-arch`, `cmd/loom-gui/frontend/main.js:639`), renders agent-authored architecture outlines and decision records inside Loom, and paints the orchestrator/child delegation graph live while a run is in flight. This slice **renders and analyses; it never authors, never advances a run, and never writes into the user's workspace.**

---

## 1. What this delivers

The requirement is literal: the orchestrator's picture of the work must be *in the platform*, legible at a glance, and true while the run is moving.

1. **The overview stops being a shell.** A project's page gains: the architecture outline (agent-authored markdown, rendered), the decision records (as cards with status and date), the interface contracts children are gated against, and a run strip for the active orchestration run.
2. **The delegation graph.** Orchestrator and children as a dependency DAG. Per-node live state fused from the same status engine the rail uses. Wait-edges that light when the waiter is actually parked. The current bottleneck and the longest remaining chain readable without reading anything.
3. **Blocked-on-you, first.** A strip above the graph listing every node where *the human* is the blocker — a `needs_you` child, a failed gate awaiting a decision, an integration merge awaiting approval. This is the single highest-value element on the page and it is placed accordingly.
4. **Honest emptiness and honest breakage.** No orchestrator, no manifest, or a manifest that will not parse each render as a specific, named, visible state. There is no path through this spec that produces a blank panel.
5. **Zero new frontend runtime dependencies** (§6). The repo has shipped one vanilla-ESM frontend with xterm and nothing else; that holds.

---

## 2. Inherited constraints, restated as rendering rules (BINDING)

Slice 1 §11 constrains what this slice is allowed to show. Each constraint has a visual consequence, and the consequences are binding because a UI that displays the wrong authority re-introduces the failure the constraint was written against.

1. **A child's "done" is an executable check on a published artifact, never a message.** Therefore: a node's *completion* badge is rendered **only** from a recorded gate result (command, exit code, timestamp). Transcript-derived session status (`running`/`needs_you`/`idle`) is rendered as a *separate, subordinate* dot and is never promoted to "done". A child that says it is finished and whose gate has not run renders **`awaiting check`**, not green. Self-report is not a state this UI can express.
2. **No orchestrator-reviews-children-in-prose.** There is no card, panel, tooltip, or column anywhere in this slice that renders an orchestrator's opinion of a child's work. Reflection-style review measured worse than nothing; surfacing it would make it load-bearing. Node evidence is: the gate result, `SessionDiff` on the child's worktree, and the child's own outcome from the memory index — all primary sources.
3. **Isolation is visible.** Every child node renders its **worktree path** (or container id). A node whose declared write scope is not an isolated tree renders a warning chip. This is a display of an out-of-band-enforced fact, not a substitute for enforcing it.
4. **The integration gate is a node, not a footnote.** The test-gated integration step appears in the graph as a first-class node of kind `integration`, always terminal, with the human merge gate rendered on it.
5. **Authorization scope is displayed verbatim.** The node inspector shows the child's authorization-scope text from its brief, unedited and untruncated (scrolled if long). Removing it from the brief raises overreach; the UI's job is to make its presence or absence auditable at a glance — a node whose brief carries no scope block renders a warning chip.
6. **Dependency-gated is primary; park is the fallback — and the graph says which.** Plan edges and park edges are drawn differently (§5.1) and the count of park edges is rendered as a run-health number. A run that accumulates park edges is a run whose plan was wrong, and that is worth seeing.
7. **Low inter-task cohesion is the precondition, so the graph must make cohesion visible.** Edge density and the number of nodes touching the same repo are rendered in the run strip. This is the cheap empirical read slice 1 §11 asks for before full commitment.

---

## 3. The project overview, filled in

`renderProject()` today emits: header (name, tags, root, hide/solo/rename/re-point), Repos, Sessions, and the empty `#po-arch` seam. Slice 4 renders **into that seam** and adds nothing above it, so the existing overview tests keep meaning what they say.

> The seam's comment reads *"Seam for slice 2's architecture view"*. Slice ordering moved after slice 1 shipped (slice 2 = orchestrator + brief assembly; slice 4 = rendering). Fix the comment in this slice's first commit; a stale seam comment is how the next reader mis-attributes the whole view.

The seam expands into four stacked blocks, in this order (BINDING — order is the argument):

| # | Block | Present when |
|---|---|---|
| 1 | **Run strip** — active run: name, elapsed, `k/n` nodes integrated, park-edge count, cohesion read, run switcher if >1 active | a run row exists for this project |
| 2 | **Blocked on you** — human-blocking nodes, click to attach/act | any such node exists |
| 3 | **Delegation graph** | a parseable manifest exists |
| 4 | **Architecture & decisions** — outline reader + decision cards + contracts | any document resolves (§4) |

Blocks 1–3 are absent, not empty, when there is no run: a project with no orchestration shows only block 4, and a project with neither shows the existing overview exactly as it is today. **Absence of orchestration is not an error state** and must not render as one.

Everything in the seam reuses existing Blush primitives: `.po-block`/`.po-bhead`/`h3` for section chrome, `.sh-btn` for actions, `.tact` for row actions, `.po-warn` for warnings, `.modal` for the node inspector, `statusColor()`/`statusWord()` from `theme.js` for session state.

### 3.1 Visibility is enforced on the payload, not on the route (BINDING)

Revision 1 assumed a hidden project's overview was unreachable and let the seam inherit that. **That is false against shipped code, and the mistake is load-bearing**, so the rule is restated here rather than left to §9.

Slice 1 shipped the opposite, deliberately: `ListProjectDetails` is explicitly **not** §6-filtered (`cmd/loom-gui/projects.go:183-186`, `main.js:14-16`), because Hide/Show and Solo/Leave-solo live on that very screen (`main.js:621`, `main.js:653-654`), `openHiddenProjects()` (`main.js:808`) exists precisely to reach a hidden project, and `projectAction()` re-polls and re-renders the overview **in place** — so immediately after clicking Hide the user is left sitting on a hidden project's overview, by design. A project that vanished from its own settings screen could never be unhidden.

That screen is safe today only by accident: everything on it is either already §6-filtered upstream (`projectSessionsHtml` filters `latestSessions`/`latestRecent`, which the server filtered, so the Sessions block reads `none`) or non-identifying. Slice 4 destroys that accident. `OrchestrationSnapshot(root)` is keyed on a **root**, not on a session list, and inherits no filter; and everything §5 asks it to return — node titles, `repo` and `worktree` paths, brief paths, verbatim authorization-scope text, gate command tails, document bodies — is exactly the client-identifying material §6 exists to keep off a shared screen. Left unfixed, the highest-content surface in the app would be the one surface that ignores hiding, on the one screen guaranteed to be open at the moment of hiding.

Therefore, binding:

1. **The server call is the gate, not the route.** `OrchestrationSnapshot(root, sinceRev)`, `ProjectDocuments(root)` and `ProjectDocument(path)` each construct the resolver and evaluate the predicate themselves, **before touching disk**. When `!res.ProjectVisible(root)` they return an empty payload carrying a single `hidden: true` marker and nothing else: no `rev`, no run title, no node or document count, no error text, no path. **Fail closed** — an unattributable, unresolvable or unknown root is treated as hidden, matching slice 1 §6's fail-closed rule for unattributable rows.
2. **`ProjectDocument(path)` attributes before it admits.** Its argument is a path, not a root, so it resolves the owning project via the resolver and refuses when that project is not visible. This is an **additional** check, not a replacement for §4.2 containment; both must pass, and the visibility check runs first so a refusal never echoes a path from a hidden project.
3. **The hidden render is constant text.** The seam renders one line — *"hidden — orchestration view suppressed; unhide to view"* — whose wording does **not** vary with whether a run, a manifest or any document exists. A hidden state that renders differently for a project with a run than for one without is itself the leak, in one bit.
4. **Node-level filtering, because a run can span a hidden repo.** A run is scoped to a subset of *one* project's repos (slice 1 §2), but exclusive membership is per-repo and a `park` edge or a shared contract can point a node at a repo whose owning project is hidden or solo-suppressed. Any node whose `repo` or `worktree` fails `res.Visible(repo, worktree)` is replaced, server-side, by an opaque **`hidden` placeholder node**: edges, rank, ready-set and bottleneck are computed from it so the picture stays *structurally* true, but it carries no title, repo, worktree, brief path, scope text, artifact list or check tail. The run strip states a **bare count** — *"2 nodes hidden"* — never a name, per slice 1 §6.4's ban on identity-bearing counts.
5. **Everything downstream reads the filtered payload.** Blocked-on-you (§5.5), the run strip figures, cohesion and park counts are computed from the post-filter DTO, never from raw store rows. There is one filter site per call; a second would be a second bug, exactly as §4.2 says of path matching.
6. **§6.3's GUI leak-surface list gains four entries:** orchestration snapshot · delegation graph · blocked-on-you strip · document reader. Slice 1 §9 requires one test per surface per frontend; §12 carries them.

This is not defence in depth over an existing gate — **it is the only gate**, because the route is deliberately open. That asymmetry is why the rule is binding here and tested twice in §12.

---

## 4. Documents: what gets rendered, and from where

### 4.1 Discovery is declared first, discovered second (BINDING)

The document set is **declared by the manifest** (`documents[]`, §5.3). A rendering layer must not guess which markdown file is the architecture.

Fallback, used only when there is no manifest (block 4 must still work for a project with no orchestrator): a fixed, ordered convention scan under the project root and each member repo — `docs/ARCHITECTURE.md`, `docs/architecture/*.md`, `docs/decisions/*.md`, `docs/adr/*.md`, `ADR/*.md`. Non-recursive beyond those globs. This mirrors slice 1 §3's discipline: an ordered, first-match, explicitly-enumerated rule set, not a heuristic crawl.

### 4.2 Containment is a security boundary, not a tidiness rule (BINDING)

The manifest is **agent-authored**, which puts it in the same trust class as transcript content in ARCHITECTURE.md §10: untrusted input that Loom reads. A `documents[]` entry naming `~/.ssh/id_rsa` or `../../other-client/secrets.md` must never render.

Every document path is resolved and admitted only if **all** hold:

1. `filepath.Abs` + `Clean`, then `EvalSymlinks` (the physical form — slice 1 §4 and the add-dir spike; a symlink out of the tree is the obvious escape).
2. Segment-wise contained in the project root **or** one of its `project_repos.path` entries, using the exact matcher `internal/projects` already exports. No new path logic; a second implementation is a second bug.
3. Extension is `.md`.
4. Size ≤ 512 KB. Over-cap renders the head with a visible `…[truncated at 512 KB]` marker (the workflow-substitution precedent, §2.3 of the workflows spec).

A refused path renders as a `.po-warn` card naming the path and the rule it broke. **Refusals are visible, never silent** — a silently-dropped document is indistinguishable from a missing one, and the user would debug the wrong thing.

### 4.3 Document kinds and how each renders

| Kind | Renders as |
|---|---|
| `architecture` | Reading pane: rendered markdown, with an auto-built section outline (h2/h3) pinned right. Mermaid fences per §6. |
| `decision` | A card: title, status chip, date, one-line consequence, expandable body. Cards sort newest-first, superseded ones dimmed. |
| `contract` | A card linked from the graph nodes that cite it; renders the interface a child must publish and the gate command that checks it. |

Decision metadata comes from a **restricted front-matter subset**: a leading `---` block of flat `key: value` scalar lines, keys `title|status|date|supersedes|id`. No nesting, no lists, no anchors — a full YAML parser is a dependency and an attack surface for a four-key need. Malformed front-matter renders as body text plus a warning chip, never fatally.

**Missing metadata is stated, never invented (BINDING).** With no `status:` the chip reads `status unknown`, not `accepted`. With no `date:` the card shows the file mtime, labelled `mtime` so it is not mistaken for a decision date. This exists because a decision record UI that confidently displays a fabricated "Accepted" is worse than one that displays nothing.

### 4.4 Markdown rendering (Go side, no dependency)

`internal/arch` parses markdown into a **safe token tree** (headings, paragraphs, lists, fenced code with an info string, blockquote, table, inline code/emph/strong/link) which the frontend paints into DOM nodes. Rules, binding:

- **Raw HTML in the source is escaped and rendered as literal text.** No passthrough, ever. The renderer's output is a token tree, not an HTML string, so there is no injection site by construction.
- Links: `file:`/relative paths route to the existing `OpenInEditor`; `http(s)` route to the existing `OpenURL` gate; **every other scheme is rendered as inert text** (that gate already exists and is already tested — reuse it, do not re-derive).
- Unsupported constructs degrade to their source text. There is no failure mode where the pane is empty because of a syntax the parser did not know.

Documents are **read from disk at render time and never copied into `loom.db`.** The file is the source of truth; a cached copy is a staleness bug waiting for a git checkout. In-memory cache keyed on `(path, size, mtime)` — the same fingerprint discipline `indexed_files` uses.

---

## 5. The delegation graph

### 5.1 Node and edge model

**Node kinds:** `orchestrator` (exactly one, rank 0) · `child` · `integration` (the mandatory test-gated step; terminal) · `ghost` (synthesised by slice 4 for a dangling edge target, §9).

**Node state** is a pair, rendered as two distinct marks, and conflating them is the mistake this design is built to avoid:

- **Lifecycle** (authoritative, from manifest + gate results): `planned` → `ready` → `running` → `awaiting check` → `checked` / `check failed` → `integrated`; plus `parked`, `abandoned`. Rendered as the node's border and badge.
- **Session status** (decorative, from `status.Snapshot` via the existing poll): `running|needs_you|idle|done|error|unknown`. Rendered as the small `statusColor()` dot, exactly as the rail does. Never as the badge.

**Edge kinds:**

| Kind | Meaning | Drawn |
|---|---|---|
| `plan` | dependency-gated, foreseen at planning time | solid, rank-forward |
| `park` | discovered mid-task; the waiter parked on something the plan missed | dashed, drawn in the back-edge band above the ranks, counted in the run strip |
| `gate` | node → integration node | solid, heavier weight |

A **wait-edge lights** (slow directional dash, ~2.2s, `prefers-reduced-motion` gated) only when the edge is *actually costing time right now*: the target node's lifecycle is `parked` or `ready`-but-blocked **and** its session status is not `running`. An edge into a node that is merrily working is not a wait. One animation, one meaning — consistent with the design language's "one earned moment".

### 5.2 What slice 4 computes vs what it is given

Slice 4 computes, from the manifest and the poll (no new persistence):

- **Ready set** — nodes whose every incoming `plan`/`park` edge terminates in a `checked`/`integrated` node.
- **Bottleneck** — among nodes currently blocking anything, the one with the largest count of *transitively blocked descendants*; ties broken by longest blocked-duration. Rendered as a single, subtle `bottleneck` chip on that node, and named in the run strip. Exactly one, or none.
- **Longest remaining chain** — the longest path in the subgraph of not-yet-`integrated` nodes, highlighted on hover of the run-strip figure.

**We do not have schedule estimates and we do not fabricate them (BINDING).** The chain is measured in **nodes**, not minutes, and is labelled *"longest remaining chain: 4 nodes"* — never "critical path: 2h 40m". Weighting unstarted nodes by a made-up duration would produce a confident, wrong number on the one screen the user is meant to trust at a glance. If durations are wanted later, the honest input is measured per-node elapsed from completed runs, which does not exist yet.

### 5.3 The manifest contract (BINDING ON SLICE 3)

One **JSON** file per run — not markdown, not front-matter, not convention-discovered. Its path is recorded on the run row; the renderer never guesses. It carries a `schema` integer so a future shape renders as *"manifest schema 2; this build renders 1"* instead of silently mis-drawing.

```jsonc
{
  "schema": 1,
  "run_id": 42,
  "project_root": "/Users/h/Sauce/Innostream",
  "title": "Atlas re-architecture",
  "rev": 7,                       // monotonic; bumped on every write (§7)
  "documents": [
    { "kind": "architecture", "path": "docs/atlas/OUTLINE.md", "title": "Atlas outline" },
    { "kind": "decision",     "path": "docs/decisions/0003-event-bus.md" },
    { "kind": "contract",     "path": "docs/atlas/contracts/ledger-v2.md", "id": "ledger-v2" }
  ],
  "nodes": [
    {
      "id": "orchestrator",
      "kind": "orchestrator",
      "title": "Atlas orchestrator",
      "session_name": "loom-8f3…",       // may be ""
      "claude_session_id": "8f3…",       // identity, survives resume
      "repo": "/Users/h/Sauce/Innostream/bankenstein",
      "worktree": "",
      "lifecycle": "running",
      "brief_path": "…/briefs/orchestrator.md",
      "authorization_scope": "May edit …; may NOT …",
      "artifacts": [],
      "check": null,
      "decisions": [],
      "depends_on": [],
      "created_at": 1753… , "started_at": 1753…, "parked_at": 0
    },
    {
      "id": "ledger",
      "kind": "child",
      "title": "Ledger v2 in bankenstein",
      "session_name": "loom-a12…", "claude_session_id": "a12…",
      "repo": "/Users/h/Sauce/Innostream/bankenstein",
      "worktree": "/Users/h/.loom/wt/42-ledger",
      "lifecycle": "awaiting check",
      "brief_path": "…/briefs/ledger.md",
      "authorization_scope": "…",
      "artifacts": ["internal/ledger/…"],
      "check": { "cmd": "go test ./internal/ledger/...", "last": { "ok": false, "exit": 1,
                 "ran_at": 1753…, "duration_ms": 8421, "tail": "…last 40 lines…" } },
      "decisions": ["0003-event-bus"],
      "contracts": ["ledger-v2"],
      "depends_on": [ { "id": "schema", "kind": "plan" },
                      { "id": "auth",   "kind": "park", "since": 1753…, "reason": "needs auth token shape" } ]
    }
  ]
}
```

Field-level demands, each with the reason it is non-negotiable for this slice:

| Field | Why slice 4 cannot render without it |
|---|---|
| `claude_session_id` | Node → live status must resolve **by claude id**, not tmux name. A resumed child mints a new tmux name (ARCHITECTURE §4.1); keying on `session_name` greys out every node the moment a step is resumed. |
| `worktree` | §2.3 — isolation must be visible. |
| `authorization_scope` | §2.5 — its presence must be auditable. |
| `check.cmd` + `check.last` | §2.1 — the *only* legitimate source of a completion badge. |
| `depends_on[].kind` | §2.6 — plan vs park is the run-health signal; a bare id list erases it. |
| `rev` | §7 — cheap change detection under the poll model. |
| `parked_at` / `since` | Blocked-duration, which is the bottleneck tie-break. |

### 5.4 Does slice 3 produce this? — the honest answer

**Not yet, and this section is a demand on slice 3, not a confirmation from it.** At the time of this revision, `docs/superpowers/specs/` contains slice 1 only; slices 2 and 3 are unwritten. Claiming confirmation would be exactly the kind of unverified self-report §2.1 forbids.

What can be said, and is what makes the demand cheap:

- `claude_session_id`, `session_name`, `repo` — slice 3 must already hold these to spawn and address a child; `store.GetLatestByClaudeSessionID` is the existing primitive.
- `worktree` — slice 1 §11 obliges slice 3 to create worktrees; recording the path is a field, not a mechanism.
- `check.cmd`/`check.last` — slice 1 §11 obliges slice 3 to run an executable check; recording exit code, timestamp and a bounded tail is the same field-vs-mechanism gap.
- `depends_on[].kind` — slice 3 must already distinguish planned dependency-gating from park-and-resume to schedule at all.
- `authorization_scope` — obliged to be *in the brief*; the manifest must echo it so the renderer does not parse briefs.
- `rev`, `parked_at` — pure bookkeeping.

**Therefore §5.3 is binding on slice 3 and must be reviewed against slice 3's spec before either is built.** If slice 3 ships without a field, the failure is defined, not accidental: §9's degradation matrix has a row for every one of them, and slice 4 renders a named gap rather than a wrong picture.

**Slice 4 ships useful work even if slice 3 slips**, because the frontend already gates on `bound("…")` (`main.js:29`): an unbound `OrchestrationSnapshot` degrades block 4 to documents-only and blocks 1–3 to absent. Stage 4a (§11) depends on nothing from slice 3 at all.

### 5.5 Blocked-on-you

A node is human-blocking when any of: its session status is `needs_you`; `check.last.ok == false` and nothing has run since; lifecycle is `integrated`-pending-merge (the mandatory human gate); its session row carries `seed_status=failed` or a non-empty `pending_seed` (reuse the workflow-run rendering precedent — Loom already renders `seed pending` / `seed FAILED` honestly).

Each entry: node title, one-line reason, and the action that unblocks it (attach · view diff · view failing check output · approve merge). Rows reuse `.po-sess` shape and click semantics.

The strip is computed from the **already-filtered** snapshot payload (§3.1.5), never from raw session or run rows: a hidden node cannot become human-blocking, and the whole strip is absent under the hidden payload.

**Scoped to the project overview, deliberately.** A global orchestration inbox would be a *new* leak surface under slice 1 §6.3 and would need its own filter and its own per-frontend test. The titlebar attention count already covers the global case. If a global inbox is built later it must be added to §6.3's list first.

### 5.6 Layout

Layered DAG, computed **in Go** (`internal/arch/layout.go`), returning coordinates; the frontend paints inline SVG.

- **Rank** = longest-path depth over `plan` and `gate` edges. `park` edges do not contribute to rank (they are frequently backward — that is what makes them park edges) and are routed through a back-edge band.
- **Order within a rank** = two barycenter sweeps for crossing reduction, with ties broken by node id. **Layout is deterministic**: the same manifest yields byte-identical coordinates, which is what makes golden tests possible and what stops the graph shuffling under the user on an unrelated status tick.
- **Orientation left-to-right.** Node cards carry a title, a repo chip and a badge; they are wide, and a top-down layout wastes the stage's horizontal budget.
- Edges: orthogonal elbows with a short straight run into the target, so many-into-one reads as a bundle rather than a fan.
- Viewport: fit-on-first-render, then pan/drag and ⌘-scroll zoom. **No manual node dragging** — a hand-positioned graph that re-layouts on the next manifest write is a promise the model cannot keep.

Why layout in Go, not JS: it is pure, it is the single most test-sensitive piece of this slice, this repo's test mass lives in Go (11.5k test lines), a future TUI view could reuse the ranks, and it keeps the frontend at zero new dependencies.

---

## 6. Diagrams: mermaid is the format, and no renderer is vendored (BINDING)

**Format decision:** the on-disk format for agent-authored diagrams is **mermaid in fenced blocks**. Agents write it natively and unprompted; it survives outside Loom (GitHub renders it); it is diffable text. No argument against it.

**Runtime decision: mermaid.js is NOT bundled.** The GUI is a Wails webview serving `//go:embed`ed vite output with no network, so a vendored mermaid *would* work offline — the objection is not feasibility:

1. **Size.** Mermaid with its layout backends is ~2.5–3 MB minified, several times the entire current frontend, in an app whose stated frontend property is "dependency-light: vanilla ES modules + xterm.js".
2. **It gives us the wrong error.** Mermaid renders a syntax failure as its own full-width error graphic. This spec requires malformed input to render as *Loom's* named, actionable error (§9). A renderer we cannot get a structured `(line, col, message)` out of cannot satisfy that.
3. **It buys nothing for the graph.** The delegation graph is structured data that must be hit-tested, patched live, and laid out stably. Round-tripping it through generated mermaid to get a picture back would forfeit node identity, live patching and layout determinism — the three things §5 is built on. **The delegation graph is never mermaid, in either direction.**
4. **Theming.** Mermaid's theming would have to be bent to Blush tokens across two terminal themes; hand-painted SVG inherits them for free.

**What ships instead:** `internal/arch/mermaid.go` parses a **defined subset** and hands it to the *same* layout engine §5.6 already needs — one layout implementation, two callers.

Supported subset (BINDING; anything outside is "unsupported", not "broken"):
`flowchart`/`graph` with `TD|TB|LR|RL` · node declarations `id`, `id[Label]`, `id(Label)`, `id{Label}`, `id([Label])` · edges `-->`, `---`, `-.->`, `==>`, each with optional `|label|` · `subgraph … end` (one nesting level, rendered as a titled band) · `%%` comments.

Everything else — sequence, class, state, ER, gantt, pie, styling directives, click handlers — is **unsupported by design**. An unsupported diagram renders as a syntax-highlighted code block with a quiet chip: *"mermaid `sequenceDiagram` — shown as source"*, plus "Open in editor". That is a legible outcome, not a failure. A *malformed* diagram inside the supported subset renders the source **plus** a red line with the line number and the parse message.

This costs a few hundred well-tested Go lines and no dependency. The rejected alternative — vendor mermaid, render into a shadow root, restyle — was rejected on (1)+(2), and the *further* alternative of shelling out to a mermaid CLI was rejected outright: it is a network-installed npm binary and a subprocess on a render path.

---

## 7. Staying live under the poll model

**The poll stays. No websockets, no Wails events, no watchers.** `poll()` runs every 1.5s and already batches `ListSessions` / `ListRecent` / `ListProjectDetails`; ARCHITECTURE §5 and the §7 concurrency guards assume that shape.

Additions:

1. One more batched call, **only while `stageView.kind === "project"` and the seam is populated**: `OrchestrationSnapshot(root)`. Off the project page it is not called at all.
2. `OrchestrationSnapshot` returns `{ rev, statuses[], strip }` cheaply, and the **full node/edge/layout payload only when `rev` changed** since the client's last-seen. `rev` = manifest `rev` field, falling back to `(mtime, size)` when the field is absent — the `indexed_files` fingerprint discipline again.
3. **Two update paths, and the split is the whole point:**
   - *Status tick* (every poll): patch in place. Each node card is a DOM node keyed by node id; only the status dot, badge, blocked-duration and edge-lit classes change. **No re-layout, no `innerHTML` on the graph host.** This is the same lesson already encoded at `main.js:1418` — re-rendering the whole overview every 1.5s destroys in-flight user state (there, a half-typed name; here, pan/zoom and an open inspector).
   - *Rev change* (rare): re-layout. New nodes fade in; removed nodes fade out; **the viewport transform is preserved**. If the node set changed, a quiet `graph changed` chip appears in the run strip rather than the picture silently reshuffling under the eye.
4. Documents refresh on `(size, mtime)` change only, checked on the same tick, at most one stat per declared document.
5. **Cost ceiling (binding):** a poll that finds `rev` unchanged does no file reads beyond the manifest stat and the document stats, and no graph work. A run with 40 nodes must not make the 1.5s poll measurably slower than it is today; if it does, the snapshot call moves to its own slower timer (4s) — stated now so the fix is a decision, not a scramble.

---

## 8. Visual language

Everything derives from `tokens.css` and `theme.js`. **No new hues.** The graph is neutral-on-`--stage` with chroma spent only where the app already spends it: attention.

| Element | Token |
|---|---|
| Node card | `background: var(--surface)`, `border: 1px solid var(--edge)`, `border-radius: 9px`, `--shadow-card` when it is the bottleneck |
| Node title / repo chip | `--font-mono` 12.5px / `.po-tag` |
| Session status dot | `statusColor()` — literally the rail's palette, so a dot means the same thing everywhere in the app |
| `check failed` | the existing error ink `#a5453a` on `#F7E4E0` with `#E7B3A9` border (the `.po-warn` / `.wf-bad` pairing already in the sheet) |
| `integrated` | `--done` green, receded |
| Blocked-on-you node | `--accent-soft` fill, `--accent-line` border, `--accent-ink` text — the `.attn` / `.hidechip.solo` treatment |
| `plan` edge | `var(--edge)`, 1.5px |
| `park` edge | `var(--accent-line)`, dashed |
| Lit wait-edge | `var(--accent)`, dashed, animated `stroke-dashoffset` ~2.2s |
| Section chrome | `.po-block` / `h3` micro-caps (10px, `0.16em`, `--text-faint`) |
| Node inspector | `.modal` (its `blush-pop` entrance already exists) |

Motion budget: **exactly two moments** — the lit wait-edge, and the existing `.attn` dot's glow on the blocked-on-you strip. Both under `@media (prefers-reduced-motion: no-preference)`. Nothing pans, zooms, scrolls or focuses itself in response to a status change; the graph must be a stable object the user's eye can return to.

Dark: the graph inherits chrome tokens, which are theme-fixed today (only the *terminal* has a dark mode, via `:root[data-term-theme]`). The graph follows the chrome, not the terminal. Stated so a later full dark theme has one place to change.

New CSS lands as one `/* ---- orchestration view (slice 4) ---- */` block appended to `tokens.css`, `.dg-*` / `.doc-*` prefixed, matching the file's existing section-comment convention.

---

## 9. Degradation matrix (BINDING — one test per row, §12)

**No row in this table renders as a blank panel, a spinner, or a silently-absent section.**

| Condition | Renders |
|---|---|
| No orchestration run for this project | Blocks 1–3 absent (not empty). Block 4 renders discovered documents. Overview otherwise unchanged. |
| Run row exists, manifest path empty | Run strip + a `.po-warn`: *"run #42 has no manifest — nothing to draw"*, with the run's sessions listed from `workflow_runs.session_names` so the agents remain reachable. |
| Manifest path set, file missing/unreadable | Error card naming the absolute path and the OS error, "Open in editor". Sessions still listed. |
| Manifest is not JSON | Error card with the parse error and **line/column**, plus the offending ±3 lines in a `.diff-patch`-styled block. Sessions still listed. |
| Valid JSON, schema-invalid (missing `nodes`, bad kind, duplicate id) | Error card enumerating up to 10 violations by JSON pointer. Sessions still listed. |
| `schema` newer than this build | *"manifest schema 2; this Loom renders schema 1 — update Loom"*. No partial draw. |
| Cycle in `depends_on` | Graph draws; the cycle's edges render red; a banner names the cycle members. Rank falls back to input order inside the SCC. **Layout never fails.** |
| `depends_on` names an unknown id | A `ghost` node labelled `unknown: <id>`, dashed border, plus a warning. |
| Node with no `session_name` yet | Drawn `planned`, hollow, no status dot. Normal, not an error. |
| Node's session killed / row gone | Lifecycle badge unchanged (it is manifest-authored); status dot goes `unknown`; the inspector says *"no live session — resume from Finished"*. |
| `check` absent on a `child` | Badge reads `no check declared` with a warning chip. §2.1 makes this a defect worth showing, not a blank. |
| `authorization_scope` empty | Warning chip *"brief declares no authorization scope"*. |
| `worktree` empty on a `child` | Warning chip *"not isolated"*. |
| Document refused by §4.2 | `.po-warn` naming path + rule. |
| Document > 512 KB | Head rendered + `…[truncated at 512 KB]`. |
| Mermaid fence outside the subset | Source block + *"shown as source"* chip. |
| Mermaid fence malformed within the subset | Source block + red line/message. |
| > 60 nodes | Auto-collapse to repo bands, expandable; a chip states the collapse. |
| > 200 nodes | Graph replaced by a node table (sortable, same data, same actions) + a stated cap. |
| Two manifests / two active runs | Run switcher in the strip; exactly one graph at a time; selection is local UI state. |
| Project hidden, or suppressed by another project's solo | **Overview stays reachable — it is the settings screen** (slice 1's deliberate exception, §3.1). `OrchestrationSnapshot` / `ProjectDocuments` / `ProjectDocument` return the empty `hidden:true` payload; blocks 1–4 collapse to one constant line, *"hidden — orchestration view suppressed; unhide to view"*, whose text does not vary with whether a run or a document exists. Hide/Show and Solo/Leave-solo keep working from the page. Unhiding restores the full view on the next poll. |
| Root unattributable / unknown to the resolver | Same hidden payload. Fail closed (§3.1.1). |
| A node's `repo`/`worktree` belongs to a project hidden by §6 (cross-project run) | Opaque `hidden` placeholder node: edges, rank, ready-set and bottleneck preserved; no title, path, brief, scope or check tail. Run strip carries a bare count chip, *"2 nodes hidden"* — never a name (slice 1 §6.4). |

---

## 10. Reuse vs new code

**Reused, unchanged:**

- `internal/projects` — the resolver: attribution, `ProjectVisible` (the §3.1 payload gate), `Visible(dirs…)` (the §3.1.4 per-node gate), and the segment-wise containment matcher §4.2 depends on. No second path-matching implementation and no second visibility predicate — the resolver is the single site slice 1 built for exactly this.
- `internal/status` + the existing `poll()` — all live node status. **No new poller, no new engine awareness of projects or runs** (slice 1 §6.2a stays true).
- `store.GetLatestByClaudeSessionID` / the `ResolveStepSession` identity pattern — node → live row across resumes.
- `internal/workflow` — the CAS-guarded runner and the durable seed path (`pending_seed`, `waitForContinueGate`, `sendPendingSeed`). Slice 4 **reads** run rows and renders `seed pending` / `seed FAILED` with the workflow view's existing honesty; it never advances a run.
- `internal/memory` — per-session `ask`/`outcome`/`files` for the node inspector body. No new extraction.
- `SessionDiff`, now sectioned per repo (slice 1 §8) — "review this child's artifact" straight from a node.
- `OpenInEditor` / `OpenURL` — every link target in every rendered document.
- Blush primitives: `.modal`, `.po-*`, `.sh-btn`, `.tact`, `.po-warn`, `.diff-*`, `statusColor()`, `statusWord()`, `esc()`, `bound()`.

**New:**

- `internal/arch/` — `docs.go` (discovery + containment + front-matter), `md.go` (markdown → safe token tree), `mermaid.go` (subset parser), `graph.go` (manifest decode + validation + analysis), `layout.go` (ranking, ordering, coordinates).
- `cmd/loom-gui/orchestration.go` — `ProjectDocuments(root)`, `ProjectDocument(path)`, `OrchestrationSnapshot(root, sinceRev)`; DTOs only, `defer recover()` on the poll path per house style. **All three open with the §3.1 visibility gate before any disk access** — that is the whole of hiding on this surface.
- `cmd/loom-gui/frontend/`: `graph.js` (SVG paint + patch + viewport), `doc.js` (token-tree → DOM), and one appended `tokens.css` block.

**Schema: no migration.** Slice 4 adds no tables and no columns. Every rendered fact comes from files on disk, existing rows, or the live snapshot. Whatever run/manifest persistence is needed belongs to slice 3.

---

## 11. Staging — this is the largest UI effort in the arc

Four independently shippable stages. **4a depends on nothing from slices 2–3 and can be built today.**

- **4a — Overview stops being a shell.** Document discovery + containment + markdown renderer + outline pane + decision cards. Mermaid fences render as source. No graph, no run strip. *Delivers "display all the architectural outlines and decisions within the platform" on its own.*
- **4b — The graph, static-but-live.** Manifest decode + validation + layout + SVG paint + node inspector (brief, authorization scope, gate result, diff, memory outcome) + per-poll status patching + the whole of §9. *Delivers the picture.*
- **4c — The analysis layer.** Blocked-on-you strip, wait-edge lighting, bottleneck, longest remaining chain, park-edge and cohesion figures in the run strip. *This is where "at a glance" is actually delivered; 4b without 4c is a diagram, not an instrument.*
- **4d — Mermaid subset rendering.** Parser onto the 4b layout engine.

Recommended commitment: **4a + 4b as the minimum bar, 4c as the point of the slice, 4d last** — 4d is the only stage whose absence has a graceful, permanent fallback (source blocks), so it is the correct thing to cut under pressure.

---

## 12. Testing (binding)

**Go — `internal/arch`:**

- *Layout determinism*: the same manifest yields byte-identical coordinates across 100 runs and across map-iteration reorderings of the input.
- *Ranking*: chains, diamonds, multiple roots, `park` edges excluded from rank, `gate` edges terminal.
- *Cycles*: 2-cycle, 3-cycle, cycle plus clean subgraph — layout returns, edges flagged, no hang, no panic.
- *Dangling edge* → ghost node + warning. *Duplicate node id*, *unknown kind*, *missing `nodes`* → structured violations with pointers, never a panic.
- *Analysis*: bottleneck with a tie (blocked-duration breaks it); bottleneck absent when nothing blocks; longest-remaining-chain excludes `integrated`; ready-set correctness.
- *Containment*: `../` escape, absolute escape, symlink-out-of-tree, out-of-root member repo (**must be admitted** — slice 1 allows out-of-root repos), sibling-prefix (`HappyPay/HappyPay` vs `HappyPayCoreApi`, the exact live shape slice 1 §9 pins), non-`.md`, oversize.
- *Markdown*: `<img src=x onerror=alert(1)>` renders as text; `javascript:` link inert; `file:`/relative link routes to editor; unterminated fence; table; nested list; 1 MB file.
- *Front-matter*: full block; missing `status` → `status unknown`; missing `date` → mtime, labelled; malformed block → body text + warning.
- *Mermaid*: one test per supported form; each unsupported diagram type → `Unsupported` with the type named; malformed-within-subset → error with the correct line number.

**Go — `cmd/loom-gui`:** `OrchestrationSnapshot` returns the full payload on rev change and the light payload otherwise; nil store / nil manifest / half-built engine each return an empty DTO, not a panic (existing `defer recover()` precedent); node → live status resolves **through claude session id**, proven by a fixture where the tmux name changed under a resume.

**Frontend:** status tick patches without re-layout (assert the graph host's node identities are the same objects); pan/zoom and an open inspector survive a status tick; a rev change preserves the viewport; every §9 row renders non-empty text (assert on rendered text, not on absence of exceptions); a 12-node fixture golden.

**Hiding (per slice 1 §6.3, one test per surface per frontend).** Orchestration snapshot, delegation graph, blocked-on-you strip and document reader join the GUI leak-surface list. These tests assert on **payload**, not on reachability — the route is deliberately open (§3.1), so a reachability assertion would pass while leaking:

- While the project is hidden, `OrchestrationSnapshot`, `ProjectDocuments` and `ProjectDocument` each return the empty `hidden:true` payload, asserted **field by field** on the DTO: no `rev`, no run title, no node list, no counts, no document titles or bodies, no paths, no error string.
- **Marshalled-JSON test (the strongest available form):** with a hidden project holding a full fixture run, no repo path, worktree path, brief path, node title, authorization-scope text, gate command, check tail or document byte appears anywhere in the serialized snapshot. Substring search over the marshalled bytes, not over a struct walk.
- **Round-trip:** unhiding returns a payload byte-identical to the visible case — slice 1's solo↔hidden restore discipline.
- Another project's solo suppresses this one identically; solo on *this* project does not suppress it.
- **Regression on slice 1's exception:** while hidden, the overview is still reachable, `ListProjectDetails` still returns the row, and Hide/Show plus Solo/Leave-solo still render and still function from it. A future "fix" that makes the overview unreachable must fail this test.
- The hidden line's rendered text is identical for a project with an active run and for one with none.
- Fail-closed: an unattributable or unknown root returns the hidden payload, not a populated one.
- Cross-project: a run with one node in a hidden repo renders the placeholder, the strip chip is a bare count, and the marshalled-JSON test above holds for that node's fields; a child session in a hidden project never appears in the blocked-on-you strip.

**§2 conformance tests — these encode the evidence constraints and must not be deleted:**

- A node whose session status is `needs_you` and whose `check.last` is null renders `awaiting check`, **never** a completion badge.
- A node with `check.last.ok == false` renders as human-blocking.
- No rendered surface contains orchestrator prose about a child (assert the DTO carries no such field — the strongest available form of this test).
- A `child` with an empty `worktree` renders the not-isolated warning.
- A `child` with an empty `authorization_scope` renders the no-scope warning.
- `park` edges are counted and rendered distinctly from `plan` edges.

---

## 13. Accepted limits

Read-only: Loom renders documents and manifests, never edits them, and never writes into the workspace. No historical scrubbing or timeline replay of a finished run (the obvious next want; it needs per-tick persistence slice 3 does not have). No manual node positioning. No TUI graph — an ASCII DAG is a second layout problem for a fraction of the value; the TUI keeps the existing run rows. Mermaid subset only. Longest chain is topological, not time-estimated (§5.2). One graph at a time. >200 nodes degrades to a table. Documents are per-project, not cross-project — an initiative spanning two *projects* has no single view, which is correct: slice 1 §2 binds an initiative to a subset of **one** project's repos. Chrome has no dark theme yet, so neither does the graph. No global blocked-on-you inbox (§5.5).

## 14. Disclosed failure modes

- **Resume greys the graph if identity is keyed wrong.** Every node lookup goes through `claude_session_id`. If slice 3 omits it, nodes go `unknown` on the first resume and the picture silently becomes useless. This is the single most likely way this slice fails in the field, which is why §12 tests it explicitly.
- **Status lags the truth by up to one poll, and by more under fusion.** A child that ended its turn while its pane still moves reads `running` (ARCHITECTURE §5). The graph is therefore *decoratively* late. The gate result is not, which is the second reason §2.1 puts completion on the gate.
- **A live-rewritten manifest reshuffles the picture.** The orchestrator will add park nodes mid-run. Mitigated by rev-diff, fade-in and the `graph changed` chip; not eliminated — a large plan revision genuinely re-draws.
- **Blocked-duration inflates across machine sleep.** Durations are wall-clock deltas over `updated_at` seconds; a laptop closed for two hours shows a two-hour park. Stated, not fixed.
- **Hiding has no route-level backstop on this surface, by design.** The overview is reachable while hidden and the user is *left standing on it* the instant they hide (§3.1). So every new server call added to the seam — now, or in any later slice — is one forgotten `ProjectVisible` check away from putting a client's repo paths and brief text on a shared screen, with no second gate to catch it. The mitigation is structural and stated so it survives: the check lives in the three named entry points, the leak-surface list in slice 1 §6.3 names them, and §12's marshalled-JSON test fails loudly rather than subtly. A fourth entry point added without the check will pass every existing test.
- **A hidden node still shapes the graph.** §3.1.4 keeps placeholder nodes in the topology so rank, ready-set and bottleneck stay true. That means the *structure* of a hidden project's work — how many nodes, how deep the chain, where the block is — remains visible even when identity is not. This is a deliberate trade against silently mis-drawing the graph, and it is within slice 1 §6.5's stated threat model (screen-share hygiene, not confidentiality). If it ever is not, the alternative is to suppress the whole graph when any node is hidden, and that decision belongs to whoever changes the threat model.
- **The manifest is agent-authored input on a render path.** §4.2 containment, the token-tree markdown renderer, the size caps and the scheme gate are the whole defence. A future field that takes a path must go through the same admission check; there is no second one.
- **The cohesion read is a proxy, not a measurement.** Edge density and repo-sharing correlate with inter-task cohesion; they do not measure it. Labelled as an indicator in the UI, and it is the cheapest available empirical read for slice 1 §11's open risk.
- **Two Loom instances** both render fine (layout is pure, the manifest is read-only here), but run selection is per-window local state, so two windows can show different runs of the same project. Acceptable; stated.

## 15. Rejected, and why

- **Vendoring mermaid.js** — §6: size against a dependency-light frontend, no structured parse error, and it solves none of the graph's actual problems.
- **Rendering the delegation graph *as* mermaid** — forfeits node identity, live patching and layout stability, which are the three properties §5 and §7 are built on.
- **A JS graph library (d3 / cytoscape / elk)** — same dependency cost as mermaid, plus layout moves out of Go and out of the repo's test mass.
- **A markdown or YAML library** — a four-key front-matter subset and a fixed token set do not justify a parser dependency and its raw-HTML default.
- **Pushing updates over Wails events or a file watcher** — the poll is the house model and the §7 concurrency guards are written against it; a second liveness path is a second set of races.
- **Storing rendered documents or layout in `loom.db`** — the file is the truth; a cache invites a stale render after a git checkout, which is precisely the "confident and wrong" outcome §12 of ARCHITECTURE forbids.
- **Fabricated time estimates on the critical path** — a confident wrong number on the glance surface.
- **An orchestrator-opinion panel** — slice 1 §11 measured reflection-style review as worse than nothing.
- **A global blocked-on-you inbox in this slice** — a new §6.3 leak surface with its own filter and per-frontend tests; deferred deliberately, not forgotten.
- **Auto-focus / auto-pan on status change** — steals the eye and breaks the one-earned-moment motion budget.
- **An in-app ADR/architecture editor** — Loom does not write the user's workspace, and that property is absolute.
