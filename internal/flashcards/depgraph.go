package flashcards

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henricktissink/loom/internal/arch"
	"github.com/henricktissink/loom/internal/store"
)

// DepNode is one subsystem in the dependency graph: a laid-out position, its
// card coverage, and whether its source has drifted since its cards were made.
type DepNode struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Cards   int    `json:"cards"`
	Changed bool   `json:"changed"`
}

// DepEdge is a directed import between two subsystems.
type DepEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// DepGraph is the subsystem import graph with coverage + drift overlaid.
type DepGraph struct {
	Nodes  []DepNode `json:"nodes"`
	Edges  []DepEdge `json:"edges"`
	Width  int       `json:"width"`
	Height int       `json:"height"`
	NodeW  int       `json:"nodeW"`
	NodeH  int       `json:"nodeH"`
}

// BuildDepGraph walks a Go project's internal import graph over its subsystems,
// lays it out with arch.Layout (the same deterministic layout the orchestration
// DAG uses), and overlays each node with its flashcard coverage and drift state.
// Non-Go projects (no go.mod) yield nodes with no edges.
func BuildDepGraph(st *store.Store, project, projectRoot string) (DepGraph, error) {
	out := DepGraph{Nodes: []DepNode{}, Edges: []DepEdge{}, NodeW: arch.NodeW, NodeH: arch.NodeH}
	parts, err := BuildManifest(projectRoot)
	if err != nil {
		return out, err
	}
	module := moduleOf(projectRoot)
	var subs []Part
	subByImport := map[string]string{} // import path -> subsystem ID
	for _, p := range parts {
		if p.Kind != PartSubsystem {
			continue
		}
		subs = append(subs, p)
		if module != "" {
			subByImport[module+"/"+p.ID] = p.ID
		}
	}
	if len(subs) == 0 {
		return out, nil
	}

	edgeSet := map[[2]string]bool{}
	for _, p := range subs {
		for _, imp := range parseImports(filepath.Join(projectRoot, filepath.FromSlash(p.ID))) {
			if to, ok := subByImport[imp]; ok && to != p.ID {
				edgeSet[[2]string{p.ID, to}] = true
			}
		}
	}

	lnodes := make([]arch.LayoutNode, 0, len(subs))
	for _, p := range subs {
		lnodes = append(lnodes, arch.LayoutNode{ID: p.ID})
	}
	ledges := make([]arch.LayoutEdge, 0, len(edgeSet))
	for e := range edgeSet {
		ledges = append(ledges, arch.LayoutEdge{From: e[0], To: e[1]})
	}
	placed, w, h := arch.Layout(lnodes, ledges)
	pos := make(map[string]arch.Placement, len(placed))
	for _, pl := range placed {
		pos[pl.ID] = pl
	}
	out.Width, out.Height = w, h

	cards, _ := st.FlashcardsForProject(project)
	cardCount := map[string]int{}
	for _, c := range cards {
		cardCount[c.Part]++
	}
	changedSet := map[string]bool{}
	if changed, cerr := ChangedParts(st, project, projectRoot); cerr == nil {
		for _, c := range changed {
			changedSet[c] = true
		}
	}

	for _, p := range subs {
		pl := pos[p.ID]
		out.Nodes = append(out.Nodes, DepNode{
			ID: p.ID, Title: p.Title, X: pl.X, Y: pl.Y,
			Cards: cardCount[p.ID], Changed: changedSet[p.ID],
		})
	}
	for e := range edgeSet {
		out.Edges = append(out.Edges, DepEdge{From: e[0], To: e[1]})
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	sort.Slice(out.Edges, func(i, j int) bool {
		if out.Edges[i].From != out.Edges[j].From {
			return out.Edges[i].From < out.Edges[j].From
		}
		return out.Edges[i].To < out.Edges[j].To
	})
	return out, nil
}

// parseImports returns the import paths of a directory's non-test Go files.
func parseImports(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, imp := range f.Imports {
			out = append(out, strings.Trim(imp.Path.Value, `"`))
		}
	}
	return out
}

// moduleOf reads the module path from a project's go.mod, or "" if absent.
func moduleOf(projectRoot string) string {
	b, err := os.ReadFile(filepath.Join(projectRoot, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(s, "module "))
		}
	}
	return ""
}
