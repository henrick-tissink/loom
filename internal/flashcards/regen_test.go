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
