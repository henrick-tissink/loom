package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/flashcards"
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
	if cards[0].Front != "new front" || cards[0].Back != "new back" ||
		cards[0].StemHash != flashcards.StemHash("new front") ||
		cards[0].AnswerHash != flashcards.Hash("new back") {
		t.Fatalf("edit not applied/hashed correctly (front→stem, back→answer): %+v", cards[0])
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

func TestFlashcardReadBridge_coverageDraftsQueue(t *testing.T) {
	app, st := newFlashApp(t)
	a1 := seedFCard(t, st, "a.go", "a1", "active") // will be due
	seedFCard(t, st, "a.go", "a2", "active")       // new (never reviewed)
	seedFCard(t, st, "a.go", "a3", "draft")        // draft
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
