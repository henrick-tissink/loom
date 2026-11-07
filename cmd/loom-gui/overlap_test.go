package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
)

// overlapRepo is a repository with a.go and b.go committed and a claude
// worktree (branch worktree-w) beside the main checkout.
func overlapRepo(t *testing.T) (main, wt string) {
	t.Helper()
	main, _ = filepath.EvalSymlinks(t.TempDir())
	gitT(t, main, "init", "-q", "-b", "main")
	for _, f := range []string{"a.go", "b.go"} {
		os.WriteFile(filepath.Join(main, f), []byte("package a\n"), 0o644)
	}
	gitT(t, main, "add", ".")
	gitT(t, main, "commit", "-q", "-m", "base")
	wt = filepath.Join(main, ".claude", "worktrees", "w")
	gitT(t, main, "worktree", "add", "-q", "-b", "worktree-w", wt)
	return main, wt
}

// ovRow is a live row that edited the files just now: after any read the
// test makes, so a clean read does not forget them.
func ovRow(name, cwd string, edited ...string) status.Row {
	return ovRowAt(name, cwd, time.Now().Unix()+3600, edited...)
}

func ovRowAt(name, cwd string, at int64, edited ...string) status.Row {
	var es []transcript.Edit
	for _, p := range edited {
		es = append(es, transcript.Edit{Path: p, At: at})
	}
	return status.Row{SessionRow: store.SessionRow{Name: name, Cwd: cwd}, Edited: es}
}

// overlaps runs apply until the background git reads have landed (the
// first poll only starts them), and returns each row's overlaps. visible
// names the rows the rail shows; nil shows them all.
func overlaps(t *testing.T, o *overlapState, rows []status.Row, visible ...string) map[string][]OverlapDTO {
	t.Helper()
	dtos := func() []SessionDTO {
		var out []SessionDTO
		for _, r := range rows {
			if visible == nil || contains(visible, r.Name) {
				out = append(out, SessionDTO{Name: r.Name})
			}
		}
		return out
	}
	o.apply(dtos(), rows)
	o.wg.Wait()
	d := dtos()
	o.apply(d, rows)
	o.wg.Wait()
	got := map[string][]OverlapDTO{}
	for _, x := range d {
		if x.Overlaps != nil {
			got[x.Name] = x.Overlaps
		}
	}
	return got
}

func edit(t *testing.T, path string) string {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("package a // edited "+filepath.Base(path)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Two sessions in one checkout that both edited a still-uncommitted file
// overlap; a file only one of them edited, or one already committed, does not.
func TestOverlapSharedCheckout(t *testing.T) {
	main, _ := overlapRepo(t)
	a := edit(t, filepath.Join(main, "a.go"))
	b := filepath.Join(main, "b.go") // edited by both, then committed
	rows := []status.Row{
		ovRow("loom-1", main, a, b),
		ovRow("loom-2", main, a, b, edit(t, filepath.Join(main, "only2.go"))),
		ovRow("loom-3", main, filepath.Join(main, "b.go")),
	}
	got := overlaps(t, newOverlapState(time.Now), rows)
	want := map[string][]OverlapDTO{
		"loom-1": {{With: "loom-2", Files: []string{"a.go"}}},
		"loom-2": {{With: "loom-1", Files: []string{"a.go"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlaps = %+v\nwant %+v", got, want)
	}
}

// A worktree session owns its worktree: its branch's commits and its
// uncommitted files count, edits through Bash included, against a session
// editing the same file in the main checkout.
func TestOverlapWorktreeBranchAgainstMainCheckout(t *testing.T) {
	main, wt := overlapRepo(t)
	edit(t, filepath.Join(wt, "b.go"))
	gitT(t, wt, "commit", "-qam", "feat b")
	edit(t, filepath.Join(wt, "c.go")) // new, untracked, never seen in a transcript
	rows := []status.Row{
		ovRow("loom-wt", wt),
		ovRow("loom-main", main, edit(t, filepath.Join(main, "b.go")), edit(t, filepath.Join(main, "c.go"))),
	}
	got := overlaps(t, newOverlapState(time.Now), rows)
	want := map[string][]OverlapDTO{
		"loom-wt":   {{With: "loom-main", Files: []string{"b.go", "c.go"}}},
		"loom-main": {{With: "loom-wt", Files: []string{"b.go", "c.go"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlaps = %+v\nwant %+v", got, want)
	}
}

// Two sessions in one worktree share it: neither owns it, so only the
// files each edited count.
func TestOverlapSharedWorktreeUsesEdits(t *testing.T) {
	_, wt := overlapRepo(t)
	edit(t, filepath.Join(wt, "a.go"))
	rows := []status.Row{
		ovRow("loom-1", wt, edit(t, filepath.Join(wt, "b.go"))),
		ovRow("loom-2", wt),
	}
	if got := overlaps(t, newOverlapState(time.Now), rows); len(got) != 0 {
		t.Fatalf("overlaps in a shared worktree from files nobody edited: %+v", got)
	}
}

// A hidden project's session decides ownership but is never named.
// A session that edited a file and committed it is not named when
// another session changes the file later, until it edits the file again.
func TestOverlapForgetsACommittedEdit(t *testing.T) {
	main, _ := overlapRepo(t)
	a := filepath.Join(main, "a.go")
	now := time.Now()
	o := newOverlapState(func() time.Time { return now })
	early := ovRowAt("loom-1", main, now.Unix()-60, a) // edited, then committed: clean
	later := ovRow("loom-2", main)
	if got := overlaps(t, o, []status.Row{early, later}); len(got) != 0 {
		t.Fatalf("overlap on a clean file: %+v", got)
	}
	edit(t, a)
	later = ovRow("loom-2", main, a)
	now = now.Add(changesTTL)
	if got := overlaps(t, o, []status.Row{early, later}); len(got) != 0 {
		t.Fatalf("a committed edit still counts: %+v", got)
	}
	again := ovRowAt("loom-1", main, now.Unix()+1, a)
	if got := overlaps(t, o, []status.Row{again, later}); len(got["loom-1"]) != 1 {
		t.Fatalf("a fresh edit after the commit does not count: %+v", got)
	}
}

func TestOverlapNeverNamesAHiddenSession(t *testing.T) {
	main, _ := overlapRepo(t)
	a := edit(t, filepath.Join(main, "a.go"))
	rows := []status.Row{ovRow("loom-shown", main, a), ovRow("loom-hidden", main, a)}
	if got := overlaps(t, newOverlapState(time.Now), rows, "loom-shown"); len(got) != 0 {
		t.Fatalf("a hidden session showed up: %+v", got)
	}
}

func TestOverlapListsAtMostOverlapFiles(t *testing.T) {
	main, _ := overlapRepo(t)
	var files []string
	for i := 0; i < overlapFiles+3; i++ {
		files = append(files, edit(t, filepath.Join(main, "f", strings.Repeat("x", i+1)+".go")))
	}
	got := overlaps(t, newOverlapState(time.Now), []status.Row{ovRow("loom-1", main, files...), ovRow("loom-2", main, files...)})
	o := got["loom-1"]
	if len(o) != 1 || len(o[0].Files) != overlapFiles || o[0].More != 3 {
		t.Fatalf("overlap = %+v, want %d files and 3 more", o, overlapFiles)
	}
}

func TestOverlapStaleAnswerRefreshes(t *testing.T) {
	main, _ := overlapRepo(t)
	now := time.Now()
	o := newOverlapState(func() time.Time { return now })
	a := filepath.Join(main, "a.go")
	rows := []status.Row{ovRow("loom-1", main, a), ovRow("loom-2", main, a)}
	if got := overlaps(t, o, rows); len(got) != 0 {
		t.Fatalf("overlap on a clean file: %+v", got)
	}
	edit(t, a)
	if got := overlaps(t, o, rows); len(got) != 0 {
		t.Fatalf("answer refreshed inside the TTL: %+v", got)
	}
	now = now.Add(changesTTL)
	if got := overlaps(t, o, rows); len(got["loom-1"]) != 1 {
		t.Fatalf("answer not refreshed after the TTL: %+v", got)
	}
	now = now.Add(changesKeep + time.Second)
	o.prune()
	if len(o.entries) != 0 {
		t.Fatalf("entries kept past changesKeep: %d", len(o.entries))
	}
}

func TestNilOverlapStateIsANoop(t *testing.T) {
	var o *overlapState
	o.apply([]SessionDTO{{Name: "x"}}, nil)
}

func TestNudgePrompt(t *testing.T) {
	a := &App{}
	if _, err := a.NudgePrompt("loom-1"); err == nil {
		t.Fatal("a prompt for a session that is not spinning")
	}
	a.noteSpins([]SessionDTO{{Name: "loom-1", Spin: spinDTO(&transcript.Spin{Tool: "Bash", Count: 4,
		Sample: "exit 1 · --- FAIL: TestHedge (0.03s)", Subject: "go test ./vega/"})}, {Name: "loom-2"}})
	p, err := a.NudgePrompt("loom-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"same failure 4 times", "Bash: `go test ./vega/`", "--- FAIL: TestHedge", "What exactly failed", "repeating an approach", "tell me what is blocking you"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if _, err := a.NudgePrompt("loom-2"); err == nil {
		t.Fatal("a prompt for a session with no spin")
	}
	edit := nudgePrompt(SpinDTO{Tool: "Edit", Count: 3, Subject: "/r/a.go"})
	if !strings.Contains(edit, "(Edit on /r/a.go)") {
		t.Fatalf("file tool prompt: %s", edit)
	}
	if spinDTO(nil) != nil {
		t.Fatal("spinDTO(nil) != nil")
	}
}
