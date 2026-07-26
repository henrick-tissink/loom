# Flashcards — Slice 6a Implementation Plan (regenerate core, headless)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** The verified, headless foundation for generate/regenerate-from-the-GUI: a store primitive that deletes a part's cards (replace semantics — user's choice), a `Pipeline.RegeneratePart` that deletes-then-authors, and a query for which parts have stale cards (to drive "regenerate all stale"). The async Wails bridge + Study-surface affordances are Slice 6b (controller-driven).

**Architecture:** `store.DeleteCardsForPart` cascades to reviews + log in one transaction (mirrors `DeleteCard`). `store.StalePartsForProject` returns the distinct parts carrying a stale card. `flashcards.Pipeline.RegeneratePart` composes delete + the existing `GenerateForPart` (which keeps the hardened author + cited-source verify gate). No schema change.

**Tech Stack:** Go 1.26, modernc SQLite. Tests seed a store + a fake `claude` (the established pattern).

## Global Constraints

- Module `github.com/henricktissink/loom`.
- **Replace semantics** (user's decision): regenerating a part DELETES its existing cards — and their review/log history — then authors a fresh batch. Review progress on the old cards is intentionally discarded. Generate (a part with 0 cards) and regenerate go through the SAME path; the delete is a no-op when the part has no cards.
- `DeleteCardsForPart` is atomic (one `tx`), cascading `flashcard_review_log` → `flashcard_reviews` → `flashcards` (children before parent — same order/rationale as `DeleteCard`).
- Reuse `Pipeline`/`GenerateForPart` (Slice 1), `store` helpers. No new third-party deps. No schema change.

---

### Task 1: Store — delete-by-part + stale-parts query

**Files:**
- Create: `internal/store/flashcard_regen.go`
- Test: `internal/store/flashcard_regen_test.go`

**Interfaces:**
- Produces:
  - `func (s *Store) DeleteCardsForPart(project, part string) (deleted int, err error)` — atomic cascade delete of a part's cards + their reviews + log; returns the card count deleted.
  - `func (s *Store) StalePartsForProject(project string) ([]string, error)` — distinct `part` values that have at least one `status='stale'` card, ordered by part.

- [ ] **Step 1: Write the failing test**

```go
package store

import "testing"

func TestDeleteCardsForPartCascadesAndCounts(t *testing.T) {
	st := openTestStore(t)
	a1 := seedCard(t, st, "p", "a.go", "a1", "active")
	a2 := seedCard(t, st, "p", "a.go", "a2", "active")
	b1 := seedCard(t, st, "p", "b.go", "b1", "active") // different part — must survive
	// give a1 a review row + a log row, to prove the cascade
	if err := st.PutReview(ReviewRow{CardID: a1, Ease: 2.5}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendReviewLog(a1, 3, true, 100); err != nil {
		t.Fatal(err)
	}

	deleted, err := st.DeleteCardsForPart("p", "a.go")
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteCardsForPart = %d err=%v, want 2", deleted, err)
	}
	cards, _ := st.FlashcardsForProject("p")
	if len(cards) != 1 || cards[0].ID != b1 {
		t.Fatalf("only b.go's card should remain: %+v", cards)
	}
	if _, ok, _ := st.GetReview(a1); ok {
		t.Fatalf("a1's review row not cascaded")
	}
	var logRows int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM flashcard_review_log WHERE card_id=?", a1).Scan(&logRows); err != nil {
		t.Fatal(err)
	}
	if logRows != 0 {
		t.Fatalf("a1's log rows not cascaded: %d", logRows)
	}
	_ = a2
}

func TestStalePartsForProject(t *testing.T) {
	st := openTestStore(t)
	seedCard(t, st, "p", "a.go", "a1", "active")
	s1 := seedCard(t, st, "p", "a.go", "a2", "active")
	s2 := seedCard(t, st, "p", "b.go", "b1", "active")
	seedCard(t, st, "p", "c.go", "c1", "draft") // draft, not stale
	st.SetCardStatus(s1, "stale", 1)
	st.SetCardStatus(s2, "stale", 1)

	parts, err := st.StalePartsForProject("p")
	if err != nil {
		t.Fatalf("StalePartsForProject: %v", err)
	}
	if len(parts) != 2 || parts[0] != "a.go" || parts[1] != "b.go" {
		t.Fatalf("stale parts = %v, want [a.go b.go] (distinct, ordered)", parts)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'DeleteCardsForPart|StaleParts' -v`
Expected: FAIL — undefined methods.

- [ ] **Step 3: Implement** (`internal/store/flashcard_regen.go`)

```go
package store

// DeleteCardsForPart removes every card of one project part — and its review
// state and log rows — in a single transaction (replace semantics: a regenerate
// wipes the part before re-authoring). Children are deleted before the parent,
// the same order DeleteCard uses. Returns the number of cards deleted.
func (s *Store) DeleteCardsForPart(project, part string) (deleted int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	sub := "SELECT id FROM flashcards WHERE project=? AND part=?"
	for _, q := range []string{
		"DELETE FROM flashcard_review_log WHERE card_id IN (" + sub + ")",
		"DELETE FROM flashcard_reviews WHERE card_id IN (" + sub + ")",
	} {
		if _, err = tx.Exec(q, project, part); err != nil {
			return 0, err
		}
	}
	res, e := tx.Exec("DELETE FROM flashcards WHERE project=? AND part=?", project, part)
	if e != nil {
		err = e
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// StalePartsForProject returns the distinct parts that carry at least one stale
// card, ordered by part — the set a "regenerate all stale" action iterates.
func (s *Store) StalePartsForProject(project string) ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT part FROM flashcards WHERE project=? AND status='stale' ORDER BY part", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -run 'DeleteCardsForPart|StaleParts' -v` then `go test ./internal/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/flashcard_regen.go internal/store/flashcard_regen_test.go
git commit -m "feat(flashcards): DeleteCardsForPart (cascade) + StalePartsForProject"
```

---

### Task 2: Pipeline.RegeneratePart

**Files:**
- Modify: `internal/flashcards/cli.go` (add `RegeneratePart` to the `Pipeline` type)
- Test: `internal/flashcards/regen_test.go`

**Interfaces:**
- Consumes: `store.DeleteCardsForPart` (Task 1), `Pipeline.GenerateForPart` (Slice 1), `Part`.
- Produces:
  - `func (pl *Pipeline) RegeneratePart(project string, p Part, now int64) (deleted, stored, rejected int, err error)` — deletes the part's existing cards, then generates + verifies + stores a fresh batch. (Generate = regenerate with nothing to delete.)

- [ ] **Step 1: Write the failing test**

```go
package flashcards

import (
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func TestRegeneratePartReplaces(t *testing.T) {
	st := openStore(t)
	// an existing (stale-ish) card for the part, plus its review row
	old, _, err := st.InsertFlashcard(store.Flashcard{
		Project: "p", Part: "internal/status/status.go", Anchor: "old", StemHash: "old", Type: "code",
		Front: "old q", Back: "old a", Status: "active", CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	st.PutReview(store.ReviewRow{CardID: old, Ease: 2.5})

	// fake claude authors one valid code card; verifier accepts it
	pl := &Pipeline{
		Store: st,
		Gen:   &Generator{Binary: fakeBin(t, fakeClaudeCards), WorkDir: t.TempDir()},
		Ver:   &Verifier{Binary: fakeBin(t, fakeClaudeVerdictYes), WorkDir: t.TempDir()},
	}
	p := Part{Kind: PartCode, ID: "internal/status/status.go", Title: "status.go",
		SourceRef: "internal/status/status.go", Source: "func Fuse() int { return 1 }"}

	deleted, stored, rejected, err := pl.RegeneratePart("p", p, 100)
	if err != nil {
		t.Fatalf("RegeneratePart: %v", err)
	}
	if deleted != 1 || stored != 1 || rejected != 0 {
		t.Fatalf("deleted=%d stored=%d rejected=%d, want 1/1/0", deleted, stored, rejected)
	}
	// the old card (and its review) are gone; exactly one fresh draft remains
	cards, _ := st.FlashcardsForProject("p")
	if len(cards) != 1 || cards[0].ID == old || cards[0].Status != "draft" {
		t.Fatalf("after regen: %+v (old should be gone, one fresh draft)", cards)
	}
	if _, ok, _ := st.GetReview(old); ok {
		t.Fatalf("old review row not cascaded on regenerate")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flashcards/ -run RegeneratePartReplaces -v`
Expected: FAIL — `RegeneratePart` undefined.

- [ ] **Step 3: Implement** (append to `internal/flashcards/cli.go`, near `GenerateForPart`)

```go
// RegeneratePart replaces a part's cards: it deletes the part's existing cards
// (and their review history — replace semantics) and then authors a fresh,
// verified batch. Generating a part that has no cards is the same call with
// nothing to delete. Returns how many were deleted, stored, and rejected.
func (pl *Pipeline) RegeneratePart(project string, p Part, now int64) (deleted, stored, rejected int, err error) {
	deleted, err = pl.Store.DeleteCardsForPart(project, p.ID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("regenerate %s: clear old cards: %w", p.ID, err)
	}
	stored, rejected, err = pl.GenerateForPart(project, p, now)
	return deleted, stored, rejected, err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/flashcards/ -run RegeneratePartReplaces -v` then `go test ./internal/flashcards/ ./internal/store/` and `go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/flashcards/cli.go internal/flashcards/regen_test.go
git commit -m "feat(flashcards): Pipeline.RegeneratePart — replace a part's cards"
```

---

## Self-Review

**Spec coverage (Slice 6a — the headless core for GUI generate/regenerate):**
- Replace semantics (delete a part's cards, cascade) → Task 1 `DeleteCardsForPart`. ✓
- Regenerate = delete + author fresh, keeping the verify gate → Task 2 `RegeneratePart` (reuses `GenerateForPart`). ✓
- Drive "regenerate all stale" → Task 1 `StalePartsForProject`. ✓
- Deferred to Slice 6b (controller-driven): the async Wails bridge (`App.FlashcardGenerate` goroutine + `flashcards:progress` events + one-job guard) and the Study-surface affordances (per-part generate/regenerate buttons, the stale banner, live progress). Not headless-testable — correctly out.

**Placeholder scan:** No TBD/TODO; complete code and commands in every step.

**Type consistency:** `DeleteCardsForPart(project, part)` (Task 1) is consumed by `RegeneratePart` (Task 2) with `p.ID` as the part. `GenerateForPart`'s `(stored, rejected, err)` shape is preserved and extended with `deleted`. `fakeClaudeCards`/`fakeClaudeVerdictYes`/`fakeBin`/`openStore` are the existing test helpers. `StalePartsForProject` reads `status='stale'`, the status `CheckStale` sets.
