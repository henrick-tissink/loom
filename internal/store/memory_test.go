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

func TestSessionDocsMultiRange(t *testing.T) {
	s := open(t)

	// Insert one session across 3 files (3 ranges)
	fA := IndexedFile{Path: "/proj/a.jsonl", SessionID: "sess1", Size: 100, Mtime: 111}
	docsA := []Doc{
		{Content: "alpha one", Role: "user", TS: 1},
		{Content: "alpha two", Role: "assistant", TS: 2},
	}
	if err := s.ReplaceFileDocs(fA, docsA); err != nil {
		t.Fatal(err)
	}

	fB := IndexedFile{Path: "/proj/b.jsonl", SessionID: "sess1", Size: 200, Mtime: 222}
	docsB := []Doc{
		{Content: "beta one", Role: "user", TS: 3},
	}
	if err := s.ReplaceFileDocs(fB, docsB); err != nil {
		t.Fatal(err)
	}

	fC := IndexedFile{Path: "/proj/c.jsonl", SessionID: "sess1", Size: 300, Mtime: 333}
	docsC := []Doc{
		{Content: "gamma one", Role: "assistant", TS: 4},
		{Content: "gamma two", Role: "user", TS: 5},
	}
	if err := s.ReplaceFileDocs(fC, docsC); err != nil {
		t.Fatal(err)
	}

	// SessionDocs should return all 5 docs in rowid order
	docs, err := s.SessionDocs("sess1")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 5 {
		t.Fatalf("SessionDocs = %d docs, want 5: %+v", len(docs), docs)
	}

	// Verify the order (by content)
	expectedContents := []string{"alpha one", "alpha two", "beta one", "gamma one", "gamma two"}
	for i, d := range docs {
		if d.Content != expectedContents[i] {
			t.Errorf("docs[%d].Content = %q, want %q", i, d.Content, expectedContents[i])
		}
	}
}

func TestUpsertPreservesLLMSummary(t *testing.T) {
	s := open(t)

	// Insert initial transcript with empty summary
	tr := Transcript{
		SessionID: "sess1", ProjectDir: "loom", Cwd: "/w/loom", Title: "title1",
		Ask: "initial ask", Outcome: "outcome", Files: "a.go", LLMSummary: "",
		FirstTS: 10, LastTS: 20, MsgCount: 5, SummaryAt: 0, FileMissing: false,
	}
	if err := s.UpsertTranscript(tr); err != nil {
		t.Fatal(err)
	}

	// Set LLM summary
	if err := s.SetLLMSummary("sess1", "paid summary", 999); err != nil {
		t.Fatal(err)
	}

	// Verify summary was set
	got, _, _ := s.GetTranscript("sess1")
	if got.LLMSummary != "paid summary" || got.SummaryAt != 999 {
		t.Fatalf("SetLLMSummary failed: got %+v", got)
	}

	// Upsert the same session with different ask (should NOT overwrite summary)
	tr2 := Transcript{
		SessionID: "sess1", ProjectDir: "loom", Cwd: "/w/loom", Title: "title1",
		Ask: "different ask", Outcome: "outcome", Files: "a.go", LLMSummary: "",
		FirstTS: 10, LastTS: 20, MsgCount: 5, SummaryAt: 0, FileMissing: false,
	}
	if err := s.UpsertTranscript(tr2); err != nil {
		t.Fatal(err)
	}

	// Verify ask was updated but summary was preserved
	got, _, _ = s.GetTranscript("sess1")
	if got.Ask != "different ask" {
		t.Errorf("Ask not updated: got %q want 'different ask'", got.Ask)
	}
	if got.LLMSummary != "paid summary" {
		t.Errorf("LLMSummary was overwritten: got %q want 'paid summary'", got.LLMSummary)
	}
	if got.SummaryAt != 999 {
		t.Errorf("SummaryAt was overwritten: got %d want 999", got.SummaryAt)
	}
}

func TestRecentTranscriptsByProjectDirOrderingAndLimit(t *testing.T) {
	s := open(t)
	mk := func(id, project string, lastTS int64) {
		if err := s.UpsertTranscript(Transcript{SessionID: id, ProjectDir: project, LastTS: lastTS}); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", "loom", 100)
	mk("b", "loom", 300)
	mk("c", "loom", 200)
	mk("d", "other", 400) // different project: must be excluded regardless of recency

	got, err := s.RecentTranscriptsByProjectDir("loom", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d hits, want 2 (limit): %+v", len(got), got)
	}
	if got[0].SessionID != "b" || got[1].SessionID != "c" {
		t.Fatalf("order = [%s %s], want [b c] (last_ts desc)", got[0].SessionID, got[1].SessionID)
	}
}

// TestRecentTranscriptsByProjectDirs covers the orchestrator brief's "Recent
// work" scope (orchestrator spec §5.2): a project is {root} ∪ {member repos},
// so the recency query has to span a SET of directories, and the limit applies
// to the merged result rather than per directory.
func TestRecentTranscriptsByProjectDirs(t *testing.T) {
	s := open(t)
	mk := func(id, project string, lastTS int64) {
		if err := s.UpsertTranscript(Transcript{SessionID: id, ProjectDir: project, LastTS: lastTS}); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", "/w/bankenstein", 100)
	mk("b", "/w/ballista", 300)
	mk("c", "/w/bankenstein", 200)
	mk("d", "/w/elsewhere", 400) // another project: never quoted in this brief

	cases := []struct {
		name  string
		dirs  []string
		limit int
		want  []string
	}{
		{
			name: "spans the whole member set, newest first",
			dirs: []string{"/w/bankenstein", "/w/ballista"}, limit: 10,
			want: []string{"b", "c", "a"},
		},
		{
			// the cap is on the merged result: a project whose work all
			// happened in one repo should still fill the section
			name: "limit applies to the merge, not per directory",
			dirs: []string{"/w/bankenstein", "/w/ballista"}, limit: 2,
			want: []string{"b", "c"},
		},
		{
			name: "one dir behaves exactly like the single-dir form",
			dirs: []string{"/w/bankenstein"}, limit: 10,
			want: []string{"c", "a"},
		},
		{
			// fail-closed, slice 1 §4's direction: an unattributable or empty
			// project must not silently widen into a scan of the whole index
			name: "empty set returns nothing, never everything",
			dirs: nil, limit: 10,
			want: nil,
		},
		{
			name: "unknown dir contributes nothing",
			dirs: []string{"/w/nope"}, limit: 10,
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.RecentTranscriptsByProjectDirs(c.dirs, c.limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %d rows %+v, want %v", len(got), got, c.want)
			}
			for i := range c.want {
				if got[i].SessionID != c.want[i] {
					t.Fatalf("row %d = %q, want %q (full: %+v)", i, got[i].SessionID, c.want[i], got)
				}
			}
		})
	}
}

func TestSearchSessionsRawUsesPrebuiltExprNoSanitizing(t *testing.T) {
	s := open(t)
	if err := s.UpsertTranscript(Transcript{
		SessionID: "sess1", ProjectDir: "loom", Cwd: "/w/loom", Title: "t", LastTS: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileDocs(IndexedFile{Path: "/f1", SessionID: "sess1", Size: 1, Mtime: 1}, []Doc{
		{Content: "database migration notes", Role: "user", TS: 1},
	}); err != nil {
		t.Fatal(err)
	}

	// A pre-built OR expression, exactly the shape buildRecallQuery
	// produces — NOT run through sanitizeFTSQuery's implicit-AND +
	// trailing-* shape.
	hits, err := s.SearchSessionsRaw(`"database" OR "migration"`, 15)
	if err != nil {
		t.Fatalf("SearchSessionsRaw error: %v", err)
	}
	if len(hits) != 1 || hits[0].SessionID != "sess1" {
		t.Fatalf("hits = %+v, want 1 hit for sess1", hits)
	}

	// Empty expr never errors and returns no hits (mirrors SearchSessions'
	// malformed-input contract).
	hits, err = s.SearchSessionsRaw("", 15)
	if err != nil || len(hits) != 0 {
		t.Fatalf("SearchSessionsRaw(\"\") = %+v, err=%v; want 0 hits, nil err", hits, err)
	}
}

func TestDeleteFileDocs(t *testing.T) {
	s := open(t)

	// Insert docs for two files in the same session
	fA := IndexedFile{Path: "/proj/a.jsonl", SessionID: "sess1", Size: 100, Mtime: 111}
	docsA := []Doc{
		{Content: "alpha one", Role: "user", TS: 1},
		{Content: "alpha two", Role: "assistant", TS: 2},
	}
	if err := s.ReplaceFileDocs(fA, docsA); err != nil {
		t.Fatal(err)
	}

	fB := IndexedFile{Path: "/proj/b.jsonl", SessionID: "sess1", Size: 200, Mtime: 222}
	docsB := []Doc{
		{Content: "beta one", Role: "user", TS: 3},
	}
	if err := s.ReplaceFileDocs(fB, docsB); err != nil {
		t.Fatal(err)
	}

	// Verify both files are indexed
	_, ok, _ := s.GetIndexedFile("/proj/a.jsonl")
	if !ok {
		t.Fatal("file A should be indexed")
	}
	gotB, ok, _ := s.GetIndexedFile("/proj/b.jsonl")
	if !ok {
		t.Fatal("file B should be indexed")
	}

	// Verify all 3 docs are in the session
	docs, err := s.SessionDocs("sess1")
	if err != nil || len(docs) != 3 {
		t.Fatalf("SessionDocs = %d docs, want 3", len(docs))
	}

	// Delete file A's docs
	if err := s.DeleteFileDocs("/proj/a.jsonl"); err != nil {
		t.Fatal(err)
	}

	// Verify file A is no longer indexed
	_, ok, _ = s.GetIndexedFile("/proj/a.jsonl")
	if ok {
		t.Fatal("file A should no longer be indexed after DeleteFileDocs")
	}

	// Verify file B is still indexed
	gotB2, ok, _ := s.GetIndexedFile("/proj/b.jsonl")
	if !ok {
		t.Fatal("file B should still be indexed")
	}
	if gotB2 != gotB {
		t.Fatalf("file B was modified: before=%+v after=%+v", gotB, gotB2)
	}

	// Verify only file B's docs remain
	docs, err = s.SessionDocs("sess1")
	if err != nil || len(docs) != 1 {
		t.Fatalf("SessionDocs = %d docs, want 1 (only B)", len(docs))
	}
	if docs[0].Content != "beta one" {
		t.Errorf("remaining doc is %q, want 'beta one'", docs[0].Content)
	}
}
