# Workflows Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Phase 3 Workflows v1 per spec `docs/superpowers/specs/2026-07-03-workflows-design.md` (Revision 2 — the spec's §2 decisions and §5 test list are BINDING; read it before every task).

**Architecture:** Store migration v5 + CAS-guarded run CRUD; a workflow package (JSON defs with load-time validation; a Runner doing identity-resolved, template-substituted, pending-seed-persisted step transitions); a two-section workflows view wired to `w`.

**Tech Stack:** existing only.

## Global Constraints

- Spec §2 decisions 3–11 are BINDING (CAS-before-launch, identity resolution, demote-to-fork, pending_seed + engine-state gating, caps/whitelist/ask-filter, run-id tags, confirm previews).
- UI invariants as ever (exact width, truncate-before-style, captured targets, in-flight guards, ONE tickAfter per tickMsg).
- Liveness of a step session comes from STORE rows, never tmux queries.
- gofmt -w; conventional commits; `go vet ./... && go test -race -count=1 ./...` green each task end.

---

### Task 1: Store — migration v5 + run CRUD with CAS

**Files:** Modify `internal/store/store.go` (migration slice); Create `internal/store/runs.go`, `internal/store/runs_test.go`.

**Interfaces (Produces):**
```go
type RunRow struct {
	ID int64; Name, DefJSON string; StepIdx int64
	SessionNames []string // (de)serialized JSON in the column
	PendingSeed, Status string; CreatedAt, UpdatedAt int64
}
func (s *Store) InsertRun(name, defJSON string, now int64) (int64, error) // step_idx=0, names=[], status running — names appended by the step-1 CAS? NO: simpler contract below
func (s *Store) GetRun(id int64) (RunRow, bool, error)
func (s *Store) ActiveRuns() ([]RunRow, error) // status='running', newest first
func (s *Store) SetRunStatus(id int64, status string, now int64) error
func (s *Store) ClearPendingSeed(id int64, now int64) error
// AdvanceRunCAS: the §2.6 claim. expectedStepIdx = the snapshot the caller acted on.
// Writes step_idx=newIdx, session_names=names, pending_seed=seed atomically iff
// id matches AND step_idx==expectedStepIdx AND status='running'. Returns claimed=false on 0 rows.
func (s *Store) AdvanceRunCAS(id int64, expectedStepIdx, newIdx int64, names []string, pendingSeed string, now int64) (claimed bool, err error)
```
Contract note: `InsertRun` creates the row BEFORE step-1 launches (run id needed for tags); step 1 itself is recorded via `AdvanceRunCAS(id, expected=0→... ` — NO: step-1 recording uses `AdvanceRunCAS(id, 0, 0, [name1], seedIfPending, now)` (same-index write, names go from empty to one — the CAS's expected==stored==0 holds; invariant len==step_idx+1 satisfied after). Runner (Task 2) owns this dance; the store just enforces the CAS.

- [ ] Failing tests: v5 migration on a v4-copy DB (+ re-entrancy); Insert/Get/ActiveRuns roundtrip (status filtering, JSON array integrity incl. empty); CAS: two sequential AdvanceRunCAS from the SAME expectedStepIdx → second returns claimed=false and row unchanged; SetRunStatus done/abandoned excludes from ActiveRuns; ClearPendingSeed.
- [ ] FAIL → implement → PASS + full suite → commit `feat: workflow run store with CAS advance`.

---

### Task 2: workflow package — defs + Runner

**Files:** Create `internal/workflow/def.go`, `def_test.go`, `internal/workflow/run.go`, `run_test.go`.

**Interfaces (Produces):**
```go
// def.go
type Step struct { Label, Project, Model, Mode, Seed, Relation string }
type Definition struct { Name string; Steps []Step; Path string }
type LoadError struct { Path, Err string }
func LoadAll(dir string, projects []registry.Project) ([]Definition, []LoadError)
// run.go
type Runner struct { Store *store.Store; Launcher *session.Launcher; ClaudeConfigDir string }
type StepPreview struct { Label, Relation, Seed string; Unavailable bool; Finish bool }
func (r *Runner) Preview(run store.RunRow) (StepPreview, error)      // §2.11: substituted, at confirm-open
func (r *Runner) Start(def Definition, w, h int, now time.Time) (int64, error)
func (r *Runner) Advance(run store.RunRow, forceFork bool, w, h int, now time.Time) error // forceFork = §2.8 demotion
func (r *Runner) RetryPendingSeed(run store.RunRow) error
func (r *Runner) Abandon(run store.RunRow, now time.Time) error
func (r *Runner) ResolveStepSession(run store.RunRow) (store.SessionRow, bool) // §2.5 identity resolution
func substitute(seed string, prev extractionLike) (string, bool /*hadUnavailable*/)   // §2.3 caps+filters
```
Implementation notes: Start = InsertRun → build step-1 Recipe (Tags `wf:<name>#<id>:step1`) → Launch → AdvanceRunCAS(id,0,0,[name],"") (failure here = surfaced error; the session exists but unrecorded — log-style errStr, documented). Advance = resolve current session by identity → dead+continue+!forceFork → ErrContinueDead (typed, UI offers `f`) → else per relation; CAS FIRST with pending_seed set for continue, then deliver (continue: goroutine gated on transcript state via a small poll of `transcript.NewReader(path).Poll()` — NeedsYou/Idle only — then SendLiteral+Enter, then ClearPendingSeed); fork/fresh: Launch (launch-time seeding is already safe), CAS records the new name with empty pending_seed... ORDER: CAS claim BEFORE Launch per spec §2.6 — for fork/fresh, CAS writes the new step with a PLACEHOLDER name? No — spec §2.6's point is the CLAIM; the tmux name is minted before Launch (NewSessionID→TmuxName), so: mint name → CAS(names+[name], pending="") → Launch. If Launch then fails: run points at a never-created session → ResolveStepSession returns false → run shows dead-step hint; acceptable failure mode, note in code. Continue: CAS(names+[sameName], pending=seed) → deliver async.
Fork extraction: previous step's resolved row → transcript.Path(ccd, row.Cwd, row.ClaudeSessionID) → memory.ExtractFile(path, "") → substitute with caps (8KB/value w/ `…[truncated]`, 15KB total) + ask-filter rule (reuse/replicate memory's excluded-prefix check — if memory doesn't export it, add exported memory.AskUsable(string) bool with a tiny test rather than duplicating the list).

- [ ] Failing tests per spec §5 def+run rows: validation matrix; template whitelist/typo; caps; ask-filter; CAS race (two Advance calls same snapshot → one claimed); invariant after every transition; identity-after-resume (build rows directly in store simulating a resume: old row done, new row live same claude id); dead-continue → typed error; forceFork demotion works; last-step → done; pending_seed persist/retry/clear (fake session via throwaway tmux + fake-claude script pattern; transcript-state gating tested by writing a NeedsYou-tail transcript fixture then delivering); Abandon.
- [ ] FAIL → implement → PASS + full suite → commit `feat: workflow definitions and CAS-guarded runner`.

---

### Task 3: viewWorkflows

**Files:** Modify `internal/ui/app.go`, `app_test.go`; Deps gains `Runner *workflow.Runner`, `WorkflowsDir string`, `Registry []registry.Project` (already has Projects — reuse it).

Behavior per spec §2.7/§2.8/§2.11/§4: two sections RUNS (ActiveRuns, honest store-row status, `seed pending`/`seed FAILED` markers) + WORKFLOWS (LoadAll on open; LoadErrors dim-red). Keys: ↓/↑ across sections; def ↵ start (run appears, stay in view); run ↵ attach ONLY when resolved-live else hint line; run `n` → confirm with Preview (substituted snippet ~60 chars, ⚠ unavailable, finish-run variant, `f` offered when ErrContinueDead), in-flight guard per run id, CAS-failure error surfaced; run with pending_seed: `n` = retry delivery; `x` abandon confirm; errStr rendered in body; esc dash, q/ctrl+c quit. All actions in tea.Cmds; results via msgs with stale/in-flight discipline.

- [ ] Failing tests per spec §5 ui row: cursor math across sections (incl. one/both empty); start flow; confirm previews (fork substituted, continue wording, finish variant, unavailable warning); in-flight guard; dead-attach hint; abandon; errStr in body; frame invariants 100/40 populated + empty; `w` opens from dash.
- [ ] FAIL → implement → PASS + full suite → commit `feat: workflows view with guarded advance`.

---

### Task 4: wiring + e2e + README

- [ ] main.go: Deps.Runner = &workflow.Runner{Store, Launcher, ClaudeConfigDir}; WorkflowsDir = <LoomDir>/workflows (MkdirAll); dashboard keybar `w workflows` live (drop ·soon).
- [ ] README: workflows section w/ the definition format + relations + template vars + limits.
- [ ] Ship a starter def: write `~/.loom/workflows/plan-execute-review.json` — NO: that's user-space; instead commit `docs/examples/plan-execute-review.json` and README points at it (`cp` one-liner).
- [ ] Full suite + vet + gofmt.
- [ ] E2E with PATH-injected fake claude (build a temp bin dir with a fake `claude` script ahead of PATH for the loom process in a throwaway tmux -L loomviz): copy the example def into a temp LOOM home (`HOME` override or config seam — use env HOME= pointing at a scratch home with .loom/workflows populated; CLAUDE_CONFIG_DIR pointed at a scratch archive so the indexer doesn't chew the real one) → `w` → start → capture; advance (fork) → verify substitution reached the fake's stdin sink → capture; finish → done state capture; dashboard tags visible. Record captures in report. The REAL product DB/socket untouched in this task.
- [ ] Commit `feat: wire workflows into the cockpit + example + README`.

---

## Self-Review
1. Spec §2.3–§2.11 → T1 (CAS, schema), T2 (identity/templates/pending/demotion/terminal), T3 (previews/guards/honest rows), T4 (wiring/example/e2e). §5 test list distributed verbatim. §6 limits respected (no editor/branching/auto-advance).
2. Placeholders: none — interfaces exact; the Start/Advance ordering dance is spelled out including its one accepted failure mode.
3. Types: RunRow/AdvanceRunCAS shared T1→T2; Runner/StepPreview T2→T3; Deps additions T3→T4; memory.AskUsable seam disclosed.
