package delegate

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// TaskState is the value of `delegation_tasks.state` — §13.2's single
// CAS-guarded column. It is a string, not an int, because the column is a
// string and a schema whose meaning survives `sqlite3 loom.db` is worth more
// than four bytes a row.
//
// 3a's reachable path is:
//
//	pending → ready → approved → spawning → running ⇄ blocked
//	        → checking → verified | failed → merged
//
// plus `abandoned` from anywhere. §13.2's `integrating` and `mergeable` are
// deliberately ABSENT: they only exist once Loom runs the merge itself (§10),
// which 3a does not do. Adding them now would put two unreachable states in
// every switch the next wave writes, and the compiler would not tell anyone.
type TaskState string

const (
	StatePending   TaskState = "pending"
	StateReady     TaskState = "ready"
	StateApproved  TaskState = "approved"
	StateSpawning  TaskState = "spawning"
	StateRunning   TaskState = "running"
	StateBlocked   TaskState = "blocked"
	StateChecking  TaskState = "checking"
	StateVerified  TaskState = "verified"
	StateFailed    TaskState = "failed"
	StateMerged    TaskState = "merged"
	StateAbandoned TaskState = "abandoned"
)

// Terminal reports whether no further transition is expected WITHOUT A HUMAN.
// That is the literal contract, and it is why `verified` and `failed` are in the
// set alongside the two obvious members:
//
//   - merged / abandoned: no outgoing edge exists at all;
//   - verified: the only successor is `merged`, and 3a does not merge — §10 is
//     deferred, so the merge is an act the human performs in their own tree;
//   - failed: the state machine has no outgoing edge either, and recovery is a
//     human amending the manifest or re-spawning.
//
// The deadlock predicate (§9.3, deferred) is the intended consumer. Ready
// deliberately does NOT call this: a scheduler is a safety property and it
// enumerates its own candidate states, so that a state added here cannot
// silently change which tasks Loom proposes to run — the same reason
// ActiveChildren enumerates rather than delegating to HoldsAChild.
func (s TaskState) Terminal() bool {
	switch s {
	case StateVerified, StateFailed, StateMerged, StateAbandoned:
		return true
	}
	return false
}

// HoldsAChild reports whether a task in this state is holding a live `claude`
// and therefore a slot against §6.6's cap. Blocked counts: a blocked child holds
// its worktree AND its context, which is the entire reason Loom parks children
// rather than killing them. `checking` counts for the same reason — Loom runs
// the check, the child sits idle in its worktree, and it is still a process with
// a context. `spawning` counts because the session row is written after the
// process exists (§13.3), so a launch in flight may already be a real claude.
//
// This set is duplicated by ActiveChildren's switch ON PURPOSE — see its comment
// — so the cap cannot be quietly changed from here. The duplication is asserted
// by a test rather than left to good intentions.
func (s TaskState) HoldsAChild() bool {
	switch s {
	case StateSpawning, StateRunning, StateBlocked, StateChecking:
		return true
	}
	return false
}

// Flag is one entry of `delegation_tasks.flags`, a JSON set kept deliberately
// OUT of the state column (§13.2). A state machine with a `stalled` state has to
// define the cross product of stalled × everything and stops being testable.
// Flags are set and cleared independently and NEVER gate a transition on their
// own.
type Flag string

const (
	// FlagOrphaned marks a task whose child session died. The session dying and
	// the work being worthless are unrelated events (§6.3): the worktree is kept
	// untouched and the run renders the task as recoverable.
	FlagOrphaned Flag = "orphaned"
	// FlagDiverged marks a task that touched files outside its declared paths
	// (§12.3.1). Non-blocking; it is what the merge gate demands a second
	// acknowledgement for.
	FlagDiverged Flag = "diverged"
	// FlagEnvSuspect marks a check failure whose output matched one of §6.4's
	// environment shapes (port in use, DB locked). It is a triage label on a
	// FAILURE and never turns one into a pass — see Result.EnvSuspect.
	FlagEnvSuspect Flag = "env-suspect"
)

// Flags is the decoded flag set. §13.2's other flags (stalled, forced,
// stale-contract, block-malformed) belong to §§10-12 and have no writer in 3a;
// EncodeFlags must round-trip an unknown flag it was given rather than drop it,
// so a row written by a future Loom is not silently downgraded by this one.
type Flags map[Flag]bool

// DecodeFlags parses the stored JSON array. An unparseable value degrades to an
// empty set rather than an error: a corrupt flag column must cost a badge, never
// a run — the same "degrade, never block" rule session.DecodeAddDirs follows.
func DecodeFlags(s string) Flags {
	f := Flags{}
	if strings.TrimSpace(s) == "" {
		return f
	}
	var raw []string
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		// Deliberately not an error return and deliberately not a log line that
		// nobody reads: the caller is a renderer, and the worst outcome
		// available here is a task that will not render because its flag column
		// has a stray byte in it.
		return f
	}
	for _, v := range raw {
		if v == "" {
			continue
		}
		// UNKNOWN FLAGS ARE KEPT. A row written by a future Loom that knows
		// about `stale-contract` must survive a read/modify/write by this one;
		// filtering to the constants above is exactly how a two-instance setup
		// silently downgrades its own data.
		f[Flag(v)] = true
	}
	return f
}

// EncodeFlags renders the set as a sorted JSON array, and the EMPTY STRING —
// not "[]" — for the empty set, matching the migration default so an untouched
// row and a cleared row are byte-identical.
func EncodeFlags(f Flags) string {
	keys := make([]string, 0, len(f))
	for k, on := range f {
		// A false value is an absence, not a stored "off": Without deletes, and
		// a map literal in a caller's test may well spell a cleared flag this
		// way. Encoding `"orphaned": false` as a present flag would invert it.
		if on && k != "" {
			keys = append(keys, string(k))
		}
	}
	if len(keys) == 0 {
		// The EMPTY STRING, not "[]" — the migration's column default. An
		// untouched row and a cleared row must be byte-identical, or every
		// "has anything changed" comparison in the system gets a false
		// positive the first time a flag is set and cleared again.
		return ""
	}
	// Sorted so the encoding is a function of the SET and nothing else. Go's
	// map iteration order is randomized per run, so an unsorted encoding would
	// make two Looms rewrite the same column back and forth forever.
	sort.Strings(keys)
	b, err := json.Marshal(keys)
	if err != nil {
		return ""
	}
	return string(b)
}

// With returns a copy with flag set, and Without a copy with it cleared. Copies
// rather than mutation because a Flags value read off a task row is shared with
// whatever rendered it.
func (f Flags) With(flag Flag) Flags {
	out := f.clone()
	if flag != "" {
		out[flag] = true
	}
	return out
}

func (f Flags) Without(flag Flag) Flags {
	out := f.clone()
	// Deleted, not set to false: Flags is a SET, and EncodeFlags reading a
	// false entry as an absence is the only other consistent choice. Two
	// spellings of "cleared" is one too many.
	delete(out, flag)
	return out
}

// clone always returns a non-nil map, so With on a zero Flags (the value a
// caller gets from a struct it never populated) does not panic on assignment.
func (f Flags) clone() Flags {
	out := make(Flags, len(f)+1)
	for k, v := range f {
		out[k] = v
	}
	return out
}

// Sentinel errors. Every one of these is a condition a caller must be able to
// distinguish and RENDER — this package never degrades a refusal into a silent
// no-op, because an invisible failure in a system that spawns real agents is how
// a night's quota disappears with nothing on screen.
var (
	// ErrProjectNotFound and ErrProjectAmbiguous are §4.4 rule 2: the manifest's
	// `project` must resolve to EXACTLY one project root. Two errors and not one
	// because the remedies are opposite — one is a typo, the other is two
	// projects sharing a display name.
	ErrProjectNotFound  = errors.New("delegate: no such project")
	ErrProjectAmbiguous = errors.New("delegate: project name matches more than one project")

	// ErrEscapesRepo is §4.4 rule 7: an artifact path or a check cwd that
	// resolves outside its repo after filepath.Clean. Checked at load AND again
	// at execution against the materialized worktree, because a load-time pass
	// is a statement about a file that may since have been amended.
	ErrEscapesRepo = errors.New("delegate: path escapes its repo")

	// ErrWorktreeOccupied is §6.2 step 3's hard precondition: a live sessions
	// row already has cwd == physicalDir(<worktree>). Two claudes in one
	// worktree on one branch is the worst outcome available in this design, so
	// it is made structurally impossible at the launch site rather than argued
	// about in recovery.
	ErrWorktreeOccupied = errors.New("delegate: a live session already occupies this worktree")

	// ErrCapReached is §6.6. It is an error at the SPAWN site only; the
	// scheduler keeps proposing ready tasks and the gate keeps rendering them,
	// greyed with "cap reached (n/n)".
	ErrCapReached = errors.New("delegate: concurrency cap reached")

	// ErrTaskNotApproved is §13.3 step 1's refusal: the approved→spawning CAS
	// was rejected, so this task is not in `approved` any more. Either a spawn
	// already claimed it — the double-spawn case, and the reason the claim
	// precedes every side effect — or it was abandoned while the human was
	// looking at the gate.
	ErrTaskNotApproved = errors.New("delegate: task is not approved (already claimed, or abandoned)")
)
