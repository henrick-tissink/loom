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
	s1b := seedCard(t, st, "p", "a.go", "a3", "active") // second stale in a.go → DISTINCT must collapse
	s2 := seedCard(t, st, "p", "b.go", "b1", "active")
	seedCard(t, st, "p", "c.go", "c1", "draft") // draft, not stale

	st.SetCardStatus(s1, "stale", 1)
	st.SetCardStatus(s1b, "stale", 1)
	st.SetCardStatus(s2, "stale", 1)

	parts, err := st.StalePartsForProject("p")
	if err != nil {
		t.Fatalf("StalePartsForProject: %v", err)
	}
	if len(parts) != 2 || parts[0] != "a.go" || parts[1] != "b.go" {
		t.Fatalf("stale parts = %v, want [a.go b.go] (a.go has 2 stale cards → 1 entry via DISTINCT)", parts)
	}
}
