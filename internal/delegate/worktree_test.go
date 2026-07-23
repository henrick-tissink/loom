package delegate

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

// Every test in this file builds its own repo under t.TempDir(). Nothing here
// may run a mutating git command against the loom repo itself — `git worktree
// add` writes .git/worktrees/<id>/ and a refs/heads/loom/** ref into whatever
// repo it is pointed at, and a test suite is not allowed to do that to the
// developer's checkout.

func newScratchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "user.email", "loom@example.test")
	mustGit(t, dir, "config", "user.name", "loom test")
	writeFile(t, filepath.Join(dir, "README.md"), "hello\n")
	writeFile(t, filepath.Join(dir, ".gitignore"), ".env\nsecrets/\n")
	mustGit(t, dir, "add", "README.md", ".gitignore")
	mustGit(t, dir, "commit", "-qm", "init")
	return dir
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestWorktrees(t *testing.T) *Worktrees {
	t.Helper()
	return &Worktrees{Layout: NewLayout(t.TempDir()), Store: newTestStore(t)}
}

// occupy inserts a LIVE sessions row at dir, standing in for a child claude.
// The cwd is written through physicalPath because that is what Launcher.Launch
// writes, and comparing on anything else is the drift the guard cannot survive.
func occupyWorktree(t *testing.T, s *store.Store, name, dir string) {
	t.Helper()
	err := s.Upsert(store.SessionRow{
		Name: name, Cwd: physicalPath(dir), EndedAt: -1, ExitCode: -1, LastStatus: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func wtRequest(repo, base string) Request {
	return Request{
		RunSlug: "atlas-7", TaskID: "schema", RepoLabel: "bankenstein",
		RepoPath: repo, Base: base, Brief: "# brief\ndo the thing\n",
	}
}

func mustCreateWorktree(t *testing.T, w *Worktrees, r Request) Created {
	t.Helper()
	c, err := w.Create(r)
	if err != nil {
		t.Fatalf("Create(%s): %v", r.TaskID, err)
	}
	return c
}

func TestLayoutNaming(t *testing.T) {
	l := NewLayout("/home/u/.loom")
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"root", l.Root, "/home/u/.loom/worktrees"},
		{"run root", l.RunRoot("atlas-7"), "/home/u/.loom/worktrees/atlas-7"},
		{"dir", l.Dir("atlas-7", "bankenstein", "schema"), "/home/u/.loom/worktrees/atlas-7/bankenstein/schema"},
		{"meta is a sibling", l.MetaDir("atlas-7", "bankenstein", "schema"), "/home/u/.loom/worktrees/atlas-7/bankenstein/schema.meta"},
		{"brief", l.BriefPath("atlas-7", "bankenstein", "schema"), "/home/u/.loom/worktrees/atlas-7/bankenstein/schema.meta/brief.md"},
		{"block", l.BlockPath("atlas-7", "bankenstein", "schema"), "/home/u/.loom/worktrees/atlas-7/bankenstein/schema.meta/block.json"},
		{"branch", BranchName("atlas-7", "schema"), "loom/atlas-7/schema"},
		{"two runs of one manifest never collide", l.Dir("atlas-8", "bankenstein", "schema"), "/home/u/.loom/worktrees/atlas-8/bankenstein/schema"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
	// The meta dir must never be inside the worktree: §6.2's whole repair is that
	// Loom's files are out of the tree, and a path that starts with the worktree
	// would silently put them back in it.
	wt := l.Dir("atlas-7", "bankenstein", "schema")
	if md := l.MetaDir("atlas-7", "bankenstein", "schema"); strings.HasPrefix(md, wt+string(os.PathSeparator)) {
		t.Errorf("meta dir %q is inside the worktree %q", md, wt)
	}
}

func TestWorktreeCreate(t *testing.T) {
	repo := newScratchRepo(t)
	base, err := PinBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	w := newTestWorktrees(t)
	c := mustCreateWorktree(t, w, wtRequest(repo, base))

	if c.Branch != "loom/atlas-7/schema" {
		t.Errorf("branch = %q", c.Branch)
	}
	if c.Base != base {
		t.Errorf("base = %q, want %q", c.Base, base)
	}
	if c.Reused {
		t.Error("first Create reported Reused")
	}
	if c.Dir != physicalPath(w.Layout.Dir("atlas-7", "bankenstein", "schema")) {
		t.Errorf("Dir = %q, not the physically resolved layout path", c.Dir)
	}
	if got := mustGit(t, c.Dir, "symbolic-ref", "--short", "HEAD"); strings.TrimSpace(got) != c.Branch {
		t.Errorf("worktree HEAD = %q, want %q", strings.TrimSpace(got), c.Branch)
	}

	// The brief lands in the sibling meta dir, and NOT inside the worktree.
	if b, err := os.ReadFile(filepath.Join(c.MetaDir, "brief.md")); err != nil || !strings.Contains(string(b), "do the thing") {
		t.Fatalf("brief.md: %v / %q", err, b)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, ".loom")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("something Loom owns was written inside the worktree: %v", err)
	}

	// The verified fact this design is built on: <worktree>/.git is a FILE.
	fi, err := os.Lstat(filepath.Join(c.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.IsDir() {
		t.Error("<worktree>/.git is a directory — the whole meta-dir rationale assumed a file")
	}
}

// The load-bearing negative: a fresh worktree's tracked tree is EMPTY of Loom's
// files, and Loom has not written into the user's own repository beyond the git
// metadata §4.1 discloses. An info/exclude line per task per run was revision
// 1's design and is exactly what must not appear.
func TestWorktreeCreateWritesNothingIntoTheTree(t *testing.T) {
	repo := newScratchRepo(t)
	base, _ := PinBase(repo)
	w := newTestWorktrees(t)

	excludeBefore := readOrEmpty(filepath.Join(repo, ".git", "info", "exclude"))
	c := mustCreateWorktree(t, w, wtRequest(repo, base))

	if out := mustGit(t, c.Dir, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("fresh worktree is not clean:\n%s", out)
	}
	if out := mustGit(t, repo, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("the user's primary work tree was modified:\n%s", out)
	}
	if got := readOrEmpty(filepath.Join(repo, ".git", "info", "exclude")); got != excludeBefore {
		t.Errorf("main repo .git/info/exclude changed:\nbefore %q\nafter  %q", excludeBefore, got)
	}
}

func readOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestWorktreeCreateIdempotent(t *testing.T) {
	repo := newScratchRepo(t)
	base, _ := PinBase(repo)
	w := newTestWorktrees(t)

	first := mustCreateWorktree(t, w, wtRequest(repo, base))
	// A commit on the child's branch, so a re-create that silently recreated the
	// branch from base would lose work and this test would see it.
	writeFile(t, filepath.Join(first.Dir, "work.txt"), "child work\n")
	mustGit(t, first.Dir, "add", "work.txt")
	mustGit(t, first.Dir, "commit", "-qm", "child work")
	head := strings.TrimSpace(mustGit(t, first.Dir, "rev-parse", "HEAD"))

	second := mustCreateWorktree(t, w, wtRequest(repo, base))
	if !second.Reused {
		t.Error("re-create did not report Reused")
	}
	if got := strings.TrimSpace(mustGit(t, second.Dir, "rev-parse", "HEAD")); got != head {
		t.Errorf("re-create moved HEAD: %s → %s", head, got)
	}

	// …and again after the directory is removed but the branch survives, which is
	// §6.3's post-merge/post-discard shape.
	if err := w.Remove(repo, "atlas-7", "bankenstein", "schema", true); err != nil {
		t.Fatal(err)
	}
	third := mustCreateWorktree(t, w, wtRequest(repo, base))
	if !third.Reused {
		t.Error("re-create onto a surviving branch did not report Reused")
	}
	if got := strings.TrimSpace(mustGit(t, third.Dir, "rev-parse", "HEAD")); got != head {
		t.Errorf("re-create from the surviving branch lost the child's commit: %s → %s", head, got)
	}
}

func TestWorktreeCreateRefusals(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, w *Worktrees, repo string) Request
		wantErr error  // errors.Is, when there is a sentinel
		wantMsg string // substring, when the refusal is a plain error
	}{
		{
			name: "a live session already occupies the worktree",
			setup: func(t *testing.T, w *Worktrees, repo string) Request {
				base, _ := PinBase(repo)
				// Deliberately BEFORE the directory exists: this is the ordering
				// §6.2 step 3 demands, and it is what physicalPath has to survive.
				occupyWorktree(t, w.Store, "loom-a", w.Layout.Dir("atlas-7", "bankenstein", "schema"))
				return wtRequest(repo, base)
			},
			wantErr: ErrWorktreeOccupied,
		},
		{
			name: "the pinned base is not in this repo",
			setup: func(t *testing.T, w *Worktrees, repo string) Request {
				return wtRequest(repo, "0123456789012345678901234567890123456789")
			},
			wantMsg: "pinned base",
		},
		{
			name: "no pinned base at all",
			setup: func(t *testing.T, w *Worktrees, repo string) Request {
				return wtRequest(repo, "  ")
			},
			wantMsg: "no pinned base",
		},
		{
			name: "the path is occupied by something that is not a work tree",
			setup: func(t *testing.T, w *Worktrees, repo string) Request {
				base, _ := PinBase(repo)
				dir := w.Layout.Dir("atlas-7", "bankenstein", "schema")
				writeFile(t, filepath.Join(dir, "someone-elses.txt"), "x\n")
				return wtRequest(repo, base)
			},
			wantMsg: "not a git work tree",
		},
		{
			name: "the path is a work tree on the wrong branch",
			setup: func(t *testing.T, w *Worktrees, repo string) Request {
				base, _ := PinBase(repo)
				dir := w.Layout.Dir("atlas-7", "bankenstein", "schema")
				if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
					t.Fatal(err)
				}
				mustGit(t, repo, "worktree", "add", "-b", "somebody-else", dir, base)
				return wtRequest(repo, base)
			},
			wantMsg: "expected loom/atlas-7/schema",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newScratchRepo(t)
			w := newTestWorktrees(t)
			r := tt.setup(t, w, repo)
			_, err := w.Create(r)
			if err == nil {
				t.Fatal("Create succeeded, want a refusal")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

// §6.6: 3a runs at 3, and an unset cap must mean 3 rather than "unlimited" or
// "none".
func TestWorktreeConcurrencyCap(t *testing.T) {
	tests := []struct {
		name string
		cap  int
		want int
	}{
		{"unset means 3a's three", 0, Concurrency3a},
		{"negative means 3a's three", -4, Concurrency3a},
		{"one is legal", 1, 1},
		{"the default is legal", ConcurrencyDefault, ConcurrencyDefault},
		{"above the hard maximum is clamped", 99, ConcurrencyMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &Worktrees{Cap: tt.cap}
			if got := w.cap(); got != tt.want {
				t.Errorf("cap() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWorktreeCapEnforcedAtThree(t *testing.T) {
	repo := newScratchRepo(t)
	base, _ := PinBase(repo)
	w := newTestWorktrees(t)

	// Three tasks, each created and then occupied by a live child.
	for _, id := range []string{"one", "two", "three"} {
		r := wtRequest(repo, base)
		r.TaskID = id
		c := mustCreateWorktree(t, w, r)
		occupyWorktree(t, w.Store, "loom-"+id, c.Dir)
	}
	if n, err := w.LiveChildren("atlas-7"); err != nil || n != 3 {
		t.Fatalf("LiveChildren = %d, %v; want 3", n, err)
	}

	r := wtRequest(repo, base)
	r.TaskID = "four"
	if _, err := w.Create(r); !errors.Is(err, ErrCapReached) {
		t.Fatalf("fourth Create err = %v, want ErrCapReached", err)
	}
	// The refusal must name the numbers — "cap reached" with no counter is a
	// dead end for whoever reads it.
	_, err := w.Create(r)
	if !strings.Contains(err.Error(), "3/3") {
		t.Errorf("err = %v, want it to carry the counter", err)
	}
	// Nothing was created for the refused task: the cap is checked before any
	// side effect.
	if _, statErr := os.Stat(w.Layout.Dir("atlas-7", "bankenstein", "four")); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a capped Create left a worktree behind")
	}

	// A child ending frees its slot. This is the same event §6.3 calls a dead
	// child, seen from the cap's side.
	if err := w.Store.MarkEnded("loom-one", "done", 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Create(r); err != nil {
		t.Fatalf("Create after a child ended: %v", err)
	}

	// Another run's children do not count against this one's cap.
	other := wtRequest(repo, base)
	other.RunSlug, other.TaskID = "ballista-9", "one"
	if _, err := w.Create(other); err != nil {
		t.Fatalf("Create in a second run: %v", err)
	}
}

func TestWorktreeSeedFiles(t *testing.T) {
	repo := newScratchRepo(t)
	// A gitignored file that should be copied, a tracked file that must not be,
	// a symlink, and a file outside the repo.
	writeFile(t, filepath.Join(repo, ".env"), "SECRET=1\n")
	writeFile(t, filepath.Join(repo, "secrets", "key.pem"), "pem\n")
	writeFile(t, filepath.Join(repo, "tracked.txt"), "tracked\n")
	mustGit(t, repo, "add", "tracked.txt")
	mustGit(t, repo, "commit", "-qm", "tracked")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "outside\n")
	if err := os.Symlink(outside, filepath.Join(repo, ".env.link")); err != nil {
		t.Fatal(err)
	}

	base, _ := PinBase(repo)
	w := newTestWorktrees(t)
	r := wtRequest(repo, base)
	r.Setup = RepoSetup{SeedFiles: []string{
		".env", "secrets/key.pem", "tracked.txt", ".env.link", "../escape.txt", "missing.env",
	}}
	c := mustCreateWorktree(t, w, r)

	if want := []string{".env", filepath.Join("secrets", "key.pem")}; !sameStrings(c.Seeded, want) {
		t.Errorf("Seeded = %v, want %v", c.Seeded, want)
	}
	refused := map[string]string{}
	for _, e := range c.SeedRefused {
		refused[e.File] = e.Why
	}
	wantRefused := map[string]string{
		"tracked.txt":   "is tracked by git",
		".env.link":     "is a symlink",
		"../escape.txt": "escapes the repo",
		"missing.env":   "not present in the primary work tree",
	}
	for file, why := range wantRefused {
		if refused[file] != why {
			t.Errorf("refusal for %q = %q, want %q", file, refused[file], why)
		}
	}
	if len(refused) != len(wantRefused) {
		t.Errorf("refusals = %v, want exactly %v", refused, wantRefused)
	}
	// One bad entry did not drop the rest.
	if b, err := os.ReadFile(filepath.Join(c.Dir, ".env")); err != nil || string(b) != "SECRET=1\n" {
		t.Fatalf("seeded .env: %v / %q", err, b)
	}
	// And the seeds are gitignored, so the worktree's tracked tree is still clean.
	if out := mustGit(t, c.Dir, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("seeding dirtied the worktree:\n%s", out)
	}
	// A refused seed file must be legible on its own — this string is what the
	// spawn gate shows.
	e := &SeedFileError{File: "tracked.txt", Why: "is tracked by git"}
	if !strings.Contains(e.Error(), "tracked.txt") || !strings.Contains(e.Error(), "is tracked by git") {
		t.Errorf("SeedFileError.Error() = %q", e.Error())
	}
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestWorktreeBootstrap(t *testing.T) {
	repo := newScratchRepo(t)
	base, _ := PinBase(repo)

	t.Run("success", func(t *testing.T) {
		w := newTestWorktrees(t)
		r := wtRequest(repo, base)
		r.Setup = RepoSetup{Bootstrap: []string{"sh", "-c", "echo ok > bootstrapped.txt"}}
		c := mustCreateWorktree(t, w, r)
		if _, err := os.Stat(filepath.Join(c.Dir, "bootstrapped.txt")); err != nil {
			t.Fatalf("bootstrap did not run in the worktree: %v", err)
		}
	})

	t.Run("failure blocks the spawn, loudly, with output", func(t *testing.T) {
		w := newTestWorktrees(t)
		r := wtRequest(repo, base)
		r.TaskID = "boom"
		r.Setup = RepoSetup{Bootstrap: []string{"sh", "-c", "echo 'no such module'; exit 7"}}
		c, err := w.Create(r)
		var be *BootstrapError
		if !errors.As(err, &be) {
			t.Fatalf("err = %v, want a *BootstrapError", err)
		}
		if be.Exit != 7 || !strings.Contains(be.Output, "no such module") {
			t.Errorf("BootstrapError = %+v", be)
		}
		if !strings.Contains(be.Error(), "exit 7") {
			t.Errorf("Error() = %q, want the exit status in it", be.Error())
		}
		// The worktree is returned alongside the error rather than stranded
		// nameless.
		if c.Dir == "" {
			t.Error("Created.Dir empty after a failed bootstrap")
		}
	})

	t.Run("a missing command is an error, not a pass", func(t *testing.T) {
		w := newTestWorktrees(t)
		r := wtRequest(repo, base)
		r.TaskID = "nocmd"
		r.Setup = RepoSetup{Bootstrap: []string{"loom-no-such-binary-xyz"}}
		_, err := w.Create(r)
		var be *BootstrapError
		if !errors.As(err, &be) {
			t.Fatalf("err = %v, want a *BootstrapError", err)
		}
		if be.Output == "" {
			t.Error("a failed exec produced no output to render")
		}
	})

	t.Run("the environment is scrubbed of CLAUDE_CODE state", func(t *testing.T) {
		w := newTestWorktrees(t)
		w.Environ = []string{"PATH=" + os.Getenv("PATH"), "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "HOME=" + os.Getenv("HOME")}
		r := wtRequest(repo, base)
		r.TaskID = "env"
		r.Setup = RepoSetup{Bootstrap: []string{"sh", "-c", "env; exit 1"}}
		_, err := w.Create(r)
		var be *BootstrapError
		if !errors.As(err, &be) {
			t.Fatalf("err = %v, want a *BootstrapError", err)
		}
		for _, banned := range []string{"CLAUDECODE=", "CLAUDE_CODE_ENTRYPOINT="} {
			if strings.Contains(be.Output, banned) {
				t.Errorf("%s survived the scrub:\n%s", banned, be.Output)
			}
		}
		if !strings.Contains(be.Output, "LOOM_WORKTREE=") {
			t.Errorf("LOOM_WORKTREE missing from the bootstrap environment:\n%s", be.Output)
		}
	})
}

func TestWorktreeRemove(t *testing.T) {
	tests := []struct {
		name string
		// prepare mutates the created worktree before removal.
		prepare func(t *testing.T, w *Worktrees, repo string, c Created)
		force   bool
		// occupied marks the cases that deliberately leave a live session row
		// behind, so the shared tail does not assert a re-create that the
		// occupancy guard is correct to refuse.
		occupied bool
		wantErr  error
		wantMsg  string
	}{
		{
			name:  "a live worktree is removed and its branch kept",
			force: false,
		},
		{
			name: "a branch already merged into main removes cleanly",
			prepare: func(t *testing.T, w *Worktrees, repo string, c Created) {
				writeFile(t, filepath.Join(c.Dir, "work.txt"), "done\n")
				mustGit(t, c.Dir, "add", "work.txt")
				mustGit(t, c.Dir, "commit", "-qm", "work")
				mustGit(t, repo, "merge", "-q", "--no-ff", "-m", "merge", c.Branch)
			},
		},
		{
			name: "the child died — an ended session no longer holds the worktree",
			prepare: func(t *testing.T, w *Worktrees, repo string, c Created) {
				occupyWorktree(t, w.Store, "loom-dead", c.Dir)
				if err := w.Store.MarkEnded("loom-dead", "done", 0, 1); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "the user deleted the directory by hand",
			prepare: func(t *testing.T, w *Worktrees, repo string, c Created) {
				if err := os.RemoveAll(c.Dir); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a live child holds it — refused",
			prepare: func(t *testing.T, w *Worktrees, repo string, c Created) {
				occupyWorktree(t, w.Store, "loom-live", c.Dir)
			},
			wantErr: ErrWorktreeOccupied,
		},
		{
			name: "a live child holds it — force wins, because a discard is the human saying so",
			prepare: func(t *testing.T, w *Worktrees, repo string, c Created) {
				occupyWorktree(t, w.Store, "loom-live", c.Dir)
			},
			force:    true,
			occupied: true,
		},
		{
			name: "uncommitted work — refused without force",
			prepare: func(t *testing.T, w *Worktrees, repo string, c Created) {
				writeFile(t, filepath.Join(c.Dir, "wip.txt"), "half done\n")
			},
			wantMsg: "worktree remove",
		},
		{
			name: "uncommitted work — discarded with force",
			prepare: func(t *testing.T, w *Worktrees, repo string, c Created) {
				writeFile(t, filepath.Join(c.Dir, "wip.txt"), "half done\n")
			},
			force: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newScratchRepo(t)
			base, _ := PinBase(repo)
			w := newTestWorktrees(t)
			c := mustCreateWorktree(t, w, wtRequest(repo, base))
			if tt.prepare != nil {
				tt.prepare(t, w, repo, c)
			}

			err := w.Remove(repo, "atlas-7", "bankenstein", "schema", tt.force)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			case tt.wantMsg != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantMsg)
				}
				return
			case err != nil:
				t.Fatalf("Remove: %v", err)
			}

			// The directory is gone…
			if _, err := os.Stat(c.Dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("worktree dir survived: %v", err)
			}
			// …the branch is NOT, because a branch is the only durable record of
			// a discarded attempt and deleting one is irreversible…
			if !branchExists(repo, c.Branch) {
				t.Error("Remove deleted the branch")
			}
			// …the meta dir is not, because block.json and the brief are the only
			// record of what the child was told…
			if _, err := os.Stat(filepath.Join(c.MetaDir, "brief.md")); err != nil {
				t.Errorf("meta dir did not survive removal: %v", err)
			}
			// …and the repo is left in a state needing no hand repair: exactly one
			// work tree remains registered, so the next `worktree add` on this path
			// succeeds.
			if lines := worktreeCount(t, repo); lines != 1 {
				t.Errorf("`git worktree list` shows %d entries, want 1:\n%s", lines, mustGit(t, repo, "worktree", "list"))
			}
			if !tt.occupied {
				if _, err := w.Create(wtRequest(repo, base)); err != nil {
					t.Errorf("re-create after removal: %v", err)
				}
			}
		})
	}
}

func worktreeCount(t *testing.T, repo string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(mustGit(t, repo, "worktree", "list")), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// The stale administrative entry on its own: `git worktree list` advertising a
// directory that is not there is invisible from the filesystem and is what makes
// the next `worktree add` on that path fail.
func TestWorktreeRemovePrunesStaleEntry(t *testing.T) {
	repo := newScratchRepo(t)
	base, _ := PinBase(repo)
	w := newTestWorktrees(t)
	c := mustCreateWorktree(t, w, wtRequest(repo, base))

	if err := os.RemoveAll(c.Dir); err != nil {
		t.Fatal(err)
	}
	if got := worktreeCount(t, repo); got != 2 {
		t.Fatalf("expected a stale entry to still be listed, got %d entries", got)
	}
	if err := w.Remove(repo, "atlas-7", "bankenstein", "schema", false); err != nil {
		t.Fatalf("Remove on a vanished directory: %v", err)
	}
	if got := worktreeCount(t, repo); got != 1 {
		t.Errorf("stale entry survived Remove: %d entries", got)
	}
	// Idempotent: removing again is a no-op, not an error.
	if err := w.Remove(repo, "atlas-7", "bankenstein", "schema", false); err != nil {
		t.Errorf("second Remove: %v", err)
	}
	// And Create still works against the freed path — the repair is real.
	if _, err := w.Create(wtRequest(repo, base)); err != nil {
		t.Errorf("Create after the stale entry was pruned: %v", err)
	}
}

func TestWorktreeOccupant(t *testing.T) {
	repo := newScratchRepo(t)
	base, _ := PinBase(repo)
	w := newTestWorktrees(t)
	c := mustCreateWorktree(t, w, wtRequest(repo, base))

	if _, found, err := w.Occupant(c.Dir); err != nil || found {
		t.Fatalf("Occupant on an empty worktree = %v, %v", found, err)
	}
	occupyWorktree(t, w.Store, "loom-child", c.Dir)
	row, found, err := w.Occupant(c.Dir)
	if err != nil || !found || row.Name != "loom-child" {
		t.Fatalf("Occupant = %+v, %v, %v", row, found, err)
	}
	// An ended session does not occupy anything — §6.3's dead child.
	if err := w.Store.MarkEnded("loom-child", "done", 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := w.Occupant(c.Dir); found {
		t.Error("an ended session still counted as an occupant")
	}
}

// physicalPath is what makes the occupancy guard work at all on macOS, where the
// per-user temp dir hangs off /var → /private/var: the guard runs BEFORE the
// directory exists, and an unresolved path never equals the resolved cwd
// Launcher.Launch wrote.
func TestPhysicalPathResolvesPathsThatDoNotExistYet(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"an existing path is exactly physicalDir", link, resolvedReal},
		{"one missing segment", filepath.Join(link, "nope"), filepath.Join(resolvedReal, "nope")},
		{"several missing segments", filepath.Join(link, "a", "b", "c"), filepath.Join(resolvedReal, "a", "b", "c")},
		{"empty stays empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := physicalPath(tt.in); got != tt.want {
				t.Errorf("physicalPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPinBaseAndDirty(t *testing.T) {
	repo := newScratchRepo(t)
	base, err := PinBase(repo)
	if err != nil || len(base) != 40 {
		t.Fatalf("PinBase = %q, %v", base, err)
	}
	if Dirty(repo) {
		t.Error("a fresh repo reported dirty")
	}
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	if !Dirty(repo) {
		t.Error("a modified repo reported clean")
	}
	// Not a repo at all is NOT dirtiness: warning "this repo is dirty" about a
	// path that is not a repo points the human at the wrong problem.
	if Dirty(t.TempDir()) {
		t.Error("a non-repo reported dirty")
	}
	if _, err := PinBase(t.TempDir()); err == nil {
		t.Error("PinBase on a non-repo succeeded")
	}
}

func TestWorktreeCreateRefusesWithoutAStore(t *testing.T) {
	repo := newScratchRepo(t)
	base, _ := PinBase(repo)
	w := &Worktrees{Layout: NewLayout(t.TempDir())}
	if _, err := w.Create(wtRequest(repo, base)); err == nil {
		t.Fatal("Create without a Store succeeded — the occupancy guard was skipped silently")
	}
}

// --- failure-mode probes (slice 3a) ---------------------------------------

// The user deletes a worktree directory by hand. This is a supported thing to
// do to a window, and the failure it would otherwise cause is the nastiest
// shape in this file: the administrative entry survives in the USER'S repo, so
// `worktree add` then refuses both the path and the branch ("already checked
// out"), and the remedy is a git subcommand the user should never have to
// learn.
//
// Asserted end to end: the re-spawn succeeds, the child's committed work comes
// back with it (it lives in the shared object store on the branch, not in the
// directory), and the user's repo is left clean.
func TestWorktreeRecoversFromAHandDeletedDirectory(t *testing.T) {
	repo := newScratchRepo(t)
	w := newTestWorktrees(t)
	base, _ := PinBase(repo)

	c := mustCreateWorktree(t, w, wtRequest(repo, base))
	writeFile(t, filepath.Join(c.Dir, "db", "0007.sql"), "create table account;\n")
	mustGit(t, c.Dir, "add", "db/0007.sql")
	mustGit(t, c.Dir, "commit", "-qm", "child work")

	if err := os.RemoveAll(c.Dir); err != nil {
		t.Fatal(err)
	}

	again, err := w.Create(wtRequest(repo, base))
	if err != nil {
		t.Fatalf("re-create after a hand deletion: %v", err)
	}
	if !again.Reused {
		t.Error("Reused = false; the branch already existed and must not have been recreated from base")
	}
	if b, err := os.ReadFile(filepath.Join(again.Dir, "db", "0007.sql")); err != nil {
		t.Errorf("the child's committed work did not come back with the worktree: %v", err)
	} else if string(b) != "create table account;\n" {
		t.Errorf("recovered artifact = %q", b)
	}
	if got := mustGit(t, repo, "status", "--porcelain"); got != "" {
		t.Errorf("the user's repo is not clean after the recovery: %q", got)
	}
	if n := worktreeCount(t, repo); n != 2 {
		t.Errorf("worktree list has %d entries, want 2 (the repo and one worktree) — a stale "+
			"entry left behind is exactly what the user would have to repair by hand", n)
	}
}

// The user moves the repo while a worktree of it is live. Every linked worktree
// records an absolute gitdir, so the pair is broken until `git worktree repair`
// runs, and there is nothing Loom can do about it from the old path.
//
// What this pins is that the breakage is LOUD in every direction and that
// nothing is destroyed: no silent success, no half-removal, and the child's
// worktree directory and branch both survive so a repair recovers the work.
func TestWorktreeAfterTheRepoMovesFailsLoudly(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "-q", "-b", "main")
	mustGit(t, repo, "config", "user.email", "loom@example.test")
	mustGit(t, repo, "config", "user.name", "loom test")
	writeFile(t, filepath.Join(repo, "README.md"), "hello\n")
	mustGit(t, repo, "add", "README.md")
	mustGit(t, repo, "commit", "-qm", "init")

	w := newTestWorktrees(t)
	base, _ := PinBase(repo)
	c := mustCreateWorktree(t, w, wtRequest(repo, base))

	moved := filepath.Join(parent, "moved")
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Create(wtRequest(repo, base)); err == nil {
		t.Error("Create against the moved-away repo path succeeded")
	}
	if err := w.Remove(repo, "atlas-7", "bankenstein", "schema", false); err == nil {
		t.Error("Remove against the moved-away repo path succeeded — a silent no-op here would " +
			"report a cleaned-up run while the worktree is still registered")
	}
	if _, err := os.Stat(c.Dir); err != nil {
		t.Errorf("the child's worktree was destroyed by a failed operation: %v", err)
	}
	// Retrying against the NEW path is also refused, and loudly: the worktree's
	// .git file still points at the old gitdir. `git worktree repair` in the
	// moved repo is the remedy and Loom does not run it — see the handoff note.
	if _, err := w.Create(Request{RunSlug: "atlas-7", TaskID: "schema", RepoLabel: "bankenstein",
		RepoPath: moved, Base: base, Brief: "b"}); err == nil {
		t.Error("Create against the new repo path silently adopted a worktree whose gitdir is stale")
	}
	if !strings.Contains(mustGit(t, moved, "worktree", "list"), "schema") {
		t.Error("the moved repo no longer lists the worktree — the work is unreachable by repair")
	}
}

// §6.3's "child session died, work uncommitted" row, end to end. A worktree
// whose child died is NOT garbage: the session dying and the work being
// worthless are unrelated events.
//
// The assertions that matter are the ones about the USER'S repo, because that
// is the one thing Loom must never leave needing manual repair: an unforced
// removal refuses rather than discarding the uncommitted work, the forced
// removal leaves the branch and the .meta directory, and the repo ends clean
// with no stale worktree entry.
func TestWorktreeDeadChildKeepsItsWorkUntilAHumanDiscardsIt(t *testing.T) {
	repo := newScratchRepo(t)
	w := newTestWorktrees(t)
	base, _ := PinBase(repo)
	c := mustCreateWorktree(t, w, wtRequest(repo, base))

	// The half-written artifact a dead child leaves behind: on disk, never
	// committed. §8.3 calls that unpublished, and it is also what makes an
	// unforced removal refuse.
	writeFile(t, filepath.Join(c.Dir, "db", "0007.sql"), "-- half a migration\n")
	missing, err := Published(c.Dir, []Artifact{{ID: "account-schema", Path: "db/0007.sql"}})
	if err != nil || len(missing) != 1 {
		t.Fatalf("Published = %v, %v; an uncommitted artifact is unpublished", missing, err)
	}

	if err := w.Remove(repo, "atlas-7", "bankenstein", "schema", false); err == nil {
		t.Fatal("an unforced Remove discarded a dead child's uncommitted work")
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "db", "0007.sql")); err != nil {
		t.Fatalf("the refused removal took the work anyway: %v", err)
	}

	// The human discards it explicitly. Branch and .meta survive (§6.3): the
	// branch is the only durable record of a discarded attempt and deleting one
	// is the single irreversible act available in this design.
	if err := w.Remove(repo, "atlas-7", "bankenstein", "schema", true); err != nil {
		t.Fatalf("forced Remove: %v", err)
	}
	if _, err := os.Stat(c.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree still on disk after a forced removal: %v", err)
	}
	if !branchExists(repo, c.Branch) {
		t.Error("the branch was deleted — Loom never deletes a branch (§6.3)")
	}
	if _, err := os.Stat(filepath.Join(c.MetaDir, briefFile)); err != nil {
		t.Errorf(".meta/brief.md did not survive the removal: %v", err)
	}
	if got := mustGit(t, repo, "status", "--porcelain"); got != "" {
		t.Errorf("the user's repo is dirty after a removal: %q", got)
	}
	if n := worktreeCount(t, repo); n != 1 {
		t.Errorf("worktree list has %d entries after the removal, want 1 — a stale entry is "+
			"a repo the user has to repair by hand", n)
	}
}

// Two tasks of one run in ONE repo, with overlapping declared paths. §4.2 is
// explicit that `paths` is a detector and never the isolation mechanism —
// instruction-level file ownership measured BELOW the single-agent baseline
// where worktrees measured above it — so the property under test is that the
// worktree, and nothing else, keeps the two apart even when their declared
// paths overlap exactly.
func TestWorktreeSiblingTasksInOneRepoAreIsolatedByTheWorktreeAlone(t *testing.T) {
	repo := newScratchRepo(t)
	w := newTestWorktrees(t)
	base, _ := PinBase(repo)

	a := wtRequest(repo, base)
	b := wtRequest(repo, base)
	b.TaskID = "auth-api"
	ca := mustCreateWorktree(t, w, a)
	cb := mustCreateWorktree(t, w, b)

	if ca.Dir == cb.Dir || ca.Branch == cb.Branch {
		t.Fatalf("siblings collided: %+v vs %+v", ca, cb)
	}
	// The same declared path, written by both children.
	writeFile(t, filepath.Join(ca.Dir, "shared.go"), "package a\n")
	if _, err := os.Stat(filepath.Join(cb.Dir, "shared.go")); !errors.Is(err, os.ErrNotExist) {
		t.Error("one child's uncommitted write is visible in its sibling's worktree")
	}
	mustGit(t, ca.Dir, "add", "shared.go")
	mustGit(t, ca.Dir, "commit", "-qm", "a writes the shared file")
	if _, err := os.Stat(filepath.Join(cb.Dir, "shared.go")); !errors.Is(err, os.ErrNotExist) {
		t.Error("one child's COMMIT is visible in its sibling's worktree — the bases are not pinned apart")
	}
	if got := mustGit(t, repo, "status", "--porcelain"); got != "" {
		t.Errorf("a child's work reached the user's own work tree: %q", got)
	}
	if n := worktreeCount(t, repo); n != 3 {
		t.Errorf("worktree list has %d entries, want 3", n)
	}
}
