package flashcards

import (
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

const fakeClaudeVerdictYes = `#!/bin/sh
cat >/dev/null
echo '{"correct":true,"reason":"matches the source"}'`

const fakeClaudeVerdictNo = `#!/bin/sh
cat >/dev/null
echo '{"correct":false,"reason":"source says otherwise"}'`

func TestVerifyAcceptsAndRejects(t *testing.T) {
	c := store.Flashcard{Front: "What does Fuse return when the pane is active?", Back: "Running.", SourceRef: "status.go"}
	src := "func Fuse(...) { if active { return Running } }"

	vYes := &Verifier{Binary: fakeBin(t, fakeClaudeVerdictYes), WorkDir: t.TempDir()}
	if ok, _, err := vYes.Verify(c, src); err != nil || !ok {
		t.Fatalf("expected accept: ok=%v err=%v", ok, err)
	}
	vNo := &Verifier{Binary: fakeBin(t, fakeClaudeVerdictNo), WorkDir: t.TempDir()}
	if ok, reason, err := vNo.Verify(c, src); err != nil || ok || reason == "" {
		t.Fatalf("expected reject with reason: ok=%v reason=%q err=%v", ok, reason, err)
	}
}
