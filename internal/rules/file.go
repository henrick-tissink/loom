// Package rules turns failures that keep repeating (hook holds in the
// Agent Discipline logs, tool errors in Claude's transcripts) into
// proposed rules, and reads and edits the file adopted rules live in:
// <ccd>/discipline/rules.md, imported from CLAUDE.md.
//
// Nothing here writes to disk; internal/discipline applies a change the
// person reviewed. Every rule says why it exists and when it lapses, and a
// lapsed rule is only ever offered for removal or renewal.
package rules

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Rule is one adopted rule, in local time.
type Rule struct {
	Text   string `json:"text"`
	Why    string `json:"why"`
	Key    string `json:"key"`    // the failure it answers, as Event.Key
	Since  string `json:"since"`  // when it was adopted, SinceLayout
	Lapses string `json:"lapses"` // the last day it holds, DateLayout
}

const (
	DateLayout  = "2006-01-02"
	SinceLayout = "2006-01-02 15:04"
)

// Lapsed reports whether the rule's lapse day is before now's day: a rule
// holds through the day it lapses.
func (r Rule) Lapsed(now time.Time) bool {
	return r.Lapses < now.Format(DateLayout)
}

// Header opens a new rules file. It is written for the agent, which reads
// the file as part of CLAUDE.md, and says enough for a person who opens it.
const Header = `# Rules from repeated failures

Each rule below answers a failure that kept repeating in past sessions (a hook
holding the work, or a tool call failing the same way). Follow them like the rest
of CLAUDE.md. Loom proposed them and the user adopted each one; the Why line says
what it answers and when it lapses.
`

var (
	itemRe = regexp.MustCompile(`^- (.+)$`)
	whyRe  = regexp.MustCompile(`^\s+Why: (.*) · since (\d{4}-\d\d-\d\d \d\d:\d\d) · lapses (\d{4}-\d\d-\d\d) · key (\S+)\s*$`)
	keyRe  = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,200}$`)
	// Claude reads "@path" in a memory file as an import.
	importRe = regexp.MustCompile(`(^|\s)@\S`)
)

// whyLine renders a rule's second line.
func whyLine(r Rule) string {
	return fmt.Sprintf("  Why: %s · since %s · lapses %s · key %s", r.Why, r.Since, r.Lapses, r.Key)
}

// item is a parsed rule and the line it starts on.
type item struct {
	Rule
	line int
}

func parse(content string) []item {
	lines := strings.Split(content, "\n")
	var out []item
	for i := 0; i+1 < len(lines); i++ {
		m := itemRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		w := whyRe.FindStringSubmatch(lines[i+1])
		if w == nil {
			continue
		}
		out = append(out, item{Rule: Rule{Text: strings.TrimSpace(m[1]), Why: w[1], Since: w[2], Lapses: w[3], Key: w[4]}, line: i})
		i++
	}
	return out
}

// Parse reads the adopted rules. A list item without loom's Why line is
// the person's own text: it is left alone and not returned.
func Parse(content []byte) []Rule {
	items := parse(string(content))
	out := make([]Rule, 0, len(items))
	for _, it := range items {
		out = append(out, it.Rule)
	}
	return out
}

// Find returns the rule with the key.
func Find(content []byte, key string) (Rule, bool) {
	for _, r := range Parse(content) {
		if r.Key == key {
			return r, true
		}
	}
	return Rule{}, false
}

// Validate checks a rule before it is written.
func Validate(r Rule) error {
	t := strings.TrimSpace(r.Text)
	switch {
	case t == "":
		return errors.New("the rule is empty")
	case strings.ContainsAny(r.Text, "\r\n"):
		return errors.New("a rule is one line")
	case utf8.RuneCountInString(t) > 300:
		return errors.New("a rule is at most 300 characters")
	case strings.HasPrefix(t, "#"):
		return errors.New("a rule cannot start with #")
	case importRe.MatchString(t):
		return errors.New("a rule cannot contain @path: Claude reads it as an import")
	case !keyRe.MatchString(r.Key):
		return fmt.Errorf("bad key %q", r.Key)
	}
	if _, err := time.Parse(SinceLayout, r.Since); err != nil {
		return fmt.Errorf("bad date %q", r.Since)
	}
	if _, err := time.Parse(DateLayout, r.Lapses); err != nil {
		return fmt.Errorf("bad date %q", r.Lapses)
	}
	if r.Lapses < r.Since[:len(DateLayout)] {
		return errors.New("a rule cannot lapse before it starts")
	}
	return nil
}

// CleanWhy makes generated reason text safe for the Why line: one line,
// no import-looking @, no separator, at most 240 characters.
func CleanWhy(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "@", "at ")
	s = strings.ReplaceAll(s, " · ", " - ")
	if utf8.RuneCountInString(s) > 240 {
		s = string([]rune(s)[:239]) + "…"
	}
	return s
}

// Add appends a rule, starting the file with the header when it is empty.
// A rule whose key is already in the file is refused.
func Add(content []byte, r Rule) ([]byte, error) {
	r.Text = strings.TrimSpace(r.Text)
	r.Why = CleanWhy(r.Why)
	if err := Validate(r); err != nil {
		return nil, err
	}
	if _, ok := Find(content, r.Key); ok {
		return nil, fmt.Errorf("a rule for %s is already adopted", r.Key)
	}
	s := strings.TrimRight(string(content), "\n")
	if strings.TrimSpace(s) == "" {
		s = strings.TrimRight(Header, "\n")
	}
	sep := "\n\n"
	if items := parse(s); len(items) > 0 && items[len(items)-1].line+2 == len(strings.Split(s, "\n")) {
		sep = "\n" // the file ends on a rule: keep the list together
	}
	return []byte(s + sep + "- " + r.Text + "\n" + whyLine(r) + "\n"), nil
}

// Remove drops the rule with the key: its two lines and nothing else.
func Remove(content []byte, key string) ([]byte, error) {
	lines := strings.Split(string(content), "\n")
	for _, it := range parse(string(content)) {
		if it.Key != key {
			continue
		}
		rest := lines[it.line+2:]
		// Tidy only the splice: a blank line on both sides becomes one.
		if it.line > 0 && strings.TrimSpace(lines[it.line-1]) == "" && len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
			rest = rest[1:]
		}
		kept := append(append([]string{}, lines[:it.line]...), rest...)
		return []byte(strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n"), nil
	}
	return nil, fmt.Errorf("no adopted rule for %s", key)
}

// Renew moves the rule's lapse day.
func Renew(content []byte, key, lapses string) ([]byte, error) {
	lines := strings.Split(string(content), "\n")
	for _, it := range parse(string(content)) {
		if it.Key != key {
			continue
		}
		r := it.Rule
		r.Lapses = lapses
		if err := Validate(r); err != nil {
			return nil, err
		}
		lines[it.line+1] = whyLine(r)
		return []byte(strings.Join(lines, "\n")), nil
	}
	return nil, fmt.Errorf("no adopted rule for %s", key)
}
