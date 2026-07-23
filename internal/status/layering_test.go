package status

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoOrchestratorKnowledgeInStatus is orchestrator spec §13's layering
// clause, asserted as a property of the source rather than as prose.
//
// The claim it pins: "an orchestrator session polls, transitions, hides and
// resumes with zero orchestrator-specific code in status or ui". An
// orchestrator IS an ordinary claude session — one store row, one transcript,
// one tmux window, one `orch` tag — and the engine must stay entirely unaware
// of that tag. The property that makes the claim TRUE is that
// internal/orchestrator adds nothing to these packages, so the test that can
// actually fail is this one: it fails the moment someone special-cases an
// orchestrator here.
//
// Why a source scan and not a behavioural test: the behaviour of "no special
// case" is unobservable — every behavioural assertion about an orchestrator
// session in this package is identical to the one about a plain session, and
// would keep passing after a special case was added for some OTHER reason.
// The import edge and the tag literal are the things that cannot be there.
func TestNoOrchestratorKnowledgeInStatus(t *testing.T) {
	assertNoOrchestratorKnowledge(t, ".")
}

// assertNoOrchestratorKnowledge is shared with internal/ui's copy of this test.
// Restated in each package rather than lifted into a helper package because a
// test that guards a LAYER has to live inside the layer it guards — a shared
// helper would import both and become the coupling it exists to forbid.
func assertNoOrchestratorKnowledge(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// This file necessarily names the thing it forbids.
		if name == "layering_test.go" {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			if strings.Contains(imp.Path.Value, "internal/orchestrator") {
				t.Errorf("%s imports internal/orchestrator: this layer must not know "+
					"an orchestrator exists (orchestrator spec §13)", path)
			}
			if strings.Contains(imp.Path.Value, "internal/delegate") {
				t.Errorf("%s imports internal/delegate: delegation attribution is a DTO-layer "+
					"wrapper (delegation spec §14.1), never an engine concern", path)
			}
		}

		// The `orch` tag is the other way the knowledge leaks in — a tag test
		// needs no import at all.
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{`"orch"`, "`orch`", `"dlg:`} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s mentions %s: session tags are not this layer's business; "+
					"an orchestrator or delegation child must poll and transition "+
					"exactly like any other session", path, bad)
			}
		}
	}
}
