package flashcards

import (
	"bytes"
	"path/filepath"
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

	// RunCLI over the whole project prints a report and is idempotent (dedup).
	var buf bytes.Buffer
	if err := RunCLI([]string{"generate", root}, st, binGen, root, 200, &buf); err != nil {
		t.Fatalf("RunCLI: %v", err)
	}
	if !contains(buf.String(), "stored") {
		t.Fatalf("report missing summary: %q", buf.String())
	}
}
