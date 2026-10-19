package taskfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `# Task
Session: abc
Started: 2026-10-14 14:40
Cwd: /w/loom
Status: open

## Outcome
The rail shows task status.
Second line of the same paragraph.

Not part of it.

## Scope
Steps 1-4.

## Done when
- [x] build: $ go build ./...  # ok
- [ ] tests: $ go test ./...  #
- [X] docs: updated
- [ ] <check name>: <the observation that proves it>

## Notes
`

func TestParseTask(t *testing.T) {
	tk, ok := ParseTask(sample)
	if !ok {
		t.Fatal("want ok")
	}
	if tk.Status != "open" || tk.Done != 2 || tk.Total != 3 {
		t.Fatalf("got %+v", tk)
	}
	if tk.Outcome != "The rail shows task status. Second line of the same paragraph." {
		t.Fatalf("outcome = %q", tk.Outcome)
	}
	if _, ok := ParseTask("   \n"); ok {
		t.Fatal("empty file must not parse")
	}
	if _, ok := ParseTask("Status: done\n"); ok {
		t.Fatal("a header without sections is not a task file")
	}
	// the template placeholder outcome reads as no outcome; an unknown
	// status reads as open, like the hooks assume
	tk, _ = ParseTask("Status: weird\n\n## Outcome\n(the behaviour or deliverable)\n")
	if tk.Status != "open" || tk.Outcome != "" {
		t.Fatalf("got %+v", tk)
	}
	tk, _ = ParseTask("Status: Consultation\n\n## Outcome\nx\n")
	if tk.Status != "consultation" {
		t.Fatalf("got %+v", tk)
	}
}

func TestParseCheckpoint(t *testing.T) {
	got := ParseCheckpoint("# Checkpoint\n\n## State\nfine\n\n## Next step\ngit commit + push;\nthen report.\n\n## Open blockers\nNone\n")
	if got != "git commit + push; then report." {
		t.Fatalf("got %q", got)
	}
	if ParseCheckpoint("# Checkpoint\n## State\nx\n") != "" {
		t.Fatal("no next step section → empty")
	}
}

func TestCacheReparsesOnChangeOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "checkpoints"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewCache(dir)
	if _, ok := c.Task("sid"); ok {
		t.Fatal("missing file must read as absent")
	}
	p := filepath.Join(dir, "tasks", "sid.md")
	if err := os.WriteFile(p, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	tk, ok := c.Task("sid")
	if !ok || tk.Done != 2 {
		t.Fatalf("got %+v %v", tk, ok)
	}
	// same size + mtime → cached value; a rewrite with a new mtime → reparsed
	old := time.Now().Add(-time.Hour)
	done := []byte(sample[:len(sample)-len("## Notes\n")] + "## Notes\nx\n")
	if err := os.WriteFile(p, done, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if tk, _ := c.Task("sid"); tk.Done != 2 {
		t.Fatalf("got %+v", tk)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Task("sid"); ok {
		t.Fatal("a removed file must read as absent again")
	}

	cp := filepath.Join(dir, "checkpoints", "sid.md")
	if err := os.WriteFile(cp, []byte("# Checkpoint\n\n## Next step\nhand back\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cp, old, old); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Checkpoint("sid")
	if !ok || got.NextStep != "hand back" || got.WrittenAt != old.Unix() {
		t.Fatalf("got %+v %v", got, ok)
	}
	if _, ok := c.Checkpoint("nope"); ok {
		t.Fatal("missing checkpoint must read as absent")
	}
}

func TestStartedAtAndVerifiedMarker(t *testing.T) {
	tk, _ := ParseTask("Session: s\nStarted: 2026-10-16 20:49\nStatus: open\n\n## Outcome\nx\n")
	want := time.Date(2026, 10, 16, 20, 49, 0, 0, time.Local).Unix()
	if tk.StartedAt != want {
		t.Fatalf("started %d, want %d", tk.StartedAt, want)
	}
	if tk, _ := ParseTask("Started: yesterday\n\n## Outcome\nx\n"); tk.StartedAt != 0 {
		t.Fatalf("unparsable start must be 0, got %d", tk.StartedAt)
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "tasks", "sid.md")
	if err := os.WriteFile(p, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCache(dir)
	if tk, _ := c.Task("sid"); tk.Verified {
		t.Fatal("no marker: must not be verified")
	}
	fi, _ := os.Stat(p)
	mtime := float64(fi.ModTime().UnixNano()) / 1e9
	// The guard writes repr(st_mtime) of the version it checked.
	if err := os.WriteFile(p+".verified", []byte(fmt.Sprintf("%.6f\n", mtime)), 0o644); err != nil {
		t.Fatal(err)
	}
	if tk, _ := c.Task("sid"); !tk.Verified {
		t.Fatal("marker at the file's mtime: must be verified, even though the task file itself is cached")
	}
	// A later edit of the task file outdates the marker.
	later := fi.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	if tk, _ := c.Task("sid"); tk.Verified {
		t.Fatal("edited after the marker: must not be verified")
	}
	if err := os.WriteFile(p+".verified", []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tk, _ := c.Task("sid"); tk.Verified {
		t.Fatal("junk marker must read as unverified")
	}
}

func TestGoalCondition(t *testing.T) {
	tk, _ := ParseTask(sample)
	if len(tk.Checks) != 3 || tk.Open[0] || !tk.Open[1] {
		t.Fatalf("checks %q open %v", tk.Checks, tk.Open)
	}
	got, ok := GoalCondition(tk, "/s/tasks/x.md", 5)
	want := "(1) build: $ go build ./...; (2) tests: $ go test ./...; (3) docs: updated; and that file says Status: done, or stop after 5 turns"
	if !ok || !strings.HasPrefix(got, "every Done-when check of the task file /s/tasks/x.md holds,") || !strings.HasSuffix(got, want) || strings.Contains(got, "\n") {
		t.Fatalf("condition %q", got)
	}
	// too long verbatim: the checks are named, and it fits
	long := tk
	long.Checks = append([]string(nil), tk.Checks...)
	long.Checks[1] = "tests: $ " + strings.Repeat("go test ./x/... && ", 40)
	got, ok = GoalCondition(long, "/s/tasks/x.md", 5)
	if !ok || len("/goal ")+len(got) > GoalMaxLen || !strings.Contains(got, "(1) build; (2) tests; (3) docs; and that file") {
		t.Fatalf("named form %v %d %q", ok, len(got), got)
	}
	many := Task{}
	for i := 0; i < 60; i++ {
		many.Checks = append(many.Checks, "a check with a long name: $ true")
		many.Open = append(many.Open, true)
	}
	if _, ok := GoalCondition(many, "/s/tasks/x.md", 5); ok {
		t.Fatal("60 checks fit")
	}
}
