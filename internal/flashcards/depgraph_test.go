package flashcards

import (
	"path/filepath"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func TestBuildDepGraph(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/m\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "internal", "a", "a.go"),
		"package a\nimport _ \"example.com/m/internal/b\"\nfunc F() {}\n")
	writeFile(t, filepath.Join(root, "internal", "b", "b.go"), "package b\nfunc G() {}\n")
	// a card for internal/a carrying a stale hash → node flagged changed
	if _, _, err := st.InsertFlashcard(store.Flashcard{
		Project: "p", Part: "internal/a", Anchor: "a1", StemHash: "a1", Type: "concept",
		Front: "q", Back: "a", SourceRef: "internal/a", SourceHash: "OLD", Status: "active", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	g, err := BuildDepGraph(st, "p", root)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(g.Nodes))
	}
	if len(g.Edges) != 1 || g.Edges[0].From != "internal/a" || g.Edges[0].To != "internal/b" {
		t.Fatalf("edges = %+v, want internal/a -> internal/b", g.Edges)
	}
	var a DepNode
	for _, n := range g.Nodes {
		if n.ID == "internal/a" {
			a = n
		}
	}
	if a.Cards != 1 {
		t.Fatalf("internal/a cards = %d, want 1", a.Cards)
	}
	if !a.Changed {
		t.Fatal("internal/a should be flagged changed (stale source hash)")
	}
	if g.NodeW == 0 || g.Width == 0 {
		t.Fatalf("layout dims not populated: %+v", g)
	}
}
