package projects

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

// proj/repo keep the table rows readable; every field the predicate reads is
// named at the call site.
func proj(root, name string) store.Project { return store.Project{Root: root, Name: name} }

func hiddenProj(root, name string) store.Project {
	p := proj(root, name)
	p.Hidden = true
	return p
}

func soloProj(root, name string) store.Project {
	p := proj(root, name)
	p.Solo = true
	return p
}

func repo(path, root, label string) store.ProjectRepo {
	return store.ProjectRepo{Path: path, ProjectRoot: root, Label: label}
}

// ungrouped is the reserved row migration v7 seeds. It must be excluded from
// the prefix scan (an empty root prefixes everything) and returned as the
// fallback attribution.
var ungrouped = store.Project{Root: store.UngroupedRoot, Name: "Ungrouped", Origin: "reserved"}

func TestAttribute(t *testing.T) {
	// The live shape §4 is written against: a group whose root is NOT a repo,
	// one member repo sharing the group's basename, and five siblings that a
	// raw string prefix would swallow.
	hp := "/w/HappyPay"
	inno := "/w/Innostream"
	projects := []store.Project{
		ungrouped,
		proj(hp, "Happy Pay"),
		proj(inno, "Innostream"),
		proj("/w/loom", "Loom"),
	}
	repos := []store.ProjectRepo{
		repo(hp+"/HappyPay", hp, "HappyPay/HappyPay"),
		repo(hp+"/HappyPayCLM", hp, "HappyPay/HappyPayCLM"),
		repo(hp+"/HappyPayCoreApi", hp, "HappyPay/HappyPayCoreApi"),
		repo(hp+"/HappyPayMembers", hp, "HappyPay/HappyPayMembers"),
		repo(hp+"/HappyPayMerchants", hp, "HappyPay/HappyPayMerchants"),
		repo(hp+"/HappyPaySavaToolset", hp, "HappyPay/HappyPaySavaToolset"),
		repo(inno+"/ballista", inno, "Innostream/ballista"),
		// Out-of-root member: added by hand, lives nowhere near its root.
		repo("/elsewhere/v-atlas", inno, "Innostream/v-atlas"),
		// The root of a single-repo project is also a repo.
		repo("/w/loom", "/w/loom", "loom"),
	}
	r := NewResolver(projects, repos)

	tests := []struct {
		name    string
		cwd     string
		want    string // expected project root
		wantOK  bool
		wantNam string
	}{
		{"sibling prefix HappyPayCoreApi is not HappyPay/HappyPay", hp + "/HappyPayCoreApi", hp, true, "Happy Pay"},
		{"sibling prefix HappyPayCLM", hp + "/HappyPayCLM", hp, true, "Happy Pay"},
		{"sibling prefix HappyPayMembers", hp + "/HappyPayMembers", hp, true, "Happy Pay"},
		{"sibling prefix HappyPayMerchants", hp + "/HappyPayMerchants", hp, true, "Happy Pay"},
		{"sibling prefix HappyPaySavaToolset", hp + "/HappyPaySavaToolset", hp, true, "Happy Pay"},
		{"repo whose basename repeats the group", hp + "/HappyPay", hp, true, "Happy Pay"},
		{"cwd equals root exactly", hp, hp, true, "Happy Pay"},
		{"cwd below a repo", hp + "/HappyPayCLM/internal/api", hp, true, "Happy Pay"},
		{"out-of-root repo resolves to its project", "/elsewhere/v-atlas", inno, true, "Innostream"},
		{"below an out-of-root repo", "/elsewhere/v-atlas/cmd", inno, true, "Innostream"},
		{"path that is both root and repo", "/w/loom", "/w/loom", true, "Loom"},
		{"empty cwd falls back to Ungrouped", "", store.UngroupedRoot, false, "Ungrouped"},
		{"unknown cwd falls back to Ungrouped", "/tmp/scratch", store.UngroupedRoot, false, "Ungrouped"},
		{"sibling of a root sharing its prefix", "/w/HappyPayOther", store.UngroupedRoot, false, "Ungrouped"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := r.Attribute(tc.cwd)
			if ok != tc.wantOK || got.Root != tc.want || got.Name != tc.wantNam {
				t.Fatalf("Attribute(%q) = %+v, %v; want root %q name %q ok %v",
					tc.cwd, got, ok, tc.want, tc.wantNam, tc.wantOK)
			}
		})
	}
}

// Nested roots: the longer one owns everything under it, and the outer one
// keeps only what the inner does not claim.
func TestAttributeNestedRoots(t *testing.T) {
	outer, inner := "/w/group", "/w/group/inner"
	r := NewResolver(
		[]store.Project{ungrouped, proj(outer, "Outer"), proj(inner, "Inner")},
		[]store.ProjectRepo{repo(outer+"/other", outer, "group/other")},
	)
	for cwd, want := range map[string]string{
		outer:            outer,
		outer + "/other": outer,
		inner:            inner,
		inner + "/deep":  inner,
		// A sibling of the inner root that merely shares its prefix.
		outer + "/inner-tools": outer,
	} {
		got, ok := r.Attribute(cwd)
		if !ok || got.Root != want {
			t.Errorf("Attribute(%q) = %q, %v; want %q", cwd, got.Root, ok, want)
		}
	}
}

// A trailing slash must not break either arm of the segment-wise match. Writes
// go through Canonical, but a row that predates it must still attribute rather
// than silently orphaning every session under it.
func TestAttributeTrailingSlashRoot(t *testing.T) {
	r := NewResolver(
		[]store.Project{ungrouped, proj("/w/group/", "Group")},
		[]store.ProjectRepo{repo("/w/group/repo/", "/w/group/", "group/repo")},
	)
	for _, cwd := range []string{"/w/group", "/w/group/repo", "/w/group/repo/pkg"} {
		if _, ok := r.Attribute(cwd); !ok {
			t.Errorf("Attribute(%q) did not match a trailing-slash root", cwd)
		}
	}
	if got := Canonical("/w/group/"); got != "/w/group" {
		t.Errorf("Canonical(%q) = %q", "/w/group/", got)
	}
}

// Symlinks are handled at the COMPARISON site: the stored cwd is never
// rewritten (transcript.ProjectDirName keys transcripts on it), so the root is
// matched in both its raw and its resolved form.
func TestAttributeSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// The resolved form is what a session's cwd looks like when the shell
	// followed the link — on macOS that also unwinds /var → /private/var, so
	// the literal `real` path above is NOT the form the comparison sees.
	realResolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	// Root stored as the symlink; sessions recorded under the real path.
	r := NewResolver([]store.Project{ungrouped, proj(link, "Linked")}, nil)
	for _, cwd := range []string{link, filepath.Join(link, "repo"), realResolved, filepath.Join(realResolved, "repo")} {
		got, ok := r.Attribute(cwd)
		if !ok || got.Name != "Linked" {
			t.Errorf("Attribute(%q) = %+v, %v; want the linked project", cwd, got, ok)
		}
	}
}

func TestVisibility(t *testing.T) {
	a, b, c := "/w/a", "/w/b", "/w/c"
	base := []store.Project{ungrouped, proj(a, "A"), proj(b, "B"), proj(c, "C")}
	repos := []store.ProjectRepo{
		repo(a+"/one", a, "a/one"),
		repo(b+"/two", b, "b/two"),
	}
	withFlags := func(mut func([]store.Project)) []store.Project {
		cp := append([]store.Project(nil), base...)
		mut(cp)
		return cp
	}

	tests := []struct {
		name     string
		projects []store.Project
		dirs     []string
		want     bool
	}{
		{"nothing hidden: everything visible", base, []string{a}, true},
		{"nothing hidden: unattributable row stays visible", base, []string{""}, true},
		{"hidden project hides its root", withFlags(func(p []store.Project) { p[1] = hiddenProj(a, "A") }), []string{a}, false},
		{"hidden project hides its repo", withFlags(func(p []store.Project) { p[1] = hiddenProj(a, "A") }), []string{a + "/one"}, false},
		{"hidden project leaves siblings visible", withFlags(func(p []store.Project) { p[1] = hiddenProj(a, "A") }), []string{b}, true},
		{"fail closed: empty cwd while something is hidden",
			withFlags(func(p []store.Project) { p[1] = hiddenProj(a, "A") }), []string{""}, false},
		{"fail closed: unknown cwd while something is hidden",
			withFlags(func(p []store.Project) { p[1] = hiddenProj(a, "A") }), []string{"/tmp/orphan"}, false},
		{"fail closed: no dirs at all",
			withFlags(func(p []store.Project) { p[1] = hiddenProj(a, "A") }), nil, false},
		{"cwd ∪ add_dirs: visible cwd plus hidden add-dir is hidden",
			withFlags(func(p []store.Project) { p[2] = hiddenProj(b, "B") }), []string{a, b + "/two"}, false},
		{"cwd ∪ add_dirs: all visible stays visible",
			withFlags(func(p []store.Project) { p[3] = hiddenProj(c, "C") }), []string{a, b + "/two"}, true},
		{"cwd ∪ add_dirs: unattributable add-dir fails closed",
			withFlags(func(p []store.Project) { p[3] = hiddenProj(c, "C") }), []string{a, "/tmp/orphan"}, false},
		{"solo: soloed project visible", withFlags(func(p []store.Project) { p[1] = soloProj(a, "A") }), []string{a}, true},
		{"solo: soloed project's repo visible", withFlags(func(p []store.Project) { p[1] = soloProj(a, "A") }), []string{a + "/one"}, true},
		{"solo: other project hidden even though not flagged hidden",
			withFlags(func(p []store.Project) { p[1] = soloProj(a, "A") }), []string{b}, false},
		{"solo suppresses Ungrouped", withFlags(func(p []store.Project) { p[1] = soloProj(a, "A") }), []string{"/tmp/orphan"}, false},
		{"solo: add-dir outside the soloed project hides the row",
			withFlags(func(p []store.Project) { p[1] = soloProj(a, "A") }), []string{a, b + "/two"}, false},
		{"solo beats hidden on the soloed project itself", withFlags(func(p []store.Project) {
			s := soloProj(a, "A")
			s.Hidden = true
			p[1] = s
		}), []string{a}, true},
		{"solo root missing degrades to nothing hidden", withFlags(func(p []store.Project) {
			s := soloProj(a, "A")
			s.Missing = true
			p[1] = s
		}), []string{b}, true},
		{"solo root missing: unattributable row is not hidden either", withFlags(func(p []store.Project) {
			s := soloProj(a, "A")
			s.Missing = true
			p[1] = s
		}), []string{""}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResolver(tc.projects, repos)
			if got := r.Visible(tc.dirs...); got != tc.want {
				t.Fatalf("Visible(%v) = %v, want %v", tc.dirs, got, tc.want)
			}
		})
	}
}

// A missing solo row must not leave hiding half-applied either: solo is
// ignored entirely, so the plain hidden rule takes over.
func TestSoloRootMissingFallsBackToHiddenRule(t *testing.T) {
	a, b := "/w/a", "/w/b"
	solo := soloProj(a, "A")
	solo.Missing = true
	r := NewResolver([]store.Project{ungrouped, solo, hiddenProj(b, "B")}, nil)

	if _, on := r.SoloRoot(); on {
		t.Fatal("a missing solo root must not count as solo")
	}
	if !r.Visible(a) {
		t.Error("the missing soloed project should still be visible")
	}
	if r.Visible(b) {
		t.Error("hidden must still apply once solo has degraded")
	}
}

// hidden is untouched by solo, so exiting solo restores the prior state
// exactly — the round trip is what makes the demo gesture safe to use.
func TestSoloHiddenRoundTrip(t *testing.T) {
	a, b := "/w/a", "/w/b"
	before := []store.Project{ungrouped, hiddenProj(a, "A"), proj(b, "B")}
	r := NewResolver(before, nil)
	if r.Visible(a) || !r.Visible(b) {
		t.Fatal("precondition: A hidden, B visible")
	}

	// Solo B: A stays hidden (for a different reason), B is the only visible one.
	during := []store.Project{ungrouped, hiddenProj(a, "A"), soloProj(b, "B")}
	rs := NewResolver(during, nil)
	if rs.Visible(a) || !rs.Visible(b) {
		t.Fatal("under solo B, only B is visible")
	}

	// Leaving solo clears only the solo flag; hidden is unchanged.
	after := NewResolver(before, nil)
	if after.Visible(a) || !after.Visible(b) {
		t.Fatal("leaving solo must restore the exact prior state")
	}
}

func TestProjectVisible(t *testing.T) {
	a, b := "/w/a", "/w/b"
	tests := []struct {
		name     string
		projects []store.Project
		root     string
		want     bool
	}{
		{"no filtering", []store.Project{ungrouped, proj(a, "A")}, a, true},
		{"no filtering: Ungrouped shown", []store.Project{ungrouped, proj(a, "A")}, store.UngroupedRoot, true},
		{"hidden project", []store.Project{ungrouped, hiddenProj(a, "A"), proj(b, "B")}, a, false},
		{"sibling of hidden", []store.Project{ungrouped, hiddenProj(a, "A"), proj(b, "B")}, b, true},
		{"Ungrouped survives plain hiding", []store.Project{ungrouped, hiddenProj(a, "A")}, store.UngroupedRoot, true},
		{"solo shows only the soloed project", []store.Project{ungrouped, soloProj(a, "A"), proj(b, "B")}, b, false},
		{"solo shows the soloed project", []store.Project{ungrouped, soloProj(a, "A"), proj(b, "B")}, a, true},
		{"solo suppresses Ungrouped", []store.Project{ungrouped, soloProj(a, "A")}, store.UngroupedRoot, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResolver(tc.projects, nil)
			if got := r.ProjectVisible(tc.root); got != tc.want {
				t.Fatalf("ProjectVisible(%q) = %v, want %v", tc.root, got, tc.want)
			}
		})
	}
}

// A membership row whose project row is absent must not invent a project: the
// row is unattributed and therefore fails closed while anything is hidden.
func TestAttributeOrphanMembership(t *testing.T) {
	r := NewResolver(
		[]store.Project{ungrouped, hiddenProj("/w/a", "A")},
		[]store.ProjectRepo{repo("/w/ghost/repo", "/w/ghost", "ghost/repo")},
	)
	if _, ok := r.Attribute("/w/ghost/repo"); ok {
		t.Error("a membership row with no project row must not attribute")
	}
	if r.Visible("/w/ghost/repo") {
		t.Error("an orphan membership must fail closed while hiding is active")
	}
}
