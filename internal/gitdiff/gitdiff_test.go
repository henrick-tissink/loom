package gitdiff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run fails the test on a non-zero git; every scratch repo lives under
// t.TempDir(), never the loom checkout.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "t@t")
	run(t, dir, "config", "user.name", "t")
	run(t, dir, "commit", "--allow-empty", "-q", "-m", "base")
	return dir
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", msg)
	return run(t, dir, "rev-parse", "HEAD")
}

// A path that is not a repo — and an empty path — must degrade to an Error
// rather than crash or claim a clean tree.
func TestDiffCapture_notARepo(t *testing.T) {
	for _, tc := range []struct{ name, dir string }{
		{"plain directory", t.TempDir()},
		{"empty", ""},
		{"missing", filepath.Join(t.TempDir(), "nope")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := WorkingTree(tc.dir)
			if got.Error == "" {
				t.Fatalf("want an error, got %+v", got)
			}
			if got.Patch != "" || got.Dirty || len(got.Files) > 0 {
				t.Fatalf("error capture must carry nothing else: %+v", got)
			}
		})
	}
}

func TestDiffWorkingTree(t *testing.T) {
	dir := initRepo(t)
	if got := WorkingTree(dir); got.Error != "" || got.Dirty {
		t.Fatalf("clean repo: %+v", got)
	}

	write(t, dir, "a.txt", "hello\n")
	commit(t, dir, "add a")
	write(t, dir, "a.txt", "hello world\n")
	write(t, dir, "new.txt", "x\n")

	got := WorkingTree(dir)
	if got.Error != "" || !got.Dirty {
		t.Fatalf("dirty repo: %+v", got)
	}
	if got.Stat == "" || !strings.Contains(got.Patch, "hello world") {
		t.Fatalf("want stat+patch for the tracked change: %+v", got)
	}
	if len(got.Files) != 1 || got.Files[0] != "a.txt" {
		t.Errorf("Files = %v, want [a.txt]", got.Files)
	}
	if len(got.Untracked) != 1 || got.Untracked[0] != "new.txt" {
		t.Errorf("Untracked = %v, want [new.txt]", got.Untracked)
	}
}

// A repo with no commit yet still reports its untracked files: `git diff HEAD`
// fails there, and swallowing that failure in working-tree mode is deliberate.
func TestDiffWorkingTree_unbornHEAD(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "new.txt", "x\n")

	got := WorkingTree(dir)
	if got.Error != "" {
		t.Fatalf("unborn HEAD must not become an error banner: %+v", got)
	}
	if !got.Dirty || len(got.Untracked) != 1 {
		t.Fatalf("want the untracked file: %+v", got)
	}
}

// Base-relative mode is what the merge gate reads: what this branch did since
// it forked, and nothing the base's own line did afterwards.
func TestDiffSinceBase(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "shared.txt", "v1\n")
	base := commit(t, dir, "base content")

	run(t, dir, "checkout", "-q", "-b", "loom/run/task")
	write(t, dir, "child.txt", "child\n")
	commit(t, dir, "child work")

	// The base's line moves on after the fork. A two-dot diff would attribute
	// this, inverted, to the child.
	run(t, dir, "checkout", "-q", "main")
	write(t, dir, "elsewhere.txt", "moved on\n")
	commit(t, dir, "unrelated work on main")
	run(t, dir, "checkout", "-q", "loom/run/task")

	got := SinceBase(dir, base, "loom/run/task")
	if got.Error != "" {
		t.Fatalf("unexpected error: %+v", got)
	}
	if len(got.Files) != 1 || got.Files[0] != "child.txt" {
		t.Fatalf("Files = %v, want [child.txt] only", got.Files)
	}
	if strings.Contains(got.Patch, "elsewhere.txt") {
		t.Errorf("base-relative diff leaked the base line's own work:\n%s", got.Patch)
	}
	if !got.Dirty || got.Stat == "" {
		t.Errorf("committed branch work must show: %+v", got)
	}

	// Empty Ref means HEAD, which is the same branch here.
	if h := SinceBase(dir, base, ""); len(h.Files) != 1 || h.Files[0] != "child.txt" {
		t.Errorf("empty ref should mean HEAD, got %+v", h)
	}

	// Working-tree mode over the same repo sees nothing: everything is
	// committed. That is exactly why the merge gate cannot use it.
	if w := WorkingTree(dir); w.Dirty {
		t.Errorf("working-tree mode should be clean here: %+v", w)
	}
}

// An unresolvable base must be LOUD. Rendering "no changes" at a merge gate
// because the sha was wrong is the silent failure this codebase refuses.
func TestDiffSinceBase_badBase(t *testing.T) {
	dir := initRepo(t)
	got := SinceBase(dir, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "HEAD")
	if got.Error == "" {
		t.Fatalf("an unknown base must report an error, got %+v", got)
	}
	if got.Dirty || len(got.Files) > 0 {
		t.Fatalf("failed capture must carry nothing: %+v", got)
	}
}

// git quotes non-ASCII paths by default; a quoted path matches no declared
// glob and would show up as a phantom divergence.
func TestDiffCapture_nonASCIIPathsUnquoted(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "internal/café/ünïcode.go", "package x\n")
	got := WorkingTree(dir)
	if len(got.Untracked) != 1 || got.Untracked[0] != "internal/café/ünïcode.go" {
		t.Fatalf("Untracked = %q, want the raw path", got.Untracked)
	}

	commit(t, dir, "add unicode")
	base := run(t, dir, "rev-parse", "HEAD~0")
	write(t, dir, "internal/café/ünïcode.go", "package x\n\n// more\n")
	commit(t, dir, "touch unicode")
	if got := SinceBase(dir, base+"~1", "HEAD"); !contains(got.Files, "internal/café/ünïcode.go") {
		t.Fatalf("Files = %q, want the raw path", got.Files)
	}
}
