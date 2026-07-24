// Package delegate is slice 3a of the orchestration arc (spec docs/superpowers/
// specs/2026-07-22-delegation-design.md): an agent-authored plan manifest over a
// subset of ONE project's repos, human-gated spawning of real `claude` children
// into git worktrees, and an executable definition of "done".
//
// The shape of the thing, in one paragraph. A manifest declares tasks; each task
// owns one repo, declares the paths it expects to touch, publishes named
// artifacts, and carries an executable check. Dependency edges are declared over
// ARTIFACT ids, never task ids, so the ready condition is a statement about a
// file that exists on disk and passed a check rather than about a peer's
// self-declared status (§4.2). A task the human approves gets a worktree, a
// brief written beside that worktree, and a child launched by the same
// session.Launcher every other Loom session uses. The child is done when LOOM
// runs the check against the child's COMMITTED artifacts and gets exit 0 — there
// is no message a child can send that means done (§8), because ~22.6% of
// validated misalignment episodes were inaccurate self-reporting.
//
// # The three channels (BINDING, §3)
//
// The orchestrator never reads a child's transcript. Reflection-style review
// measures worse than doing nothing (slice 1 §11). The only channels between a
// child and the rest of the system are: commits on its branch, the check's exit
// code and output, and a block declaration file. There is no prose review, no
// child-to-child message, and no shared scratchpad. A child's memory extraction
// is rendered for the HUMAN and is never an input to a state transition.
//
// # Why worktrees and not declared file ownership
//
// Slice 1 §11's controlled ablation: shared tree + declared ownership scored
// 55.5%, BELOW the 57.2% single-agent baseline; worktree isolation scored 63.3%.
// Declared ownership is not a weaker form of isolation, it is worse than not
// parallelizing at all. So a task's `paths` is a DETECTOR — it feeds the
// overlap warning at load and the divergence report at check time — and is never
// what keeps two children apart. The worktree is.
//
// # SCOPE: this is 3a, and the boundary is binding
//
// §2 of the spec is a schedule constraint, not a suggestion: the multi-agent
// benefit is conditioned on LOW inter-task cohesion, and a multi-repo
// re-architecture with declared handoff contracts is high-cohesion by
// construction. 3a exists to MEASURE that precondition on one real initiative
// before the expensive half is built.
//
// The 3a half, which was built first and has run:
//
//   - §4 the manifest — format, loader, validation, three-colour cycle
//     detection (manifest.go, graph and vocabulary in state.go)
//   - §6 worktree creation, naming, cleanup, the dead-child rule, seed files and
//     bootstrap (worktree.go)
//   - §8 the check contract and its runner, including §8.3's published-artifact
//     precondition (check.go)
//   - §5.1 approve-to-spawn and §6.6's concurrency cap, at 3 for 3a (spawn.go)
//   - §14.1 the attribution override, without which every child is invisible the
//     moment anything is hidden (attribute.go)
//
// §§9-12 are no longer deferred and are no longer declarations either: the
// bodies, the CAS discipline and the tests are all here. §9 is same-repo
// producer materialization and the dependency graph, §10 the integration
// worktree and the merges Loom itself runs, §11 the rendezvous/park-and-resume
// path and dynamic amendments, §12 the divergence comparator, the deadlock
// detector and the watchdogs:
//
//	graph.go       §9.1-9.3: EffectiveGraph, amendments, Ready, Progress, BasePlan
//	block.go       §11.1-11.3: the block declaration a child writes, its
//	               detector, and the amendment PROPOSALS a block produces
//	rendezvous.go  §11.4: pending seeds and their delivery to a live child
//	integrate.go   §10: the integration worktree, the merge sequence, cross-repo
//	               checks, and the reset path
//	snapshot.go    §12.3.3 + §10.5: spawn snapshots and the stale-contract alarm
//	watchdog.go    §12.1-12.2: the pure watchdog pass, §6.3's dead-child row, the
//	               spawn-orphan resolver and the deadlock diagnosis
//	run.go         §2 and the ORDER: the Runner that sequences all of the above,
//	               and the measurements §2's kill criterion is read off
//
// NOT in this package, and each absence is a decision rather than an oversight:
//
//   - Nothing SPAWNS on its own. Tick computes what is ready and stops; §5.1's
//     human gate is the only path to a child (§16). A scheduler that could start
//     work would make that sentence false, and this is the file that would have
//     to be edited to break it.
//   - No rendering and no IPC. Every DTO, every gate on a project root and every
//     button lives in cmd/loom-gui; this package returns facts and errors.
//   - No projects.Resolver. §14's hiding bit and the repo-label→path map arrive
//     as FUNCTIONS on Runner (Hidden, Repos) — §14.1's warning is that a
//     delegation child's cwd matches no project target, so a raw resolver fails
//     closed and hides every child the moment anything is hidden.
//   - The manifest's `integration` block is still decoded through IntegrationOf
//     rather than being a validated field of Manifest. §4.4 rule 7 has to apply
//     to it verbatim, and that means editing the frozen loader; it is a named
//     handoff in run.go.
//   - There is no flag-add/flag-clear CAS. Two Looms' watchdogs can lose each
//     other's flags through a read-modify-write; DecodeFlags' unknown-flag
//     tolerance degrades that to a lost BADGE rather than a corrupted set, and
//     FlagSeedPending — the one flag that is a durable CLAIM — goes through
//     SetTaskFlagsCAS for exactly that reason.
//
// # Ownership of the files
//
//	state.go      vocabulary: TaskState, Flags, the shared sentinel errors
//	manifest.go   §4: types, LoadAll, validation, Graph, cycle detection, Ready
//	worktree.go   §6 + §9.2's execution half: git plumbing, meta dir, seed
//	              files, bootstrap, cleanup, MergeProducers
//	check.go      §8: the subprocess runner and the published-artifact gate
//	spawn.go      §5.1 + §6.6 + §7: the gate preview, the brief, the launch
//	attribute.go  §14.1: the delegation-aware attribution wrapper
//	graph.go      §9: the effective graph, the amendment log's semantics
//	block.go      §11.1-11.3: block files, their detector, their proposals
//	rendezvous.go §11.4: the seed, its delivery, and the cross-repo relaunch
//	integrate.go  §10: integration worktrees and every merge Loom runs
//	snapshot.go   §12.3.3, §10.5: the out-of-worktree tripwire and contracts
//	watchdog.go   §12.1-12.2: the pure pass and the deadlock diagnosis
//	run.go        §2, and the ORDER every one of the above is applied in
//
// state.go and manifest.go are one agent's scope — the graph types and the
// vocabulary are the seam every other file codes against, and splitting them
// across two authors is how two definitions of "ready" get written.
//
// The one duplication to leave alone: ActiveChildren's switch (spawn.go) and
// TaskState.HoldsAChild (state.go) enumerate the same set on purpose, and
// state_test.go's TestActiveChildrenAgreesWithHoldsAChild is what catches the
// drift. It caught it already — `integrating` and `mergeable` reached
// HoldsAChild one wave before they reached ActiveChildren.
//
// # Position in the dependency graph
//
// delegate sits above session/store/projects/gitdiff and BESIDE workflow. It
// deliberately does not import internal/workflow: the shared seed-delivery rule
// is small enough that restating it with a citation beats a dependency, and this
// sentence exists so the duplication reads as a decision. ARCHITECTURE §3 fixes
// the direction downward — nothing here may import a frontend, which is why the
// diff primitive lives in internal/gitdiff rather than in cmd/loom-gui.
package delegate
