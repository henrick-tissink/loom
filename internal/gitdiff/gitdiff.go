// Package gitdiff captures a git working tree's changes as data.
//
// It lives here rather than in cmd/loom-gui because ARCHITECTURE §3 fixes the
// dependency direction downward: nothing in internal/ may import a frontend.
// The GUI's diff.go is a DTO shim over this package.
package gitdiff

import (
	"errors"
	"os/exec"
	"strings"
)

// RepoDiff is one work tree's changes. Error is non-empty (and the rest empty)
// when the directory isn't a git work tree.
//
// The JSON is read by the diff panel's frontend, so the keys are load-bearing
// and must not be renamed.
type RepoDiff struct {
	Path      string   `json:"path"`
	Label     string   `json:"label"`
	Stat      string   `json:"stat"`      // `git diff … --stat` summary
	Patch     string   `json:"patch"`     // the full patch
	Files     []string `json:"files"`     // repo-relative paths the patch touches
	Untracked []string `json:"untracked"` // new files not yet tracked
	Dirty     bool     `json:"dirty"`     // any tracked change or untracked file
	Error     string   `json:"error"`     // set when the path isn't a git work tree
}

// WorkingTree captures uncommitted changes vs HEAD — what the diff panel
// shows. A non-repo or a git failure degrades to a RepoDiff with Error set;
// nothing here panics and nothing here returns a Go error, because the
// caller's job is to render the reason rather than to abort.
func WorkingTree(dir string) RepoDiff {
	if strings.TrimSpace(dir) == "" {
		return RepoDiff{Error: "no working directory"}
	}
	d := RepoDiff{Path: dir}
	out, err := git(dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(out) != "true" {
		d.Error = "not a git repository"
		return d
	}

	// Errors from `git diff HEAD` are swallowed on purpose: it also fails on a
	// freshly `git init`ed repo with no commit yet, and that session must
	// still get its untracked-file list rather than an error banner.
	stat, _ := git(dir, "diff", "HEAD", "--stat")
	patch, _ := git(dir, "diff", "HEAD")
	names, _ := git(dir, "diff", "HEAD", "--name-only", "-z")
	untr, _ := git(dir, "ls-files", "--others", "--exclude-standard", "-z")

	d.Stat = strings.TrimRight(stat, "\n")
	d.Patch = patch
	d.Files = splitNUL(names)
	d.Untracked = splitNUL(untr)
	d.Dirty = strings.TrimSpace(d.Patch) != "" || len(d.Untracked) > 0
	return d
}

// git runs one git command and returns stdout, folding stderr into the error so
// a failure says why rather than just "exit status 128".
func git(dir string, args ...string) (string, error) {
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

// splitNUL parses git's -z output. -z rather than newlines because git quotes
// any path with a non-ASCII or special character by default (core.quotePath),
// and a quoted path matches no declared glob — it would show up as a phantom
// divergence for every author whose filenames aren't plain ASCII.
func splitNUL(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
