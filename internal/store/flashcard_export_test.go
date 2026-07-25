package store

import "testing"

func TestExportCardsActiveOnlyOrdered(t *testing.T) {
	st := openTestStore(t)
	seedCard(t, st, "p", "b.go", "b1", "active")
	seedCard(t, st, "p", "a.go", "a1", "active")
	seedCard(t, st, "p", "a.go", "a2", "draft") // excluded
	got, err := st.ExportCards("p")
	if err != nil {
		t.Fatalf("ExportCards: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("exported %d cards, want 2 (active only)", len(got))
	}
	if got[0].Part != "a.go" || got[1].Part != "b.go" {
		t.Fatalf("not ordered by part: %s then %s", got[0].Part, got[1].Part)
	}
}
