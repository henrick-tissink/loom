package orchestrator

import (
	"errors"
	"os/exec"
	"strings"
)

// RepoState is what §4's `## Project` section says about one repo, and what
// §8's drift compares against. Deliberately three facts and a dirty count:
// §11's "not loaded, ever" table forbids file listings, language detection and
// `git log` beyond a single HEAD sha, because preloading a guess is how a 200 k
// window becomes 40 k of usable room.
type RepoState struct {
	Label   string
	Path    string
	Branch  string
	Head    string
	Dirty   int
	Missing bool
	// Err is the rendered reason a stat failed. It is never fatal and never
	// silent: the brief prints `(unavailable)` beside the repo rather than
	// omitting the line, so an orchestrator can tell "I could not read this"
	// from "this repo is clean".
	Err string
}

const unavailable = "(unavailable)"

// gitRunner is swapped in tests so assembly can be exercised without building a
// real repository for every case. Production always uses runGit.
type gitRunner func(dir string, args ...string) (string, error)

// runGit is the same bounded shell-out discipline as internal/gitdiff: stderr
// is folded into the error so a failure says why rather than "exit status 128",
// and no caller ever aborts on one.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return string(out), errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), err
	}
	return string(out), nil
}

// repoState reads one repo's branch/HEAD/dirty count. A repo slice 1's sweep
// already flagged `missing` is NOT stat'ed (§5.1): shelling out to git in a
// directory that does not exist buys three error strings and costs three
// process spawns per brief.
func repoState(g gitRunner, label, path string, missing bool) RepoState {
	rs := RepoState{Label: label, Path: path, Missing: missing}
	if missing {
		return rs
	}
	branch, err := g(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		rs.Err = err.Error()
		return rs
	}
	rs.Branch = strings.TrimSpace(branch)
	head, err := g(path, "rev-parse", "HEAD")
	if err != nil {
		rs.Err = err.Error()
		return rs
	}
	rs.Head = strings.TrimSpace(head)
	// --porcelain and count lines rather than `git status --short | wc -l`:
	// the pipe is a shell, and Loom shells out with exec.Command, never sh -c.
	st, err := g(path, "status", "--porcelain")
	if err != nil {
		rs.Err = err.Error()
		return rs
	}
	for _, line := range strings.Split(st, "\n") {
		if strings.TrimSpace(line) != "" {
			rs.Dirty++
		}
	}
	return rs
}

// commitsSince answers §8's `repos moved` line. It returns ok=false — never a
// fabricated number and never an error — when `old` is unknown to the repo: a
// rebase, a force-push, a shallow clone or a fresh clone all produce that, and
// all of them are ordinary events an orchestrator should be told about in
// words rather than have guessed at in digits.
func commitsSince(g gitRunner, dir, old string) (int, bool) {
	if old == "" {
		return 0, false
	}
	// `cat-file -e <sha>^{commit}` first: rev-list on an unknown sha also
	// fails, but its stderr is about a bad revision, and distinguishing
	// "unknown commit" from "git is broken" by parsing that string is exactly
	// the match-by-string this codebase refuses to do.
	if _, err := g(dir, "cat-file", "-e", old+"^{commit}"); err != nil {
		return 0, false
	}
	out, err := g(dir, "rev-list", "--count", old+"..HEAD")
	if err != nil {
		return 0, false
	}
	n := 0
	for _, r := range strings.TrimSpace(out) {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	if strings.TrimSpace(out) == "" {
		return 0, false
	}
	return n, true
}
