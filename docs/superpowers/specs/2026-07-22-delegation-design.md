# Loom — Delegation: the manifest, approve-to-spawn, isolation, integration, rendezvous — Design

**Status:** Revision 2 (hardened after adversarial review: 8 findings folded — 6 Critical / 2 Important; two of the criticals were the same §10.4 merge-source defect found from different angles)
**Date:** 2026-07-22
**Scope:** Slice 3 of 4 in the orchestration arc. An **orchestration run**: an agent-authored plan manifest over a subset of one project's repos, human-gated spawning of children into git worktrees, an executable definition of "done", a test-gated integration step, and a park-and-resume rendezvous for dependencies the plan did not foresee. NOT the orchestrator session or brief assembly UI (slice 2), NOT architecture/graph rendering (slice 4).

**This slice is the one the evidence constrains most.** Slice 1 §11 is binding input, not background. Where this document contradicts it, this document is wrong. §2 exists because the same evidence says the payoff is conditioned on a precondition this use case probably violates — so the first deliverable is a measurement, not a feature.

---

## 1. What this delivers

A **run** takes a manifest — `tasks`, each owning a repo and declared paths, each producing named artifacts, each with an executable check, wired by artifact-level dependency edges — and turns it into gated, isolated, verified work:

1. Loom validates the manifest the way `workflow.LoadAll` validates definitions, **including cycle detection**, because a dependency cycle is a silent deadlock that looks like healthy progress (§4.4).
2. Each ready task becomes an **approve** action. The human presses it. A worktree is created, a brief is written into it, a real `claude` starts in it (§5, §6).
3. A child is **done** when Loom — not the child — runs the task's check against the child's *committed* artifacts and gets exit 0 (§8). There is no message a child can send that means done.
4. Green work is offered at a second gate, **approve-to-merge**, after Loom has merged it into a per-repo integration worktree and re-run the check there (§10). The integration worktree is a **test bed and never the thing that ships**: what the gate merges into the user's branch is the task's own branch, so the diff shown is the diff applied (§10.4). This is the load-bearing gate.
5. A child that hits an unforeseen dependency writes a machine-readable block file and **stops at its prompt** — costing nothing, retaining its context — and Loom resumes it through the existing `pending_seed` / `waitForContinueGate` / `sendPendingSeed` path (§11).
6. Every state transition is a compare-and-swap, because two Loom instances against one DB is a supported state (§13).

---

## 2. Cheap validation first — slice 3a, and the kill criterion (BINDING)

Slice 1 §11 records the open risk plainly: **the multi-agent benefit is conditioned on LOW inter-task cohesion, and a multi-repo re-architecture with declared handoff contracts is high-cohesion by construction.** Building §§4–12 in full before testing that precondition is the most expensive mistake available in this arc.

**Slice 3a is built and run on one real initiative before anything else in this document is written.** It is deliberately not a product.

**In 3a:** the manifest format, its loader, its validation and cycle detection (§4) — pure, no DB, no UI, exercised by tests and a GUI-side "validate" affordance. Worktree creation and cleanup (§6). The check contract and its runner (§8). Spawn from an approved task, concurrency cap **3**. The published-artifact rule (§8.3). The `internal/gitdiff` extraction and its base-relative mode, plus divergence reporting over declared `paths` (§12.3.1–2 — §12.3.3's spawn snapshot is not in 3a). §14.1's attribution override, because without it 3a's children are invisible the moment anything is hidden.

**Not in 3a:** integration worktrees, `git merge` run by Loom, cross-repo checks, rendezvous, dynamic amendments, the deadlock detector, watchdogs, the scheduler beyond "which tasks have no unmet edges", any GUI beyond a list of tasks with state chips. The merge gate in 3a is a human reading the check result and running `git merge` themselves; Loom prints the command and does not execute it. An unforeseen dependency in 3a is handled by the human attaching to the child and typing.

**What 3a measures** (recorded per initiative, in the spec's follow-up, not in the DB):

| # | Measurement | Why it is the one that matters |
|---|---|---|
| M1 | Manifest authoring wall-clock vs. honest solo estimate for the same work | If planning costs more than doing, there is nothing to amortize |
| M2 | Fraction of tasks whose check was green the first time it ran after the child stopped | The real yield of an isolated child |
| M3 | **Count of unforeseen cross-task dependencies encountered per task** | This *is* inter-task cohesion, measured directly |
| M4 | Checks that failed for environment reasons (missing gitignored files, ports, DB state) | §6.4's non-isolation, quantified |
| M5 | Merge conflicts between sibling tasks in the same repo | Whether path ownership is real or aspirational |

**Decision rule (binding).** Build §§9–12 in full only if, on at least one real initiative of ≥4 tasks: **M3 ≤ 1 per 4 tasks** and **M2 ≥ 0.5**.

- If **M3 is high**, the correct product is not parallel children. It is the workflow chain Loom already has (`internal/workflow`), and slice 3 is re-scoped to **worktree isolation + an executable done-check + a test-gated merge gate for a SINGLE delegated child**. That subset has independent value at any cohesion level, is roughly a third of the work, and is the honest salvage path. Say so now so that outcome reads as a result rather than a failure.
- If **M1 is bad but M3 is fine**, the bottleneck is manifest authoring, and slice 2's brief-assembly work is what should be funded next — not §§9–12.
- If **M4 dominates**, §6.5's bootstrap/seed-files mechanism is the whole product, and it is worth building alone.

**No part of §§9–12 is started before 3a has run on real work.** This is a schedule constraint, and it is binding.

---

## 3. Model and vocabulary

- **Run** — one orchestration attempt: a manifest snapshot, a set of tasks, a pinned base commit per in-scope repo, a status. Slice 1 §2 binds the containment rule: **a run is scoped to a subset of exactly one project's repos.** An "Atlas re-architecture across bankenstein/ballista/v-atlas" is a run, never a project. Cross-project runs are rejected at load (§4.4) — under exclusive membership, a task set spanning two projects means the project boundary is wrong, and the fix is to fix the projects.
- **Task** — one unit of delegated work: one repo, one worktree, one branch, one child session, one check, zero or more produced artifacts.
- **Artifact** — a named, path-addressed, **committed** file (or fingerprintable interface) that a task publishes. Artifacts are the only currency between tasks.
- **Child** — a real `claude` in tmux, launched by `session.Launcher` like every other Loom session. Nothing about a child is special to the rest of Loom; it has a store row, a transcript, a memory index entry, a `SessionDiff`.
- **Orchestrator** — whoever authored the manifest. Slice 2 will make this a first-class session. **Slice 3 does not depend on slice 2**: the loader does not care who wrote the file, and a human writing it by hand is a supported and expected path (it is the 3a path).

**BINDING — the orchestrator never reads a child's transcript.** Slice 1 §11: reflection-style review measures worse than doing nothing. The channels between a child and the rest of the system are exactly three: **commits on its branch**, **the check's exit code and output**, and **a block declaration file**. There is no prose review, no orchestrator summary of a child, no child-to-child message, and no shared scratchpad. A child's `memory` extraction (ask/outcome/files) is rendered for the *human* and is explicitly never an input to any state transition — it is a self-report.

---

## 4. The manifest

### 4.1 Location and ownership

The manifest lives at **`<project_root>/.loom/manifests/<slug>.json`** — inside the user's workspace, authored by an agent, read by Loom **read-only**.

Rejected: `~/.loom/manifests/`, mirroring `~/.loom/workflows/`. It would require granting the authoring agent `--add-dir ~/.loom`, i.e. write access to `loom.db` and to every other run's state, to an agent whose input is untrusted by construction. The workflows precedent is right for hand-edited user files and wrong for agent-authored ones. Loom reads the manifest and never writes it.

**Correction to slice 1's "Loom never writes into your workspace" (BINDING disclosure).** Revision 1 asserted the property survives this slice intact. It does not, and the honest restatement is §6.2's: **Loom writes no tracked content into your repos, and Loom writes git metadata and refs into them.** `git worktree add` unavoidably creates `.git/worktrees/<id>/` in the repo's common dir and a `refs/heads/loom/**` ref; `§10.4`'s merge writes a commit to the branch you have checked out, which is the point of the gate. Revision 1 additionally specified a write that is *both* impossible and, in its only working form, a write into the user's repo — see §6.2 step 4, which is now fixed by keeping Loom's per-task files out of the tree entirely.

Loaded on view open, like `workflow.LoadAll` — no watcher. **Snapshotted into `manifest_json` at run creation**, exactly as `workflow_runs.def_json` is: a run replays its snapshot even if the file changes underneath it.

### 4.2 Format

```json
{
  "manifest": 1,
  "name": "atlas-rearchitecture",
  "project": "Innostream",
  "defaults": { "model": "sonnet", "mode": "acceptEdits", "check_timeout": "10m" },
  "repos": {
    "bankenstein": { "bootstrap": ["go", "mod", "download"], "seed_files": [".env.test"] },
    "ballista":    { "bootstrap": ["npm", "ci"], "seed_files": [".env.local"] }
  },
  "tasks": [
    {
      "id": "schema",
      "title": "Extract the account schema into a versioned migration",
      "repo": "bankenstein",
      "paths": ["db/migrations/**", "internal/account/schema.go"],
      "brief": "…what to do, in the author's words…",
      "authorization": "You may modify db/migrations and internal/account/schema.go in this worktree only. You may read every other file. You may NOT change the HTTP layer, the ballista client, or any test outside internal/account.",
      "needs": [],
      "produces": [
        { "id": "account-schema", "kind": "interface", "path": "db/migrations/0007_account.sql",
          "fingerprint": ["sha256sum", "db/migrations/0007_account.sql"] }
      ],
      "check": { "cmd": ["go", "test", "./internal/account/..."], "cwd": ".", "timeout": "10m" }
    },
    {
      "id": "auth-api",
      "repo": "bankenstein",
      "paths": ["internal/auth/**"],
      "brief": "…", "authorization": "…",
      "needs": ["account-schema"],
      "produces": [{ "id": "auth-openapi", "kind": "interface", "path": "api/auth.yaml",
                     "fingerprint": ["./scripts/openapi-hash", "api/auth.yaml"] }],
      "check": { "cmd": ["go", "test", "./internal/auth/..."] }
    },
    {
      "id": "ballista-client",
      "repo": "ballista",
      "paths": ["src/clients/auth/**"],
      "brief": "…", "authorization": "…",
      "needs": ["auth-openapi"],
      "produces": [],
      "check": { "cmd": ["npm", "run", "test:auth"] }
    }
  ],
  "integration": {
    "per_repo": { "bankenstein": { "cmd": ["go", "test", "./..."], "timeout": "20m" },
                  "ballista":    { "cmd": ["npm", "test"], "timeout": "20m" } },
    "cross": [ { "id": "auth-contract",
                 "cmd": ["./scripts/contract-test.sh"],
                 "repo": "ballista",
                 "needs_repos": ["bankenstein", "ballista"],
                 "timeout": "20m" } ]
  }
}
```

**Design notes on the shape:**

- **`needs` names ARTIFACT ids, not task ids.** The task graph is *derived* (`producer(artifact) → consumer`). This is deliberate: the ready condition is then a statement about a thing that exists on disk and passed a check (§9), not about a peer's self-declared status. A dependency you cannot name an artifact for is a dependency you have not specified, and the loader will make you say so.
- **`paths` is a detector, not the isolation mechanism.** Slice 1 §11: instruction-level file slices measured *below* the single-agent baseline. `paths` feeds (a) a static overlap warning at load, (b) the pre-merge divergence check in §12.3. It is never the thing keeping two children apart — the worktree is.
- **`authorization` is required and must be non-empty.** Removing explicit authorization-scope text measurably raises scope overreach, so its absence is a load *error*, not a defaulted field. Loom appends its own invariants (§7) but will not invent the task-specific half.
- **`check` is required on every task.** A task without an executable check has no definition of done and cannot be part of a run.
- **`repos[].bootstrap` / `seed_files`** exist because §6.4 is real: a fresh worktree has no `node_modules`, no `.env`, no `venv`.

### 4.3 The check object

```json
{ "cmd": ["go","test","./internal/auth/..."], "cwd": ".", "timeout": "10m", "env": {"CGO_ENABLED":"0"} }
```

`cmd` is an **argv array, never a shell string.** No shell, no word splitting, no interpolation. A manifest is agent-authored and therefore untrusted input; `sh -c` over it is a remote-code-execution channel with extra steps. (It is *already* arbitrary code execution — but as an explicit, reviewable, rendered argv the human approves at the spawn gate, which is the whole point of §5.)

### 4.4 Validation — `delegate.LoadAll(dir, resolver)`

Modeled directly on `workflow.loadOne`: every failure is a `LoadError` carrying the file path and a human-readable reason, never a panic, and a bad file never costs the user the other files. Ordered checks:

1. `manifest` version known (`1`). `name` == filename stem (workflows precedent).
2. `project` resolves through `internal/projects` to exactly one project root.
3. ≥1 task. Task `id`s unique, non-empty, `[a-z0-9-]{1,64}` (they become branch and directory components — see §6.2).
4. Every `repo` is a known repo label **belonging to the named project**. A repo from another project is an error naming both projects and pointing at §3's containment rule.
5. `model`/`mode` in the same known sets `workflow/def.go` uses. A `mode` of `bypassPermissions` is legal but flagged: it is rendered in red at the spawn gate with the task id.
6. Artifact `id`s globally unique across the manifest. Every `needs` entry names a declared artifact. **A task may not `need` an artifact it produces itself.**
7. Artifact `path` and check `cwd` must resolve **inside** the task's repo after `filepath.Clean` — no `..` escape, no absolute paths. Re-checked at execution time against the materialized worktree, because a manifest amendment (§11.3) is a second write path.
8. **Cycle detection** — §4.5.
9. `integration.per_repo` covers every repo any task names. Each `cross` entry's `needs_repos` ⊆ the run's repos, and its `repo` ∈ its `needs_repos`.
10. **Warnings** (non-fatal, rendered on the run): two tasks in the same repo with overlapping `paths` globs; a task with `produces: []` that some other task `needs` nothing from and that nothing needs (a leaf whose output no one integrates — legal, often correct, worth a glance); a check whose `cmd[0]` is not on `PATH`.

### 4.5 Cycle detection (BINDING)

A dependency cycle is a *silent* deadlock that presents as healthy progress: every task sits `pending`, no error is raised, and the run looks like it is waiting on work that is being done. It must be impossible to start such a run.

Detection is an **iterative three-colour DFS** over the derived task graph, not Kahn's algorithm. Kahn tells you a cycle exists; the three-colour DFS lets you recover the actual back-edge path, and the error message must name it:

```
manifest "atlas-rearchitecture": dependency cycle:
  auth-api → (auth-openapi) → ballista-client → (ballista-types) → schema → (account-schema) → auth-api
```

Iterative, not recursive, so a pathological manifest cannot blow the stack. Complexity O(V+E) over a graph that will never exceed a few dozen nodes; there is no reason to be clever.

**Cycle detection runs three times, on three different graphs, and this is the part that is easy to get wrong:**
- at load, over the on-disk manifest;
- at run creation, over the snapshot (identical, but the snapshot is the thing that will actually execute);
- **at every manifest amendment** (§11.3), over the amended graph. A rendezvous-discovered edge is exactly the kind of edge that closes a loop the author did not see, and accepting it unchecked converts a loud block into a silent deadlock. An amendment that introduces a cycle is **rejected** and escalated as a re-plan request.

`integration.order`, if a future revision adds one, is validated as a topological order of the same graph. Revision 1 derives the order instead (§10.2) and has no such field.

---

## 5. The two gates

Both exist. They are not the same kind of gate, and conflating them is how you end up with a system that asks the human eleven questions and gets no safety from any of them.

### 5.1 Approve-to-spawn — a budget and consent gate

Loom never launches a child on its own. A task reaching `ready` produces an **approve action** in the run view; pressing it is the only path to `spawning`. What the human is shown, and is deciding about:

- task id, title, repo, the **branch and worktree path** that will be created, the base commit;
- the full assembled brief (§7), scrollable, exactly as the child will receive it;
- the check argv, verbatim;
- `mode`, with `bypassPermissions` in red;
- the current running-child count against the cap (§6.6);
- the run's spend so far (child count, wall-clock).

**Batch-approve is allowed here** ("approve all 3 ready tasks"), because the decision is cheap and reversible: a bad spawn is undone by discarding a worktree. Nothing is lost but tokens, and the count of tokens is on the screen.

### 5.2 Approve-to-merge — the correctness gate, and the load-bearing one (BINDING)

Slice 1 §11: *"the primary human gate belongs at merge, not only at spawn."* At spawn the human is approving a plan they mostly already read. At merge they are approving a diff that a machine wrote and a check certified, into a tree they own. That is the moment where a wrong decision is expensive and where new information exists.

Offered only when **all** of the following hold — Loom refuses to render the action otherwise:
- the task's declared artifacts exist and are committed on its branch (§8.3);
- the task check is green on the child's branch;
- the merge into the repo's **integration worktree** succeeded without conflict, and the **per-repo integration check is green on the merged result** (§10);
- every `cross` check whose `needs_repos` are all satisfied is green.

What is shown: the sectioned diff (the `internal/gitdiff` primitive per §12.3, base→branch rather than `git diff HEAD`), the divergence report (files outside declared `paths`, listed, and if non-empty the approval requires a second explicit acknowledgement), the check output tails, and the exact `git` commands Loom will run.

**Never batchable, and the diff shown is the diff applied (BINDING).** One task, one decision, one diff. Revision 1 broke this in §10.4 by merging the *integration* branch — which is cumulative — into the user's branch while rendering only the approved task's `base→branch` diff. Approving task B would then have landed A+B, silently applying work whose own gate was never shown and whose divergence acknowledgement was never given, and would have mis-recorded a `forced` merge of B as covering an unforced A. The gate merges `loom/<run-slug>/<task-id>` and nothing else (§10.4). The integration worktree is a staging area that answers one question — *is this task green combined with its already-verified siblings?* — and is never a merge source.

**Disclosed cost of that choice:** the user's branch after a merge is not byte-identical to the tree the integration check was green on (it lacks siblings not yet approved, and carries whatever the user's branch had that the pinned base did not). The integration check is evidence about the combination, not a certificate about the user's tree. §10.4 re-derives the staging area from the user's branch head after each merge so the evidence tracks reality; nothing makes it a proof.

**Force-merge exists** — a red check must not make work unrecoverable — but it is a distinct, differently-worded, armed action (`killButton`'s armed-confirm idiom, slice 1 §6.4), it records `forced=1` and the failing check output in the task row permanently, and the run renders `merged (FORCED, check red)` forever after. A red merge you can explain is fine. A red merge you cannot later find is not.

---

## 6. Isolation — git worktrees

### 6.1 Why worktrees and not the alternatives

Slice 1 §11's ablation: shared tree + declared ownership **55.5%**, below the 57.2% single-agent baseline; worktree isolation **63.3%**. Declared ownership is not a weaker version of worktrees — it is worse than not parallelizing at all. This is the single most decision-shaping number in the arc.

Containers are the other option the evidence endorses and are **rejected for revision 1**: Loom is macOS-in-practice, the target repos have no container discipline, and a `claude` inside a container is a materially different product (auth, PATH hydration, editor ⌘-click, tmux attach all change). The manifest reserves `"isolation": "worktree" | "container"` at the run level, defaulted and currently validated to `worktree` only, so adding containers later is not a schema break.

### 6.2 Creation and naming

Deterministic from `(run, task)`, which is what makes crash recovery exact:

```
branch:   loom/<run-slug>/<task-id>
worktree: ~/.loom/worktrees/<run-slug>/<repo-label>/<task-id>
meta dir: ~/.loom/worktrees/<run-slug>/<repo-label>/<task-id>.meta/   -- brief.md, block.json
```

`<run-slug>` is `<manifest-name>-<run-id>`, so two runs of the same manifest never collide. Task ids are `[a-z0-9-]` (§4.4) precisely so they are safe as both a path and a git ref component.

**Under `~/.loom/`, not in the workspace.** A worktree inside the repo pollutes `git status`, `.gitignore`, editor indexers, and every glob in every check. A worktree beside the repo in `$LOOM_WORKSPACE` gets picked up by `registry.Discover` as sixteen new single-repo projects. `~/.loom/worktrees/` is Loom's own directory, which Loom already writes to, so **no tracked content** is written into the user's workspace — but `git worktree add` does write `.git/worktrees/<id>/` and a `refs/heads/loom/**` ref into the user's repo, and §4.1 now says so. The child is granted **only its worktree path** as cwd — it is not granted `~/.loom`, so `loom.db` is not in its authorization scope. The `.meta/` sibling directory **is** granted, as its own `--add-dir` — it is the one add-dir the child needs write access to (it writes `block.json` there), and per the spike an add-dir grants read+write silently with no second trust prompt, which is exactly what is wanted here. It contains only Loom's two files for this one task; `~/.loom` itself is never granted. (It is readable by anything with the user's uid; §6.5 of slice 1 already states Loom's confidentiality claims honestly. This is not a security boundary and is not claimed as one.)

Creation, in order, all idempotent:

1. `git -C <repo> rev-parse HEAD` → the **pinned base**, recorded per repo on the run at run-creation time, not per task. Every child of a run branches from the same commit, which is what makes §10's integration deterministic.
2. If the task has same-repo producers, the worktree is created at the run's pinned base and each producer branch is merged in, in a defined order — §9.2. (Revision 1 said "branch from the producer's branch head", singular, which is undefined for the normal two-producer shape.)
3. `git -C <repo> worktree add -b loom/<run>/<task> <path> <base>`. If the branch already exists (a re-spawn), `worktree add <path> <branch>` instead; if the path already exists and its branch matches, treat as success and reuse. Any other collision is a hard, loud refusal — never a silent overwrite. **Hard precondition, checked first:** no live `sessions` row may already have `cwd == physicalDir(<worktree>)` (§13.3). Two claudes in one worktree on one branch is the worst outcome in this design and is made structurally impossible here rather than argued about in recovery.
4. `mkdir <task-id>.meta` **beside** the worktree, write `brief.md` (§7) into it. **Nothing Loom owns is written inside the worktree.**

   Revision 1 wrote `<worktree>/.loom/` and excluded it via `<worktree>/.git/info/exclude`. That is impossible: in a linked worktree `<worktree>/.git` is a *file* containing `gitdir: …`, so the path is `not a directory` and the append fails outright. Verified empirically, along with the reason the obvious repair is worse: `info/` is a **common-dir** path, so writing `.git/worktrees/<id>/info/exclude` is *not honored* (`.loom/` stayed untracked-and-listed) while writing the main repo's `.git/info/exclude` **is** — i.e. the only working form writes into the user's own repository, affects the user's working tree and every other worktree of it, and accumulates one line per task per run forever.

   The per-worktree-config alternative (`git config --worktree core.excludesFile`, behind `extensions.worktreeConfig`) works but flips a repo-wide extension on the user's repo to solve a problem Loom created. Keeping the files outside the tree removes the exclusion problem completely, removes any chance of a child committing its own brief into the artifact set, and needs no git feature at all.
5. Copy `seed_files`, run `bootstrap` (§6.5).

`git worktree add` on a repo whose main tree is **dirty** is fine and is allowed at spawn. Dirtiness matters at merge (§10.3), where it is refused.

**Disclosed:** children branch from committed `HEAD`. Uncommitted work in the user's own tree is invisible to every child and will conflict at merge. The spawn gate warns when an in-scope repo is dirty, naming it.

### 6.3 Cleanup, and a worktree whose child died

| Event | Worktree dir | Branch |
|---|---|---|
| Merged (§10.4) | removed (`git worktree remove`) | **kept** |
| Discarded by the human | removed (`--force`) | **kept** |
| Child session died, work uncommitted | **kept, untouched** | kept |
| Run abandoned | kept until an explicit sweep | kept |
| Loom restarted mid-anything | kept; re-derived from `(run, task)` | kept |

The `<task-id>.meta/` directory follows the worktree in every row of this table except one: it is **kept** when the worktree is removed after a merge, because `block.json` and the final brief are the only durable record of what the child was told and why it stopped. It is a few kilobytes and the explicit sweep takes it.

**Branches are never deleted by Loom.** A branch is a few bytes and is the only durable record of a discarded attempt; deleting one is the single irreversible act available in this design and there is no reason to take it. A `git worktree prune` + branch-delete sweep is offered as an explicit, listing-first human action.

**A worktree whose child died is not garbage.** The session dying and the work being worthless are unrelated events, and Loom's first principle is that it is a window, never an owner. The task is flagged `orphaned` (a flag on `running`/`blocked`, not a state — §13.2) and the run renders it as recoverable. Recovery is a **re-spawn onto the same worktree**: if the dead session's `claude_session_id` is known, `session.Resume` with the worktree as cwd (identity-by-claude-id, the same primitive `workflow.ResolveStepSession` uses); if not, a fresh child with the same brief plus a rendered summary of the uncommitted diff already in the worktree. Loom never kills a child, ever — not on stall, not on timeout, not on budget (§12.2).

### 6.4 What worktrees do NOT isolate (BINDING disclosure)

This list is not caveats. It is the failure mode that will actually bite, and M4 in §2 exists to measure it.

- **Ports.** Two children running `npm run dev` or a test server on 3000 collide, and the second one's check fails for a reason that has nothing to do with its work.
- **Databases.** A shared local Postgres/Redis is shared. Migrations run by one child are seen by all of them, in whatever order they happen to run.
- **Caches and build state.** `~/.cache`, `~/.npm`, Go build cache, Gradle, Docker. Mostly benign, occasionally catastrophic (a poisoned cache entry fails every child at once).
- **Gitignored files do not come along.** A fresh worktree has no `.env`, no `node_modules`, no `venv`, no local config. **This is the most common cause of a check that fails for non-reasons**, and the reason `repos[].bootstrap` and `seed_files` exist (§6.5).
- **The git object store is shared.** A `git gc` in one worktree affects all of them. Branch deletion is global — another reason Loom never deletes branches.
- **Global tool state**: Docker containers, running dev servers, `~/.claude` itself, any daemon.
- **Test state that is not in the repo**: fixtures on absolute paths, seeded data, snapshots in a user-level directory.

Loom does not solve these. It **detects** the port class cheaply: a check that fails with a nonzero exit and whose captured output matches a small set of shapes (`EADDRINUSE`, `address already in use`, `connection refused`, `database is locked`) is flagged **`environment-suspect`** on the task row, rendered distinctly from a genuine failure, and **excluded from M2**. A heuristic label, explicitly, not a diagnosis — the point is that a human triaging ten red checks can tell "your code is wrong" from "your neighbour took port 3000" in one glance.

### 6.5 Bootstrap and seeded files

After worktree creation and before the child launches:

1. `seed_files` are copied from the repo's primary working tree into the worktree at the same relative path. Refused if the entry is not gitignored (copying a tracked file means it is either already there or you are hiding a modification), if it escapes the repo, or if it is a symlink. Each copy is listed at the spawn gate — the human is being shown that `.env` is about to be handed to an agent.
2. `bootstrap` runs as a check-shaped subprocess (§8, same argv/timeout/capture contract) with the worktree as cwd. **A failed bootstrap blocks the spawn**, loudly, with output — it is strictly cheaper to fail here than to spend a child's whole context discovering that `node_modules` is missing.

### 6.6 The concurrency cap (BINDING)

**Default 4. Configurable 1–10. Hard maximum 10.** 3a runs at 3.

The reasons, in order of how quickly they bite:
1. **Practitioner reports put the overhead crossover at roughly 8–10 concurrent worktrees** — disk, editor/index rebuild, and above all the review bandwidth of the one human who has to look at every diff at the §5.2 gate. Past that, the queue is the human, and adding children lengthens it.
2. **§6.4's shared resources collide superlinearly.** Ports and a shared DB are pairwise-conflicting; four children is six pairs, ten is forty-five.
3. **Each child is a real `claude`** with real quota. Ten of them run down a budget faster than a human can read one diff.
4. **Loom's own poll loop is O(sessions)** with a tmux probe per session every 1.5s (TUI), plus per-task branch-head and block-file polling here. This is the least important reason and is listed last on purpose; it is a fixable engineering cost, unlike 1–3.

The cap counts **running and blocked** children (a blocked child holds its worktree and its context), not tasks. Reaching it does not stop the scheduler — ready tasks still queue and still show their approve action, greyed with `cap reached (4/4)`.

---

## 7. The child brief

**The seed is a pointer, not the brief.** Loom writes the assembled brief to `<worktree-parent>/<task-id>.meta/brief.md` (§6.2 step 4 — outside the worktree) and seeds the child with the absolute path:

```
Read <abs>/<task-id>.meta/brief.md — it is your complete task brief. Follow it exactly, including the STOP protocol.
```

This is not a stylistic choice. `send-keys` has a measured argv ceiling of ~16.3KB (workflows spec §2.3), and a real brief with authorization text, artifact paths and a block protocol will exceed it. A file also survives context compaction, is re-readable by the child on demand, and is visible to the human beside the worktree it governs. Because it lives **outside** the worktree it cannot be committed as an artifact, cannot appear in the child's own diff, and needs no `.gitignore`/`info/exclude` mechanism at all (Revision 1's did not work — §6.2 step 4). The 15KB seed cap and `truncateBytes` from `internal/workflow` are reused for the pointer path anyway, unchanged.

`brief.md` sections, in order, all Loom-rendered from the manifest:

1. **Identity** — run, task id, repo, branch, worktree path, base commit.
2. **Authorization (verbatim from the manifest, plus Loom's invariants).** BINDING: this section is mandatory and non-empty (§4.4 rule 5). Loom appends, never replaces:
   - write only inside this worktree;
   - these additional directories are readable and **must not be written**: `<add-dirs>`;
   - do not `git merge`, `rebase`, `push`, `checkout` another branch, or touch another worktree;
   - do not modify these paths, which belong to sibling tasks in this repo: `<list>`;
   - do not spawn subagents that write outside this worktree.
3. **The task** — the manifest's `brief`, verbatim.
4. **Artifacts to publish** — exact paths, and the rule: *an artifact is published when it is committed on this branch. Uncommitted work does not exist.*
5. **Done** — *"You do not declare done. Loom runs `<check argv>` against your committed work. Commit when you believe it will pass; Loom will run it and tell you. Do not report completion in prose."* The check argv is given verbatim so the child can run it itself while working, which is encouraged.
6. **The STOP protocol** — §11.1, verbatim.

---

## 8. DONE — the executable check contract (BINDING)

Slice 1 §11: *a child's "done" is an executable check on a published artifact, never a message.* ~22.6% of validated misalignment episodes involved inaccurate self-reporting. This section is that constraint made literal.

### 8.1 Execution

Loom runs the check as a subprocess, out-of-band, in the child's worktree. Never inside the child's session, never as a slash command, never as anything the child could influence the reported result of.

- `exec.CommandContext` with `cmd[0]` looked up on the hydrated PATH; **argv array, no shell** (§4.3).
- `Dir` = `<worktree>/<check.cwd>`, re-validated to be inside the worktree at execution time.
- Env: the parent env with `CLAUDECODE` and `CLAUDE_CODE_*` scrubbed (the `memory.Summarizer` precedent — a check must not think it is inside a Claude session), plus `check.env`, plus `LOOM_TASK_ID`, `LOOM_RUN_ID`, `LOOM_WORKTREE`, and `LOOM_REPO_<LABEL>` for each repo in scope (§10.2).
- Timeout: `check.timeout`, default 10m, **hard max 30m**. Timeout is a **failure**, not an unknown. `cmd.WaitDelay` is set, for the reason the summarizer sets it: an orphaned grandchild otherwise wedges `Wait()` forever.
- Output: stdout+stderr interleaved, captured, capped at 256KB head + 256KB tail with a visible elision marker, stored on the task row and rendered.
- **Exit 0 = pass. Anything else = fail.** No parsing of output, no "looks like the tests passed", no heuristic. The one exception is §6.4's `environment-suspect` labelling, which changes how a *failure* is rendered and never turns one into a pass.

### 8.2 When it runs

Automatically when **both**: the task's branch head has moved since the last check, **and** the child's transcript state is `Idle` or `NeedsYou`. The second condition is `waitForContinueGate`'s existing rule, reused verbatim: `❯` is meaningless mid-generation, and running a test suite against a tree the child is halfway through writing produces noise. Debounced 10s; at most one check in flight per task; a manual "run check" action always available.

### 8.3 The published-artifact precondition

Before the check runs, Loom verifies for every declared artifact:

```
git -C <worktree> ls-files --error-unmatch -- <path>     # tracked
git -C <worktree> diff --quiet HEAD -- <path>            # committed, not just staged
```

A missing or uncommitted artifact **short-circuits the check as `unpublished`** with the specific paths named. This is what "published artifact" means operationally, and it is the whole reason the artifact list is in the manifest: it converts "did you finish?" into two git commands.

For `kind: "interface"` artifacts, the `fingerprint` argv is run at this moment and its stdout (trimmed, capped 4KB) recorded against the artifact and the producing commit. §10.5 uses this.

### 8.4 Flakes

A check that passes on the branch and fails at integration is **reported as two results, never averaged and never re-run until green.** There is no retry-on-failure. There is exactly one retry, on *infrastructure* error — `cmd[0]` not found, fork failure — which is not a non-zero exit and is recorded distinctly. Retrying a failing check until it passes is how a system launders a flake into a false certification, and this system's entire claim to trustworthiness is that the check result means something.

---

## 9. Scheduling — dependency-gated, primary

### 9.1 Ready

`delegate.Ready(manifest, states) []TaskID` is a **pure function**: no DB, no side effects, no spawning. It proposes; §5.1 disposes.

A task is `ready` when, for every artifact in `needs`:
- the producing task is `verified` (check green — §8), **and**
- the artifact exists at its declared path, committed, on the producer's branch.

Both conditions, not either. Producer-verified without the artifact means the check did not actually cover the handoff. Artifact-present without verification means an untested artifact.

A task with `needs: []` is ready as soon as the run is created.

### 9.2 Materializing a dependency

The consumer must be able to *see* what it depends on. Two cases:

- **Same repo — one or MANY producers (BINDING).** `needs` is a list of artifacts, so a task can have several same-repo producers (`api` needs both `schema` and `config` is the normal shape once a manifest has any width), and a worktree has exactly one base commit. Revision 1 said "branch from the producer's branch head", singular, which is undefined for that shape; the plausible implementation picks the first producer and silently hands the child a tree missing a declared dependency. The child then either blocks — and M3 records a *scheduler* failure as a *planning* failure, corrupting §2's binding kill criterion — or re-implements the missing piece outside its authorized paths.

  Defined instead: the worktree is created at the **run's pinned base**, and each same-repo producer branch is merged into it in **ascending producer task-id order** (deterministic, so a re-spawn reproduces the same tree byte-for-byte). One producer is the degenerate case and is still a merge, not a special path — a chain of length one is still fast-forward-shaped, so §10's integration merges stay trivial.

  **A conflict between two producers at spawn time is a hard stop**, not something the child absorbs: the task does not spawn, it goes `blocked` with `kind: needs-decision`, the conflicting file list, and both producer task ids. Two producers already disagree about the same lines; that is real information about the plan, it belongs to a human, and asking a child to resolve it is asking it to make a design decision it was not authorized for.

  `delegation_tasks.base_sha` records the resulting merge commit, and the producer branch heads merged in are recorded alongside it so a re-spawn or a divergence computation can reproduce the base exactly.

  **Disclosed cost, unchanged:** the consumer's base is built from branches that may later be revised (a failed integration sends a producer back to work — §10.3). §10.5's stale-contract alarm is what catches this; nothing prevents it.
- **Cross repo** — the producer's repo integration worktree is passed as `--add-dir` on the consumer's launch. This is a **direct reuse of slice 1's `Recipe.AddDirs`**, persisted, physical-path-resolved, and restored on resume, all already built and spike-verified. Write access to an add-dir cannot be technically revoked (spike: `--add-dir` grants read+write silently, with no second trust prompt), so it is constrained by §7's authorization text and **checked pre-merge** by §12.3 — exactly the "out-of-band plus checked pre-merge, never trusted to the brief" shape slice 1 §11 requires.

### 9.3 Progress and the deadlock check

The scheduler ticks on every state transition and on the poll loop. Each tick computes `ready`, `running`, `blocked`, `terminal`. If **`ready` is empty, `running` is empty, and some task is non-terminal**, the run is **DEADLOCKED** — §12.1.

---

## 10. Test-gated integration — the mandatory piece, and the hard part

Slice 1 §11: a test-gated integration step *"is the load-bearing component in every system that measured a win"*. It is also, cross-repo, the part of this design with the weakest foundation. §10.5 says so plainly.

### 10.1 Per-repo integration worktree

One per repo per run, created at run creation:

```
branch:   loom/<run-slug>/integration/<repo-label>
worktree: ~/.loom/worktrees/<run-slug>/<repo-label>/__integration
```

branched from the same pinned base as the children. It is Loom's staging area, is never a child's cwd, is never `--add-dir`'d for write, and — BINDING, §5.2 — **is never a merge source into anything the user owns.** Its only job is to answer "is this task green combined with its already-verified siblings?".

### 10.2 The integration sequence

Triggered when a task becomes `verified`. Serialized per run — one integration at a time, run-wide, because a cross-check reads several repos' integration worktrees at once and must not see one mid-merge.

0. **Record `pre = git -C <integration-wt> rev-parse HEAD`.** Every failure path below ends in `git reset --hard <pre>` (plus `git clean -fd` for merge debris). This is the rule that keeps the staging area meaningful and is BINDING — see "the integration branch only ever contains green work" below.
1. `git -C <integration-wt> merge --no-ff <task-branch>`.
   - **Conflict** → abort the merge, task → `integration_blocked`, §10.3.
2. Re-run `repos[].bootstrap` in the integration worktree if the merge touched dependency manifests (`package.json`, `go.mod`, …) — a coarse mtime test, deliberately over-eager. Failure → reset to `pre`, `integration_blocked`.
3. Run `integration.per_repo[repo]` in the integration worktree. Red → reset to `pre`, `integration_blocked`, §10.3.
4. Run every `cross` check whose `needs_repos` are all currently at a green per-repo integration. Red → reset to `pre`, `integration_blocked`, with the cross check named and attribution decided as below.
5. Green throughout → the task becomes **mergeable**, and the §5.2 gate appears. The integration branch keeps the merge.

**The integration branch only ever contains work that has been green end-to-end (BINDING).** Revision 1 aborted only on *conflict* (step 1), so a clean merge whose check was red left the failing commits in the integration branch permanently. Every later task then integrated on top of known-broken code: its check was red for reasons that had nothing to do with it, step 4 attributed that failure to "the task that triggered this pass" — systematically the wrong task — and §5.2's precondition *"per-repo integration check is green on the merged result"* became **unreachable for the rest of the run** until a human hand-repaired a branch Loom owns. That also inverts §8.4's honesty principle in the worst direction: a task could be permanently unable to be shown green because a sibling contaminated the staging area. Reset-on-red is not a nicety; without it the mechanism the evidence calls load-bearing degrades to noise after the first red.

A task reset out of the integration branch is re-attempted (from step 0, against the then-current head) whenever the child pushes new commits, exactly as a first attempt.

**Two failure attributions, distinguished explicitly** — because "the check is red" is ambiguous and the remedies are opposite:

| Observation | Blame | Escalation |
|---|---|---|
| Red with the task merged, **green at `pre` without it** | the task | `integration_blocked` on the task, §10.3 parks the child |
| Red with the task merged **and red at `pre`** | the integration **baseline** | a **run-level fault**: the run row goes red, no task is blamed, spawning stops, needs-you-grade escalation (§12.1's rendering) |

The second row is cheap to evaluate — the previous pass's result at `pre` is already recorded — and it exists because a baseline that is red for environmental reasons (§6.4) would otherwise silently blame every task in sequence.

**Cross-check execution environment:** cwd is the integration worktree of `cross.repo`; every repo in `needs_repos` is exported as `LOOM_REPO_<LABEL>=<its integration worktree path>`. So a consumer's contract test can build and run against the producer's *staged* code rather than its released code, which is the only thing that makes a cross-repo test meaningful mid-run. Loom supplies the environment; the initiative supplies the test.

### 10.3 A blocked integration is sent back to the child, not fixed by Loom

Loom does not resolve conflicts and does not fix failing tests. On `integration_blocked`:

- the task is parked using the **same mechanism as a rendezvous** (§11): Loom writes a `pending_seed` describing the conflict (file list) or the failure (check name + captured tail, capped), and delivers it through `sendPendingSeed` once the child is idle;
- the child still has full context on the work — this is the entire argument for not killing children;
- if the child is dead, the task is `orphaned` and re-spawn (§6.3) carries the failure into the new brief.

**Merge into the user's own branch (§5.2's action)** requires the target repo's working tree to be clean; a dirty tree is refused with the offending files named, because merging into a dirty tree is how a human loses work to a machine.

### 10.4 The merge, and what is merged (BINDING)

**Merge `loom/<run-slug>/<task-id>` — the task's own branch — into the user's checked-out branch** (`git merge --no-ff`, message naming run and task). Not the integration branch. Revision 1 said the integration branch, which is cumulative (§10.2 step 1 merges each task as it verifies, independently of merge approval), so the first task through the gate would have dragged in every sibling that verified before it: the human approves `diff(B)` and Loom lands `diff(A)+diff(B)`, with A's own §5.2 gate never shown, A's §12.3.1 divergence acknowledgement never given, and a `forced` flag recorded against B only. That is the exact shape §5.2 exists to forbid, on the one mechanism the evidence says is load-bearing.

Then, in order:

1. Merge task branch → user's branch. Task → `merged`.
2. **Re-derive the integration worktree from the user's branch head** (`git -C <integration-wt> reset --hard <user-branch-head>`, `git clean -fd`) and re-run `integration.per_repo[repo]`. The staging area now stages *on top of what actually shipped*, so every subsequent task's integration evidence is about the real tree rather than a parallel history. If that re-run is red, it is a **baseline** fault by §10.2's table — the user's own branch is red — and no task is blamed.
3. Worktree removed, branch kept, `.meta/` kept (§6.3).

Sibling tasks in the same repo now have a base that is behind; they are **not** rebased automatically — they will meet the merged code at their own integration step, which is exactly where a conflict should surface, with a check to run against the result.

**A verified sibling that was in the integration branch before step 2 is not lost:** it is still `verified`, its own branch is untouched, and its integration is re-attempted against the new baseline (§10.2). It has to re-earn its green against reality, which is the correct cost.

### 10.5 Cross-repo: the honest part

**No VCS operation can surface a cross-repo interface break.** `git merge` in `bankenstein` cannot know that `ballista` calls a function whose signature just changed. There is no analogue of a merge conflict across repository boundaries, and this design does not invent one.

Two mechanisms, and neither is a substitute for the missing thing:

1. **`integration.cross` (§10.2)** — a real executable check spanning repos. Strong when it exists. **It only exists if the initiative writes it.** If an initiative has no runnable cross-repo test, slice 3 **cannot** provide test-gated cross-repo integration, and the design degrades to per-repo gating plus a human reading the seam at §5.2. Stated as an accepted limit (§16), and it is the second thing 3a should measure after M3.
2. **Stale-contract alarm** — cheap, automatic, and narrow. For every `kind: "interface"` artifact, Loom records the `fingerprint` output and commit at publish time (§8.3). When a consumer becomes `verified`, Loom re-fingerprints every artifact the consumer `needs`. **If a fingerprint changed after the consumer was spawned, the consumer is flagged `stale-contract`** naming the artifact and both commits, its mergeability is withdrawn, and it is re-parked via §11's path with a seed describing the change. This catches the single most common cross-repo break — the provider revised the interface after the consumer built against it — without needing any cross-repo test at all. It catches nothing else. It is not integration testing and is not presented as such.

---

## 11. Rendezvous — the fallback (BINDING: fallback, not the mechanism)

Dependency-gated scheduling is primary. Rendezvous exists for dependencies the plan did not foresee. If most tasks rendezvous, the manifest was wrong, and that is precisely M3 in §2 — the metric that decides whether this slice should exist.

### 11.1 The STOP protocol

A child that cannot proceed writes **`<task-id>.meta/block.json`** (§6.2 step 4 — beside the worktree, not in it, so a block declaration can never be committed as an artifact and never shows up in the child's diff) and then **stops at its prompt**. It does not exit, is not killed, and is not asked to summarize. An idle `claude` at a prompt costs nothing and retains its entire context — that is the whole reason this is a park and not a restart.

```json
{ "block": 1,
  "run": "atlas-rearchitecture-7",
  "task": "auth-api",
  "at": "2026-07-22T14:03:11Z",
  "kind": "needs-artifact",
  "need": { "artifact": "account-schema", "from": "schema" },
  "summary": "auth handler needs the account table's final column set",
  "detail": "internal/auth/session.go must read account.tenant_id; the column does not exist at my base commit and I must not create it (it is task `schema`'s path).",
  "resume_when": "artifact account-schema is published and task schema is verified" }
```

`kind` ∈ `needs-artifact` (a peer must publish something) · `needs-decision` (a human must choose) · `needs-scope` (the work requires touching paths outside this task's authorization) · `blocked-external` (a credential, a service, an outage).

`need.artifact` may name an artifact **not declared in the manifest** — that is the common case for an unforeseen dependency, and §11.3 handles it.

The brief (§7 section 6) states this protocol verbatim, including: *write the file, then stop and say nothing further; do not work around the block; do not modify paths outside your authorization to unblock yourself.* A `needs-scope` block is the correct, encouraged response to discovering the task's boundary was drawn wrong — and it is the signal that makes scope overreach visible instead of silent.

### 11.2 Detection

Loom polls `block.json`'s mtime+size (the `indexed_files` fingerprint idiom) at the run view's cadence, 2s. **The file is the trigger; the transcript going idle is corroboration only.** A malformed or unparseable `block.json` becomes a loud `block-malformed` flag on the task with the raw content rendered — never silently ignored, because a swallowed block is a child parked forever with no one told.

The task moves `running → blocked` under CAS.

### 11.3 Amendments

An unforeseen dependency is recorded as a **manifest amendment** — an append-only row on the run (§13.1), never a mutation of the on-disk file, which Loom does not own and which the author may be editing.

- `kind: needs-artifact` naming an existing artifact → a new edge, consumer ← producer.
- naming an artifact that does not exist → the run raises a **re-plan request**: the human (or slice 2's orchestrator) must add the artifact to the producing task, and Loom offers the exact task/artifact to add. Loom does not invent tasks.
- `needs-scope` → a proposed authorization amendment, human-approved, which rewrites `brief.md` in place and re-seeds the child. Never auto-granted.
- **Every amendment re-runs cycle detection over the amended graph (§4.5).** An amendment that closes a loop is **rejected** and escalated. This is the specific case where a loud block would otherwise silently become a deadlock.

### 11.4 Resume — reusing the existing durable path

Verbatim reuse of `internal/workflow`'s delivery machinery: the seed is written to a durable `pending_seed` column, delivered by `sendPendingSeed`, gated by `waitForContinueGate` on transcript state, cleared on send, retried after a restart, and rendered as `seed pending` / `seed FAILED` when it is not. The re-read-before-send guard against double delivery applies unchanged.

**One step is added, and its order is load-bearing: materialize, THEN seed.**

1. The unblock condition is satisfied (producer verified, artifact published).
2. Loom materializes it in the child's worktree: `git merge <producer-branch>` for a same-repo dependency, or a re-launch with the new `--add-dir` for a cross-repo one (an add-dir cannot be added to a live session — this is the one unblock that costs a restart, and §6.3's resume-by-claude-id keeps the context).
3. **Only then** is the seed delivered: *"`account-schema` is now present at `db/migrations/0007_account.sql` in your worktree. Continue."*

Seeding before materializing sends a child a statement that is false when it reads it, and it will burn its next several turns discovering that. If step 2's merge conflicts, the seed says so instead and the task stays `blocked` with a conflict flag.

---

## 12. Deadlock, watchdogs, divergence

### 12.1 The deadlock detector (BINDING: escalate loudly)

Condition (§9.3): no task ready, none running, at least one non-terminal. Two shapes, distinguished because the remedies differ:

- **(a) Mutual wait among children** — a cycle in the *effective* graph (declared edges + accepted amendments + live blocks). Always fatal to the run as planned; requires a human re-plan. Rendered as the actual wait-for cycle, naming every task and artifact in it.
- **(b) All remaining work blocked on something outside the run** — `needs-decision`, `needs-scope`, `blocked-external`. Rendered as an actionable list of the specific decisions owed.

Escalation is **needs-you-grade**: the run row goes red and permanent, the project's needs-you count includes it, and a notification fires through the existing path — subject to slice 1 §6.4's rule that a notification escalating out of a *hidden* project degrades to a label-free body. A deadlock is never a quiet state; the entire hazard is that it looks like progress.

### 12.2 Watchdogs — flag, never kill

| Watchdog | Condition | Action |
|---|---|---|
| `no-progress` | `running`, branch head unmoved **and** transcript unadvanced for 20m | flag `stalled` |
| `check-timeout` | §8.1 | check = fail |
| `block-stale` | `blocked`, unblock condition satisfied >5m, seed not delivered | render `seed pending`, offer retry |
| `spawn-orphan` | `spawning` >60s | resolve by **cwd**, never by tag — §13.3 |
| `run-budget` | run exceeds max children or wall-clock | **stop offering new spawns**; nothing running is touched |

**No watchdog kills anything.** Loom is a window, never an owner; a stalled child may be mid-thought, and the human can attach and look. "Degrade the label, never the session" is the governing principle here and it is not negotiable for a process that holds an hour of irreplaceable context.

### 12.3 Scope divergence — and the diff primitive moves into `internal/`

**`internal/gitdiff` is NEW work in this slice (BINDING).** Revision 1 listed "`SessionDiff`'s sectioned `[]RepoDiff`" as *reused unchanged*. It cannot be: `RepoDiff`, `gitDiff` and `SessionDiff` all live in `cmd/loom-gui/diff.go`, a frontend, and ARCHITECTURE §3 is explicit that dependency direction is strictly downward — nothing in `internal/` imports a frontend. `internal/delegate` cannot call any of it. Nor is this a place to bend the rule: divergence is **persisted** (`delegation_tasks.divergence`), it **gates a state transition** (§5.2's second acknowledgement), and it must mean the same thing under the TUI, which has no diff surface at all today.

So: the primitive moves to `internal/gitdiff` (`RepoDiff`, the porcelain/stat/untracked capture, and a base-relative mode `git diff <base-sha>...<branch>`), and `cmd/loom-gui/diff.go` becomes a thin DTO shim over it. Behaviour for the existing GUI surface is unchanged; `TestSessionDiff_sectioned` and `TestSessionDiff_noDirectory` must pass untouched, which is the migration's own regression test.

Divergence is computed on every check run and again immediately before every merge — before, because a divergence discovered after a merge is a fact, not a gate.

Three comparisons:

1. **Outside declared `paths`** → `diverged` flag with the file list. Not blocking, but §5.2's merge approval requires an explicit second acknowledgement when it is non-empty.
2. **Inside a sibling task's declared `paths` in the same repo** → a stronger flag: this *predicts* the merge conflict before integration reaches it, which is worth the whole mechanism on its own.
3. **Writes outside the worktree** — the worktree makes this impossible by accident but not by absolute-path intent, and add-dirs are writable (§9.2). This is **a different mechanism from the diff above and is also new work**: it needs a spawn-time snapshot store plus a comparator, not `git diff`. At spawn, Loom records the dirty-file set (path + mtime + size, the `indexed_files` fingerprint idiom) of every in-scope repo's primary working tree and every add-dir'd integration worktree into `delegation_tasks.spawn_snapshot`; the comparator re-walks the same set at check and pre-merge time. Any change → **loud** flag, because this is the exact failure `--add-dir` cannot prevent and the brief's authorization text can only discourage.

  Cost, disclosed: this walk is O(dirty files across every in-scope repo) per check, and a repo the user is actively working in will produce **false positives** — the human's own edits are indistinguishable from a child's. The flag therefore names the files and says "changed since spawn", never "the child wrote this"; §5.2's acknowledgement text says the same.

**These are detectors, not the isolation mechanism.** Reading them as re-introducing declared-ownership-as-isolation is the specific misreading slice 1 §11's ablation forbids: the worktree keeps children apart; these say what went wrong afterwards.

---

## 13. Store — migrations v10 and v11, and the CAS discipline

Current head is v9 (slice 1 §7). `IF NOT EXISTS` on every object, one transaction per migration, standalone `ALTER`s in their own version — the non-re-entrancy trap slice 1 §7 documents.

### 13.1 Schema

```sql
-- v10
CREATE TABLE IF NOT EXISTS delegation_runs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  slug          TEXT NOT NULL UNIQUE,      -- <manifest-name>-<id>, the worktree/branch component
  name          TEXT NOT NULL,
  project_root  TEXT NOT NULL,             -- §3 containment; FK-by-convention to projects.root
  manifest_json TEXT NOT NULL,             -- SNAPSHOT (workflow_runs.def_json precedent)
  base_shas     TEXT NOT NULL,             -- JSON {repoLabel: sha}, pinned at creation
  integration   TEXT NOT NULL DEFAULT '',  -- JSON {repoLabel: {head, status, at, out}} — the
                                           -- baseline result at `pre`, read by §10.2's
                                           -- task-vs-baseline attribution table
  status        TEXT NOT NULL,             -- planning|running|deadlocked|done|abandoned
  created_at    INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS delegation_tasks (
  run_id        INTEGER NOT NULL,
  task_id       TEXT NOT NULL,
  state         TEXT NOT NULL,
  session_name  TEXT NOT NULL DEFAULT '',  -- store PK of the child, when one exists
  repo_label    TEXT NOT NULL,
  worktree      TEXT NOT NULL DEFAULT '',
  branch        TEXT NOT NULL DEFAULT '',
  base_sha      TEXT NOT NULL DEFAULT '',  -- may differ from run base (§9.2 same-repo dep)
  base_producers TEXT NOT NULL DEFAULT '', -- JSON [{task,branch,sha}] merged to build base_sha (§9.2)
  check_status  TEXT NOT NULL DEFAULT '',  -- ''|pass|fail|unpublished|env-suspect|infra-error
  check_exit    INTEGER NOT NULL DEFAULT 0,
  check_out     TEXT NOT NULL DEFAULT '',  -- capped head+tail
  check_at      INTEGER NOT NULL DEFAULT 0,
  branch_head   TEXT NOT NULL DEFAULT '',  -- last sha the check ran against (§8.2)
  block_json    TEXT NOT NULL DEFAULT '',
  pending_seed  TEXT NOT NULL DEFAULT '',
  divergence    TEXT NOT NULL DEFAULT '',  -- JSON file lists, §12.3
  spawn_snapshot TEXT NOT NULL DEFAULT '', -- JSON {dir: [{path,mtime,size}]} at spawn, §12.3.3
  flags         TEXT NOT NULL DEFAULT '',  -- JSON: stalled|orphaned|diverged|stale-contract|forced|block-malformed
  updated_at    INTEGER NOT NULL,
  PRIMARY KEY (run_id, task_id)
);
CREATE TABLE IF NOT EXISTS delegation_artifacts (
  run_id       INTEGER NOT NULL, artifact_id TEXT NOT NULL,
  task_id      TEXT NOT NULL, path TEXT NOT NULL,
  fingerprint  TEXT NOT NULL DEFAULT '', commit_sha TEXT NOT NULL DEFAULT '',
  published_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (run_id, artifact_id)
);
CREATE TABLE IF NOT EXISTS delegation_amendments (
  run_id INTEGER NOT NULL, seq INTEGER NOT NULL,
  kind TEXT NOT NULL, body TEXT NOT NULL, approved_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_dtasks_state ON delegation_tasks(state);

-- v11 (standalone ALTER, cf. v2/v3/v8)
ALTER TABLE sessions ADD COLUMN delegation TEXT NOT NULL DEFAULT '';  -- "<runID>:<taskID>"
```

`sessions.delegation` gives a reverse lookup from any session row to its task without joining through tags, and is the column §14's attribution override keys on. Tags are still set (`dlg:<slug>#<task>`), matching the `wf:` convention, for human-facing dashboard filtering only.

**Tags are NOT visible in tmux.** Revision 1 claimed "an orphan is identifiable from tmux alone" and built the `spawn-orphan` watchdog on it. `store.SetTags` (`internal/store/store.go:325`) writes the `sessions.tags` *column*; tmux carries only `loom-<uuid>`, because ARCHITECTURE §4.1 forbids embedding a label in the tmux name (`.` and `:` break `tmux -t` targeting). `internal/workflow/run.go:283,393` sets `wf:` the same way. There is nothing to interrogate in tmux, and §13.3 no longer pretends otherwise.

### 13.2 States and flags

**States** (CAS-guarded, one column): `pending → ready → approved → spawning → running ⇄ blocked → checking → verified | failed → integrating → mergeable → merged`, plus `abandoned` from anywhere.

**Flags** (an independent JSON set): `stalled`, `orphaned`, `diverged`, `stale-contract`, `env-suspect`, `forced`, `block-malformed`. Keeping these out of the state column is deliberate: a state machine with a `stalled` state has to define the cross product of stalled × everything, and the transition table stops being testable. Flags are set and cleared independently and never gate a transition on their own.

### 13.3 CAS

Every transition is `UPDATE delegation_tasks SET state=?, … WHERE run_id=? AND task_id=? AND state=?` with `RowsAffected()==0` ⇒ `ErrTaskMovedElsewhere` — the `AdvanceRunCAS` discipline, applied per task. Same for run status.

**The spawn ordering problem is narrowed here, not solved (BINDING correction).** Revision 1 claimed delegation strictly improves on `workflow.Advance`'s disclosed stranded-launch failure mode because "the worktree and branch are deterministic from `(run, task)` and exist before any launch". The claim is **retracted**: `Launcher.Launch` still mints its own session id (`launch.go:155`) and the task↔session linkage is still written *after* the process exists, so the window is the same shape. What is genuinely better is only the *recovery evidence* — and only if recovery keys on something durable that `Launch` writes atomically with the tmux session, which the tag is not.

Sequence:

1. CAS `approved → spawning` (claims the task; no session name yet).
2. Create/verify the worktree — including §6.2 step 3's **hard precondition**: refuse to launch into a worktree that already has a live `sessions` row. Write the brief into `.meta/`, bootstrap.
3. `Launch`, `SetTags(dlg:<slug>#<task>)`.
4. CAS `spawning → running`, writing `session_name`.

**Recovery keys on `sessions.cwd`.** `Launcher.Launch` upserts the session row — with `Cwd = physicalDir(<worktree>)`, the deterministic path — in the same call that creates the tmux session (`launch.go:162–167`). That is the one identity that exists the instant the child does. `spawn-orphan` therefore:

- query `sessions` for a **live row whose `cwd == physicalDir(<deterministic worktree>)`**;
- **found** → adopt it: complete the CAS to `running`, write `session_name`, re-apply the tag if missing;
- **not found**, branch has no commits → CAS back to `approved`;
- **not found**, branch has commits → `orphaned`, work preserved.

Revision 1's rule ("no tag + branch has no commits ⇒ re-approve") was actively dangerous: a crash between `Launch` and `SetTags` leaves a real, running child with no tag and — because it has only just started — no commits, so the watchdog would have re-approved it and put a **second `claude` in the same worktree on the same branch**. That is strictly worse than workflow's accepted "stranded but idle session". §6.2 step 3's precondition is the second line of defence: even a misjudging recovery cannot double-spawn, because the launch itself refuses.

**Residual hole, disclosed:** step 4's CAS can be rejected by a concurrent abandon, leaving a live child whose row is no longer in `spawning`, which a watchdog scanning `spawning` never sees. Abandon therefore also sweeps by cwd over the run's deterministic worktree paths, which closes it for the abandon case specifically; a crash between a successful `Launch` and *any* subsequent row write in a task that is then abandoned elsewhere remains a stranded-but-idle session, dashboard-visible via its `dlg:` tag. The hole is narrowed. It is not closed, and this slice does not claim it is.

---

## 14. UI, and the interaction with hiding

The run renders **into the project overview stage** slice 1 deliberately left as a shell. Tasks as an indented list ordered topologically, each with state chip, check status, block summary, flags, and at most one action. Graph rendering is slice 4's; revision 1 shows edges as `needs: account-schema (schema ✓)` text.

**Hiding (slice 1 §6.2) inherits with one decision to make.** §6.2a: hiding never alters in-flight behaviour. §6.2b: hiding suppresses new Loom-initiated background work.

| While the project is hidden | Behaviour | Why |
|---|---|---|
| Running/blocked children | untouched, keep running | §6.2a |
| Checks | **keep running**, output not rendered | The check is the run's clock; suppressing it stalls the run silently, which is worse than the spend. Output is what would leak, not execution. |
| Seed deliveries (§11.4) | **keep running** | Continuation of in-flight work, not new work |
| Spawns | **suppressed**; approve actions stay pending, greyed with a reason | A new tmux window titled with the client's repo is exactly the leak §6 exists to prevent — and it is unambiguously new work |
| Merges | suppressed (human-gated anyway, and the gate is hidden) | |
| Deadlock notification | fires, with a label-free body | §6.4 |

### 14.1 Child sessions must be attributable — the delegation override (BINDING)

Run rows attribute to their `project_root` directly — no union computation is needed (unlike workflow runs, whose steps roam), because §3 confines a run to one project. **Revision 1 stopped there, and that was a hole, not a completion:** the N *child* session rows are ordinary `sessions` rows flowing through the same DTO and hiding path as everything else, and by §6.2 their cwd is `~/.loom/worktrees/…`, deliberately outside `$LOOM_WORKSPACE`.

`projects.Resolver.Attribute` (`internal/projects/resolver.go:114`) matches only over `{projects.root} ∪ {project_repos.path}`. A worktree path matches no target, so `ok` is false for every child, and `Visible` (`resolver.go:180`) fails **closed**. Two live consequences, both wrong:

- The moment **any** project is hidden or solo is on, `Filtering()` is true and every delegation child vanishes from the rail, Finished, search, needs-you counts and notifications — **including when the user solos precisely the project the run belongs to.** The one situation where you most want to watch a run is the one that blanks it.
- With nothing hidden, `Attribute` returns the reserved Ungrouped row, so children render under **Ungrouped** rather than their project — §8's sectioned rail groups on `projectRoot`, which would be `''`.

Fail-closed is the right default for a path Loom cannot place. A delegation child is not such a path: Loom created it and knows exactly which project it belongs to.

**Fix: an explicit override, keyed on identity, not on path prefix.** `sessions.delegation` (v11) already carries `<runID>:<taskID>`. Attribution for a session consults, **before** the prefix scan: if `delegation != ''`, resolve `runID → delegation_runs.project_root` and attribute there. The override lives in a thin delegation-aware wrapper the DTO layer calls, so `internal/projects` keeps no knowledge of delegation and its own tests stay as they are. A `delegation` value naming a run row that no longer exists falls through to the prefix scan and thus to fail-closed — a deleted run is exactly the case where the conservative answer is right.

Rejected alternative: registering each run's worktree roots as ephemeral resolver targets at run creation. It works, but it re-derives attribution from paths a second time, adds targets to an O(sessions × targets) scan for every session in the process, and leaves stale targets behind after a crash. Identity beats geometry here.

The consequence is the one the user expects: **solo the run's own project and the children are visible; solo a different project and they are hidden; hide nothing and they group under their project.** §17 asserts all three.

---

## 15. Reuse ledger

**Reused unchanged:** `session.Launcher` (launch, ready/trust gate, `selectCursorPattern`, `seed_status`) · `session.Recipe.AddDirs` incl. persistence and resume re-passing (§9.2 is the primary consumer) · `session.physicalDir` (worktree paths under `~/.loom` are symlink-free on macOS only by luck; resolve anyway) · `workflow`'s `pending_seed` / `waitForContinueGate` / `sendPendingSeed` incl. the double-delivery re-read (§11.4) · `truncateBytes` / `stripCRLF` / the 8KB+15KB seed caps · identity-by-`claude_session_id` resolution (§6.3 re-spawn) · CAS-with-`RowsAffected` and the `IF NOT EXISTS` + one-transaction migration discipline · `store.SetTags` and the `wf:`-style tag convention · `memory` indexing of children (free; human-facing only, never a signal) · `internal/projects` resolver for containment and hiding (**unchanged** — §14.1's override wraps it rather than editing it) · `killButton`'s armed-confirm for force-merge and worktree removal · the summarizer's env-scrubbing and `cmd.WaitDelay` for check subprocesses.

**New:** `internal/delegate` — `manifest.go` (parse, validate, three-colour cycle detection, pure), `graph.go` (derived edges, `Ready`, deadlock predicate, pure), `worktree.go` (git plumbing, incl. §9.2's multi-producer base merge), `check.go` (the §8 runner), `run.go` (the CAS-guarded runner: approve, spawn, check, integrate, park, resume), `block.go` (declaration parsing + amendments), `attribute.go` (§14.1's delegation-aware attribution wrapper).

**New — moved down a layer, NOT reused as-is:** `internal/gitdiff`, extracted from `cmd/loom-gui/diff.go` (`RepoDiff`, `gitDiff`, plus §12.3's base-relative mode), because `internal/delegate` cannot import a frontend (ARCHITECTURE §3) and divergence is persisted and gates a transition. `cmd/loom-gui/diff.go` becomes a DTO shim; its existing tests are the migration's regression test. **New** also: §12.3.3's spawn-time dirty-set snapshot and comparator (`delegation_tasks.spawn_snapshot`), which is a different mechanism from `git diff` and has no existing implementation to lean on.

Store: v10/v11 and their accessors. GUI: the run view in the project-overview shell.

`internal/delegate` sits above `session`/`store`/`projects` and beside `workflow` in the dependency graph. It does **not** import `workflow`; the shared seed-delivery logic is small enough that copying `waitForContinueGate`'s rule with a citation beats a dependency, and this is stated so the duplication reads as a decision.

---

## 16. Rejected, with reasons

- **Instruction-level file ownership as the isolation mechanism.** Measured *below* the single-agent baseline (55.5% vs 57.2%) where worktrees scored 63.3%. `paths` survives only as a detector (§12.3).
- **Orchestrator reads children's transcripts and reviews them in prose.** Reflection-style review measures worse than doing nothing (slice 1 §11). §3 lists the only three legal channels.
- **A shared coordination file children read and write.** A shared mutable tree by another name — the ablated design — plus an unbounded agent-to-agent injection channel. Rejected on both counts.
- **Child declares done** (message, tag, marker, `.done` file, "outcome" extraction). ~22.6% of validated misalignment episodes were inaccurate self-reporting. §8 exists for exactly this.
- **Auto-merge on green.** The merge gate is the load-bearing human gate (§5.2); automating it removes the one thing the evidence says works.
- **Auto-spawn of ready tasks.** Loom's stated principle is that nothing silently auto-advances, and unattended spawning is also how a night's quota disappears.
- **Killing stalled or over-budget children.** Loom is a window, never an owner; an hour of context is not Loom's to discard.
- **Running the check inside the child's session** (`/test`, a hook). A self-report with extra steps.
- **`sh -c` for check commands.** Argv only (§4.3).
- **Manifest in `~/.loom/`.** Would require granting an agent write access to `loom.db` and every other run's state.
- **Loom authoring or repairing the manifest.** Loom validates, snapshots, and executes. It proposes the *shape* of an amendment (§11.3) and never invents a task.
- **Containers for revision 1.** Endorsed by the evidence, wrong for this codebase today; the schema reserves the word (§6.1).
- **Retry-until-green on checks.** Launders flakes into false certifications (§8.4).
- **Deleting branches on cleanup.** The only irreversible act available, for no benefit (§6.3).
- **Merging the integration branch into the user's branch.** It is cumulative, so it batches the one gate the evidence says is load-bearing and applies diffs no human was shown (§5.2, §10.4).
- **Leaving a red merge in the integration branch.** Poisons every later task's integration and systematically mis-attributes the failure (§10.2).
- **Writing Loom's per-task files inside the worktree** (`.loom/` + `info/exclude`). The exclusion is impossible in a linked worktree and its only working form writes into the user's repo (§6.2 step 4).
- **Recovering a spawn orphan by tmux tag.** Tags are a DB column; tmux carries only `loom-<uuid>` (§13.1, §13.3).
- **Importing `cmd/loom-gui`'s diff code from `internal/`.** ARCHITECTURE §3; the primitive moves down instead (§12.3).
- **Registering worktree paths as ephemeral resolver targets** to fix child attribution. Identity, not geometry (§14.1).
- **Building §§9–12 before 3a runs.** §2.

---

## 17. Testing (binding)

**Manifest / validation** — one test per §4.4 rule, each asserting a `LoadError` and not a panic: bad version, name≠stem, unknown project, repo from another project, duplicate task id, illegal id charset, unknown model/mode, missing `authorization`, missing `check`, duplicate artifact id, `needs` an undeclared artifact, self-need, artifact path with `..`, check `cwd` escaping the repo, missing `integration.per_repo` for a used repo, `cross.repo` ∉ `needs_repos`. Warnings (overlapping `paths`, unreachable leaf, `cmd[0]` not on PATH) are warnings and not errors.

**Cycle detection** — 2-cycle; 3-cycle; self-loop; a cycle reachable only from one root among several; a diamond (must NOT be flagged); a 200-node chain (no stack growth, iterative); the error message contains the actual cycle path in order; a graph that is acyclic on the manifest but cyclic after an amendment is rejected **at amendment time** with the same message.

**Scheduler** — `Ready` is pure (same input twice, same output, no writes); ready only when producer verified **and** artifact published (both halves independently negative-tested); a task with `needs: []` is ready at creation; cap reached ⇒ tasks stay ready and are not spawned; deadlock predicate true for mutual-block, true for all-external-block, **false** when a check is in flight, false when a seed is pending.

**Multi-producer base (§9.2)** — a task with **two** same-repo producers gets a base containing **both** producers' artifacts (assert both files present in the materialized worktree — this is the test Revision 1 would have failed); merge order is ascending task-id and a re-spawn reproduces the same tree; `base_producers` records both branches and shas; **a producer-vs-producer conflict at spawn does not spawn** — the task goes `blocked`/`needs-decision` with both producer ids and the conflicting file list, and no worktree is left half-merged.

**Worktree** — creation is idempotent (same call twice); an existing branch is reused, not recreated; a path collision with a *different* branch is a loud refusal; **a worktree with a live `sessions` row at its physical cwd refuses to launch** (§6.2 step 3 precondition, asserted directly — this is the double-spawn guard); `brief.md` and `block.json` are written **outside** the worktree and `git status --porcelain` in the worktree is empty after creation (the negative form of Revision 1's broken `info/exclude` test — assert nothing was written to the repo's `.git/info/exclude` either); `seed_files` refuses a tracked file, a symlink, and an escaping path; a failed bootstrap blocks the spawn with output; removal after merge keeps the branch and keeps `.meta/`; a dead child's worktree survives a Loom restart and is re-derived from `(run, task)`.

**Check** — exit 0 = pass, exit 1 = fail, timeout = fail (not unknown); `cmd[0]` missing = `infra-error` with exactly one retry; output capped head+tail with the elision marker; env has `CLAUDECODE`/`CLAUDE_CODE_*` scrubbed and `LOOM_REPO_*` present; `cwd` escaping the worktree is refused at run time even when it passed at load time; **unpublished artifact short-circuits before the command runs** (assert the command never executed); an uncommitted-but-staged artifact is `unpublished`; auto-run fires only when branch head moved **and** transcript state is idle/needs-you, and never twice concurrently; `EADDRINUSE` in output ⇒ `env-suspect` flag and the result is still a failure.

**Integration** — a clean merge + green per-repo check ⇒ `mergeable`; a merge conflict ⇒ merge aborted (integration worktree left clean, asserted) and the task parked with the conflicting file list in `pending_seed`; a green branch check + red integration check ⇒ two results recorded, not one, and no averaging; a cross check runs with all `LOOM_REPO_*` pointing at integration worktrees; merge into a dirty user tree is refused naming the files; forced merge records `forced` + the failing output permanently.

**Integration is never poisoned (§10.2)** — task A merges cleanly but its per-repo check is red ⇒ **integration branch head equals the recorded pre-merge sha** (asserted directly) and the worktree is clean; task B's subsequent integration is then **green**, and B is not blamed for A's failure. Same assertions for a red *cross* check and for a failed bootstrap. Red-with-task + green-at-`pre` ⇒ the task is blamed; red-with-task + red-at-`pre` ⇒ a **run-level** fault, no task blamed, spawning stopped. A task reset out of the integration branch is re-attempted after its child pushes new commits.

**The merge gate applies exactly one task (§5.2/§10.4)** — two tasks A and B both reach `verified` and both integrate; the human approves **only B**; assert the user's branch contains **B's commits and none of A's**, that the rendered gate diff equals the diff actually applied, and that `forced` on B records nothing about A. After the merge, the integration worktree head equals the user's branch head and the per-repo check has been re-run against it; A is still `verified`, its branch untouched, and its integration re-attempted against the new baseline.

**Stale contract** — fingerprint recorded at publish; producer revises and re-publishes; consumer that was already `verified` gets `stale-contract`, loses mergeability, and is re-parked. Fingerprint unchanged after an unrelated producer commit ⇒ no flag.

**Rendezvous** — a well-formed `block.json` moves `running → blocked` under CAS; malformed JSON ⇒ `block-malformed` with raw content, never silently ignored; a block naming an undeclared artifact raises a re-plan request and creates no edge; an accepted amendment appends and does not mutate the on-disk file; **materialize-then-seed ordering asserted** (the merge happens before `SendLiteral`, and a conflicting merge sends the conflict seed instead); `sendPendingSeed`'s re-read guard still prevents a double delivery when a retry races the async deliverer; a restart with a non-empty `pending_seed` renders `seed pending` and retries.

**CAS / two-instance** — every transition rejects a stale `from` state with `RowsAffected==0` and leaves the row untouched; two concurrent approve-spawns of the same task ⇒ exactly one `spawning`; abandon-vs-merge race cannot overwrite `merged`.

**Spawn-orphan recovery (§13.3)** — recovery resolves by **`sessions.cwd == physicalDir(worktree)`**, never by tag: (a) a live row at that cwd ⇒ **adopted**, CAS completed to `running`, `session_name` written, tag re-applied — and critically, **no second launch happens** (assert the launcher was not called); (b) no row + branch empty ⇒ back to `approved`; (c) no row + branch has commits ⇒ `orphaned`. Direct regression for the Revision 1 defect: simulate a crash **between `Launch` and `SetTags`** (row exists, tag absent, branch empty) and assert the child is adopted rather than re-approved. Assert that tags are absent from tmux (`tmux list-sessions` output contains no `dlg:`), so the tmux-tag assumption cannot be reintroduced.

**Store** — v10+v11 replay from a stale `user_version` on a real DB copy (this is what catches a non-re-entrant `ALTER`); `slug` uniqueness; artifacts and amendments survive a run status change; manifest snapshot replays after the on-disk file is edited or deleted.

**Hiding** — §14's table, one test per row, per frontend where the surface exists: children keep running, checks keep running, seeds keep delivering, **spawns are suppressed**, the run row is absent from the rail/overview, and the deadlock notification body carries no label.

**Child attribution (§14.1)** — the three tests Revision 1's plan omitted, and which it would have passed while broken: (a) **solo the run's own project ⇒ every child session is VISIBLE**; (b) solo a different project ⇒ children hidden; (c) nothing hidden ⇒ children group under `project_root`, **not Ungrouped** (assert the DTO's `projectRoot`). Plus: a child whose `delegation` names a deleted run falls through to fail-closed; `internal/projects`' own resolver tests are unchanged (the override is a wrapper, not an edit).

**Divergence** — files outside declared `paths` flagged; files inside a sibling's `paths` flagged more strongly; a write into an add-dir'd integration worktree detected against the spawn-time snapshot, and an *unchanged* dirty set produces no flag; merge approval requires the second acknowledgement when divergence is non-empty. **`internal/gitdiff` extraction**: `cmd/loom-gui`'s existing `TestSessionDiff_sectioned` and `TestSessionDiff_noDirectory` pass unmodified against the shim; base-relative mode returns `base...branch`, not `HEAD`.

**e2e** — PATH-injected fake `claude` (the workflows e2e precedent) plus a real throwaway git repo: manifest → run → approve → worktree exists on disk with the brief → fake child commits an artifact → check runs and passes → integration merges → merge gate → merged, worktree gone, branch present. Second e2e: two tasks, one blocks, amendment accepted, seed delivered after materialization, both merge.

---

## 18. Accepted limits and disclosed failure modes

- **The precondition may not hold.** §2 exists because the multi-agent benefit is conditioned on low inter-task cohesion and a multi-repo re-architecture is high-cohesion by construction. If 3a's M3 is bad, §§9–12 are not built, and the salvage is single-child worktree isolation + executable done + merge gate.
- **Cross-repo integration is only as strong as the initiative's own cross-repo test.** With no `integration.cross` entry, there is no test-gated cross-repo integration — only per-repo gating, the stale-contract alarm, and a human at §5.2. This is the weakest joint in the design and is named as such (§10.5).
- **The stale-contract alarm catches one failure shape.** Provider revises an interface after a consumer built against it. Nothing else.
- **Worktrees do not isolate ports, databases, caches, gitignored files, global tool state, or test state (§6.4).** Loom mitigates with `bootstrap`/`seed_files` and labels the common port/DB failures `env-suspect`. It does not solve them.
- **Add-dir write access cannot be revoked** (spike-verified). Cross-repo read dependencies are enforced by brief text plus a pre-merge detector, not by the OS.
- **Concurrency is capped at 10, defaulted to 4** (§6.6). The binding constraint above that is the single human at the merge gate, which no engineering fixes.
- **Same-repo dependency chains branch from a moving base** (§9.2): a producer revised after integration invalidates its consumers' base, caught only by the stale-contract alarm or by the consumer's own integration conflict.
- **A check is executed arbitrary code from an agent-authored file.** It is argv-only, rendered verbatim, and human-approved at §5.1 — which is a review gate, not a sandbox. There is no sandbox.
- **Loom never kills a child.** A wedged child holds a worktree and a slot against the cap until a human acts.
- **The merge gate (§5.2) is GUI-only in revision 1.** It renders a diff and requires a divergence acknowledgement; the TUI has no diff surface at all today, and `internal/gitdiff` gives it the primitive but not the view. A TUI user can see task state, check results and the divergence *file list*, and must approve merges from the GUI. Stated rather than hidden: this is the first place in Loom where a load-bearing gate exists in one frontend only.
- **The user's branch is not the tree the integration check certified** (§5.2, §10.4). The gate merges the task's own branch — the diff shown is the diff applied — so the integration check is evidence about a combination, not a certificate about your tree. The staging area is re-derived and re-checked after every merge, which converts the gap into a fresh signal rather than closing it.
- **Loom writes git metadata and refs into your repos** (§4.1): `.git/worktrees/<id>/`, `refs/heads/loom/**`, and the merge commit the gate creates on your branch. The no-writes property survives for **tracked content** only.
- **`spawn-orphan` recovery narrows, not closes, the stranded-launch window** (§13.3). Revision 1's claim that this strictly improves on ARCHITECTURE §8's accepted workflow failure mode is retracted.
- **§12.3.3's outside-the-worktree detector produces false positives** whenever the human edits an in-scope repo while a run is live. It reports "changed since spawn", never "the child did this".
- **No cross-run coordination.** Two runs over overlapping repos will fight at merge; nothing detects it. Runs are confined to one project (§3) but not to disjoint repo sets.
- **Manifest amendments are per-run and never written back to disk.** Re-running the same manifest re-learns the same unforeseen edges. A "promote amendments to the manifest" action is obvious follow-on work and is not in revision 1.
- **`environment-suspect` is a string-matching heuristic** on check output, offered as triage help and never as a diagnosis.
- **Slice 4 owes the graph.** Revision 1 renders dependencies as text; a manifest of thirty tasks will be unreadable until slice 4 lands.
