# Orchestrator Authorship (Phase A · Plan 1 of 3) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the orchestrator author a *loadable* delegation manifest from a plain-language intent and dispatch children that do subagent-driven development — the minimal spine that turns the existing (hand-manifest-only) machinery into an end-to-end testable loop.

**Architecture:** Three surgical changes to prompt/brief text plus one shipped reference doc. No engine changes (those are Plan 3). The orchestrator's brief is a deterministic, golden-tested assembly (`internal/orchestrator/assemble.go`); we (1) ship a manifest-schema doc whose embedded example provably loads via `delegate.LoadAll`, (2) widen the orchestrator's write scope to `<root>/.loom/manifests/` and rewrite its "What to do" to instruct manifest authoring + the single-session fallback, and (3) add a subagent-driven-development instruction to the child brief (`delegate.Brief`).

**Tech Stack:** Go 1.26; `internal/orchestrator` (brief assembly), `internal/delegate` (manifest loader + child brief). Tests are standard `go test`.

## Global Constraints

- **Human-gated spawns are kept.** The orchestrator's brief MUST retain the line *"You may not start, resume, or kill other sessions."* — the human still gates every spawn via `ApproveTask`. Only the "delegation does not exist yet — write the split and stop" clause is replaced.
- **Write scope widens by EXACTLY one path:** `<projectRoot>/.loom/manifests/`. Nothing else becomes writable. The scope section is the never-truncated, safety-load-bearing text (`assemble.go` §4.1) — edit it deliberately, do not restructure it.
- **The brief stays deterministic** — identical `Input` ⇒ byte-identical brief. The golden test in `internal/orchestrator/assemble_test.go` is the guard; update it in the same task that changes the text.
- **The manifest schema doc's embedded example MUST load** — `delegate.LoadAll` accepts it with zero errors. This is the plan's key correctness anchor: the example we hand the LLM is the one we prove loads.
- **`manifest` version is `1`**; a task's `authorization` is required and non-empty; `repo` is a project repo LABEL; `needs` names produced artifact ids; ids match `^[a-z0-9-]{1,64}$`; every task needs a non-empty `check.cmd` (Plan 3 relaxes this for leaves — NOT this plan; here every example task carries a real check).

---

### Task 1: Ship a manifest-schema reference doc whose example provably loads

**Files:**
- Create: `internal/orchestrator/manifestdoc.go` — the schema doc as a Go const + a writer.
- Create: `internal/orchestrator/manifestdoc_test.go` — proves the embedded example loads via `delegate.LoadAll`.
- Modify: `internal/orchestrator/spawn.go` (`Spawn`, around line 149-174) — write the doc into `notesDir` at spawn so it lands in the orchestrator's already-`--add-dir`'d, readable context.

**Interfaces:**
- Produces: `orchestrator.ManifestSchemaDoc string` (the markdown); `orchestrator.WriteManifestSchemaDoc(notesDir string) (string, error)` (writes `<notesDir>/loom-manifest-schema.md`, returns its path).
- Consumes: `delegate.LoadAll(dir string, resolver *delegate.Resolver) ([]delegate.Manifest, []delegate.LoadError)` — the loader the example must satisfy.

- [ ] **Step 1: Write the failing test** — the doc's embedded example manifest loads clean.

```go
// internal/orchestrator/manifestdoc_test.go
package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/delegate"
)

// extractExampleJSON pulls the single ```json fenced block out of the doc.
func extractExampleJSON(t *testing.T) string {
	t.Helper()
	const fence = "```json"
	i := strings.Index(ManifestSchemaDoc, fence)
	if i < 0 {
		t.Fatal("schema doc has no ```json example block")
	}
	rest := ManifestSchemaDoc[i+len(fence):]
	j := strings.Index(rest, "```")
	if j < 0 {
		t.Fatal("unterminated ```json block")
	}
	return strings.TrimSpace(rest[:j])
}

func TestManifestSchemaDocExampleLoads(t *testing.T) {
	dir := t.TempDir()
	// The example is a two-repo manifest; name it "example" and give the loader
	// a resolver whose repo labels match the example's `repos` keys.
	if err := os.WriteFile(filepath.Join(dir, "example.json"), []byte(extractExampleJSON(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Resolver maps the example's labels to temp dirs so path/containment passes.
	repoA, repoB := t.TempDir(), t.TempDir()
	res := delegate.NewResolver(map[string]string{"api": repoA, "web": repoB})
	ms, errs := delegate.LoadAll(dir, res)
	if len(errs) != 0 {
		t.Fatalf("example manifest must load clean, got errors: %+v", errs)
	}
	if len(ms) != 1 || ms[0].Name != "example" {
		t.Fatalf("want 1 manifest named 'example', got %+v", ms)
	}
}
```

- [ ] **Step 2: Run it — verify it fails** (`ManifestSchemaDoc` undefined).

Run: `go test ./internal/orchestrator/ -run TestManifestSchemaDocExampleLoads`
Expected: FAIL — undefined: `ManifestSchemaDoc`.

- [ ] **Step 3: Write `manifestdoc.go`.** First READ `internal/delegate/manifest.go` (the `Manifest`/`Task`/`RepoSetup`/`Artifact`/`Check` structs and `loadOne` validation, ~lines 369-561 and 760-770) so the example uses exact field names and satisfies every validation. Then author the doc: a tight schema description (field-by-field, the "seam ⇒ check + interface artifact" convention, the "don't split what you can't contract ⇒ single-session" rule) followed by ONE fenced ```json example: a two-repo manifest with two tasks where `web` needs an `interface` artifact produced by `api`, each task with a real `check.cmd`, non-empty `authorization`, and the produced-artifact ids referenced by `needs`. Keep the whole doc under ~6 KB. Add `WriteManifestSchemaDoc(notesDir)`.

- [ ] **Step 4: Run it — verify it passes.** Iterate the example JSON until `LoadAll` returns zero errors (this is the whole point of the task).

Run: `go test ./internal/orchestrator/ -run TestManifestSchemaDocExampleLoads`
Expected: PASS.

- [ ] **Step 5: Wire the writer into `Spawn`.** In `spawn.go` `Spawn`, after `notesDir` is materialized (line ~130) and before/around `assembleAndWrite`, call `WriteManifestSchemaDoc(notesDir)`; ignore-with-log on error (a missing schema doc must not fail the launch — the brief still references it by name). Confirm `go build ./...`.

- [ ] **Step 6: Commit.**

```bash
git add internal/orchestrator/manifestdoc.go internal/orchestrator/manifestdoc_test.go internal/orchestrator/spawn.go
git commit -m "feat(orchestrator): ship manifest-schema doc whose example provably loads"
```

---

### Task 2: Rewrite the brief — write scope + "What to do" for manifest authoring

**Files:**
- Modify: `internal/orchestrator/assemble.go` — `scopeSection` (~lines 170-188) and `whatSection`.
- Modify: `internal/orchestrator/assemble_test.go` — the golden/brief assertions.

**Interfaces:**
- Consumes: `Input{ProjectName, Root, NotesDir, Repos, ...}` (unchanged); `WriteManifestSchemaDoc` writes `loom-manifest-schema.md` into `NotesDir` (Task 1), which the brief references by name.
- Produces: an assembled brief whose Authorization scope names `<Root>/.loom/manifests/` as writable and whose "What to do" instructs manifest authoring + the single-session fallback.

- [ ] **Step 1: Write the failing test.** READ the current `scopeSection`/`whatSection` and the existing brief test first, then add assertions.

```go
// in internal/orchestrator/assemble_test.go — add to the existing brief test file
func TestBriefInstructsManifestAuthoring(t *testing.T) {
	in := Input{
		ProjectName: "atlas", Root: "/tmp/atlas",
		NotesDir: "/tmp/atlas/.loom/notes",
		Repos:    []RepoState{{Label: "api", Path: "/tmp/atlas/api"}, {Label: "web", Path: "/tmp/atlas/web"}},
		Intent:   "add export-to-pdf across api and web",
		Now:      fixedNow(t), // reuse the test file's existing fixed clock helper
	}
	b := Assemble(in).Text
	// write scope widened by exactly the manifests dir
	if !strings.Contains(b, "/tmp/atlas/.loom/manifests") {
		t.Fatal("scope must name the writable manifests dir")
	}
	// the no-spawn invariant is KEPT
	if !strings.Contains(b, "may not start, resume, or kill other sessions") {
		t.Fatal("the no-spawn invariant must be retained")
	}
	// the old 'delegation does not exist yet' clause is GONE
	if strings.Contains(b, "Delegation does not exist yet") {
		t.Fatal("the delegation-disabled clause must be removed")
	}
	// What-to-do instructs manifest authoring, references the schema doc, and the single-session fallback
	for _, want := range []string{"loom-manifest-schema.md", ".loom/manifests/", "single session"} {
		if !strings.Contains(b, want) {
			t.Fatalf("What to do must mention %q", want)
		}
	}
}
```

- [ ] **Step 2: Run it — verify it fails.** Run: `go test ./internal/orchestrator/ -run TestBriefInstructsManifestAuthoring` — FAIL.

- [ ] **Step 3: Edit `scopeSection`.** Change the writable-directories bullet to name BOTH the notes dir (unchanged) AND `filepath.Join(in.Root, ".loom", "manifests")`. KEEP verbatim: *"You may not start, resume, or kill other sessions."* and *"If you believe you need something outside this scope, say so and stop."* REMOVE only the *"Delegation does not exist yet — … write the split into loom-open.md and stop."* clause.

- [ ] **Step 4: Edit `whatSection`.** Replace/extend the standing instruction so it reads (substance, adapt to the section's existing voice): decompose the intent into a delegation manifest at `<root>/.loom/manifests/<name>.json` following `loom-manifest-schema.md`; for each dependency between slices, write a contract (a human sentence in both briefs + an `interface` artifact) and a check for the producing seam; **if the work does not decompose into clean, separable, multi-repo slices, do NOT invent splits — write a one-line note that this is a single-session job and stop.** Keep it inside `whatCap` (4 KB) — it references the schema doc rather than inlining the schema.

- [ ] **Step 5: Update the golden brief.** If the test file carries a golden-string/file, regenerate it and eyeball the diff (scope bullet changed, What-to-do rewritten). Run the FULL orchestrator suite: `go test ./internal/orchestrator/` — all green (deterministic brief preserved).

- [ ] **Step 6: Commit.**

```bash
git add internal/orchestrator/assemble.go internal/orchestrator/assemble_test.go
git commit -m "feat(orchestrator): brief authors a manifest and dispatches (keeps human-gated spawn)"
```

---

### Task 3: Child briefs instruct subagent-driven development

**Files:**
- Modify: `internal/delegate/spawn.go` — `Brief(run store.DelegationRun, m Manifest, t Task, c Created, addDirs []string) string` (~line 565).
- Modify: `internal/delegate/spawn_test.go` — the brief-content test (or add one).

**Interfaces:**
- Consumes: the existing `Brief(...)` signature — do NOT change it.
- Produces: a child brief whose text instructs the child to execute its slice using subagent-driven development.

- [ ] **Step 1: Write the failing test.** READ the existing `Brief` and any brief test first.

```go
// internal/delegate/spawn_test.go
func TestChildBriefInstructsSDD(t *testing.T) {
	// Build a minimal run/manifest/task/created using the same fixtures the
	// other spawn tests use (copy their setup helper), then:
	b := Brief(run, m, task, created, nil)
	if !strings.Contains(strings.ToLower(b), "subagent-driven development") {
		t.Fatal("child brief must instruct subagent-driven development")
	}
}
```

- [ ] **Step 2: Run it — verify it fails.** Run: `go test ./internal/delegate/ -run TestChildBriefInstructsSDD` — FAIL.

- [ ] **Step 3: Add the instruction.** In `Brief`, append a short standing paragraph: the child owns exactly its slice; it must build against the contracts named in its brief; and it should **execute the slice using subagent-driven development** (decompose its slice into tasks, implement+test each, self-review). Do not touch the authorization/scope/contract text already assembled — append after it.

- [ ] **Step 4: Run it — verify it passes, and the delegate suite is green.** Run: `go test ./internal/delegate/` — all green.

- [ ] **Step 5: Commit.**

```bash
git add internal/delegate/spawn.go internal/delegate/spawn_test.go
git commit -m "feat(delegate): child briefs instruct subagent-driven development"
```

---

## After this plan (manual end-to-end smoke — the "test it out")

Not a task; the acceptance check. On a real two-repo project: spawn an orchestrator with an intent → confirm it writes a manifest under `.loom/manifests/` that `Start run…` loads → approve a ready task → confirm a child spawns in a worktree and its brief tells it to do SDD. Failures here (invalid manifest, no manifest, orchestrator refuses to write) feed **Plan 2 (safety net: plan-review, manifest-repair loop, single-session fallback, abandon-run, batch-approve, stall/stop, budget)**; the honest-"done" rigor (consumer-driven seam checks, human-review leaf terminal, degenerate-check rejection) is **Plan 3**.

## Self-Review

- **Spec coverage:** this plan implements spec §5.1 (authorship: write-scope rewrite kept-no-spawn, schema via reference doc, single-session fallback instruction) and §5.3 (children do SDD). It intentionally does NOT cover the re-author loop, plan-review, safety net (§7 → Plan 2) or the rigor state-machine (§3 → Plan 3); the acceptance smoke names them as follow-ups. No gap silently dropped.
- **Placeholder scan:** none — each task carries real test code and precise edit targets; the two "READ the current function first" steps are grounding, not deferrals (the functions are small and located).
- **Type consistency:** `ManifestSchemaDoc`/`WriteManifestSchemaDoc` used identically in Task 1 and referenced by name (as a filename) in Task 2; `delegate.LoadAll`/`delegate.NewResolver` signatures match the code read during planning; `Brief(...)` signature is explicitly unchanged.
