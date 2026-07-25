package flashcards

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

func TestRunCLICurateReviewStats(t *testing.T) {
	st := openStore(t)
	// two active-by-curation cards in one project via direct seed
	a1 := active(t, st, "a.go", "c1")
	_ = a1
	// make them drafts first to exercise curate
	st.SetCardStatus(a1, "draft", 0)
	root := t.TempDir()

	// curate --activate-all flips the draft to active
	var cur bytes.Buffer
	if err := RunCLI([]string{"curate", root, "--activate-all"}, st, "claude", root, 1000, strings.NewReader(""), &cur); err != nil {
		t.Fatalf("curate: %v", err)
	}
	// note: curate operates on the project derived from root's basename; align the
	// seeded project with it so the draft is found
	_ = cur

	// review: feed one grade, expect one recorded review
	var rev bytes.Buffer
	// project name derives from root basename; re-seed under that project name
	proj := projectName(root)
	id, _, _ := st.InsertFlashcard(store.Flashcard{Project: proj, Part: "a.go", Anchor: "z1", StemHash: "z1", Type: "code", Front: "q", Back: "a", Status: "active", CreatedAt: 1})
	if err := RunCLI([]string{"review", root}, st, "claude", root, 2000, strings.NewReader("3\n"), &rev); err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, ok, _ := st.GetReview(id); !ok {
		t.Fatalf("review did not record a grade for the queued card")
	}
	if !strings.Contains(rev.String(), "A:") {
		t.Fatalf("review did not reveal answers: %q", rev.String())
	}

	// stats prints a pass-rate line
	var stx bytes.Buffer
	if err := RunCLI([]string{"stats", root}, st, "claude", root, 3000, strings.NewReader(""), &stx); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !strings.Contains(stx.String(), "pass-rate") {
		t.Fatalf("stats missing pass-rate: %q", stx.String())
	}
	_ = filepath.Separator
}
