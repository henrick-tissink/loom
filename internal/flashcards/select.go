package flashcards

import "strings"

// GenScope names a no-path generation preset (spec: "be clever, no path"). The
// point is the user doesn't enumerate files — they pick a semantic scope and the
// manifest supplies the parts.
type GenScope string

const (
	ScopeDocs      GenScope = "docs"      // the "why": doc SECTIONS only (arch headings, specs)
	ScopeUncovered GenScope = "uncovered" // parts that have no cards yet
	ScopeAll       GenScope = "all"       // every manifest part (whole project)
	ScopePath      GenScope = "path"      // parts whose ID contains a path filter (targeted)
)

// SelectParts chooses which manifest parts a generation job covers, given a
// scope and — for ScopeUncovered — the set of part IDs that already have cards.
// Deterministic; preserves manifest order. Unknown scopes select nothing.
func SelectParts(parts []Part, scope GenScope, filter string, covered map[string]bool) []Part {
	var out []Part
	for _, p := range parts {
		keep := false
		switch scope {
		case ScopeDocs:
			keep = p.Kind == PartDoc
		case ScopeUncovered:
			keep = !covered[p.ID]
		case ScopeAll:
			keep = true
		case ScopePath:
			keep = filter != "" && strings.Contains(p.ID, filter)
		}
		if keep {
			out = append(out, p)
		}
	}
	return out
}
