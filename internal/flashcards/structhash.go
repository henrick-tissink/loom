package flashcards

import (
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
)

// StructuralHash fingerprints a card's source so that only a MEANINGFUL change
// marks its cards stale. For a Go source that parses, it hashes the go/printer
// reprint of the AST parsed WITHOUT comments — so a comment edit, and
// indentation/spacing WITHIN the existing layout, normalize away and do not
// churn. (go/printer, like gofmt, preserves an author's single-line-vs-multi-line
// block layout from the retained token positions, so a reformat that changes
// THAT still churns — rare in already-gofmt'd code.) Non-Go sources, and Go that
// does not parse (e.g. a file truncated at a byte budget), fall back to the raw text.
//
// Three shapes by SourceRef:
//   - a doc ref ("path#slug") → raw hash of the section text
//   - a single-file code ref ("pkg/x.go") → hash of the file's normalized reprint
//   - a subsystem ref (a directory, "internal/status") → the Source is the dir's
//     files joined by subsystemFilePrefix separators; each Go file is normalized
//     independently and the rejoined normalization is hashed, so a comment edit in
//     ANY file of the subsystem does not churn, but adding/removing/renaming a file,
//     or a structural code change, does.
func StructuralHash(sourceRef, source string) string {
	if strings.IndexByte(sourceRef, '#') >= 0 {
		return Hash(source) // doc part: "path#slug"
	}
	if strings.HasSuffix(sourceRef, ".go") {
		return Hash(normalizeGo(source)) // single Go file
	}
	return Hash(normalizeSubsystem(source)) // directory of files
}

// normalizeGo reprints Go source from its comment-free AST; unparseable source
// (e.g. a truncated file) falls back to the raw text.
func normalizeGo(source string) string {
	fset := token.NewFileSet()
	// Mode 0 (no ParseComments): comments are not attached to the AST, so the
	// printer omits them and only the code structure survives.
	f, err := parser.ParseFile(fset, "src.go", source, 0)
	if err != nil {
		return source
	}
	var b strings.Builder
	cfg := &printer.Config{Mode: printer.RawFormat, Tabwidth: 8}
	if err := cfg.Fprint(&b, fset, f); err != nil {
		return source
	}
	return b.String()
}

// normalizeSubsystem normalizes a subsystem's joined-files Source: it splits on
// the file separator, keeps each file's header line (so a rename/add/remove
// churns the hash), and normalizes each file's Go body (so comment edits do not).
// A Source with no separator (not a subsystem shape) hashes as-is.
func normalizeSubsystem(source string) string {
	if !strings.Contains(source, subsystemFilePrefix) {
		return source
	}
	var b strings.Builder
	for _, seg := range strings.Split(source, subsystemFilePrefix) {
		if seg == "" {
			continue
		}
		nl := strings.IndexByte(seg, '\n')
		if nl < 0 {
			b.WriteString(seg) // header with no body (e.g. the "N omitted" marker)
			b.WriteByte('\n')
			continue
		}
		header, body := seg[:nl], seg[nl+1:]
		b.WriteString(header) // "<rel> ====" — file identity is part of the fingerprint
		b.WriteByte('\n')
		b.WriteString(normalizeGo(body))
		b.WriteByte('\n')
	}
	return b.String()
}
