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
	if susp, _ := rv.Record(id, GradeAgain, true, 2000); susp {
		t.Fatal("suspended too early (lapse 1 of 3)")
	}
	if susp, _ := rv.Record(id, GradeAgain, true, 3000); susp {
		t.Fatal("suspended too early (lapse 2 of 3)")
	}
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

func TestBuildQueueBudgetReducedByIntroduced(t *testing.T) {
	st := openStore(t)
	rv := &Reviewer{Store: st, Cfg: ReviewConfig{NewPerDay: 2, LeechThreshold: 8}}
	n1 := active(t, st, "a.go", "n1")
	active(t, st, "b.go", "n2")
	active(t, st, "c.go", "n3")

	// Introduce n1 at t=5000: it gets a review row (no longer "new") and schedules
	// ~1 day out, so it is not due at t=6000.
	if _, err := rv.Record(n1, GradeGood, false, 5000); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// NewPerDay=2, one introduced today (dayStart=0) → budget = 1 new card; n1 not due.
	q, err := rv.BuildQueue("p", 6000, 0)
	if err != nil {
		t.Fatalf("BuildQueue: %v", err)
	}
	if len(q) != 1 {
		t.Fatalf("queue len=%d, want 1 (cap reduced by the 1 card introduced today)", len(q))
	}
	// Exhaust the budget: cap 1, one introduced → budget 0 → no new cards, none due.
	rv.Cfg.NewPerDay = 1
	q, _ = rv.BuildQueue("p", 6000, 0)
	if len(q) != 0 {
		t.Fatalf("queue len=%d, want 0 (daily budget exhausted)", len(q))
	}
}

func TestInterleaveAllSamePartKeepsAllAndNoMutation(t *testing.T) {
	cards := []store.Flashcard{{ID: 1, Part: "x"}, {ID: 2, Part: "x"}, {ID: 3, Part: "x"}}
	out := interleaveByPart(cards)
	if len(out) != 3 {
		t.Fatalf("len=%d, want 3 (all-same-part must not drop/dup)", len(out))
	}
	seen := map[int64]bool{}
	for _, c := range out {
		if seen[c.ID] {
			t.Fatalf("duplicate id %d in output", c.ID)
		}
		seen[c.ID] = true
	}
	if cards[0].ID != 1 || cards[1].ID != 2 || cards[2].ID != 3 {
		t.Fatalf("interleaveByPart mutated its input: %+v", cards)
	}
}
