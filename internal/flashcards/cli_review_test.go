package flashcards

import (
	"bytes"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func TestRunCLICurateReviewStats(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	proj := projectName(root)

	// Seed a draft under the SAME project the CLI derives from root.
	draftID, _, err := st.InsertFlashcard(store.Flashcard{
		Project: proj, Part: "a.go", Anchor: "d1", StemHash: "d1", Type: "code",
		Front: "q draft", Back: "a", Status: "draft", CreatedAt: 1,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// curate --activate-all must flip the draft to active and report it.
	var cur bytes.Buffer
	if err := RunCLI([]string{"curate", root, "--activate-all"}, st, "claude", root, 1000, strings.NewReader(""), &cur); err != nil {
		t.Fatalf("curate: %v", err)
	}
	if !strings.Contains(cur.String(), "activated 1") {
		t.Fatalf("curate report = %q, want to contain 'activated 1'", cur.String())
	}
	cards, _ := st.FlashcardsForProject(proj)
	if len(cards) != 1 || cards[0].Status != "active" || cards[0].CuratedAt != 1000 {
		t.Fatalf("curate did not activate the draft: %+v", cards)
	}

	// review: the now-active, never-reviewed card is queued; feed one Good grade.
	var rev bytes.Buffer
	if err := RunCLI([]string{"review", root}, st, "claude", root, 2000, strings.NewReader("3\n"), &rev); err != nil {
		t.Fatalf("review: %v", err)
	}
	if !strings.Contains(rev.String(), "A: a") {
		t.Fatalf("review did not reveal the answer: %q", rev.String())
	}
	if _, ok, _ := st.GetReview(draftID); !ok {
		t.Fatalf("review did not record a grade for the queued card")
	}

	// review again: the card is now due after 1 day; feed Good again (2000 + 86400 = 88400, so review at 90000).
	var rev2 bytes.Buffer
	if err := RunCLI([]string{"review", root}, st, "claude", root, 90000, strings.NewReader("3\n"), &rev2); err != nil {
		t.Fatalf("review (2nd): %v", err)
	}

	// stats: one due Good review (first-exposure is excluded) → 100% measured pass-rate.
	var stx bytes.Buffer
	if err := RunCLI([]string{"stats", root}, st, "claude", root, 100000, strings.NewReader(""), &stx); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !strings.Contains(stx.String(), "pass-rate: 100%") {
		t.Fatalf("stats pass-rate = %q, want to contain 'pass-rate: 100%%'", stx.String())
	}
}
