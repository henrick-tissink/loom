# Orchestrated Delegation — Design

**Date:** 2026-07-27
**Status:** Revision 3 — adds the autonomy model (§11): after the human confirms the plan in the orchestrator's console, loom executes the validated manifest end-to-end (spawn → check → unblock → integrate → **merge**) with no per-step approval, bounded and abortable. Revision 2 hardened the design after a four-agent adversarial review (premise, codebase, correctness, UX); those findings remain folded below.
**Arc:** Completes and re-surfaces loom's orchestration arc (specs of 2026-07-22): the slice that turns the orchestrator from a *planner that stops* into a *planner that dispatches* — but only for the case where the evidence supports it, and only after we prove it wins.

**Goal:** From a plain-language intent, an orchestrator session decomposes genuinely-multi-repo work, writes the seams and their conformance tests, and — on human approval — spawns child sessions that each do their slice with subagent-driven development. A single session you launch yourself stays first-class and untouched.

---

## 0. What the review changed (read this first)

The adversarial review validated the coordination **core** (contracts + checked artifacts + park/resume + human-gated spawn; CAS-enforced caps, cycle detection, no auto-kill) and found four things that reshape this spec:

1. **The premise is unproven for coding-with-dependent-state.** A single strong session running subagent-driven development already delivers safe decomposition *and* intra-task parallelism inside one coherent context. The only thing parallel *sibling* sessions add is **true wall-clock parallelism across repos**. So the orchestrator is scoped to genuinely multi-repo work, and **the beautiful UI is gated behind proving orchestrated delegation beats one session + SDD** (§8).
2. **The trust anchor was self-graded.** "Done = a check passes" rested on a check the *same agent* wrote (`["true"]` validates). A seam's conformance is now a **consumer-driven contract test** — authored by the consumer, run against the producer's real artifact at integration, a hard merge block (§3).
3. **Three factual errors about the code** are corrected (§5): checkless leaves are a state-machine change (a new human-review terminal), not a loader tweak; authoring the manifest is a deliberate rewrite of the brief's safety-load-bearing write-scope; "contracts" are fingerprinted `interface` artifacts, not a `kind:"contract"` object.
4. **The day-one safety net was missing** and is now first-class (§7): plan review before commit, abandon-run, real batch wave-approve, a manifest-repair loop, a defined single-session fallback, stuck-child detection + stop, and a real token/cost budget.

## 1. What we're building, and where it's the right tool

A **fractal** delegation, three levels: *initiative* (orchestrator) → *slices* (child sessions) → *tasks* (each child's SDD subagents). loom renders the top two as sessions.

**Two doors, both first-class:**
1. **Launch a session** — today's path, untouched. The right tool for *most* work.
2. **Orchestrate** — chosen for **genuinely multi-repo work where one session cannot hold both worktrees at once.** This is the honest niche: within a single repo, one session + SDD already decomposes safely inside a coherent context, so the orchestrator earns its cost only when true cross-repo wall-clock parallelism is the win.

A child session and a launched session are the **same primitive** (a briefed Claude session); the orchestrator is a *layer* that authors briefs and coordinates. The build is additive.

**The null hypothesis we are testing (BINDING):** *one strong session running subagent-driven development.* Orchestrated delegation must be shown to beat it on real intents before its dedicated UI is built (§8).

## 2. The coordination model (the crux — BINDING)

Dependent slices coordinate **without live conversation** — not child-to-child, not child↔orchestrator in real time. Live coordination couples two non-deterministic processes at unpredictable moments (timing races, moving targets), poisons a consumer with a producer's half-formed decisions, and turns "done" into a claim. Instead:

1. **Contracts, up front.** When the orchestrator decomposes, it writes the *seam* between dependent slices as human-readable prose in both briefs, backed by a first-class **`interface` artifact with a fingerprint** (loom's real mechanism — there is no `kind:"contract"` object; the "contract" is the agreed prose plus the fingerprinted interface). Both children build against it in parallel.
2. **The handoff is a finished, committed, conformance-tested artifact — never a message.** A consumer consumes the producer's committed file, gated as in §3.
3. **Park-and-resume for the unforeseen.** A child hitting an unplanned dependency writes a machine-readable *"blocked on X"* and stops (costs nothing, keeps context); the harness resumes it when X materializes. A *decision* escalates to the orchestrator, then the human.

**Honest limits (folded from review):** the fingerprint drift-detector (`Integrator.StaleContract`) catches only a producer *revising* an interface after a consumer spawned — never one wrong from birth, and nothing verifies the two children actually agreed. That gap is closed by §3's consumer-driven test, not by the fingerprint. And the load-bearing corollary stands: **the dependency question and the decomposition question are the same** — where two pieces can't be given a clean contract, they are **one child, not two**; an intent that doesn't decompose becomes **one session** (§7 defines that transition).

## 3. "Done" — consumer-driven seams, human-review leaves (DECISION)

- A **seam** (a slice whose artifact another slice `needs`) is done when a **consumer-driven contract test passes at integration.** The *consumer* authors the test from the agreed contract (the party that depends on the interface writes its conformance test — the Pact / consumer-driven-contract pattern); it runs against the producer's **real committed artifact** at integration and is a **hard merge block.** The producer's own check still gates its isolated correctness, but it is *never* the conformance gate. **Degenerate checks are rejected** at load (no-op / `true` / `false` / a check that references none of the task's produced paths).
- A **leaf** (nobody `needs` its artifact) may carry **no check**; its "done" is a new **human-review terminal state** — the human reads the diff and merges. This is a real state-machine addition (`running → awaiting-review → merged`), *not* a loader relaxation and *not* a fake `["true"]` check (which §3's degenerate-rejection would forbid anyway).
- **Amendment guard:** if any edge (declared or a runtime `AmendEdge`) targets a leaf's artifact, that leaf is *promoted to a seam* and must acquire a consumer test before its consumer can be approved — otherwise a checkless producer would leave the consumer parked forever.

## 4. Roles & autonomy (BINDING)

- **You — director.** State intent; **review the plan before it runs**; approve waves; make escalated calls; merge leaves; **abandon a bad run.**
- **Orchestrator — tech lead.** Decompose (multi-repo); write contracts + seam wiring; arbitrate blocks; re-plan on drift. Live but quiet; never reads a working child's transcript.
- **Children — ICs.** Each heads-down on its slice via subagent-driven development; a consumer additionally authors its seam's conformance test.

**Autonomy:** the human approves every spawn (never the agent). The dependency graph paces this into **waves** (leaves first; the next wave unlocks as conformance tests go green) — and wave-approval is a **real batch action** (§7), not the per-task modal repeated.

## 5. Exists vs. new — corrected against the code

**Exists (reuse):** orchestrator-as-session; manifest format/loader/cycle-detection; worktrees; human-gated `ApproveTask`/`Spawner` (CAS-enforced concurrency cap, default 4, clamp [1,10]); the check runner; artifact-gated deps; `interface` fingerprint + `StaleContract` drift; park/resume + amendments; the integration worktree + human merge gate; divergence/deadlock detection; `OrchestrationSnapshot`/`ProjectDelegation`; the `Start run…` wiring (2026-07-27).

**New — corrected:**
1. **Orchestrator authorship = a deliberate write-scope rewrite, not a deleted line.** The brief's scope section (`assemble.go`, the never-truncated safety text) currently forbids all repo writes and says *"delegation does not exist yet."* Authoring the manifest requires **explicitly widening write access to `<repo>/.loom/manifests/` only**, plus decomposition instructions, contract-authoring, and the single-session fallback. The sibling invariant *"you may not start/resume/kill sessions"* is **kept** (the human still gates every spawn). A **re-author loop** is required: a manifest that fails to load re-seeds the orchestrator with the exact `LoadError`. The schema is taught via an `--add-dir` reference doc, not inlined (the brief's `whatSection` is ~4 KB).
2. **Consumer-driven seam checks + degenerate-check rejection + a human-review terminal for leaves** — a run-engine change (`state.go`/`run.go`/`integrate.go`), not a loader relaxation.
3. **Children do SDD** — child-brief template addition.
4. **The safety net** (§7) — several small backend/GUI additions, most of which have unused primitives already present (`Budget.ActionStopSpawns`, per-task discard to generalize).

## 6. What the review confirmed we should NOT change

Kept as-is on the evidence: human-gated spawns; no child-to-child or live child↔orchestrator messaging; no orchestrator review of child transcripts; worktree isolation; the CAS concurrency cap; no auto-resolve of merge conflicts.

## 7. The safety net (first-class, Phase A) and the look (Phase B)

**Safety net — the day-one experience (Phase A, on the existing plain surface):**
- **Plan review before commit** — Start Run opens a read-only rendering of the slices + contracts (states `planned`), so "does this decomposition make sense" is answerable *before* the run row, worktrees, or spawns exist.
- **Abandon-run** — a run-level action marking every unstarted task abandoned and hiding the run (distinct from per-task discard).
- **Batch wave-approve** — one "approve ready (N)" action (a real endpoint or a client loop with visible per-item progress), not N modals.
- **Manifest-repair loop** — a validation failure offers "reopen the orchestrator with this error," not a raw schema string in a dismissable modal.
- **Defined single-session fallback** — when the orchestrator concludes the intent doesn't decompose, its row **collapses into a single plain session** with a one-line receipt ("doesn't decompose — continuing as one session"). The two doors visibly merge; no silent dead end.
- **Stuck-child detection + stop** — a stall heuristic (no commit/output past a threshold) promotes a `working` row to a distinct `stalled` state; a real "stop this child" action (not only destructive worktree discard). Per-edge "your producer died" fires the instant a producer goes terminal, not on whole-run quiesce.
- **Token/cost budget** — wire real cumulative token/cost accounting into the existing `Budget` (currently `MaxChildren`/`MaxWall`, both zero=unlimited) with `ActionStopSpawns`; show projected per-run spend at approve time (the multiplier is 1 + up to 10 children + each child's SDD fan-out).
- **Instrumentation (the point of Phase A):** count Orchestrate-chosen, collapse-to-single, contracts-authored-vs-amended, and run an explicit **bakeoff** vs one session + SDD on the same real intents.

**The look (Phase B — only after Phase A shows a win):** nested session rows in the rail; the control-room stage (vertical, dependency-ordered, needs-you-first) that demotes the DAG to an optional map; the two earned motions (settle, release-pulse) and the weave. Full detail retained from Revision 1; deferred behind the proof.

## 8. Phasing — prove it, then make it beautiful (BINDING)

**Phase A — Prove it.** Authorship (§5.1) · consumer-driven seams + human-review leaves (§3) · the full safety net + instrumentation (§7). Surface: the *existing* plain delegation UI, improved only enough for the safety net. Exit criterion: on real multi-repo intents, orchestrated delegation demonstrably beats one session + SDD by enough to clear the token/latency multiplier — **and** Orchestrate is chosen (not collapsed-to-single) often enough to be worth a dedicated surface.

**Phase B — Make it beautiful.** The control room, rail nesting, and motion/weave — built only if Phase A clears its bar.

## 9. Testing

- Loader: degenerate seam-check rejected; a leaf with no check accepted and routed to the review terminal (not left in `running`); an `AmendEdge` onto a checkless leaf promotes it and blocks its consumer until a consumer test exists.
- Engine: a leaf reaches `merged` via the review terminal with no check; a seam blocks merge until the consumer conformance test is green against the producer's real artifact.
- Brief golden tests: the scope section widens write access to `.loom/manifests/` only and keeps the no-spawn invariant; a failed load re-seeds the orchestrator with the error; the single-session fallback is instructed.
- Budget: cumulative-token cap triggers `ActionStopSpawns`.
- Bridge/DTO: `StartDelegationRun`/`ValidateManifests`/`OrchestrationSnapshot` (exist); add abandon-run and batch-approve.
- Frontend (Phase B): the never-blank state matrix + a hand-authored 2–3 slice manifest driven through the real app.

## 10. Non-goals / open bets

- **No autonomous spawning; no child-to-child or live messaging; no transcript review; no container isolation.**
- **Honest residual risk (not eliminated, mitigated):** a consumer-driven seam test is still LLM-authored — it removes the producer's self-grading incentive and adds an independent conformance gate, but the ultimate backstop remains the **human integration review at the seam.** Silent wrong integration is reduced, not proven impossible.
- **The premise is a bet under test, not a settled fact.** Phase A exists to measure it; a negative result kills Phase B, and that is a success of the process, not a failure.

## 11. Autonomy — confirm-in-console, then hands-off (Revision 3, DECISION)

The default supervised flow (human approves every wave; §4) stays available. Autonomy is a per-run mode chosen at start.

**The model — two phases, one gate, and the gate lives in the console:**

1. **Align (human + orchestrator, in the console).** The human converses with the orchestrator until they agree on the plan (the validated manifest). *That conversation is the review* — there is no separate GUI plan-review gate for an autonomous run.
2. **Go (in the console).** The human speaks an explicit **arm-phrase** (e.g. *"approved — run it"*); the orchestrator invokes loom (a CLI call, `loom run start --auto <manifest>` or equivalent) to start the autonomous run. **The orchestrator agent still never launches a session itself** — it invokes the harness, which does. The trigger is delivered through the console, but the spawner is loom.
3. **Run (loom, autonomous).** Loom's **auto-runner** drives the confirmed manifest end-to-end: spawn ready slices (up to the CAS concurrency cap) → run each check on committed work → unblock and spawn the next wave → integrate → **merge** — with no per-step human action.

**The trigger is in the agent's hands, so it is bound hard (BINDING):**
- It can **only** start a run for a manifest that already exists and has **validated** (`loom manifest validate` clean). It cannot invent work — only execute the confirmed plan.
- The concurrency cap and the token/cost **budget** bind every spawn exactly as under supervised approval; the trigger fires the plan, it does not unlock unbounded spawning.
- Every spawn, check, and merge is **visible** in the control room and the run is **abortable** — the human can pause or kill the whole run from loom at any moment. The console starts it; loom stays the kill switch.

**Full-auto-merge (DECISION).** loom merges a slice without a human gate — but only when every existing integration gate is green: the slice check passes, the integration worktree's per-repo check passes, the cross-repo checks pass, and git merges cleanly. A **conflict, a failing check, or a scope divergence does not merge** — it escalates and pauses. Every merge is an ordinary git commit (visible, `git revert`-able). This **supersedes §5's human merge gate for autonomous runs only**; supervised runs keep it.
- **Residual risk, stated plainly:** full-auto is the one place a green-but-semantically-wrong result lands with no human eyes. The guard is §3's **consumer-driven seam checks** — they are therefore the **top hardening item** for autonomy, not an optional later nicety. Until they exist, full-auto merges on the per-task + integration checks the manifest author wrote, backstopped only by revert.

**Escalation.** The auto-runner pauses and notifies on: a check failing past a retry ceiling, a deadlock, a park of kind needs-decision/needs-scope, a scope divergence, or the budget cap. **First cut escalates straight to the human;** routing through the orchestrator first (let it re-plan, then pull the human in) is a later refinement.

**New machinery (vs. what exists):** the **auto-runner** (a background loop per autonomous run that drives spawn → check → tick → integrate → merge, honoring cap + budget + abort), the **console trigger** (`loom run start --auto`, guarded to validated-manifest-only), and the **abort/pause** control. It wires together pieces that already exist (spawn, checks, tick, integration, park/resume) rather than inventing them.
