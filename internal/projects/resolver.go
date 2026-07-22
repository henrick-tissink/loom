// Package projects is the single authority for attribution and visibility
// (spec docs/superpowers/specs/2026-07-22-projects-foundation-design.md §4 and
// §6.1). Every surface — rail, Finished, search, notifications, workflow runs,
// the launcher — calls into here; nothing re-derives which project a session
// belongs to. A second derivation is how a hidden client leaks: the surface
// that forgets a branch still passes its own test.
//
// It sits under both frontends and deliberately outside `status` and `store`:
// the engine never learns about projects (§6.2a), and the store stays a dumb
// pipe that persists flags without knowing what they mean.
package projects

import (
	"path/filepath"
	"strings"

	"github.com/henricktissink/loom/internal/store"
)

// Attribution is the answer to "which project owns this directory". It is
// computed, never stored — zero data migration, and every historical
// transcript groups correctly the moment a project exists. Reassigning a repo
// re-attributes its history immediately because the scan keys on repo paths,
// not only on roots.
type Attribution struct {
	Root    string // project root; "" is the reserved Ungrouped row
	Name    string
	Hidden  bool
	Solo    bool
	Missing bool
}

// target is one entry of §4's target set, {projects.root} ∪ {project_repos.path}.
type target struct {
	raw         string // as stored (canonicalized on write)
	resolved    string // EvalSymlinks(raw), == raw when it does not resolve
	projectRoot string // a repo match resolves to its project's root
}

// Resolver answers attribution and visibility over one snapshot of the project
// tables. It is immutable and cheap to rebuild; the service rebuilds it
// read-through rather than caching, because loom.db is the runtime source of
// truth and a startup snapshot is exactly why a project created in-app was
// listed but not launchable.
type Resolver struct {
	targets []target
	byRoot  map[string]Attribution

	ungrouped Attribution

	// soloRoot is the soloed project's root, "" when solo is not in force.
	// A soloed project whose root is `missing` does NOT set it: see Visible.
	soloRoot  string
	anyHidden bool
}

// NewResolver builds the authority from raw store rows. Symlinks are resolved
// HERE, once per project and repo — O(projects), not O(sessions) — because the
// stored cwd of a session may never be rewritten: transcript.ProjectDirName
// keys transcripts on the raw cwd, so canonicalizing one would break status
// polling and the memory index. Handling them at the comparison site is what
// makes /var vs /private/var attribute the same without touching a session row.
func NewResolver(ps []store.Project, repos []store.ProjectRepo) *Resolver {
	r := &Resolver{byRoot: make(map[string]Attribution, len(ps))}
	seen := make(map[string]bool, len(ps)+len(repos))

	for _, p := range ps {
		a := Attribution{Root: p.Root, Name: p.Name, Hidden: p.Hidden, Solo: p.Solo, Missing: p.Missing}
		r.byRoot[p.Root] = a
		if p.Hidden {
			r.anyHidden = true
		}
		if p.Solo && !p.Missing {
			r.soloRoot = p.Root
		}
		if p.Root == store.UngroupedRoot {
			// The reserved row is excluded from the prefix scan — an empty
			// root prefixes EVERY path — and serves as the fallback instead.
			r.ungrouped = a
			continue
		}
		r.targets = append(r.targets, newTarget(p.Root, p.Root))
		seen[clean(p.Root)] = true
	}

	for _, m := range repos {
		if m.Path == store.UngroupedRoot {
			continue
		}
		// If a path is both a project root and a member repo, the project root
		// wins (§4). Skipping here rather than sorting keeps the rule in one
		// place: whichever row loses, the winning target still resolves to a
		// project root, so attribution is unchanged either way — but the
		// association a surface reads off the target is not.
		if seen[clean(m.Path)] {
			continue
		}
		seen[clean(m.Path)] = true
		r.targets = append(r.targets, newTarget(m.Path, m.ProjectRoot))
	}
	return r
}

func newTarget(path, projectRoot string) target {
	raw := clean(path)
	return target{raw: raw, resolved: resolve(raw), projectRoot: projectRoot}
}

// Attribute reports the project owning cwd, by LONGEST match over the target
// set. The second return is false when nothing matched, in which case the
// reserved Ungrouped row comes back as the attribution — callers that group
// can use it directly, and callers that filter must treat it as fail-closed
// (see Visible).
func (r *Resolver) Attribute(cwd string) (Attribution, bool) {
	best := -1
	var bestRoot string
	for _, t := range r.targets {
		n := match(cwd, t.raw)
		if m := match(cwd, t.resolved); m > n {
			n = m
		}
		if n > best {
			best, bestRoot = n, t.projectRoot
		}
	}
	if best < 0 {
		return r.ungrouped, false
	}
	a, ok := r.byRoot[bestRoot]
	if !ok {
		// A membership row pointing at a root with no project row. The store
		// refuses to create one, but a hand-edited DB can; degrade to
		// unattributed rather than inventing a nameless project, so hiding
		// stays fail-closed.
		return r.ungrouped, false
	}
	return a, true
}

// match reports the length of root when cwd lies at or under it, else -1.
// Matching is SEGMENT-WISE, never a raw string prefix: `…/HappyPay/HappyPay`
// is a string prefix of the five real siblings HappyPayCLM, HappyPayCoreApi,
// HappyPayMembers, HappyPayMerchants and HappyPaySavaToolset, every one of
// which would otherwise attribute — and hide — with the wrong project.
func match(cwd, root string) int {
	if root == "" || cwd == "" {
		return -1
	}
	if cwd == root {
		return len(root)
	}
	if strings.HasPrefix(cwd, root+string(filepath.Separator)) {
		return len(root)
	}
	return -1
}

// Filtering reports whether anything is being hidden at all. When false every
// row is visible and the fail-closed rule is dormant — an unattributable row
// must not vanish from a user who has hidden nothing.
func (r *Resolver) Filtering() bool { return r.anyHidden || r.soloRoot != "" }

// SoloRoot is the soloed project's root, or "" when solo is not in force. It
// is "" for a soloed project whose root is `missing`: that case degrades to
// "nothing hidden", never to "everything hidden" — a demo that shows too
// little is a confusing bug, one that shows a client is the failure this
// feature exists to prevent, and an empty rail would be blamed on Loom.
func (r *Resolver) SoloRoot() (string, bool) { return r.soloRoot, r.soloRoot != "" }

// Visible applies §6.1's predicate over the session's WHOLE directory set —
// cwd ∪ add_dirs — so a session whose cwd sits in a visible project while it
// edits a hidden project's repo is hidden. Visibility is an AND over the set;
// any single dir that must not be on screen takes the whole row with it.
//
//	if any project has solo=1 → visible ⟺ solo
//	else                      → visible ⟺ !hidden
//
// `hidden` is never consulted under solo and never written by it, so leaving
// solo restores the prior state exactly.
func (r *Resolver) Visible(dirs ...string) bool {
	if !r.Filtering() {
		return true
	}
	if len(dirs) == 0 {
		return false // nothing to attribute — fail closed
	}
	for _, d := range dirs {
		a, ok := r.Attribute(d)
		if !ok {
			// Fail closed: while anything is hidden, a row we cannot place
			// (the one live transcript with cwd='', an adopted orphan under a
			// path no project claims) is treated as hidden. Solo also
			// suppresses Ungrouped, and this is the same branch.
			return false
		}
		if r.soloRoot != "" {
			if a.Root != r.soloRoot {
				return false
			}
			continue
		}
		if a.Hidden {
			return false
		}
	}
	return true
}

// ProjectVisible applies the same predicate to a project row itself, for
// surfaces that render sections rather than sessions. Under solo the reserved
// Ungrouped row is suppressed along with every other non-soloed project.
func (r *Resolver) ProjectVisible(root string) bool {
	if !r.Filtering() {
		return true
	}
	if r.soloRoot != "" {
		return root == r.soloRoot
	}
	a, ok := r.byRoot[root]
	return !ok || !a.Hidden
}

// Project returns a known project row by root, for surfaces holding an
// already-attributed root.
func (r *Resolver) Project(root string) (Attribution, bool) {
	a, ok := r.byRoot[root]
	return a, ok
}

// Canonical is §4's WRITE-side canonicalization: Abs+Clean, applied to
// discovery output, folder-picker results and membership inserts. A trailing
// slash otherwise breaks both arms of the segment-wise match AND mints a
// second primary-key row for the same directory. Symlinks are deliberately not
// resolved here — that is a comparison-site concern (see NewResolver).
func Canonical(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		// Abs only fails with no working directory. Degrade to Clean rather
		// than dropping the path: a slightly-wrong target still launches.
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

// resolve is the comparison-site symlink resolution. A path that does not
// resolve (missing project, permission bit) degrades to itself: the raw form
// still matches, so a vanished root loses symlink tolerance, not attribution.
func resolve(p string) string {
	if p == "" {
		return ""
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return rp
}

// clean normalizes a STORED target for comparison only. Rows written through
// Canonical are already clean; this is defence for a row that predates it or
// arrived by hand, and it never touches a session cwd.
func clean(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}
