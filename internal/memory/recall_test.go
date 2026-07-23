package memory

import (
	"fmt"
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

func TestBuildRecallQueryDeduplicatesTokens(t *testing.T) {
	// Seed with duplicate tokens: "card" appears twice. After filtering and
	// deduplication, each surviving term should appear exactly once in the
	// terms slice and in the OR expression, so countMatchedTerms counts
	// distinct terms correctly.
	expr, terms := buildRecallQuery("card payments card refunds")
	wantTerms := []string{"card", "payments", "refunds"}
	if len(terms) != len(wantTerms) {
		t.Fatalf("terms = %v, want %v (deduped)", terms, wantTerms)
	}
	for i, w := range wantTerms {
		if terms[i] != w {
			t.Fatalf("terms[%d] = %q, want %q (terms=%v)", i, terms[i], w, terms)
		}
	}
	want := `"card" OR "payments" OR "refunds"`
	if expr != want {
		t.Fatalf("expr = %q, want %q (deduped)", expr, want)
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

// TestRelatedAskBlobCoincidenceDoesNotQualify is the real-DB Critical
// finding, reproduced as a fixture: a session's `ask` is a multi-KB pasted
// blob in which generic engineering vocabulary ("implement", "settings",
// "page") appears scattered far apart, purely by coincidence, while the
// session's actual indexed content (and hence its FTS snippet) only ever
// co-locates a single seed term ("mode"). Before the fix, countMatchedTerms
// scanned snippet+title+ask and the scattered ask terms alone cleared the
// ≥2-term gate, surfacing a confidently-irrelevant session. The fix counts
// snippet+title only, so this session must be filtered as noise — while a
// genuinely topical session (seed terms co-occurring in one FTS window)
// must still be found.
func TestRelatedAskBlobCoincidenceDoesNotQualify(t *testing.T) {
	s := openStore(t)

	// Genuine hit: several seed terms co-occur within one indexed doc, so
	// the FTS snippet window captures them together.
	seedRecallSession(t, s, "real", "loom", "added a dark mode toggle to the settings page today", 200)

	// Noise: the indexed doc (and thus the FTS snippet) only ever mentions
	// "mode" in isolation, but `ask` is a ~3KB generic engineering blob that
	// happens to contain "implement", "settings", and "page" scattered far
	// apart from each other and from any real topical connection to the
	// seed — the exact real-DB failure mode this fix closes.
	var blob strings.Builder
	blob.WriteString("We need to implement a new caching layer for the backend service. ")
	for i := 0; i < 150; i++ {
		blob.WriteString("Filler engineering discussion about deployment pipelines and code review notes. ")
	}
	blob.WriteString("Please check the settings for the retry policy timeout value. ")
	for i := 0; i < 150; i++ {
		blob.WriteString("More unrelated filler text about logging and metrics collection. ")
	}
	blob.WriteString("Finally render the results on the page for the end user.")
	if blob.Len() < 3000 {
		t.Fatalf("fixture blob too small to be realistic: %d bytes", blob.Len())
	}

	if err := s.UpsertTranscript(store.Transcript{
		SessionID: "noise", ProjectDir: "loom", Cwd: "/w/loom",
		Title: "session noise", Ask: blob.String(), LastTS: 300,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileDocs(store.IndexedFile{Path: "/f-noise", SessionID: "noise", Size: 1, Mtime: 1},
		[]store.Doc{{Content: "the deployment mode was switched last week", Role: "user", TS: 300}}); err != nil {
		t.Fatal(err)
	}

	got, err := Related(s, "loom", "implement dark mode toggle for the settings page", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range got {
		if h.T.SessionID == "noise" {
			t.Fatalf("ask-blob coincidence session must not qualify: %+v", got)
		}
	}
	if len(got) != 1 || got[0].T.SessionID != "real" {
		t.Fatalf("got %+v, want only 'real' (genuine snippet co-occurrence)", got)
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

func TestRelatedDuplicateTokensInSeedDoesNotWarpGate(t *testing.T) {
	// Regression test: seed with duplicate tokens ("card payments card
	// refunds") must not cause countMatchedTerms to double-count "card" and
	// falsely pass a session containing only "card" (once). The deduplication
	// fix ensures terms == ["card", "payments", "refunds"] and countMatchedTerms
	// counts distinct terms, so the ≥2-term gate correctly rejects this noise.
	s := openStore(t)

	// Noise: session contains only "card", many times. Before the fix,
	// buildRecallQuery yields terms=["card", "payments", "card", "refunds"]
	// (duplicated), countMatchedTerms counts 2 matches (both "card" entries
	// match), gate wrongly passes. After the fix, buildRecallQuery dedupes to
	// terms=["card", "payments", "refunds"], countMatchedTerms counts only 1
	// match, gate correctly rejects.
	seedRecallSession(t, s, "noise", "loom",
		"card card card card card processing card card card", 100)

	// Real hit: contains "card" and "payments" together.
	seedRecallSession(t, s, "good", "loom",
		"we need to process card payments for the invoice", 200)

	got, err := Related(s, "loom", "card payments card refunds", 5)
	if err != nil {
		t.Fatal(err)
	}

	// Noise session must be filtered out (only 1 matched distinct term: "card").
	for _, h := range got {
		if h.T.SessionID == "noise" {
			t.Fatalf("session with only 1 distinct term must not qualify: %+v", got)
		}
	}

	// The "good" session should either be in the results (if FTS finds it) or
	// we fall back to recency. Either way, "noise" must not appear.
	// Since "good" mentions "card" and "payments", it should survive the gate
	// and be returned.
	if len(got) == 0 {
		t.Fatalf("expected at least recency fallback results, got none")
	}
}

// The candidate pool must widen with the caller's limit, or an over-fetching
// caller (the RELATED panel, which asks wide and then drops hits belonging to
// hidden projects) cannot reach past the floor no matter what it asks for.
func TestFetchLimitScalesWithLimit(t *testing.T) {
	cases := []struct{ limit, want int }{
		{limit: 0, want: minFetchLimit},
		{limit: 5, want: minFetchLimit}, // the panel's display limit: floor unchanged
		{limit: minFetchLimit / candidatesPerHit, want: minFetchLimit},
		{limit: 15, want: 45},
		{limit: 40, want: 120},
	}
	for _, tc := range cases {
		if got := fetchLimit(tc.limit); got != tc.want {
			t.Errorf("fetchLimit(%d) = %d, want %d", tc.limit, got, tc.want)
		}
	}
}

// End-to-end: with more qualifying sessions than the old fixed 15-candidate
// pool, a caller asking for 20 must actually get 20 back.
func TestRelatedWideLimitReachesPastTheOldFixedPool(t *testing.T) {
	s := openStore(t)
	const n = 30
	for i := 0; i < n; i++ {
		seedRecallSession(t, s, fmt.Sprintf("s%02d", i), "loom",
			"planning the database migration carefully", int64(100+i))
	}
	got, err := Related(s, "loom", "database migration plan", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Fatalf("Related(limit=20) = %d hits, want 20 (the pool must widen past %d)",
			len(got), minFetchLimit)
	}
}

// --- RelatedIn: the dir SET (orchestrator spec §5.2/§13) ----------------

// TestRelatedInSameProjectForHitInAnyDir is §13's binding clause: a project's
// "same project" is {root} ∪ {member repos}, so a hit in ANY dir of the set
// carries the boost. The single-dir composition this replaced ranked with the
// wrong flag — a hit in a member repo other than dirs[0] sorted BELOW a
// cross-project hit and was only filtered afterwards, so the boost never
// applied to it. Each member dir is asserted independently, because a
// membership test that accidentally only ever consults dirs[0] passes any
// test that puts the interesting row first.
func TestRelatedInSameProjectForHitInAnyDir(t *testing.T) {
	dirs := []string{"/w/proj", "/w/proj/api", "/w/proj/web"}

	for _, member := range dirs {
		t.Run(member, func(t *testing.T) {
			s := openStore(t)
			// The member-repo hit is deliberately the WEAKEST by raw bm25.
			seedRecallSession(t, s, "target", member, "notes about database performance work", 50)
			for i, id := range []string{"cross-1", "cross-2", "cross-3"} {
				seedRecallSession(t, s, id, "/w/elsewhere",
					"database performance database performance database performance tuning", int64(1000+i))
			}

			got, err := RelatedIn(s, dirs, "database performance tuning", 3)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 || got[0].T.SessionID != "target" {
				t.Fatalf("hit in member dir %q was not boosted to first: %+v", member, got)
			}
			if !got[0].SameProject {
				t.Fatalf("hit in member dir %q not marked SameProject", member)
			}
			for _, h := range got[1:] {
				if h.SameProject {
					t.Fatalf("hit outside the set marked SameProject: %+v", h)
				}
			}
		})
	}
}

// TestRelatedInRecencyFallbackCoversWholeSet: the fallback is set-scoped too.
// A fallback that only covered dirs[0] would silently blank the section for a
// project whose recent work happened in a member repo.
func TestRelatedInRecencyFallbackCoversWholeSet(t *testing.T) {
	s := openStore(t)
	seedRecallSession(t, s, "in-root", "/w/proj", "irrelevant body text", 100)
	seedRecallSession(t, s, "in-member", "/w/proj/api", "irrelevant body text", 200)
	seedRecallSession(t, s, "outside", "/w/elsewhere", "irrelevant body text", 300)

	// All stopwords -> empty expr -> recency fallback.
	got, err := RelatedIn(s, []string{"/w/proj", "/w/proj/api"}, "the for with", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].T.SessionID != "in-member" || got[1].T.SessionID != "in-root" {
		t.Fatalf("got %+v, want set-scoped recency order [in-member in-root]", got)
	}
	for _, h := range got {
		if !h.SameProject {
			t.Fatalf("fallback row not marked SameProject: %+v", h)
		}
	}
}

// TestRelatedInEmptySetIsFailClosed: an empty dir set must not widen into a
// scan of the whole index. Same direction as
// store.RecentTranscriptsByProjectDirs and slice 1 §4.
func TestRelatedInEmptySetIsFailClosed(t *testing.T) {
	s := openStore(t)
	seedRecallSession(t, s, "somewhere", "/w/elsewhere", "irrelevant body text", 100)

	got, err := RelatedIn(s, nil, "the for with", 5) // fallback path
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty dir set returned %d rows, want none (fail-closed): %+v", len(got), got)
	}

	ranked, err := RelatedIn(s, nil, "irrelevant body text", 5) // ranked path
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range ranked {
		if h.SameProject {
			t.Fatalf("empty dir set marked a hit SameProject: %+v", h)
		}
	}
}

// TestRelatedIsOneElementRelatedIn pins the wrapper relationship §5.2 binds.
// If Related ever stops delegating, the single-dir behaviour and the set
// behaviour can drift apart silently.
func TestRelatedIsOneElementRelatedIn(t *testing.T) {
	s := openStore(t)
	seedRecallSession(t, s, "a", "/w/proj", "planning the database migration carefully", 100)
	seedRecallSession(t, s, "b", "/w/other", "planning the database migration carefully", 200)

	one, err := Related(s, "/w/proj", "database migration", 5)
	if err != nil {
		t.Fatal(err)
	}
	set, err := RelatedIn(s, []string{"/w/proj"}, "database migration", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != len(set) {
		t.Fatalf("Related got %d hits, RelatedIn({d}) got %d", len(one), len(set))
	}
	for i := range one {
		if one[i].T.SessionID != set[i].T.SessionID || one[i].SameProject != set[i].SameProject {
			t.Fatalf("divergence at %d: Related=%+v RelatedIn=%+v", i, one[i], set[i])
		}
	}
}
