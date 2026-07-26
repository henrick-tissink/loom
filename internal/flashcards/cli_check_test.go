package flashcards

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func TestRunCLICheck(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal", "a", "a.go"), "package a\nfunc F() int { return 1 }\n")
	proj := projectName(root)
	// an active card for the internal/a subsystem carrying a stale source hash
	if _, _, err := st.InsertFlashcard(store.Flashcard{
		Project: proj, Part: "internal/a", Anchor: "c1", StemHash: "c1", Type: "code",
		Front: "q", Back: "a", SourceRef: "internal/a", SourceHash: "OLD", Status: "active", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := RunCLI([]string{"check", root}, st, "claude", root, 1, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.Contains(buf.String(), "1 stale") {
		t.Fatalf("report = %q, want to mention '1 stale'", buf.String())
	}
	cards, _ := st.FlashcardsForProject(proj)
	if cards[0].Status != "stale" {
		t.Fatalf("card not flagged stale: %s", cards[0].Status)
	}
}
