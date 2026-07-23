package delegate

import (
	"strconv"
	"strings"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
)

// §14.1, BINDING. A delegation child is an ordinary sessions row flowing through
// the same DTO and hiding path as everything else, and its cwd is a worktree
// under ~/.loom — deliberately outside $LOOM_WORKSPACE. projects.Resolver
// matches only over {projects.root} ∪ {project_repos.path}, so a worktree path
// matches no target, Attribute returns ok=false, and Visible FAILS CLOSED.
//
// Two live consequences, both wrong:
//
//   - the moment ANY project is hidden or solo is on, Filtering() is true and
//     every delegation child vanishes from the rail, Finished, search, needs-you
//     counts and notifications — INCLUDING when the user solos precisely the
//     project the run belongs to. The one situation where you most want to watch
//     a run is the one that blanks it;
//   - with nothing hidden, Attribute returns the reserved Ungrouped row, so
//     children render under Ungrouped rather than under their project (the rail
//     sections on projectRoot, which would be "").
//
// Fail-closed is the right default for a path Loom cannot place. A delegation
// child is not such a path: Loom created it and knows exactly which project it
// belongs to. So the fix is an explicit override keyed on IDENTITY, not on path
// prefix — sessions.delegation carries "<runID>:<taskID>", and the run row
// carries project_root.
//
// Rejected: registering each run's worktree roots as ephemeral resolver targets.
// It works, but it re-derives attribution from paths a second time, adds targets
// to an O(sessions × targets) scan for EVERY session in the process, and leaves
// stale targets behind after a crash. Identity beats geometry here.

// Runs is the narrow store slice the override needs. An interface so the wrapper
// is testable without a DB and so internal/projects keeps no knowledge of
// delegation whatsoever — its own tests must stay exactly as they are.
type Runs interface {
	// DelegationRunProjectRoot returns the run's project root. ok=false for an
	// unknown id is the CORRECT answer and not an error: §14.1 says a delegation
	// value naming a run that no longer exists falls through to the prefix scan
	// and thus to fail-closed, because a deleted run is exactly the case where
	// the conservative answer is right.
	DelegationRunProjectRoot(id int64) (string, bool, error)
}

// Attributor is the thin delegation-aware wrapper the DTO layer calls instead of
// touching projects.Resolver directly for session rows. internal/projects is
// UNCHANGED by this slice — the override wraps it rather than editing it.
type Attributor struct {
	Resolver *projects.Resolver
	Runs     Runs
}

// Attribute answers "which project owns this session", consulting the delegation
// override BEFORE the prefix scan. A non-delegation row, or a delegation row
// whose run has been deleted, falls through to Resolver.Attribute unchanged.
func (a *Attributor) Attribute(row store.SessionRow) (projects.Attribution, bool) {
	if a == nil || a.Resolver == nil {
		return projects.Attribution{}, false
	}
	if root, ok := a.delegationRoot(row); ok {
		// Project() looks the root up in the resolver's own snapshot, so the
		// Attribution carries the live Hidden/Solo/Missing flags rather than a
		// synthesized row. A run whose project has since been deleted answers
		// ok=false here and falls through — same conservative direction as a
		// deleted run.
		if att, known := a.Resolver.Project(root); known {
			return att, true
		}
	}
	return a.Resolver.Attribute(row.Cwd)
}

// delegationRoot resolves a row's delegation column to its run's project root.
//
// Every failure — no column, unparseable column, unknown run, store error —
// answers ok=false and thus falls through to the prefix scan and to
// fail-closed. That is deliberate for the store error too: a transient DB
// failure must not silently promote a child into visibility, because the whole
// point of the containment rule is that the conservative answer is the safe
// one.
func (a *Attributor) delegationRoot(row store.SessionRow) (string, bool) {
	if row.Delegation == "" || a.Runs == nil {
		return "", false
	}
	runID, _, ok := ParseDelegation(row.Delegation)
	if !ok {
		return "", false
	}
	root, ok, err := a.Runs.DelegationRunProjectRoot(runID)
	if err != nil || !ok || root == "" {
		return "", false
	}
	return root, true
}

// Visible is §6.1's predicate for a session row, with the same override.
//
// The add-dir subtlety is load-bearing and is easy to get backwards.
// Resolver.Visible ANDs over cwd ∪ add_dirs, so a session editing a hidden
// project's repo is hidden. A delegation child's add-dirs are LOOM'S OWN
// directories (§6.2 grants the .meta dir and nothing else), which no project
// claims and which therefore fail closed — ANDing them in would blank every
// child even under the override, reintroducing the exact bug the override
// exists to fix. So for a delegation row visibility is decided by its run's
// project alone.
//
// The consequence is the one the user expects: solo the run's own project and
// the children are VISIBLE; solo a different project and they are hidden; hide
// nothing and they group under their project rather than under Ungrouped.
func (a *Attributor) Visible(row store.SessionRow) bool {
	if a == nil || a.Resolver == nil {
		// No resolver means we cannot know what is hidden. Showing the row is
		// the safe failure at THIS layer, matching the DTO layer's own nil
		// handling: the alternative is a blank UI blamed on Loom.
		return true
	}
	root, ok := a.delegationRoot(row)
	if !ok {
		return a.Resolver.Visible(sessionDirs(row)...)
	}
	// Decided by the run's project ALONE — deliberately not ANDed with the
	// row's cwd or add-dirs. See the doc comment above: a child's cwd is a
	// worktree no project claims and its add-dirs are Loom's own .meta
	// directory, so both fail closed, and ANDing either in would blank every
	// child even under the override — reintroducing the exact bug this exists
	// to fix.
	return a.Resolver.ProjectVisible(root)
}

// sessionDirs is the directory set a non-delegation row is attributed over:
// cwd plus every add-dir. It matches the DTO layer's own rule — a session
// editing a hidden project's repo through --add-dir is hidden — and exists
// here so the fall-through path is identical to what the caller would have
// done without the wrapper.
// AddDirs is stored as raw JSON (the store stays a dumb record), so it goes
// through session.DecodeAddDirs — the same decoder the DTO layer uses, not a
// second json.Unmarshal that could disagree about a malformed value.
func sessionDirs(row store.SessionRow) []string {
	dirs := make([]string, 0, 4)
	if row.Cwd != "" {
		dirs = append(dirs, row.Cwd)
	}
	return append(dirs, session.DecodeAddDirs(row.AddDirs)...)
}

// FormatDelegation and ParseDelegation are the only two places the composite
// "<runID>:<taskID>" is written or read. It is a composite and not two columns
// because the column is a pointer for attribution, not a record — the runner
// gets its task by store.GetDelegationTaskBySession, which never parses this.
//
// ParseDelegation returns ok=false for anything it cannot parse, including "".
// A corrupt value degrades to "not a delegation child" and therefore to
// fail-closed, which is the safe direction: a child that is wrongly hidden is a
// support question, a hidden client that is wrongly shown is the failure this
// whole feature exists to prevent.
func FormatDelegation(runID int64, taskID string) string {
	return strconv.FormatInt(runID, 10) + ":" + taskID
}

func ParseDelegation(s string) (runID int64, taskID string, ok bool) {
	// SplitN(…, 2) rather than Split: a task id is [a-z0-9-] by §4.4 rule 3 and
	// so cannot contain ':', but this parses UNTRUSTED column content, and a
	// hand-edited row must degrade rather than mis-split.
	idStr, task, found := strings.Cut(s, ":")
	if !found || idStr == "" || task == "" {
		return 0, "", false
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		// id <= 0 is rejected because SQLite rowids start at 1, so a
		// zero or negative value can only be corruption — and resolving it
		// would look up a run that cannot exist, which is the same
		// fall-through, reached less obviously.
		return 0, "", false
	}
	return id, task, true
}
