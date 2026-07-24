package delegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// §12.3.3's tests. The comparator is exercised from LITERALS, because the
// interesting cases (a same-size rewrite, a file leaving the dirty set) are
// tedious to stage on disk and trivial to write down; the walk is exercised
// against real scratch repos, because the thing it can get wrong is what git
// calls a dirty file and where the path is rooted. Nothing here runs a mutating
// git command outside t.TempDir().

func stamps(fs ...FileStamp) []FileStamp { return fs }

func TestSnapshotRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   Snapshot
		want string
	}{
		{"nil encodes to the column default", nil, ""},
		{"empty encodes to the column default", Snapshot{}, ""},
		{
			// The load-bearing case: a dir that WAS walked and had nothing dirty
			// is the strongest baseline there is, and collapsing it to "" would
			// turn it into "never snapshotted".
			name: "a clean dir is still a baseline",
			in:   Snapshot{"/repo": {}},
			want: `{"/repo":[]}`,
		},
		{
			name: "stamps carry path, mtime and size",
			in:   Snapshot{"/repo": stamps(FileStamp{Path: "a.go", Mod: 100, Size: 7})},
			want: `{"/repo":[{"path":"a.go","mtime":100,"size":7}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EncodeSnapshot(tc.in)
			if got != tc.want {
				t.Fatalf("EncodeSnapshot = %q, want %q", got, tc.want)
			}
			back := DecodeSnapshot(got)
			if len(back) != len(tc.in) {
				t.Fatalf("round trip lost dirs: %#v", back)
			}
			for dir, want := range tc.in {
				gotFiles, ok := back[dir]
				if !ok {
					t.Fatalf("round trip lost dir %q", dir)
				}
				if len(gotFiles) != len(want) {
					t.Fatalf("dir %q: got %#v, want %#v", dir, gotFiles, want)
				}
				for i := range want {
					if gotFiles[i] != want[i] {
						t.Fatalf("dir %q file %d: got %#v, want %#v", dir, i, gotFiles[i], want[i])
					}
				}
			}
		})
	}
}

func TestSnapshotDecodeDegradesToNoBaseline(t *testing.T) {
	// A corrupt column must be an ADMISSION, never a claim. nil decodes to "no
	// baseline", which Compare reports as NoBaseline and the UI renders as "not
	// snapshotted" — not as "nothing changed".
	for _, raw := range []string{"", "   ", "not json", `["wrong shape"]`} {
		if got := DecodeSnapshot(raw); got != nil {
			t.Fatalf("DecodeSnapshot(%q) = %#v, want nil", raw, got)
		}
	}
	d := Compare(DecodeSnapshot("garbage"), Snapshot{"/repo": {}})
	if len(d.NoBaseline) != 1 || d.NoBaseline[0] != "/repo" {
		t.Fatalf("want /repo reported as NoBaseline, got %#v", d)
	}
	if d.Empty() {
		t.Fatal(`"we cannot tell" is a thing to say; Empty() must be false`)
	}
	if !strings.Contains(d.Summary(), "not snapshotted") {
		t.Fatalf("Summary must say it has no baseline, got %q", d.Summary())
	}
}

func TestSnapshotCompare(t *testing.T) {
	const dir = "/repo"
	base := stamps(
		FileStamp{Path: "a.go", Mod: 100, Size: 10},
		FileStamp{Path: "b.go", Mod: 200, Size: 20},
	)
	tests := []struct {
		name       string
		baseline   Snapshot
		now        Snapshot
		wantFiles  []string
		wantNoBase bool
	}{
		{
			name:     "untouched set is not flagged",
			baseline: Snapshot{dir: base},
			now:      Snapshot{dir: base},
		},
		{
			// The stamp's whole reason to exist: a rewrite that keeps the byte
			// count is caught by the mtime half.
			name:     "mtime-only change",
			baseline: Snapshot{dir: base},
			now: Snapshot{dir: stamps(
				FileStamp{Path: "a.go", Mod: 101, Size: 10},
				FileStamp{Path: "b.go", Mod: 200, Size: 20},
			)},
			wantFiles: []string{"a.go"},
		},
		{
			// And the other half: a filesystem whose mtime resolution loses a
			// fast write still gives up the size.
			name:     "size-only change",
			baseline: Snapshot{dir: base},
			now: Snapshot{dir: stamps(
				FileStamp{Path: "a.go", Mod: 100, Size: 11},
				FileStamp{Path: "b.go", Mod: 200, Size: 20},
			)},
			wantFiles: []string{"a.go"},
		},
		{
			name:      "a file joining the dirty set",
			baseline:  Snapshot{dir: base},
			now:       Snapshot{dir: append(append([]FileStamp{}, base...), FileStamp{Path: "c.go", Mod: 300, Size: 30})},
			wantFiles: []string{"c.go"},
		},
		{
			// A file that STOPPED being dirty was written to just as surely as
			// one that started — reverted, committed or deleted underneath us.
			name:      "a file leaving the dirty set",
			baseline:  Snapshot{dir: base},
			now:       Snapshot{dir: stamps(base[0])},
			wantFiles: []string{"b.go"},
		},
		{
			name:      "everything at once, sorted",
			baseline:  Snapshot{dir: base},
			now:       Snapshot{dir: stamps(FileStamp{Path: "a.go", Mod: 100, Size: 99}, FileStamp{Path: "z.go", Mod: 1, Size: 1})},
			wantFiles: []string{"a.go", "b.go", "z.go"},
		},
		{
			name:       "no baseline for a dir is not 'no change'",
			baseline:   Snapshot{"/other": base},
			now:        Snapshot{dir: base},
			wantNoBase: true,
		},
		{
			name:     "a dir the caller did not re-walk is not invented",
			baseline: Snapshot{dir: base, "/other": base},
			now:      Snapshot{dir: base},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Compare(tc.baseline, tc.now)
			var got []string
			if files, ok := d.Changed[dir]; ok {
				got = files
			}
			if strings.Join(got, ",") != strings.Join(tc.wantFiles, ",") {
				t.Fatalf("Changed[%s] = %v, want %v", dir, got, tc.wantFiles)
			}
			if hasNoBase := len(d.NoBaseline) > 0; hasNoBase != tc.wantNoBase {
				t.Fatalf("NoBaseline = %v, want any = %v", d.NoBaseline, tc.wantNoBase)
			}
			wantEmpty := len(tc.wantFiles) == 0 && !tc.wantNoBase
			if d.Empty() != wantEmpty {
				t.Fatalf("Empty() = %v, want %v (%#v)", d.Empty(), wantEmpty, d)
			}
		})
	}
}

// TestSnapshotSummaryNeverBlamesTheChild pins §12.3.3's BINDING wording. The
// walk cannot tell the human's own edits from a child's, and a detector that
// overclaims is one the human learns to dismiss.
func TestSnapshotSummaryNeverBlamesTheChild(t *testing.T) {
	d := Compare(Snapshot{"/repo": {}}, Snapshot{"/repo": stamps(FileStamp{Path: "a.go", Mod: 1, Size: 1})})
	s := d.Summary()
	if !strings.Contains(s, "changed since spawn") {
		t.Fatalf("summary must say 'changed since spawn', got %q", s)
	}
	if !strings.Contains(s, "a.go") {
		t.Fatalf("summary must name the files, got %q", s)
	}
	if !strings.Contains(s, "Loom cannot tell") {
		t.Fatalf("summary must carry the false-positive disclosure, got %q", s)
	}
	for _, banned := range []string{"the child wrote", "child wrote this", "written by the child"} {
		if strings.Contains(strings.ToLower(s), banned) {
			t.Fatalf("summary attributes the write to the child (%q): %q", banned, s)
		}
	}
}

func TestSnapshotDirs(t *testing.T) {
	m := Manifest{RepoPaths: map[string]string{"a": "/repos/alpha", "b": "/repos/beta"}}
	plan := BasePlan{AddDirs: []string{"/wt/gamma", "/repos/alpha"}}
	got := SnapshotDirs(m, plan)
	want := []string{"/repos/alpha", "/repos/beta", "/wt/gamma"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SnapshotDirs = %v, want %v (sorted and deduplicated)", got, want)
	}
	if got := SnapshotDirs(Manifest{}, BasePlan{AddDirs: []string{"", "   "}}); len(got) != 0 {
		t.Fatalf("blank entries must not become dirs: %v", got)
	}
}

// TestSnapshotDirtySetWalksRealRepos is the end-to-end half: what git calls
// dirty, where the paths are rooted, and that an untouched tree stays quiet.
func TestSnapshotDirtySetWalksRealRepos(t *testing.T) {
	repo := newScratchRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "hello\nmodified\n")
	writeFile(t, filepath.Join(repo, "new.txt"), "untracked\n")

	dirs := []string{repo}
	baseline := TakeSnapshot(dirs)
	if len(baseline[repo]) != 2 {
		t.Fatalf("want the modified tracked file and the untracked one, got %#v", baseline[repo])
	}
	if EncodeSnapshot(baseline) == "" {
		t.Fatal("a snapshot with files must not encode to the empty column")
	}

	// Nothing touched: the detector must stay quiet, or it is a detector nobody
	// reads.
	if d := Compare(baseline, TakeSnapshot(dirs)); !d.Empty() {
		t.Fatalf("untouched tree flagged: %#v", d)
	}

	// A write outside the child's worktree, by absolute path — exactly what
	// --add-dir cannot prevent.
	writeFile(t, filepath.Join(repo, "README.md"), "hello\nmodified again, and longer\n")
	d := Compare(baseline, TakeSnapshot(dirs))
	if d.Empty() {
		t.Fatal("a file modified between spawn and check was NOT flagged")
	}
	if files := d.Changed[repo]; len(files) != 1 || files[0] != "README.md" {
		t.Fatalf("Changed = %#v, want just README.md", d.Changed)
	}
	if !strings.Contains(d.Summary(), "README.md") {
		t.Fatalf("the flag must name the file: %q", d.Summary())
	}

	// A brand-new dirty file joins the set.
	writeFile(t, filepath.Join(repo, "sneaky.go"), "package x\n")
	d = Compare(baseline, TakeSnapshot(dirs))
	if files := d.Changed[repo]; len(files) != 2 || files[1] != "sneaky.go" {
		t.Fatalf("Changed = %#v, want README.md and sneaky.go", d.Changed)
	}
}

// TestSnapshotIgnoresCleanRepoAndNonRepo covers the two degradations that must
// not become errors: a clean repo (a baseline, empty), and a directory that is
// not a git work tree at all (an empty entry, no error, no spawn blocked).
func TestSnapshotIgnoresCleanRepoAndNonRepo(t *testing.T) {
	clean := newScratchRepo(t)
	plain := t.TempDir()
	s := TakeSnapshot([]string{clean, plain})
	for _, dir := range []string{clean, plain} {
		files, ok := s[dir]
		if !ok {
			t.Fatalf("dir %q must be RECORDED even with nothing in it: the key is the baseline", dir)
		}
		if len(files) != 0 {
			t.Fatalf("dir %q: got %#v, want no stamps", dir, files)
		}
	}
	if d := Compare(s, TakeSnapshot([]string{clean, plain})); !d.Empty() {
		t.Fatalf("clean dirs drifted: %#v", d)
	}
}

// TestSnapshotVanishedDirectoryDegradesVisibly: the dir is gone at check time.
// The comparator must not crash and must not stay silent — every file that was
// dirty there reads as changed, which is the visible degradation the house rules
// require.
func TestSnapshotVanishedDirectoryDegradesVisibly(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "-q", "-b", "main")
	mustGit(t, repo, "config", "user.email", "loom@example.test")
	mustGit(t, repo, "config", "user.name", "loom test")
	writeFile(t, filepath.Join(repo, "a.txt"), "one\n")
	mustGit(t, repo, "add", "a.txt")
	mustGit(t, repo, "commit", "-qm", "init")
	writeFile(t, filepath.Join(repo, "a.txt"), "one\ntwo\n")

	baseline := TakeSnapshot([]string{repo})
	if len(baseline[repo]) != 1 {
		t.Fatalf("baseline = %#v, want one dirty file", baseline[repo])
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}

	now := TakeSnapshot([]string{repo}) // must not panic
	if files, ok := now[repo]; !ok || len(files) != 0 {
		t.Fatalf("a vanished dir must contribute an EMPTY entry, got ok=%v %#v", ok, files)
	}
	d := Compare(baseline, now)
	if d.Empty() {
		t.Fatal("a vanished directory was silent; it must degrade VISIBLY")
	}
	if files := d.Changed[repo]; len(files) != 1 || files[0] != "a.txt" {
		t.Fatalf("Changed = %#v, want a.txt", d.Changed)
	}
}

// TestSnapshotCheckReWalksItsOwnDirs pins Snapshot.Check's contract, including
// the one place it deliberately cannot report NoBaseline (see its comment).
func TestSnapshotCheckReWalksItsOwnDirs(t *testing.T) {
	repo := newScratchRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "hello\nmodified\n")
	s := TakeSnapshot([]string{repo})

	if d := s.Check(); !d.Empty() {
		t.Fatalf("Check on an unchanged tree: %#v", d)
	}
	// Second granularity is the documented miss of the stamp; change the size so
	// the assertion is about the comparator and not about the clock.
	writeFile(t, filepath.Join(repo, "README.md"), "hello\nmodified rather more\n")
	if d := s.Check(); d.Empty() {
		t.Fatal("Check did not re-walk its own dirs")
	}
	if d := Snapshot(nil).Check(); !d.Empty() {
		t.Fatalf("a nil snapshot has no dirs to re-walk: %#v", d)
	}
}

// TestSnapshotStampGranularity documents, as an executable statement, the miss
// the stamp admits: a write preserving BOTH mtime and size is invisible. It is
// here so a future hand changing to a content hash can see what it buys, and so
// nobody discovers it as a surprise in production.
func TestSnapshotStampGranularity(t *testing.T) {
	repo := newScratchRepo(t)
	path := filepath.Join(repo, "README.md")
	writeFile(t, path, "hello\naaaa\n")
	when := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	baseline := TakeSnapshot([]string{repo})

	writeFile(t, path, "hello\nbbbb\n") // same length
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	if d := Compare(baseline, TakeSnapshot([]string{repo})); !d.Empty() {
		t.Fatalf("unexpected: this miss is documented, so the doc is now wrong: %#v", d)
	}

	// One second of mtime is all it takes to see it.
	if err := os.Chtimes(path, when.Add(time.Second), when.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if d := Compare(baseline, TakeSnapshot([]string{repo})); d.Empty() {
		t.Fatal("an mtime-only change on disk was not detected")
	}
}
