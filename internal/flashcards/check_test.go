package flashcards

import (
	"path/filepath"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func seedActive(t *testing.T, st *store.Store, project, part, anchor, srcHash string) int64 {
	t.Helper()
	id, ins, err := st.InsertFlashcard(store.Flashcard{
		Project: project, Part: part, Anchor: anchor, StemHash: anchor, Type: "code",
		Front: "q " + anchor, Back: "a", SourceRef: part, SourceHash: srcHash,
		Status: "active", CreatedAt: 1,
	})
	if err != nil || !ins {
		t.Fatalf("seed %s: %v", anchor, err)
	}
	return id
}

func TestCheckStaleFlagsDriftAndOrphans(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal", "a", "a.go"), "package a\nfunc F() int { return 1 }\n")

	// the current structural hash of the subsystem's part, from the manifest
	parts, err := BuildManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	var partID, curHash string
	for _, p := range parts {
		if p.Kind == PartSubsystem {
			partID, curHash = p.ID, StructuralHash(p.SourceRef, p.Source)
		}
	}
	if partID == "" {
		t.Fatal("manifest produced no subsystem part")
	}

	fresh := seedActive(t, st, "p", partID, "f1", curHash)   // matches → stays active
	drift := seedActive(t, st, "p", partID, "d1", "OLDHASH") // mismatch → stale
	orphan := seedActive(t, st, "p", "internal/gone", "o1", "x") // part gone → orphan

	res, err := CheckStale(st, "p", root, 1000)
	if err != nil {
		t.Fatalf("CheckStale: %v", err)
	}
	if res.Checked != 3 || res.Stale != 1 || res.Orphan != 1 {
		t.Fatalf("result = %+v, want Checked3 Stale1 Orphan1", res)
	}
	status := map[int64]string{}
	cards, _ := st.FlashcardsForProject("p")
	for _, c := range cards {
		status[c.ID] = c.Status
	}
	if status[fresh] != "active" {
		t.Fatalf("fresh card wrongly flagged: %s", status[fresh])
	}
	if status[drift] != "stale" || status[orphan] != "stale" {
		t.Fatalf("drift=%s orphan=%s, want both stale", status[drift], status[orphan])
	}
}

func TestChangedPartsDetectsDriftNonMutating(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal", "a", "a.go"), "package a\nfunc F() int { return 1 }\n")
	writeFile(t, filepath.Join(root, "internal", "b", "b.go"), "package b\nfunc G() int { return 2 }\n")
	parts, _ := BuildManifest(root)
	h := map[string]string{}
	for _, p := range parts {
		if p.Kind == PartSubsystem {
			h[p.ID] = StructuralHash(p.SourceRef, p.Source)
		}
	}
	// internal/a fresh (stored hash == current), internal/b drifted (old hash), gone orphan
	mk := func(part, anchor, srcHash string) {
		if _, _, err := st.InsertFlashcard(store.Flashcard{
			Project: "p", Part: part, Anchor: anchor, StemHash: anchor, Type: "code",
			Front: "q", Back: "a", SourceRef: part, SourceHash: srcHash, Status: "active", CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("internal/a", "f1", h["internal/a"])
	mk("internal/b", "d1", "OLDHASH")
	mk("internal/gone", "o1", "X")

	got, err := ChangedParts(st, "p", root)
	if err != nil {
		t.Fatalf("ChangedParts: %v", err)
	}
	if len(got) != 1 || got[0] != "internal/b" {
		t.Fatalf("ChangedParts = %v, want [internal/b] (internal/a fresh, gone orphan-excluded)", got)
	}
	// non-mutating: no card's status changed
	cards, _ := st.FlashcardsForProject("p")
	for _, c := range cards {
		if c.Status != "active" {
			t.Fatalf("ChangedParts mutated a status: %s -> %s", c.Anchor, c.Status)
		}
	}
}
