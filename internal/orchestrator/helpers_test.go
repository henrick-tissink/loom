package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
)

// fixedNow is the clock every assembly test runs on. §5.3's determinism claim
// is only assertable against a fixed clock, and the brief stamps a timestamp.
var fixedNow = time.Unix(1753200000, 0).UTC()

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// gitRepo builds a real one-commit repository under t.TempDir(). Scratch repos
// only: the house rules forbid running a mutating git command against the loom
// repo itself, and every git-touching test here builds its own tree.
func gitRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "one")
	return strings.TrimSpace(run("rev-parse", "HEAD"))
}

func gitCommit(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", dir, "add", ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", dir, "commit", "-qm", name)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// fakeGit answers the three commands assembly runs, from a table, so budget and
// determinism cases do not each need a real repository.
type fakeGit struct {
	branch map[string]string
	head   map[string]string
	status map[string]string
	known  map[string]bool // "<dir>\x00<sha>" the repo can resolve
	count  map[string]string
	fail   map[string]bool
}

func (f fakeGit) run(dir string, args ...string) (string, error) {
	if f.fail[dir] {
		return "", os.ErrNotExist
	}
	switch {
	case len(args) == 3 && args[0] == "rev-parse" && args[1] == "--abbrev-ref":
		return f.branch[dir] + "\n", nil
	case len(args) == 2 && args[0] == "rev-parse":
		return f.head[dir] + "\n", nil
	case len(args) == 2 && args[0] == "status":
		return f.status[dir], nil
	case len(args) == 3 && args[0] == "cat-file":
		sha := strings.TrimSuffix(args[2], "^{commit}")
		if f.known[dir+"\x00"+sha] {
			return "", nil
		}
		return "", os.ErrNotExist
	case len(args) == 3 && args[0] == "rev-list":
		return f.count[dir+"\x00"+args[2]], nil
	}
	return "", os.ErrNotExist
}

// fakeLauncher records what Spawn asked for. It also keeps the argv the recipe
// would produce, because §13 requires the permission mode to be asserted on the
// EXACT argv rather than on the recipe field that feeds it — the whole point of
// the revision-1 bug is that a field can look right and produce no flag.
type fakeLauncher struct {
	recipes []session.Recipe
	argvs   [][]string
	err     error
	n       int
}

func (l *fakeLauncher) Launch(r session.Recipe, w, h int, now time.Time) (string, error) {
	if l.err != nil {
		return "", l.err
	}
	l.n++
	id := "sess" + string(rune('0'+l.n))
	l.recipes = append(l.recipes, r)
	l.argvs = append(l.argvs, r.Argv(id))
	return session.TmuxName(id), nil
}

// svc wires a Service against a temp ~/.loom, a fixed clock, and a git runner.
func svc(t *testing.T, st *store.Store, l Launcher, g gitRunner) (*Service, string) {
	t.Helper()
	loomDir := t.TempDir()
	s := New(st, l, loomDir)
	s.now = func() time.Time { return fixedNow }
	if g != nil {
		s.git = g
	}
	return s, loomDir
}

func mustProject(t *testing.T, st *store.Store, root, name string) {
	t.Helper()
	if err := st.UpsertProject(store.Project{
		Root: root, Name: name, Origin: "discovered",
		CreatedAt: fixedNow.Unix(), UpdatedAt: fixedNow.Unix(),
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
}

func mustRepo(t *testing.T, st *store.Store, path, root, label string) {
	t.Helper()
	if err := st.UpsertProjectRepo(store.ProjectRepo{
		Path: path, ProjectRoot: root, Label: label, AddedAt: fixedNow.Unix(),
	}); err != nil {
		t.Fatalf("UpsertProjectRepo: %v", err)
	}
}

// mustIndexedSession writes the pair the echo-chamber guard joins across: a
// sessions row (which carries the tag) and a transcripts row (which carries the
// outcome the brief would quote). transcripts.session_id == sessions.
// claude_session_id is the join §5.4 names.
func mustIndexedSession(t *testing.T, st *store.Store, id, dir, title, outcome, tags string, ts int64) {
	t.Helper()
	if err := st.Upsert(store.SessionRow{
		Name: "loom-" + id, ClaudeSessionID: id, ProjectLabel: "x", Cwd: dir,
		Tags: tags, CreatedAt: ts, EndedAt: ts, ExitCode: 0, LastStatus: "done",
	}); err != nil {
		t.Fatalf("Upsert session: %v", err)
	}
	if err := st.UpsertTranscript(store.Transcript{
		SessionID: id, ProjectDir: dir, Cwd: dir, Title: title,
		FirstTS: ts, LastTS: ts, MsgCount: 4, Outcome: outcome,
	}); err != nil {
		t.Fatalf("UpsertTranscript: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// mustIndexDocs puts real rows into messages_fts so the RANKED path
// (memory.Related → SearchSessionsRaw) actually ranks. Without docs the recall
// query finds nothing and Related falls back to recency — which would make an
// "intent present ⇒ ranked path" test pass while proving nothing.
func mustIndexDocs(t *testing.T, st *store.Store, id, content string, ts int64) {
	t.Helper()
	if err := st.ReplaceFileDocs(
		store.IndexedFile{Path: "/tmp/" + id + ".jsonl", SessionID: id, Size: 1, Mtime: ts},
		[]store.Doc{{Content: content, Role: "user", TS: ts}},
	); err != nil {
		t.Fatalf("ReplaceFileDocs: %v", err)
	}
}
