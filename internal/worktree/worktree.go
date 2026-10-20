// Package worktree reads and removes the git worktrees `claude --worktree`
// creates, under <repo>/.claude/worktrees/<name> on a branch
// worktree-<name>.
//
// A worktree session's transcript, pane directory and registry cwd all move
// into the worktree (verified on Claude Code 2.1.303, and documented: "the
// transcript follows"), so a row's cwd IS the worktree path and everything
// here keys on it. Nothing in this package writes to a repository except
// Remove, which runs only on an explicit click and refuses whenever removal
// could lose work.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Dir is where claude puts its worktrees, relative to the repository root.
const Dir = ".claude/worktrees"

var marker = string(filepath.Separator) + filepath.Join(Dir) + string(filepath.Separator)

// Parse reports whether path is (inside) a claude worktree, and returns the
// main checkout's root and the worktree's name and root.
func Parse(path string) (repo, name, root string, ok bool) {
	i := strings.Index(path, marker)
	if i <= 0 {
		return "", "", "", false
	}
	rest := path[i+len(marker):]
	name, _, _ = strings.Cut(rest, string(filepath.Separator))
	if name == "" {
		return "", "", "", false
	}
	repo = path[:i]
	return repo, name, filepath.Join(repo, Dir, name), true
}

// SessionName is the worktree name loom passes to `claude --worktree` for a
// session: short, unique per session, and recognisably loom's.
func SessionName(sessionID string) string {
	if len(sessionID) > 8 {
		sessionID = sessionID[:8]
	}
	return "loom-" + sessionID
}

// git runs git in dir and returns its trimmed stdout, or an error carrying
// git's own message.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Toplevel is the root of the repository dir is in.
func Toplevel(ctx context.Context, dir string) (string, error) {
	return git(ctx, dir, "rev-parse", "--show-toplevel")
}

// State is what a worktree holds that removing it could lose.
type State struct {
	Branch string // "" when HEAD is detached
	// Changed counts changed and untracked files (submodules included, as
	// git status reports them).
	Changed int
	// Unique counts commits reachable from HEAD and from no other branch or
	// remote: work that exists only in this worktree's branch.
	Unique int
}

// Read reports a worktree's branch, changed files and unique commits.
func Read(ctx context.Context, root string) (State, error) {
	out, err := git(ctx, root, "status", "--porcelain=v2", "--branch")
	if err != nil {
		return State{}, err
	}
	var st State
	for _, l := range strings.Split(out, "\n") {
		switch {
		case l == "":
		case strings.HasPrefix(l, "# branch.head "):
			if h := strings.TrimPrefix(l, "# branch.head "); h != "(detached)" {
				st.Branch = h
			}
		case strings.HasPrefix(l, "#"):
		default:
			st.Changed++
		}
	}
	args := []string{"rev-list", "--count", "HEAD", "--not"}
	if st.Branch != "" {
		args = append(args, "--exclude="+st.Branch)
	}
	args = append(args, "--branches", "--remotes")
	n, err := git(ctx, root, args...)
	if err != nil {
		return st, err
	}
	st.Unique, _ = strconv.Atoi(n)
	return st, nil
}

// lockPID pulls the pid out of the lock reason claude writes ("claude
// session loom-1a2b3c4d (pid 22813 start Mon Oct 19 06:44:35 2026)").
var lockPID = regexp.MustCompile(`^claude session .*\(pid (\d+)\b`)

// pidAlive is overridable so tests can mark a pid live or dead.
var pidAlive = func(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// lockReason returns the worktree's lock reason from its main checkout, and
// whether it is locked at all.
func lockReason(ctx context.Context, repo, root string) (string, bool, error) {
	out, err := git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return "", false, err
	}
	in := false
	for _, l := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(l, "worktree "); ok {
			in = samePath(p, root)
			continue
		}
		if !in {
			continue
		}
		if l == "locked" {
			return "", true, nil
		}
		if r, ok := strings.CutPrefix(l, "locked "); ok {
			return r, true, nil
		}
	}
	return "", false, nil
}

func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	return ea == nil && eb == nil && ra == rb
}

// Remove deletes a claude worktree's directory and keeps its branch. It
// refuses, with the reason, when the worktree has changed or untracked
// files, holds commits found on no other branch or remote, or is locked by a
// running claude or by anyone else. A lock left by a claude that has exited
// (loom kills a session's pane, so claude never runs its own exit cleanup)
// is released first. git itself refuses a dirty worktree too: no --force.
func Remove(ctx context.Context, path string) error {
	repo, _, root, ok := Parse(path)
	if !ok {
		return fmt.Errorf("%s is not a claude worktree", path)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("worktree %s is already gone", root)
	}
	st, err := Read(ctx, root)
	if err != nil {
		return fmt.Errorf("could not check the worktree, so it was kept: %w", err)
	}
	if st.Changed > 0 {
		return fmt.Errorf("kept: %d changed or untracked %s", st.Changed, plural(st.Changed, "file", "files"))
	}
	if st.Unique > 0 {
		return fmt.Errorf("kept: %d %s on no other branch or remote", st.Unique, plural(st.Unique, "commit", "commits"))
	}
	reason, locked, err := lockReason(ctx, repo, root)
	if err != nil {
		return fmt.Errorf("could not read the worktree's lock, so it was kept: %w", err)
	}
	if locked {
		m := lockPID.FindStringSubmatch(reason)
		if m == nil {
			return fmt.Errorf("kept: locked (%s)", strings.TrimSpace(reason))
		}
		if pid, _ := strconv.Atoi(m[1]); pidAlive(pid) {
			return fmt.Errorf("kept: a running claude (pid %d) holds it", pid)
		}
		if _, err := git(ctx, repo, "worktree", "unlock", root); err != nil {
			return err
		}
	}
	_, err = git(ctx, repo, "worktree", "remove", root)
	return err
}

// Discard deletes a claude worktree's directory even though it holds
// changes, and keeps its branch, so commits made there survive and only
// uncommitted work and new files are lost. It is for a best-of-N attempt
// the person chose not to keep, on an explicit confirm. It still refuses
// while a running claude (or a lock it can't attribute) holds the worktree.
func Discard(ctx context.Context, path string) error {
	repo, _, root, ok := Parse(path)
	if !ok {
		return fmt.Errorf("%s is not a claude worktree", path)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("worktree %s is already gone", root)
	}
	reason, locked, err := lockReason(ctx, repo, root)
	if err != nil {
		return fmt.Errorf("could not read the worktree's lock, so it was kept: %w", err)
	}
	if locked {
		m := lockPID.FindStringSubmatch(reason)
		if m == nil {
			return fmt.Errorf("kept: locked (%s)", strings.TrimSpace(reason))
		}
		if pid, _ := strconv.Atoi(m[1]); pidAlive(pid) {
			return fmt.Errorf("kept: a running claude (pid %d) holds it", pid)
		}
		if _, err := git(ctx, repo, "worktree", "unlock", root); err != nil {
			return err
		}
	}
	_, err = git(ctx, repo, "worktree", "remove", "--force", root)
	return err
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
