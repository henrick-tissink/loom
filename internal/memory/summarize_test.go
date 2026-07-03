package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

// fakeClaudeSummaryScript stands in for the real `claude` binary. It dumps
// its own argv, cwd, a handful of env vars, and everything piped to its
// stdin into "summarize-sink.txt" *inside its cwd* — which Summarize sets
// to sm.WorkDir, so the sink lands at filepath.Join(workDir,
// "summarize-sink.txt") without needing any extra out-of-band argv/env
// smuggled in just for the test. It then prints the fixed "FAKE SUMMARY"
// to real stdout, which Summarize captures as the result.
const fakeClaudeSummaryScript = `#!/bin/sh
{
  echo "===ARGV==="
  for a in "$@"; do printf '%s\n' "$a"; done
  echo "===CWD==="
  pwd
  echo "===ENV==="
  echo "CLAUDECODE=${CLAUDECODE}"
  echo "CLAUDE_CODE_FOO=${CLAUDE_CODE_FOO}"
  echo "CLAUDE_CONFIG_DIR=${CLAUDE_CONFIG_DIR}"
  echo "===STDIN==="
  cat
} > "summarize-sink.txt"
echo "FAKE SUMMARY"
`

// fakeClaudeSleepScript sleeps well beyond any test timeout, to exercise
// the ctx-deadline path (no output, no store write, no zombie).
const fakeClaudeSleepScript = `#!/bin/sh
sleep 5
echo "FAKE SUMMARY"
`

// testSummarizer wires a Summarizer against a throwaway store and a fake
// claude script, and sets CLAUDECODE/CLAUDE_CODE_FOO/CLAUDE_CONFIG_DIR in
// THIS process's env so scrubEnv has something real to prove it strips (the
// first two) and keeps (the last one).
func testSummarizer(t *testing.T, script string) (*Summarizer, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	workDir := t.TempDir()
	scriptPath := filepath.Join(t.TempDir(), "fake-claude.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_FOO", "leaked-if-present")
	t.Setenv("CLAUDE_CONFIG_DIR", "/fake/claude/config")

	return &Summarizer{Store: st, Binary: scriptPath, WorkDir: workDir}, workDir
}

// seedSession writes a session's docs + transcript row directly (bypassing
// the extractor/indexer, which aren't under test here).
func seedSession(t *testing.T, st *store.Store, sessionID string, docs []store.Doc, files string) {
	t.Helper()
	if err := st.UpsertTranscript(store.Transcript{SessionID: sessionID, Files: files}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceFileDocs(store.IndexedFile{Path: "/fake/" + sessionID + ".jsonl", SessionID: sessionID}, docs); err != nil {
		t.Fatal(err)
	}
}

// sinkSection extracts the text between two "===MARKER===\n" delimiters
// written by fakeClaudeSummaryScript.
func sinkSection(t *testing.T, content, start, end string) string {
	t.Helper()
	si := strings.Index(content, start)
	if si < 0 {
		t.Fatalf("sink missing marker %q in:\n%s", start, content)
	}
	si += len(start)
	rest := content[si:]
	if end == "" {
		return rest
	}
	ei := strings.Index(rest, end)
	if ei < 0 {
		t.Fatalf("sink missing marker %q in:\n%s", end, content)
	}
	return rest[:ei]
}

func readSink(t *testing.T, workDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workDir, "summarize-sink.txt"))
	if err != nil {
		t.Fatalf("reading sink: %v", err)
	}
	return string(b)
}

// (a)+(b): argv carries every binding flag verbatim (incl. the corrected
// mcp-config JSON shape and --exclude-dynamic-system-prompt-sections), the
// -p prompt contains the UNTRUSTED framing, cwd == WorkDir, and the child
// env has CLAUDECODE/CLAUDE_CODE_FOO stripped while CLAUDE_CONFIG_DIR
// survives.
func TestSummarizeArgvPromptEnvAndCwd(t *testing.T) {
	sm, workDir := testSummarizer(t, fakeClaudeSummaryScript)
	seedSession(t, sm.Store, "sess1", []store.Doc{{Content: "hello", Role: "user", TS: 1}}, "")

	if _, err := sm.Summarize("sess1", time.Now()); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	content := readSink(t, workDir)

	argvRaw := sinkSection(t, content, "===ARGV===\n", "\n===CWD===")
	argv := strings.Split(argvRaw, "\n")
	wantLen := 14
	if len(argv) != wantLen {
		t.Fatalf("argv has %d elements, want %d: %#v", len(argv), wantLen, argv)
	}
	if argv[0] != "-p" {
		t.Fatalf("argv[0] = %q, want -p", argv[0])
	}
	if !strings.Contains(argv[1], "UNTRUSTED") {
		t.Fatalf("prompt = %q, missing UNTRUSTED framing", argv[1])
	}
	if !strings.Contains(argv[1], "Write plain text only — no markdown formatting.") {
		t.Fatalf("prompt = %q, missing plain-text instruction", argv[1])
	}
	wantRest := []string{
		"--model", "haiku",
		"--no-session-persistence",
		"--tools", "",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		"--disable-slash-commands",
		"--setting-sources", "",
		"--exclude-dynamic-system-prompt-sections",
	}
	for i, want := range wantRest {
		got := argv[2+i]
		if got != want {
			t.Fatalf("argv[%d] = %q, want %q (full argv: %#v)", 2+i, got, want, argv)
		}
	}

	cwdRaw := strings.TrimSpace(sinkSection(t, content, "===CWD===\n", "\n===ENV==="))
	wantCwd, _ := filepath.EvalSymlinks(workDir)
	gotCwd, _ := filepath.EvalSymlinks(cwdRaw)
	if gotCwd != wantCwd {
		t.Fatalf("child cwd = %q, want %q", cwdRaw, workDir)
	}

	envRaw := sinkSection(t, content, "===ENV===\n", "\n===STDIN===")
	if strings.Contains(envRaw, "CLAUDECODE=1") {
		t.Fatalf("CLAUDECODE leaked into child env:\n%s", envRaw)
	}
	if strings.Contains(envRaw, "leaked-if-present") {
		t.Fatalf("CLAUDE_CODE_FOO leaked into child env:\n%s", envRaw)
	}
	if !strings.Contains(envRaw, "CLAUDE_CONFIG_DIR=/fake/claude/config") {
		t.Fatalf("CLAUDE_CONFIG_DIR missing/wrong from child env:\n%s", envRaw)
	}
}

// (c): stdin payload has user docs before assistant docs, and a "Files
// touched:" section grounded from transcript.Files (NOT from any doc).
func TestSummarizeStdinPayloadOrderingAndFiles(t *testing.T) {
	sm, workDir := testSummarizer(t, fakeClaudeSummaryScript)
	docs := []store.Doc{
		{Content: "first user turn", Role: "user", TS: 1},
		{Content: "first assistant turn", Role: "assistant", TS: 2},
		{Content: "second user turn", Role: "assistant", TS: 3}, // deliberately mixed order in storage
		{Content: "an agent aside, must be excluded", Role: "agent", TS: 4},
	}
	seedSession(t, sm.Store, "sess2", docs, "internal/foo.go\ninternal/bar.go")

	if _, err := sm.Summarize("sess2", time.Now()); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	content := readSink(t, workDir)
	stdin := sinkSection(t, content, "===STDIN===\n", "")

	userIdx := strings.Index(stdin, "USER: first user turn")
	assistantIdx := strings.Index(stdin, "ASSISTANT:")
	if userIdx < 0 || assistantIdx < 0 {
		t.Fatalf("stdin missing expected role lines: %q", stdin)
	}
	if userIdx > assistantIdx {
		t.Fatalf("assistant text precedes user text in payload: %q", stdin)
	}
	if strings.Contains(stdin, "agent aside") {
		t.Fatalf("role \"agent\" doc leaked into payload: %q", stdin)
	}
	if !strings.Contains(stdin, "Files touched:\ninternal/foo.go\ninternal/bar.go") {
		t.Fatalf("Files touched section missing/wrong: %q", stdin)
	}
}

// (e): a successful call stores the summary and sets SummaryAt.
func TestSummarizeStoresResult(t *testing.T) {
	sm, _ := testSummarizer(t, fakeClaudeSummaryScript)
	seedSession(t, sm.Store, "sess3", []store.Doc{{Content: "hi", Role: "user", TS: 1}}, "")

	now := time.Unix(1_700_000_000, 0)
	got, err := sm.Summarize("sess3", now)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if got != "FAKE SUMMARY" {
		t.Fatalf("returned summary = %q, want FAKE SUMMARY", got)
	}

	tr, ok, err := sm.Store.GetTranscript("sess3")
	if err != nil || !ok {
		t.Fatalf("GetTranscript: ok=%v err=%v", ok, err)
	}
	if tr.LLMSummary != "FAKE SUMMARY" {
		t.Fatalf("LLMSummary = %q, want FAKE SUMMARY", tr.LLMSummary)
	}
	if tr.SummaryAt != now.Unix() {
		t.Fatalf("SummaryAt = %d, want %d", tr.SummaryAt, now.Unix())
	}
}

// (f): a child that hangs past the deadline is killed, an error is
// returned, and nothing is stored. exec.CommandContext's kill-on-cancel
// means cmd.Run() (Start+Wait) has already fully returned by the time
// Summarize returns — no goroutine/process is left running (no zombie).
func TestSummarizeTimeoutReturnsErrorAndStoresNothing(t *testing.T) {
	sm, _ := testSummarizer(t, fakeClaudeSleepScript)
	sm.Timeout = 300 * time.Millisecond
	seedSession(t, sm.Store, "sess4", []store.Doc{{Content: "hi", Role: "user", TS: 1}}, "")

	start := time.Now()
	_, err := sm.Summarize("sess4", time.Now())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Summarize: want error on timeout, got nil")
	}
	// The fake script sleeps 5s; if the process were not actually killed at
	// the deadline, this call would take ~5s. Give headroom for the 300ms
	// ctx timeout plus Summarize's WaitDelay bound (2s) while staying well
	// under the full 5s sleep.
	if elapsed > 4*time.Second {
		t.Fatalf("Summarize took %v after a 300ms timeout — process likely not killed promptly", elapsed)
	}

	tr, ok, err := sm.Store.GetTranscript("sess4")
	if err != nil {
		t.Fatal(err)
	}
	if ok && tr.LLMSummary != "" {
		t.Fatalf("LLMSummary = %q, want empty (nothing stored on timeout)", tr.LLMSummary)
	}
}

// (d): payload budget rules — all-fits, user-only overflow (head+tail
// fallback), and assistant sampling to fill the remainder.
func TestBuildPayloadBudget(t *testing.T) {
	t.Run("small docs: everything included, user before assistant", func(t *testing.T) {
		docs := []store.Doc{
			{Content: "u1", Role: "user"},
			{Content: "a1", Role: "assistant"},
			{Content: "u2", Role: "user"},
			{Content: "agent stuff", Role: "agent"},
		}
		got := buildPayload(docs, "", 40_000)
		want := "USER: u1\nUSER: u2\nASSISTANT: a1"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("files touched appended when non-empty", func(t *testing.T) {
		docs := []store.Doc{{Content: "u1", Role: "user"}}
		got := buildPayload(docs, "a.go\nb.go", 40_000)
		want := "USER: u1\n\nFiles touched:\na.go\nb.go"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("files touched omitted when empty", func(t *testing.T) {
		docs := []store.Doc{{Content: "u1", Role: "user"}}
		got := buildPayload(docs, "", 40_000)
		if strings.Contains(got, "Files touched") {
			t.Fatalf("unexpected Files touched section: %q", got)
		}
	})

	t.Run("user docs alone exceed budget: head+tail fallback, no assistant text", func(t *testing.T) {
		budget := 1000
		var docs []store.Doc
		for i := 0; i < 200; i++ {
			docs = append(docs, store.Doc{Content: strings.Repeat("x", 20), Role: "user"})
		}
		docs = append(docs, store.Doc{Content: "TAIL-MARKER-CONTENT", Role: "user"})
		docs = append(docs, store.Doc{Content: "should never appear", Role: "assistant"})

		got := buildPayload(docs, "", budget)
		if len(got) > budget+64 {
			t.Fatalf("payload len = %d, want <= budget+eps (%d)", len(got), budget+64)
		}
		if strings.Contains(got, "ASSISTANT:") {
			t.Fatalf("assistant text leaked into head+tail fallback payload: %q", got)
		}
		if !strings.HasPrefix(got, "USER: xxxxxxxxxxxxxxxxxxxx") {
			t.Fatalf("payload does not start with head of user docs: %q", got[:min(60, len(got))])
		}
		if !strings.Contains(got, "TAIL-MARKER-CONTENT") {
			t.Fatalf("payload missing tail content: %q", got)
		}
	})

	t.Run("assistant docs sampled evenly to fill remainder when they overflow", func(t *testing.T) {
		budget := 500
		docs := []store.Doc{{Content: "short user ask", Role: "user"}}
		for i := 0; i < 100; i++ {
			docs = append(docs, store.Doc{Content: strings.Repeat("y", 50), Role: "assistant"})
		}
		got := buildPayload(docs, "", budget)
		if len(got) > budget+8 {
			t.Fatalf("payload len = %d, want <= budget+eps (%d): %q", len(got), budget+8, got)
		}
		if !strings.HasPrefix(got, "USER: short user ask") {
			t.Fatalf("payload does not lead with the user doc: %q", got)
		}
		if !strings.Contains(got, "ASSISTANT:") {
			t.Fatalf("payload has no sampled assistant content: %q", got)
		}
	})
}
