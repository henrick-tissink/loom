package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

// The two-project shape every §6 test in this package keys on: one project the
// user keeps on screen, one they hid before a screen-share, each with a member
// repo under its root.
const (
	visRoot = "/ws/visible"
	hidRoot = "/ws/hidden"
	visRepo = visRoot + "/api"
	hidRepo = hidRoot + "/api"
)

// testResolver builds the §4 authority over that shape. Passing a root as
// `hidden` is how each leak-surface test flips the one bit under test.
func testResolver(hidden ...string) *projects.Resolver {
	h := map[string]bool{}
	for _, r := range hidden {
		h[r] = true
	}
	ps := []store.Project{
		{Root: store.UngroupedRoot, Name: "Ungrouped", Origin: "reserved"},
		{Root: visRoot, Name: "Visible", Origin: "discovered", Hidden: h[visRoot]},
		{Root: hidRoot, Name: "Client", Origin: "discovered", Hidden: h[hidRoot]},
	}
	repos := []store.ProjectRepo{
		{Path: visRepo, ProjectRoot: visRoot, Label: "visible/api"},
		{Path: hidRepo, ProjectRoot: hidRoot, Label: "hidden/api"},
	}
	return projects.NewResolver(ps, repos)
}

// newTestApp wires an App over a real (temp) loom.db and a real project
// service. The DB is the point: these tests exercise the read-through path
// that replaced the startup snapshot, and a fake store would not catch a
// service that had gone back to caching.
func newTestApp(t *testing.T) *App {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return newApp(nil, tmux.New(), st, nil, projects.New(st),
		func() time.Time { return time.Unix(1000, 0) })
}

// seedProjects writes the visible/hidden pair into a real store.
func seedProjects(t *testing.T, a *App) {
	t.Helper()
	for _, p := range []store.Project{
		{Root: visRoot, Name: "Visible", Origin: "discovered", CreatedAt: 1, UpdatedAt: 1},
		{Root: hidRoot, Name: "Client", Origin: "discovered", CreatedAt: 1, UpdatedAt: 1},
	} {
		if err := a.st.UpsertProject(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []store.ProjectRepo{
		{Path: visRepo, ProjectRoot: visRoot, Label: "visible/api", AddedAt: 1},
		{Path: hidRepo, ProjectRoot: hidRoot, Label: "hidden/api", AddedAt: 1},
	} {
		if err := a.st.UpsertProjectRepo(m); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCreatedProjectIsLaunchableWithoutRestart is the §7 regression: the
// launcher used to validate against a by-value registry.Discover snapshot
// taken at startup, so a project created in-app was listed but not launchable
// until the next launch. Nothing here restarts anything.
func TestCreatedProjectIsLaunchableWithoutRestart(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()

	if got := a.ListProjects(); len(got) != 0 {
		t.Fatalf("fresh store should offer no targets, got %+v", got)
	}
	if err := a.CreateProject(root, "Atlas", []string{root}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	got := a.ListProjects()
	if len(got) != 1 || got[0].Path != root {
		t.Fatalf("created project not offered as a target: %+v", got)
	}
	if got[0].ProjectName != "Atlas" {
		t.Errorf("target ProjectName = %q, want Atlas", got[0].ProjectName)
	}
	// The label rule (§2): a repo that IS its project's root keeps the bare
	// basename, never "root/root".
	if want := filepath.Base(root); got[0].Label != want {
		t.Errorf("label = %q, want %q", got[0].Label, want)
	}
	if _, err := buildRecipe(a.launchableTargets(), root, "", "", "", nil); err != nil {
		t.Fatalf("created project must resolve as a launch target: %v", err)
	}
}

func TestCreateProject_rejects(t *testing.T) {
	a := newTestApp(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		root, pname string
	}{
		{"nonexistent root", filepath.Join(dir, "nope"), "Ghost"},
		{"root is a file", file, "File"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := a.CreateProject(tt.root, tt.pname, nil); err == nil {
				t.Fatal("want error, got nil (§7: the root must already exist)")
			}
		})
	}

	if err := a.CreateProject(dir, "Atlas", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.CreateProject(dir, "Atlas2", nil); err == nil {
		t.Error("duplicate root must be rejected")
	}
	other := t.TempDir()
	if err := a.CreateProject(other, "Atlas", nil); err == nil {
		t.Error("duplicate name must be rejected (labels would be ambiguous)")
	}
}

// TestRemoveProjectRepo_isReassignment pins §7's definition: removing a repo
// leaves a row behind, as a single-repo project at its own path. Deleting it
// would let the next discovery pass re-absorb it into the project the user
// just took it out of.
func TestRemoveProjectRepo_isReassignment(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)

	if err := a.RemoveProjectRepo(hidRepo); err != nil {
		t.Fatalf("RemoveProjectRepo: %v", err)
	}
	m, ok, err := a.st.GetProjectRepo(hidRepo)
	if err != nil || !ok {
		t.Fatalf("membership row must persist: ok=%v err=%v", ok, err)
	}
	if m.ProjectRoot != hidRepo {
		t.Errorf("project_root = %q, want the repo's own path", m.ProjectRoot)
	}
	if m.Label != "api" {
		t.Errorf("label = %q, want the bare basename for a project of one", m.Label)
	}
	if _, ok, _ := a.st.GetProject(hidRepo); !ok {
		t.Error("a single-repo project must exist at the repo's own path")
	}
}

// TestMemberThatIsAnotherProjectRoot_rejectedAtWrite is §4's "path present as
// both root and repo (rejected at write)" case, over BOTH gestures that write
// a membership row. Real directories on disk, so the rejection has to come
// from the root check and not incidentally from mustBeDir.
//
// The create-project half is the regression: "+ New project" fed its checklist
// straight into addRepo, which did no root check at all, so the path landed in
// §4's target set as both a root and a repo and the root-wins rule dropped the
// membership silently.
func TestMemberThatIsAnotherProjectRoot_rejectedAtWrite(t *testing.T) {
	mk := func(t *testing.T) (*App, string, string) {
		t.Helper()
		a := newTestApp(t)
		owner, other := t.TempDir(), t.TempDir()
		// `other` is a ROOT and not a member of anything, so the path PK does
		// not incidentally block the bad insert: only the root check does.
		if err := a.CreateProject(other, "Other", nil); err != nil {
			t.Fatal(err)
		}
		return a, owner, other
	}

	t.Run("add-repo", func(t *testing.T) {
		a, owner, other := mk(t)
		if err := a.CreateProject(owner, "Owner", nil); err != nil {
			t.Fatal(err)
		}
		if err := a.AddProjectRepo(owner, other); err == nil {
			t.Fatal("adding another project's root as a repo must be rejected")
		}
		if m, ok, _ := a.st.GetProjectRepo(other); ok && m.ProjectRoot == owner {
			t.Fatalf("membership written anyway: %+v", m)
		}
	})

	t.Run("create-project member list", func(t *testing.T) {
		a, owner, other := mk(t)
		if err := a.CreateProject(owner, "Owner", []string{owner, other}); err == nil {
			t.Fatal("a member that is another project's root must be rejected")
		}
		if m, ok, _ := a.st.GetProjectRepo(other); ok && m.ProjectRoot == owner {
			t.Fatalf("membership written anyway: %+v", m)
		}
	})

	t.Run("create-project member is not a directory", func(t *testing.T) {
		a, owner, _ := mk(t)
		file := filepath.Join(owner, "f")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// §12: an unusable member path reaches `tmux new-session -c`, which
		// exits 0 and starts the agent in $HOME.
		if err := a.CreateProject(owner, "Owner", []string{file}); err == nil {
			t.Fatal("a member that is not a directory must be rejected")
		}
		if _, ok, _ := a.st.GetProjectRepo(file); ok {
			t.Fatal("non-directory member written anyway")
		}
	})
}

func TestSetProjectHiddenAndSolo_persist(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)

	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	if err := a.SetProjectSolo(visRoot, true); err != nil {
		t.Fatal(err)
	}
	// hidden is untouched by solo, so leaving solo restores the prior state
	// exactly (§6.1).
	if err := a.SetProjectSolo(visRoot, false); err != nil {
		t.Fatal(err)
	}
	p, ok, err := a.st.GetProject(hidRoot)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if !p.Hidden || p.Solo {
		t.Fatalf("after a solo round-trip: %+v", p)
	}
}

// TestSetProjectCollapsed_persistsOnTheDTO is §8's "collapse state lives in
// loom.db alongside the other project flags". The frontend prefers the server
// value over its localStorage mirror the moment the DTO carries the field, so
// the round-trip through ListProjectDetails is the contract, not the store call.
func TestSetProjectCollapsed_persistsOnTheDTO(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)

	for _, p := range a.ListProjectDetails() {
		if p.Collapsed {
			t.Fatalf("%s starts collapsed: %+v", p.Root, p)
		}
	}
	if err := a.SetProjectCollapsed(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range a.ListProjectDetails() {
		if want := p.Root == hidRoot; p.Collapsed != want {
			t.Fatalf("%s collapsed = %v, want %v", p.Root, p.Collapsed, want)
		}
	}
	// Collapse is a view gesture and must not disturb the flags §6 keys on.
	if p, _, _ := a.st.GetProject(hidRoot); p.Hidden || p.Solo {
		t.Fatalf("collapse touched the visibility flags: %+v", p)
	}
	if err := a.SetProjectCollapsed(hidRoot, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range a.ListProjectDetails() {
		if p.Collapsed {
			t.Fatalf("%s still collapsed: %+v", p.Root, p)
		}
	}
}

// TestSetProjectCollapsed_noStore pins the house rule that a degraded App
// reports rather than panics — every other bound writer here does the same.
func TestSetProjectCollapsed_noStore(t *testing.T) {
	a := &App{}
	if err := a.SetProjectCollapsed(visRoot, true); err == nil {
		t.Fatal("want an error with no store")
	}
}

// TestSuggestRepos backs the create-project checklist prefill (§8/§3 rule 2).
// It returns a value, never an error: an unreadable or repo-less root must
// still leave the modal usable, offering the root itself.
func TestSuggestRepos(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	for _, d := range []string{"api/.git", "web/.git", "notes"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got := a.SuggestRepos(root)
	want := []string{filepath.Join(root, "api"), filepath.Join(root, "web")}
	if len(got) != len(want) {
		t.Fatalf("SuggestRepos = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SuggestRepos = %v, want %v", got, want)
		}
	}

	// A trailing slash must not survive into the checklist: these paths become
	// project_repos PKs, and an uncanonical one mints a second row for the same
	// directory (§4).
	if slashed := a.SuggestRepos(root + string(filepath.Separator)); len(slashed) != len(want) || slashed[0] != want[0] {
		t.Fatalf("SuggestRepos(trailing slash) = %v, want %v", slashed, want)
	}

	for _, bad := range []string{"", filepath.Join(root, "nope"), filepath.Join(root, "notes")} {
		if got := a.SuggestRepos(bad); got == nil || len(got) != 0 {
			t.Fatalf("SuggestRepos(%q) = %v, want a non-nil empty slice", bad, got)
		}
	}
}

// TestOpenDirectoryDialog_noWindow: the picker needs the Wails app context, so
// the headless case must report rather than panic on a nil ctx — this is the
// binding whose absence the frontend degrades around, and a crash here would
// take the window down instead.
func TestOpenDirectoryDialog_noWindow(t *testing.T) {
	a := newTestApp(t)
	if _, err := a.OpenDirectoryDialog("Choose"); err == nil {
		t.Fatal("want an error with no window context")
	}
}

func TestListProjectDetails_includesHiddenAndUngrouped(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)
	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	got := a.ListProjectDetails()
	// This is the surface the toggles live on: a project that vanished from it
	// the moment it was hidden could never be unhidden.
	var sawHidden, sawUngrouped bool
	for _, p := range got {
		if p.Root == hidRoot && p.Hidden {
			sawHidden = true
			if len(p.Repos) != 1 {
				t.Errorf("hidden project repos = %+v", p.Repos)
			}
		}
		if p.Ungrouped {
			sawUngrouped = true
		}
	}
	if !sawHidden {
		t.Error("hidden project missing from the overview")
	}
	if !sawUngrouped {
		t.Error("reserved Ungrouped row missing from the overview")
	}
}

// TestResolverKeepsLastGoodOnReadError: a transient DB read failure must not
// un-hide a project. internal/ui already behaved this way; the GUI returned
// nil, i.e. "nothing is hidden", so the same blip hid a client in one frontend
// and revealed it in the other — against one shared loom.db, with both
// instances declared live by ARCHITECTURE.md.
func TestResolverKeepsLastGoodOnReadError(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)
	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	if visible(a.resolver(), hidRepo) {
		t.Fatal("hidden project visible before the failure was even simulated")
	}

	if err := a.st.Close(); err != nil { // every subsequent read now errors
		t.Fatal(err)
	}
	r := a.resolver()
	if r == nil {
		t.Fatal("read error dropped the authority entirely")
	}
	if visible(r, hidRepo) {
		t.Error("a transient read error un-hid the project (§6: the worse failure)")
	}
}
