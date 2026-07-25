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
