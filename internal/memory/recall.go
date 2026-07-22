// Recall engine (Phase 2.5, spec
// docs/superpowers/specs/2026-07-04-recall-design.md §2/§6): the launcher's
// manual-pull RELATED panel. buildRecallQuery turns a free-text seed into a
// recall-specific FTS5 match expression (deliberately NOT
// store.sanitizeFTSQuery, whose implicit-AND + trailing-`*` shape returns
// zero hits for natural-sentence seeds — verified on the real index).
// Related runs that query (or falls back to same-project recency), applies
// the ≥2-matched-term relevance gate, and ranks same-project above
// cross-project at equal match tier.
package memory

import (
	"regexp"
	"sort"
	"strings"

	"github.com/henricktissink/loom/internal/store"
)

// RelatedHit is one row of the RELATED panel (spec §6): a transcript plus
// recall-specific display metadata. Snippet is empty for recency-fallback
// entries (spec M4) — the launcher renders an outcome preview instead in
// that shape. MatchedTerms is 0 for recency-fallback entries (no query to
// match against).
type RelatedHit struct {
	T            store.Transcript
	Snippet      string
	SameProject  bool
	MatchedTerms int
}

// minFetchLimit is the floor on how many candidates Related pulls from
// SearchSessionsRaw before ranking and trimming to the caller's limit (spec
// §6): wide enough that the same-project boost can promote a hit ranked lower
// by raw bm25 into the visible set.
//
// candidatesPerHit widens that floor for callers asking for a large limit.
// A fixed 15 made over-fetching impossible: a caller that asks for 15 in order
// to filter down to 5 (the projects-slice RELATED panel, which drops hits
// belonging to hidden projects) got at most 15 candidates, of which the ≥2-term
// gate and the filter could leave fewer than 5 — a visibly short panel with
// qualifying hits sitting just below the cut. The pool now scales with what was
// asked for, so the widest fetch is the caller's decision.
//
// A multiplier rather than a second parameter: every candidate that survives
// the gate costs a GetTranscript point lookup, so the cost is bounded by the
// caller's own limit instead of by an independent knob nothing would keep in
// step with it. Small limits are unaffected — 5 still fetches the floor of 15.
const (
	minFetchLimit    = 15
	candidatesPerHit = 3
)

func fetchLimit(limit int) int {
	if n := limit * candidatesPerHit; n > minFetchLimit {
		return n
	}
	return minFetchLimit
}

// stopwords is dropped from recall seeds before querying (spec §2): common
// function words that would otherwise OR their way into confident-noise
// hits once quoted and OR-joined with the seed's real content terms.
var stopwords = map[string]bool{
	"the": true, "this": true, "that": true, "with": true, "for": true,
	"from": true, "into": true, "have": true, "will": true, "what": true,
	"when": true, "where": true, "which": true, "there": true, "their": true,
	"about": true, "would": true, "could": true, "should": true, "your": true,
	"then": true, "than": true, "them": true, "these": true, "those": true,
	"were": true, "been": true, "being": true, "does": true, "just": true,
	"also": true, "because": true, "while": true, "after": true, "before": true,
}

// tokenRe splits a seed on any run of non-letter/non-digit characters
// (spec §2: "tokenize seed"), unicode-aware.
var tokenRe = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// buildRecallQuery builds recall's own FTS5 match expression from a seed
// (spec §2, binding, empirically derived against the real index by a
// red-team): tokenize on non-letter/digit boundaries; drop tokens under 4
// characters and the stopword list above; deduplicate surviving tokens while
// preserving first-occurrence order; quote-escape each surviving term
// (doubling any embedded quote, mirroring store.sanitizeFTSQuery's
// escaping); OR-join the quoted terms with NO trailing `*` (recall wants
// exact-term matches across sessions, not prefix-complete the seed's last
// word). Fewer than 2 surviving terms returns an empty expr — the signal
// callers use to fall back to same-project recency instead of running a
// single-term (or zero-term) query that would surface confident noise.
//
// terms is returned (lowercased, in seed order, deduplicated) so the caller
// can compute per-hit matched-term counts without re-tokenizing, and the
// ≥2-matched-term gate correctly counts distinct terms.
func buildRecallQuery(seed string) (expr string, terms []string) {
	seen := make(map[string]bool)
	for _, tok := range tokenRe.Split(strings.ToLower(seed), -1) {
		if len(tok) < 4 || stopwords[tok] || seen[tok] {
			continue
		}
		seen[tok] = true
		terms = append(terms, tok)
	}
	if len(terms) < 2 {
		return "", terms
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " OR "), terms
}

// countMatchedTerms is recall's client-side approximation (documented, not
// exact — bm25/rank don't expose a per-term match count at the SQL level)
// of how many distinct query terms co-occur in what actually earned the FTS
// hit: the snippet and the session title, joined and searched
// case-insensitively via substring containment. terms are already lowercase
// (from buildRecallQuery).
//
// Deliberately EXCLUDES the session's ask (verified on the real DB, Critical
// finding): ask fields are often multi-KB pasted blobs, and generic
// engineering vocabulary ("implement", "settings", "mode", "page") is common
// enough to scatter across such a blob by sheer coincidence — clearing the
// ≥2-term gate for sessions with no genuine relation to the seed (e.g. the
// probe "implement dark mode toggle for the settings page" matched 5
// confidently-irrelevant sessions this way). The FTS snippet, by contrast,
// is a ~12-token window centered on an actual match, so ≥2 distinct terms
// found there reflects real co-occurrence near the matched content, not
// coincidental scatter across a large blob. Title is small and curated
// enough to carry the same guarantee.
func countMatchedTerms(terms []string, fields ...string) int {
	haystack := strings.ToLower(strings.Join(fields, "\x00"))
	n := 0
	for _, t := range terms {
		if strings.Contains(haystack, t) {
			n++
		}
	}
	return n
}

// Related is the recall engine (spec §2/§6). It builds a recall-specific
// FTS query from seed; an unusable seed (fewer than 2 surviving terms)
// falls straight back to same-project recency. Otherwise it fetches
// fetchLimit(limit) candidates, computes each hit's matched-term count, and drops
// any hit matching fewer than 2 terms (spec §2: "require ≥2 matched
// content terms for an FTS hit to show at all" — kills the
// confident-noise failure mode where OR + stopwords surface unrelated
// sessions). If NO hit survives that gate, it also falls back to
// recency. Surviving hits are ranked SameProject desc, MatchedTerms desc,
// stable bm25 order (the order SearchSessionsRaw returned them in, which
// is itself bm25-best-first), LastTS desc — then trimmed to limit.
//
// Per-hit full Transcript is fetched via GetTranscript (disclosed
// implementation choice, see task report): SearchHit only carries a subset
// of transcript fields (no FirstTS/MsgCount/Outcome/Files/LLMSummary/
// SummaryAt), and the candidate pool stays small — a few tens of rows,
// bounded by the caller's own limit — for what is an on-demand,
// user-triggered launcher action, not a hot path, so a point lookup per
// qualifying candidate is simpler and safer than widening
// SearchHit (a struct shared with SearchSessions) to carry every
// Transcript field recall might need.
func Related(st *store.Store, projectDir, seed string, limit int) ([]RelatedHit, error) {
	expr, terms := buildRecallQuery(seed)
	if expr == "" {
		return recencyFallback(st, projectDir, limit)
	}

	hits, err := st.SearchSessionsRaw(expr, fetchLimit(limit))
	if err != nil {
		return nil, err
	}

	type ranked struct {
		hit   RelatedHit
		order int // position in hits: stable bm25 order (ascending = better)
	}
	var qualifying []ranked
	for i, h := range hits {
		matched := countMatchedTerms(terms, h.Snippet, h.Title)
		if matched < 2 {
			continue
		}
		tr, ok, err := st.GetTranscript(h.SessionID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // transcript vanished between the FTS hit and the lookup
		}
		qualifying = append(qualifying, ranked{
			hit: RelatedHit{
				T:            tr,
				Snippet:      h.Snippet,
				SameProject:  h.ProjectDir == projectDir,
				MatchedTerms: matched,
			},
			order: i,
		})
	}

	if len(qualifying) == 0 {
		return recencyFallback(st, projectDir, limit)
	}

	sort.SliceStable(qualifying, func(i, j int) bool {
		a, b := qualifying[i], qualifying[j]
		if a.hit.SameProject != b.hit.SameProject {
			return a.hit.SameProject // same-project sorts first
		}
		if a.hit.MatchedTerms != b.hit.MatchedTerms {
			return a.hit.MatchedTerms > b.hit.MatchedTerms
		}
		if a.order != b.order {
			return a.order < b.order // stable bm25 order
		}
		return a.hit.T.LastTS > b.hit.T.LastTS
	})

	if len(qualifying) > limit {
		qualifying = qualifying[:limit]
	}
	out := make([]RelatedHit, len(qualifying))
	for i, r := range qualifying {
		out[i] = r.hit
	}
	return out, nil
}

// recencyFallback is the same-project recency path (spec §2/§6): used both
// when the seed can't produce a usable recall query and when it produces
// one but zero hits survive the ≥2-matched-term gate.
func recencyFallback(st *store.Store, projectDir string, limit int) ([]RelatedHit, error) {
	trs, err := st.RecentTranscriptsByProjectDir(projectDir, limit)
	if err != nil {
		return nil, err
	}
	out := make([]RelatedHit, len(trs))
	for i, tr := range trs {
		out[i] = RelatedHit{T: tr, SameProject: true}
	}
	return out, nil
}
