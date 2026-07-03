package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// buildArchive lays out a fake $CLAUDE_CONFIG_DIR/projects tree:
//   - projDir1 ("-w-loom1", cwd /w/loom1): main session "s1" + a subagent
//     directory "s1/subagents/agent-a.jsonl" that touches internal/x.go.
//   - projDir2 ("-w-loom2", cwd /w/loom2): main session "s2" only, no subagent.
//
// Returns the claudeConfigDir root and the two main file paths.
func buildArchive(t *testing.T) (ccd, mainPath1, mainPath2 string) {
	t.Helper()
	root := t.TempDir()
	projectsDir := filepath.Join(root, "projects")

	projDir1 := filepath.Join(projectsDir, "-w-loom1")
	if err := os.MkdirAll(projDir1, 0o755); err != nil {
		t.Fatal(err)
	}
	mainPath1 = filepath.Join(projDir1, "s1.jsonl")
	mainLines := []string{
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"please refactor the auth module"},"uuid":"u1","timestamp":"2026-07-02T12:00:00.000Z","cwd":"/w/loom1","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"internal/auth.go","old_string":"x","new_string":"y"}},{"type":"text","text":"done, refactored auth.go"}]},"uuid":"a1","timestamp":"2026-07-02T12:01:00.000Z","cwd":"/w/loom1","sessionId":"s1"}`,
		`{"type":"ai-title","aiTitle":"Refactor auth module","isSidechain":false,"sessionId":"s1"}`,
	}
	if err := os.WriteFile(mainPath1, []byte(strings.Join(mainLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	subDir := filepath.Join(projDir1, "s1", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subPath := filepath.Join(subDir, "agent-a.jsonl")
	subLines := []string{
		`{"isSidechain":true,"agentId":"agent1","type":"user","message":{"role":"user","content":"investigate the quantum flux capacitor bug in x.go"},"uuid":"su1","timestamp":"2026-07-02T12:02:00.000Z","cwd":"/w/loom1","sessionId":"s1"}`,
		`{"isSidechain":true,"agentId":"agent1","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"internal/x.go","old_string":"a","new_string":"b"}},{"type":"text","text":"fixed the quantum flux capacitor issue in x.go"}]},"uuid":"sa1","timestamp":"2026-07-02T12:03:00.000Z","cwd":"/w/loom1","sessionId":"s1"}`,
	}
	if err := os.WriteFile(subPath, []byte(strings.Join(subLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	projDir2 := filepath.Join(projectsDir, "-w-loom2")
	if err := os.MkdirAll(projDir2, 0o755); err != nil {
		t.Fatal(err)
	}
	mainPath2 = filepath.Join(projDir2, "s2.jsonl")
	main2Lines := []string{
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"unrelated second session prompt"},"uuid":"u2","timestamp":"2026-07-02T13:00:00.000Z","cwd":"/w/loom2","sessionId":"s2"}`,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"unrelated second session reply"}]},"uuid":"a2","timestamp":"2026-07-02T13:01:00.000Z","cwd":"/w/loom2","sessionId":"s2"}`,
	}
	if err := os.WriteFile(mainPath2, []byte(strings.Join(main2Lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return root, mainPath1, mainPath2
}

func TestSweepIndexesMainAndSubagentAcrossProjectDirs(t *testing.T) {
	st := openStore(t)
	ccd, _, _ := buildArchive(t)
	ix := NewIndexer(st, ccd)

	if err := ix.Sweep(); err != nil {
		t.Fatal(err)
	}

	tr, ok, err := st.GetTranscript("s1")
	if err != nil || !ok {
		t.Fatalf("GetTranscript(s1): ok=%v err=%v", ok, err)
	}
	if tr.Title != "Refactor auth module" {
		t.Errorf("Title = %q", tr.Title)
	}
	if tr.Ask != "please refactor the auth module" {
		t.Errorf("Ask = %q", tr.Ask)
	}
	if tr.Outcome != "done, refactored auth.go" {
		t.Errorf("Outcome = %q", tr.Outcome)
	}
	if tr.Cwd != "/w/loom1" {
		t.Errorf("Cwd = %q, want /w/loom1", tr.Cwd)
	}
	if tr.ProjectDir != "-w-loom1" {
		t.Errorf("ProjectDir = %q", tr.ProjectDir)
	}
	if tr.FirstTS == 0 || tr.LastTS == 0 {
		t.Errorf("FirstTS/LastTS not set: %+v", tr)
	}
	// LastTS must extend to cover the subagent file's later timestamp (12:03)
	// even though main file's own last record is at 12:01.
	wantLastTS := int64(1782993780) // 2026-07-02T12:03:00Z
	if tr.LastTS != wantLastTS {
		t.Errorf("LastTS = %d, want %d (extended by subagent)", tr.LastTS, wantLastTS)
	}
	// MsgCount = main-file docs only (2: 1 user + 1 assistant text) — the
	// subagent's 2 docs must NOT inflate this.
	if tr.MsgCount != 2 {
		t.Errorf("MsgCount = %d, want 2 (main-file docs only)", tr.MsgCount)
	}
	wantFiles := "internal/auth.go\ninternal/x.go"
	if tr.Files != wantFiles {
		t.Errorf("Files = %q, want %q (parent's file first, then subagent's)", tr.Files, wantFiles)
	}
	if tr.FileMissing {
		t.Error("FileMissing = true, want false")
	}

	tr2, ok, err := st.GetTranscript("s2")
	if err != nil || !ok {
		t.Fatalf("GetTranscript(s2): ok=%v err=%v", ok, err)
	}
	if tr2.Ask != "unrelated second session prompt" || tr2.Cwd != "/w/loom2" {
		t.Errorf("s2 transcript wrong: %+v", tr2)
	}

	// Subagent text must be searchable and attributed to the PARENT session.
	hits, err := st.SearchSessions("quantum flux capacitor", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].SessionID != "s1" {
		t.Fatalf("SearchSessions(quantum flux capacitor) = %+v, want 1 hit for s1", hits)
	}

	status := ix.Status()
	if status.Total != 2 {
		t.Errorf("Status.Total = %d, want 2", status.Total)
	}
	if status.Changed != 3 { // main1 + subagent + main2
		t.Errorf("Status.Changed = %d, want 3", status.Changed)
	}
	if status.Active {
		t.Error("Status.Active = true after Sweep returned")
	}
}

func TestSweepAgainNoChanges(t *testing.T) {
	st := openStore(t)
	ccd, _, _ := buildArchive(t)
	ix := NewIndexer(st, ccd)

	if err := ix.Sweep(); err != nil {
		t.Fatal(err)
	}
	if err := ix.Sweep(); err != nil {
		t.Fatal(err)
	}
	status := ix.Status()
	if status.Changed != 0 {
		t.Errorf("Status.Changed = %d, want 0 (fingerprints hold)", status.Changed)
	}
	if status.Swept == 0 {
		t.Error("Status.Swept = 0, want > 0 (files were still examined)")
	}
}

func TestSweepReindexesOnlyChangedFile(t *testing.T) {
	st := openStore(t)
	ccd, mainPath1, _ := buildArchive(t)
	ix := NewIndexer(st, ccd)

	if err := ix.Sweep(); err != nil {
		t.Fatal(err)
	}

	// Append one more line to session s1's main file only.
	f, err := os.OpenFile(mainPath1, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	extra := `{"isSidechain":false,"type":"user","message":{"role":"user","content":"one more follow-up prompt"},"uuid":"u3","timestamp":"2026-07-02T12:10:00.000Z","cwd":"/w/loom1","sessionId":"s1"}` + "\n"
	if _, err := f.WriteString(extra); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// Ensure mtime differs (some filesystems have coarse mtime resolution).
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(mainPath1, future, future); err != nil {
		t.Fatal(err)
	}

	if err := ix.Sweep(); err != nil {
		t.Fatal(err)
	}
	status := ix.Status()
	if status.Changed != 1 {
		t.Errorf("Status.Changed = %d, want 1 (only the appended-to file)", status.Changed)
	}

	docs, err := st.SessionDocs("s1")
	if err != nil {
		t.Fatal(err)
	}
	// 2 main (original) + 1 main (new) + 2 subagent = 5, no duplicates.
	if len(docs) != 5 {
		t.Fatalf("SessionDocs(s1) = %d docs, want 5 (no duplicates): %+v", len(docs), docs)
	}
}

func TestSweepFileMissingKeepsRow(t *testing.T) {
	st := openStore(t)
	ccd, _, mainPath2 := buildArchive(t)
	ix := NewIndexer(st, ccd)

	if err := ix.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetTranscript("s2"); !ok {
		t.Fatal("s2 should exist before deletion")
	}

	if err := os.Remove(mainPath2); err != nil {
		t.Fatal(err)
	}
	if err := ix.Sweep(); err != nil {
		t.Fatal(err)
	}

	tr, ok, err := st.GetTranscript("s2")
	if err != nil || !ok {
		t.Fatalf("row must be KEPT after source file deletion: ok=%v err=%v", ok, err)
	}
	if !tr.FileMissing {
		t.Error("FileMissing = false, want true after source file deleted")
	}
	if tr.Ask != "unrelated second session prompt" {
		t.Errorf("distilled fields must be kept, got Ask=%q", tr.Ask)
	}

	// Docs must also be kept (spec §2.5): SessionDocs still returns them.
	docs, err := st.SessionDocs("s2")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 {
		t.Error("docs must be KEPT after source file deletion, got 0")
	}
}

func TestRunSweepsImmediatelyThenOnTicker(t *testing.T) {
	st := openStore(t)
	ccd, _, _ := buildArchive(t)
	ix := NewIndexer(st, ccd)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ix.Run(ctx, 10*time.Millisecond)
		close(done)
	}()

	// Immediate first sweep: give it a moment then check Total.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ix.Status().Total == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ix.Status().Total != 2 {
		t.Fatalf("Status.Total = %d after Run started, want 2", ix.Status().Total)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}
