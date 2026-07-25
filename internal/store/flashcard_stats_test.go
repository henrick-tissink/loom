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
