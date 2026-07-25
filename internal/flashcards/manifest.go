package flashcards

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henricktissink/loom/internal/arch"
	"github.com/henricktissink/loom/internal/registry"
)

type PartKind string

const (
	PartCode PartKind = "code"
	PartDoc  PartKind = "doc"
)

// Part is one node of the coverage manifest: a unit worth authoring cards over.
type Part struct {
	Kind      PartKind
	ID        string // project-relative path; doc parts append "#slug"
	Title     string
	SourceRef string // what a card cites (equals ID for code; path#slug for docs)
	Source    string // the text fed to generation
}

// codeExts is the Slice-1 source-file heuristic. Extension-based, not `go list`
// (spec §3: language-agnostic enumeration; this is the seam later languages hook).
var codeExts = map[string]bool{".go": true}

const maxCodeSource = 24_000 // bytes fed per code part (bounds token cost)

// BuildManifest deterministically enumerates a project's parts: one code part
// per source file under the root and its child repos, and one doc part per
// markdown heading under <root>/docs. Result is sorted by ID.
func BuildManifest(projectRoot string) ([]Part, error) {
	roots := append([]string{projectRoot}, registry.ChildRepos(projectRoot)...)
	seen := map[string]bool{}
	var parts []Part

	for _, r := range roots {
		err := filepath.WalkDir(r, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				if d != nil && d.IsDir() && skipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !codeExts[filepath.Ext(path)] || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel := relTo(projectRoot, path)
			if seen[rel] {
				return nil
			}
			seen[rel] = true
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil // unreadable file: skip, not fatal
			}
			src := string(b)
			if len(src) > maxCodeSource {
				src = src[:maxCodeSource]
			}
			parts = append(parts, Part{Kind: PartCode, ID: rel, Title: rel, SourceRef: rel, Source: src})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	docsDir := filepath.Join(projectRoot, "docs")
	_ = filepath.WalkDir(docsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel := relTo(projectRoot, path)
		// usedSlugs holds slugs already EMITTED for this doc (not raw occurrence
		// counts): a synthesized "foo-2" can equal another heading's natural
		// slug, so uniqueness must be checked against what was actually assigned.
		usedSlugs := map[string]bool{}
		for _, h := range arch.Headings(string(b)) {
			slug := h.Slug
			for n := 2; usedSlugs[slug]; n++ {
				slug = fmt.Sprintf("%s-%d", h.Slug, n)
			}
			usedSlugs[slug] = true
			ref := rel + "#" + slug
			parts = append(parts, Part{Kind: PartDoc, ID: ref, Title: h.Text, SourceRef: ref, Source: h.Body})
		}
		return nil
	})

	sort.SliceStable(parts, func(i, j int) bool { return parts[i].ID < parts[j].ID })
	return parts, nil
}

func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".loom":
		return true
	}
	return false
}
