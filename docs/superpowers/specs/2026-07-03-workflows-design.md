# Loom — Phase 3: Workflows (v1) — Design

**Status:** Revision 2 (hardened after 3-lens red-team: 21 findings → 14 verified → folded)
**Date:** 2026-07-03
**Scope:** Saved multi-step chains of REAL interactive claude sessions with explicit context topology. Guide-rail, not rails: the user advances every step.

## 1. What this delivers

`w` goes live. A workflow is a named recipe-chain (*plan → execute → review*) defined once. Running it launches step 1 as a normal cockpit session; the user advances when ready — Loom launches the next step per its **relation**, threading the previous step's outcome into the seed when asked. Runs survive restarts, recover from dead sessions, and are abandonable.

## 2. Design decisions

1. **Definitions = JSON files** in `~/.loom/workflows/*.json`, hand-edited (no in-app editor in v1). Loaded on every workflows-view open; malformed files listed dim-red with their error.
2. **Relations:** `fresh` (new session, seed as-is) · `fork` (new session, seed template substituted) · `continue` (seed sent into the current step's session; model/mode ignored — documented limit). Step 1's relation is ignored (always fresh). Templates are substituted for ALL relations (`{{prev.*}}` on `continue` = same session's extraction — well-defined).
3. **Templates:** `{{prev.outcome}}`, `{{prev.title}}`, `{{prev.ask}}`. Values come from `memory.ExtractFile` on the previous step's transcript and are **single-line by construction** (extraction CleanTexts ask/outcome — no embedded newline can ever submit a seed early). **Binding rules:**
   - Load-time validation: any `{{…}}` token outside the whitelist = LoadError for that file (typos surface at load, never ship literal braces).
   - `{{prev.ask}}` substitutes only an Ask that passed the real ask filters; a fallback-ask with an excluded prefix (`<command-`, `<local-command-stdout>`, `Caveat:`, `[Request interrupted`) renders `(unavailable)` (never inject command-wrapper XML).
   - Size caps: each substituted value truncated to 8KB with a visible `…[truncated]` marker; the assembled seed hard-capped at 15KB (tmux send-keys argv ceiling measured ≈16.3KB — an over-cap seed would be silently dropped).
   - Missing/empty values → `(unavailable)`; never blocks an advance.
4. **Run persistence (migration v5):**
```sql
CREATE TABLE workflow_runs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT NOT NULL,
  def_json      TEXT NOT NULL,          -- snapshot at start
  step_idx      INTEGER NOT NULL,      -- 0-based CURRENT step
  session_names TEXT NOT NULL,         -- JSON array; INVARIANT: len == step_idx+1
  pending_seed  TEXT NOT NULL DEFAULT '', -- undelivered continue/fork seed (see §4)
  status        TEXT NOT NULL,         -- running | done | abandoned
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
```
5. **Identity, not names:** `session_names` pins the tmux names Loom minted (historical record), but EVERY action (attach, continue-liveness check, fork extraction, project inheritance) resolves the step's session **by identity at action time**: `store.Get(name)` → `ClaudeSessionID`/`Cwd` → `GetLatestByClaudeSessionID` for the current live/latest row. A dashboard `r`-resume of a dead step session (new tmux name, same claude id) therefore never orphans a run.
6. **Advance is a compare-and-swap claim BEFORE any launch:**
   `UPDATE workflow_runs SET step_idx=?, session_names=?, pending_seed=?, updated_at=? WHERE id=? AND step_idx=? AND status='running'` — `RowsAffected()==0` ⇒ "run advanced elsewhere" error, NO launch. Closes double-press and two-instance races. UI adds an in-flight guard per run id (the `detailSummarizing` pattern) and the confirm re-verifies the captured step_idx against a fresh read before firing.
7. **Terminal step:** when `step_idx == len(steps)-1`, the confirm reads `finish run <name>? y/n`; `y` sets status=done (no launch, no append). The CAS's `AND status='running'` structurally rejects advances on done/abandoned rows.
8. **Dead-session recovery (continue):** `continue` onto a dead current session is refused — with recovery, not a dead end: the confirm offers **`f` — fork from transcript instead** (one-shot demotion of THIS advance to `fork`; the transcript outlives the session so the machinery already works). Attach on a dead step shows the same hint (`step session ended — n advance (f fork) · x abandon`) instead of a raw tmux error. Liveness is read from the STORE row status (tmux reaps dead panes within a poll — never query tmux for this).
9. **Seed delivery is persisted, not fire-and-forget:** the CAS write records `pending_seed`; the async delivery goroutine gates `continue` on the ENGINE's transcript-derived state (deliver only when NeedsYou/Idle — the `❯` glyph is meaningless mid-generation) and clears `pending_seed` on sent. A Loom restart with a non-empty `pending_seed` renders the run as `seed pending` and `n` retries delivery instead of advancing. Fresh/fork launches reuse the existing launch-time gated seeding (already safe pre-mount) — `pending_seed` for those clears on successful Launch handoff. A `SeedStatus=failed` on the step's session row renders loudly on the run (`seed FAILED`).
10. **Tags carry the run id:** `wf:<name>#<runID>:step<N>` — `InsertRun` happens BEFORE the step-1 launch so the id exists. Unambiguous for concurrent runs of the same workflow; orphaned sessions are detectable.
11. **Confirm shows what will happen:** substitution runs at confirm-OPEN (deterministic local file read — openDetail precedent): `advance to step 2 execute (fork) · seed: "Execute the plan… " y/n`, with `⚠ prev.outcome unavailable` appended when any token resolved to `(unavailable)`. `continue` confirms read `sends into current session`.
12. **Abandon ≠ kill; project inheritance; def_json snapshot** — as v1 drafted: sessions outlive abandonment; empty step project inherits the resolved previous step's cwd/label (step 1 must name a project, validated against the registry); runs replay their snapshot even if the file changes. A run whose def_json fails to parse (corruption) renders dim-red, abandonable, never panics.

## 3. Definition format (README-documented)

```json
{ "name": "plan-execute-review",
  "steps": [
    { "label": "plan",    "project": "parallax", "model": "opus", "mode": "plan",
      "seed": "Plan the following work: <describe>. Write the plan to docs/plan.md.", "relation": "fresh" },
    { "label": "execute", "model": "sonnet", "mode": "acceptEdits", "relation": "fork",
      "seed": "Execute the plan just written. Prior step concluded: {{prev.outcome}}" },
    { "label": "review",  "relation": "fresh", "seed": "/code-review" } ] }
```
Load validation: name == filename stem; ≥1 step; step-1 project known to the registry; relations/models/modes in known sets; template-token whitelist. Only `*.json` regular files at the top level of the dir are considered.

## 4. Architecture

```
internal/workflow/def.go   — Definition/Step, LoadAll(dir, projects) ([]Definition, []LoadError)
internal/workflow/run.go   — Runner{Store, Launcher, ClaudeConfigDir}: Start/Advance/AdvanceAsFork/
                             RetryPendingSeed/Abandon; resolveStepSession (identity, §2.5);
                             substitute() (§2.3); deliverContinueSeed (engine-state-gated, §2.9)
internal/store             — v5 + InsertRun/UpdateRunCAS/GetRun/ActiveRuns/SetRunStatus/SetPendingSeed
internal/ui                — viewWorkflows + confirm-advance/abandon flows; errStr rendered in-body;
                             attach gated on resolved-live; keybar `w workflows` live
```

**RUNS rows render honestly from store rows** (survives restart/reap): `▸ name#id · step 2/3 execute · <status icon+label>` (+ `· seed pending` / `· seed FAILED`).

## 5. Testing (binding)

- def: every validation failure = LoadError not panic; template-token typo rejected; registry resolution.
- run: CAS race (two Advances from the same snapshot — second gets 0 rows, no double launch); invariant `len(session_names)==step_idx+1` after every transition incl. continue; terminal confirm → done; fork substitution from a fixture transcript incl. 8KB/15KB caps and ask-filter rule; identity resolution after a simulated resume (old name dead, new row same claude id → attach/continue/fork all use the new row); dead-continue refusal + AdvanceAsFork demotion; pending_seed persisted, retried, cleared; abandon.
- store: v5 migration on v4 copy; CAS semantics; ActiveRuns.
- ui: two-section cursor across RUNS/WORKFLOWS incl. empty sections; confirm previews (substituted snippet + unavailable warning); in-flight guard; errStr in body; frame invariants 100/40 populated+empty; dead-attach hint.
- e2e: PATH-injected fake `claude`; full run: start → advance(fork, substitution visible in fake's stdin) → finish; dashboard shows `wf:` tags; captures.

## 6. Accepted limits (v1)

No in-app editing · no branching/conditionals · no auto-advance · no scheduling · no per-step worktree isolation · `continue` can't change model/mode · no `{{prev.summary}}` (no surprise API cost mid-advance) · run rows never GC'd (future: recent-runs section) · `continue` seed delivery waits for NeedsYou/Idle — if the user never lets the session settle, the seed stays pending (visible, retryable).
