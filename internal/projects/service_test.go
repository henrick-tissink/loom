package projects

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/store"
)

// The real store must satisfy the narrowed interface. Asserted in the test
// binary rather than in service.go so the production package keeps no
// dependency on the concrete type — but asserted somewhere, or a signature
// drift in the store surfaces as a frontend build break two waves later.
var _ Store = (*store.Store)(nil)

// fakeStore reproduces the two rules the reconciler leans on — insert-only
// upserts and the ErrLabelTaken sentinel — without a DB, so a failure here is
// unambiguously the service's and not a migration's. The real store's own
// behaviour is pinned by internal/store's tests.
type fakeStore struct {
	projects map[string]store.Project
	repos    map[string]store.ProjectRepo
	labels   map[string]string // label -> path
	listErr  error
	upsertPE error // forced failure from UpsertProject
}

func newFake() *fakeStore {
	return &fakeStore{
		projects: map[string]store.Project{store.UngroupedRoot: {Root: store.UngroupedRoot, Name: "Ungrouped", Origin: "reserved"}},
		repos:    map[string]store.ProjectRepo{},
		labels:   map[string]string{},
	}
}

func (f *fakeStore) ListProjects() ([]store.Project, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]store.Project, 0, len(f.projects))
	for _, p := range f.projects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Root < out[j].Root })
	return out, nil
}

func (f *fakeStore) ListAllProjectRepos() ([]store.ProjectRepo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]store.ProjectRepo, 0, len(f.repos))
	for _, r := range f.repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (f *fakeStore) UpsertProject(p store.Project) error {
	if f.upsertPE != nil {
		return f.upsertPE
	}
	if _, ok := f.projects[p.Root]; ok {
		return nil // insert-only
	}
	f.projects[p.Root] = p
	return nil
}

func (f *fakeStore) UpsertProjectRepo(r store.ProjectRepo) error {
	if _, ok := f.repos[r.Path]; ok {
		return nil // insert-only
	}
	if owner, ok := f.labels[r.Label]; ok && owner != r.Path {
		return store.ErrLabelTaken
	}
	f.repos[r.Path] = r
	f.labels[r.Label] = r.Path
	return nil
}

func (f *fakeStore) SetProjectMissing(root string, missing bool, now int64) error {
	p, ok := f.projects[root]
	if !ok {
		return store.ErrNoProject
	}
	p.Missing, p.UpdatedAt = missing, now
	f.projects[root] = p
	return nil
}

func (f *fakeStore) SetProjectRepoMissing(path string, missing bool) error {
	r, ok := f.repos[path]
	if !ok {
		return errors.New("no such repo")
	}
	r.Missing = missing
	f.repos[path] = r
	return nil
}

// mkdirs builds a real workspace: the sweep stats the filesystem, so faking it
// would test nothing.
func mkdirs(t *testing.T, base string, rel ...string) {
	t.Helper()
	for _, r := range rel {
		if err := os.MkdirAll(filepath.Join(base, r), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReconcileInsertsWithoutClobbering(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "group/one", "group/two")
	root := filepath.Join(base, "group")

	f := newFake()
	svc := New(f)
	disc := []registry.Project{{Name: "group", Root: root, Repos: []registry.Repo{
		{Label: "group/one", Path: filepath.Join(root, "one")},
		{Label: "group/two", Path: filepath.Join(root, "two")},
	}}}
	if err := svc.Reconcile(disc); err != nil {
		t.Fatal(err)
	}
	if got := f.projects[root].Origin; got != "discovered" {
		t.Fatalf("origin = %q, want discovered", got)
	}
	if len(f.repos) != 2 {
		t.Fatalf("repos = %d, want 2", len(f.repos))
	}

	// The user renames and hides; the next discovery pass must not undo it.
	p := f.projects[root]
	p.Name, p.Hidden = "Renamed", true
	f.projects[root] = p

	if err := svc.Reconcile(disc); err != nil {
		t.Fatal(err)
	}
	if got := f.projects[root]; got.Name != "Renamed" || !got.Hidden {
		t.Fatalf("discovery clobbered user state: %+v", got)
	}
	if w := svc.Warnings(); len(w) != 0 {
		t.Fatalf("unexpected warnings: %v", w)
	}
}

// Discovery is never fatal: a label collision skips the insert, records a
// warning, and the rest of the pass still lands.
func TestReconcileLabelCollisionSkipsAndWarns(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "g1/dup", "g2/dup", "g2/other")
	g1, g2 := filepath.Join(base, "g1"), filepath.Join(base, "g2")

	f := newFake()
	svc := New(f)
	err := svc.Reconcile([]registry.Project{
		{Name: "g1", Root: g1, Repos: []registry.Repo{{Label: "shared", Path: filepath.Join(g1, "dup")}}},
		{Name: "g2", Root: g2, Repos: []registry.Repo{
			{Label: "shared", Path: filepath.Join(g2, "dup")},
			{Label: "g2/other", Path: filepath.Join(g2, "other")},
		}},
	})
	if err != nil {
		t.Fatalf("a label collision must not be fatal: %v", err)
	}
	if _, ok := f.repos[filepath.Join(g2, "dup")]; ok {
		t.Error("the colliding insert should have been skipped")
	}
	if _, ok := f.repos[filepath.Join(g2, "other")]; !ok {
		t.Error("the rest of the pass must still land")
	}
	w := svc.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "shared") {
		t.Fatalf("warnings = %v, want one naming the label", w)
	}

	// Warnings belong to the LAST pass; a clean pass must clear a stale one so
	// the overview does not accuse a project that is now fine.
	if err := svc.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	if w := svc.Warnings(); len(w) != 0 {
		t.Fatalf("warnings not reset: %v", w)
	}
}

// A project-level write failure is warned about and skipped, not returned:
// one bad row must not cost the user every other project on this launch.
func TestReconcileProjectErrorIsNonFatal(t *testing.T) {
	f := newFake()
	f.upsertPE = errors.New("disk on fire")
	svc := New(f)
	if err := svc.Reconcile([]registry.Project{{Name: "g", Root: "/w/g"}}); err != nil {
		t.Fatalf("discovery must never be fatal: %v", err)
	}
	if w := svc.Warnings(); len(w) != 1 {
		t.Fatalf("warnings = %v, want one", w)
	}
}

// Paths are canonicalized on WRITE, so a trailing slash cannot mint a second
// primary-key row for the same directory.
func TestReconcileCanonicalizesOnWrite(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "group/one")
	root := filepath.Join(base, "group")

	f := newFake()
	svc := New(f)
	if err := svc.Reconcile([]registry.Project{{Name: "group", Root: root + "/", Repos: []registry.Repo{
		{Label: "group/one", Path: filepath.Join(root, "one") + "/"},
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.projects[root]; !ok {
		t.Fatalf("project root not canonicalized: %v", f.projects)
	}
	if _, ok := f.repos[filepath.Join(root, "one")]; !ok {
		t.Fatalf("repo path not canonicalized: %v", f.repos)
	}
}

// The sweep stats EVERY known row rather than diffing against the scan set —
// an out-of-root member is absent from every scan by construction, and diffing
// would flag it missing on every pass.
func TestSweepStatsEveryKnownRow(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, base, "group/present", "outside/member", "gone/repo")
	root := filepath.Join(base, "group")
	outOfRoot := filepath.Join(base, "outside", "member")
	goneRoot := filepath.Join(base, "gone")

	f := newFake()
	f.projects[root] = store.Project{Root: root, Name: "group", Origin: "discovered"}
	f.projects[goneRoot] = store.Project{Root: goneRoot, Name: "gone", Origin: "discovered"}
	f.repos[filepath.Join(root, "present")] = store.ProjectRepo{Path: filepath.Join(root, "present"), ProjectRoot: root, Label: "group/present"}
	f.repos[outOfRoot] = store.ProjectRepo{Path: outOfRoot, ProjectRoot: root, Label: "group/member"}
	f.repos[filepath.Join(goneRoot, "repo")] = store.ProjectRepo{Path: filepath.Join(goneRoot, "repo"), ProjectRoot: goneRoot, Label: "gone/repo"}

	svc := New(f)

	// Nothing has vanished yet: no row is flagged, least of all the member
	// that no workspace scan will ever list.
	if err := svc.Sweep(); err != nil {
		t.Fatal(err)
	}
	if f.repos[outOfRoot].Missing {
		t.Fatal("an out-of-root member must not be flagged missing")
	}

	// The `gone` project disappears from disk. Only its rows flip — including
	// the repo under it, which no scan of the workspace would enumerate.
	if err := os.RemoveAll(goneRoot); err != nil {
		t.Fatal(err)
	}
	if err := svc.Sweep(); err != nil {
		t.Fatal(err)
	}
	if !f.projects[goneRoot].Missing {
		t.Error("vanished project not flagged missing")
	}
	if !f.repos[filepath.Join(goneRoot, "repo")].Missing {
		t.Error("vanished repo not flagged missing")
	}
	if f.projects[root].Missing || f.repos[outOfRoot].Missing {
		t.Error("a surviving row was flagged missing")
	}

	// It comes back: the flag self-clears, no user gesture required.
	mkdirs(t, base, "gone/repo")
	if err := svc.Sweep(); err != nil {
		t.Fatal(err)
	}
	if f.projects[goneRoot].Missing || f.repos[filepath.Join(goneRoot, "repo")].Missing {
		t.Error("missing did not self-clear")
	}
}

// The reserved Ungrouped row owns no directory; stat-ing "" would flag it
// missing on every pass and dim a bucket that always exists.
func TestSweepSkipsUngrouped(t *testing.T) {
	f := newFake()
	if err := New(f).Sweep(); err != nil {
		t.Fatal(err)
	}
	if f.projects[store.UngroupedRoot].Missing {
		t.Fatal("the reserved row must never be flagged missing")
	}
}

func TestLaunchTargets(t *testing.T) {
	group, solo := "/w/group", "/w/loom"
	f := newFake()
	f.projects[group] = store.Project{Root: group, Name: "Group"}
	f.projects[solo] = store.Project{Root: solo, Name: "Loom"}
	f.repos[group+"/one"] = store.ProjectRepo{Path: group + "/one", ProjectRoot: group, Label: "group/one"}
	f.repos["/elsewhere/two"] = store.ProjectRepo{Path: "/elsewhere/two", ProjectRoot: group, Label: "group/two", Missing: true}
	// A single-repo project whose repo path IS its root: the root row is
	// suppressed, the repo survives and keeps the association.
	f.repos[solo] = store.ProjectRepo{Path: solo, ProjectRoot: solo, Label: "loom"}

	ts, err := New(f).LaunchTargets()
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Target{}
	for _, tt := range ts {
		if _, dup := byPath[tt.Path]; dup {
			t.Fatalf("targets are not a set: %q appears twice", tt.Path)
		}
		byPath[tt.Path] = tt
	}
	if len(byPath) != 4 {
		t.Fatalf("targets = %+v, want 4 (group root, two repos, loom)", ts)
	}
	if _, ok := byPath[store.UngroupedRoot]; ok {
		t.Error("the reserved Ungrouped row is not launchable")
	}
	// §2: the root target's Label is the root BASENAME, not projects.name. The
	// display name rides along in ProjectName.
	if got := byPath[group]; got.Kind != TargetRoot || got.Label != "group" || got.ProjectName != "Group" {
		t.Errorf("group root target = %+v, want Label=\"group\" ProjectName=\"Group\"", got)
	}
	if got := byPath[solo]; got.Kind != TargetRepo || got.ProjectRoot != solo || got.Label != "loom" {
		t.Errorf("root-as-repo target = %+v, want the repo row keeping its project association", got)
	}
	if got := byPath["/elsewhere/two"]; !got.Missing || got.ProjectRoot != group {
		t.Errorf("out-of-root target = %+v", got)
	}
	// A missing PROJECT makes its repos non-launchable too — the repo cannot
	// outlive the root it hangs off in the picker.
	p := f.projects[group]
	p.Missing = true
	f.projects[group] = p
	ts, _ = New(f).LaunchTargets()
	for _, tt := range ts {
		if tt.ProjectRoot == group && !tt.Missing {
			t.Errorf("target %+v should inherit its project's missing flag", tt)
		}
	}
}

// §2's whole reason for deriving labels from the directory: a rename must not
// invalidate a workflow definition on disk. Target.Label therefore survives a
// rename unchanged while ProjectName follows it.
func TestLaunchTargetsLabelSurvivesRename(t *testing.T) {
	root := "/w/innostream"
	f := newFake()
	f.projects[root] = store.Project{Root: root, Name: "Innostream"}

	before, err := New(f).LaunchTargets()
	if err != nil {
		t.Fatal(err)
	}
	p := f.projects[root]
	p.Name = "Innostream (2027 rebuild)"
	f.projects[root] = p
	after, err := New(f).LaunchTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("want one target either side, got %d/%d", len(before), len(after))
	}
	if before[0].Label != "innostream" || after[0].Label != before[0].Label {
		t.Errorf("label changed under rename: %q -> %q", before[0].Label, after[0].Label)
	}
	if after[0].ProjectName != "Innostream (2027 rebuild)" {
		t.Errorf("ProjectName = %q, want the renamed value", after[0].ProjectName)
	}
}

// The service is queried read-through: a project created after startup is
// resolvable and launchable immediately, which the old by-value startup
// snapshot could not do.
func TestResolverIsReadThrough(t *testing.T) {
	f := newFake()
	svc := New(f)
	r, err := svc.Resolver()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Attribute("/w/new/repo"); ok {
		t.Fatal("precondition: nothing registered yet")
	}

	f.projects["/w/new"] = store.Project{Root: "/w/new", Name: "New", Origin: "created"}
	r, err = svc.Resolver()
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := r.Attribute("/w/new/repo"); !ok || a.Name != "New" {
		t.Fatalf("a created project must resolve without a restart: %+v %v", a, ok)
	}
}

func TestListFailurePropagates(t *testing.T) {
	f := newFake()
	f.listErr = errors.New("db closed")
	svc := New(f)
	if _, err := svc.Resolver(); err == nil {
		t.Error("Resolver must surface a read failure rather than a silently empty authority")
	}
	if _, err := svc.LaunchTargets(); err == nil {
		t.Error("LaunchTargets must surface a read failure")
	}
}
