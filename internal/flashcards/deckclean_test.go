package flashcards

import (
	"path/filepath"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func TestInspectAndCleanDeck(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal", "a", "a.go"), "package a\nfunc F() int { return 1 }\n")
	writeFile(t, filepath.Join(root, "internal", "b", "b.go"), "package b\nfunc G() {}\n")

	hashOf := func(id string) string {
		parts, _ := BuildManifest(root)
		for _, p := range parts {
			if p.ID == id {
				return StructuralHash(p.SourceRef, p.Source)
			}
		}
		return ""
	}
	seed := func(part, anchor, status, hash string) {
		if _, _, err := st.InsertFlashcard(store.Flashcard{
			Project: "p", Part: part, Anchor: anchor, StemHash: anchor, Type: "concept",
			Front: "q " + anchor, Back: "a", SourceRef: part, SourceHash: hash, Status: status, CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("internal/a", "cur1", "active", hashOf("internal/a")) // current — kept
	seed("internal/a", "cur2", "draft", hashOf("internal/a"))  // current draft — kept unless dropDrafts
	seed("internal/gone", "orph1", "active", "x")              // orphaned (part gone)
	seed("internal/gone", "orph2", "active", "x")
	seed("internal/b", "stale1", "active", "OLDHASH") // stale (source drifted)

	insp, err := InspectDeck(st, "p", root)
	if err != nil {
		t.Fatal(err)
	}
	if insp.Orphaned != 2 || insp.Stale != 1 || insp.Drafts != 1 {
		t.Fatalf("inspect = %+v, want orphaned2 stale1 drafts1", insp)
	}

	del, err := CleanDeck(st, "p", root, false)
	if err != nil {
		t.Fatal(err)
	}
	if del != 3 {
		t.Fatalf("deleted %d, want 3 (2 orphan + 1 stale)", del)
	}
	if rem, _ := st.FlashcardsForProject("p"); len(rem) != 2 {
		t.Fatalf("remaining %d, want 2 (current active + draft)", len(rem))
	}

	del2, err := CleanDeck(st, "p", root, true)
	if err != nil {
		t.Fatal(err)
	}
	if del2 != 1 {
		t.Fatalf("draft sweep deleted %d, want 1", del2)
	}
	rem2, _ := st.FlashcardsForProject("p")
	if len(rem2) != 1 || rem2[0].Anchor != "cur1" {
		t.Fatalf("remaining after drafts = %+v, want only cur1", rem2)
	}
}
