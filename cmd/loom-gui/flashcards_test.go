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
