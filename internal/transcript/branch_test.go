package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastBranch(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := write("a.jsonl", `{"type":"user","gitBranch":"main"}
{"type":"assistant","gitBranch":"feat/a\"b"}
{"type":"ai-title","aiTitle":"x"}
`)
	if b, err := LastBranch(p); err != nil || b != `feat/a"b` {
		t.Fatalf("newest branch: %q %v", b, err)
	}
	// the newest branch sits behind a record bigger than the first window
	big := `{"type":"user","gitBranch":"worktree-loom-1"}` + "\n" + `{"type":"user","x":"` + strings.Repeat("y", 200<<10) + `"}` + "\n"
	if b, err := LastBranch(write("b.jsonl", big)); err != nil || b != "worktree-loom-1" {
		t.Fatalf("branch behind a large record: %q %v", b, err)
	}
	if b, err := LastBranch(write("c.jsonl", `{"type":"user"}`+"\n")); err != nil || b != "" {
		t.Fatalf("no branch recorded: %q %v", b, err)
	}
	// only the appended bytes are scanned
	if b, err := LastBranchSince(p, int64(len(`{"type":"user","gitBranch":"main"}`+"\n"))); err != nil || b != `feat/a"b` {
		t.Fatalf("since, branch after offset: %q %v", b, err)
	}
	if b, err := LastBranchSince(p, int64(len(`{"type":"user","gitBranch":"main"}`+"\n"+`{"type":"assistant","gitBranch":"feat/a\"b"}`+"\n"))); err != nil || b != "" {
		t.Fatalf("since, no branch after offset: %q %v", b, err)
	}
	if b, err := LastBranch(filepath.Join(dir, "missing.jsonl")); err != nil || b != "" {
		t.Fatalf("missing transcript: %q %v", b, err)
	}
}
