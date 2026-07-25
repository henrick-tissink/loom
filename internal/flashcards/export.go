package flashcards

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"

	"github.com/henricktissink/loom/internal/store"
)

// ToCSV renders cards as Anki-importable CSV (columns Front,Back,Tags). Fields
// are RFC-4180 quoted by encoding/csv, so a comma or newline in card text can
// never break a row.
func ToCSV(cards []store.Flashcard) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"Front", "Back", "Tags"})
	for _, c := range cards {
		_ = w.Write([]string{c.Front, c.Back, ankiTag(c.Part) + " " + c.Type})
	}
	w.Flush()
	return b.String()
}

// ToMarkdown renders cards as a greppable study document grouped by part.
func ToMarkdown(cards []store.Flashcard) string {
	var b strings.Builder
	part := ""
	for _, c := range cards {
		if c.Part != part {
			if part != "" {
				b.WriteString("\n")
			}
			part = c.Part
			fmt.Fprintf(&b, "## %s\n\n", part)
		}
		fmt.Fprintf(&b, "**Q (%s):** %s\n\n**A:** %s\n\n", c.Type, c.Front, c.Back)
	}
	return b.String()
}

// ankiTag turns a manifest part into ONE hierarchical Anki tag. Anki separates
// tags on spaces and nests on "::", so slashes and "#" become "::", dots and
// spaces become "_" — keeping the source path browsable in Anki's tag tree.
func ankiTag(part string) string {
	t := strings.ReplaceAll(part, "/", "::")
	t = strings.ReplaceAll(t, "#", "::")
	t = strings.ReplaceAll(t, ".", "_")
	t = strings.ReplaceAll(t, " ", "_")
	return t
}
