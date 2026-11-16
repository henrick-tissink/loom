// Package mustread names the files in a change that a person must read
// rather than take on the evidence of passing tests: auth, payments,
// permissions, migrations, CI. A repository lists its own in
// .loom/must-read; one without the file gets Defaults.
//
// The file is one rule per line: a path pattern, then optionally the reason
// after whitespace. Blank lines and lines starting with # are skipped.
//
//	auth/                 auth
//	*payment*             payments
//	db/migrate/**         migrations
//	.github/workflows/    CI
//
// Patterns follow .gitignore's shape, simplified and case-insensitive: a
// pattern with no slash but a trailing one matches a path component at any
// depth (a trailing slash means a directory, and everything under it); a
// pattern with a slash elsewhere is anchored at the repository root, where
// ** crosses directories; * and ? never cross a slash.
package mustread

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// File is where a repository lists its rules, relative to its root.
const File = ".loom/must-read"

// Rule is one pattern and the reason it names.
type Rule struct {
	Pattern string
	Why     string
	re      *regexp.Regexp
}

// Rules is a repository's rules, and where they came from.
type Rules struct {
	Rules  []Rule
	Source string // File, or "defaults"
}

// defaults is what a repository without the file gets.
var defaults = [][2]string{
	{"auth/", "auth"}, {"*auth.*", "auth"}, {"*oauth*", "auth"}, {"*authn*", "auth"}, {"*authz*", "auth"},
	{"*login*", "auth"}, {"*password*", "auth"}, {"*session_token*", "auth"},
	{"*payment*", "payments"}, {"*billing*", "payments"}, {"*stripe*", "payments"}, {"*invoice*", "payments"},
	{"*permission*", "permissions"}, {"*rbac*", "permissions"}, {"acl/", "permissions"},
	{"migrations/", "migrations"}, {"migrate/", "migrations"}, {"*.sql", "migrations"},
	{".github/workflows/", "CI"}, {".gitlab-ci.yml", "CI"}, {".circleci/", "CI"}, {"Jenkinsfile", "CI"}, {".buildkite/", "CI"},
}

// Defaults is the built-in rule set.
func Defaults() Rules {
	r := Rules{Source: "defaults"}
	for _, d := range defaults {
		r.Rules = append(r.Rules, compile(d[0], d[1]))
	}
	return r
}

// Load reads root's .loom/must-read, or returns Defaults when it has none.
// A file that exists but cannot be read is an error, with Defaults.
func Load(root string) (Rules, error) {
	b, err := os.ReadFile(filepath.Join(root, File))
	if errors.Is(err, fs.ErrNotExist) {
		return Defaults(), nil
	}
	if err != nil {
		return Defaults(), err
	}
	return Parse(b), nil
}

// Parse reads the file's text.
func Parse(b []byte) Rules {
	r := Rules{Source: File, Rules: []Rule{}}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		f := strings.Fields(ln)
		pat, why := f[0], strings.Join(f[1:], " ")
		if why == "" {
			why = "must read"
		}
		r.Rules = append(r.Rules, compile(pat, why))
	}
	return r
}

// Match is the first rule a repository-relative path matches.
func (r Rules) Match(path string) (Rule, bool) {
	p := strings.ToLower(filepath.ToSlash(strings.TrimPrefix(path, "./")))
	for _, x := range r.Rules {
		if x.re.MatchString(p) {
			return x, true
		}
	}
	return Rule{}, false
}

func compile(pat, why string) Rule {
	p := strings.ToLower(strings.TrimPrefix(filepath.ToSlash(pat), "./"))
	dir := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	anchored := strings.Contains(p, "/")
	p = strings.TrimPrefix(p, "/")
	var b strings.Builder
	b.WriteString("^")
	if !anchored {
		b.WriteString("(?:.*/)?") // any depth
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '*' && strings.HasPrefix(p[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case c == '*' && strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if dir {
		b.WriteString("/.*$") // a directory: what is under it
	} else {
		b.WriteString("(?:/.*)?$") // a file, or a directory named so
	}
	return Rule{Pattern: pat, Why: why, re: regexp.MustCompile(b.String())}
}
