package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeJSONL writes lines (already-valid JSONL, one record per string) to a
// temp file and returns its path.
func writeJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "session.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// --- Filter table (spec §2, binding) ---------------------------------

func TestUserStringContentIndexedAndAskable(t *testing.T) {
	p := writeJSONL(t, `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"fix the parser bug"},"uuid":"u1","timestamp":"2026-07-02T12:55:36.397Z","cwd":"/w/loom","sessionId":"s1"}`)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 1 || ex.Docs[0].Content != "fix the parser bug" || ex.Docs[0].Role != "user" {
		t.Fatalf("Docs = %+v", ex.Docs)
	}
	if ex.Ask != "fix the parser bug" {
		t.Fatalf("Ask = %q", ex.Ask)
	}
}

func TestUserListContentWithoutToolResultConcatenatedAndFiltered(t *testing.T) {
	p := writeJSONL(t, `{"isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>ignore me</system-reminder>"},{"type":"text","text":"Base directory for this skill: /foo"},{"type":"text","text":"first part"},{"type":"text","text":"second part"}]},"uuid":"u1","timestamp":"2026-07-02T12:55:36.397Z","cwd":"/w/loom","sessionId":"s1"}`)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 1 {
		t.Fatalf("Docs = %+v", ex.Docs)
	}
	want := "first part second part" // blocks joined with \n, then CleanText collapses it to a space
	if ex.Docs[0].Content != want {
		t.Fatalf("content = %q, want %q", ex.Docs[0].Content, want)
	}
	if ex.Ask != want {
		t.Fatalf("Ask = %q", ex.Ask)
	}
}

func TestUserIsMetaSkipped(t *testing.T) {
	p := writeJSONL(t, `{"isSidechain":false,"isMeta":true,"type":"user","message":{"role":"user","content":"some meta wrapper text"},"uuid":"u1","timestamp":"2026-07-02T12:55:36.397Z","cwd":"/w/loom","sessionId":"s1"}`)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 0 || ex.Ask != "" {
		t.Fatalf("expected fully skipped, got Docs=%+v Ask=%q", ex.Docs, ex.Ask)
	}
}

func TestUserCommandWrapperSkippedForAskAndIndex(t *testing.T) {
	cases := []string{
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"<command-name>/foo</command-name>"},"uuid":"u1","timestamp":"2026-07-02T12:55:36.397Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"Caveat: the messages below were generated"},"uuid":"u2","timestamp":"2026-07-02T12:55:37.397Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"[Request interrupted by user]"},"uuid":"u3","timestamp":"2026-07-02T12:55:38.397Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"<local-command-stdout>ok</local-command-stdout>"},"uuid":"u4","timestamp":"2026-07-02T12:55:39.397Z","cwd":"/w/loom","sessionId":"s1"}`,
	}
	for _, line := range cases {
		p := writeJSONL(t, line)
		ex, err := ExtractFile(p, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(ex.Docs) != 0 || ex.Ask != "" {
			t.Fatalf("line %q: expected skipped, got Docs=%+v Ask=%q", line, ex.Docs, ex.Ask)
		}
	}
}

func TestUserListContentWithToolResultNotIndexed(t *testing.T) {
	p := writeJSONL(t, `{"isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"some result text"}]},"uuid":"u1","timestamp":"2026-07-02T12:55:36.397Z","cwd":"/w/loom","sessionId":"s1"}`)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 0 || ex.Ask != "" {
		t.Fatalf("expected skipped, got Docs=%+v Ask=%q", ex.Docs, ex.Ask)
	}
}

func TestAssistantTextIndexedAsOutcomeNeverAsk(t *testing.T) {
	p := writeJSONL(t,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"pondering"},{"type":"text","text":"first reply"}]},"uuid":"a1","timestamp":"2026-07-02T12:55:40.397Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"final reply"}]},"uuid":"a2","timestamp":"2026-07-02T12:55:41.397Z","cwd":"/w/loom","sessionId":"s1"}`,
	)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 2 || ex.Docs[0].Content != "first reply" || ex.Docs[1].Content != "final reply" {
		t.Fatalf("Docs = %+v", ex.Docs)
	}
	if ex.Docs[0].Role != "assistant" {
		t.Fatalf("role = %q", ex.Docs[0].Role)
	}
	if ex.Outcome != "final reply" {
		t.Fatalf("Outcome = %q, want last assistant text", ex.Outcome)
	}
	if ex.Ask != "" {
		t.Fatalf("assistant text must never set Ask, got %q", ex.Ask)
	}
}

func TestIsCompactSummaryIndexedButNeverAskOrOutcome(t *testing.T) {
	p := writeJSONL(t, `{"isSidechain":false,"type":"user","isVisibleInTranscriptOnly":true,"isCompactSummary":true,"message":{"role":"user","content":"This session is being continued from a previous conversation."},"uuid":"u1","timestamp":"2026-07-02T12:55:36.397Z","cwd":"/w/loom","sessionId":"s1"}`)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 1 || ex.Docs[0].Content != "This session is being continued from a previous conversation." {
		t.Fatalf("Docs = %+v", ex.Docs)
	}
	if ex.Ask != "" {
		t.Fatalf("compact summary must never set Ask, got %q", ex.Ask)
	}
	if ex.Outcome != "" {
		t.Fatalf("compact summary (user record) must never set Outcome, got %q", ex.Outcome)
	}
}

func TestThinkingToolUseToolResultNeverIndexed(t *testing.T) {
	p := writeJSONL(t,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"deep thought"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},"uuid":"a1","timestamp":"2026-07-02T12:55:40.397Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file1\nfile2"}]},"uuid":"u1","timestamp":"2026-07-02T12:55:41.397Z","cwd":"/w/loom","sessionId":"s1"}`,
	)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 0 {
		t.Fatalf("Docs = %+v, want none (thinking/tool_use/tool_result never indexed)", ex.Docs)
	}
}

// --- Streaming reader correctness ------------------------------------

func TestHugeToolResultLineSkippedWithoutAbortingFile(t *testing.T) {
	huge := strings.Repeat("A", 1_200_000) // >1MB single line
	hugeLine := `{"isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"` + huge + `"}]},"uuid":"u1","timestamp":"2026-07-02T12:55:36.397Z","cwd":"/w/loom","sessionId":"s1"}`
	after := `{"isSidechain":false,"type":"user","message":{"role":"user","content":"a normal prompt after the huge line"},"uuid":"u2","timestamp":"2026-07-02T12:55:37.397Z","cwd":"/w/loom","sessionId":"s1"}`
	p := writeJSONL(t, hugeLine, after)

	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 1 || ex.Docs[0].Content != "a normal prompt after the huge line" {
		t.Fatalf("Docs = %+v, want only the post-huge-line prompt", ex.Docs)
	}
}

func TestFinalLineWithoutTrailingNewline(t *testing.T) {
	line := `{"isSidechain":false,"type":"user","message":{"role":"user","content":"no trailing newline"},"uuid":"u1","timestamp":"2026-07-02T12:55:36.397Z","cwd":"/w/loom","sessionId":"s1"}`
	p := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil { // no trailing "\n"
		t.Fatal(err)
	}
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 1 || ex.Docs[0].Content != "no trailing newline" {
		t.Fatalf("Docs = %+v", ex.Docs)
	}
}

func TestBadJSONLineSkippedNotAborting(t *testing.T) {
	p := writeJSONL(t,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"before"}, "uuid":"u1","timestamp":"2026-07-02T12:55:36.397Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{not valid json at all`,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"after"}, "uuid":"u2","timestamp":"2026-07-02T12:55:37.397Z","cwd":"/w/loom","sessionId":"s1"}`,
	)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 2 || ex.Docs[0].Content != "before" || ex.Docs[1].Content != "after" {
		t.Fatalf("Docs = %+v", ex.Docs)
	}
}

// --- Timestamps, sidecars, cwd, files ---------------------------------

func TestTimestampsIgnoreSidecarsAtHeadAndTail(t *testing.T) {
	p := writeJSONL(t,
		`{"type":"mode","mode":"normal","sessionId":"s1"}`,
		`{"type":"permission-mode","permissionMode":"auto","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"first real prompt"},"uuid":"u1","timestamp":"2026-07-02T12:00:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]},"uuid":"a1","timestamp":"2026-07-02T12:05:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"type":"file-history-snapshot","sessionId":"s1"}`,
		`{"type":"queue-operation","sessionId":"s1"}`,
	)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := int64(1782993600) // 2026-07-02T12:00:00Z
	wantLast := int64(1782993900)  // 2026-07-02T12:05:00Z
	if ex.FirstTS != wantFirst {
		t.Fatalf("FirstTS = %d, want %d", ex.FirstTS, wantFirst)
	}
	if ex.LastTS != wantLast {
		t.Fatalf("LastTS = %d, want %d", ex.LastTS, wantLast)
	}
}

func TestMultiCwdAllReturned(t *testing.T) {
	p := writeJSONL(t,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"one"},"uuid":"u1","timestamp":"2026-07-02T12:00:00.000Z","cwd":"/w/a","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"two"}]},"uuid":"a1","timestamp":"2026-07-02T12:01:00.000Z","cwd":"/w/b","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"three"},"uuid":"u2","timestamp":"2026-07-02T12:02:00.000Z","cwd":"/w/a","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"four"},"uuid":"u3","timestamp":"2026-07-02T12:03:00.000Z","cwd":"/w/c","sessionId":"s1"}`,
	)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/w/a", "/w/b", "/w/c"}
	if len(ex.Cwds) != len(want) {
		t.Fatalf("Cwds = %v, want %v", ex.Cwds, want)
	}
	for i, w := range want {
		if ex.Cwds[i] != w {
			t.Fatalf("Cwds = %v, want %v", ex.Cwds, want)
		}
	}
}

func TestFilesFromEditWriteMultiEditAndNotebookEdit(t *testing.T) {
	p := writeJSONL(t,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/w/loom/a.go","old_string":"x","new_string":"y"}}]},"uuid":"a1","timestamp":"2026-07-02T12:00:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Write","input":{"file_path":"/w/loom/b.go","content":"package x"}}]},"uuid":"a2","timestamp":"2026-07-02T12:01:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"MultiEdit","input":{"file_path":"/w/loom/c.go","edits":[]}}]},"uuid":"a3","timestamp":"2026-07-02T12:02:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t4","name":"NotebookEdit","input":{"notebook_path":"/w/loom/nb.ipynb","new_source":"1+1"}}]},"uuid":"a4","timestamp":"2026-07-02T12:03:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
		// duplicate path must not be re-added
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t5","name":"Edit","input":{"file_path":"/w/loom/a.go","old_string":"y","new_string":"z"}}]},"uuid":"a5","timestamp":"2026-07-02T12:04:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
		// Bash tool_use must never contribute a "file"
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t6","name":"Bash","input":{"command":"go build ./..."}}]},"uuid":"a6","timestamp":"2026-07-02T12:05:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
	)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/w/loom/a.go", "/w/loom/b.go", "/w/loom/c.go", "/w/loom/nb.ipynb"}
	if len(ex.Files) != len(want) {
		t.Fatalf("Files = %v, want %v", ex.Files, want)
	}
	for i, w := range want {
		if ex.Files[i] != w {
			t.Fatalf("Files = %v, want %v", ex.Files, want)
		}
	}
}

func TestAiTitleSetsTitle(t *testing.T) {
	p := writeJSONL(t,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1","timestamp":"2026-07-02T12:00:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"type":"ai-title","aiTitle":"Respond with pong message","isSidechain":false,"sessionId":"s1"}`,
	)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Title != "Respond with pong message" {
		t.Fatalf("Title = %q", ex.Title)
	}
}

func TestControlCharsStrippedFromIndexedContent(t *testing.T) {
	// \x01 must never survive into indexed content (it's the FTS snippet
	// highlight-start marker; letting transcript content contain a raw
	// \x01 would let it forge a highlight).
	line := `{"isSidechain":false,"type":"user","message":{"role":"user","content":"before` + "\\u0001" + `after"},"uuid":"u1","timestamp":"2026-07-02T12:00:00.000Z","cwd":"/w/loom","sessionId":"s1"}`
	p := writeJSONL(t, line)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 1 {
		t.Fatalf("Docs = %+v", ex.Docs)
	}
	if strings.ContainsRune(ex.Docs[0].Content, 0x01) {
		t.Fatalf("content still contains \\x01: %q", ex.Docs[0].Content)
	}
	if ex.Docs[0].Content != "beforeafter" {
		t.Fatalf("content = %q, want %q", ex.Docs[0].Content, "beforeafter")
	}
}

// --- roleOverride "agent" for subagent files ---------------------------

func TestRoleOverrideAgentForSubagentFile(t *testing.T) {
	p := writeJSONL(t,
		`{"parentUuid":null,"isSidechain":true,"agentId":"agent1","type":"user","message":{"role":"user","content":"do the subagent task"},"uuid":"u1","timestamp":"2026-07-02T12:00:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"isSidechain":true,"agentId":"agent1","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"subagent result text"}]},"uuid":"a1","timestamp":"2026-07-02T12:01:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
	)
	ex, err := ExtractFile(p, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Docs) != 2 {
		t.Fatalf("Docs = %+v", ex.Docs)
	}
	for _, d := range ex.Docs {
		if d.Role != "agent" {
			t.Fatalf("Doc role = %q, want agent: %+v", d.Role, d)
		}
	}
	// Text still passes the same filters (both docs indexed), but a
	// subagent file must NEVER set Ask/Outcome/Title.
	if ex.Ask != "" {
		t.Fatalf("subagent file must never set Ask, got %q", ex.Ask)
	}
	if ex.Outcome != "" {
		t.Fatalf("subagent file must never set Outcome, got %q", ex.Outcome)
	}
	if ex.Title != "" {
		t.Fatalf("subagent file must never set Title, got %q", ex.Title)
	}
}

func TestRoleOverrideAgentStillContributesFiles(t *testing.T) {
	p := writeJSONL(t,
		`{"isSidechain":true,"agentId":"agent1","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/w/loom/sub.go","old_string":"x","new_string":"y"}}]},"uuid":"a1","timestamp":"2026-07-02T12:00:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
	)
	ex, err := ExtractFile(p, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Files) != 1 || ex.Files[0] != "/w/loom/sub.go" {
		t.Fatalf("Files = %v", ex.Files)
	}
}

// --- msg_count ----------------------------------------------------------

func TestMsgCountEqualsIndexedDocCount(t *testing.T) {
	p := writeJSONL(t,
		`{"isSidechain":false,"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1","timestamp":"2026-07-02T12:00:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
		`{"isSidechain":false,"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]},"uuid":"a1","timestamp":"2026-07-02T12:01:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
		// isMeta user: does not count
		`{"isSidechain":false,"isMeta":true,"type":"user","message":{"role":"user","content":"meta wrapper"},"uuid":"u2","timestamp":"2026-07-02T12:02:00.000Z","cwd":"/w/loom","sessionId":"s1"}`,
	)
	ex, err := ExtractFile(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if ex.MsgCount != len(ex.Docs) {
		t.Fatalf("MsgCount = %d, len(Docs) = %d", ex.MsgCount, len(ex.Docs))
	}
	if ex.MsgCount != 2 {
		t.Fatalf("MsgCount = %d, want 2", ex.MsgCount)
	}
}

// --- CleanText unit tests ------------------------------------------------

func TestCleanText(t *testing.T) {
	cases := map[string]string{
		"hello world":         "hello world",
		"line1\nline2":        "line1 line2",
		"a\r\nb":              "a b",
		"tab\ttab":            "tab tab",
		"multi\n\n\nnewline":  "multi newline",
		"ctl\x01\x02\x03end":  "ctlend",
		"mix\n\x01\t \x02end": "mix   end", // \n->sp, \t->sp, literal sp kept, control bytes dropped: 3 spaces
		"":                    "",
		"\x1b[31mred\x1b[0m":  "[31mred[0m",
	}
	for in, want := range cases {
		if got := CleanText(in); got != want {
			t.Fatalf("CleanText(%q) = %q, want %q", in, got, want)
		}
	}
}
