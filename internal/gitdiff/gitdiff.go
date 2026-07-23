// Package gitdiff captures a git working tree's changes as data.
//
// It lives here rather than in cmd/loom-gui because ARCHITECTURE §3 fixes the
// dependency direction downward: nothing in internal/ may import a frontend,
// and internal/delegate needs the same capture the GUI's diff panel uses — the
// divergence it derives is persisted, gates the merge approval, and has to mean
// the same thing under the TUI, which has no diff surface at all. The GUI's
// diff.go is now a DTO shim over this package; its two tests are unchanged and
// are the migration's regression test.
package gitdiff

import (
	"errors"
	"os/exec"
	"strings"
)

// RepoDiff is one work tree's changes. Error is non-empty (and the rest empty)
// when the directory isn't a git work tree.
//
// Field-for-field the shape cmd/loom-gui shipped, plus Files: the JSON is read
// by the diff panel's frontend, so the existing keys are load-bearing and must
// not be renamed. Files is additive — an unknown key is ignored by the frontend
// and is what the divergence report is computed from.
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

// Options selects what a capture compares.
type Options struct {
	// Dir is the work tree to inspect. Required.
	Dir string
	// Base, when set, switches to base-relative mode: the capture is
	// `git diff <Base>...<Ref>`, i.e. what Ref has done since it forked from
	// Base, ignoring anything Base's own line has done since. Empty means
	// working-tree mode — uncommitted changes vs HEAD, what the GUI shows.
	Base string
	// Ref is the far side in base-relative mode; empty means HEAD. Ignored
	// when Base is empty.
	Ref string
}

// WorkingTree captures uncommitted changes vs HEAD — the GUI's mode.
func WorkingTree(dir string) RepoDiff { return Capture(Options{Dir: dir}) }

// SinceBase captures what ref has committed since it forked from base — the
// merge-gate mode. ref may be empty for HEAD.
func SinceBase(dir, base, ref string) RepoDiff {
	return Capture(Options{Dir: dir, Base: base, Ref: ref})
}

// Capture shells out to git. A non-repo or a git failure degrades to a RepoDiff
// with Error set; nothing here panics and nothing here returns a Go error,
// because every caller's job is to render the reason rather than to abort.
func Capture(opts Options) RepoDiff {
	if strings.TrimSpace(opts.Dir) == "" {
		return RepoDiff{Error: "no working directory"}
	}
	d := RepoDiff{Path: opts.Dir}
	out, err := git(opts.Dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(out) != "true" {
		d.Error = "not a git repository"
		return d
	}

	// `A...B` (three dots) and not `A..B`: the question at the merge gate is
	// "what did this task do", not "how do these two trees differ". If the
	// user's branch moved on after the base was pinned, a two-dot diff would
	// attribute that movement, inverted, to the child.
	spec := "HEAD"
	if opts.Base != "" {
		ref := opts.Ref
		if ref == "" {
			ref = "HEAD"
		}
		spec = opts.Base + "..." + ref
	}

	stat, statErr := git(opts.Dir, "diff", spec, "--stat")
	patch, patchErr := git(opts.Dir, "diff", spec)
	names, _ := git(opts.Dir, "diff", spec, "--name-only", "-z")

	// Asymmetric on purpose. In base-relative mode a bad base (a sha that was
	// gc'd, a branch that never got created) makes git exit non-zero and print
	// nothing, and rendering that as "no changes" at a merge gate is the
	// invisible failure the house rules forbid — so it is surfaced. In
	// working-tree mode it is swallowed, because `git diff HEAD` also fails on
	// a freshly `git init`ed repo with no commit yet, and that session must
	// still get its untracked-file list rather than an error banner.
	if opts.Base != "" {
		if e := firstErr(statErr, patchErr); e != nil {
			d.Error = "git diff " + spec + ": " + e.Error()
			return d
		}
	}

	untr, _ := git(opts.Dir, "ls-files", "--others", "--exclude-standard", "-z")

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

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
