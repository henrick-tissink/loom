package registry

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/henricktissink/loom/internal/transcript"
)

// fixture builds a workspace from a declarative tree so the table cases below
// read as the spec's decision list rather than as filesystem plumbing.
type fixture struct {
	dirs        []string // dirs to create under the workspace root
	transcripts []string // workspace-relative dirs given a claude transcript dir
	symlinks    [][2]string
}

func (f fixture) build(t *testing.T) (root, ccd string) {
	t.Helper()
	root, ccd = t.TempDir(), t.TempDir()
	for _, d := range f.dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range f.transcripts {
		p := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(filepath.Join(ccd, "projects", transcript.ProjectDirName(p)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range f.symlinks {
		if err := os.Symlink(filepath.Join(root, filepath.FromSlash(s[0])), filepath.Join(root, filepath.FromSlash(s[1]))); err != nil {
			t.Fatal(err)
		}
	}
	return root, ccd
}

// shape renders discovery output as "name=root:label|label" strings so a case
// pins roots, membership and labels at once — the three things §3 and §2 bind.
func shape(root string, ps []Project) []string {
	var out []string
	for _, p := range ps {
		s := p.Name + "=" + rel(root, p.Root) + ":"
		for i, r := range p.Repos {
			if i > 0 {
				s += "|"
			}
			s += r.Label + "@" + rel(root, r.Path)
		}
		out = append(out, s)
	}
	return out
}

func rel(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

func TestDiscoverDecisionList(t *testing.T) {
	cases := []struct {
		name string
		fix  fixture
		want []string
	}{
		{
			name: "rule 1: repo is a single-repo project labeled with the bare basename",
			fix:  fixture{dirs: []string{"solo/.git"}},
			want: []string{"solo=solo:solo@solo"},
		},
		{
			name: "rule 1 beats rule 2: a repo containing repos is not descended into",
			fix:  fixture{dirs: []string{"outer/.git", "outer/vendored/.git", "outer/sub/.git"}},
			want: []string{"outer=outer:outer@outer"},
		},
		{
			name: "rule 2: group with repo children becomes a project owning them",
			fix:  fixture{dirs: []string{"group/a/.git", "group/b/.git", "group/plain"}},
			want: []string{"group=group:group/a@group/a|group/b@group/b"},
		},
		{
			name: "rule 2 wins over rule 3: parent has a transcript dir AND repo children",
			fix: fixture{
				dirs:        []string{"Innostream/ballista/.git", "Innostream/bankenstein/.git"},
				transcripts: []string{"Innostream"},
			},
			want: []string{"Innostream=Innostream:Innostream/ballista@Innostream/ballista|Innostream/bankenstein@Innostream/bankenstein"},
		},
		{
			name: "rule 2: a transcript-only child rides along on a qualifying group",
			fix: fixture{
				dirs:        []string{"group/repo/.git", "group/clauded"},
				transcripts: []string{"group/clauded"},
			},
			want: []string{"group=group:group/clauded@group/clauded|group/repo@group/repo"},
		},
		{
			name: "rule 2 keys on descent: transcript-only children alone do not promote a group",
			fix: fixture{
				dirs:        []string{"group/clauded"},
				transcripts: []string{"group/clauded"},
			},
			want: nil,
		},
		{
			name: "rule 3: transcript-only leaf is a zero-repo project rooted at itself",
			fix: fixture{
				dirs:        []string{"clauded"},
				transcripts: []string{"clauded"},
			},
			want: []string{"clauded=clauded:"},
		},
		{
			name: "rule 4: non-git non-transcript leaf is not a project",
			fix:  fixture{dirs: []string{"plaindir"}},
			want: nil,
		},
		{
			name: "rule 4: empty group of plain dirs is not a project",
			fix:  fixture{dirs: []string{"emptygroup/plain"}},
			want: nil,
		},
		{
			name: "dot-prefixed entries are skipped even when they are repos",
			fix:  fixture{dirs: []string{".hidden/.git", "group/.hiddenchild/.git", "group/real/.git"}},
			want: []string{"group=group:group/real@group/real"},
		},
		{
			name: "depth stays one level: a nested group at depth 2 is not discovered",
			fix:  fixture{dirs: []string{"Innostream/albedo/voucher-api/.git", "Innostream/ballista/.git"}},
			want: []string{"Innostream=Innostream:Innostream/ballista@Innostream/ballista"},
		},
		{
			name: "a one-child group keeps the project/repo label so saved workflows resolve",
			fix:  fixture{dirs: []string{"group/only/.git"}},
			want: []string{"group=group:group/only@group/only"},
		},
		{
			name: "mixed workspace, sorted by root",
			fix: fixture{
				dirs:        []string{"a-repo/.git", "b-group/child/.git", "c-clauded", "d-plain"},
				transcripts: []string{"c-clauded"},
			},
			want: []string{
				"a-repo=a-repo:a-repo@a-repo",
				"b-group=b-group:b-group/child@b-group/child",
				"c-clauded=c-clauded:",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, ccd := tc.fix.build(t)
			ps, err := Discover(root, ccd)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if got := shape(root, ps); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Discover =\n  %v\nwant\n  %v", got, tc.want)
			}
		})
	}
}

// A plain file must never be mistaken for a launchable dir.
func TestDiscoverIgnoresFiles(t *testing.T) {
	root, ccd := fixture{dirs: []string{"repo/.git"}}.build(t)
	if err := os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := Discover(root, ccd)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shape(root, ps), []string{"repo=repo:repo@repo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover = %v, want %v", got, want)
	}
}

// Symlinked entries are skipped at both levels: DirEntry.Type() comes from the
// dirent, so IsDir() is false. This is why Innostream/leikur (a symlink to
// bankenstein) stays out of the restored set.
func TestDiscoverSkipsSymlinks(t *testing.T) {
	fix := fixture{
		dirs:     []string{"group/real/.git", "target/.git"},
		symlinks: [][2]string{{"target", "link"}, {"group/real", "group/leikur"}},
	}
	root, ccd := fix.build(t)
	ps, err := Discover(root, ccd)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"group=group:group/real@group/real", "target=target:target@target"}
	if got := shape(root, ps); !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover = %v, want %v", got, want)
	}
}

// One bad permission bit must not cost the user every other project.
func TestDiscoverUnreadableDirIsNonFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any directory")
	}
	root, ccd := fixture{dirs: []string{"good/child/.git", "locked/child"}}.build(t)
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) }) // else TempDir cleanup fails

	ps, err := Discover(root, ccd)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got, want := shape(root, ps), []string{"good=good:good/child@good/child"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover = %v, want %v", got, want)
	}
}

// An unreadable workspace root is the one fatal case: there is nothing to
// degrade to, and the caller decides whether to continue.
func TestDiscoverMissingWorkspaceRootErrors(t *testing.T) {
	if _, err := Discover(filepath.Join(t.TempDir(), "nope"), t.TempDir()); err == nil {
		t.Fatal("Discover on a missing root = nil error, want error")
	}
}

// Spec §4: output paths are Abs+Clean, so a trailing slash cannot break
// segment-wise matching or mint a second row for the same directory.
func TestDiscoverCanonicalizesPaths(t *testing.T) {
	root, ccd := fixture{dirs: []string{"group/child/.git"}}.build(t)
	ps, err := Discover(root+string(filepath.Separator)+".", ccd)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || len(ps[0].Repos) != 1 {
		t.Fatalf("Discover = %+v", ps)
	}
	if want := filepath.Join(root, "group"); ps[0].Root != want {
		t.Fatalf("Root = %q, want %q", ps[0].Root, want)
	}
	if want := filepath.Join(root, "group", "child"); ps[0].Repos[0].Path != want {
		t.Fatalf("Repo.Path = %q, want %q", ps[0].Repos[0].Path, want)
	}
}

// Discovery must not read anything but the filesystem, and must be stable:
// two passes over an unchanged tree return identical output.
func TestDiscoverIsDeterministic(t *testing.T) {
	root, ccd := fixture{dirs: []string{"a/.git", "b/x/.git", "b/y/.git"}}.build(t)
	first, err := Discover(root, ccd)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Discover(root, ccd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Discover not stable:\n %+v\n %+v", first, second)
	}
}

func TestRepos(t *testing.T) {
	root, ccd := fixture{
		dirs:        []string{"solo/.git", "group/b/.git", "group/a/.git", "clauded"},
		transcripts: []string{"clauded"},
	}.build(t)
	ps, err := Discover(root, ccd)
	if err != nil {
		t.Fatal(err)
	}
	rs := Repos(ps)
	var labels []string
	for _, r := range rs {
		labels = append(labels, r.Label)
	}
	want := []string{"group/a", "group/b", "solo"} // the zero-repo project contributes none
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("Repos labels = %v, want %v", labels, want)
	}
	if !sort.StringsAreSorted(labels) {
		t.Fatalf("Repos not sorted by label: %v", labels)
	}
	if rs[2].Path != filepath.Join(root, "solo") {
		t.Fatalf("Path = %q", rs[2].Path)
	}
}

// TestChildRepos pins the create-project prefill (§8): §3's rule-2 arm applied
// to one picked root, sharing Discover's dotfile/symlink/isRepo rules so the
// checklist and the scanner cannot drift apart.
func TestChildRepos(t *testing.T) {
	cases := []struct {
		name string
		fix  fixture
		pick string
		want []string
	}{
		{
			name: "immediate child repos, sorted",
			fix:  fixture{dirs: []string{"g/b/.git", "g/a/.git", "g/plain"}},
			pick: "g",
			want: []string{"a", "b"},
		},
		{
			// A transcript-only child is launchable but is NOT a repo, so it is
			// not pre-checked into a project the user is composing by hand.
			name: "transcript-only child is not offered",
			fix:  fixture{dirs: []string{"g/a/.git", "g/hist"}, transcripts: []string{"g/hist"}},
			pick: "g",
			want: []string{"a"},
		},
		{
			name: "dotfiles and symlinks skipped",
			fix: fixture{
				dirs:     []string{"g/a/.git", "g/.hidden/.git"},
				symlinks: [][2]string{{"g/a", "g/link"}},
			},
			pick: "g",
			want: []string{"a"},
		},
		{
			name: "nested repos are not descended into",
			fix:  fixture{dirs: []string{"g/a/.git", "g/a/inner/.git"}},
			pick: "g",
			want: []string{"a"},
		},
		{
			name: "a root with no repo children yields nothing",
			fix:  fixture{dirs: []string{"g/plain"}},
			pick: "g",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, _ := c.fix.build(t)
			got := ChildRepos(filepath.Join(root, c.pick))
			var rels []string
			for _, p := range got {
				rels = append(rels, rel(filepath.Join(root, c.pick), p))
			}
			if !reflect.DeepEqual(rels, c.want) {
				t.Fatalf("ChildRepos = %v, want %v", rels, c.want)
			}
		})
	}
}

// An unreadable or absent root degrades to an empty checklist, never an error:
// the modal still offers the root itself, so it is never a dead end (§8).
func TestChildReposMissingRootIsEmpty(t *testing.T) {
	if got := ChildRepos(filepath.Join(t.TempDir(), "nope")); got != nil {
		t.Fatalf("ChildRepos(missing) = %v, want nil", got)
	}
}

// The live regression (§3): a parent with a transcript dir AND repo children
// yields the children, not the parent as a leaf. Pinned to the verified shape.
func TestDiscoverRestoresVerifiedInvisibleSet(t *testing.T) {
	inno := []string{"ballista", "bankenstein", "bb-integr8", "flux-fleet", "quickbit", "terraform-infra-eks"}
	happy := []string{"HappyCardEngine", "HappyPay", "HappyPayCLM", "HappyPayCoreApi", "HappyPayMembers",
		"HappyPayMerchants", "HappyPaySavaToolset", "_v3-members", "_v3-merchants", "_v3-monolith"}
	fix := fixture{transcripts: []string{"Innostream", "HappyPay"}, symlinks: [][2]string{{"Innostream/bankenstein", "Innostream/leikur"}}}
	for _, r := range inno {
		fix.dirs = append(fix.dirs, "Innostream/"+r+"/.git")
	}
	for _, r := range happy {
		fix.dirs = append(fix.dirs, "HappyPay/"+r+"/.git")
	}
	root, ccd := fix.build(t)

	ps, err := Discover(root, ccd)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("Discover = %v, want 2 projects", shape(root, ps))
	}
	if got := len(Repos(ps)); got != 16 {
		t.Fatalf("restored %d repos, want 16 (leikur is a symlink and must not count): %v", got, shape(root, ps))
	}
	for _, p := range ps {
		for _, r := range p.Repos {
			if want := p.Name + "/" + filepath.Base(r.Path); r.Label != want {
				t.Fatalf("Label = %q, want %q", r.Label, want)
			}
		}
	}
}
