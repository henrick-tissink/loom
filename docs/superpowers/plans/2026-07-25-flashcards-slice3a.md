# Flashcards — Slice 3a Implementation Plan (Wails bridge)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Expose the Slice 1/2 flashcards engine to the GUI through headless-testable Wails `App` bridge methods (coverage, review queue, grade, curation, stats) — the Go foundation the frontend Study surface (Slice 3b) will call. No frontend, no window.

**Architecture:** One new file `cmd/loom-gui/flashcards.go` adds DTOs (JSON-tagged for JS) and `App` methods that wrap `flashcards.Reviewer` + store primitives, built inline from the App's existing `a.st`/`a.now`. Follows the established loom-gui bridge idiom: nil-guard `a.st`, `recover()` to degrade reads to empty, non-nil slices, `error` returns on mutations. No change to `App`/`newApp`/`main.go`.

**Tech Stack:** Go 1.26, Wails v2 (methods auto-bound; JS bindings regenerate at `wails build`, out of scope here). Tests open a temp-dir `store.Store` and construct `newApp(nil, tmux.New(), st, nil, nil, now)` — the `LAUNCHER-T1`/`projects_test.go` pattern.

## Global Constraints

- Package `main` in `cmd/loom-gui`. Import `internal/flashcards` and `internal/store`.
- Bridge idiom (match `App.ListRecent`/`ListProjects`): a READ method returns a NON-nil slice/struct initialized `out := []T{}`, has `defer func() { _ = recover() }()`, and `if a.st == nil { return out }` before any store call. A MUTATION returns `error` (like `App.LaunchSession`/`AttachSession`) and returns a sentinel when `a.st == nil`.
- All timestamps come from `a.now().Unix()` — never `time.Now()` directly (tests inject a fixed clock).
- Reuse, do not reimplement: `flashcards.Reviewer`/`DefaultReviewConfig`/`Coverage`/`PassRate`/`BuildQueue`/`Record`, `flashcards.Grade`/`ValidGrade`/`StemHash`/`Hash`, and store methods `DraftsForProject`/`DueReviewCards`/`SetCardStatus`/`EditCardText`/`DeleteCard`. Only `active` cards are scheduled (the store enforces this).
- JSON tags are camelCase (`json:"wasDue"`) — the frontend consumes these.
- No new third-party dependencies. No change to `newApp` signature or `cmd/loom-gui/main.go`.
- Out of scope (Slice 3b / later): the frontend Study surface; generate-cards-from-GUI (generation stays the CLI's hardened `claude -p`); FSRS.

---

### Task 1: Read bridge — DTOs, coverage, stats, drafts, queue

**Files:**
- Create: `cmd/loom-gui/flashcards.go`
- Test: `cmd/loom-gui/flashcards_test.go`

**Interfaces:**
- Produces:
  - `type FlashcardDTO struct { ID int64; Part, Type, Front, Back, Status string; WasDue bool }` (JSON-tagged).
  - `type CoverageDTO struct { Part string; Total, Active, Draft, Due int }`.
  - `type FlashcardStatsDTO struct { PassRate float64; Reviews int; Parts []CoverageDTO }`.
  - `func (a *App) reviewer() *flashcards.Reviewer` (inline construct from `a.st`).
  - `func (a *App) FlashcardCoverage(project string) []CoverageDTO`
  - `func (a *App) FlashcardStats(project string) FlashcardStatsDTO`
  - `func (a *App) FlashcardDrafts(project string) []FlashcardDTO`
  - `func (a *App) FlashcardQueue(project string) []FlashcardDTO` (each card `WasDue`-marked from the due set).
  - `func cardDTO(c store.Flashcard, wasDue bool) FlashcardDTO`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

func newFlashApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := newApp(nil, tmux.New(), st, nil, nil, func() time.Time { return time.Unix(1000, 0) })
	return app, st
}

func seedFCard(t *testing.T, st *store.Store, part, anchor, status string) int64 {
	t.Helper()
	id, ins, err := st.InsertFlashcard(store.Flashcard{
		Project: "p", Part: part, Anchor: anchor, StemHash: anchor, Type: "code",
		Front: "q " + anchor, Back: "a", Status: status, CreatedAt: 1,
	})
	if err != nil || !ins {
		t.Fatalf("seed %s: %v", anchor, err)
	}
	return id
}

func TestFlashcardReadBridge_nilStoreEmpty(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if got := app.FlashcardCoverage("p"); got == nil || len(got) != 0 {
		t.Fatalf("nil-store Coverage = %v, want empty non-nil", got)
	}
	if got := app.FlashcardQueue("p"); got == nil || len(got) != 0 {
		t.Fatalf("nil-store Queue = %v, want empty non-nil", got)
	}
	if got := app.FlashcardDrafts("p"); got == nil || len(got) != 0 {
		t.Fatalf("nil-store Drafts = %v, want empty non-nil", got)
	}
	s := app.FlashcardStats("p")
	if s.Parts == nil || s.Reviews != 0 || s.PassRate != 0 {
		t.Fatalf("nil-store Stats = %+v, want zero with non-nil Parts", s)
	}
}

func TestFlashcardReadBridge_coverageDraftsQueue(t *testing.T) {
	app, st := newFlashApp(t)
	a1 := seedFCard(t, st, "a.go", "a1", "active") // will be due
	seedFCard(t, st, "a.go", "a2", "active")        // new (never reviewed)
	seedFCard(t, st, "a.go", "a3", "draft")         // draft
	st.PutReview(store.ReviewRow{CardID: a1, DueAt: 500, Reps: 1})

	cov := app.FlashcardCoverage("p")
	if len(cov) != 1 || cov[0].Part != "a.go" || cov[0].Total != 3 || cov[0].Active != 2 || cov[0].Draft != 1 || cov[0].Due != 1 {
		t.Fatalf("Coverage = %+v", cov)
	}
	drafts := app.FlashcardDrafts("p")
	if len(drafts) != 1 || drafts[0].Status != "draft" {
		t.Fatalf("Drafts = %+v", drafts)
	}
	q := app.FlashcardQueue("p") // now=1000: a1 due (500<=1000) + a2 new
	if len(q) != 2 {
		t.Fatalf("Queue len = %d, want 2", len(q))
	}
	var dueMarked, newMarked int
	for _, c := range q {
		if c.ID == a1 && c.WasDue {
			dueMarked++
		}
		if !c.WasDue {
			newMarked++
		}
	}
	if dueMarked != 1 || newMarked != 1 {
		t.Fatalf("Queue WasDue marking wrong: %+v", q)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/loom-gui/ -run FlashcardReadBridge -v`
Expected: FAIL — `undefined: App.FlashcardCoverage` etc.

- [ ] **Step 3: Write the implementation**

```go
package main

import (
	"github.com/henricktissink/loom/internal/flashcards"
	"github.com/henricktissink/loom/internal/store"
)

// FlashcardDTO is one study card for the GUI. WasDue marks a card drawn from the
// due set (a retention re-test) rather than a newly introduced card; the grade
// call passes it back so the review log records due-vs-first-exposure honestly.
type FlashcardDTO struct {
	ID     int64  `json:"id"`
	Part   string `json:"part"`
	Type   string `json:"type"`
	Front  string `json:"front"`
	Back   string `json:"back"`
	Status string `json:"status"`
	WasDue bool   `json:"wasDue"`
}

// CoverageDTO is per-part card counts for the coverage map (no "mastery %").
type CoverageDTO struct {
	Part   string `json:"part"`
	Total  int    `json:"total"`
	Active int    `json:"active"`
	Draft  int    `json:"draft"`
	Due    int    `json:"due"`
}

// FlashcardStatsDTO is the project's measured pass-rate (over due reviews) and coverage.
type FlashcardStatsDTO struct {
	PassRate float64       `json:"passRate"` // 0..1
	Reviews  int           `json:"reviews"`
	Parts    []CoverageDTO `json:"parts"`
}

// reviewer builds a Reviewer over the app's store with default session config.
func (a *App) reviewer() *flashcards.Reviewer {
	return &flashcards.Reviewer{Store: a.st, Cfg: flashcards.DefaultReviewConfig()}
}

// FlashcardCoverage returns per-part card counts for a project.
func (a *App) FlashcardCoverage(project string) []CoverageDTO {
	out := []CoverageDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	cov, err := a.reviewer().Coverage(project, a.now().Unix())
	if err != nil {
		return out
	}
	for _, c := range cov {
		out = append(out, CoverageDTO{Part: c.Part, Total: c.Total, Active: c.Active, Draft: c.Draft, Due: c.Due})
	}
	return out
}

// FlashcardStats returns the measured pass-rate (over due reviews) plus coverage.
func (a *App) FlashcardStats(project string) FlashcardStatsDTO {
	out := FlashcardStatsDTO{Parts: []CoverageDTO{}}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	rate, n, err := a.reviewer().PassRate(project, 0)
	if err != nil {
		return out
	}
	out.PassRate = rate
	out.Reviews = n
	out.Parts = a.FlashcardCoverage(project)
	return out
}

// FlashcardDrafts returns a project's uncurated cards.
func (a *App) FlashcardDrafts(project string) []FlashcardDTO {
	out := []FlashcardDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	drafts, err := a.st.DraftsForProject(project)
	if err != nil {
		return out
	}
	for _, c := range drafts {
		out = append(out, cardDTO(c, false))
	}
	return out
}

// FlashcardQueue returns the review-session queue (due cards, then capped new
// cards, interleaved), each marked WasDue from the due set.
func (a *App) FlashcardQueue(project string) []FlashcardDTO {
	out := []FlashcardDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	now := a.now().Unix()
	dayStart := now - now%86400
	due, err := a.st.DueReviewCards(project, now, 1000)
	if err != nil {
		return out
	}
	dueIDs := make(map[int64]bool, len(due))
	for _, c := range due {
		dueIDs[c.ID] = true
	}
	queue, err := a.reviewer().BuildQueue(project, now, dayStart)
	if err != nil {
		return out
	}
	for _, c := range queue {
		out = append(out, cardDTO(c, dueIDs[c.ID]))
	}
	return out
}

func cardDTO(c store.Flashcard, wasDue bool) FlashcardDTO {
	return FlashcardDTO{ID: c.ID, Part: c.Part, Type: c.Type, Front: c.Front, Back: c.Back, Status: c.Status, WasDue: wasDue}
}
```

Fix the test's loose `drafts[0]` assertion to simply check `drafts[0].Status == "draft"` (drop the `seedDraftID` helper if the implementer finds it awkward — it's a stand-in; the real assertion is on Status). Keep the test meaningful.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/loom-gui/ -run FlashcardReadBridge -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/loom-gui/flashcards.go cmd/loom-gui/flashcards_test.go
git commit -m "feat(gui): flashcards read bridge — coverage, stats, drafts, queue"
```

---

### Task 2: Mutation bridge — grade, curation, edit, kill

**Files:**
- Modify: `cmd/loom-gui/flashcards.go` (add mutation methods)
- Modify: `cmd/loom-gui/flashcards_test.go` (add mutation tests)

**Interfaces:**
- Consumes: `reviewer()`/`cardDTO` (Task 1); `flashcards.Grade`/`ValidGrade`/`StemHash`/`Hash`; store `SetCardStatus`/`DraftsForProject`/`EditCardText`/`DeleteCard`.
- Produces:
  - `func (a *App) FlashcardGrade(cardID int64, grade int, wasDue bool) (suspended bool, err error)` — validates grade, calls `Reviewer.Record`.
  - `func (a *App) FlashcardActivate(cardID int64) error`
  - `func (a *App) FlashcardActivateAll(project string) (int, error)`
  - `func (a *App) FlashcardEdit(cardID int64, front, back string) error` — recomputes stem/answer hashes.
  - `func (a *App) FlashcardKill(cardID int64) error`

- [ ] **Step 1: Write the failing test**

```go
func TestFlashcardMutationBridge(t *testing.T) {
	app, st := newFlashApp(t)
	draft := seedFCard(t, st, "a.go", "m1", "draft")

	// invalid grade rejected; nil-store sentinels
	nilApp := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if _, err := nilApp.FlashcardGrade(1, 3, false); err == nil {
		t.Fatal("nil-store grade should error")
	}
	if err := nilApp.FlashcardActivate(1); err == nil {
		t.Fatal("nil-store activate should error")
	}

	// activate the draft
	if err := app.FlashcardActivate(draft); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if d := app.FlashcardDrafts("p"); len(d) != 0 {
		t.Fatalf("draft still listed after activate: %+v", d)
	}

	// grade validation
	if _, err := app.FlashcardGrade(draft, 9, false); err == nil {
		t.Fatal("invalid grade 9 should error")
	}
	// a real grade records a review
	if _, err := app.FlashcardGrade(draft, 3, false); err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if _, ok, _ := st.GetReview(draft); !ok {
		t.Fatal("grade did not record a review row")
	}

	// edit recomputes hashes
	if err := app.FlashcardEdit(draft, "new front", "new back"); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cards, _ := st.FlashcardsForProject("p")
	if cards[0].Front != "new front" || cards[0].StemHash == "m1" || cards[0].AnswerHash == "" {
		t.Fatalf("edit not applied/hashed: %+v", cards[0])
	}

	// activate-all count
	seedFCard(t, st, "b.go", "m2", "draft")
	seedFCard(t, st, "b.go", "m3", "draft")
	if n, err := app.FlashcardActivateAll("p"); err != nil || n != 2 {
		t.Fatalf("ActivateAll = %d err=%v, want 2", n, err)
	}

	// kill removes the card
	if err := app.FlashcardKill(draft); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	for _, c := range mustCards(t, st) {
		if c.ID == draft {
			t.Fatal("killed card still present")
		}
	}
}

func mustCards(t *testing.T, st *store.Store) []store.Flashcard {
	t.Helper()
	c, err := st.FlashcardsForProject("p")
	if err != nil {
		t.Fatal(err)
	}
	return c
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/loom-gui/ -run FlashcardMutationBridge -v`
Expected: FAIL — `undefined: App.FlashcardGrade` etc.

- [ ] **Step 3: Write the implementation** (append to `cmd/loom-gui/flashcards.go`; add `"errors"` and `"fmt"` imports)

```go
var errFlashNoStore = errors.New("flashcards: no store")

// FlashcardGrade records a grade (1..4) for a card and reports whether the card
// was auto-suspended as a leech. wasDue must be the value the queue reported for
// this card (retention vs. first-exposure), so the review log stays honest.
func (a *App) FlashcardGrade(cardID int64, grade int, wasDue bool) (suspended bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			suspended, err = false, fmt.Errorf("flashcards: grade panicked: %v", r)
		}
	}()
	if a.st == nil {
		return false, errFlashNoStore
	}
	g := flashcards.Grade(grade)
	if !flashcards.ValidGrade(g) {
		return false, fmt.Errorf("flashcards: invalid grade %d", grade)
	}
	return a.reviewer().Record(cardID, g, wasDue, a.now().Unix())
}

// FlashcardActivate curates one draft card into the active deck.
func (a *App) FlashcardActivate(cardID int64) error {
	if a.st == nil {
		return errFlashNoStore
	}
	return a.st.SetCardStatus(cardID, "active", a.now().Unix())
}

// FlashcardActivateAll activates every draft in a project and returns the count.
func (a *App) FlashcardActivateAll(project string) (int, error) {
	if a.st == nil {
		return 0, errFlashNoStore
	}
	drafts, err := a.st.DraftsForProject(project)
	if err != nil {
		return 0, err
	}
	now := a.now().Unix()
	n := 0
	for _, c := range drafts {
		if err := a.st.SetCardStatus(c.ID, "active", now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// FlashcardEdit rewrites a card's text, recomputing its stem and answer hashes.
func (a *App) FlashcardEdit(cardID int64, front, back string) error {
	if a.st == nil {
		return errFlashNoStore
	}
	return a.st.EditCardText(cardID, front, back, flashcards.StemHash(front), flashcards.Hash(back), a.now().Unix())
}

// FlashcardKill deletes a card and its review history (curation reject).
func (a *App) FlashcardKill(cardID int64) error {
	if a.st == nil {
		return errFlashNoStore
	}
	return a.st.DeleteCard(cardID)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/loom-gui/ -run FlashcardMutationBridge -v` then `go test ./cmd/loom-gui/` (full package, no regressions) and `go build ./...`.
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add cmd/loom-gui/flashcards.go cmd/loom-gui/flashcards_test.go
git commit -m "feat(gui): flashcards mutation bridge — grade, curation, edit, kill"
```

---

## Self-Review

**Spec coverage (Slice 3a scope — the GUI backend the Study surface calls):**
- Coverage map data (`FlashcardCoverage`) + honest measured stats (`FlashcardStats`, pass-rate over due reviews) → Task 1. ✓
- Review session (`FlashcardQueue` with WasDue marking; `FlashcardGrade` → SM-2 record, leech suspend surfaced) → Tasks 1/2. ✓
- Curation (`FlashcardDrafts` list; `FlashcardActivate`/`FlashcardActivateAll`/`FlashcardEdit` hash-recompute/`FlashcardKill`) → Tasks 1/2. ✓
- Deferred (Slice 3b / later): the frontend rendering; generate-from-GUI; FSRS. Correctly out.

**Placeholder scan:** No TBD/TODO. (Task 1's test has a loose `seedDraftID` stand-in — Step 3 instructs the implementer to simplify that assertion to `Status == "draft"`; not a placeholder in shipped code.)

**Type consistency:** `FlashcardDTO`/`CoverageDTO`/`FlashcardStatsDTO` (Task 1) consumed unchanged in Task 2. `reviewer()`/`cardDTO` (Task 1) reused by Task 2. Bridge idiom (nil-guard, recover on reads, `error` on mutations) matches `App.ListRecent`/`LaunchSession`. `a.now().Unix()` used for every timestamp. `flashcards.Grade(grade)` cast + `ValidGrade` guard mirrors the CLI's `parseGrade`.

**Idiom compliance:** every read returns a non-nil slice/struct and nil-guards `a.st` before any store call (verified against `ListRecent`); mutations return `error` with a sentinel on nil store (verified against `LaunchSession`/`AttachSession`); no `newApp`/`main.go` change (methods build the Reviewer from `a.st` inline). JSON tags camelCase.

**Note for Slice 3b (frontend):** these methods auto-bind at `wails build` (regenerating `wailsjs/`); the Study surface calls `window.go.main.App.FlashcardQueue(project)` etc. The WasDue flag on each queue card must be passed back verbatim to `FlashcardGrade` so the pass-rate stays honest.
