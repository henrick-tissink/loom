package discipline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/jsonedit"
	"github.com/henricktissink/loom/internal/rules"
)

// Adopted rules (internal/rules) live in <ccd>/discipline/rules.md, beside
// the revisions but never inside one: revisions are immutable and come from
// the bundle, rules come from the person. CLAUDE.md imports the file by one
// line, added with the first rule. A change is planned (the diffs the
// person reviews) and then applied only if nothing moved since the plan:
// the files are backed up, written atomically and the ledger records it.
// Rules change what the agent is told, not the hooks, so a rule is not a
// change mark for the hook rates.

const RulesImportLine = "@discipline/rules.md"

func (l Layout) RulesPath() string { return filepath.Join(l.DisciplineDir(), "rules.md") }

// RuleChange is what the person asked for.
type RuleChange struct {
	Op     string `json:"op"` // adopt | drop | renew
	Key    string `json:"key"`
	Text   string `json:"text,omitempty"`   // adopt
	Why    string `json:"why,omitempty"`    // adopt
	Lapses string `json:"lapses,omitempty"` // adopt, renew: YYYY-MM-DD
	// Since is the adoption time PlanRule stamps on an adopt; the apply
	// writes that one, so the bytes written are the bytes reviewed.
	Since string `json:"since,omitempty"`
}

type FileChange struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
}

// RulePlan is a change ready for review. Token fingerprints the files as
// they were read; ApplyRule refuses when they no longer match.
type RulePlan struct {
	Change RuleChange   `json:"change"`
	Files  []FileChange `json:"files"`
	Token  string       `json:"token"`
	after  map[string][]byte
}

var rulesImportRe = regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(RulesImportLine) + `[ \t]*$`)

// MaxLapse is the furthest a rule may lapse: a year.
const MaxLapse = 366 * 24 * time.Hour

// PlanRule computes the writes for a change, without making them.
func PlanRule(l Layout, c RuleChange, now time.Time) (*RulePlan, error) {
	rulesPath, claudePath := l.RulesPath(), l.ClaudeMD()
	before, err := readIfExists(rulesPath)
	if err != nil {
		return nil, err
	}
	claude, err := readIfExists(claudePath)
	if err != nil {
		return nil, err
	}
	today := now.Format(rules.DateLayout)
	if c.Op == "adopt" || c.Op == "renew" {
		lapses, err := time.ParseInLocation(rules.DateLayout, c.Lapses, now.Location())
		switch {
		case err != nil:
			return nil, fmt.Errorf("bad lapse date %q", c.Lapses)
		case c.Lapses <= today:
			return nil, errors.New("the lapse date must be after today")
		case lapses.Sub(now) > MaxLapse:
			return nil, errors.New("a rule lapses within a year")
		}
	}
	var after []byte
	switch c.Op {
	case "adopt":
		if c.Since == "" {
			c.Since = now.Format(rules.SinceLayout)
		}
		after, err = rules.Add(before, rules.Rule{Text: c.Text, Why: c.Why, Key: c.Key, Since: c.Since, Lapses: c.Lapses})
	case "drop":
		after, err = rules.Remove(before, c.Key)
	case "renew":
		after, err = rules.Renew(before, c.Key, c.Lapses)
	default:
		err = fmt.Errorf("unknown rule change %q", c.Op)
	}
	if err != nil {
		return nil, err
	}
	p := &RulePlan{Change: c, Files: []FileChange{}, after: map[string][]byte{}}
	p.add(rulesPath, before, after)
	claudeAfter := claude
	if c.Op == "adopt" && !rulesImportRe.Match(claude) {
		s := strings.TrimRight(string(claude), "\n")
		if s != "" {
			s += "\n\n"
		}
		claudeAfter = []byte(s + RulesImportLine + "\n")
		p.add(claudePath, claude, claudeAfter)
	}
	p.Token = ruleToken(before, claude, after, claudeAfter)
	return p, nil
}

func (p *RulePlan) add(path string, before, after []byte) {
	p.after[path] = after
	p.Files = append(p.Files, FileChange{Path: path, Diff: UnifiedDiff(filepath.Base(path), string(before), string(after))})
}

// ruleToken fingerprints both files before and after, so an apply that
// would write anything other than the reviewed diff is refused.
func ruleToken(parts ...[]byte) string {
	h := sha256.New()
	for _, b := range parts {
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ErrStale means a file changed between the review and the apply.
var ErrStale = errors.New("rules.md or CLAUDE.md changed since this change was reviewed; review it again")

// ApplyRule makes a reviewed change. token is the plan's; the change is
// planned again under the lock and refused if the files moved. It returns
// the backup directory ("" when there was nothing to back up).
func ApplyRule(l Layout, c RuleChange, token string, now time.Time) (string, error) {
	unlock, err := lock(l)
	if err != nil {
		return "", err
	}
	defer unlock()
	p, err := PlanRule(l, c, now)
	if err != nil {
		return "", err
	}
	if p.Token != token {
		return "", ErrStale
	}
	paths := make([]string, 0, len(p.Files))
	for _, f := range p.Files {
		paths = append(paths, f.Path)
	}
	dir, err := Backup(l, "rule", now, paths...)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(l.DisciplineDir(), 0o755); err != nil {
		return dir, err
	}
	sums := map[string]string{}
	// rules.md first: the import line never points at a file that is not
	// there yet.
	for _, f := range p.Files {
		if err := jsonedit.WriteBytes(f.Path, p.after[f.Path]); err != nil {
			return dir, err
		}
		s := sha256.Sum256(p.after[f.Path])
		sums[f.Path] = hex.EncodeToString(s[:])
	}
	note := c.Op + " " + c.Key
	if c.Op != "drop" {
		note += " until " + c.Lapses
	}
	return dir, AppendLedger(l, LedgerEntry{TS: now, Op: "rule", Backup: dir, Files: sums, Note: note})
}

func readIfExists(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}
