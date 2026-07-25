# Flashcards — Slice 4 Implementation Plan (Anki / CSV / Markdown export)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Let a curated deck leave loom for a real spaced-repetition surface — Anki-importable CSV (so daily review can happen on a phone, where SRS actually works) and a greppable Markdown study document. Provable from `loom flashcards export`.

**Architecture:** A store query returns the active deck; pure functions in `internal/flashcards/export.go` render it to CSV (via `encoding/csv`) and Markdown; a `runExport` CLI verb writes the chosen format to the CLI's `out` writer (stdout, redirectable). No GUI, no schema change.

**Tech Stack:** Go 1.26, stdlib `encoding/csv`. Tests seed a `store.Store` and assert rendered output.

## Global Constraints

- Module `github.com/henricktissink/loom`. Export covers the ACTIVE deck only (`status='active'`) — drafts and suspended cards are not exported. Deterministic order: `ORDER BY part, id`.
- Anki semantics: Anki separates tags on spaces and builds tag hierarchy on `::`. So a part path becomes ONE hierarchical tag (slashes/`#` → `::`, dots → `_`, spaces → `_`) and the card type is a second, space-separated tag.
- CSV uses `encoding/csv` (RFC-4180 quoting) so a comma or newline in card text can never break a row. Columns: `Front,Back,Tags`.
- The `export` verb prints to the CLI's `out io.Writer` (stdout) — the caller redirects to a file (`> deck.csv`). No file I/O in the exporter.
- Reuse the existing `store.Flashcard`, `flashcardCols`, `scanCards`, and the `RunCLI` dispatch. No new third-party dependencies.

---

### Task 1: Exporter (pure) + store query

**Files:**
- Create: `internal/store/flashcard_export.go`
- Create: `internal/flashcards/export.go`
- Test: `internal/store/flashcard_export_test.go`, `internal/flashcards/export_test.go`

**Interfaces:**
- Produces (store): `func (s *Store) ExportCards(project string) ([]Flashcard, error)` — `status='active'`, `ORDER BY part, id`, via `scanCards`.
- Produces (flashcards):
  - `func ToCSV(cards []store.Flashcard) string` — `encoding/csv`, header `Front,Back,Tags`.
  - `func ToMarkdown(cards []store.Flashcard) string` — grouped by `## <part>`, `**Q (<type>):** … / **A:** …`.
  - `func ankiTag(part string) string` — part → single hierarchical Anki tag.

- [ ] **Step 1: Write the failing tests**

`internal/store/flashcard_export_test.go`:
```go
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
```

`internal/flashcards/export_test.go`:
```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run ExportCards -v` and `go test ./internal/flashcards/ -run 'AnkiTag|ToCSV|ToMarkdown' -v`
Expected: FAIL — undefined `ExportCards`/`ToCSV`/`ToMarkdown`/`ankiTag`.

- [ ] **Step 3a: Store query** (`internal/store/flashcard_export.go`)

```go
package store

// ExportCards returns a project's ACTIVE deck (drafts and suspended cards
// excluded), ordered by part then id — the deterministic set an export renders.
func (s *Store) ExportCards(project string) ([]Flashcard, error) {
	rows, err := s.db.Query("SELECT "+flashcardCols+" FROM flashcards WHERE project=? AND status='active' ORDER BY part, id", project)
	if err != nil {
		return nil, err
	}
	return scanCards(rows)
}
```

- [ ] **Step 3b: Exporter** (`internal/flashcards/export.go`)

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ ./internal/flashcards/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/flashcard_export.go internal/store/flashcard_export_test.go internal/flashcards/export.go internal/flashcards/export_test.go
git commit -m "feat(flashcards): Anki-CSV + Markdown exporter for the active deck"
```

---

### Task 2: `export` CLI verb

**Files:**
- Modify: `internal/flashcards/cli.go` (add `export` to `RunCLI`'s dispatch + `runExport`)
- Test: `internal/flashcards/cli_export_test.go`

**Interfaces:**
- Consumes: `store.ExportCards` (Task 1), `ToCSV`/`ToMarkdown` (Task 1).
- Produces:
  - `RunCLI` gains `case "export": return runExport(args, st, project, out)`.
  - `func runExport(args []string, st *store.Store, project string, out io.Writer) error` — `export <projectRoot> [csv|md]` (default `csv`); writes the rendered deck to `out`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flashcards/ -run RunCLIExport -v`
Expected: FAIL — unknown verb / `undefined: runExport`.

- [ ] **Step 3: Implement**

In `internal/flashcards/cli.go`, add the dispatch case in `RunCLI`'s `switch verb` (alongside `stats`):

```go
	case "export":
		return runExport(args, st, project, out)
```

Add the handler (near `runStats`):

```go
// runExport writes the project's active deck to out in the chosen format:
// export <projectRoot> [csv|md]. csv is Anki-importable; md is a greppable
// study document. The caller redirects to a file.
func runExport(args []string, st *store.Store, project string, out io.Writer) error {
	format := "csv"
	if len(args) > 2 {
		format = args[2]
	}
	cards, err := st.ExportCards(project)
	if err != nil {
		return err
	}
	switch format {
	case "csv":
		fmt.Fprint(out, ToCSV(cards))
	case "md", "markdown":
		fmt.Fprint(out, ToMarkdown(cards))
	default:
		return fmt.Errorf("unknown export format %q (want csv or md)", format)
	}
	return nil
}
```

Update the usage string in `RunCLI` (the `len(args) < 2` error) to include `export`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/flashcards/ && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/flashcards/cli.go internal/flashcards/cli_export_test.go
git commit -m "feat(flashcards): loom flashcards export (csv|md) CLI verb"
```

---

## Self-Review

**Spec coverage (Slice 4 — Anki/CSV export, spec §10):**
- Anki-importable CSV (retention leaves loom for a real SRS) → Task 1 `ToCSV` + `ankiTag`. ✓
- Greppable Markdown study document → Task 1 `ToMarkdown`. ✓
- Provable from the CLI → Task 2 `export` verb. ✓
- Active deck only, deterministic order → Task 1 `ExportCards`. ✓
- Deferred (later / follow-on): a GUI "Export" button (a small 3b-style add + a bridge method); a true `.apkg` package (CSV import is the pragmatic, robust v1 — Anki imports CSV natively with field mapping). Correctly out.

**Placeholder scan:** No TBD/TODO; complete code and commands in every step.

**Type consistency:** `store.Flashcard` (existing) flows into `ToCSV`/`ToMarkdown` unchanged. `ExportCards` reuses `flashcardCols`/`scanCards`. `runExport` reuses the `RunCLI(args, st, binary, workDir, now, in, out)` signature — export ignores `binary`/`workDir`/`now`/`in`, matching how `runStats` ignores `binary`/`workDir`/`in`. `ankiTag` replacement order (`/`→`::`, `#`→`::`, `.`→`_`, ` `→`_`) is asserted by `TestAnkiTag`.
