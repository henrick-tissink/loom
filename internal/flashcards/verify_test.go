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

// fakeClaudeFail exits non-zero — stands in for a child-process failure.
const fakeClaudeFail = `#!/bin/sh
cat >/dev/null
exit 1`

// fakeClaudeNonJSON emits non-empty, non-JSON output — triggers the verdict
// parse-error path.
const fakeClaudeNonJSON = `#!/bin/sh
cat >/dev/null
echo 'not a verdict at all'`

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

func TestVerifyFailsClosedOnChildError(t *testing.T) {
	c := store.Flashcard{Front: "q", Back: "a", SourceRef: "x"}
	v := &Verifier{Binary: fakeBin(t, fakeClaudeFail), WorkDir: t.TempDir()}
	ok, _, err := v.Verify(c, "src")
	if err == nil || ok {
		t.Fatalf("child failure must fail closed: ok=%v err=%v (want ok=false, err!=nil)", ok, err)
	}
}

func TestVerifyFailsClosedOnMalformedVerdict(t *testing.T) {
	c := store.Flashcard{Front: "q", Back: "a", SourceRef: "x"}
	v := &Verifier{Binary: fakeBin(t, fakeClaudeNonJSON), WorkDir: t.TempDir()}
	ok, _, err := v.Verify(c, "src")
	if err == nil || ok {
		t.Fatalf("malformed verdict must fail closed: ok=%v err=%v (want ok=false, err!=nil)", ok, err)
	}
}
