package flashcards

import (
	"bytes"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func TestRunCLIExport(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	proj := projectName(root)
	if _, _, err := st.InsertFlashcard(store.Flashcard{
		Project: proj, Part: "a.go", Anchor: "e1", StemHash: "e1", Type: "code",
		Front: "front, with comma", Back: "the answer", Status: "active", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// default csv
	var csvBuf bytes.Buffer
	if err := RunCLI([]string{"export", root}, st, "claude", root, 1, strings.NewReader(""), &csvBuf); err != nil {
		t.Fatalf("export csv: %v", err)
	}
	if !strings.Contains(csvBuf.String(), "Front,Back,Tags") || !strings.Contains(csvBuf.String(), `"front, with comma"`) {
		t.Fatalf("csv export wrong: %q", csvBuf.String())
	}

	// md
	var mdBuf bytes.Buffer
	if err := RunCLI([]string{"export", root, "md"}, st, "claude", root, 1, strings.NewReader(""), &mdBuf); err != nil {
		t.Fatalf("export md: %v", err)
	}
	if !strings.Contains(mdBuf.String(), "## a.go") || !strings.Contains(mdBuf.String(), "**A:** the answer") {
		t.Fatalf("md export wrong: %q", mdBuf.String())
	}

	// unknown format errors
	if err := RunCLI([]string{"export", root, "pdf"}, st, "claude", root, 1, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("unknown format should error")
	}
}
