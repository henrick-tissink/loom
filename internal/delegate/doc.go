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
// In this package today:
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
// NOT in this package, and not to be added "because it is small": §§9-12 —
// integration worktrees, git merge run by Loom, cross-repo checks, the
// rendezvous/park-and-resume path, dynamic manifest amendments, the deadlock
// detector, the watchdogs, and any scheduling beyond "which tasks have no unmet
// edges" (Ready). In 3a the merge gate is a human reading the check result and
// running `git merge` themselves — Loom prints the command and does not execute
// it — and an unforeseen dependency is handled by the human attaching to the
// child and typing.
//
// The deferral is why several obvious fields are absent. delegation_runs.
// integration and delegation_tasks.spawn_snapshot exist in migration v12 and
// have no writer here; the `integration` block of the manifest JSON parses to
// nothing and is not validated; TaskState omits `integrating` and `mergeable`.
// A field nobody sets is worse than a field added when it becomes real.
//
// # Ownership of the files, for the wave that fills them in
//
//	state.go      vocabulary: TaskState, Flags, the shared sentinel errors
//	manifest.go   §4: types, LoadAll, validation, Graph, cycle detection, Ready
//	worktree.go   §6: git plumbing, meta dir, seed files, bootstrap, cleanup
//	check.go      §8: the subprocess runner and the published-artifact gate
//	spawn.go      §5.1 + §6.6 + §7: the gate preview, the brief, the launch
//	attribute.go  §14.1: the delegation-aware attribution wrapper
//
// state.go and manifest.go are one agent's scope — the graph types and the
// vocabulary are the seam every other file codes against, and splitting them
// across two authors is how two definitions of "ready" get written.
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
