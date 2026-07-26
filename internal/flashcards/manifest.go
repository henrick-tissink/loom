package flashcards

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henricktissink/loom/internal/arch"
	"github.com/henricktissink/loom/internal/registry"
)

type PartKind string

const (
	PartCode      PartKind = "code"      // a single source file (reference-deck seam; not emitted by default)
	PartDoc       PartKind = "doc"       // a markdown heading section — "the why"
	PartSubsystem PartKind = "subsystem" // a directory of source files — the high-level unit
)

// Part is one node of the coverage manifest: a unit worth authoring cards over.
type Part struct {
	Kind      PartKind
	ID        string // project-relative path; doc parts append "#slug"; subsystem parts are a dir
	Title     string
	SourceRef string // what a card cites (equals ID for code/subsystem; path#slug for docs)
	Source    string // the text fed to generation
}

// codeExts is the source-file heuristic. Extension-based, not `go list`
// (language-agnostic enumeration; this is the seam later languages hook).
var codeExts = map[string]bool{".go": true}

const (
	maxSubsystemSource = 48_000 // bytes of joined source fed per subsystem part (bounds token cost)
	maxSubsystemCards  = 6      // cap on cards kept per subsystem (few, dense)
)

// subsystemFilePrefix begins each file block inside a subsystem part's Source.
// The full separator line is `subsystemFilePrefix + <rel> + " ====\n"`.
// StructuralHash splits on this exact prefix, so the two must stay in lockstep.
const subsystemFilePrefix = "// ==== loom:file "

// BuildManifest deterministically enumerates a project's parts: one SUBSYSTEM
// part per directory of source files (under the root and its child repos), and
// one doc part per markdown heading under <root>/docs. File-level code parts are
// no longer emitted — a subsystem (its whole directory, fed at once) is the unit
// for high-level, architectural cards. Result is sorted by ID.
func BuildManifest(projectRoot string) ([]Part, error) {
	roots := append([]string{projectRoot}, registry.ChildRepos(projectRoot)...)
	seen := map[string]bool{}
	filesByDir := map[string][]string{} // dir rel -> file rels (sorted at emit)
	content := map[string]string{}       // file rel -> file bytes
	var parts []Part

	for _, r := range roots {
		err := filepath.WalkDir(r, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				if d != nil && d.IsDir() && skipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !codeExts[filepath.Ext(p)] || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel := relTo(projectRoot, p)
			if seen[rel] {
				return nil
			}
			seen[rel] = true
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil // unreadable file: skip, not fatal
			}
			dir := path.Dir(rel) // slash-form rel → slash-form dir
			filesByDir[dir] = append(filesByDir[dir], rel)
			content[rel] = string(b)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	for dir, files := range filesByDir {
		sort.Strings(files)
		parts = append(parts, Part{
			Kind:      PartSubsystem,
			ID:        dir,
			Title:     subsystemTitle(dir),
			SourceRef: dir,
			Source:    subsystemSource(files, content),
		})
	}

	docsDir := filepath.Join(projectRoot, "docs")
	_ = filepath.WalkDir(docsDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".md" {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		rel := relTo(projectRoot, p)
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

// subsystemSource joins a directory's files into one blob, each prefixed by a
// separator that names the file (so structural hashing can normalize each file
// and detect add/remove/rename). Whole files are added in sorted order until the
// byte budget; the first file is always included (truncated if it alone exceeds
// the budget), and any remainder is noted so the count is visible, not silent.
func subsystemSource(files []string, content map[string]string) string {
	var b strings.Builder
	total := 0
	for i, f := range files {
		block := subsystemFilePrefix + f + " ====\n" + content[f]
		if !strings.HasSuffix(block, "\n") {
			block += "\n"
		}
		if total > 0 && total+len(block) > maxSubsystemSource {
			fmt.Fprintf(&b, "%s(%d more files omitted) ====\n", subsystemFilePrefix, len(files)-i)
			break
		}
		if total == 0 && len(block) > maxSubsystemSource {
			block = block[:maxSubsystemSource]
		}
		b.WriteString(block)
		total += len(block)
	}
	return b.String()
}

// subsystemTitle is the human label for a subsystem: its directory's last
// segment ("internal/status" → "status"); the project root maps to "(root)".
func subsystemTitle(dir string) string {
	if dir == "." || dir == "" {
		return "(root)"
	}
	return path.Base(dir)
}

func relTo(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
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
