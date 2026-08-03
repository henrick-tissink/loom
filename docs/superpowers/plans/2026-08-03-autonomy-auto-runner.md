# Autonomy: the Auto-Runner (Autonomy Slice 1 of 4) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Once a delegation run is confirmed and flagged autonomous, loom drives it to completion on its own — spawn ready slices → run each slice's check when its child finishes → unblock and spawn the next wave → integrate → **full-auto-merge** — bounded by the concurrency cap + budget, visible and abortable, pausing only on genuine escalations. No per-step human action.

**Architecture:** A background **auto-runner** — one goroutine per autonomous run — that loops over loom's EXISTING primitives rather than inventing new ones: `delegate.Runner.Tick` (state: block-detection, ready-set promotion), the `ApproveTask` core (`AdvanceTaskCAS: ready→approved` + `Spawner.Spawn`), `RunTaskCheck`, and `IntegrateTask`/`MergeTask`. The genuinely new pieces are: the loop, **child-done detection** (when to run a check — via the status engine), a check-fail **retry** with an escalation ceiling, **full-auto-merge** on green integration gates, and **abort/pause**. This slice runs on the existing integration gates; the consumer-driven seam checks (spec §3, the merge guard) are Autonomy Slice 3 and land right behind it.

**Tech Stack:** Go 1.26. `internal/delegate` (Runner/Spawner/Integrator), `internal/status` (child-done signal), `cmd/loom-gui` (the App owns the runner + store), `cmd/loom` (the `loom run` CLI). Standard `go test`.

## Global Constraints (from spec §11, BINDING)

- **The agent never spawns.** The auto-runner is loom code; the orchestrator agent only *triggers* it (Slice 2). Every spawn is still `Spawner.Spawn` under its CAS. Human-gated supervised mode (`ApproveTask`) stays intact and unchanged.
- **Bounded to the validated plan.** The runner executes only the manifest already loaded for the run; it never re-plans or invents tasks.
- **Caps bind autonomy exactly as approval does** — the CAS concurrency cap (`Worktrees.cap()`, default 4, clamp [1,10]) and the run's token/cost budget gate every spawn. A run that would exceed the budget escalates instead of spawning.
- **Abortable and visible.** The run carries a mode/state; an abort stops the loop before the next spawn/check/merge; the control room reflects state live.
- **Full-auto-merge merges only on green gates.** A conflict, a failing check, or a scope divergence NEVER merges — it escalates. Every merge is an ordinary git commit (revertable).
- **Escalations go straight to the human** in this slice (orchestrator-first routing is a later refinement).

---

### Task 1: Run mode + `loom run start --auto` + abort

**Files:**
- Modify: `internal/store/delegation.go` (or wherever `delegation_runs` is defined) — add `mode` (`supervised`|`auto`) and `run_state` (`running`|`paused`|`escalated`|`aborted`|`done`) columns + a store migration + getters/CAS setters.
- Modify: `cmd/loom/main.go` — a `run` subcommand: `loom run start --auto <projectRoot> <manifestName>` and `loom run abort <runID>`.
- Create: `cmd/loom-gui/autorun.go` — `App.StartAutonomousRun(root, manifestName) (int64, error)` and `App.AbortRun(runID) error` bridge methods (also called by the CLI path).
- Test: `internal/store/delegation_test.go`, `cmd/loom-gui/autorun_test.go`.

**Interfaces:**
- Produces: `store` gains `SetRunMode(runID, mode)`, `SetRunState(runID, from, to string) (bool, error)` (CAS), and `mode`/`run_state` on the run row DTO. `StartAutonomousRun` = the existing `StartDelegationRun` create path + `SetRunMode(auto)` + starts the runner (Task 2). `AbortRun` = `SetRunState(*, aborted)`.
- Consumes: `StartDelegationRun` (exists), `delegate.LoadAll`, `NewResolver`.

- [ ] **Step 1: Write the failing test** — an autonomous run is created flagged `auto`/`running`, and abort moves it to `aborted`.

```go
func TestStartAutonomousRunFlagsAndAborts(t *testing.T) {
	app, st := newDelegationTestApp(t) // reuse orchestration_test.go's harness
	seedManifest(t, app, "atlas") // reuse the existing test manifest fixture
	id, err := app.StartAutonomousRun(app.testRoot, "atlas")
	if err != nil { t.Fatal(err) }
	run, _ := st.GetDelegationRun(id)
	if run.Mode != "auto" || run.State != "running" {
		t.Fatalf("run mode/state = %q/%q, want auto/running", run.Mode, run.State)
	}
	if err := app.AbortRun(id); err != nil { t.Fatal(err) }
	run, _ = st.GetDelegationRun(id)
	if run.State != "aborted" { t.Fatalf("state = %q, want aborted", run.State) }
}
```

- [ ] **Step 2: Run it — verify it fails** (`Mode`/`State` undefined). Run: `go test ./cmd/loom-gui/ -run TestStartAutonomousRun`.
- [ ] **Step 3: Add the columns + migration + CAS setters.** READ `internal/store/delegation.go` first for the run schema + how existing columns/migrations are done; add `mode TEXT NOT NULL DEFAULT 'supervised'` and `run_state TEXT NOT NULL DEFAULT 'running'`, a versioned migration, and `SetRunMode`/`SetRunState(CAS)`.
- [ ] **Step 4: Implement `StartAutonomousRun`/`AbortRun` + the CLI `run` subcommand** (mirror the `manifest` subcommand dispatch added earlier). Wire `StartAutonomousRun` to create via the existing path, flag `auto`, and start the runner (a no-op stub until Task 2).
- [ ] **Step 5: Run it — verify it passes.** Full `go test ./cmd/loom-gui/ ./internal/store/`.
- [ ] **Step 6: Commit.** `git commit -m "feat(delegate): run mode + loom run start --auto + abort"`

---

### Task 2: The auto-runner loop + child-done detection (the core)

**Files:**
- Create: `internal/delegate/autorun.go` — `AutoRunner` with the tick loop and the pure `nextActions(state) []Action` decision function.
- Create: `internal/delegate/autorun_test.go` — drives the pure decision function through a full run with fakes (no real spawn/check).
- Modify: `cmd/loom-gui/autorun.go` — the App owns a `map[int64]*AutoRunner`, starts one per autonomous run, stops it on abort/done.

**Interfaces:**
- Consumes: `Runner.Tick`, the task state set (`delegate.State*`), `status.Snapshot` (child-done signal).
- Produces: `AutoRunner.Run(ctx)` (the loop), and a PURE `nextActions(run, tasks, statuses, now) []Action` where `Action` is one of `SpawnTask{id}`, `CheckTask{id}`, `IntegrateTask{id}`, `Escalate{reason}`, `Done`. The loop = Tick → `nextActions` → execute each Action → sleep → repeat. Purity is what makes it testable without a live tmux.

- [ ] **Step 1: Write the failing test for `nextActions`** — the decision function, exhaustively, as a table. This is where child-done detection is pinned:

```go
func TestNextActions(t *testing.T) {
	cap := 2
	cases := []struct {
		name     string
		tasks    []taskState   // {id, state, childIdle bool, checkedAt, committedAt, lapses}
		want     []Action
	}{
		{"ready under cap spawns", []taskState{{"a", Ready, false, 0, 0, 0}, {"b", Ready, false, 0, 0, 0}},
			[]Action{SpawnTask{"a"}, SpawnTask{"b"}}},
		{"ready over cap spawns only up to remaining", []taskState{
			{"a", Running, false, 0, 0, 0}, {"b", Running, false, 0, 0, 0}, {"c", Ready, false, 0, 0, 0}},
			nil}, // cap 2 already used by two running
		{"child idle with commits since spawn -> check", []taskState{
			{"a", Running, true /*idle*/, 0 /*never checked*/, 100 /*committed*/, 0}}, []Action{CheckTask{"a"}}},
		{"child still working -> no check", []taskState{{"a", Running, false, 0, 100, 0}}, nil},
		{"verified -> integrate", []taskState{{"a", Verified, false, 0, 0, 0}}, []Action{IntegrateTask{"a"}}},
		{"all merged -> Done", []taskState{{"a", Merged, false, 0, 0, 0}}, []Action{Done{}}},
		{"lapses at ceiling -> escalate", []taskState{{"a", Running, true, 50, 100, maxCheckRetries}},
			[]Action{Escalate{"check failed " + itoa(maxCheckRetries) + "x: a"}}},
	}
	for _, c := range cases { /* assert nextActions(...) == c.want */ }
}
```

- [ ] **Step 2: Run it — verify it fails** (`nextActions` undefined).
- [ ] **Step 3: Implement `nextActions`.** The rules (child-done detection is rule 2, the key decision): (1) promote/count: how many are live (`spawning|running|checking|integrating`) → `remaining = cap - live`; emit `SpawnTask` for up to `remaining` `Ready` tasks, oldest first, unless budget exhausted (→ `Escalate{"budget"}`). (2) **A `Running` task whose child session is idle/ended AND has commits after its spawn AND `lapses < maxCheckRetries` → `CheckTask`.** (3) `Verified` → `IntegrateTask`. (4) A task at `lapses >= maxCheckRetries`, a deadlock (all live tasks blocked, none ready), or a `needs-decision`/`needs-scope` park → `Escalate`. (5) every task `Merged`/terminal → `Done`. Deterministic order (sort by task id).
- [ ] **Step 4: Run it — verify it passes.**
- [ ] **Step 5: Implement `AutoRunner.Run(ctx)`** — the loop: on each iteration, `if run.State == aborted { return }`; `Runner.Tick`; load task states + `status.Snapshot`; `acts := nextActions(...)`; execute each (Task 3–5 provide the executors; here call stubs); if `Done` set `run_state=done` and return; if `Escalate` set `run_state=escalated` + record reason + return; sleep the tick interval; repeat. Context-cancel on abort.
- [ ] **Step 6: Commit.** `git commit -m "feat(delegate): auto-runner loop + pure nextActions decision (child-done detection)"`

---

### Task 3: Auto-spawn executor (cap + budget bound)

**Files:** Modify `internal/delegate/autorun.go` (the `SpawnTask` executor); `cmd/loom-gui/autorun.go` wires the App's spawner/budget. Test: `autorun_test.go`.

**Interfaces:** Consumes the `ApproveTask` core — `AdvanceTaskCAS(ready→approved)` + `Spawner.Spawn(run, m, task)` — reused verbatim from a loop context. Produces `execSpawn(taskID) (sessionName string, err error)`.

- [ ] **Step 1: Write the failing test** — with a fake spawner, 3 ready tasks and cap 2 spawn exactly 2 (the third stays ready); a spawn that fails escalates that task, not the run.
- [ ] **Step 2: Run — fails.**
- [ ] **Step 3: Implement `execSpawn`** = `AdvanceTaskCAS(runID, id, Ready→Approved)`; if claimed, `Spawner.Spawn`; a `!claimed` (cap/race) is a no-op this tick, not an error. Budget check before spawn.
- [ ] **Step 4: Run — passes.**
- [ ] **Step 5: Commit.** `git commit -m "feat(delegate): auto-runner spawns ready tasks, cap- and budget-bound"`

---

### Task 4: Check-on-done + retry, escalate at the ceiling

**Files:** Modify `internal/delegate/autorun.go` (the `CheckTask` executor). Test: `autorun_test.go`.

**Interfaces:** Consumes `RunTaskCheck` (exists) and the resume/nudge path used by park/resume. Produces `execCheck(taskID)` → runs the check; pass → `Verified`; fail → increment lapses + nudge the child with the failing output (a resume) if `lapses < maxCheckRetries`, else leave for `nextActions` to escalate.

- [ ] **Step 1: Write the failing test** — a passing check moves a task to `Verified`; a failing check increments lapses and nudges the child; at `maxCheckRetries` the next `nextActions` yields `Escalate`.
- [ ] **Step 2–4: implement + verify.**
- [ ] **Step 5: Commit.** `git commit -m "feat(delegate): auto-runner runs checks on done children, retries then escalates"`

---

### Task 5: Integrate + full-auto-merge on green gates

**Files:** Modify `internal/delegate/autorun.go` (the `IntegrateTask` executor). Test: `autorun_test.go`.

**Interfaces:** Consumes `IntegrateTask` + `MergeTask` (exist; MergeTask already refuses on conflict/divergence and never auto-resolves). Produces `execIntegrate(taskID)` → integrate; if the integration gates are green and git merges cleanly → merge (`Merged`); a conflict/divergence/failed integration check → `Escalate` (no merge).

- [ ] **Step 1: Write the failing test** — a verified task with green gates auto-merges to `Merged`; a task whose integration check fails (or conflicts) escalates and does NOT merge.
- [ ] **Step 2–4: implement + verify.**
- [ ] **Step 5: Commit.** `git commit -m "feat(delegate): auto-runner full-auto-merge on green gates, escalate on conflict/fail"`

---

### Task 6: Escalation + abort surfacing in the control room

**Files:** Modify `cmd/loom-gui/orchestration.go` (`OrchestrationSnapshot`/run DTO to carry `mode`, `run_state`, escalation reason); `cmd/loom-gui/frontend/main.js` (render an autonomous run's state + reason + a Pause/Kill control). Test: `cmd/loom-gui/autorun_test.go` for the DTO; frontend verified by build + the never-blank matrix.

**Interfaces:** Consumes the run's `mode`/`run_state`/reason. Produces the DTO fields + a `Pause/Kill` wired to `AbortRun`.

- [ ] **Step 1: Write the failing test** — the snapshot/run DTO carries `mode="auto"`, `state`, and (when escalated) the reason.
- [ ] **Step 2–4: implement + verify** (DTO test + `make gui` builds; drive an escalated run and confirm the control room names the reason and offers Kill).
- [ ] **Step 5: Commit.** `git commit -m "feat(gui): control room shows autonomous run state + escalation + kill"`

---

## After this plan (the "test it out")

Not a task; the acceptance check. On **orchestra-demo**, with the existing valid `farewell-path.json`: `loom run start --auto ~/Sauce/orchestra-demo farewell-path` → watch loom spawn both slices, run their checks when the children finish, and merge — with no clicks — and confirm you can Kill it mid-run. What surfaces here feeds **Slice 2** (the console trigger — the orchestrator says the arm-phrase and runs this itself), **Slice 3** (consumer-driven seam checks — the merge guard), and **Slice 4** (escalation routing through the orchestrator).

## Self-Review

- **Spec coverage:** implements §11's auto-runner (spawn→check→unblock→integrate→full-auto-merge), the cap/budget bounds, abort, and escalation-to-human. It does NOT cover the console trigger (§11 step 2 → Slice 2) or the consumer-driven merge guard (§3 → Slice 3); the acceptance check names them.
- **Placeholder scan:** the loop and its decision are fully specified (`nextActions` table); executors reuse named existing ops (`ApproveTask` core, `RunTaskCheck`, `IntegrateTask`/`MergeTask`). The two "READ … first" steps are grounding on small, located functions, not deferrals.
- **Type consistency:** `Action` variants (`SpawnTask`/`CheckTask`/`IntegrateTask`/`Escalate`/`Done`) are used identically across Tasks 2–5; `execSpawn`/`execCheck`/`execIntegrate` names match their tasks; `mode`/`run_state` names match Task 1.
- **Open design point (flagged, not hidden):** child-done detection (Task 2, rule 2) — "child session idle/ended AND commits after spawn" — is the one genuinely new signal; Task 2's test table pins it, and it is the thing to validate first on the real demo.
