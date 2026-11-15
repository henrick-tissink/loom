// Package diffaudit reads a unified diff for the shortcuts an agent takes to
// make work look done, and for edits a reviewer should not miss: tests
// deleted, assertions removed, tests skipped or focused, checks silenced,
// new tests that assert nothing, test or CI configuration changed,
// dependencies changed, secrets added, and the agent's own instructions or
// hooks edited.
//
// It is pattern matching on the patch text, so every flag is advisory: a
// refactor can remove assertions and an upgrade changes go.mod. The point is
// that the reviewer sees them without reading every hunk.
package diffaudit

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Flag is one finding. Level is bad (a possible secret) or warn.
type Flag struct {
	Kind  string   `json:"kind"`
	Level string   `json:"level"`
	Text  string   `json:"text"`
	Files []string `json:"files"`
}

// file is one file's part of a patch.
type file struct {
	path             string
	isNew, isDeleted bool
	added, removed   []string // line contents, without the +/- marker
}

// parse splits a unified diff (git diff, including --no-index new files)
// into files. Header lines are read only before a file's first hunk, so
// content that starts with "+++" or "---" is not mistaken for one.
func parse(patch string) []*file {
	var out []*file
	var f *file
	inHunk := false
	for _, ln := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			f = &file{path: gitPath(ln)}
			out = append(out, f)
			inHunk = false
		case f == nil:
		case strings.HasPrefix(ln, "@@"):
			inHunk = true
		case !inHunk:
			switch {
			case strings.HasPrefix(ln, "new file mode"):
				f.isNew = true
			case strings.HasPrefix(ln, "deleted file mode"):
				f.isDeleted = true
			case strings.HasPrefix(ln, "rename to "):
				f.path = unquote(strings.TrimPrefix(ln, "rename to "))
			case strings.HasPrefix(ln, "+++ "):
				if p := strings.TrimPrefix(ln, "+++ "); p != "/dev/null" {
					f.path = strip(unquote(p), "b/")
				}
			case strings.HasPrefix(ln, "--- "):
				if p := strings.TrimPrefix(ln, "--- "); p != "/dev/null" && f.path == "" {
					f.path = strip(unquote(p), "a/")
				}
			}
		case strings.HasPrefix(ln, "+"):
			f.added = append(f.added, ln[1:])
		case strings.HasPrefix(ln, "-"):
			f.removed = append(f.removed, ln[1:])
		}
	}
	return out
}

// gitPath reads the b/ side of a `diff --git a/x b/x` line, the fallback
// when a file has no +++/--- lines (a binary file, a mode change).
func gitPath(ln string) string {
	rest := strings.TrimPrefix(ln, "diff --git ")
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return unquote(rest[i+3:])
	}
	return ""
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return s
}

func strip(s, prefix string) string { return strings.TrimPrefix(s, prefix) }

var (
	// codeFile keeps a test directory's docs and fixtures (specs/*.md) from
	// counting as tests.
	codeFile = regexp.MustCompile(`\.(?:go|py|[cm]?[jt]sx?|rb|java|kt|cs|swift|rs|php|exs?)$`)
	testPath = regexp.MustCompile(`(^|/)(tests?|__tests__|spec|specs)/|_test\.go$|(^|/)test_[^/]*\.py$|_test\.py$|\.(test|spec)\.[cm]?[jt]sx?$|_spec\.rb$|(Test|Tests)\.(java|kt|cs|swift)$`)
	// assertion is a test's check: go's t.Error/t.Fatal, the assert/
	// expect/require/should families (FluentAssertions' .Should()), mock
	// verifications (Moq Verify, NSubstitute Received), XCTest, Rust's
	// assert macros.
	assertion = regexp.MustCompile(`\b(?:[Aa]ssert\w*!?|expect\w*|t\.(?:Error|Errorf|Fatal|Fatalf|Fail|FailNow)|require\.\w+|XCTAssert\w*|[Ss]hould\w*|[Vv]erify\w*|Received\w*)\b`)
	// testDef is a test being defined, so a new helper file is not a test
	// without assertions.
	testDef = regexp.MustCompile(`^\s*func Test\w*\(|^\s*(?:async\s+)?def test_|(?:^|[\s;(])(?:it|test)\(\s*['"\x60]|@Test\b|\[(?:Test|Fact|Theory)\b|#\[test\]`)
	// skip turns a test off or focuses one (which turns the others off).
	skip = regexp.MustCompile(`\bt\.Skip(?:Now|f)?\(|@pytest\.mark\.(?:skip|skipif|xfail)\b|\bpytest\.(?:skip|xfail)\(|@unittest\.skip|\b(?:it|test|describe|context)\.(?:skip|only|todo)\(|(?:^|[\s;(])(?:xit|xdescribe|xtest|fit|fdescribe)\(|@(?:Disabled|Ignore)\b|\[Ignore\b|#\[ignore\]`)
	// silence turns a type checker or linter off for a line or a file. Style
	// suppressions (# noqa, pylint) and coverage pragmas are left out: on
	// real history they are routine and drowned the rest.
	silence = regexp.MustCompile(`eslint-disable|@ts-ignore|@ts-nocheck|@ts-expect-error|#\s*type:\s*ignore|//\s*nolint\b|#!?\[allow\(|@SuppressWarnings\b`)
)

// marker names what matched, shortened to its first word-ish run.
func marker(re *regexp.Regexp, s string) string {
	m := strings.TrimSpace(re.FindString(s))
	m = strings.TrimLeft(m, "(;")
	return strings.TrimSpace(strings.TrimSuffix(m, "("))
}

var secrets = []struct {
	name string
	re   *regexp.Regexp
}{
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"an AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"a GitHub token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b|\bgithub_pat_[A-Za-z0-9_]{22,}`)},
	{"an API key (sk-)", regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
}

// credential is a hard-coded value assigned to a secret-sounding name. The
// value must mix letters and digits, so words ('research_hut') and all-caps
// placeholders (CHANGE_ME_IN_PROD) don't count; test files are skipped.
var (
	credential  = regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|passw(?:or)?d)\b["']?\s*(?::=|=|:)\s*["']([^"'\s]{12,})["']`)
	mixed       = regexp.MustCompile(`[0-9]`)
	letter      = regexp.MustCompile(`[A-Za-z]`)
	placeholder = regexp.MustCompile(`(?i)example|placeholder|changeme|dummy|fake|xxxx|redacted|your[_-]|<|\$\{|\{\{`)
)

var depFiles = map[string]bool{
	"go.mod": true, "go.sum": true, "package.json": true, "package-lock.json": true, "pnpm-lock.yaml": true,
	"yarn.lock": true, "bun.lock": true, "bun.lockb": true, "Pipfile": true, "Pipfile.lock": true, "poetry.lock": true,
	"uv.lock": true, "pyproject.toml": true, "Cargo.toml": true, "Cargo.lock": true, "Gemfile": true, "Gemfile.lock": true,
	"composer.json": true, "composer.lock": true, "pom.xml": true, "build.gradle": true, "build.gradle.kts": true,
	"Directory.Packages.props": true, "packages.lock.json": true, "Package.swift": true, "Package.resolved": true,
}

// lockFile is generated and can be huge; its lines are not scanned.
var lockFile = regexp.MustCompile(`(?:^|[.-])lock(?:\.json|\.ya?ml|b)?$|\.lock$|^go\.sum$|^Package\.resolved$`)

var requirements = regexp.MustCompile(`^requirements[\w.-]*\.txt$|\.csproj$`)

var configFile = regexp.MustCompile(`^(?:pytest\.ini|tox\.ini|conftest\.py|\.coveragerc|codecov\.ya?ml|\.nycrc.*|mypy\.ini|\.flake8|\.?ruff\.toml|\.golangci\.ya?ml|\.eslintrc.*|eslint\.config\..*|\.pre-commit-config\.yaml|\.mocharc.*|phpunit\.xml.*|\.gitlab-ci\.yml|azure-pipelines\.yml|Jenkinsfile|(?:jest|vitest|vitest\.workspace|karma|playwright|cypress)\.conf(?:ig)?\..*)$`)
var configDir = regexp.MustCompile(`(^|/)(?:\.github/workflows|\.circleci|\.buildkite)/`)

var agentFile = regexp.MustCompile(`(^|/)(?:CLAUDE(?:\.local)?\.md|AGENTS\.md|GEMINI\.md|\.cursorrules|\.github/copilot-instructions\.md)$|(^|/)(?:\.claude|\.cursor/rules)/`)

var envFile = regexp.MustCompile(`(^|/)\.env(?:\.[\w-]+)?$`)
var envExample = regexp.MustCompile(`\.(?:example|sample|template|dist)$`)

// Audit reads a patch and returns its flags, the possible secrets first.
func Audit(patch string) []Flag {
	files := parse(patch)
	var (
		secretHits                            []string
		deleted, newWeak, config, deps, agent []string
		skipHits, silenceHits                 []string
		assertLoss                            []string
	)
	// moved holds the assertion lines added to test files: one removed from
	// a test file and added again elsewhere was moved, not lost.
	moved := map[string]int{}
	for _, f := range files {
		if !f.isDeleted && testPath.MatchString(f.path) && codeFile.MatchString(path.Base(f.path)) {
			for _, l := range f.added {
				if assertion.MatchString(l) {
					moved[norm(l)]++
				}
			}
		}
	}
	for _, f := range files {
		p := f.path
		if p == "" {
			continue
		}
		base := path.Base(p)
		isTest := testPath.MatchString(p) && codeFile.MatchString(base)

		if f.isDeleted {
			if isTest {
				deleted = append(deleted, p)
			}
			continue
		}
		if isTest {
			// A removed assertion added again in this file (reordered) or in
			// another (moved) is not a loss.
			own := map[string]int{}
			add := 0
			for _, l := range f.added {
				if assertion.MatchString(l) {
					own[norm(l)]++
					add++
				}
			}
			rem := 0
			for _, l := range f.removed {
				if !assertion.MatchString(l) {
					continue
				}
				switch k := norm(l); {
				case own[k] > 0:
					own[k]--
					moved[k]--
					add--
				case moved[k] > own[k]:
					moved[k]--
				default:
					rem++
				}
			}
			if net := add - rem; !f.isNew && rem > 0 && net < 0 {
				assertLoss = append(assertLoss, fmt.Sprintf("%s (%d)", p, net))
			}
			if f.isNew && add == 0 && count(testDef, f.added) > 0 {
				newWeak = append(newWeak, p)
			}
			if m := firstMatch(skip, f.added); m != "" {
				skipHits = append(skipHits, fmt.Sprintf("%s (%s)", p, m))
			}
		}
		if lockFile.MatchString(base) {
			deps = append(deps, p) // generated: listed, not read line by line
			continue
		}
		if m := firstMatch(silence, f.added); m != "" {
			silenceHits = append(silenceHits, fmt.Sprintf("%s (%s)", p, m))
		}
		if configFile.MatchString(base) || configDir.MatchString(p) {
			config = append(config, p)
		}
		if depFiles[base] || requirements.MatchString(base) {
			deps = append(deps, p)
		}
		if agentFile.MatchString(p) {
			agent = append(agent, p)
		}
		if kind := secretIn(f, isTest); kind != "" {
			secretHits = append(secretHits, fmt.Sprintf("%s (%s)", p, kind))
		}
	}

	var out []Flag
	add := func(kind, level string, files []string, text string) {
		if len(files) > 0 {
			sort.Strings(files)
			out = append(out, Flag{Kind: kind, Level: level, Files: files, Text: text + " " + List(files) + "."})
		}
	}
	add("secret", "bad", secretHits, "Possible secret added; check before it is committed or pushed:")
	add("tests_deleted", "warn", deleted, "Test files deleted:")
	add("assertions_removed", "warn", assertLoss, "Tests lost assertions (net, per file):")
	add("tests_skipped", "warn", skipHits, "Tests skipped or focused in the new code:")
	add("weak_tests", "warn", newWeak, "New test files that define tests but assert nothing:")
	add("checks_silenced", "warn", silenceHits, "Linter or type checks silenced in the new code:")
	add("test_config", "warn", config, "Test, lint or CI configuration changed:")
	add("dependencies", "warn", deps, "Dependency files changed:")
	add("agent_files", "warn", agent, "It edited agent instructions, settings or hooks:")
	return out
}

func norm(s string) string { return strings.Join(strings.Fields(s), " ") }

func count(re *regexp.Regexp, lines []string) int {
	n := 0
	for _, l := range lines {
		if re.MatchString(l) {
			n++
		}
	}
	return n
}

func firstMatch(re *regexp.Regexp, lines []string) string {
	for _, l := range lines {
		if re.MatchString(l) {
			return marker(re, l)
		}
	}
	return ""
}

// secretIn names the first kind of secret a file's added lines carry, or
// "". A new .env file counts; its example/template variants don't.
func secretIn(f *file, isTest bool) string {
	if f.isNew && envFile.MatchString(f.path) && !envExample.MatchString(f.path) {
		return "an .env file"
	}
	for _, l := range f.added {
		for _, s := range secrets {
			if m := s.re.FindString(l); m != "" && !placeholder.MatchString(m) {
				return s.name
			}
		}
		if !isTest {
			if m := credential.FindStringSubmatch(l); m != nil && !placeholder.MatchString(m[1]) && mixed.MatchString(m[1]) && letter.MatchString(m[1]) {
				return "a hard-coded credential"
			}
		}
	}
	return ""
}

// List names up to five things.
func List(xs []string) string {
	if len(xs) > 5 {
		return strings.Join(xs[:5], ", ") + fmt.Sprintf(" and %d more", len(xs)-5)
	}
	return strings.Join(xs, ", ")
}
