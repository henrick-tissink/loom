# Loom — Phase 3: Workflows (v1) — Design

**Status:** Draft for red-team review
**Date:** 2026-07-03
**Scope:** The last pillar of the original vision: saved, multi-step chains of REAL interactive claude sessions with explicit context topology. v1 is a guide-rail, not rails: the user advances every step by hand.

## 1. What this delivers

`w` goes live. A workflow is a named recipe-chain (e.g. *plan → execute → review*) defined once and run against any project. Running it launches step 1 as a normal cockpit session; when the user judges a step done, they advance — Loom launches the next step per its **context relation**, threading the previous step's outcome into the new session's seed when asked. Runs are visible, resumable across Loom restarts, and abandonable.

## 2. Design decisions (made, disclosed)

1. **Definitions are JSON files** in `~/.loom/workflows/*.json`, hand-edited. No in-app editor in v1 (YAGNI — the user is a developer; a documented format beats a form). Loaded fresh each time the workflows view opens; malformed files are listed with their error, never crash.
2. **Step relations:**
   - `fresh` — new session; seed used as-is. (Step 1 is always effectively fresh; its relation field is ignored.)
   - `fork` — new session; the seed is a TEMPLATE: `{{prev.outcome}}` (last assistant text of the previous step's session), `{{prev.title}}`, `{{prev.ask}}` are substituted before seeding. Extraction reuses `memory.ExtractFile` on the previous session's transcript (deterministic, no LLM). Missing/empty values substitute as `(unavailable)` — never block the advance.
   - `continue` — NO new session: the step's seed is sent into the CURRENT session (gated seed machinery). The step's model/mode fields are ignored (changing a live session's model/mode via key injection is fragile — documented v1 limit).
3. **Advancing is manual and explicit** (the guide-rail principle from the original design). No auto-advance on needs-you, no scheduling. The user can attach/detach/work in the step's session as long as they like first.
4. **Run persistence:** migration v5 table `workflow_runs(id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, def_json TEXT, step_idx INTEGER, session_names TEXT /*JSON array, one per launched step*/, status TEXT running|done|abandoned, created_at INTEGER, updated_at INTEGER)`. `def_json` snapshots the definition at start — editing the file mid-run never corrupts an active run.
5. **Dashboard visibility:** each step session is tagged `wf:<name>:<stepN>` via the existing Tags field at launch — chains are visible on the main dashboard without new columns.
6. **Project inheritance:** a step with empty `project` inherits the previous step's cwd/label (the common case: whole chain in one project). Step 1 requires a project (validated at load).
7. **Abandon ≠ kill:** abandoning a run marks it `abandoned`; its sessions stay alive as normal sessions (the user kills them from the dashboard if wanted). Advancing past the last step marks the run `done`.

## 3. Definition format (documented in README)

```json
{
  "name": "plan-execute-review",
  "steps": [
    { "label": "plan",    "project": "parallax", "model": "opus",   "mode": "plan",
      "seed": "Plan the following work: <describe>. Write the plan to docs/plan.md.", "relation": "fresh" },
    { "label": "execute", "model": "sonnet", "mode": "acceptEdits", "relation": "fork",
      "seed": "Execute the plan that was just written. Prior step concluded: {{prev.outcome}}" },
    { "label": "review",  "relation": "fresh", "seed": "/code-review" }
  ]
}
```
Validation at load: non-empty name (must match filename stem), ≥1 step, step 1 has project, relations ∈ {fresh, fork, continue}, models/modes ∈ the launcher's known sets (empty = default). `project` values resolve against the registry by label; unknown project = load-time error for that file.

## 4. Architecture

```
internal/workflow/
  def.go     — Definition/Step structs, LoadAll(dir, registry) ([]Definition, []LoadError), validation
  run.go     — Runner: Start(def) / Advance(run) / Abandon(run); template substitution
internal/store — migration v5 + RunRow CRUD: InsertRun, UpdateRun, ActiveRuns, GetRun
internal/ui    — viewWorkflows (runs + definitions sections), keybar `w workflows` live
```

**Runner.Start(def):** launch step 1 via `Launcher.Launch` (Recipe from the step; Tags `wf:<name>:step1`) → InsertRun(step_idx=0 wait — step_idx = index of the CURRENT step, 0-based; session_names=[name1], status running).

**Runner.Advance(run):**
- current step is last → status done, no launch.
- next step relation `continue` → seed-template? NO — continue seeds are sent verbatim (no `{{prev}}`: the session already has its own context; substitution allowed anyway? Keep it simple and consistent: TEMPLATES ARE SUBSTITUTED FOR ALL RELATIONS — for `continue`, `{{prev.*}}` refers to the SAME session's extraction, which is well-defined and occasionally useful). Send via the existing gated `seedWhenReady` path against the current session's tmux name; session_names appends the SAME name; step_idx++.
- `fork`/`fresh` → build Recipe (project inherited if empty; fork substitutes `{{prev.*}}` from the previous step session's transcript via `memory.ExtractFile(transcript.Path(ccd, prevCwd, prevSessionID), "")`); `Launcher.Launch`; append new session name; step_idx++.
- The previous step's session is NEVER killed by an advance.
- Advance is allowed even if the current step's session has died (its transcript still exists for fork extraction; `continue` onto a dead session is refused with an error hint — the one guard).

**viewWorkflows:** framed. Sections: `RUNS` (active: `▸ name · step 2/3 execute · <session status icon+label>`) and `WORKFLOWS` (defs: `name · N steps · first-project`; load errors dim-red inline). Keys: `↓/↑` move across both sections; on a def: `↵` start (jump to viewDash with the new session hint? stay in workflows view showing the new run — STAY, the run appears in RUNS); on a run: `↵` attach to current step's session (tea.ExecProcess), `n` advance (with a one-line confirm showing the next step's label+relation: `advance to step 3 review (fresh)? y/n`), `x` abandon (confirm y/n, captured-target discipline); `esc` dashboard; `q`/`ctrl+c` quit. Frame invariants as ever.

**Advance UX detail:** after `y`, the launch/seed happens in a tea.Cmd; result message refreshes the runs list; errors surface in the standard errStr slot.

## 5. Testing

- def.go: valid file loads; each validation failure (missing project step 1, bad relation, unknown project, name/filename mismatch, malformed JSON) produces a LoadError not a panic; project resolution against a fake registry.
- run.go: Start inserts run + launches (throwaway tmux socket, fake-claude pattern from Phase-1 launch tests); Advance fresh/fork/continue semantics incl. template substitution from a real fixture transcript (fork), same-session seeding (continue), refusal on dead-session continue, done-at-last-step, abandoned runs excluded from ActiveRuns; sessions tagged `wf:name:stepN`.
- store: v5 migration; run CRUD roundtrip; ActiveRuns filters status.
- ui: `w` opens view (frame invariants 100/40, populated + empty states); start-run flow (def selected → run appears); advance confirm flow (captured target, y launches via cmd, n cancels); abandon; attach returns tea.ExecProcess cmd; esc/q/ctrl+c.
- e2e (final task): real workflow file, real run through 2 steps with a trivial fake... NO — real claude steps are expensive/slow; e2e uses the fake-claude script for step sessions (PATH-injected Launcher binary? Launcher runs "claude" fixed... Launcher.Launch builds `claude` argv via Recipe.ShellCommand — for e2e, use a PATH override dir containing a fake `claude`). Verify: start → advance(fork with substitution visible in the fake's captured stdin) → advance → done; captures of the workflows view; dashboard shows wf: tags.

## 6. Accepted limits (v1)

- No in-app workflow editing/creation; no branching/conditionals; no auto-advance; no scheduling; no per-step worktree isolation (future); `continue` cannot change model/mode; `{{prev.summary}}` (LLM) not offered — deterministic outcome only (no surprise API costs mid-advance).
- Run rows are never garbage-collected in v1 (tiny; a future `Recent runs` section could show done/abandoned).
