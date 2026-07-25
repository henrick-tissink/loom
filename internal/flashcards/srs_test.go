package flashcards

import "testing"

func TestScheduleProgressionAndReset(t *testing.T) {
	r := NewReview(1)
	if r.Ease != 2.5 || r.Reps != 0 {
		t.Fatalf("NewReview: %+v", r)
	}
	// first Good → 1 day, reps 1
	r = Schedule(r, GradeGood, 1000)
	if r.Reps != 1 || r.Interval != 1 || r.DueAt != 1000+86400 {
		t.Fatalf("after 1st Good: %+v", r)
	}
	// second Good → 6 days
	r = Schedule(r, GradeGood, 2000)
	if r.Reps != 2 || r.Interval != 6 {
		t.Fatalf("after 2nd Good: interval=%d reps=%d", r.Interval, r.Reps)
	}
	// third Good → 6 * ease(2.5) = 15
	r = Schedule(r, GradeGood, 3000)
	if r.Interval != 15 {
		t.Fatalf("after 3rd Good: interval=%d, want 15", r.Interval)
	}
	// Again resets reps to 0, bumps lapses, drops ease, interval 1
	prevEase := r.Ease
	r = Schedule(r, GradeAgain, 4000)
	if r.Reps != 0 || r.Lapses != 1 || r.Interval != 1 || r.Ease >= prevEase {
		t.Fatalf("after Again: %+v (ease was %.2f)", r, prevEase)
	}
}

func TestScheduleGradeOrderingAndEaseFloor(t *testing.T) {
	// From an identical mature state, Hard < Good < Easy interval.
	base := Review{CardID: 1, Ease: 2.5, Interval: 20, Reps: 5}
	hard := Schedule(base, GradeHard, 0).Interval
	good := Schedule(base, GradeGood, 0).Interval
	easy := Schedule(base, GradeEasy, 0).Interval
	if !(hard < good && good < easy) {
		t.Fatalf("ordering broken: hard=%d good=%d easy=%d", hard, good, easy)
	}
	// Ease never drops below the floor even after many Again/Hard.
	r := NewReview(1)
	for i := 0; i < 30; i++ {
		r = Schedule(r, GradeAgain, int64(i))
	}
	if r.Ease < 1.3 {
		t.Fatalf("ease floor breached: %.3f", r.Ease)
	}
	// An out-of-range grade is a no-op (caller validates).
	unchanged := Schedule(base, Grade(9), 0)
	if unchanged != base {
		t.Fatalf("unknown grade mutated state: %+v", unchanged)
	}
	if ValidGrade(9) || !ValidGrade(GradeGood) {
		t.Fatal("ValidGrade wrong")
	}
}

// Near the ease floor with a small interval, round() would tie Hard and Good;
// flooring Hard keeps the ordering strict. (Counterexample: prev=8, ease=1.3.)
func TestScheduleHardStrictlyBelowGoodNearFloor(t *testing.T) {
	base := Review{CardID: 1, Ease: 1.3, Interval: 8, Reps: 5}
	hard := Schedule(base, GradeHard, 0).Interval
	good := Schedule(base, GradeGood, 0).Interval
	easy := Schedule(base, GradeEasy, 0).Interval
	if !(hard < good && good <= easy) {
		t.Fatalf("near floor ordering: hard=%d good=%d easy=%d (want hard < good <= easy)", hard, good, easy)
	}
}
