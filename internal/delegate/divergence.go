package delegate

import (
	"encoding/json"
	"fmt"

	"github.com/henricktissink/loom/internal/gitdiff"
)

// Divergence reporting (§12.3.1–2), which §2 puts inside slice 3a.
//
// §12.3.3's spawn-time snapshot — writes reaching OUTSIDE the worktree by
// absolute path — is a DIFFERENT mechanism (a dirty-file fingerprint store plus
// a comparator, not `git diff`) and §2 excludes it from 3a. It is not started
// here, and nothing in this file half-implements it.
//
// This is a DETECTOR, not the isolation mechanism. The worktree is what keeps
// two children apart; this says what went wrong afterwards. Reading it as
// declared-ownership-as-isolation is the specific misreading slice 1 §11's
// ablation forbids.

// TaskDivergence computes what a task touched that its manifest did not say it
// would, on the task's own branch, relative to its pinned base.
//
// base is `delegation_tasks.base_sha` — the commit the worktree was cut from,
// pinned once at run creation — and the capture is `base...HEAD`, three dots,
// so the report is what THIS TASK did and never what the base's own line has
// done since (gitdiff.Capture's comment has the argument).
//
// A git failure is returned as an error and NOT as an empty Divergence. Empty
// means "declared everything it touched", which is the answer a human acts on;
// a broken capture that renders as empty is a detector that silently reports
// clean, and silence is the one failure mode this codebase does not accept.
func TaskDivergence(worktree, base string, m Manifest, t Task) (gitdiff.Divergence, error) {
	if worktree == "" {
		return gitdiff.Divergence{}, fmt.Errorf("delegate: task %q has no worktree to inspect", t.ID)
	}
	if base == "" {
		return gitdiff.Divergence{}, fmt.Errorf("delegate: task %q has no pinned base", t.ID)
	}
	d := gitdiff.SinceBase(worktree, base, "")
	if d.Error != "" {
		return gitdiff.Divergence{}, fmt.Errorf("delegate: task %q divergence: %s", t.ID, d.Error)
	}
	// Untracked files count as touched. A child that created a file outside its
	// declared paths and has not committed it yet has still done the thing the
	// detector exists to notice, and the merge gate is not the first place a
	// human should learn about it. --exclude-standard means .gitignore'd build
	// output is already out of the set, so this is not the noise it looks like.
	files := append(append([]string{}, d.Files...), d.Untracked...)
	return gitdiff.Diverge(files, t.Paths, SiblingPaths(m, t)), nil
}

// SiblingPaths is the declared-path set of every OTHER task in the same repo,
// keyed by task id — §12.3.2's comparison.
//
// Same repo only. Two tasks in different repos cannot collide in one work tree,
// so pairing them would report a divergence that predicts no merge conflict,
// and a detector that cries wolf is a detector a human learns to click through.
func SiblingPaths(m Manifest, t Task) map[string][]string {
	out := map[string][]string{}
	for _, other := range m.Tasks {
		if other.ID == t.ID || other.Repo != t.Repo || len(other.Paths) == 0 {
			continue
		}
		out[other.ID] = other.Paths
	}
	return out
}

// EncodeDivergence renders a Divergence for `delegation_tasks.divergence`, and
// the EMPTY STRING — not "{}" — for an empty one, matching EncodeFlags' rule and
// the column default. An untouched row and a cleared row must be byte-identical
// or every "has anything changed" comparison gets a false positive the first
// time a task diverges and is then corrected.
func EncodeDivergence(d gitdiff.Divergence) string {
	if d.Empty() {
		return ""
	}
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodeDivergence parses the stored value. An unparseable column degrades to
// empty rather than to an error, the same "degrade, never block" rule
// DecodeFlags follows — but the caller keeps the `diverged` FLAG, which is
// stored separately, so a corrupt list costs the file names and never the
// warning itself.
func DecodeDivergence(s string) gitdiff.Divergence {
	var d gitdiff.Divergence
	if s == "" {
		return d
	}
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return gitdiff.Divergence{}
	}
	return d
}
