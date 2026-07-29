# Orchestrated Delegation — Design

**Date:** 2026-07-27
**Status:** Brainstormed and approved in dialogue; awaiting spec review.
**Arc:** Completes and re-surfaces loom's orchestration arc (specs of 2026-07-22). This is the slice that turns the orchestrator from a *planner that stops* into a *planner that dispatches*, and replaces the floating delegation graph with a native, legible control room.

**Goal:** From a plain-language intent, an orchestrator session decomposes the work, writes the seams (contracts) and their checks, and — on the human's approval — spawns child sessions that each do their slice with subagent-driven development. The whole tree is rendered natively and beautifully inside loom. A single session you spawn yourself stays a first-class, untouched path.

---

## 1. What we're building (the picture)

A **fractal** delegation, three levels deep:

- **Initiative** — the *orchestrator*, one Claude session that holds the intent and the plan.
- **Slices** — *child sessions*, one per decomposed piece, each a full Claude Code session.
- **Tasks** — the *subagents* each child dispatches internally via subagent-driven development.

loom renders the top two levels as real sessions. The third is internal to each child.

**Two doors, both first-class:**

1. **Launch a session** — exactly what exists today. One area, quick, direct. No plan, no orchestrator, no ceremony. This is the right tool for most work and is *not* touched by this design.
2. **Spawn an orchestrator** — chosen only when work genuinely decomposes. It plans, decomposes, coordinates.

A child session and a single session you launch are **the same primitive** — a briefed Claude session. The orchestrator is a *layer* that authors briefs and coordinates; remove it and you have today's launch. This is why the build is additive, not a second system.

## 2. The coordination model (the crux — BINDING)

Dependent slices are coordinated **without live conversation** — not child-to-child, not child-to-orchestrator-in-real-time. Live coordination is rejected on evidence and first principles: it couples two non-deterministic long-running processes at unpredictable moments (timing races, moving targets), it poisons a consumer with a producer's half-formed decisions, and a producer's "I'm done" is a *claim* (~1 in 5 wrong), not a fact.

Coordination happens three ways instead:

1. **Contracts, up front.** When the orchestrator decomposes, it writes the *seam* between dependent pieces — the interface both sides build against (e.g. `POST /export {docId} → application/pdf`). Both children are briefed with it. They then work fully in parallel against one written agreement. (Contracts already exist in loom as `kind:"contract"` documents referenced by tasks, with drift detected at merge.)
2. **The handoff is a finished, checked artifact — never a message.** Where a consumer needs a producer's output, it consumes the producer's *committed, check-passed file*, not its live reasoning. A fact, not a claim.
3. **Park-and-resume, for the unforeseen.** A child that hits a dependency the plan missed writes a machine-readable *"blocked on X"* and stops at its prompt (costs nothing, keeps its context). When X materializes, the harness nudges it to resume. A *decision* it can't make escalates to the orchestrator, then to the human. (This is loom's existing rendezvous, §11 of the delegation spec.)

**The load-bearing corollary — "don't split what you can't contract":** the dependency question and the decomposition question are the same question. A good breakdown cuts at clean seams so cross-slice dependency is minimal and contractable. Where two pieces cannot be given a clean contract, they are **one child, not two**. This judgment is the orchestrator's single most important job — and the honest fallback: an intent that does not decompose becomes **one session**, not three fake pieces.

## 3. "Done" — rigor at the seams, light at the leaves (DECISION)

- A **check** is a piece's own definition of done, expressed as something runnable. "Done" means the check passes — never the child announcing it. When a check goes green, loom flips the artifact to real and wakes anyone parked on it. This is the whole engine.
- Rigor is required **only where something depends on a piece** (a seam/handoff). A **leaf** piece nobody waits on may declare **no check**; its "done" is the human's review-and-merge.
- **Change required:** loom's manifest currently makes `check` mandatory on *every* task ("a task without an executable check … cannot be part of a run"). This design **relaxes that**: a task whose produced artifacts appear in no other task's `needs` may omit its check; its done-state is the existing human merge gate. A task on a seam (its artifact is needed elsewhere) still **must** carry a check. Validation enforces exactly this asymmetry.

## 4. Roles (BINDING)

- **You — the director.** State intent; approve waves; make the calls the orchestrator escalates; merge finished leaves.
- **The orchestrator — the tech lead.** Decompose; write contracts + seam-checks; arbitrate blocks; re-plan when reality drifts. It stays **live but quiet** — present for escalations, never watching over shoulders, never reading a working child's transcript (reflection-review measured worse than nothing).
- **The children — the ICs.** Each heads-down on its slice using **subagent-driven development**, building against its contract.

**Autonomy:** the human approves every spawn; the orchestrator never launches a session on its own (a measured failure mode). But the dependency graph *paces* this — only pieces whose inputs are ready can spawn, so the human approves **waves** (leaves first; the next wave becomes approvable as checks go green), never a wall of buttons. loom's existing per-task `ApproveTask` gate is reused; "approve all ready" is the ergonomic layer.

## 5. What already exists vs. what's new

**Exists (the delegate/orchestrator packages — reuse, do not rebuild):** the orchestrator-as-session (`SpawnOrchestrator`); the manifest format, loader, and cycle detection; worktree creation/isolation; human-gated spawn (`ApproveTask`/`Spawner`); the executable-check contract and runner; artifact-gated dependencies; contracts + drift detection; park-and-resume and dynamic amendments; the integration worktree + human merge gate; divergence + deadlock detection; the `OrchestrationSnapshot`/`ProjectDelegation` read path; and the `Start run…` wiring added 2026-07-27.

**New (this design):**
1. **Orchestrator authorship.** Its brief (`internal/orchestrator/assemble.go`) currently says *"Delegation does not exist yet — write the split into loom-open.md and stop."* That gate is removed and replaced with instructions to **decompose the intent into a manifest** at `.loom/manifests/<name>.json` — authoring contracts for the seams, a check per seam, and the honest single-session fallback. This is the pivot from planner-that-stops to planner-that-dispatches.
2. **Checkless leaves** (§3) — a manifest-validation relaxation.
3. **Children do SDD** — the child brief template (`delegate.Brief`) instructs subagent-driven development for the slice.
4. **The control room + rail nesting** (§6) — a UI redesign replacing the floating DAG-as-primary.

This is the *planned next slice of loom's own arc*, not a departure: it keeps the two safeguards the evidence (and our own deep-research) endorsed — human-gated spawns and check-based done — and adds only authorship and a legible surface.

## 6. The look

Native to loom's DNA — a rail of sessions — not a diagram bolted onto a page.

**Surface 1 — the rail.** The orchestrator appears as a session marked with a woven-knot glyph; its children hang beneath on hairline threads, each a normal session row with a live status dot. Click a child → its terminal; click the orchestrator → the control room.

**Surface 2 — the orchestrator's stage (the control room).** Replaces the unreadable DAG with a clean **vertical, dependency-ordered plan**: each piece with its state, its contract (the seam), and what it waits on — **"needs you" pinned to the top**, because the page's job is to tell you where to act at a glance. Approve-wave and merge actions are inline. The pretty node-graph survives as an optional **map** view; the list is primary, because legible beats impressive.

## 7. Polish (craft, in Blush)

- **Rail:** a roll-up dot so a collapsed orchestrator still shows *"1 needs you · 2 working · 1 done"* (loom's dot-survives-collapse rule). **Parked is calm** — muted and still, never red; loud color is reserved for *you are the blocker*.
- **Control room:** "needs you" is an honest hero panel — present → verb buttons; empty → the reassuring *"3 in flight · nothing needs you"*. **Every "done" shows its receipt** (`go test … green 8s ago`) — the UI cannot render a green it cannot prove. A **failing check is a doorway** — its output tail and the owning child one click away. The **"thinking" state** names the gap between intent and plan; no empty is ever blank.
- **Two earned moments of motion** (reduced-motion gated): the **settle** as a check goes green; and the **release** — the signature moment — a pulse traveling the thread from a just-finished piece to the sibling it unblocks, which lights up ready. loom's "wait-edge lights," inverted into a hand-off completing. Everything else is still.
- **State vocabulary** — one icon, one word, one meaning: `planned · ready · working · needs you · parked · done · check failed · abandoned`.
- **Words carry trust:** contracts read as human sentences (JSON stays under the hood); buttons say exactly what happens; each child shows its **worktree path** (isolation visible, not merely claimed).
- **The weave** — the one place loom's name earns a metaphor: connectors are threads, the orchestrator's glyph a knot, the release-pulse a thread pulling taut. Spent once, never sprinkled.
- **Two doors, unmistakable:** everyday **Launch** unchanged; **Orchestrate** a distinct, equal affordance; the knot marks orchestrated work everywhere.

## 8. Proposed slices (for the implementation plan)

1. **Authorship** — rewrite the orchestrator brief to decompose an intent into a manifest (contracts + seam-checks) at `.loom/manifests/`, with the honest single-session fallback. Backend/prompt; testable via a golden brief + a manifest the loader accepts.
2. **Rigor-at-seams** — relax manifest validation to allow checkless leaf tasks (seam tasks still required); child brief instructs SDD. Backend; unit-tested at the loader.
3. **Control room** — redesign the orchestration seam into the needs-you-first vertical plan (states, receipts, contracts, approve-waves, thinking state); demote the DAG to an optional map. Frontend.
4. **Rail nesting** — orchestrator + children as nested rows with the roll-up dot, collapse, and calm parked state. Frontend.
5. **Motion + weave + two-door polish.** Frontend polish.

## 9. Testing

- Loader/validation: checkless-leaf accepted, checkless-seam rejected (Go unit).
- Brief golden tests: the orchestrator brief instructs manifest authoring and the single-session fallback; the child brief instructs SDD.
- Bridge/DTO tests already cover `StartDelegationRun`/`ValidateManifests`/`OrchestrationSnapshot`.
- Frontend has no unit harness; each surface is verified by building and driving a hand-authored 2–3 task manifest through the real app, plus the never-blank state matrix.

## 10. Non-goals / deferred

- **No autonomous spawning.** The human gate stays.
- **No child-to-child or live child↔orchestrator messaging.** Coordination is contracts + checked artifacts + park/resume only.
- **No orchestrator review of child transcripts.** Evidence-forbidden.
- **Containers** remain reserved-but-unbuilt (`isolation` stays "worktree").
- The optional node-graph **map** view is retained but not itself redesigned beyond demotion to secondary.
