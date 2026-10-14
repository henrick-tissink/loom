package taskfile

import (
	"os"
	"path/filepath"
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
