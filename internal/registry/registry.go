// Package registry discovers workspace projects and their repos (spec §3).
package registry

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henricktissink/loom/internal/transcript"
)

// Repo is a launchable working tree. The name is deliberately not "Project":
// a project is a named initiative that may own several repos, so the two are
// distinct axes and conflating them made nested repos unaddressable.
type Repo struct {
	Label string // "project/repo", or the bare basename when the repo is its project's root
	Path  string // absolute, Abs+Clean canonical; used as cwd
}

// Project is a root directory plus the repos it owns. Discovery emits both:
// spec §4's target set is {roots} ∪ {repo paths}, and a project with no repos
// at all (rule 3) is still launchable at its root, so a flat []Repo could not
// express it.
type Project struct {
	Name  string // root basename; never a user-editable name — labels must survive a rename (§2)
	Root  string // absolute, Abs+Clean canonical
	Repos []Repo // may be empty (rule 3); sorted by Label
}

// Discover applies spec §3's ordered, first-match decision list to each
// immediate subdir of workspaceRoot:
//
//  1. is a repo                    → single-repo project, DO NOT descend
//  2. else any immediate child is a repo → project owning its launchable
//     children, regardless of whether the parent itself has a transcript dir
//  3. else has a transcript dir    → zero-repo project rooted at itself
//  4. else                         → not a project
//
// The two predicates the old isProject conflated are now separate: isRepo
// (has .git) governs DESCENT, a claude transcript dir governs LAUNCHABILITY.
// Testing launchability before descending is what hid 16 real repos — Innostream
// and HappyPay have no .git but do have transcript dirs, so they self-promoted
// to leaves and their children were never scanned. Rule 2's "regardless" clause
// is that bug's fix.
//
// Rule 1 not descending preserves today's behaviour for submodules and vendored
// trees, which are working copies of the parent's history, not siblings of it.
//
// Discovery is a pure function of the filesystem: no store access, so the
// reconciler (spec §7) stays the sole owner of curated membership.
func Discover(workspaceRoot, claudeConfigDir string) ([]Project, error) {
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return nil, err
	}
	var ps []Project
	for _, e := range entries {
		if skipEntry(e) {
			continue
		}
		path := canonical(filepath.Join(workspaceRoot, e.Name()))

		if isRepo(path) { // rule 1
			ps = append(ps, project(path, []string{path}))
			continue
		}

		children, err := os.ReadDir(path)
		if err != nil {
			// An unreadable dir is skipped, never fatal: one bad permission bit
			// in the workspace must not cost the user every other project.
			children = nil
		}
		var members []string
		anyRepo := false
		for _, c := range children {
			if skipEntry(c) {
				continue
			}
			cpath := filepath.Join(path, c.Name())
			cIsRepo := isRepo(cpath)
			anyRepo = anyRepo || cIsRepo
			// Membership is launchability, not repo-ness: a child with only a
			// transcript dir has live history to resume and was reachable
			// before this change. It rides along on a group that qualified via
			// a .git sibling — it cannot promote the group on its own, because
			// rule 2 keys on descent, which only .git governs.
			if cIsRepo || hasTranscript(cpath, claudeConfigDir) {
				members = append(members, cpath)
			}
		}
		if anyRepo { // rule 2
			ps = append(ps, project(path, members))
			continue
		}
		if hasTranscript(path, claudeConfigDir) { // rule 3
			ps = append(ps, project(path, nil))
			continue
		}
		// rule 4: not a project. Depth stays one level, so a nested group
		// (Innostream/albedo/*) is reachable only via "+ New project" (§10).
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Root < ps[j].Root })
	return ps, nil
}

// ChildRepos returns the immediate child working trees of root, sorted. It is
// §3's rule-2 arm applied to a single directory the user picked, and it backs
// "+ New project"'s prefilled repo checklist (§8).
//
// It deliberately does NOT include rule 2's transcript-only children: rule 2
// lets those ride along on a group that already qualified via a .git sibling,
// which is a statement about restoring history that a workspace scan found, not
// about what a user is choosing to group. Offering a non-repo directory as a
// prefilled, pre-checked member would silently make it a "repo" of the project.
// The user can still add any path by hand through add-repo-outside-root.
//
// Failures return nil rather than an error: a root that cannot be read is a
// checklist with nothing prefilled, and the modal still offers the root itself
// (a project of one is a legitimate shape, §2).
func ChildRepos(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if skipEntry(e) {
			continue
		}
		p := canonical(filepath.Join(root, e.Name()))
		if isRepo(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Repos flattens discovery output to the launchable working trees, for the
// surfaces that still take a flat list.
func Repos(ps []Project) []Repo {
	var out []Repo
	for _, p := range ps {
		out = append(out, p.Repos...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// project builds a Project and applies spec §2's label rule: "project/repo",
// except for a repo that IS its project's root, which keeps the bare basename
// ("loom", not "loom/loom"). The test is the path rule §5 blesses rather than
// len(repos)==1 — rule 3 and out-of-root membership both produce roots that are
// also repos with siblings, and a one-child group must stay "group/child" or a
// saved workflow naming it stops resolving.
func project(root string, repoPaths []string) Project {
	name := filepath.Base(root)
	p := Project{Name: name, Root: root}
	for _, rp := range repoPaths {
		label := name + "/" + filepath.Base(rp)
		if rp == root {
			label = name
		}
		p.Repos = append(p.Repos, Repo{Label: label, Path: rp})
	}
	sort.Slice(p.Repos, func(i, j int) bool { return p.Repos[i].Label < p.Repos[j].Label })
	return p
}

// skipEntry drops dotfiles, plain files, and symlinks. Symlinks fall out for
// free: os.ReadDir's DirEntry.Type() comes from the dirent, so a symlink to a
// directory reports IsDir()==false and never reaches isRepo. Stated rather than
// changed — following symlinks would surface the same tree under two paths,
// which §2's exclusive membership forbids. (This is why Innostream/leikur, a
// symlink to bankenstein, is not in the restored set; it is also a duplicate of
// a repo already restored.)
func skipEntry(e os.DirEntry) bool {
	return !e.IsDir() || strings.HasPrefix(e.Name(), ".")
}

// isRepo reports whether path is a git working tree. .git may be a file rather
// than a directory for worktrees and submodules, so this stats rather than
// checking for a dir.
func isRepo(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// hasTranscript reports whether claude has history for path. It governs
// launchability only — never descent.
func hasTranscript(path, claudeConfigDir string) bool {
	_, err := os.Stat(filepath.Join(claudeConfigDir, "projects", transcript.ProjectDirName(path)))
	return err == nil
}

// canonical is spec §4's write-side canonicalization: Abs+Clean, so a trailing
// slash cannot break segment-wise matching or mint a second row for the same
// directory. Symlinks are deliberately NOT resolved — transcript.ProjectDirName
// keys transcripts on the raw cwd, so rewriting a path here would break status
// polling and the memory index. Abs only fails if the process has no working
// directory; degrade to the joined path rather than dropping the project.
func canonical(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}
