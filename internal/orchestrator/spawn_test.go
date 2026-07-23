package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

// fixture builds a project with a root and one out-of-root sibling repo, plus a
// Service wired to a fake launcher and a fake git.
type fixture struct {
	st      *store.Store
	svc     *Service
	l       *fakeLauncher
	root    string
	sibling string
	loomDir string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := openStore(t)
	root := t.TempDir()
	sibling := t.TempDir()
	inRoot := filepath.Join(root, "inner")
	if err := os.MkdirAll(inRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	mustProject(t, st, root, "Innostream")
	mustRepo(t, st, inRoot, root, "inner")
	mustRepo(t, st, sibling, root, "sidecar")

	g := fakeGit{
		branch: map[string]string{root: "main", inRoot: "main", sibling: "dev"},
		head:   map[string]string{root: "aaa", inRoot: "bbb", sibling: "ccc"},
	}
	l := &fakeLauncher{}
	s, loomDir := svc(t, st, l, g.run)
	return &fixture{st: st, svc: s, l: l, root: root, sibling: sibling, loomDir: loomDir}
}

// TestLaunchShape is §13's launch-shape case. The permission-mode assertion is
// made on the EXACT argv, not on Recipe.Mode: revision 1 of the spec set
// Mode:"" and called it "default mode", and Recipe.Argv appends nothing for an
// empty Mode — so the field looked deliberate and produced no flag at all,
// leaving the session on the account default (auto), under which a write to an
// --add-dir'd sibling is allowed with no prompt.
func TestLaunchShape(t *testing.T) {
	f := newFixture(t)
	res, err := f.svc.Spawn(f.root, "map the seam", 100, 40)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if len(f.l.recipes) != 1 {
		t.Fatalf("want exactly one launch, got %d", len(f.l.recipes))
	}
	r, argv := f.l.recipes[0], f.l.argvs[0]

	if r.Cwd != f.root {
		t.Fatalf("cwd = %q, want the project root %q", r.Cwd, f.root)
	}
	if r.Model != "opus" {
		t.Fatalf("model = %q, want opus", r.Model)
	}
	if !argvHasPair(argv, "--permission-mode", "default") {
		t.Fatalf("argv lacks --permission-mode default: %v", argv)
	}
	if argvHas(argv, "--permission-mode", "acceptEdits") || argvHas(argv, "--permission-mode", "plan") {
		t.Fatalf("argv carries a rejected permission mode: %v", argv)
	}

	// Add-dirs: the out-of-root sibling and the materialized notes dir, never
	// the in-root repo (cwd already grants it) and never the root itself.
	notesDir := PathsFor(f.loomDir, f.root).NotesDir
	want := map[string]bool{f.sibling: true, notesDir: true}
	if len(r.AddDirs) != len(want) {
		t.Fatalf("add-dirs = %v, want exactly %v", r.AddDirs, want)
	}
	for _, d := range r.AddDirs {
		if !want[d] {
			t.Fatalf("unexpected add-dir %q in %v", d, r.AddDirs)
		}
	}
	for _, d := range r.AddDirs {
		if !argvHasPair(argv, "--add-dir", d) {
			t.Fatalf("add-dir %q missing from argv %v", d, argv)
		}
	}

	// The seed is a POINTER, not a payload (§6).
	if !strings.Contains(r.Seed, res.BriefPath) || len(r.Seed) >= seedCap ||
		strings.ContainsAny(r.Seed, "\n\r") {
		t.Fatalf("seed is not a single short pointer line: %q", r.Seed)
	}
	if _, err := os.Stat(res.BriefPath); err != nil {
		t.Fatalf("brief.md was not written before the launch: %v", err)
	}
}

func argvHasPair(argv []string, flag, val string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == val {
			return true
		}
	}
	return false
}

func argvHas(argv []string, flag, val string) bool { return argvHasPair(argv, flag, val) }

// TestSpawnGuards pins §7.1 and §2: Ungrouped can never have an orchestrator
// (the BACKEND rejects it), a missing root cannot spawn, and an unknown root is
// named rather than silently ignored. Every refusal must leave no launch behind.
func TestSpawnGuards(t *testing.T) {
	tests := []struct {
		name string
		root func(f *fixture) string
		prep func(t *testing.T, f *fixture)
		want error
	}{
		{"ungrouped", func(f *fixture) string { return store.UngroupedRoot }, nil, ErrUngrouped},
		{"unknown project", func(f *fixture) string { return "/no/such/project" }, nil, ErrNoProject},
		{
			name: "missing project",
			root: func(f *fixture) string { return f.root },
			prep: func(t *testing.T, f *fixture) {
				if err := f.st.SetProjectMissing(f.root, true, 1); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrProjectMissing,
		},
		{
			name: "root directory gone",
			root: func(f *fixture) string { return f.root },
			prep: func(t *testing.T, f *fixture) { os.RemoveAll(f.root) },
			want: ErrProjectMissing,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.prep != nil {
				tc.prep(t, f)
			}
			_, err := f.svc.Spawn(tc.root(f), "", 80, 24)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if len(f.l.recipes) != 0 {
				t.Fatal("a refused spawn still launched something")
			}
		})
	}
}

// TestSingleton pins §2 and §7.3: the claim is taken before the launch, so two
// concurrent spawns produce exactly ONE launch and the loser is told who won.
// This is what makes the singleton true across two Loom instances, where a UI
// guard would not be.
func TestSingleton(t *testing.T) {
	f := newFixture(t)
	first, err := f.svc.Spawn(f.root, "", 80, 24)
	if err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	_, err = f.svc.Spawn(f.root, "", 80, 24)
	if !errors.Is(err, ErrOrchestratorExists) {
		t.Fatalf("second spawn: got %v, want ErrOrchestratorExists", err)
	}
	if !strings.Contains(err.Error(), first.SessionName) {
		t.Fatalf("refusal does not name the winner: %v", err)
	}
	if len(f.l.recipes) != 1 {
		t.Fatalf("two spawns produced %d launches", len(f.l.recipes))
	}
}

// TestSpawnAfterEnded pins the conflict predicate's other arm: a terminated
// orchestrator's row is KEPT (so the overview can say "last orchestrator ran
// Tuesday"), so the row's mere existence must not block the next spawn.
func TestSpawnAfterEnded(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Spawn(f.root, "", 80, 24); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.EndOrchestrator(f.root, 999); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Spawn(f.root, "", 80, 24); err != nil {
		t.Fatalf("spawn after the previous one ended: %v", err)
	}
	if len(f.l.recipes) != 2 {
		t.Fatalf("want 2 launches, got %d", len(f.l.recipes))
	}
}

// TestNotesDirMaterialized pins §3 and §7.2's first branch, including the
// regression the stored-not-derived rule exists for: RepointProject changes the
// root, and a DERIVED default would silently relocate the project's whole brain
// on a directory rename.
func TestNotesDirMaterialized(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Spawn(f.root, "", 80, 24); err != nil {
		t.Fatal(err)
	}
	want := PathsFor(f.loomDir, f.root).NotesDir
	p, ok, err := f.st.GetProject(f.root)
	if err != nil || !ok {
		t.Fatalf("GetProject: %v %v", ok, err)
	}
	if p.NotesDir != want {
		t.Fatalf("notes_dir = %q, want %q written back to the row", p.NotesDir, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("notes dir not created under ~/.loom: %v", err)
	}

	newRoot := t.TempDir()
	if err := f.st.RepointProject(f.root, newRoot, 2); err != nil {
		t.Fatalf("RepointProject: %v", err)
	}
	p, _, err = f.st.GetProject(newRoot)
	if err != nil {
		t.Fatal(err)
	}
	if p.NotesDir != want {
		t.Fatalf("notes_dir moved on re-point: %q, want %q", p.NotesDir, want)
	}
}

// TestNotesDirInWorkspaceMustExist pins §7.2's named error. Loom does not
// create directories in the user's workspace, full stop — so the spawn fails,
// creates nothing, and does not leave a claim behind that a launch would have.
func TestNotesDirInWorkspaceMustExist(t *testing.T) {
	f := newFixture(t)
	inWorkspace := filepath.Join(f.root, "docs", "loom")
	if err := f.st.SetProjectNotesDir(f.root, inWorkspace, 1); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Spawn(f.root, "", 80, 24)
	if !errors.Is(err, ErrNotesDirMissing) {
		t.Fatalf("got %v, want ErrNotesDirMissing", err)
	}
	if _, statErr := os.Stat(inWorkspace); !os.IsNotExist(statErr) {
		t.Fatal("Loom created a directory in the user's workspace")
	}
	if len(f.l.recipes) != 0 {
		t.Fatal("a refused spawn still launched something")
	}
	if _, ok, _ := f.st.GetOrchestrator(f.root); ok {
		t.Fatal("a guard failure left a claim row behind")
	}
}

// TestNotesDirUnderLoomIsCreated is the asymmetry's other arm: a notes_dir Loom
// owns (under ~/.loom) that has been deleted is simply recreated, because that
// tree is Loom's to write in.
func TestNotesDirUnderLoomIsCreated(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.loomDir, "projects", "custom", "notes")
	if err := f.st.SetProjectNotesDir(f.root, dir, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Spawn(f.root, "", 80, 24); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("notes dir under ~/.loom was not created: %v", err)
	}
}

// TestMoveNotesCopiesAndKeepsSource pins §3's *Move notes…*: Loom retires,
// never deletes, so the source stays exactly where it was.
func TestMoveNotesCopiesAndKeepsSource(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Spawn(f.root, "", 80, 24); err != nil {
		t.Fatal(err)
	}
	src := PathsFor(f.loomDir, f.root).NotesDir
	for _, n := range NoteFiles {
		if err := os.WriteFile(filepath.Join(src, n), []byte("body of "+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dst := t.TempDir()
	if err := f.svc.MoveNotes(f.root, dst); err != nil {
		t.Fatalf("MoveNotes: %v", err)
	}
	for _, n := range NoteFiles {
		if got := readFile(t, filepath.Join(dst, n)); got != "body of "+n {
			t.Fatalf("%s not copied: %q", n, got)
		}
		if _, err := os.Stat(filepath.Join(src, n)); err != nil {
			t.Fatalf("%s was removed from the source: %v", n, err)
		}
	}
	p, _, _ := f.st.GetProject(f.root)
	if p.NotesDir != dst {
		t.Fatalf("notes_dir = %q, want %q", p.NotesDir, dst)
	}
}

// TestSweepReapsStaleClaims pins §7's disclosed failure mode and its recovery: a
// launch that fails after a successful claim strands a claim row, which is
// reaped only once it is older than ClaimGrace. Younger claims are left alone,
// because reaping one early would let a second spawn win a root whose first
// launch is still in flight.
func TestSweepReapsStaleClaims(t *testing.T) {
	f := newFixture(t)
	f.l.err = errors.New("tmux is on fire")
	if _, err := f.svc.Spawn(f.root, "", 80, 24); err == nil {
		t.Fatal("a failing launch reported success")
	}
	if _, ok, _ := f.st.GetOrchestrator(f.root); !ok {
		t.Fatal("the claim was rolled back — the sweep, not the caller, owns recovery")
	}

	// Younger than the grace: untouched.
	f.svc.now = func() time.Time { return fixedNow.Add(ClaimGrace - time.Second) }
	if n, err := f.svc.Sweep(); err != nil || n != 0 {
		t.Fatalf("Sweep reaped %d young claims (err %v)", n, err)
	}
	if _, ok, _ := f.st.GetOrchestrator(f.root); !ok {
		t.Fatal("a claim younger than the grace was reaped")
	}

	// Older: reaped, and the project can spawn again.
	f.svc.now = func() time.Time { return fixedNow.Add(ClaimGrace + time.Second) }
	if n, err := f.svc.Sweep(); err != nil || n != 1 {
		t.Fatalf("Sweep reaped %d stale claims (err %v)", n, err)
	}
	if _, ok, _ := f.st.GetOrchestrator(f.root); ok {
		t.Fatal("a stale claim survived the sweep")
	}
	f.l.err = nil
	if _, err := f.svc.Spawn(f.root, "", 80, 24); err != nil {
		t.Fatalf("spawn after the reap: %v", err)
	}
}

// TestSweepAdoptsAndRetires pins §9's adopt-before-reap and the fact that
// ended_at is stamped HERE rather than by status.Engine — the engine stays
// project-unaware, which is what keeps slice 1 §6.2a true.
func TestSweepAdoptsAndRetires(t *testing.T) {
	f := newFixture(t)

	// A live orch-tagged session with no row: adopted.
	if err := f.st.Upsert(store.SessionRow{
		Name: "loom-orphan", ClaudeSessionID: "orphan", Cwd: f.root, Tags: "orch",
		CreatedAt: fixedNow.Unix(), EndedAt: -1, ExitCode: -1, LastStatus: "running",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	o, ok, err := f.st.GetOrchestrator(f.root)
	if err != nil || !ok || o.SessionName != "loom-orphan" || o.EndedAt != -1 {
		t.Fatalf("live orch-tagged session not adopted: %+v ok=%v err=%v", o, ok, err)
	}

	// Now the session ends. The next sweep retires the claim with the SESSION's
	// ended_at, not with `now`: "last orchestrator ran Tuesday" has to mean
	// Tuesday.
	if err := f.st.MarkEnded("loom-orphan", "done", 0, 4242); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	o, _, _ = f.st.GetOrchestrator(f.root)
	if o.EndedAt != 4242 {
		t.Fatalf("ended_at = %d, want the session's 4242", o.EndedAt)
	}
}

// TestSweepIgnoresUnattributedOrchSession pins the fail-closed edge: an
// orch-tagged session under a path no project owns has nothing to be the
// singleton of, so it is left alone rather than adopted onto Ungrouped — which
// §2 says can never have an orchestrator.
func TestSweepIgnoresUnattributedOrchSession(t *testing.T) {
	f := newFixture(t)
	if err := f.st.Upsert(store.SessionRow{
		Name: "loom-stray", ClaudeSessionID: "stray", Cwd: "/somewhere/else", Tags: "orch",
		CreatedAt: fixedNow.Unix(), EndedAt: -1, ExitCode: -1, LastStatus: "running",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, ok, _ := f.st.GetOrchestrator(store.UngroupedRoot); ok {
		t.Fatal("Ungrouped was given an orchestrator")
	}
}

// TestSpawnWritesBriefAndState pins §7.4 and §8: brief.md and state.json land
// under ~/.loom (never in the workspace), the ledger increments, and the digest
// recorded matches the brief actually written.
func TestSpawnWritesBriefAndState(t *testing.T) {
	f := newFixture(t)
	res, err := f.svc.Spawn(f.root, "map the seam", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	paths := PathsFor(f.loomDir, f.root)
	if !strings.HasPrefix(paths.Brief, f.loomDir) {
		t.Fatalf("brief written outside ~/.loom: %s", paths.Brief)
	}
	text := readFile(t, paths.Brief)
	if text != res.Brief.Text {
		t.Fatal("brief.md differs from the returned brief")
	}
	st, ok := loadState(paths.State)
	if !ok {
		t.Fatal("state.json missing or unreadable")
	}
	if st.BriefSHA256 != digest([]byte(text)) || st.BriefBytes != len(text) {
		t.Fatalf("state digest does not describe the brief: %+v", st)
	}
	if st.SpawnCount != 1 || st.LastSession != res.SessionName {
		t.Fatalf("spawn ledger not stamped: %+v", st)
	}
	if st.Repos[f.sibling].Head != "ccc" {
		t.Fatalf("repo stamp missing: %+v", st.Repos)
	}

	// A second spawn (after the first is retired) increments the ledger and
	// leaves the notes location alone.
	if _, err := f.st.EndOrchestrator(f.root, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Spawn(f.root, "", 80, 24); err != nil {
		t.Fatal(err)
	}
	st, _ = loadState(paths.State)
	if st.SpawnCount != 2 {
		t.Fatalf("spawn_count = %d, want 2", st.SpawnCount)
	}
}

// TestReassembleDoesNotSpawnOrCountAsOne pins §10's ReassembleBrief: it
// refreshes drift without launching anything, and a refresh is not a spawn.
func TestReassembleDoesNotSpawnOrCountAsOne(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Spawn(f.root, "", 80, 24); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Reassemble(f.root, ""); err != nil {
		t.Fatalf("Reassemble: %v", err)
	}
	if len(f.l.recipes) != 1 {
		t.Fatalf("Reassemble launched a session (%d launches)", len(f.l.recipes))
	}
	st, _ := loadState(PathsFor(f.loomDir, f.root).State)
	if st.SpawnCount != 1 {
		t.Fatalf("spawn_count = %d, want 1 — a refresh is not a spawn", st.SpawnCount)
	}
}

// TestCorruptStateSpawnStillSucceeds pins §8's "treated as absent and
// rewritten": the notes are the brain and this file is a cache.
func TestCorruptStateSpawnStillSucceeds(t *testing.T) {
	f := newFixture(t)
	paths := PathsFor(f.loomDir, f.root)
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.State, []byte("{{{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	br, err := f.svc.Spawn(f.root, "", 80, 24)
	if err != nil {
		t.Fatalf("Spawn over a corrupt state: %v", err)
	}
	if strings.Contains(br.Brief.Text, "## Drift\n- ") {
		t.Fatal("a corrupt state produced drift lines; it must be treated as absent")
	}
	if _, ok := loadState(paths.State); !ok {
		t.Fatal("state.json was not rewritten")
	}
}

// TestDriftAcrossTwoAssemblies is the end-to-end drift case: assemble, move a
// repo's HEAD, assemble again, and the second brief must name the movement.
func TestDriftAcrossTwoAssemblies(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	base := gitRepo(t, repo)
	mustProject(t, st, root, "P")
	mustRepo(t, st, repo, root, "repo")

	s, loomDir := svc(t, st, &fakeLauncher{}, nil) // real git
	if _, err := s.Reassemble(root, ""); err != nil {
		t.Fatal(err)
	}
	if got := loadStateHead(t, loomDir, root, repo); got != base {
		t.Fatalf("stamped head %q, want %q", got, base)
	}

	gitCommit(t, repo, "b.txt")
	gitCommit(t, repo, "c.txt")
	br, err := s.Reassemble(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(br.Text, "repo: 2 commits since the last brief") {
		t.Fatalf("drift not reported:\n%s", sections(br.Text)[SecDrift])
	}
}

func loadStateHead(t *testing.T, loomDir, root, repo string) string {
	t.Helper()
	st, ok := loadState(PathsFor(loomDir, root).State)
	if !ok {
		t.Fatal("state.json missing")
	}
	return st.Repos[repo].Head
}

// TestAddDirsFor pins §7.5's set arithmetic on its own: in-root repos are
// covered by cwd, missing repos are excluded (validateDirs would fail the whole
// launch on one), and notes_dir joins only when it is outside the root.
func TestAddDirsFor(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "inner")
	outside := t.TempDir()
	notesOut := t.TempDir()
	for _, d := range []string{inner} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	notesIn := filepath.Join(root, "notes")
	if err := os.MkdirAll(notesIn, 0o755); err != nil {
		t.Fatal(err)
	}

	repos := []store.ProjectRepo{
		{Path: inner}, {Path: outside}, {Path: "/gone", Missing: true},
		{Path: root}, {Path: outside}, // duplicate + the root itself
	}
	got := addDirsFor(root, notesOut, repos)
	if len(got) != 2 || got[0] != outside || got[1] != notesOut {
		t.Fatalf("add-dirs = %v, want [%s %s]", got, outside, notesOut)
	}
	if got := addDirsFor(root, notesIn, repos); len(got) != 1 || got[0] != outside {
		t.Fatalf("in-root notes dir should need no add-dir: %v", got)
	}
}

// TestUnderIsSegmentWise guards the containment test against the string-prefix
// bug slice 1's resolver documents for five real sibling repos.
func TestUnderIsSegmentWise(t *testing.T) {
	tests := []struct {
		root, p string
		want    bool
	}{
		{"/a/HappyPay", "/a/HappyPay", true},
		{"/a/HappyPay", "/a/HappyPay/x", true},
		{"/a/HappyPay", "/a/HappyPayCLM", false},
		{"/a/HappyPay", "/b", false},
		{"", "/a", false},
		{"/a", "", false},
	}
	for _, tc := range tests {
		if got := under(tc.root, tc.p); got != tc.want {
			t.Errorf("under(%q,%q) = %v, want %v", tc.root, tc.p, got, tc.want)
		}
	}
}

// TestIsOrchestratorSession pins the exported helper §7's maybeAutoSummarize
// skip depends on. Exported so no caller re-derives the token test.
func TestIsOrchestratorSession(t *testing.T) {
	if !IsOrchestratorSession("orch") || IsOrchestratorSession("orchid") || IsOrchestratorSession("") {
		t.Fatal("IsOrchestratorSession disagrees with hasOrchTag")
	}
}

// TestProjectKey pins §3's on-disk naming: greppable basename, hash
// disambiguation, and a name that sanitizes away still producing a usable
// directory rather than one that looks like a CLI flag.
func TestProjectKey(t *testing.T) {
	a, b := ProjectKey("/work/api"), ProjectKey("/oss/api")
	if a == b {
		t.Fatal("two roots sharing a basename produced the same key")
	}
	if !strings.HasPrefix(a, "api-") || len(a) != len("api-")+8 {
		t.Fatalf("key %q is not <basename>-<8 hex>", a)
	}
	if ProjectKey("/work/api") != a {
		t.Fatal("ProjectKey is not deterministic")
	}
	if k := ProjectKey("/w/a b:c"); strings.ContainsAny(k, " :/") {
		t.Fatalf("key %q was not sanitized", k)
	}
	if k := ProjectKey("/w/***"); !strings.HasPrefix(k, "project-") {
		t.Fatalf("a name that sanitizes away must not start with '-': %q", k)
	}
}
