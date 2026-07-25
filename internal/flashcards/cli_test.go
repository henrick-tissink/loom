package flashcards

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func TestRunCLIGeneratesVerifiesStores(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "status.go"), "package status\nfunc Fuse() int { return 1 }\n")

	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Author emits one valid code card; verifier accepts it.
	binGen := fakeBin(t, fakeClaudeCards)      // from generate_test.go
	binVer := fakeBin(t, fakeClaudeVerdictYes) // from verify_test.go
	pl := &Pipeline{
		Store: st,
		Gen:   &Generator{Binary: binGen, WorkDir: root},
		Ver:   &Verifier{Binary: binVer, WorkDir: root},
	}
	// drive one code part directly
	parts, _ := BuildManifest(root)
	var code Part
	for _, p := range parts {
		if p.Kind == PartCode {
			code = p
			break
		}
	}
	stored, rejected, err := pl.GenerateForPart("loom", code, 100)
	if err != nil || stored != 1 || rejected != 0 {
		t.Fatalf("GenerateForPart: stored=%d rejected=%d err=%v", stored, rejected, err)
	}
	rows, _ := st.FlashcardsForProject("loom")
	if len(rows) != 1 || rows[0].Status != "draft" {
		t.Fatalf("want 1 draft row, got %+v", rows)
	}
}

// fakeClaudeGenAndVerify branches on stdin so ONE fake binary can serve both of
// RunCLI's passes: a verify pass (stdin carries PROPOSED_ANSWER) gets a positive
// verdict; anything else (the author pass) gets one code card. This lets the
// single-binary pipeline actually store a verified card end-to-end.
const fakeClaudeGenAndVerify = `#!/bin/sh
in=$(cat)
case "$in" in
  *PROPOSED_ANSWER*) echo '{"correct":true,"reason":"ok"}' ;;
  *) echo '{"cards":[{"type":"code","front":"What does Fuse return when the pane is active?","back":"Running.","source_ref":"ignored"}]}' ;;
esac`

func TestRunCLIStoresThenIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "status.go"), "package status\nfunc Fuse() int { return 1 }\n")
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bin := fakeBin(t, fakeClaudeGenAndVerify)
	project := projectName(root)

	var buf1 bytes.Buffer
	if err := RunCLI([]string{"generate", root}, st, bin, root, 100, strings.NewReader(""), &buf1); err != nil {
		t.Fatalf("RunCLI run 1: %v", err)
	}
	after1, _ := st.FlashcardsForProject(project)
	if len(after1) != 1 {
		t.Fatalf("run 1 stored %d cards, want 1: report=%q", len(after1), buf1.String())
	}

	var buf2 bytes.Buffer
	if err := RunCLI([]string{"generate", root}, st, bin, root, 200, strings.NewReader(""), &buf2); err != nil {
		t.Fatalf("RunCLI run 2: %v", err)
	}
	after2, _ := st.FlashcardsForProject(project)
	if len(after2) != 1 {
		t.Fatalf("run 2 must be idempotent (dedup): now %d cards, want 1", len(after2))
	}
	if !contains(buf2.String(), "stored=0") {
		t.Fatalf("run 2 report should show stored=0 (all deduped): %q", buf2.String())
	}
}
