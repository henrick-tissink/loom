package flashcards

import (
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
		for _, sec := range headingSections(string(b)) {
			parts = append(parts, Part{
				Kind: PartDoc, ID: rel + "#" + sec.slug, Title: sec.title,
				SourceRef: rel + "#" + sec.slug, Source: sec.body,
			})
		}
		return nil
	})

	sort.Slice(parts, func(i, j int) bool { return parts[i].ID < parts[j].ID })
	return parts, nil
}

type section struct{ title, slug, body string }

// headingSections splits markdown into one section per heading using arch.Render
// (reused parser, heading Slugs included). A section's body is the source text
// from the heading up to the next heading of the same or higher level.
func headingSections(src string) []section {
	blocks := arch.Render(src)
	lines := strings.Split(src, "\n")
	// find the source line index of each heading, in order
	type h struct {
		title, slug string
		level, line int
	}
	var hs []h
	li := 0
	for _, b := range blocks {
		if b.Kind != arch.BlockHeading {
			continue
		}
		title := inlineText(b)
		for li < len(lines) {
			if isHeadingLine(lines[li], title) {
				break
			}
			li++
		}
		hs = append(hs, h{title: title, slug: b.Slug, level: b.Level, line: li})
		li++
	}
	var out []section
	for i, cur := range hs {
		end := len(lines)
		for j := i + 1; j < len(hs); j++ {
			if hs[j].level <= cur.level {
				end = hs[j].line
				break
			}
		}
		body := strings.Join(lines[cur.line:end], "\n")
		out = append(out, section{title: cur.title, slug: cur.slug, body: body})
	}
	return out
}

func isHeadingLine(line, title string) bool {
	t := strings.TrimLeft(line, "#")
	return strings.HasPrefix(line, "#") && strings.TrimSpace(t) == title
}

func inlineText(b arch.Block) string {
	var sb strings.Builder
	writeInline(&sb, b.Inline)
	return strings.TrimSpace(sb.String())
}

// writeInline flattens inline nodes to plain text, recursing into Children so
// an emphasized/linked word in a heading (arch.Inline{Text:"", Children:...})
// is not dropped.
func writeInline(sb *strings.Builder, ins []arch.Inline) {
	for _, in := range ins {
		sb.WriteString(in.Text)
		writeInline(sb, in.Children)
	}
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
