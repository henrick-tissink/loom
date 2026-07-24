package delegate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henricktissink/loom/internal/gitdiff"
)

// §12.3.3 — writes OUTSIDE the worktree.
//
// This is a DIFFERENT MECHANISM from §12.3.1/2's divergence and shares no code
// with it. divergence.go answers "what did the child commit, and was it inside
// its declared paths" from `git diff <base>...<branch>` — a question about the
// child's own worktree. This file answers "did anything change outside the
// isolation boundary at all", which git cannot answer because the changes are in
// OTHER trees: the user's primary work trees and the integration worktrees
// granted as --add-dir.
//
// It exists because the grant cannot be narrowed. The spike is unambiguous
// (docs/spikes/2026-07-22-add-dir-spike.md): `--add-dir` confers read AND WRITE,
// silently, with no second trust prompt. §7's authorization text can discourage
// an absolute-path write and the worktree makes one impossible by accident — but
// not by intent, and "by intent" is exactly what an agent that has decided the
// task boundary is wrong will do. So: snapshot at spawn, re-walk at check and
// pre-merge, and flag any difference LOUDLY.
//
// COST, DISCLOSED, and it must stay disclosed in the UI: the walk is O(dirty
// files across every in-scope repo) per check, and a repo the human is actively
// working in produces FALSE POSITIVES — the human's own edits are
// INDISTINGUISHABLE from a child's. The flag therefore names the files and says
// "changed since spawn", never "the child wrote this", and §5.2's
// acknowledgement text says the same. A detector that overclaims is one the human
// learns to dismiss, and this one has exactly one chance to be believed.
//
// THIS IS A DETECTOR, NOT THE ISOLATION MECHANISM. Reading it as re-introducing
// declared-ownership-as-isolation is the specific misreading slice 1 §11's
// ablation forbids: the worktree keeps children apart; this says what went wrong
// afterwards.

// FileStamp is one file's identity in a snapshot: path plus mtime plus size —
// the `indexed_files` fingerprint idiom already used across this codebase.
//
// Not a content hash, deliberately. A hash is a full read of every dirty file in
// every in-scope repo on every check; the stamp is a Stat. The miss it admits is
// a write that preserves both mtime and size, which requires deliberate effort
// to produce and which no plausible agent behaviour produces by accident. The
// cost it avoids is a detector so expensive that the first thing anyone does is
// turn it off.
type FileStamp struct {
	Path string `json:"path"`
	Mod  int64  `json:"mtime"` // Unix seconds; nanoseconds are not portable across the filesystems this walks
	Size int64  `json:"size"`
}

// Snapshot is `delegation_tasks.spawn_snapshot`: directory → its dirty file set
// at spawn time.
//
// DIRTY files only, not every file. Walking every tracked file in several repos
// per check is unaffordable, and it is also the wrong question: a clean tracked
// file that a child modified BECOMES dirty, so it appears in the comparison
// anyway — as an addition to the set, which is exactly the signal wanted. What
// the dirty-only choice gives up is detecting a write that is immediately
// reverted, which is not a finding anybody can act on.
type Snapshot map[string][]FileStamp

// SnapshotDirs is the set walked for one task at spawn: every in-scope repo's
// PRIMARY working tree, plus every add-dir'd integration worktree the task was
// granted. The child's own worktree is deliberately NOT in it — changes there
// are the entire point of the exercise and are §12.3.1's business.
//
// Physically resolved, deduplicated and sorted. Resolved because the set is
// STORED and re-walked later, and an unresolved /tmp/... compared against a
// resolved /private/tmp/... on the next check reads as "the whole directory
// vanished and a new one appeared" — the loudest possible false positive, from
// a symlink nobody typed. Sorted so the encoded column is a function of the set
// and two Looms do not rewrite it back and forth (EncodeFlags' rule).
func SnapshotDirs(m Manifest, plan BasePlan) []string {
	seen := make(map[string]bool, len(m.RepoPaths)+len(plan.AddDirs))
	add := func(d string) {
		if strings.TrimSpace(d) == "" {
			return
		}
		seen[physicalPath(d)] = true
	}
	for _, dir := range m.RepoPaths {
		add(dir)
	}
	// The add-dirs are the cross-repo producers' integration worktrees, and they
	// are the half of the set that actually motivates the mechanism: the primary
	// work trees are merely reachable by absolute path, whereas an add-dir was
	// HANDED to the child with write permission that cannot be revoked.
	for _, dir := range plan.AddDirs {
		add(dir)
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// TakeSnapshot stats the dirty set of every dir. Degrades per-directory: a dir
// that is not a git work tree, or that has vanished, contributes an empty entry
// and no error, because a snapshot that refuses to be taken blocks a spawn, and
// blocking a spawn on a detector inverts the priority — the detector exists to
// annotate work, not to gate it.
func TakeSnapshot(dirs []string) Snapshot {
	if len(dirs) == 0 {
		return nil
	}
	s := make(Snapshot, len(dirs))
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		// A NON-NIL empty slice, always. The key's PRESENCE is the record that
		// this dir was snapshotted, and Compare distinguishes "snapshotted, no
		// dirty files" from "never snapshotted" by exactly that. A nil here
		// would survive the map but not the JSON round-trip (`null` decodes to
		// nil either way), so the empty slice is what keeps Encode∘Decode an
		// identity — and that identity is the whole reason the distinction
		// survives a restart.
		s[dir] = stampDirty(dir)
	}
	return s
}

// stampDirty is one directory's dirty set. Every failure degrades to an empty
// set: a snapshot that refuses to be taken blocks a spawn, and blocking a spawn
// on a DETECTOR inverts the priority.
func stampDirty(dir string) []FileStamp {
	out := []FileStamp{}
	d := gitdiff.WorkingTree(dir)
	if d.Error != "" {
		// Not a git work tree, or it has vanished between planning and spawning.
		// Empty entry, no error, and the emptiness is itself the finding on the
		// next comparison: every file that WAS dirty here reads as removed, and
		// removals are changes.
		return out
	}
	// git reports repo-relative paths, and `dir` is a work-tree root by
	// construction (m.RepoPaths, and worktree paths from Layout). Resolving the
	// top level anyway costs one exec per dir per check and removes the one way
	// this silently returns nothing: a dir one level down would produce paths
	// that stat nowhere, and every stat failure is skipped by design below.
	root := dir
	if top, err := gitOut(dir, "rev-parse", "--show-toplevel"); err == nil {
		if t := strings.TrimSpace(top); t != "" {
			root = t
		}
	}
	seen := make(map[string]bool, len(d.Files)+len(d.Untracked))
	for _, rel := range append(append([]string{}, d.Files...), d.Untracked...) {
		if rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true
		st, err := os.Stat(filepath.Join(root, rel))
		if err != nil || st.IsDir() {
			// A dirty path that will not stat is a DELETION, which git already
			// reports as dirtiness and which has no stamp to record. Skipping it
			// is not a swallowed error: the path is absent from the set at spawn
			// and at check alike, so a deletion that happened BEFORE spawn stays
			// invisible (correctly — it predates the child) and one that happens
			// AFTER shows up as the file leaving the set.
			continue
		}
		out = append(out, FileStamp{Path: rel, Mod: st.ModTime().Unix(), Size: st.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// EncodeSnapshot / DecodeSnapshot are the column's codec. The empty snapshot
// encodes as the EMPTY STRING to match the migration default (EncodeFlags'
// rule); Decode degrades to nil, which Compare treats as "no baseline", which
// renders as "not snapshotted" rather than as "nothing changed". Those two must
// never be confused: the second is a claim, the first is an admission.
//
// Note what is NOT collapsed to the empty string: a snapshot whose dirs are all
// CLEAN. `{"/repo":[]}` is a baseline that says "nothing was dirty at spawn",
// and encoding it as "" would turn the strongest possible baseline into an
// admission that there is none. Only the dir-less snapshot is empty.
func EncodeSnapshot(s Snapshot) string {
	if len(s) == 0 {
		return ""
	}
	b, err := json.Marshal(s)
	if err != nil {
		// Unreachable for a map of plain structs, and handled anyway: the caller
		// writes this into a column, and a panic here would take down a spawn
		// for a detector.
		return ""
	}
	return string(b)
}

func DecodeSnapshot(s string) Snapshot {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out Snapshot
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		// nil, not an empty Snapshot: nil means NO BASELINE, which Compare
		// renders as "not snapshotted". An empty Snapshot would mean "we looked
		// and there was nothing", which is a claim this function is in no
		// position to make about bytes it could not parse.
		return nil
	}
	return out
}

// SnapshotDrift is the comparator's finding.
type SnapshotDrift struct {
	// Changed is dir → files whose stamp differs, sorted. Includes additions and
	// removals; a file that stopped being dirty is as much a change as one that
	// started.
	Changed map[string][]string
	// NoBaseline lists dirs with no recorded snapshot — a task spawned by an
	// older Loom, or a snapshot that failed to encode. Rendered distinctly from
	// "no change", because it is an absence of evidence and the UI must not
	// present it as evidence of absence.
	NoBaseline []string
}

// Empty reports whether there is nothing to say. A drift with only NoBaseline
// entries is NOT empty: "we cannot tell" is a thing to say.
func (d SnapshotDrift) Empty() bool { return len(d.Changed) == 0 && len(d.NoBaseline) == 0 }

// Summary is the sentence the run renders, and its WORDING IS BINDING (§12.3.3).
//
// It says "changed since spawn" and it names the files. It does not say "the
// child wrote this", because the walk cannot distinguish the human's own edits
// from a child's and a repo the user is working in produces false positives by
// construction. A detector that overclaims is one the human learns to dismiss,
// and this one has exactly one chance to be believed — so the hedge is in the
// function that produces the text rather than left to each call site to
// remember.
func (d SnapshotDrift) Summary() string {
	if d.Empty() {
		return ""
	}
	var b strings.Builder
	if n := d.count(); n > 0 {
		fmt.Fprintf(&b, "%d file(s) outside this task's worktree changed since spawn", n)
		dirs := make([]string, 0, len(d.Changed))
		for dir := range d.Changed {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		for _, dir := range dirs {
			fmt.Fprintf(&b, "\n  %s: %s", dir, strings.Join(d.Changed[dir], ", "))
		}
		// The disclaimer is part of the finding, not a footnote the UI may drop.
		b.WriteString("\n  (changed since spawn — Loom cannot tell a child's write from your own)")
	}
	if len(d.NoBaseline) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "not snapshotted at spawn, so nothing can be said about: %s",
			strings.Join(d.NoBaseline, ", "))
	}
	return b.String()
}

// count is the total number of changed files across dirs.
func (d SnapshotDrift) count() int {
	n := 0
	for _, files := range d.Changed {
		n += len(files)
	}
	return n
}

// Dirs is the recorded directory set, sorted — what Check re-walks.
func (s Snapshot) Dirs() []string {
	out := make([]string, 0, len(s))
	for dir := range s {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// Compare re-walks and diffs against the baseline. Pure over its two arguments,
// so the comparison logic is testable from literals with no filesystem at all —
// which matters because the interesting cases (an added file, a removed file, a
// same-size rewrite) are tedious to stage on disk and trivial to write down.
//
// A dir in `baseline` that `now` does not cover is IGNORED rather than reported:
// Check re-walks exactly the recorded keys, so it cannot arise there, and a
// caller that deliberately narrows the set is asking a question about a subset.
// Reporting it would put "we did not look" and "there is no baseline" under one
// label, and NoBaseline's entire job is to be the second one precisely.
func Compare(baseline, now Snapshot) SnapshotDrift {
	var d SnapshotDrift
	for _, dir := range now.Dirs() {
		base, ok := baseline[dir]
		if !ok {
			// ABSENCE OF EVIDENCE. A task spawned by an older Loom, or a
			// snapshot that failed to encode. Never folded into "no change":
			// the second is a claim and this is an admission.
			d.NoBaseline = append(d.NoBaseline, dir)
			continue
		}
		if files := diffStamps(base, now[dir]); len(files) > 0 {
			if d.Changed == nil {
				d.Changed = map[string][]string{}
			}
			d.Changed[dir] = files
		}
	}
	return d
}

// diffStamps is the per-directory comparison. A file counts as changed when its
// mtime differs, its size differs, or it entered or left the dirty set —
// path-set membership is as much a change as a stamp is, because a file that
// STOPPED being dirty was written to just as surely as one that started.
func diffStamps(before, after []FileStamp) []string {
	idx := func(in []FileStamp) map[string]FileStamp {
		m := make(map[string]FileStamp, len(in))
		for _, f := range in {
			m[f.Path] = f
		}
		return m
	}
	a, b := idx(before), idx(after)
	changed := map[string]bool{}
	for path, was := range a {
		is, still := b[path]
		if !still || is != was {
			changed[path] = true
		}
	}
	for path := range b {
		if _, had := a[path]; !had {
			changed[path] = true
		}
	}
	out := make([]string, 0, len(changed))
	for path := range changed {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// Check is the whole §12.3.3 operation at one call site: re-walk the recorded
// dirs, compare, and return the drift for the caller to flag with
// FlagOutsideWrites and render. Called on every check run and again immediately
// before every merge (§12.3) — before, because a divergence discovered after a
// merge is a fact, not a gate.
//
// A NIL snapshot has no dirs to re-walk and therefore returns the EMPTY drift,
// which reads as "nothing to say" and not as "no baseline" — the one place the
// two do get conflated, and unavoidably: a task whose column is empty carries no
// record of what should have been walked. The call site that holds the manifest
// must not use this: it should Compare(DecodeSnapshot(row), TakeSnapshot(
// SnapshotDirs(m, plan))), which knows the dirs and so can report NoBaseline
// properly. See the handoff note at the bottom of this file.
func (s Snapshot) Check() SnapshotDrift {
	if len(s) == 0 {
		return SnapshotDrift{}
	}
	return Compare(s, TakeSnapshot(s.Dirs()))
}

// HANDOFF (run.go / integrate.go, §12.3.3's two call sites):
//
//	dirs := SnapshotDirs(m, plan)                       // at spawn
//	store.SetTaskSpawnSnapshot(runID, taskID, EncodeSnapshot(TakeSnapshot(dirs)), now)
//
//	drift := Compare(DecodeSnapshot(task.SpawnSnapshot), // at check AND again
//	    TakeSnapshot(SnapshotDirs(m, plan)))             // immediately pre-merge
//	if !drift.Empty() { flags = flags.With(FlagOutsideWrites) /* + drift.Summary() */ }
//
// PRE-MERGE MEANS BEFORE, not after: a divergence discovered after a merge is a
// fact, not a gate (§12.3). And the flag is FlagOutsideWrites, never
// FlagDiverged — different mechanism, different evidence, different remedy (see
// state.go's comment on the constant).
