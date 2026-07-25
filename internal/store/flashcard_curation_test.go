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
