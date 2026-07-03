package store

import (
	"strings"
	"testing"
)

func TestReplaceFileDocsRoundtripAndReplace(t *testing.T) {
	s := open(t)

	fA := IndexedFile{Path: "/proj/a.jsonl", SessionID: "sess1", Size: 100, Mtime: 111}
	docsA := []Doc{
		{Content: "alpha one", Role: "user", TS: 1},
		{Content: "alpha two", Role: "assistant", TS: 2},
		{Content: "alpha three", Role: "user", TS: 3},
	}
	if err := s.ReplaceFileDocs(fA, docsA); err != nil {
		t.Fatal(err)
	}

	fB := IndexedFile{Path: "/proj/b.jsonl", SessionID: "sess1", Size: 50, Mtime: 222}
	docsB := []Doc{
		{Content: "beta one", Role: "user", TS: 4},
		{Content: "beta two", Role: "assistant", TS: 5},
	}
	if err := s.ReplaceFileDocs(fB, docsB); err != nil {
		t.Fatal(err)
	}

	docs, err := s.SessionDocs("sess1")
	if err != nil || len(docs) != 5 {
		t.Fatalf("SessionDocs = %d docs, err=%v; want 5", len(docs), err)
	}

	gotA, ok, err := s.GetIndexedFile("/proj/a.jsonl")
	if err != nil || !ok {
		t.Fatalf("GetIndexedFile(A): ok=%v err=%v", ok, err)
	}
	gotB, ok, err := s.GetIndexedFile("/proj/b.jsonl")
	if err != nil || !ok {
		t.Fatalf("GetIndexedFile(B): ok=%v err=%v", ok, err)
	}

	// Replace file A with 1 doc. The caller supplies A's OLD fingerprint
	// (gotA, as read via GetIndexedFile) so ReplaceFileDocs knows which
	// rowid range to delete before inserting the new doc.
	fA2 := gotA
	fA2.Size, fA2.Mtime = 999, 333
	if err := s.ReplaceFileDocs(fA2, []Doc{{Content: "alpha replaced", Role: "user", TS: 9}}); err != nil {
		t.Fatal(err)
	}

	docs, err = s.SessionDocs("sess1")
	if err != nil || len(docs) != 3 {
		t.Fatalf("SessionDocs after replace = %d docs, err=%v; want 3 (1 from A + 2 from B)", len(docs), err)
	}

	gotA2, ok, err := s.GetIndexedFile("/proj/a.jsonl")
	if err != nil || !ok {
		t.Fatalf("GetIndexedFile(A) after replace: ok=%v err=%v", ok, err)
	}
	if gotA2.Size != 999 || gotA2.Mtime != 333 {
		t.Fatalf("fingerprint not updated: %+v", gotA2)
	}
	if gotA2.FirstRowid == gotA.FirstRowid && gotA2.LastRowid == gotA.LastRowid {
		t.Fatalf("rowid range not replaced: old=%+v new=%+v", gotA, gotA2)
	}

	// B's fingerprint/rowid range must be untouched by A's replace.
	gotB2, ok, err := s.GetIndexedFile("/proj/b.jsonl")
	if err != nil || !ok {
		t.Fatalf("GetIndexedFile(B) after A's replace: ok=%v err=%v", ok, err)
	}
	if gotB2 != gotB {
		t.Fatalf("B's fingerprint mutated by A's replace: before=%+v after=%+v", gotB, gotB2)
	}
}

func TestSearchSessionsGroupedBestPerSession(t *testing.T) {
	s := open(t)

	if err := s.UpsertTranscript(Transcript{
		SessionID: "sess-hot", ProjectDir: "loom", Cwd: "/w/loom",
		Title: "hot session", Ask: "fix the widget", LastTS: 200,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTranscript(Transcript{
		SessionID: "sess-cold", ProjectDir: "loom", Cwd: "/w/loom",
		Title: "cold session", Ask: "look at widget once", LastTS: 100,
	}); err != nil {
		t.Fatal(err)
	}

	// sess-hot: two docs, both dense with the query term (more relevant).
	if err := s.ReplaceFileDocs(IndexedFile{Path: "/h1", SessionID: "sess-hot", Size: 1, Mtime: 1}, []Doc{
		{Content: "widget widget widget everywhere in this session", Role: "user", TS: 1},
		{Content: "the widget again, still talking about the widget", Role: "assistant", TS: 2},
	}); err != nil {
		t.Fatal(err)
	}
	// sess-cold: one doc, a single passing mention amid unrelated filler.
	if err := s.ReplaceFileDocs(IndexedFile{Path: "/c1", SessionID: "sess-cold", Size: 1, Mtime: 1}, []Doc{
		{Content: "a long document about many unrelated topics with a passing mention of widget near the end and nothing else relevant", Role: "user", TS: 3},
	}); err != nil {
		t.Fatal(err)
	}

	hits, err := s.SearchSessions("widget", 50)
	if err != nil {
		t.Fatalf("SearchSessions error: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2 (one per session): %+v", len(hits), hits)
	}
	if hits[0].SessionID != "sess-hot" {
		t.Fatalf("hits[0].SessionID = %q, want sess-hot ranked first: %+v", hits[0].SessionID, hits)
	}
	if !strings.Contains(hits[0].Snippet, "\x01") || !strings.Contains(hits[0].Snippet, "\x02") {
		t.Fatalf("snippet missing \\x01/\\x02 markers: %q", hits[0].Snippet)
	}
	if hits[0].Title != "hot session" || hits[0].ProjectDir != "loom" || hits[0].Cwd != "/w/loom" ||
		hits[0].Ask != "fix the widget" || hits[0].LastTS != 200 {
		t.Fatalf("joined transcript fields not populated: %+v", hits[0])
	}
}

func TestSanitizeFTSQuery(t *testing.T) {
	cases := map[string]string{
		`hello world`: `"hello" "world"*`,
		`he"llo`:      `"he""llo"*`,
		`"`:           `""""*`,
		`-`:           `"-"*`,
		`фраза`:       `"фраза"*`,
		`NEAR`:        `"NEAR"*`,
		`(foo)`:       `"(foo)"*`,
	}
	for in, want := range cases {
		if got := sanitizeFTSQuery(in); got != want {
			t.Errorf("sanitizeFTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchMalformedNeverErrors(t *testing.T) {
	s := open(t)
	for _, raw := range []string{"", `"`, "* -()"} {
		hits, err := s.SearchSessions(raw, 50)
		if err != nil {
			t.Fatalf("SearchSessions(%q) error = %v, want nil", raw, err)
		}
		if len(hits) != 0 {
			t.Fatalf("SearchSessions(%q) = %d hits, want 0: %+v", raw, len(hits), hits)
		}
	}
}

func TestGetLatestByClaudeSessionID(t *testing.T) {
	s := open(t)
	older := SessionRow{
		Name: "loom-old", ClaudeSessionID: "shared-id", ProjectLabel: "p", Cwd: "/w",
		CreatedAt: 100, EndedAt: -1, ExitCode: -1, LastStatus: "unknown",
	}
	newer := SessionRow{
		Name: "loom-new", ClaudeSessionID: "shared-id", ProjectLabel: "p", Cwd: "/w",
		CreatedAt: 200, EndedAt: -1, ExitCode: -1, LastStatus: "unknown",
	}
	if err := s.Upsert(older); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(newer); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.GetLatestByClaudeSessionID("shared-id")
	if err != nil || !ok {
		t.Fatalf("GetLatestByClaudeSessionID: ok=%v err=%v", ok, err)
	}
	if got.Name != "loom-new" {
		t.Fatalf("got %q, want loom-new (the newer created_at row)", got.Name)
	}

	_, ok, err = s.GetLatestByClaudeSessionID("absent")
	if err != nil || ok {
		t.Fatalf("absent id: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestTranscriptUpsertGetSummaryMissing(t *testing.T) {
	s := open(t)
	tr := Transcript{
		SessionID: "sess1", ProjectDir: "loom", Cwd: "/w/loom", Title: "t",
		Ask: "ask", Outcome: "outcome", Files: "a.go\nb.go", LLMSummary: "",
		FirstTS: 10, LastTS: 20, MsgCount: 5, SummaryAt: 0, FileMissing: false,
	}
	if err := s.UpsertTranscript(tr); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetTranscript("sess1")
	if err != nil || !ok {
		t.Fatalf("GetTranscript: ok=%v err=%v", ok, err)
	}
	if got != tr {
		t.Fatalf("got %+v want %+v", got, tr)
	}

	if err := s.SetLLMSummary("sess1", "a summary", 999); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetTranscript("sess1")
	if got.LLMSummary != "a summary" || got.SummaryAt != 999 {
		t.Fatalf("SetLLMSummary: %+v", got)
	}

	if err := s.SetFileMissing("sess1", true); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetTranscript("sess1")
	if !got.FileMissing {
		t.Fatalf("SetFileMissing: %+v", got)
	}

	_, ok, err = s.GetTranscript("nope")
	if err != nil || ok {
		t.Fatalf("absent transcript: ok=%v err=%v, want ok=false", ok, err)
	}

	n, err := s.TranscriptCount()
	if err != nil || n != 1 {
		t.Fatalf("TranscriptCount = %d, err=%v; want 1", n, err)
	}
}
