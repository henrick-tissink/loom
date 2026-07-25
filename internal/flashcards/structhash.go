package flashcards

import (
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
)

// StructuralHash fingerprints a card's source so that only a MEANINGFUL change
// marks its cards stale. For a Go source that parses, it hashes the go/printer
// reprint of the AST parsed WITHOUT comments — so a comment or whitespace edit
// normalizes away and does not churn. Non-Go sources, and Go that does not parse
// (e.g. a file truncated at maxCodeSource), fall back to hashing the raw text.
//
// This is FILE-level, not symbol-level: a card is stale if anything in its file
// changed. Symbol-level anchoring (spec §8, key by qualified symbol) needs a
// per-symbol manifest and is a deferred refinement.
func StructuralHash(sourceRef, source string) string {
	path := sourceRef
	if i := strings.IndexByte(path, '#'); i >= 0 {
		path = path[:i] // doc parts carry "path#slug"
	}
	if !strings.HasSuffix(path, ".go") {
		return Hash(source)
	}
	fset := token.NewFileSet()
	// Mode 0 (no ParseComments): comments are not attached to the AST, so the
	// printer omits them and only the code structure survives.
	f, err := parser.ParseFile(fset, "src.go", source, 0)
	if err != nil {
		return Hash(source)
	}
	var b strings.Builder
	cfg := &printer.Config{Mode: printer.RawFormat, Tabwidth: 8}
	if err := cfg.Fprint(&b, fset, f); err != nil {
		return Hash(source)
	}
	return Hash(b.String())
}
