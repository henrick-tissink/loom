package orchestrator

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/store"
)

// maxRecentRows caps §4's `## Recent work` section at 40 rows (the byte cap is
// applied at render time). Both caps are binding and both are tested: a row cap
// alone lets forty pathological outcomes blow the section's 16 KB, and a byte
// cap alone lets four hundred one-word outcomes crowd out everything else.
const maxRecentRows = 40

// recallFanout widens the candidate pull before filtering. memory.Related ranks
// over the WHOLE index and trims to the caller's limit, and this section then
// drops everything outside the project's dir set, everything invisible and
// every orchestrator's own transcript — so asking for exactly 40 would hand
// back a short section with qualifying rows sitting just below the cut. Same
// reasoning, and the same shape, as recall's own candidatesPerHit.
const recallFanout = 6

// RecentRow is one line of §4's `## Recent work`.
type RecentRow struct {
	SessionID string
	LastTS    int64
	Label     string
	Title     string
	// Outcome is already resolved to what the line will say: the transcript's
	// outcome, or its ask when AskUsable allows, or the explicit
	// "(no outcome recorded)". The resolution happens here rather than in the
	// renderer so §13's fallback cases can be asserted on data, not on prose.
	Outcome string
}

// Render is the fixed one-line form §4 binds:
// `2026-07-14 · bankenstein · <title> — <outcome>`.
//
// UTC, always. A brief assembled in two timezones must be byte-identical
// (§5.3), and a local-time date would make the golden test a function of where
// the machine is standing.
func (r RecentRow) Render() string {
	date := time.Unix(r.LastTS, 0).UTC().Format("2006-01-02")
	title := r.Title
	if title == "" {
		title = "(untitled)"
	}
	return date + " · " + r.Label + " · " + title + " — " + r.Outcome
}

// recentSource is the slice of the store `Recent work` reads. Narrowed to an
// interface for the same reason projects.Store is: the ranking and filtering
// rules are the part worth testing, and a fake makes the echo-chamber guard
// assertable without a transcript file on disk.
type recentSource interface {
	RecentTranscriptsByProjectDirs(dirs []string, limit int) ([]store.Transcript, error)
	TaggedSessions() ([]store.SessionTag, error)
}

// orchTag is the tag a spawned orchestrator's session row carries. Everything
// in this package that needs to know "is this an orchestrator" asks the tag,
// never the orchestrators table: that table holds one row per project — the
// current or last orchestrator — so generation N−2's transcript is invisible to
// it, and the echo-chamber guard has to hold for every generation, not the
// latest one.
const orchTag = "orch"

// hasOrchTag tests the tag as a TOKEN, not as a substring. Tags is a free-form
// field (`fan:<group>`, `wf:<id>`) with no schema, and a substring test would
// silently swallow a user's own "orchid" or "reorch" tag — dropping their real
// sessions out of every brief with nothing anywhere saying why.
func hasOrchTag(tags string) bool {
	for _, t := range strings.FieldsFunc(tags, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		if t == orchTag {
			return true
		}
	}
	return false
}

// orchSessionIDs is the echo-chamber guard's exclusion set (§5.4, binding): the
// claude session ids of every `orch`-tagged session row, live or finished.
// `transcripts.session_id` joins `sessions.claude_session_id`, so this set is
// exactly the transcripts that must never reach a successor's brief.
//
// The guarantee, stated plainly: an orchestrator's own transcript is never fed
// back to its successor; the NOTES are. The notes (§3) are the deliberate
// channel between generations and are agent-authored on purpose; the transcript
// is a byproduct (§2) and stays one. Without this, generation N+1's `Recent
// work` would quote generation N's summary of a project generation N learned
// about from generation N−1's brief — recall compounding across generations,
// which is what memory.RecallMarker exists to prevent on the seed path.
//
// One query, over tagged rows only. This previously reconstructed the whole
// sessions table as Live() ∪ Recent(CountEnded()) — three round-trips, two of
// them full-table, both materializing every column of every session in the
// install to read one field. It also had a sharp edge: Recent takes a limit, so
// it had to be handed the exact count of terminal rows, and any drift between
// that count and reality would silently un-guard the OLDEST orchestrators on a
// mature install — precisely the failure this function exists to prevent, in
// the function that exists to prevent it. store.TaggedSessions has no limit to
// get wrong.
//
// The token test stays HERE rather than being pushed into SQL: see
// store.TaggedSessions for why a LIKE filter would be actively wrong.
func orchSessionIDs(src recentSource) (map[string]bool, error) {
	out := map[string]bool{}
	tagged, err := src.TaggedSessions()
	if err != nil {
		return nil, err
	}
	for _, r := range tagged {
		if hasOrchTag(r.Tags) {
			out[r.ClaudeSessionID] = true
		}
	}
	return out, nil
}

// recentWorkInput is everything the section needs, gathered by the caller so
// this stays a pure-ish function of its arguments (§5.3 determinism).
type recentWorkInput struct {
	dirs     []string          // {project root} ∪ {member repo paths}
	labels   map[string]string // dir → repo label, for the middle column
	intent   string            // user's typed intent, "" for the recency path
	resolver *projects.Resolver
	orchIDs  map[string]bool
}

// recentWork builds §4's `## Recent work` rows.
//
// Two paths, per §5.2: with a typed intent, recall's existing ranking runs with
// that intent as the seed; without one, plain recency over the dir set — which
// is precisely recall.go's own fallback branch, not a new code path.
//
// memory.RelatedIn now applies the dir-set scoping internally, so the ranked
// branch is one call. It previously composed the behaviour here — rank with
// memory.Related(dirs[0]), then filter the hits against the set — and that was
// not equivalent, it was a bug: SameProject is a SORT KEY, so a hit in a member
// repo other than dirs[0] was ranked as cross-project, sorted below genuinely
// unrelated work, and only then filtered. The boost has to be applied during
// the sort to mean anything. Fixed in memory.RelatedIn, where the sort is.
func recentWork(st *store.Store, in recentWorkInput) ([]RecentRow, error) {
	var trs []store.Transcript
	if in.intent != "" && len(in.dirs) > 0 {
		hits, err := memory.RelatedIn(st, in.dirs, in.intent, maxRecentRows*recallFanout)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			// RelatedIn RANKS over the whole index and still returns
			// cross-project hits — the dir set is a boost, not a WHERE clause.
			// The brief is scoped to this project (§5.2), so membership is
			// still enforced here; SameProject is now exactly that predicate,
			// so this re-tests nothing and cannot disagree with the sort.
			if h.SameProject {
				trs = append(trs, h.T)
			}
		}
	}
	if len(trs) == 0 {
		// Both the no-intent case and "the ranked path found nothing inside
		// this project" land here. recall.go makes the same choice for the same
		// reason: an empty panel is worse than a recent one.
		rows, err := st.RecentTranscriptsByProjectDirs(in.dirs, maxRecentRows*recallFanout)
		if err != nil {
			return nil, err
		}
		trs = rows
	}

	return filterRecent(trs, in), nil
}

// filterRecent applies the drops, the dedupe, the ordering and the row cap. It
// is separated from the queries so §13's echo-chamber and visibility cases can
// be driven from literal transcripts.
func filterRecent(trs []store.Transcript, in recentWorkInput) []RecentRow {
	seen := make(map[string]bool, len(trs))
	rows := make([]RecentRow, 0, len(trs))
	for _, t := range trs {
		if seen[t.SessionID] {
			continue // the same session indexed under two of the project's dirs
		}
		if in.orchIDs[t.SessionID] {
			continue // §5.4, the echo-chamber guard
		}
		if in.resolver != nil && !in.resolver.Visible(visibilityDirs(t)...) {
			// Hidden and solo-suppressed rows never appear, and an
			// unattributable row is dropped rather than included — fail-closed,
			// inherited from slice 1 §4.
			continue
		}
		seen[t.SessionID] = true
		rows = append(rows, RecentRow{
			SessionID: t.SessionID,
			LastTS:    t.LastTS,
			Label:     recentLabel(in.labels, t),
			Title:     memory.CleanText(t.Title),
			Outcome:   recentOutcome(t),
		})
	}
	// Descending recency, session id as the tie-break so two rows sharing a
	// timestamp cannot reorder between two assemblies of identical inputs
	// (§5.3 — a golden brief that flips row order is not a golden brief).
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].LastTS != rows[j].LastTS {
			return rows[i].LastTS > rows[j].LastTS
		}
		return rows[i].SessionID < rows[j].SessionID
	})
	if len(rows) > maxRecentRows {
		rows = rows[:maxRecentRows]
	}
	return rows
}

// visibilityDirs is the directory set a transcript is judged by. Cwd is
// included when it differs from ProjectDir: Visible is an AND over the set, so
// a session working under a hidden path is hidden even when its project dir is
// not — the same whole-directory-set rule slice 1 applied to add_dirs.
func visibilityDirs(t store.Transcript) []string {
	if t.Cwd != "" && t.Cwd != t.ProjectDir {
		return []string{t.ProjectDir, t.Cwd}
	}
	return []string{t.ProjectDir}
}

// recentOutcome resolves what the line says after the em dash (§5.2 step 5).
//
// The Ask fallback goes through memory.AskUsable rather than being printed
// raw: Extraction.Ask can carry a `<command-…>` wrapper or a `Caveat:`
// preamble through result()'s fallback path, and quoting one of those to an
// orchestrator as "what that session was about" is confident noise.
func recentOutcome(t store.Transcript) string {
	if o := memory.CleanText(t.Outcome); o != "" {
		return o
	}
	if a := memory.CleanText(t.Ask); a != "" && memory.AskUsable(a) {
		return a
	}
	return "(no outcome recorded)"
}

// recentLabel names the repo a row happened in. The project's own repo labels
// are preferred — they are the stable identifiers saved workflows resolve
// through — and a directory the project does not label falls back to its
// basename, which is what the rest of Loom does for an unlabelled path.
func recentLabel(labels map[string]string, t store.Transcript) string {
	if l := labels[t.ProjectDir]; l != "" {
		return l
	}
	if t.ProjectDir == "" {
		return "(unknown)"
	}
	return filepath.Base(t.ProjectDir)
}
