package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stamp(branch, head string) RepoStamp { return RepoStamp{Branch: branch, Head: head} }

// TestDriftLines covers every case §8 enumerates. Drift is the only thing a
// respawned orchestrator has to tell it what moved while it was gone, so each
// line's wording is pinned: "history rewritten or commit unknown" must never
// become a fabricated number, and "notes edited" must stay labelled expected.
func TestDriftLines(t *testing.T) {
	const a, b = "/w/a", "/w/b"
	tests := []struct {
		name   string
		prev   State
		repos  []RepoState
		notes  []noteFile
		git    fakeGit
		want   []string
		absent []string
	}{
		{
			name:  "repo moved with a known old sha reports a commit count",
			prev:  State{Repos: map[string]RepoStamp{a: stamp("main", "old111")}},
			repos: []RepoState{{Label: "a", Path: a, Branch: "main", Head: "new222"}},
			git: fakeGit{
				known: map[string]bool{a + "\x00old111": true},
				count: map[string]string{a + "\x00old111..HEAD": "14\n"},
			},
			want: []string{"a: 14 commits since the last brief (old111 → new222)"},
		},
		{
			name:  "unknown old sha says so rather than guessing",
			prev:  State{Repos: map[string]RepoStamp{a: stamp("main", "gone999")}},
			repos: []RepoState{{Label: "a", Path: a, Branch: "main", Head: "new222"}},
			git:   fakeGit{},
			want:  []string{"a: history rewritten or commit unknown (gone999 → new222)"},
		},
		{
			name:  "same head, different branch",
			prev:  State{Repos: map[string]RepoStamp{a: stamp("main", "same")}},
			repos: []RepoState{{Label: "a", Path: a, Branch: "dev", Head: "same"}},
			want:  []string{"a: branch changed main → dev"},
		},
		{
			name:  "repo added and repo removed are both listed",
			prev:  State{Repos: map[string]RepoStamp{a: stamp("main", "x")}},
			repos: []RepoState{{Label: "b", Path: b, Branch: "main", Head: "y"}},
			want: []string{
				"membership changed: repo added — " + b,
				"membership changed: repo removed — " + a,
			},
		},
		{
			name: "edited note is labelled expected",
			prev: State{Notes: map[string]NoteStamp{"loom-map.md": {SHA256: "old", Bytes: 10}}},
			notes: []noteFile{
				{Name: "loom-map.md", Exists: true, SHA256: "new", Bytes: 12},
			},
			want: []string{"notes edited (expected): loom-map.md"},
		},
		{
			name: "unchanged note says nothing",
			prev: State{Notes: map[string]NoteStamp{"loom-map.md": {SHA256: "same", Bytes: 10}}},
			notes: []noteFile{
				{Name: "loom-map.md", Exists: true, SHA256: "same", Bytes: 10},
			},
			absent: []string{"loom-map.md"},
		},
		{
			name:  "missing note is reported and not recreated",
			prev:  State{Notes: map[string]NoteStamp{"loom-open.md": {SHA256: "x", Bytes: 44}}},
			notes: []noteFile{{Name: "loom-open.md"}},
			want:  []string{"notes missing: loom-open.md is gone (recorded at 44 bytes) — not recreated"},
		},
		{
			name:   "an unreadable repo produces no drift line at all",
			prev:   State{Repos: map[string]RepoStamp{a: stamp("main", "old111")}},
			repos:  []RepoState{{Label: "a", Path: a, Err: "not a git repository"}},
			absent: []string{"a:"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(driftLines(tc.git.run, tc.prev, tc.repos, tc.notes), "\n")
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("missing %q in:\n%s", w, got)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Fatalf("unexpected %q in:\n%s", a, got)
				}
			}
		})
	}
}

// TestCommitsSinceRealRepo runs the drift counter against a real scratch
// repository — the fake proves the wiring, this proves the git invocation.
// Scratch repo under t.TempDir(); never the loom repo.
func TestCommitsSinceRealRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	base := gitRepo(t, dir)
	gitCommit(t, dir, "b.txt")
	gitCommit(t, dir, "c.txt")

	n, ok := commitsSince(runGit, dir, base)
	if !ok || n != 2 {
		t.Fatalf("commitsSince = %d, %v; want 2, true", n, ok)
	}
	if _, ok := commitsSince(runGit, dir, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); ok {
		t.Fatal("an unknown sha must report ok=false, never a fabricated count")
	}
	if _, ok := commitsSince(runGit, dir, ""); ok {
		t.Fatal("an empty old sha must report ok=false")
	}
}

// TestRepoStateRealRepo pins §5.1's three facts, including the dirty count, and
// the degrade-to-(unavailable) path for a directory that is not a repo.
func TestRepoStateRealRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	head := gitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rs := repoState(runGit, "repo", dir, false)
	if rs.Branch != "main" || rs.Head != head || rs.Dirty != 1 || rs.Err != "" {
		t.Fatalf("repoState = %+v (head %s)", rs, head)
	}

	notARepo := t.TempDir()
	if rs := repoState(runGit, "x", notARepo, false); rs.Err == "" {
		t.Fatal("a non-repo must report an error, not a clean state")
	}
	if rs := repoState(runGit, "x", "/does/not/exist", true); rs.Err != "" || rs.Branch != "" {
		t.Fatalf("a missing repo must not be stat'ed: %+v", rs)
	}
}

// TestStateRoundTripAndCorruption pins §8: state.json survives a round trip,
// and a corrupt or future-schema file is treated as ABSENT rather than
// returning an error — it holds nothing that is not rederivable, and a
// truncated write must not permanently block spawning.
func TestStateRoundTripAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := State{
		Schema: stateSchema, ProjectRoot: "/w/p", NotesDir: "/n",
		AssembledAt: 5, SpawnCount: 7, LastSession: "loom-x",
		BriefBytes: 41233, BriefSHA256: "abc", TruncatedSections: []string{SecRecent},
		Repos: map[string]RepoStamp{"/w/p": stamp("main", "h")},
		Notes: map[string]NoteStamp{"loom-map.md": {SHA256: "s", Bytes: 3}},
	}
	if err := saveState(path, want); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	got, ok := loadState(path)
	if !ok || got.SpawnCount != 7 || got.Repos["/w/p"].Head != "h" ||
		got.Notes["loom-map.md"].Bytes != 3 || len(got.TruncatedSections) != 1 {
		t.Fatalf("round trip lost data: %+v ok=%v", got, ok)
	}

	for _, corrupt := range []string{"{not json", "", `{"schema":99}`} {
		if err := os.WriteFile(path, []byte(corrupt), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := loadState(path); ok {
			t.Fatalf("corrupt state %q was accepted", corrupt)
		}
	}
	if _, ok := loadState(filepath.Join(t.TempDir(), "nope.json")); ok {
		t.Fatal("a missing state file was reported as present")
	}
}

// TestReadNotesPreExisting pins §3's pre-existing rule and the fixed order.
func TestReadNotesPreExisting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "loom-map.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No state at all: everything on disk is pre-existing, because this
	// generation did not write it.
	notes := readNotes(dir, State{}, false)
	if len(notes) != len(NoteFiles) {
		t.Fatalf("want %d notes, got %d", len(NoteFiles), len(notes))
	}
	for i, n := range notes {
		if n.Name != NoteFiles[i] {
			t.Fatalf("note order is not fixed: %v", notes)
		}
	}
	if !notes[0].Exists || !notes[0].PreExisting || notes[0].SHA256 != digest([]byte("hi")) {
		t.Fatalf("first note: %+v", notes[0])
	}
	if notes[1].Exists || notes[1].PreExisting {
		t.Fatalf("absent note flagged: %+v", notes[1])
	}

	// Known to state: not pre-existing.
	known := State{Notes: map[string]NoteStamp{"loom-map.md": {SHA256: digest([]byte("hi")), Bytes: 2}}}
	if readNotes(dir, known, true)[0].PreExisting {
		t.Fatal("a note recorded in state.json was flagged pre-existing")
	}
}
