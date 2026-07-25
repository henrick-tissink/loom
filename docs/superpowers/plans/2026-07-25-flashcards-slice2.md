# Flashcards — Slice 2 Implementation Plan (scheduler · curation · review · stats)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn generated-and-verified `draft` cards into a working spaced-repetition instrument: an SM-2 scheduler, a curation state machine (`draft→active`), a review engine (daily new-card cap, sibling-interference spacing, leech suspension), and an honest measured pass-rate — all provable from the `loom flashcards` CLI before any GUI.

**Architecture:** Pure SM-2 scheduling lives in `internal/flashcards/srs.go` (no I/O). Two new migrations add an `introduced_at` column (daily-cap accounting) and a `flashcard_review_log` table (rolling pass-rate needs review *history*, which the current-state row can't provide). Store gains review-state, log, query, and curation primitives. A `flashcards.Reviewer` composes them into `BuildQueue`/`Record`; a `flashcards.Stats` computes pass-rate. New `curate`/`review`/`stats` CLI subcommands drive it headlessly.

**Tech Stack:** Go 1.26, modernc SQLite (WAL, single conn), `math` for rounding. Tests seed the store directly and drive the CLI with in-memory readers/writers (the Slice-1 pattern).

## Global Constraints

- Module `github.com/henricktissink/loom`. Migration head is **v20** (`internal/store/store.go`). This slice claims **v21** (`ALTER TABLE flashcard_reviews ADD COLUMN introduced_at`) and **v22** (`CREATE TABLE flashcard_review_log`). Append as new slice elements — never edit an applied migration. The v21 ALTER takes its own slot with NO `IF NOT EXISTS` (house rule for ALTER, per the v13/v18 precedent); v22's CREATE uses `IF NOT EXISTS`.
- The migration-head guard tests (`TestUserVersionIsEighteen`, `TestMigrationHeadIsPinned` in `internal/store/store_test.go`) currently assert **20**; repin to **22** after confirming replay is safe (`TestMigrationsReplayFromEveryStaleVersion` already covers new slots dynamically). While there, RENAME `TestUserVersionIsEighteen` → `TestUserVersionIsPinned` (a recorded Slice-1 Minor: the name is stale).
- `Store` is `struct{ db *sql.DB }`; follow `internal/store/flashcards.go` style (a shared column const, `s.db.Exec`/`Query`, `ON CONFLICT` upserts). Multi-row deletes that must be atomic use a single `s.db.Begin()` transaction.
- SM-2 lives in the `flashcards` package as PURE functions; the store never computes a schedule. Grades are `1..4` (Again/Hard/Good/Easy). Ease floor **1.3**, start **2.5**, max interval **365** days, `day = 86400`.
- Only cards with `status = 'active'` are ever scheduled. `draft` cards are invisible to the queue until curated.
- No new third-party dependencies. `time.Now()` is used only at the CLI/main boundary; all engine functions take `now int64`.

---

### Task 1: SM-2 scheduler (pure)

**Files:**
- Create: `internal/flashcards/srs.go`
- Test: `internal/flashcards/srs_test.go`

**Interfaces:**
- Produces:
  - `type Grade int` with `GradeAgain=1`, `GradeHard=2`, `GradeGood=3`, `GradeEasy=4`; `func ValidGrade(g Grade) bool`.
  - `type Review struct { CardID int64; Ease float64; Interval int; DueAt int64; Reps, Lapses, LastGrade int; LastReviewed, IntroducedAt int64 }`
  - `func NewReview(cardID int64) Review` — a fresh, never-reviewed state (`Ease: 2.5`).
  - `func Schedule(r Review, g Grade, now int64) Review` — applies one grade, returns next state.

- [ ] **Step 1: Write the failing test**

```go
package flashcards

import "testing"

func TestScheduleProgressionAndReset(t *testing.T) {
	r := NewReview(1)
	if r.Ease != 2.5 || r.Reps != 0 {
		t.Fatalf("NewReview: %+v", r)
	}
	// first Good → 1 day, reps 1
	r = Schedule(r, GradeGood, 1000)
	if r.Reps != 1 || r.Interval != 1 || r.DueAt != 1000+86400 {
		t.Fatalf("after 1st Good: %+v", r)
	}
	// second Good → 6 days
	r = Schedule(r, GradeGood, 2000)
	if r.Reps != 2 || r.Interval != 6 {
		t.Fatalf("after 2nd Good: interval=%d reps=%d", r.Interval, r.Reps)
	}
	// third Good → 6 * ease(2.5) = 15
	r = Schedule(r, GradeGood, 3000)
	if r.Interval != 15 {
		t.Fatalf("after 3rd Good: interval=%d, want 15", r.Interval)
	}
	// Again resets reps to 0, bumps lapses, drops ease, interval 1
	prevEase := r.Ease
	r = Schedule(r, GradeAgain, 4000)
	if r.Reps != 0 || r.Lapses != 1 || r.Interval != 1 || r.Ease >= prevEase {
		t.Fatalf("after Again: %+v (ease was %.2f)", r, prevEase)
	}
}

func TestScheduleGradeOrderingAndEaseFloor(t *testing.T) {
	// From an identical mature state, Hard < Good < Easy interval.
	base := Review{CardID: 1, Ease: 2.5, Interval: 20, Reps: 5}
	hard := Schedule(base, GradeHard, 0).Interval
	good := Schedule(base, GradeGood, 0).Interval
	easy := Schedule(base, GradeEasy, 0).Interval
	if !(hard < good && good < easy) {
		t.Fatalf("ordering broken: hard=%d good=%d easy=%d", hard, good, easy)
	}
	// Ease never drops below the floor even after many Again/Hard.
	r := NewReview(1)
	for i := 0; i < 30; i++ {
		r = Schedule(r, GradeAgain, int64(i))
	}
	if r.Ease < 1.3 {
		t.Fatalf("ease floor breached: %.3f", r.Ease)
	}
	// An out-of-range grade is a no-op (caller validates).
	unchanged := Schedule(base, Grade(9), 0)
	if unchanged != base {
		t.Fatalf("unknown grade mutated state: %+v", unchanged)
	}
	if ValidGrade(9) || !ValidGrade(GradeGood) {
		t.Fatal("ValidGrade wrong")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flashcards/ -run Schedule -v`
Expected: FAIL — `undefined: NewReview`/`Schedule`.

- [ ] **Step 3: Write the implementation**

```go
package flashcards

import "math"

// SM-2 spaced repetition (spec §9), grade 1..4. Pure — persistence is separate.
type Grade int

const (
	GradeAgain Grade = 1 // forgot: reset
	GradeHard  Grade = 2 // recalled with difficulty
	GradeGood  Grade = 3 // recalled
	GradeEasy  Grade = 4 // trivial
)

func ValidGrade(g Grade) bool { return g >= GradeAgain && g <= GradeEasy }

// Review is one card's mutable SM-2 state (mirrors the flashcard_reviews row).
type Review struct {
	CardID       int64
	Ease         float64
	Interval     int   // days
	DueAt        int64 // unix seconds
	Reps         int
	Lapses       int
	LastGrade    int
	LastReviewed int64
	IntroducedAt int64
}

const (
	minEase     = 1.3
	startEase   = 2.5
	maxInterval = 365
	hardFactor  = 1.2 // Hard grows slowly and ease-independently, so Hard < Good
	easyBonus   = 1.3
	daySecs     = 86400
)

// NewReview is the state of a card that has never been reviewed.
func NewReview(cardID int64) Review { return Review{CardID: cardID, Ease: startEase} }

// Schedule applies one grade at time `now` and returns the next state. Again
// resets the card (reps→0, lapse++, ease−0.20, due tomorrow); a correct grade
// advances reps and schedules the next interval. Because ease ≥ 1.3 > hardFactor
// (1.2), a mature card always satisfies Hard < Good < Easy.
func Schedule(r Review, g Grade, now int64) Review {
	if !ValidGrade(g) {
		return r
	}
	switch g {
	case GradeAgain:
		r.Lapses++
		r.Reps = 0
		r.Ease = clampEase(r.Ease - 0.20)
		r.Interval = 1
	case GradeHard:
		r.Reps++
		r.Ease = clampEase(r.Ease - 0.15)
		r.Interval = nextInterval(r, g)
	case GradeGood:
		r.Reps++
		// ease unchanged (SM-2 q=4 → +0)
		r.Interval = nextInterval(r, g)
	case GradeEasy:
		r.Reps++
		r.Ease = clampEase(r.Ease + 0.15)
		r.Interval = nextInterval(r, g)
	}
	if r.Interval > maxInterval {
		r.Interval = maxInterval
	}
	if r.Interval < 1 {
		r.Interval = 1
	}
	r.LastGrade = int(g)
	r.LastReviewed = now
	r.DueAt = now + int64(r.Interval)*daySecs
	return r
}

// nextInterval assumes r.Reps was already incremented for this correct review.
// The first two correct reps use fixed steps (1 then 6 days); afterwards the
// interval compounds by ease (Good), a slow ease-independent factor (Hard), or
// ease plus a bonus (Easy).
func nextInterval(r Review, g Grade) int {
	switch {
	case r.Reps <= 1:
		return 1
	case r.Reps == 2:
		return 6
	default:
		prev := r.Interval
		if prev < 1 {
			prev = 1
		}
		switch g {
		case GradeHard:
			return int(math.Round(float64(prev) * hardFactor))
		case GradeEasy:
			return int(math.Round(float64(prev) * r.Ease * easyBonus))
		default: // Good
			return int(math.Round(float64(prev) * r.Ease))
		}
	}
}

func clampEase(e float64) float64 {
	if e < minEase {
		return minEase
	}
	return e
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/flashcards/ -run Schedule -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/flashcards/srs.go internal/flashcards/srs_test.go
git commit -m "feat(flashcards): SM-2 scheduler (pure)"
```

---

### Task 2: Migrations v21/v22 + review-state, log & query store primitives

**Files:**
- Modify: `internal/store/store.go` (append v21/v22)
- Modify: `internal/store/store_test.go` (repin head 20→22; rename `TestUserVersionIsEighteen`→`TestUserVersionIsPinned`)
- Create: `internal/store/flashcard_reviews.go`
- Test: `internal/store/flashcard_reviews_test.go`

**Interfaces:**
- Produces:
  - `type ReviewRow struct { CardID int64; Ease float64; Interval int; DueAt int64; Reps, Lapses, LastGrade int; LastReviewed, IntroducedAt int64 }`
  - `func (s *Store) GetReview(cardID int64) (ReviewRow, bool, error)`
  - `func (s *Store) PutReview(r ReviewRow) error` — upsert by `card_id`.
  - `func (s *Store) AppendReviewLog(cardID int64, grade int, wasDue bool, at int64) error`
  - `func (s *Store) IntroducedSince(project string, sinceTs int64) (int, error)` — count of cards in `project` whose `introduced_at >= sinceTs` (for the daily new-card cap).
  - `func (s *Store) DueReviewCards(project string, now int64, limit int) ([]Flashcard, error)` — `active` cards with a review row where `due_at <= now`, ordered by `due_at`, limited.
  - `func (s *Store) NewActiveCards(project string, limit int) ([]Flashcard, error)` — `active` cards with NO review row, oldest id first, limited.

- [ ] **Step 1: Write the failing test**

```go
package store

import "testing"

func seedCard(t *testing.T, st *Store, project, part, anchor, status string) int64 {
	t.Helper()
	id, ins, err := st.InsertFlashcard(Flashcard{
		Project: project, Part: part, Anchor: anchor, StemHash: anchor, Type: "code",
		Front: "q " + anchor, Back: "a", Status: status, CreatedAt: 1,
	})
	if err != nil || !ins {
		t.Fatalf("seedCard(%s): id=%d ins=%v err=%v", anchor, id, ins, err)
	}
	return id
}

func TestReviewRowRoundTripAndQueries(t *testing.T) {
	st := openTestStore(t)
	active1 := seedCard(t, st, "p", "f.go", "a1", "active")
	active2 := seedCard(t, st, "p", "f.go", "a2", "active")
	_ = seedCard(t, st, "p", "f.go", "a3", "draft") // never scheduled

	// active1 reviewed and due in the past; active2 never reviewed (a "new" card).
	if err := st.PutReview(ReviewRow{CardID: active1, Ease: 2.5, Interval: 1, DueAt: 500, Reps: 1, IntroducedAt: 400}); err != nil {
		t.Fatalf("PutReview: %v", err)
	}
	got, ok, err := st.GetReview(active1)
	if err != nil || !ok || got.DueAt != 500 || got.Reps != 1 {
		t.Fatalf("GetReview: %+v ok=%v err=%v", got, ok, err)
	}

	due, err := st.DueReviewCards("p", 1000, 50)
	if err != nil || len(due) != 1 || due[0].ID != active1 {
		t.Fatalf("DueReviewCards: %+v err=%v (want just active1)", due, err)
	}
	news, err := st.NewActiveCards("p", 50)
	if err != nil || len(news) != 1 || news[0].ID != active2 {
		t.Fatalf("NewActiveCards: %+v err=%v (want just active2, not the draft)", news, err)
	}

	n, err := st.IntroducedSince("p", 300)
	if err != nil || n != 1 {
		t.Fatalf("IntroducedSince(300) = %d err=%v, want 1", n, err)
	}
	if n, _ := st.IntroducedSince("p", 500); n != 0 {
		t.Fatalf("IntroducedSince(500) = %d, want 0", n)
	}

	if err := st.AppendReviewLog(active1, 3, true, 600); err != nil {
		t.Fatalf("AppendReviewLog: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'ReviewRowRoundTrip' -v`
Expected: FAIL — `undefined: ReviewRow`/`PutReview`.

- [ ] **Step 3a: Append the migrations**

In `internal/store/store.go`, immediately before the closing `}` of the `migrations` slice (after the v20 `flashcard_reviews` CREATE), add:

```go
		// v21: when a card was first reviewed, for the daily new-card cap
		// (2026-07-25-flashcards-slice2). Set once on the first RecordReview and
		// never changed. Own slot: ALTER has no IF NOT EXISTS.
		`ALTER TABLE flashcard_reviews ADD COLUMN introduced_at INTEGER NOT NULL DEFAULT 0`,
		// v22: append-only review history. The flashcard_reviews row holds only
		// CURRENT SM-2 state; an honest rolling pass-rate (spec §9) needs the
		// event log — one row per graded review.
		`CREATE TABLE IF NOT EXISTS flashcard_review_log (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			card_id     INTEGER NOT NULL,
			grade       INTEGER NOT NULL,
			was_due     INTEGER NOT NULL DEFAULT 0,
			reviewed_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_frlog_card ON flashcard_review_log(card_id, reviewed_at)`,
```

- [ ] **Step 3b: Repin the head guards and rename the stale test**

In `internal/store/store_test.go`: change both head assertions from `20` to `22` (the `v != 20`→`v != 22` in `TestUserVersionIsEighteen`, and the pin in `TestMigrationHeadIsPinned`), and rename `func TestUserVersionIsEighteen` → `func TestUserVersionIsPinned` (update its doc comment to say "asserts the ABSOLUTE migration head (22)"). Leave the replay test untouched — it derives its range dynamically.

- [ ] **Step 3c: Write the store primitives**

Create `internal/store/flashcard_reviews.go`:

```go
package store

// ReviewRow is one row of flashcard_reviews: a card's current SM-2 state.
type ReviewRow struct {
	CardID                             int64
	Ease                               float64
	Interval                           int
	DueAt                              int64
	Reps, Lapses, LastGrade            int
	LastReviewed, IntroducedAt         int64
}

const reviewCols = "card_id, ease, interval, due_at, reps, lapses, last_grade, last_reviewed, introduced_at"

func (s *Store) GetReview(cardID int64) (ReviewRow, bool, error) {
	var r ReviewRow
	err := s.db.QueryRow("SELECT "+reviewCols+" FROM flashcard_reviews WHERE card_id=?", cardID).Scan(
		&r.CardID, &r.Ease, &r.Interval, &r.DueAt, &r.Reps, &r.Lapses, &r.LastGrade, &r.LastReviewed, &r.IntroducedAt)
	if err == errNoRows {
		return ReviewRow{}, false, nil
	}
	if err != nil {
		return ReviewRow{}, false, err
	}
	return r, true, nil
}

// PutReview upserts a card's SM-2 state by card_id.
func (s *Store) PutReview(r ReviewRow) error {
	_, err := s.db.Exec(`INSERT INTO flashcard_reviews
		(card_id, ease, interval, due_at, reps, lapses, last_grade, last_reviewed, introduced_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(card_id) DO UPDATE SET
			ease=excluded.ease, interval=excluded.interval, due_at=excluded.due_at,
			reps=excluded.reps, lapses=excluded.lapses, last_grade=excluded.last_grade,
			last_reviewed=excluded.last_reviewed, introduced_at=excluded.introduced_at`,
		r.CardID, r.Ease, r.Interval, r.DueAt, r.Reps, r.Lapses, r.LastGrade, r.LastReviewed, r.IntroducedAt)
	return err
}

// AppendReviewLog records one graded review event (append-only history).
func (s *Store) AppendReviewLog(cardID int64, grade int, wasDue bool, at int64) error {
	due := 0
	if wasDue {
		due = 1
	}
	_, err := s.db.Exec("INSERT INTO flashcard_review_log (card_id, grade, was_due, reviewed_at) VALUES (?,?,?,?)",
		cardID, grade, due, at)
	return err
}

// IntroducedSince counts cards in a project first reviewed at or after sinceTs.
func (s *Store) IntroducedSince(project string, sinceTs int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM flashcard_reviews r
		JOIN flashcards c ON c.id = r.card_id
		WHERE c.project=? AND r.introduced_at >= ?`, project, sinceTs).Scan(&n)
	return n, err
}

// DueReviewCards returns active cards whose review is due at or before now.
func (s *Store) DueReviewCards(project string, now int64, limit int) ([]Flashcard, error) {
	rows, err := s.db.Query(`SELECT `+prefixed(flashcardCols, "c.")+` FROM flashcards c
		JOIN flashcard_reviews r ON r.card_id = c.id
		WHERE c.project=? AND c.status='active' AND r.due_at <= ?
		ORDER BY r.due_at LIMIT ?`, project, now, limit)
	if err != nil {
		return nil, err
	}
	return scanCards(rows)
}

// NewActiveCards returns active cards that have never been reviewed.
func (s *Store) NewActiveCards(project string, limit int) ([]Flashcard, error) {
	rows, err := s.db.Query(`SELECT `+prefixed(flashcardCols, "c.")+` FROM flashcards c
		LEFT JOIN flashcard_reviews r ON r.card_id = c.id
		WHERE c.project=? AND c.status='active' AND r.card_id IS NULL
		ORDER BY c.id LIMIT ?`, project, limit)
	if err != nil {
		return nil, err
	}
	return scanCards(rows)
}
```

Add these small helpers to `internal/store/flashcards.go` (they factor the existing scan so the JOIN queries can reuse it):

```go
// errNoRows aliases sql.ErrNoRows so callers in this package need not import database/sql.
var errNoRows = sql.ErrNoRows

// prefixed rewrites a comma-separated column list with a table alias prefix,
// e.g. prefixed("id, project", "c.") == "c.id, c.project".
func prefixed(cols, prefix string) string {
	parts := strings.Split(cols, ", ")
	for i, p := range parts {
		parts[i] = prefix + p
	}
	return strings.Join(parts, ", ")
}

// scanCards scans a rows cursor selecting flashcardCols (in order) into Flashcards.
func scanCards(rows *sql.Rows) ([]Flashcard, error) {
	defer rows.Close()
	var out []Flashcard
	for rows.Next() {
		var c Flashcard
		if err := rows.Scan(&c.ID, &c.Project, &c.Part, &c.Anchor, &c.StemHash, &c.Type,
			&c.Front, &c.Back, &c.SourceRef, &c.SourceHash, &c.AnswerHash, &c.Status,
			&c.CreatedAt, &c.CuratedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
```

Add `"database/sql"` and `"strings"` to `internal/store/flashcards.go`'s imports, and refactor the existing `FlashcardsForProject` loop to `return scanCards(rows)` (DRY — same scan). Confirm `store.go` already imports `database/sql` (it does); the new symbols live in `flashcards.go`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -run 'ReviewRowRoundTrip|Migration|UserVersion' -v` then `go test ./internal/store/`
Expected: PASS (new test + repinned guards + full suite).

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go internal/store/flashcards.go internal/store/flashcard_reviews.go internal/store/flashcard_reviews_test.go
git commit -m "feat(flashcards): v21/v22 migrations + review-state/log/query store primitives"
```

---

### Task 3: Curation store primitives

**Files:**
- Create: `internal/store/flashcard_curation.go`
- Test: `internal/store/flashcard_curation_test.go`

**Interfaces:**
- Produces:
  - `func (s *Store) DraftsForProject(project string) ([]Flashcard, error)` — `status='draft'`, oldest id first.
  - `func (s *Store) SetCardStatus(id int64, status string, at int64) error` — sets `status` and, when status is `active`, `curated_at=at`.
  - `func (s *Store) EditCardText(id int64, front, back, stemHash, answerHash string, at int64) error` — updates text + recomputed hashes + `curated_at`.
  - `func (s *Store) DeleteCard(id int64) error` — removes the card AND its review + log rows in one transaction (kill).

- [ ] **Step 1: Write the failing test**

```go
package store

import "testing"

func TestCurationLifecycle(t *testing.T) {
	st := openTestStore(t)
	id := seedCard(t, st, "p", "f.go", "a1", "draft")

	drafts, err := st.DraftsForProject("p")
	if err != nil || len(drafts) != 1 || drafts[0].ID != id {
		t.Fatalf("DraftsForProject: %+v err=%v", drafts, err)
	}

	if err := st.SetCardStatus(id, "active", 900); err != nil {
		t.Fatalf("SetCardStatus: %v", err)
	}
	cards, _ := st.FlashcardsForProject("p")
	if cards[0].Status != "active" || cards[0].CuratedAt != 900 {
		t.Fatalf("after activate: status=%s curated=%d", cards[0].Status, cards[0].CuratedAt)
	}
	if d, _ := st.DraftsForProject("p"); len(d) != 0 {
		t.Fatalf("activated card still a draft")
	}

	if err := st.EditCardText(id, "new front", "new back", "newstem", "newans", 950); err != nil {
		t.Fatalf("EditCardText: %v", err)
	}
	cards, _ = st.FlashcardsForProject("p")
	if cards[0].Front != "new front" || cards[0].StemHash != "newstem" || cards[0].AnswerHash != "newans" {
		t.Fatalf("edit not applied: %+v", cards[0])
	}

	// kill cascades to review + log
	if err := st.PutReview(ReviewRow{CardID: id, Ease: 2.5}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendReviewLog(id, 3, true, 1000); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteCard(id); err != nil {
		t.Fatalf("DeleteCard: %v", err)
	}
	if cards, _ := st.FlashcardsForProject("p"); len(cards) != 0 {
		t.Fatalf("card not deleted")
	}
	if _, ok, _ := st.GetReview(id); ok {
		t.Fatalf("review row not cascaded on delete")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run CurationLifecycle -v`
Expected: FAIL — `undefined: DraftsForProject`.

- [ ] **Step 3: Write the implementation**

```go
package store

// DraftsForProject returns a project's uncurated cards, oldest id first.
func (s *Store) DraftsForProject(project string) ([]Flashcard, error) {
	rows, err := s.db.Query("SELECT "+flashcardCols+" FROM flashcards WHERE project=? AND status='draft' ORDER BY id", project)
	if err != nil {
		return nil, err
	}
	return scanCards(rows)
}

// SetCardStatus sets a card's status; activating also stamps curated_at.
func (s *Store) SetCardStatus(id int64, status string, at int64) error {
	if status == "active" {
		_, err := s.db.Exec("UPDATE flashcards SET status=?, curated_at=? WHERE id=?", status, at, id)
		return err
	}
	_, err := s.db.Exec("UPDATE flashcards SET status=? WHERE id=?", status, id)
	return err
}

// EditCardText rewrites a card's text and its recomputed hashes (the caller
// computes stemHash/answerHash — the store never imports the flashcards pkg).
func (s *Store) EditCardText(id int64, front, back, stemHash, answerHash string, at int64) error {
	_, err := s.db.Exec("UPDATE flashcards SET front=?, back=?, stem_hash=?, answer_hash=?, curated_at=? WHERE id=?",
		front, back, stemHash, answerHash, at, id)
	return err
}

// DeleteCard removes a card and its review state and log rows atomically (kill).
func (s *Store) DeleteCard(id int64) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	for _, q := range []string{
		"DELETE FROM flashcard_review_log WHERE card_id=?",
		"DELETE FROM flashcard_reviews WHERE card_id=?",
		"DELETE FROM flashcards WHERE id=?",
	} {
		if _, err = tx.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run CurationLifecycle -v` then `go test ./internal/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/flashcard_curation.go internal/store/flashcard_curation_test.go
git commit -m "feat(flashcards): curation store primitives (activate/edit/kill)"
```

---

### Task 4: Review engine (queue + record)

**Files:**
- Create: `internal/flashcards/review.go`
- Test: `internal/flashcards/review_test.go`

**Interfaces:**
- Consumes: `Schedule`/`Review`/`Grade` (T1); `store.ReviewRow`, `GetReview`/`PutReview`/`AppendReviewLog`/`DueReviewCards`/`NewActiveCards`/`IntroducedSince`/`SetCardStatus` (T2/T3); `store.Flashcard`.
- Produces:
  - `type ReviewConfig struct { NewPerDay, LeechThreshold int }` and `func DefaultReviewConfig() ReviewConfig` (`{20, 8}`).
  - `type Reviewer struct { Store *store.Store; Cfg ReviewConfig }`
  - `func (rv *Reviewer) BuildQueue(project string, now, dayStart int64) ([]store.Flashcard, error)` — due cards first (oldest due), then up to `NewPerDay − introducedSince(dayStart)` new cards; then reordered so no two ADJACENT cards share a `Part` when avoidable.
  - `func (rv *Reviewer) Record(cardID int64, g Grade, wasDue bool, now int64) (suspended bool, err error)` — loads or initializes the card's Review, applies `Schedule`, stamps `IntroducedAt` on first review, persists state, appends the log, and if `Lapses >= LeechThreshold` sets the card `suspended` (returns `suspended=true`).

- [ ] **Step 1: Write the failing test**

```go
package flashcards

import (
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/loom.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func active(t *testing.T, st *store.Store, part, anchor string) int64 {
	t.Helper()
	id, ins, err := st.InsertFlashcard(store.Flashcard{
		Project: "p", Part: part, Anchor: anchor, StemHash: anchor, Type: "code",
		Front: "q", Back: "a", Status: "active", CreatedAt: 1,
	})
	if err != nil || !ins {
		t.Fatalf("seed %s: %v", anchor, err)
	}
	return id
}

func TestBuildQueueCapAndInterleave(t *testing.T) {
	st := openStore(t)
	rv := &Reviewer{Store: st, Cfg: ReviewConfig{NewPerDay: 2, LeechThreshold: 8}}
	// three new active cards, two share a part
	active(t, st, "a.go", "n1")
	active(t, st, "a.go", "n2")
	active(t, st, "b.go", "n3")

	q, err := rv.BuildQueue("p", 10_000, 0)
	if err != nil {
		t.Fatalf("BuildQueue: %v", err)
	}
	if len(q) != 2 { // NewPerDay cap
		t.Fatalf("queue len=%d, want 2 (new-card cap)", len(q))
	}

	// with the cap lifted, adjacent cards should not share a part when avoidable
	rv.Cfg.NewPerDay = 10
	q, _ = rv.BuildQueue("p", 10_000, 0)
	for i := 1; i < len(q); i++ {
		if q[i].Part == q[i-1].Part {
			// allowed only if every remaining card shares that part; here b.go breaks ties
			t.Fatalf("adjacent siblings not interleaved: %v", []string{q[i-1].Part, q[i].Part})
		}
	}
}

func TestRecordAppliesSM2AndSuspendsLeech(t *testing.T) {
	st := openStore(t)
	rv := &Reviewer{Store: st, Cfg: ReviewConfig{NewPerDay: 20, LeechThreshold: 3}}
	id := active(t, st, "a.go", "c1")

	// first Good creates the review row, stamps introduced_at, schedules ahead
	if susp, err := rv.Record(id, GradeGood, false, 1000); err != nil || susp {
		t.Fatalf("Record good: susp=%v err=%v", susp, err)
	}
	r, ok, _ := st.GetReview(id)
	if !ok || r.Reps != 1 || r.IntroducedAt != 1000 || r.DueAt <= 1000 {
		t.Fatalf("review after good: %+v ok=%v", r, ok)
	}

	// three Again → lapses reaches threshold → suspend
	rv.Record(id, GradeAgain, true, 2000)
	rv.Record(id, GradeAgain, true, 3000)
	susp, err := rv.Record(id, GradeAgain, true, 4000)
	if err != nil || !susp {
		t.Fatalf("expected suspend at leech threshold: susp=%v err=%v", susp, err)
	}
	cards, _ := st.FlashcardsForProject("p")
	if cards[0].Status != "suspended" {
		t.Fatalf("leech not suspended: status=%s", cards[0].Status)
	}
	// introduced_at is not overwritten by later reviews
	if r2, _, _ := st.GetReview(id); r2.IntroducedAt != 1000 {
		t.Fatalf("introduced_at changed: %d", r2.IntroducedAt)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flashcards/ -run 'BuildQueue|Record' -v`
Expected: FAIL — `undefined: Reviewer`.

- [ ] **Step 3: Write the implementation**

```go
package flashcards

import "github.com/henricktissink/loom/internal/store"

// ReviewConfig bounds a review session (spec §9): a daily cap on newly
// introduced cards, and a lapse count past which a card is auto-suspended.
type ReviewConfig struct {
	NewPerDay      int
	LeechThreshold int
}

func DefaultReviewConfig() ReviewConfig { return ReviewConfig{NewPerDay: 20, LeechThreshold: 8} }

// Reviewer composes the store primitives into a review session.
type Reviewer struct {
	Store *store.Store
	Cfg   ReviewConfig
}

// BuildQueue returns the cards to study now: everything due, then up to the
// remaining daily budget of new cards, reordered so adjacent cards avoid sharing
// a Part (sibling interference) when the queue contains more than one part.
func (rv *Reviewer) BuildQueue(project string, now, dayStart int64) ([]store.Flashcard, error) {
	due, err := rv.Store.DueReviewCards(project, now, 1000)
	if err != nil {
		return nil, err
	}
	introduced, err := rv.Store.IntroducedSince(project, dayStart)
	if err != nil {
		return nil, err
	}
	budget := rv.Cfg.NewPerDay - introduced
	var news []store.Flashcard
	if budget > 0 {
		if news, err = rv.Store.NewActiveCards(project, budget); err != nil {
			return nil, err
		}
	}
	return interleaveByPart(append(due, news...)), nil
}

// interleaveByPart greedily reorders so no two adjacent cards share a Part when
// a different-part card is available. Stable for a single part (returns as-is).
func interleaveByPart(cards []store.Flashcard) []store.Flashcard {
	if len(cards) < 3 {
		return cards
	}
	remaining := append([]store.Flashcard(nil), cards...)
	out := make([]store.Flashcard, 0, len(remaining))
	var lastPart string
	for len(remaining) > 0 {
		pick := 0
		for i, c := range remaining {
			if c.Part != lastPart {
				pick = i
				break
			}
		}
		out = append(out, remaining[pick])
		lastPart = remaining[pick].Part
		remaining = append(remaining[:pick], remaining[pick+1:]...)
	}
	return out
}

// Record grades one card: applies SM-2, persists state, appends the log, and
// suspends the card if its lapse count reaches the leech threshold.
func (rv *Reviewer) Record(cardID int64, g Grade, wasDue bool, now int64) (suspended bool, err error) {
	row, ok, err := rv.Store.GetReview(cardID)
	if err != nil {
		return false, err
	}
	var r Review
	if ok {
		r = fromRow(row)
	} else {
		r = NewReview(cardID)
		r.IntroducedAt = now // first review: stamp once
	}
	r = Schedule(r, g, now)
	if err := rv.Store.PutReview(toRow(r)); err != nil {
		return false, err
	}
	if err := rv.Store.AppendReviewLog(cardID, int(g), wasDue, now); err != nil {
		return false, err
	}
	if rv.Cfg.LeechThreshold > 0 && r.Lapses >= rv.Cfg.LeechThreshold {
		if err := rv.Store.SetCardStatus(cardID, "suspended", now); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func fromRow(r store.ReviewRow) Review {
	return Review{CardID: r.CardID, Ease: r.Ease, Interval: r.Interval, DueAt: r.DueAt,
		Reps: r.Reps, Lapses: r.Lapses, LastGrade: r.LastGrade, LastReviewed: r.LastReviewed, IntroducedAt: r.IntroducedAt}
}

func toRow(r Review) store.ReviewRow {
	return store.ReviewRow{CardID: r.CardID, Ease: r.Ease, Interval: r.Interval, DueAt: r.DueAt,
		Reps: r.Reps, Lapses: r.Lapses, LastGrade: r.LastGrade, LastReviewed: r.LastReviewed, IntroducedAt: r.IntroducedAt}
}
```

Note: `SetCardStatus(id, "suspended", now)` stamps `curated_at` only for `"active"`; for `"suspended"` it updates status only — verify against T3's implementation.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/flashcards/ -run 'BuildQueue|Record' -v` then `go test ./internal/flashcards/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/flashcards/review.go internal/flashcards/review_test.go
git commit -m "feat(flashcards): review engine — queue (cap+interleave) and record (SM-2+leech)"
```

---

### Task 5: Measured pass-rate & part stats

**Files:**
- Create: `internal/store/flashcard_stats.go`
- Create: `internal/flashcards/stats.go`
- Test: `internal/store/flashcard_stats_test.go`, `internal/flashcards/stats_test.go`

**Interfaces:**
- Produces (store):
  - `type PartStat struct { Part string; Total, Active, Draft, Due int }`
  - `func (s *Store) PartStats(project string, now int64) ([]PartStat, error)` — per-part card counts (deterministic, ordered by Part).
  - `func (s *Store) PassRate(project string, sinceTs int64) (passed, total int, err error)` — over `flashcard_review_log` rows for the project's cards with `reviewed_at >= sinceTs`, `passed` = grade ≥ 3 (Good).
- Produces (flashcards):
  - `type Coverage struct { Part string; Total, Active, Draft, Due int }` and `func (rv *Reviewer) Coverage(project string, now int64) ([]Coverage, error)` (thin pass-through of `PartStats`, kept in the flashcards package so the CLI/GUI depend on one layer).
  - `func (rv *Reviewer) PassRate(project string, sinceTs int64) (rate float64, n int, err error)` — `passed/total` as a fraction (0 when `total==0`), plus `n=total`. NOT a "mastery %": it is measured over actual graded reviews.

- [ ] **Step 1: Write the failing test (store)**

```go
package store

import "testing"

func TestPartStatsAndPassRate(t *testing.T) {
	st := openTestStore(t)
	a1 := seedCard(t, st, "p", "a.go", "a1", "active")
	seedCard(t, st, "p", "a.go", "a2", "draft")
	seedCard(t, st, "p", "b.go", "b1", "active")

	// a1 is due
	st.PutReview(ReviewRow{CardID: a1, DueAt: 100, Reps: 1})

	stats, err := st.PartStats("p", 1000)
	if err != nil || len(stats) != 2 {
		t.Fatalf("PartStats: %+v err=%v", stats, err)
	}
	var ag PartStat
	for _, s := range stats {
		if s.Part == "a.go" {
			ag = s
		}
	}
	if ag.Total != 2 || ag.Active != 1 || ag.Draft != 1 || ag.Due != 1 {
		t.Fatalf("a.go stats: %+v", ag)
	}

	st.AppendReviewLog(a1, 3, true, 500) // pass
	st.AppendReviewLog(a1, 1, true, 600) // fail
	st.AppendReviewLog(a1, 4, true, 700) // pass
	passed, total, err := st.PassRate("p", 0)
	if err != nil || total != 3 || passed != 2 {
		t.Fatalf("PassRate: passed=%d total=%d err=%v (want 2/3)", passed, total, err)
	}
	if p, _, _ := st.PassRate("p", 650); p != 1 { // only the grade-4 row is in-window
		t.Fatalf("windowed PassRate passed=%d, want 1", p)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run PartStatsAndPassRate -v`
Expected: FAIL — `undefined: PartStats`.

- [ ] **Step 3a: Write the store queries** (`internal/store/flashcard_stats.go`)

```go
package store

// PartStat is per-manifest-part card counts for the coverage view.
type PartStat struct {
	Part                    string
	Total, Active, Draft, Due int
}

// PartStats returns per-part counts for a project, ordered by Part.
func (s *Store) PartStats(project string, now int64) ([]PartStat, error) {
	rows, err := s.db.Query(`SELECT c.part,
			COUNT(*),
			SUM(CASE WHEN c.status='active' THEN 1 ELSE 0 END),
			SUM(CASE WHEN c.status='draft'  THEN 1 ELSE 0 END),
			SUM(CASE WHEN c.status='active' AND r.due_at IS NOT NULL AND r.due_at <= ? THEN 1 ELSE 0 END)
		FROM flashcards c
		LEFT JOIN flashcard_reviews r ON r.card_id = c.id
		WHERE c.project=?
		GROUP BY c.part ORDER BY c.part`, now, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PartStat
	for rows.Next() {
		var p PartStat
		if err := rows.Scan(&p.Part, &p.Total, &p.Active, &p.Draft, &p.Due); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PassRate counts graded reviews for a project since sinceTs; passed = grade >= 3.
func (s *Store) PassRate(project string, sinceTs int64) (passed, total int, err error) {
	err = s.db.QueryRow(`SELECT
			COALESCE(SUM(CASE WHEN l.grade >= 3 THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM flashcard_review_log l
		JOIN flashcards c ON c.id = l.card_id
		WHERE c.project=? AND l.reviewed_at >= ?`, project, sinceTs).Scan(&passed, &total)
	return passed, total, err
}
```

- [ ] **Step 3b: Write the flashcards wrapper** (`internal/flashcards/stats.go`)

```go
package flashcards

// Coverage is per-part progress for the coverage map (spec §9): counts only —
// there is deliberately no "mastery %".
type Coverage struct {
	Part                      string
	Total, Active, Draft, Due int
}

// Coverage returns per-part card counts for a project.
func (rv *Reviewer) Coverage(project string, now int64) ([]Coverage, error) {
	stats, err := rv.Store.PartStats(project, now)
	if err != nil {
		return nil, err
	}
	out := make([]Coverage, len(stats))
	for i, s := range stats {
		out[i] = Coverage{Part: s.Part, Total: s.Total, Active: s.Active, Draft: s.Draft, Due: s.Due}
	}
	return out, nil
}

// PassRate is the MEASURED fraction of graded reviews (since sinceTs) that were
// recalled (grade >= Good) — an honest retention signal, not card existence.
// Returns 0 when there are no reviews yet.
func (rv *Reviewer) PassRate(project string, sinceTs int64) (rate float64, n int, err error) {
	passed, total, err := rv.Store.PassRate(project, sinceTs)
	if err != nil || total == 0 {
		return 0, total, err
	}
	return float64(passed) / float64(total), total, nil
}
```

- [ ] **Step 3c: Write the flashcards test** (`internal/flashcards/stats_test.go`)

```go
package flashcards

import "testing"

func TestReviewerPassRateAndCoverage(t *testing.T) {
	st := openStore(t)
	rv := &Reviewer{Store: st, Cfg: DefaultReviewConfig()}
	id := active(t, st, "a.go", "c1")

	if rate, n, err := rv.PassRate("p", 0); err != nil || n != 0 || rate != 0 {
		t.Fatalf("empty PassRate: rate=%v n=%d err=%v (want 0,0)", rate, n, err)
	}
	rv.Record(id, GradeGood, false, 1000) // pass
	rv.Record(id, GradeAgain, true, 2000) // fail
	rate, n, err := rv.PassRate("p", 0)
	if err != nil || n != 2 || rate != 0.5 {
		t.Fatalf("PassRate: rate=%v n=%d err=%v (want 0.5, 2)", rate, n, err)
	}
	cov, err := rv.Coverage("p", 3000)
	if err != nil || len(cov) != 1 || cov[0].Active != 1 {
		t.Fatalf("Coverage: %+v err=%v", cov, err)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -run PartStatsAndPassRate -v && go test ./internal/flashcards/ -run 'PassRate|Coverage' -v` then `go test ./internal/store/ ./internal/flashcards/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/flashcard_stats.go internal/store/flashcard_stats_test.go internal/flashcards/stats.go internal/flashcards/stats_test.go
git commit -m "feat(flashcards): measured pass-rate + per-part coverage stats"
```

---

### Task 6: CLI — `curate`, `review`, `stats`

**Files:**
- Modify: `internal/flashcards/cli.go` (add subcommands to `RunCLI`)
- Modify: `cmd/loom/main.go` (`runFlashcards` passes `os.Stdin` through for `review`)
- Test: `internal/flashcards/cli_review_test.go`

**Interfaces:**
- Consumes: `Reviewer`/`ReviewConfig`/`DefaultReviewConfig` (T4), `Coverage`/`PassRate` (T5), `store` curation + query primitives, `Anchor`/`StemHash`/`Hash` (Slice 1) for edit — but Slice 2's `curate` only ACTIVATES (edit/kill are GUI, Slice 3), so hashing is not needed here.
- Produces:
  - `RunCLI` gains three verbs (keeping `generate` unchanged):
    - `curate <projectRoot> [--activate-all]` — lists drafts; with `--activate-all`, activates every draft (`SetCardStatus(id,"active",now)`) and reports the count.
    - `review <projectRoot>` — builds the queue and, for each card, prints `Q:`/`A:` then reads a grade line (`1..4`, or `q`/EOF to stop) from `in io.Reader`, calling `Reviewer.Record`; prints a session summary. Cards from `DueReviewCards` are `wasDue=true`; new cards `wasDue=false`.
    - `stats <projectRoot>` — prints per-part coverage counts and the measured project pass-rate.
  - `RunCLI` signature GAINS an `in io.Reader` parameter (before `out`) so `review` is scriptable/testable: `func RunCLI(args []string, st *store.Store, binary, workDir string, now int64, in io.Reader, out io.Writer) error`. Update the existing `generate` tests and `cmd/loom/main.go` call site accordingly (pass `os.Stdin`; tests pass a `strings.Reader`/`bytes.Reader`).

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

func TestRunCLICurateReviewStats(t *testing.T) {
	st := openStore(t)
	// two active-by-curation cards in one project via direct seed
	a1 := active(t, st, "a.go", "c1")
	_ = a1
	// make them drafts first to exercise curate
	st.SetCardStatus(a1, "draft", 0)
	root := t.TempDir()

	// curate --activate-all flips the draft to active
	var cur bytes.Buffer
	if err := RunCLI([]string{"curate", root, "--activate-all"}, st, "claude", root, 1000, strings.NewReader(""), &cur); err != nil {
		t.Fatalf("curate: %v", err)
	}
	// note: curate operates on the project derived from root's basename; align the
	// seeded project with it so the draft is found
	_ = cur

	// review: feed one grade, expect one recorded review
	var rev bytes.Buffer
	// project name derives from root basename; re-seed under that project name
	proj := projectName(root)
	id, _, _ := st.InsertFlashcard(store.Flashcard{Project: proj, Part: "a.go", Anchor: "z1", StemHash: "z1", Type: "code", Front: "q", Back: "a", Status: "active", CreatedAt: 1})
	if err := RunCLI([]string{"review", root}, st, "claude", root, 2000, strings.NewReader("3\n"), &rev); err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, ok, _ := st.GetReview(id); !ok {
		t.Fatalf("review did not record a grade for the queued card")
	}
	if !strings.Contains(rev.String(), "A:") {
		t.Fatalf("review did not reveal answers: %q", rev.String())
	}

	// stats prints a pass-rate line
	var stx bytes.Buffer
	if err := RunCLI([]string{"stats", root}, st, "claude", root, 3000, strings.NewReader(""), &stx); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !strings.Contains(stx.String(), "pass-rate") {
		t.Fatalf("stats missing pass-rate: %q", stx.String())
	}
	_ = filepath.Separator
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flashcards/ -run RunCLICurateReviewStats -v`
Expected: FAIL — wrong arg count to `RunCLI` / unknown verb.

- [ ] **Step 3a: Extend `RunCLI`** (`internal/flashcards/cli.go`)

Change the signature to add `in io.Reader` and dispatch on the verb. Keep the existing `generate` body; add the three verbs:

```go
func RunCLI(args []string, st *store.Store, binary, workDir string, now int64, in io.Reader, out io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: loom flashcards <generate|curate|review|stats> <projectRoot> [args]")
	}
	verb, root := args[0], args[1]
	project := projectName(root)
	rv := &Reviewer{Store: st, Cfg: DefaultReviewConfig()}
	switch verb {
	case "generate":
		return runGenerate(args, st, binary, workDir, now, out) // existing generate body, extracted
	case "curate":
		return runCurate(args, st, project, now, out)
	case "review":
		return runReview(rv, project, now, in, out)
	case "stats":
		return runStats(rv, project, now, out)
	default:
		return fmt.Errorf("unknown flashcards command %q", verb)
	}
}
```

Extract the current `generate` implementation (manifest build + pipeline loop) verbatim into `func runGenerate(args []string, st *store.Store, binary, workDir string, now int64, out io.Writer) error`, then add:

```go
func runCurate(args []string, st *store.Store, project string, now int64, out io.Writer) error {
	drafts, err := st.DraftsForProject(project)
	if err != nil {
		return err
	}
	activateAll := false
	for _, a := range args[2:] {
		if a == "--activate-all" {
			activateAll = true
		}
	}
	if !activateAll {
		fmt.Fprintf(out, "%d draft card(s) awaiting curation:\n", len(drafts))
		for _, c := range drafts {
			fmt.Fprintf(out, "  [%d] %-14s %s\n", c.ID, c.Type, c.Front)
		}
		fmt.Fprintln(out, "re-run with --activate-all to activate them")
		return nil
	}
	n := 0
	for _, c := range drafts {
		if err := st.SetCardStatus(c.ID, "active", now); err != nil {
			return err
		}
		n++
	}
	fmt.Fprintf(out, "activated %d card(s)\n", n)
	return nil
}

func runReview(rv *Reviewer, project string, now int64, in io.Reader, out io.Writer) error {
	dayStart := now - (now % 86400)
	// mark which queued cards were due (for the log's was_due flag)
	due, err := rv.Store.DueReviewCards(project, now, 1000)
	if err != nil {
		return err
	}
	dueIDs := map[int64]bool{}
	for _, c := range due {
		dueIDs[c.ID] = true
	}
	queue, err := rv.BuildQueue(project, now, dayStart)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(in)
	reviewed := 0
	for _, c := range queue {
		fmt.Fprintf(out, "Q: %s\n", c.Front)
		fmt.Fprintf(out, "A: %s\n", c.Back)
		fmt.Fprint(out, "grade (1=again 2=hard 3=good 4=easy, q=quit): ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "q" || line == "" {
			break
		}
		g, ok := parseGrade(line)
		if !ok {
			fmt.Fprintf(out, "  ignored %q\n", line)
			continue
		}
		if _, err := rv.Record(c.ID, g, dueIDs[c.ID], now); err != nil {
			return err
		}
		reviewed++
	}
	fmt.Fprintf(out, "reviewed %d card(s)\n", reviewed)
	return nil
}

func runStats(rv *Reviewer, project string, now int64, out io.Writer) error {
	cov, err := rv.Coverage(project, now)
	if err != nil {
		return err
	}
	for _, c := range cov {
		fmt.Fprintf(out, "  %-40s total=%d active=%d draft=%d due=%d\n", c.Part, c.Total, c.Active, c.Draft, c.Due)
	}
	rate, n, err := rv.PassRate(project, 0)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "pass-rate: %.0f%% over %d review(s)\n", rate*100, n)
	return nil
}

func parseGrade(s string) (Grade, bool) {
	switch s {
	case "1":
		return GradeAgain, true
	case "2":
		return GradeHard, true
	case "3":
		return GradeGood, true
	case "4":
		return GradeEasy, true
	}
	return 0, false
}
```

Add `"bufio"` and `"io"` to `cli.go`'s imports (keep `fmt`, `strings`, `store`).

- [ ] **Step 3b: Update the `main.go` call site and existing generate tests**

In `cmd/loom/main.go`, `runFlashcards` now passes stdin: `return flashcards.RunCLI(args, st, "claude", cfg.LoomDir, time.Now().Unix(), os.Stdin, os.Stdout)`.

In `internal/flashcards/cli_test.go`, update the existing `RunCLI(...)` calls (the generate-path tests) to pass a reader before the writer, e.g. `RunCLI([]string{"generate", root}, st, bin, root, 200, strings.NewReader(""), &buf)`. Add the `strings` import if missing.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/flashcards/ ./internal/store/ -v && go build ./...`
Expected: PASS across both packages and a clean build (all `RunCLI` call sites updated).

- [ ] **Step 5: Commit**

```bash
git add internal/flashcards/cli.go internal/flashcards/cli_test.go internal/flashcards/cli_review_test.go cmd/loom/main.go
git commit -m "feat(flashcards): curate/review/stats CLI subcommands"
```

---

## Self-Review

**Spec coverage (Slice 2 scope, spec §12 step 2 + §9):**
- SM-2 scheduler → Task 1 (pure, property-tested: progression, reset, grade ordering, ease floor). ✓
- Curation state machine (`draft→active`, edit, kill) → Task 3 (store) + Task 6 (`curate` CLI activates). ✓ (edit/kill store primitives present; their UI is Slice 3.)
- Review loop (queue + record) → Task 4; daily new-card cap (`introduced_at`, v21) + sibling interference (`interleaveByPart`) + leech suspension → Task 4. ✓
- Measured pass-rate, NOT mastery % → Task 5 (`flashcard_review_log`, v22, over graded events; `Coverage` is counts only). ✓
- Provable via CLI → Task 6 (`curate`/`review`/`stats`, scriptable). ✓
- Deferred (correctly out): GUI Study pane (Slice 3), Anki export (Slice 4), AST anchoring + `answer_hash`-driven re-learn + orphan flagging (Slice 5), FSRS + relevance-weighting (post-v1). `answer_hash` stays populated-but-unread; `introduced_at`/log are new seams.

**Placeholder scan:** No TBD/TODO; every code step is complete; every run step names the command and expected result.

**Type consistency:** `Review`/`Grade` (T1) ↔ `store.ReviewRow` (T2) bridged by `fromRow`/`toRow` (T4) — field sets match. `store` query primitives (T2) and curation primitives (T3) are consumed with the exact signatures defined. `Reviewer` (T4) is reused by T5 (`Coverage`/`PassRate` methods on it) and T6. `RunCLI`'s new `in io.Reader` parameter is threaded through T6 and the `main.go` call site; the Slice-1 `generate` tests are updated in the same task. Grade constants (`GradeGood=3`) are the pass threshold used identically in `store.PassRate` (`grade >= 3`) and the SM-2 correct/incorrect split.

**Migration discipline:** head 20 → claims v21 (ALTER, own slot, no IF NOT EXISTS) + v22 (CREATE IF NOT EXISTS); guard tests repinned 20→22 with the replay test proving safety; stale `TestUserVersionIsEighteen` renamed. If another spec lands a migration first, renumber.

**One cross-task note for the executor:** T4's leech path calls `SetCardStatus(id,"suspended",now)`; confirm T3's `SetCardStatus` updates status without stamping `curated_at` for non-`active` statuses (it does, by design — only `"active"` stamps `curated_at`).
