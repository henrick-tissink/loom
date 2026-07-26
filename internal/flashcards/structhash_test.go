package flashcards

import "testing"

func TestStructuralHashNormalizesGo(t *testing.T) {
	// same code, different comments + whitespace → same structural hash
	a := "package p\n\n// alpha\nfunc F() int {\n\treturn 1\n}\n"
	b := "package p\n\nfunc F() int {\n\treturn 1 // beta\n}\n"
	if StructuralHash("pkg/x.go", a) != StructuralHash("pkg/x.go", b) {
		t.Fatalf("comment/whitespace edit changed the structural hash")
	}
	// a real code change → different hash
	c := "package p\nfunc F() int { return 2 }\n"
	if StructuralHash("pkg/x.go", a) == StructuralHash("pkg/x.go", c) {
		t.Fatalf("a real code change did not change the hash")
	}
	// non-Go source → raw hash
	if StructuralHash("docs/x.md#slug", "hello world") != Hash("hello world") {
		t.Fatalf("non-Go source should use the raw hash")
	}
	// unparseable Go (e.g. truncated) → raw fallback
	bad := "package p\nfunc F( { oops"
	if StructuralHash("x.go", bad) != Hash(bad) {
		t.Fatalf("unparseable Go should fall back to the raw hash")
	}
}

func TestStructuralHashSubsystemIgnoresCommentsAcrossFiles(t *testing.T) {
	sep := subsystemFilePrefix
	// a subsystem's Source: two files joined by the file separator. The fixtures
	// vary ONLY comments (block layout is held constant — go/printer preserves an
	// author's single-vs-multi-line block layout, which is a real structural signal).
	a := sep + "internal/s/one.go ====\npackage s\nfunc F() int { return 1 }\n" +
		sep + "internal/s/two.go ====\npackage s\nfunc G() bool { return true }\n"
	b := sep + "internal/s/one.go ====\npackage s\n// alpha\nfunc F() int { return 1 }\n" +
		sep + "internal/s/two.go ====\n// note\npackage s\nfunc G() bool { return true }\n"
	if StructuralHash("internal/s", a) != StructuralHash("internal/s", b) {
		t.Fatalf("comment/whitespace edits across a subsystem's files changed its hash")
	}
	// a real code change in one file → different hash
	c := sep + "internal/s/one.go ====\npackage s\nfunc F() int { return 2 }\n" +
		sep + "internal/s/two.go ====\npackage s\nfunc G() bool { return true }\n"
	if StructuralHash("internal/s", a) == StructuralHash("internal/s", c) {
		t.Fatalf("a real code change in a subsystem file did not change the hash")
	}
	// adding a file (a new separator/header) → different hash
	d := a + sep + "internal/s/three.go ====\npackage s\nfunc H() {}\n"
	if StructuralHash("internal/s", a) == StructuralHash("internal/s", d) {
		t.Fatalf("adding a file to a subsystem did not change the hash")
	}
}
