package gitdiff

import (
	"path"
	"sort"
	"strings"
)

// Divergence is what a task touched that its manifest did not say it would.
//
// A detector, never the isolation mechanism: the worktree is what keeps two
// children apart. Reading this as declared-ownership-as-isolation is the exact
// misreading the slice 1 ablation forbids — declared ownership measured *below*
// the single-agent baseline. This says what went wrong afterwards.
type Divergence struct {
	// Outside is every touched file that matches none of the task's own
	// declared paths, sorted. Non-blocking, but the merge approval asks for a
	// second, explicit acknowledgement when it is non-empty.
	Outside []string `json:"outside,omitempty"`
	// Siblings maps another task's id to the touched files that fall inside
	// *its* declared paths. Stronger than Outside: it predicts the merge
	// conflict before integration reaches it.
	Siblings map[string][]string `json:"siblings,omitempty"`
}

// Empty reports whether there is nothing to acknowledge.
func (d Divergence) Empty() bool { return len(d.Outside) == 0 && len(d.Siblings) == 0 }

// Diverge classifies touched files against one task's declared paths and its
// siblings' declared paths (sibling task id → globs; the subject task's own id
// must not be a key, and is skipped if it is).
//
// An empty declared set makes every touched file Outside. The rejected
// alternative — treating "declared nothing" as "declared everything" — turns
// the detector silently off exactly when the manifest is least specific, and
// silence is the one failure mode this codebase does not accept. It also puts
// the pressure where it belongs: on the author, to declare paths.
func Diverge(files []string, declared []string, siblings map[string][]string) Divergence {
	var d Divergence
	own := cleanPatterns(declared)
	sibs := make(map[string][]string, len(siblings))
	for id, pats := range siblings {
		sibs[id] = cleanPatterns(pats)
	}

	seenOut := map[string]bool{}
	for _, f := range files {
		f = normalize(f)
		if f == "" {
			continue
		}
		if !matchAny(own, f) && !seenOut[f] {
			seenOut[f] = true
			d.Outside = append(d.Outside, f)
		}
		for id, pats := range sibs {
			if !matchAny(pats, f) {
				continue
			}
			if d.Siblings == nil {
				d.Siblings = map[string][]string{}
			}
			if !contains(d.Siblings[id], f) {
				d.Siblings[id] = append(d.Siblings[id], f)
			}
		}
	}
	sort.Strings(d.Outside)
	for id := range d.Siblings {
		sort.Strings(d.Siblings[id])
	}
	return d
}

// Match reports whether a repo-relative path falls inside a declared glob set.
func Match(patterns []string, file string) bool {
	return matchAny(cleanPatterns(patterns), normalize(file))
}

func matchAny(patterns []string, file string) bool {
	for _, p := range patterns {
		if matchGlob(p, file) {
			return true
		}
	}
	return false
}

// matchGlob is segment-wise glob matching with `**`. filepath.Match cannot be
// used directly: it has no `**`, and its `*` happily crosses `/`, so
// "internal/*" would match "internal/a/b/c.go" — the opposite of what a
// manifest author writing "internal/auth/**" means. Written here rather than
// pulled in, because the house rule is no new Go dependencies.
//
//   - `**` matches zero or more whole segments
//   - `*`, `?`, `[…]` match within one segment (path.Match semantics)
//   - a pattern with no metacharacters also matches everything beneath it, so
//     "internal/auth" covers "internal/auth/x.go". An author who writes a bare
//     directory means the directory; failing that would report every file in it
//     as diverged, which is noise that trains the human to click through the
//     acknowledgement.
func matchGlob(pattern, file string) bool {
	if pattern == "" {
		return false
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return file == pattern || strings.HasPrefix(file, pattern+"/")
	}
	return segMatch(strings.Split(pattern, "/"), strings.Split(file, "/"))
}

// segMatch is the standard two-pointer wildcard walk, iterative so a pattern
// full of `**` cannot blow the stack.
func segMatch(pat, seg []string) bool {
	pi, si := 0, 0
	star, match := -1, 0
	for si < len(seg) {
		switch {
		case pi < len(pat) && pat[pi] == "**":
			star, match = pi, si
			pi++
		case pi < len(pat) && segOK(pat[pi], seg[si]):
			pi++
			si++
		case star >= 0:
			// Back up: let the last `**` swallow one more segment.
			pi = star + 1
			match++
			si = match
		default:
			return false
		}
	}
	for pi < len(pat) && pat[pi] == "**" {
		pi++
	}
	return pi == len(pat)
}

func segOK(pat, seg string) bool {
	ok, err := path.Match(pat, seg)
	return err == nil && ok
}

func cleanPatterns(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p = normalize(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalize puts manifest globs and git's output into the same alphabet:
// slash-separated, repo-relative, no "./" prefix, no trailing slash. path.Clean
// is safe on patterns here because none of the metacharacters it could disturb
// (".", "..") are legal in a declared path — §4.4 rejects escapes at load.
func normalize(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return ""
	}
	// path.Clean("a/**") is a no-op, but it would eat a trailing "/" and a
	// leading "./", which is the whole point of calling it.
	p = strings.TrimSuffix(path.Clean(p), "/")
	if p == "." {
		return ""
	}
	return p
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
