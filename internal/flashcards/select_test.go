package flashcards

import "testing"

func TestSelectPartsByScope(t *testing.T) {
	parts := []Part{
		{Kind: PartCode, ID: "internal/a.go"},
		{Kind: PartCode, ID: "internal/b.go"},
		{Kind: PartDoc, ID: "docs/ARCHITECTURE.md#status"},
		{Kind: PartDoc, ID: "docs/ARCHITECTURE.md#data"},
	}
	covered := map[string]bool{"internal/a.go": true}

	ids := func(ps []Part) []string {
		var s []string
		for _, p := range ps {
			s = append(s, p.ID)
		}
		return s
	}
	eq := func(got []Part, want ...string) bool {
		g := ids(got)
		if len(g) != len(want) {
			return false
		}
		for i := range g {
			if g[i] != want[i] {
				return false
			}
		}
		return true
	}

	if !eq(SelectParts(parts, ScopeDocs, "", nil), "docs/ARCHITECTURE.md#status", "docs/ARCHITECTURE.md#data") {
		t.Fatalf("docs scope = %v", ids(SelectParts(parts, ScopeDocs, "", nil)))
	}
	if !eq(SelectParts(parts, ScopeUncovered, "", covered), "internal/b.go", "docs/ARCHITECTURE.md#status", "docs/ARCHITECTURE.md#data") {
		t.Fatalf("uncovered scope = %v", ids(SelectParts(parts, ScopeUncovered, "", covered)))
	}
	if len(SelectParts(parts, ScopeAll, "", nil)) != 4 {
		t.Fatalf("all scope should be 4")
	}
	if !eq(SelectParts(parts, ScopePath, "a.go", nil), "internal/a.go") {
		t.Fatalf("path scope = %v", ids(SelectParts(parts, ScopePath, "a.go", nil)))
	}
	if len(SelectParts(parts, ScopePath, "", nil)) != 0 {
		t.Fatal("empty path filter selects nothing")
	}
}
