package flashcards

import (
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func TestAnkiTag(t *testing.T) {
	if g := ankiTag("internal/status/status.go"); g != "internal::status::status_go" {
		t.Fatalf("ankiTag = %q", g)
	}
	if g := ankiTag("docs/ARCHITECTURE.md#status"); g != "docs::ARCHITECTURE_md::status" {
		t.Fatalf("ankiTag doc = %q", g)
	}
}

func TestToCSVQuotesAndTags(t *testing.T) {
	cards := []store.Flashcard{
		{Part: "a/b.go", Type: "code", Front: "what, exactly?", Back: "line1\nline2"},
	}
	out := ToCSV(cards)
	lines := strings.SplitN(out, "\n", 2)
	if lines[0] != "Front,Back,Tags" {
		t.Fatalf("header = %q", lines[0])
	}
	// a comma in Front and a newline in Back must be quoted, not break the row
	if !strings.Contains(out, `"what, exactly?"`) || !strings.Contains(out, `"line1`+"\n"+`line2"`) {
		t.Fatalf("csv not quoted: %q", out)
	}
	if !strings.Contains(out, "a::b_go code") {
		t.Fatalf("tags missing: %q", out)
	}
}

func TestToMarkdownGroupsByPart(t *testing.T) {
	cards := []store.Flashcard{
		{Part: "a.go", Type: "code", Front: "q1", Back: "a1"},
		{Part: "a.go", Type: "code", Front: "q2", Back: "a2"},
		{Part: "b.go", Type: "decision", Front: "q3", Back: "a3"},
	}
	out := ToMarkdown(cards)
	if strings.Count(out, "## a.go") != 1 || strings.Count(out, "## b.go") != 1 {
		t.Fatalf("part headers wrong: %q", out)
	}
	if !strings.Contains(out, "**Q (code):** q1") || !strings.Contains(out, "**A:** a3") {
		t.Fatalf("q/a format wrong: %q", out)
	}
}
