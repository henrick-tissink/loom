# Flashcards — Slice 5 Implementation Plan (durability: structural hashing + staleness)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make cards know when they've drifted from reality. Structural (gofmt-normalized, comment-stripped) source hashing so only a *meaningful* code change counts, and a `loom flashcards check` reconcile that flags cards whose source changed (stale) or whose source is gone (orphan) — so you know exactly which parts to regenerate.

**Architecture:** `flashcards.StructuralHash` reprints a Go source's AST without comments (via `go/parser`+`go/printer`) and hashes that; non-Go or unparseable sources fall back to the raw hash. The generator uses it for a card's `source_hash`. `flashcards.CheckStale` rebuilds the manifest, compares each active card's stored `source_hash` to its part's current structural hash, and flags drift/orphans via `SetCardStatus(id,"stale")`. A `check` CLI verb runs it and reports. No schema change.

**Tech Stack:** Go 1.26 stdlib (`go/parser`, `go/printer`, `go/token`). Tests seed a `store.Store` + a temp source tree.

## Global Constraints

- Module `github.com/henricktissink/loom`.
- `StructuralHash(sourceRef, source string) string`: if the path before any `#` ends in `.go` AND the source parses, hash the `go/printer` reprint of the AST parsed WITHOUT `ParseComments` (so comments and whitespace are normalized away); otherwise (non-Go, or a parse error — e.g. a file truncated at `maxCodeSource`) fall back to `Hash(source)`.
- **File-level, not symbol-level** (honest scope): a card is stale if *anything* in its file changed. Symbol-level anchoring (spec §8, "key by qualified symbol") needs a per-symbol manifest and is a deferred refinement — say so in a code comment, don't silently imply symbol precision.
- Staleness/orphan flagging sets `status="stale"` via the existing `SetCardStatus`. Stale cards automatically leave the review queue (`DueReviewCards`/`NewActiveCards` filter `status='active'`) — no query change needed. Only ACTIVE cards are checked.
- Reuse `BuildManifest`, `Part`, `store.FlashcardsForProject`, `store.SetCardStatus`, `Hash`. No schema change, no new third-party dependencies.
- Deferred (note honestly): symbol-level anchoring; `answer_hash`-driven re-learn on regeneration (rarely triggers under non-deterministic LLM authoring — a regenerated card gets a new stem, hence a new identity and a fresh schedule anyway); a GUI "N stale — regenerate" surface.

---

### Task 1: Structural source hash + generator wiring

**Files:**
- Create: `internal/flashcards/structhash.go`
- Modify: `internal/flashcards/generate.go` (source_hash uses `StructuralHash`)
- Test: `internal/flashcards/structhash_test.go`

**Interfaces:**
- Produces: `func StructuralHash(sourceRef, source string) string`.
- Change: `generate.go`'s `srcHash := Hash(p.Source)` becomes `srcHash := StructuralHash(p.SourceRef, p.Source)`.

- [ ] **Step 1: Write the failing test**

```go
package flashcards

import "testing"

func TestStructuralHashNormalizesGo(t *testing.T) {
	// same code, different comments + whitespace → same structural hash
	a := "package p\n\n// alpha\nfunc F() int {\n\treturn 1\n}\n"
	b := "package p\nfunc F() int { return 1 } // beta\n"
	if StructuralHash("pkg/x.go", a) != StructuralHash("pkg/x.go", b) {
		t.Fatalf("comment/whitespace edit changed the structural hash")
	}
	// a real code change → different hash
	c := "package p\nfunc F() int { return 2 }\n"
	if StructuralHash("pkg/x.go", a) == StructuralHash("pkg/x.go", c) {
		t.Fatalf("a real code change did not change the hash")
	}
	// non-Go source → raw hash
	if StructuralHash("docs/x.md#slug", "hello world") != Hash("hello world") {
		t.Fatalf("non-Go source should use the raw hash")
	}
	// unparseable Go (e.g. truncated) → raw fallback
	bad := "package p\nfunc F( { oops"
	if StructuralHash("x.go", bad) != Hash(bad) {
		t.Fatalf("unparseable Go should fall back to the raw hash")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flashcards/ -run StructuralHash -v`
Expected: FAIL — `undefined: StructuralHash`.

- [ ] **Step 3a: Implement `structhash.go`**

```go
package flashcards

import (
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
)

// StructuralHash fingerprints a card's source so that only a MEANINGFUL change
// marks its cards stale. For a Go source that parses, it hashes the go/printer
// reprint of the AST parsed WITHOUT comments — so a comment or whitespace edit
// normalizes away and does not churn. Non-Go sources, and Go that does not parse
// (e.g. a file truncated at maxCodeSource), fall back to hashing the raw text.
//
// This is FILE-level, not symbol-level: a card is stale if anything in its file
// changed. Symbol-level anchoring (spec §8, key by qualified symbol) needs a
// per-symbol manifest and is a deferred refinement.
func StructuralHash(sourceRef, source string) string {
	path := sourceRef
	if i := strings.IndexByte(path, '#'); i >= 0 {
		path = path[:i] // doc parts carry "path#slug"
	}
	if !strings.HasSuffix(path, ".go") {
		return Hash(source)
	}
	fset := token.NewFileSet()
	// Mode 0 (no ParseComments): comments are not attached to the AST, so the
	// printer omits them and only the code structure survives.
	f, err := parser.ParseFile(fset, "src.go", source, 0)
	if err != nil {
		return Hash(source)
	}
	var b strings.Builder
	cfg := &printer.Config{Mode: printer.RawFormat, Tabwidth: 8}
	if err := cfg.Fprint(&b, fset, f); err != nil {
		return Hash(source)
	}
	return Hash(b.String())
}
```

- [ ] **Step 3b: Wire the generator**

In `internal/flashcards/generate.go`, change the line

```go
	srcHash := Hash(p.Source)
```

to

```go
	srcHash := StructuralHash(p.SourceRef, p.Source)
```

(Leave everything else — `SourceHash: srcHash`, `AnswerHash: Hash(back)` — unchanged.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/flashcards/ -run 'StructuralHash|Generate' -v` then `go test ./internal/flashcards/`
Expected: PASS (new test + the existing generate test, whose fake source doesn't parse so its `source_hash` is unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/flashcards/structhash.go internal/flashcards/structhash_test.go internal/flashcards/generate.go
git commit -m "feat(flashcards): structural (gofmt-normalized) source hash for staleness"
```

---

### Task 2: Staleness reconcile

**Files:**
- Create: `internal/flashcards/check.go`
- Test: `internal/flashcards/check_test.go`

**Interfaces:**
- Consumes: `BuildManifest`/`Part` (Slice 1), `StructuralHash` (Task 1), `store.FlashcardsForProject`/`SetCardStatus`.
- Produces:
  - `type CheckResult struct { Checked, Stale, Orphan int }`
  - `func CheckStale(st *store.Store, project, projectRoot string, now int64) (CheckResult, error)`

- [ ] **Step 1: Write the failing test**

```go
package flashcards

import (
	"path/filepath"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func seedActive(t *testing.T, st *store.Store, project, part, anchor, srcHash string) int64 {
	t.Helper()
	id, ins, err := st.InsertFlashcard(store.Flashcard{
		Project: project, Part: part, Anchor: anchor, StemHash: anchor, Type: "code",
		Front: "q " + anchor, Back: "a", SourceRef: part, SourceHash: srcHash,
		Status: "active", CreatedAt: 1,
	})
	if err != nil || !ins {
		t.Fatalf("seed %s: %v", anchor, err)
	}
	return id
}

func TestCheckStaleFlagsDriftAndOrphans(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"), "package a\nfunc F() int { return 1 }\n")

	// the current structural hash of a.go's part, from the manifest
	parts, err := BuildManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	var partID, curHash string
	for _, p := range parts {
		if p.Kind == PartCode {
			partID, curHash = p.ID, StructuralHash(p.SourceRef, p.Source)
		}
	}
	if partID == "" {
		t.Fatal("manifest produced no code part")
	}

	fresh := seedActive(t, st, "p", partID, "f1", curHash)   // matches → stays active
	drift := seedActive(t, st, "p", partID, "d1", "OLDHASH") // mismatch → stale
	orphan := seedActive(t, st, "p", "gone.go", "o1", "x")   // part gone → orphan

	res, err := CheckStale(st, "p", root, 1000)
	if err != nil {
		t.Fatalf("CheckStale: %v", err)
	}
	if res.Checked != 3 || res.Stale != 1 || res.Orphan != 1 {
		t.Fatalf("result = %+v, want Checked3 Stale1 Orphan1", res)
	}
	status := map[int64]string{}
	cards, _ := st.FlashcardsForProject("p")
	for _, c := range cards {
		status[c.ID] = c.Status
	}
	if status[fresh] != "active" {
		t.Fatalf("fresh card wrongly flagged: %s", status[fresh])
	}
	if status[drift] != "stale" || status[orphan] != "stale" {
		t.Fatalf("drift=%s orphan=%s, want both stale", status[drift], status[orphan])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flashcards/ -run CheckStale -v`
Expected: FAIL — `undefined: CheckStale`.

- [ ] **Step 3: Implement `check.go`**

```go
package flashcards

import "github.com/henricktissink/loom/internal/store"

// CheckResult summarizes a staleness reconcile.
type CheckResult struct {
	Checked int
	Stale   int // active card whose part's source drifted (source_hash mismatch)
	Orphan  int // active card whose part no longer exists in the manifest
}

// CheckStale reconciles a project's ACTIVE cards against the current source. It
// rebuilds the manifest and flags (status="stale") any active card whose part's
// structural source hash has drifted, or whose part is gone entirely (orphan).
// Stale cards drop out of the review queue until their part is regenerated.
func CheckStale(st *store.Store, project, projectRoot string, now int64) (CheckResult, error) {
	var res CheckResult
	parts, err := BuildManifest(projectRoot)
	if err != nil {
		return res, err
	}
	hashByPart := make(map[string]string, len(parts))
	for _, p := range parts {
		hashByPart[p.ID] = StructuralHash(p.SourceRef, p.Source)
	}
	cards, err := st.FlashcardsForProject(project)
	if err != nil {
		return res, err
	}
	for _, c := range cards {
		if c.Status != "active" {
			continue
		}
		res.Checked++
		cur, ok := hashByPart[c.Part]
		switch {
		case !ok:
			if err := st.SetCardStatus(c.ID, "stale", now); err != nil {
				return res, err
			}
			res.Orphan++
		case cur != c.SourceHash:
			if err := st.SetCardStatus(c.ID, "stale", now); err != nil {
				return res, err
			}
			res.Stale++
		}
	}
	return res, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/flashcards/ -run CheckStale -v` then `go test ./internal/flashcards/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/flashcards/check.go internal/flashcards/check_test.go
git commit -m "feat(flashcards): CheckStale — flag cards whose source drifted or vanished"
```

---

### Task 3: `check` CLI verb

**Files:**
- Modify: `internal/flashcards/cli.go` (add `check` to `RunCLI`'s dispatch + `runCheck`)
- Test: `internal/flashcards/cli_check_test.go`

**Interfaces:**
- Consumes: `CheckStale` (Task 2). `RunCLI` already has `root` (`args[1]`) and `project` in scope.
- Produces:
  - `RunCLI` gains `case "check": return runCheck(st, project, root, now, out)`.
  - `func runCheck(st *store.Store, project, root string, now int64, out io.Writer) error` — runs `CheckStale`, prints a summary and (when anything was flagged) a next-step hint. Usage string updated to include `check`.

- [ ] **Step 1: Write the failing test**

```go
package flashcards

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func TestRunCLICheck(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"), "package a\nfunc F() int { return 1 }\n")
	proj := projectName(root)
	// an active card for a.go carrying a stale source hash
	if _, _, err := st.InsertFlashcard(store.Flashcard{
		Project: proj, Part: "a.go", Anchor: "c1", StemHash: "c1", Type: "code",
		Front: "q", Back: "a", SourceRef: "a.go", SourceHash: "OLD", Status: "active", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := RunCLI([]string{"check", root}, st, "claude", root, 1, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.Contains(buf.String(), "1 stale") {
		t.Fatalf("report = %q, want to mention '1 stale'", buf.String())
	}
	cards, _ := st.FlashcardsForProject(proj)
	if cards[0].Status != "stale" {
		t.Fatalf("card not flagged stale: %s", cards[0].Status)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flashcards/ -run RunCLICheck -v`
Expected: FAIL — unknown verb / `undefined: runCheck`.

- [ ] **Step 3: Implement**

In `internal/flashcards/cli.go`, add the dispatch case (alongside `export`):

```go
	case "check":
		return runCheck(st, project, root, now, out)
```

Add the handler:

```go
// runCheck reconciles a project's active deck against its current source,
// flagging drifted (stale) and vanished (orphan) cards: check <projectRoot>.
func runCheck(st *store.Store, project, root string, now int64, out io.Writer) error {
	res, err := CheckStale(st, project, root, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "checked %d active card(s): %d stale (source changed), %d orphaned (source gone)\n",
		res.Checked, res.Stale, res.Orphan)
	if res.Stale+res.Orphan > 0 {
		fmt.Fprintln(out, "regenerate the affected parts (loom flashcards generate) and curate the fresh cards.")
	}
	return nil
}
```

Update the `RunCLI` usage string and doc comment to include `check`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/flashcards/ && go build ./...`
Expected: PASS + clean build (no regression to other verbs).

- [ ] **Step 5: Commit**

```bash
git add internal/flashcards/cli.go internal/flashcards/cli_check_test.go
git commit -m "feat(flashcards): loom flashcards check — reconcile the deck against its source"
```

---

## Self-Review

**Spec coverage (Slice 5 — durability, spec §8):**
- Structural hashing so only meaningful changes count → Task 1 (`StructuralHash`, comment/whitespace-immune for Go). ✓
- Cards know when they've drifted / vanished → Task 2 (`CheckStale`: stale on hash drift, orphan on missing part). ✓
- Provable from the CLI → Task 3 (`check` verb). ✓
- Stale cards leave the review queue → automatic (queries filter `status='active'`; `CheckStale` sets `stale`). ✓
- Deferred, stated honestly: **symbol-level** anchoring (file-level here — needs a per-symbol manifest); `answer_hash`-driven re-learn on regeneration (rarely fires under non-deterministic authoring); a GUI "N stale" surface. Correctly out.

**Placeholder scan:** No TBD/TODO; complete code and commands in every step.

**Type consistency:** `StructuralHash(sourceRef, source)` (Task 1) is reused by `CheckStale` (Task 2) with the part's `SourceRef`/`Source`. `CheckResult` (Task 2) is consumed by `runCheck` (Task 3). `runCheck` reuses `RunCLI`'s in-scope `root`/`project`. Card `Part` field equals the manifest `Part.ID` (set by `GenerateForPart`), which is the key `CheckStale` maps on. `store.SetCardStatus(id,"stale",now)` matches the existing signature; `'stale'` is an already-defined status that the review queries exclude.

**One migration note for the executor:** cards generated *before* Task 1 stored a raw `Hash(p.Source)` source hash; after Task 1 the generator stores `StructuralHash`, so a first `check` on a pre-Task-1 deck would flag those cards stale (hash scheme changed). This is harmless for a feature with no decks in the wild yet, but worth stating so it isn't mistaken for a bug.
