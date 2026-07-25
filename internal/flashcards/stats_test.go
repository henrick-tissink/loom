package flashcards

import "testing"

func TestReviewerPassRateAndCoverage(t *testing.T) {
	st := openStore(t)
	rv := &Reviewer{Store: st, Cfg: DefaultReviewConfig()}
	id := active(t, st, "a.go", "c1")

	if rate, n, err := rv.PassRate("p", 0); err != nil || n != 0 || rate != 0 {
		t.Fatalf("empty PassRate: rate=%v n=%d err=%v (want 0,0)", rate, n, err)
	}
	rv.Record(id, GradeGood, false, 1000) // pass
	rv.Record(id, GradeAgain, true, 2000) // fail
	rate, n, err := rv.PassRate("p", 0)
	if err != nil || n != 2 || rate != 0.5 {
		t.Fatalf("PassRate: rate=%v n=%d err=%v (want 0.5, 2)", rate, n, err)
	}
	cov, err := rv.Coverage("p", 3000)
	if err != nil || len(cov) != 1 || cov[0].Active != 1 {
		t.Fatalf("Coverage: %+v err=%v", cov, err)
	}
}
