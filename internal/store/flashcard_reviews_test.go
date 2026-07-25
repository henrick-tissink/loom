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
