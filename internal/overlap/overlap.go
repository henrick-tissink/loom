// Package overlap finds live sessions whose pending changes touch the same
// files of one repository. Two sessions editing one checkout overwrite each
// other's work; two branches changing one file conflict when they merge.
// Either way it is cheaper to know while both are still running.
//
// What a session's pending changes are depends on where it runs (the GUI
// decides; see cmd/loom-gui/overlap.go): in a linked worktree that is its
// own, everything the worktree has changed and its branch's commits found
// nowhere else; in a checkout it shares (or the main checkout), only the
// uncommitted files it edited itself through its file tools.
//
// Nothing here writes: Locate reads .git entries, Pending runs `git
// status` and `git log`.
package overlap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Checkout is the git working tree a directory is in.
type Checkout struct {
	Root string // the working tree's top directory
	// Repo is the repository's common git directory: the same for a main
	// checkout and every linked worktree of it, so files in different
	// checkouts of one repository compare equal.
	Repo   string
	Linked bool // a linked worktree (`git worktree add`, `claude --worktree`)
}

// Locator finds the checkout of a directory by looking for .git upward. It
// remembers every directory it has walked through: a directory does not
// move between repositories while loom runs.
type Locator struct {
	mu   sync.Mutex
	dirs map[string]*Checkout // nil: in no checkout
}

func NewLocator() *Locator { return &Locator{dirs: map[string]*Checkout{}} }

// Find returns the checkout dir is in, or false when it is in none.
func (l *Locator) Find(dir string) (Checkout, bool) {
	if dir == "" || !filepath.IsAbs(dir) {
		return Checkout{}, false
	}
	d := filepath.Clean(dir)
	l.mu.Lock()
	defer l.mu.Unlock()
	var walked []string
	var found *Checkout
	for {
		if c, ok := l.dirs[d]; ok {
			found = c
			break
		}
		walked = append(walked, d)
		if c, ok := checkoutAt(d); ok {
			found = &c
			break
		}
		up := filepath.Dir(d)
		if up == d {
			break
		}
		d = up
	}
	for _, w := range walked {
		l.dirs[w] = found
	}
	if found == nil {
		return Checkout{}, false
	}
	return *found, true
}

// checkoutAt reads d/.git: a directory is a main checkout; a file names
// the git directory of a linked worktree (which has a commondir pointing
// at the repository's own) or of a submodule (which is its own repository).
func checkoutAt(d string) (Checkout, bool) {
	dotgit := filepath.Join(d, ".git")
	fi, err := os.Stat(dotgit)
	if err != nil {
		return Checkout{}, false
	}
	if fi.IsDir() {
		return Checkout{Root: d, Repo: dotgit}, true
	}
	b, err := os.ReadFile(dotgit)
	if err != nil {
		return Checkout{}, false
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return Checkout{}, false
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(d, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	if cd, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		common := strings.TrimSpace(string(cd))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
		return Checkout{Root: d, Repo: filepath.Clean(common), Linked: true}, true
	}
	return Checkout{Root: d, Repo: gitdir}, true
}

// Changes are a checkout's pending files, relative to its root.
type Changes struct {
	// Dirty is every changed, staged or untracked file (not ignored ones).
	Dirty []string
	// Branch is the files changed by commits on the checkout's branch that
	// no other branch or remote has (the branch's own pushed copy aside:
	// pushed is not merged). Nil unless asked for.
	Branch []string
}

// Pending reads a checkout's changes; branch adds its branch's own
// commits' files.
func Pending(ctx context.Context, root string, branch bool) (Changes, error) {
	out, err := git(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return Changes{}, err
	}
	var ch Changes
	for _, e := range bytes.Split(out, []byte{0}) {
		// "dir/" is a nested repository (a linked worktree inside the
		// checkout, say), not a file
		if len(e) > 3 && e[len(e)-1] != '/' {
			ch.Dirty = append(ch.Dirty, string(e[3:]))
		}
	}
	if !branch {
		return ch, nil
	}
	head, err := git(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ch, err
	}
	args := []string{"log", "--format=", "--name-only", "-z", "HEAD", "--not"}
	if b := strings.TrimSpace(string(head)); b != "" && b != "HEAD" {
		args = append(args, "--exclude="+b, "--branches", "--exclude=*/"+b, "--remotes")
	} else {
		args = append(args, "--branches", "--remotes")
	}
	out, err = git(ctx, root, args...)
	if err != nil {
		return ch, err
	}
	seen := map[string]bool{}
	ch.Branch = []string{}
	for _, f := range strings.FieldsFunc(string(out), func(r rune) bool { return r == 0 || r == '\n' }) {
		if !seen[f] {
			seen[f] = true
			ch.Branch = append(ch.Branch, f)
		}
	}
	return ch, nil
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotePath=false"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}

// File is one file of a repository: Repo is the common git directory
// (Checkout.Repo), Path the file relative to its working tree's root.
type File struct{ Repo, Path string }

// Session is one live session's pending files.
type Session struct {
	Name  string
	Files []File
}

// Hit is another session with pending changes to some of the same files.
type Hit struct {
	With  string
	Files []string // the shared paths, sorted
}

// Find returns, for each session that shares a pending file with another,
// those other sessions ordered by name, each with the files they share.
func Find(sessions []Session) map[string][]Hit {
	owners := map[File][]string{}
	for _, s := range sessions {
		seen := map[File]bool{}
		for _, f := range s.Files {
			if !seen[f] {
				seen[f] = true
				owners[f] = append(owners[f], s.Name)
			}
		}
	}
	type pair struct{ a, b string }
	shared := map[pair]map[string]bool{}
	for f, names := range owners {
		for i, a := range names {
			for _, b := range names[i+1:] {
				if a == b {
					continue
				}
				for _, p := range []pair{{a, b}, {b, a}} {
					if shared[p] == nil {
						shared[p] = map[string]bool{}
					}
					shared[p][f.Path] = true
				}
			}
		}
	}
	out := map[string][]Hit{}
	for p, files := range shared {
		h := Hit{With: p.b}
		for f := range files {
			h.Files = append(h.Files, f)
		}
		sort.Strings(h.Files)
		out[p.a] = append(out[p.a], h)
	}
	for _, hs := range out {
		sort.Slice(hs, func(i, j int) bool { return hs[i].With < hs[j].With })
	}
	return out
}
