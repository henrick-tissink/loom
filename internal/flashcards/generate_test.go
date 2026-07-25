package flashcards

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeClaudeCards echoes a fixed cards JSON payload, ignoring stdin — stands in
// for the real `claude -p` author pass.
const fakeClaudeCards = `#!/bin/sh
cat >/dev/null
cat <<'JSON'
{"cards":[
 {"type":"code","front":"What does Fuse return when the pane is active?","back":"Running, in every branch.","source_ref":"internal/status/status.go"},
 {"type":"bogus","front":"bad","back":"bad","source_ref":"x"}
]}
JSON`

func fakeBin(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-claude.sh")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGenerateParsesValidatesAndHashes(t *testing.T) {
	g := &Generator{Binary: fakeBin(t, fakeClaudeCards), WorkDir: t.TempDir()}
	p := Part{Kind: PartCode, ID: "internal/status/status.go", Title: "status.go",
		SourceRef: "internal/status/status.go", Source: "func Fuse() int { return 1 }"}
	cards, err := g.Generate("loom", p, 100)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(cards) != 1 { // the "bogus" type card is dropped by validation
		t.Fatalf("cards = %d, want 1 (invalid type dropped)", len(cards))
	}
	c := cards[0]
	if c.Status != "draft" || c.Project != "loom" || c.Type != "code" {
		t.Fatalf("bad card: %+v", c)
	}
	if c.Anchor != "loom|code|internal/status/status.go" || c.StemHash == "" || c.AnswerHash == "" || c.SourceHash == "" {
		t.Fatalf("missing keys/hashes: %+v", c)
	}
}
