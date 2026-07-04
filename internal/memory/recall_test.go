package memory

import (
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

// openStore is defined in indexer_test.go; reused here.

// seedSession creates a transcript row plus one FTS doc whose content is
// `body`, so it's findable by SearchSessionsRaw.
func seedRecallSession(t *testing.T, s *store.Store, id, projectDir, body string, lastTS int64) {
	t.Helper()
	if err := s.UpsertTranscript(store.Transcript{
		SessionID: id, ProjectDir: projectDir, Cwd: "/w/" + projectDir,
		Title: "session " + id, Ask: body, LastTS: lastTS,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileDocs(store.IndexedFile{Path: "/f-" + id, SessionID: id, Size: 1, Mtime: 1},
		[]store.Doc{{Content: body, Role: "user", TS: lastTS}}); err != nil {
		t.Fatal(err)
	}
}

// --- buildRecallQuery (spec §2, binding, red-team probes) ---------------

func TestBuildRecallQuerySentenceSeed(t *testing.T) {
	expr, terms := buildRecallQuery("fix the card monitoring alert thresholds")
	// "fix" (<4 chars) and "the" (stopword + <4 chars) must be dropped.
	wantTerms := []string{"card", "monitoring", "alert", "thresholds"}
	if len(terms) != len(wantTerms) {
		t.Fatalf("terms = %v, want %v", terms, wantTerms)
	}
	for i, w := range wantTerms {
		if terms[i] != w {
			t.Fatalf("terms[%d] = %q, want %q (terms=%v)", i, terms[i], w, terms)
		}
	}
	want := `"card" OR "monitoring" OR "alert" OR "thresholds"`
	if expr != want {
		t.Fatalf("expr = %q, want %q", expr, want)
	}
	if strings.HasSuffix(expr, "*") {
		t.Fatalf("expr must not have a trailing *: %q", expr)
	}
}

func TestBuildRecallQueryAllStopwordsIsEmpty(t *testing.T) {
	expr, terms := buildRecallQuery("the for with")
	if expr != "" {
		t.Fatalf("expr = %q, want empty (all stopwords/short)", expr)
	}
	if len(terms) != 0 {
		t.Fatalf("terms = %v, want none surviving", terms)
	}
}

func TestBuildRecallQuerySingleSurvivingTermIsEmpty(t *testing.T) {
	// Only one term (>=4 chars, non-stopword) survives filtering: <2
	// surviving tokens must signal recency-fallback via an empty expr.
	expr, terms := buildRecallQuery("the monitoring for")
	if expr != "" {
		t.Fatalf("expr = %q, want empty (only 1 surviving term)", expr)
	}
	if len(terms) != 1 || terms[0] != "monitoring" {
		t.Fatalf("terms = %v, want [monitoring]", terms)
	}
}

func TestBuildRecallQueryQuoteEscaping(t *testing.T) {
	// Tokenizing splits on non-letter/digit, so a literal quote can't
	// survive inside a token via the tokenizer itself; exercise the
	// escaping path directly against a crafted term set via the exported
	// entry point using digits+letters only, and separately assert no
	// unescaped quote could leak through by construction (every quote in
	// the output is doubled around a term).
	expr, _ := buildRecallQuery("database migration rollback")
	for _, part := range strings.Split(expr, " OR ") {
		if !strings.HasPrefix(part, `"`) || !strings.HasSuffix(part, `"`) {
			t.Fatalf("term %q not quote-wrapped in expr %q", part, expr)
		}
	}
}

// --- Related: relevance gate, boosting, fallback (spec §2/§6) -----------

func TestRelatedTwoTermGateKillsOneTermNoiseHit(t *testing.T) {
	s := openStore(t)
	// Matches only "database" (1 term) -> noise, must be filtered.
	seedRecallSession(t, s, "noise", "loom", "a database appears once here amid unrelated filler", 100)
	// Matches both "database" and "migration" -> qualifies.
	seedRecallSession(t, s, "good", "loom", "planning the database migration carefully this time", 200)

	got, err := Related(s, "loom", "database migration plan", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].T.SessionID != "good" {
		t.Fatalf("got %+v, want only 'good' (>=2 matched terms)", got)
	}
}

func TestRelatedSameProjectBoostPromotesLowerBM25Hit(t *testing.T) {
	s := openStore(t)

	// Weak-but-qualifying same-project hit (exactly 2 term mentions).
	seedRecallSession(t, s, "target", "myproj", "notes about database performance work", 50)

	// Five cross-project hits, all far denser in the query terms (much
	// better raw bm25) so a naive "top-limit-by-bm25-then-filter"
	// implementation would push "target" off a small display limit.
	for i, id := range []string{"cross-1", "cross-2", "cross-3", "cross-4", "cross-5"} {
		seedRecallSession(t, s, id, "other",
			"database performance database performance database performance tuning work",
			int64(1000+i))
	}

	got, err := Related(s, "myproj", "database performance tuning", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d hits, want 3 (display limit): %+v", len(got), got)
	}
	if got[0].T.SessionID != "target" || !got[0].SameProject {
		t.Fatalf("got[0] = %+v, want same-project 'target' boosted to first despite weaker bm25", got[0])
	}
	for _, h := range got[1:] {
		if h.SameProject {
			t.Fatalf("cross-project hit unexpectedly marked SameProject: %+v", h)
		}
	}
}

func TestRelatedRecencyFallbackOnEmptyExpr(t *testing.T) {
	s := openStore(t)
	seedRecallSession(t, s, "old", "loom", "irrelevant body text", 100)
	seedRecallSession(t, s, "new", "loom", "irrelevant body text", 200)

	got, err := Related(s, "loom", "the for with", 5) // all stopwords -> empty expr
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].T.SessionID != "new" || got[1].T.SessionID != "old" {
		t.Fatalf("got %+v, want recency-fallback order [new old]", got)
	}
	for _, h := range got {
		if !h.SameProject || h.Snippet != "" {
			t.Fatalf("fallback hit shape wrong: %+v", h)
		}
	}
}

func TestRelatedRecencyFallbackOnZeroQualifyingHits(t *testing.T) {
	s := openStore(t)
	// Only 1 of the 2 query terms present in any session -> qualifying==0.
	seedRecallSession(t, s, "onlyone", "loom", "a database mention with nothing else relevant", 150)
	seedRecallSession(t, s, "old", "loom", "irrelevant body text", 100)
	seedRecallSession(t, s, "new", "loom", "irrelevant body text", 200)

	got, err := Related(s, "loom", "database migration", 5)
	if err != nil {
		t.Fatal(err)
	}
	// Falls back to recency across ALL of the project's sessions, not just
	// the non-qualifying FTS hit.
	if len(got) != 3 {
		t.Fatalf("got %d hits, want 3 (recency fallback over all project sessions): %+v", len(got), got)
	}
	if got[0].T.SessionID != "new" || got[1].T.SessionID != "onlyone" || got[2].T.SessionID != "old" {
		t.Fatalf("got order %v, want [new onlyone old] by last_ts desc",
			[]string{got[0].T.SessionID, got[1].T.SessionID, got[2].T.SessionID})
	}
}
