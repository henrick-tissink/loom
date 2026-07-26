package flashcards

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildManifestEnumeratesSubsystemsAndDocHeadings(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal", "status", "status.go"), "package status\nfunc Fuse() int { return 1 }\n")
	writeFile(t, filepath.Join(root, "internal", "status", "pane.go"), "package status\nfunc Pane() bool { return true }\n")
	writeFile(t, filepath.Join(root, "docs", "ARCHITECTURE.md"),
		"# Loom\nintro\n## Status\nhow status works\n## Data model\nthe db\n")

	parts, err := BuildManifest(root)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	var subsystems, docs int
	for _, p := range parts {
		switch p.Kind {
		case PartCode:
			t.Fatalf("no file-level code parts expected, got %+v", p)
		case PartSubsystem:
			subsystems++
			if p.Source == "" || p.SourceRef == "" {
				t.Fatalf("subsystem part missing source/ref: %+v", p)
			}
			if p.ID == "internal/status" {
				if p.Title != "status" {
					t.Fatalf("subsystem title = %q, want last dir segment 'status'", p.Title)
				}
				// the whole directory is fed at once, each file behind a separator
				if !contains(p.Source, subsystemFilePrefix) ||
					!contains(p.Source, "func Fuse()") || !contains(p.Source, "func Pane()") {
					t.Fatalf("subsystem Source should join both files: %q", p.Source)
				}
			}
		case PartDoc:
			docs++
		}
	}
	if subsystems != 1 { // internal/status is one subsystem (two files)
		t.Fatalf("want 1 subsystem part, got %d", subsystems)
	}
	if docs != 3 { // three headings: Loom, Status, Data model
		t.Fatalf("want 3 doc heading parts, got %d", docs)
	}
	// deterministic order
	for i := 1; i < len(parts); i++ {
		if parts[i-1].ID > parts[i].ID {
			t.Fatalf("parts not sorted by ID: %q > %q", parts[i-1].ID, parts[i].ID)
		}
	}
	// a doc part carries its heading section text and a #slug ref
	for _, p := range parts {
		if p.Kind == PartDoc && p.Title == "Status" {
			if p.SourceRef == "" || filepath.Ext(p.SourceRef) != "" && !contains(p.SourceRef, "#") {
				t.Fatalf("doc SourceRef should carry #slug: %q", p.SourceRef)
			}
			if !contains(p.Source, "how status works") {
				t.Fatalf("Status section should include its body, got %q", p.Source)
			}
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestBuildManifestHandlesFormattedAndDuplicateHeadings(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "g.md"),
		"# `internal/gui` API\nintro\n"+
			"```sh\n# install\n```\n"+
			"## Overview\nfirst overview\n"+
			"## Overview\nsecond overview\n")
	parts, err := BuildManifest(root) // must not panic on formatted/fenced/duplicate headings
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	var docCount int
	ids := map[string]bool{}
	for _, p := range parts {
		if p.Kind != PartDoc {
			continue
		}
		docCount++
		if ids[p.ID] {
			t.Fatalf("duplicate doc part ID: %q", p.ID)
		}
		ids[p.ID] = true
		if contains(p.Title, "`") || contains(p.Title, "#") {
			t.Fatalf("doc title not stripped of markdown: %q", p.Title)
		}
	}
	if docCount != 3 { // gui-API + two Overviews; the fenced "# install" is not a heading
		t.Fatalf("doc parts = %d, want 3", docCount)
	}
}

func TestBuildManifestDedupSurvivesNaturalSlugCollision(t *testing.T) {
	root := t.TempDir()
	// Third heading's natural slug ("overview-2") equals the synthesized slug the
	// second heading would take — the de-dup must still keep all three IDs unique.
	writeFile(t, filepath.Join(root, "docs", "h.md"),
		"## Overview\na\n## Overview\nb\n## Overview 2\nc\n")
	parts, err := BuildManifest(root)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	ids := map[string]bool{}
	var doc int
	for _, p := range parts {
		if p.Kind != PartDoc {
			continue
		}
		doc++
		if ids[p.ID] {
			t.Fatalf("duplicate doc part ID: %q", p.ID)
		}
		ids[p.ID] = true
	}
	if doc != 3 {
		t.Fatalf("doc parts = %d, want 3 unique", doc)
	}
}
