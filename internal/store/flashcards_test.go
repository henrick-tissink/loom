package store

import "testing"

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir() + "/loom.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestInsertAndListFlashcards(t *testing.T) {
	st := openTestStore(t)
	c := Flashcard{
		Project: "loom", Part: "internal/status/status.go", Anchor: "loom|code|internal/status/status.go|Fuse",
		StemHash: "abc", Type: "code", Front: "What does Fuse return when the pane is active?",
		Back: "Running, in every branch.", SourceRef: "internal/status/status.go",
		SourceHash: "s1", AnswerHash: "a1", Status: "draft", CreatedAt: 100,
	}
	id, inserted, err := st.InsertFlashcard(c)
	if err != nil || !inserted || id == 0 {
		t.Fatalf("InsertFlashcard: id=%d inserted=%v err=%v", id, inserted, err)
	}
	// same (anchor, stem_hash) is a no-op dedup, not a second row
	_, inserted2, err := st.InsertFlashcard(c)
	if err != nil || inserted2 {
		t.Fatalf("dup insert: inserted=%v err=%v (want inserted=false)", inserted2, err)
	}
	got, err := st.FlashcardsForProject("loom")
	if err != nil {
		t.Fatalf("FlashcardsForProject: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	if got[0].Front != c.Front || got[0].Status != "draft" {
		t.Fatalf("round-trip mismatch: %+v", got[0])
	}
}

func TestMigrationIdempotent(t *testing.T) {
	dir := t.TempDir() + "/loom.db"
	st1, err := Open(dir)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	st1.Close()
	st2, err := Open(dir) // re-open: migrations must not re-run/error
	if err != nil {
		t.Fatalf("open2 (idempotent migrate): %v", err)
	}
	st2.Close()
}
