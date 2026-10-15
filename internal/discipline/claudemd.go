package discipline

import (
	"bytes"
	"regexp"
	"strings"
)

// CLAUDE.md carries the rules by one import line (decision D7 in
// docs/DISCIPLINE.md). A legacy install has the whole rules text inline;
// setup replaces exactly that span with the import line and leaves every
// other line (the owner's own rules, a team section) where it was.

const (
	ClaudeMDNone          = "none"           // already imports the rules
	ClaudeMDReplaceInline = "replace-inline" // inline rules text → import line
	ClaudeMDAppend        = "append"         // import line appended (or file created)
)

// PlanClaudeMD returns the new content and what was done. rules is the
// bundled rules text.
func PlanClaudeMD(current, rules []byte) (out []byte, action string) {
	if hasImportLine(current) {
		return current, ClaudeMDNone
	}
	r := bytes.TrimSpace(rules)
	if len(r) > 0 {
		if i := bytes.Index(current, r); i >= 0 {
			before := strings.TrimRight(string(current[:i]), "\n")
			after := strings.TrimLeft(string(current[i+len(r):]), "\n")
			var b strings.Builder
			if before != "" {
				b.WriteString(before + "\n\n")
			}
			b.WriteString(ImportLine + "\n")
			if after != "" {
				b.WriteString("\n" + after)
			}
			return []byte(ensureTrailingNewline(b.String())), ClaudeMDReplaceInline
		}
	}
	s := strings.TrimRight(string(current), "\n")
	if s == "" {
		return []byte(ImportLine + "\n"), ClaudeMDAppend
	}
	return []byte(s + "\n\n" + ImportLine + "\n"), ClaudeMDAppend
}

// UndoClaudeMD reverses PlanClaudeMD: the import line goes, and when the
// install had replaced inline text, that text comes back in its place so
// the person keeps their rules without loom.
func UndoClaudeMD(current, rules []byte, replacedInline bool) (out []byte, changed bool) {
	if !hasImportLine(current) {
		return current, false
	}
	repl := ""
	if replacedInline {
		repl = strings.TrimSpace(string(rules))
	}
	lines := strings.Split(string(current), "\n")
	var kept []string
	for _, ln := range lines {
		if strings.TrimSpace(ln) == ImportLine {
			if repl != "" {
				kept = append(kept, repl)
				repl = ""
			}
			continue
		}
		kept = append(kept, ln)
	}
	s := strings.Join(kept, "\n")
	s = collapseBlankLines(strings.TrimRight(s, "\n"))
	if s == "" {
		return []byte{}, true
	}
	return []byte(s + "\n"), true
}

var importRe = regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(ImportLine) + `[ \t]*$`)
var blankRunRe = regexp.MustCompile(`\n{3,}`)

func hasImportLine(b []byte) bool { return importRe.Match(b) }

func collapseBlankLines(s string) string { return blankRunRe.ReplaceAllString(s, "\n\n") }

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
