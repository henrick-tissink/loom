package delegate

import (
	"errors"
	"testing"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/store"
)

// fakeRuns is the narrow Runs slice, so these tests need no DB. The error case
// matters as much as the lookup: §14.1 requires every failure mode to fall
// through to the prefix scan rather than to a guess.
type fakeRuns struct {
	roots map[int64]string
	err   error
	calls int
}

func (f *fakeRuns) DelegationRunProjectRoot(id int64) (string, bool, error) {
	f.calls++
	if f.err != nil {
		return "", false, f.err
	}
	root, ok := f.roots[id]
	return root, ok, nil
}

const (
	runRoot   = "/w/runproj"
	otherRoot = "/w/otherproj"
	// A worktree under ~/.loom, deliberately outside any project's target set —
	// this is the whole reason the override exists.
	worktree = "/home/u/.loom/worktrees/rearch-1/api/account-schema"
)

// attributor builds the wrapper over a resolver whose two projects carry the
// given hidden/solo flags.
func attributor(t *testing.T, runHidden, runSolo, otherHidden, otherSolo bool) *Attributor {
	t.Helper()
	ps := []store.Project{
		{Root: store.UngroupedRoot, Name: "Ungrouped"},
		{Root: runRoot, Name: "RunProj", Hidden: runHidden, Solo: runSolo},
		{Root: otherRoot, Name: "OtherProj", Hidden: otherHidden, Solo: otherSolo},
	}
	repos := []store.ProjectRepo{
		{ProjectRoot: runRoot, Path: runRoot, Label: "runproj"},
		{ProjectRoot: otherRoot, Path: otherRoot, Label: "otherproj"},
	}
	return &Attributor{
		Resolver: projects.NewResolver(ps, repos),
		Runs:     &fakeRuns{roots: map[int64]string{7: runRoot}},
	}
}

func child() store.SessionRow {
	return store.SessionRow{
		Name: "loom-child", Cwd: worktree, Delegation: FormatDelegation(7, "account-schema"),
	}
}

// §17, the three cases revision 1's plan omitted and which it would have
// PASSED while broken. Each is a distinct hiding configuration, and the
// middle one is the one that actually blanked the run in practice.
func TestChildAttributionUnderHiding(t *testing.T) {
	tests := []struct {
		name        string
		runHidden   bool
		runSolo     bool
		otherHidden bool
		otherSolo   bool
		wantVisible bool
		why         string
	}{
		{
			name: "solo the run's own project", runSolo: true, wantVisible: true,
			why: "the one situation where you most want to watch a run must not blank it",
		},
		{
			name: "solo a different project", otherSolo: true, wantVisible: false,
			why: "solo means only that project's work is on screen",
		},
		{
			name: "nothing hidden", wantVisible: true,
			why: "no filtering at all",
		},
		{
			name: "the run's own project hidden", runHidden: true, wantVisible: false,
			why: "hiding a project hides its run's children with it",
		},
		{
			name: "a different project hidden", otherHidden: true, wantVisible: true,
			why: "filtering is on, but not for this project — the fail-closed trap",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := attributor(t, tc.runHidden, tc.runSolo, tc.otherHidden, tc.otherSolo)
			if got := a.Visible(child()); got != tc.wantVisible {
				t.Fatalf("Visible = %v, want %v — %s", got, tc.wantVisible, tc.why)
			}
		})
	}
}

// §17: nothing hidden ⇒ children group under project_root, NOT Ungrouped. The
// rail sections on projectRoot, so an Ungrouped answer scatters a run's
// children out of their own project even with no filtering on at all.
func TestChildAttributesToItsRunsProjectNotUngrouped(t *testing.T) {
	a := attributor(t, false, false, false, false)

	att, ok := a.Attribute(child())
	if !ok {
		t.Fatal("delegation child was not attributable")
	}
	if att.Root != runRoot {
		t.Fatalf("Attribute().Root = %q, want %q (Ungrouped is %q)", att.Root, runRoot, store.UngroupedRoot)
	}
	if att.Name != "RunProj" {
		t.Fatalf("Attribute().Name = %q, want the run's project name", att.Name)
	}
}

// §17: a child whose delegation names a DELETED run falls through to the
// prefix scan and thus to fail-closed. A deleted run is exactly the case where
// the conservative answer is right.
func TestDeletedRunFallsThroughToFailClosed(t *testing.T) {
	a := attributor(t, false, false, true, false) // something hidden ⇒ Filtering()
	row := child()
	row.Delegation = FormatDelegation(999, "account-schema") // no such run

	if a.Visible(row) {
		t.Fatal("a child naming a deleted run must fail closed while filtering is on")
	}
	if _, ok := a.Attribute(row); ok {
		t.Fatal("a child naming a deleted run must not be attributable")
	}
}

// A store error must fall through to fail-closed too, never promote the child
// into visibility. A transient DB failure that un-hid a client's session is the
// exact leak the containment rule exists to prevent.
func TestRunsLookupErrorFailsClosed(t *testing.T) {
	a := attributor(t, false, false, true, false) // something hidden ⇒ Filtering()
	a.Runs = &fakeRuns{err: errors.New("db is locked")}

	if a.Visible(child()) {
		t.Fatal("a store error must fail closed, not promote the child into visibility")
	}
}

// A non-delegation row must be untouched by the wrapper: same answer the bare
// resolver gives, over cwd ∪ add-dirs. The wrapper is an override for one row
// shape, not a new attribution policy.
func TestNonDelegationRowIsUnchanged(t *testing.T) {
	a := attributor(t, false, false, true, false)

	plain := store.SessionRow{Name: "loom-plain", Cwd: runRoot}
	if !a.Visible(plain) {
		t.Fatal("a plain row in a visible project must stay visible")
	}
	att, ok := a.Attribute(plain)
	if !ok || att.Root != runRoot {
		t.Fatalf("plain row attributed to %+v (ok=%v), want %q", att, ok, runRoot)
	}

	// And a plain row in the HIDDEN project is hidden, i.e. the wrapper did not
	// accidentally short-circuit the normal path.
	hidden := store.SessionRow{Name: "loom-hidden", Cwd: otherRoot}
	if a.Visible(hidden) {
		t.Fatal("a plain row in a hidden project must be hidden")
	}
}

// A delegation child's add-dirs are LOOM'S OWN directories, which no project
// claims. ANDing them into the visibility answer would blank every child even
// under the override — the exact bug the override exists to fix, reintroduced
// one layer down. This is the regression test for that specific mistake.
func TestChildAddDirsDoNotSuppressIt(t *testing.T) {
	a := attributor(t, false, false, true, false) // something hidden ⇒ Filtering()
	row := child()
	row.AddDirs = `["/home/u/.loom/worktrees/rearch-1/api/account-schema.meta"]`

	if !a.Visible(row) {
		t.Fatal("a child's own .meta add-dir must not suppress it (add-dirs must not be ANDed in)")
	}
}

// The override must be consulted BEFORE the prefix scan, and the lookup must
// actually happen — a wrapper that fell through first and only consulted the
// run on failure would give the right answer here by luck and the wrong one
// whenever a worktree happened to sit under a project's path.
func TestOverrideIsConsultedBeforePrefixScan(t *testing.T) {
	a := attributor(t, false, false, false, false)
	runs := a.Runs.(*fakeRuns)

	// A child whose cwd sits INSIDE the other project's tree. Geometry says
	// otherRoot; identity says runRoot. Identity must win (§14.1).
	row := child()
	row.Cwd = otherRoot + "/some/nested/dir"

	att, ok := a.Attribute(row)
	if !ok || att.Root != runRoot {
		t.Fatalf("Attribute().Root = %q (ok=%v), want %q — identity must beat geometry",
			att.Root, ok, runRoot)
	}
	if runs.calls == 0 {
		t.Fatal("the run lookup was never consulted")
	}
}

func TestFormatParseDelegationRoundTrip(t *testing.T) {
	id, task, ok := ParseDelegation(FormatDelegation(42, "account-schema"))
	if !ok || id != 42 || task != "account-schema" {
		t.Fatalf("round trip = (%d, %q, %v), want (42, account-schema, true)", id, task, ok)
	}
}

// A corrupt column degrades to "not a delegation child" and therefore to
// fail-closed. A child wrongly hidden is a support question; a hidden client
// wrongly shown is the failure this whole feature exists to prevent.
func TestParseDelegationRejectsCorruptValues(t *testing.T) {
	for _, bad := range []string{
		"", ":", "7:", ":task", "notanumber:task", "7", "-1:task", "0:task",
		"  :task", "7:task:extra-is-fine-but-id-must-parse",
	} {
		t.Run(bad, func(t *testing.T) {
			id, task, ok := ParseDelegation(bad)
			if bad == "7:task:extra-is-fine-but-id-must-parse" {
				// Cut on the FIRST colon: the id parses, the remainder is the
				// task. A task id cannot contain ':' by §4.4, so this is
				// already corrupt — but it degrades predictably rather than
				// mis-splitting into a different run.
				if !ok || id != 7 {
					t.Fatalf("got (%d, %q, %v), want id 7", id, task, ok)
				}
				return
			}
			if ok {
				t.Fatalf("ParseDelegation(%q) accepted a corrupt value as (%d, %q)", bad, id, task)
			}
		})
	}
}
