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

func TestPartStatsDueEdgeCases(t *testing.T) {
	st := openTestStore(t)
	a1 := seedCard(t, st, "p", "a.go", "e1", "active") // due (past)
	a2 := seedCard(t, st, "p", "a.go", "e2", "active") // active but NOT yet due
	d1 := seedCard(t, st, "p", "a.go", "e3", "draft")  // draft carrying a stale review row
	seedCard(t, st, "p", "b.go", "e4", "active")       // active, no review row
	st.PutReview(ReviewRow{CardID: a1, DueAt: 100})
	st.PutReview(ReviewRow{CardID: a2, DueAt: 9000})
	st.PutReview(ReviewRow{CardID: d1, DueAt: 100})

	stats, err := st.PartStats("p", 1000)
	if err != nil {
		t.Fatalf("PartStats: %v", err)
	}
	m := map[string]PartStat{}
	for _, s := range stats {
		m[s.Part] = s
	}
	ag := m["a.go"]
	if ag.Total != 3 || ag.Active != 2 || ag.Draft != 1 || ag.Due != 1 {
		t.Fatalf("a.go = %+v, want Total3 Active2 Draft1 Due1 (only a1 due; a2 not-yet-due, d1 draft both excluded)", ag)
	}
	bg := m["b.go"]
	if bg.Total != 1 || bg.Active != 1 || bg.Draft != 0 || bg.Due != 0 {
		t.Fatalf("b.go = %+v, want Total1 Active1 Draft0 Due0 (active, no review row → not due)", bg)
	}
}

func TestPassRateWindowBoundaryAndTotal(t *testing.T) {
	st := openTestStore(t)
	c := seedCard(t, st, "p", "a.go", "w1", "active")
	st.AppendReviewLog(c, 3, true, 500)         // pass, before window
	st.AppendReviewLog(c, 4, true, 650)         // pass, exactly at the boundary
	st.AppendReviewLog(c, 1, true, 700)         // fail, in window
	passed, total, err := st.PassRate("p", 650) // >= 650 includes 650 and 700
	if err != nil {
		t.Fatalf("PassRate: %v", err)
	}
	if total != 2 || passed != 1 {
		t.Fatalf("windowed since=650: passed=%d total=%d, want 1/2 (boundary >= is inclusive)", passed, total)
	}
}
