package ui

import (
	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

// Projects is the visibility/attribution authority as the TUI consumes it
// (spec §4, §6.3). Narrowed to the single method the TUI needs — the whole
// point of internal/projects is that nothing re-derives attribution, so the
// TUI takes the resolver and never looks at a project row itself.
//
// An interface rather than *projects.Service so a test can supply a resolver
// built from literal rows without standing up a DB, and so it is visible at a
// glance that the TUI never writes a project flag: the solo GESTURE is
// GUI-only (§8), the solo FLAG lives in loom.db and is honoured here.
type Projects interface {
	Resolver() (*projects.Resolver, error)
}

// Over-fetch limits (spec §6.3). store.Recent and SearchSessions apply their
// LIMIT in SQL, so a presentation-layer filter applied AFTER the cap silently
// truncates the list — badly under solo, where most rows are hidden and a
// 10-row Finished list can come back with one entry. Fetch wide, filter, then
// trim, the pattern memory/recall.go's fetchLimit already uses.
//
// The predicate is deliberately NOT pushed into SQL: Recent feeds Engine.Poll,
// and filtering there would break §6.2a's engine-independence invariant, and a
// LIKE join cannot express longest-prefix attribution anyway.
//
// The wide fetch runs only while something is actually hidden, so the normal
// case pays nothing.
const (
	// recentDisplayLimit matches status.Engine's own Recent(10). A mismatch
	// would make the Finished list visibly change length the moment a project
	// is hidden — itself a leak signal during a screen-share.
	recentDisplayLimit = 10
	recentFetchLimit   = 120

	searchDisplayLimit = 30
	searchFetchLimit   = 120

	// panelFetchLimit is deliberately far below the 120 above. memory.Related
	// scales its candidate pool with the limit it is handed (3 per requested
	// hit) and does one GetTranscript point lookup per candidate surviving its
	// ≥2-matched-term gate, so unlike the two pure-SQL over-fetches this one
	// costs lookups. 45 asked for is 135 candidates ranked for a 5-row panel —
	// generous enough that solo can hide most of the index and the panel still
	// fills, cheap enough for an on-demand launcher keystroke path.
	panelFetchLimit = 45
)

// visibility returns the current authority, read through on every call
// because loom.db is the runtime source of truth: a project hidden from the
// GUI must take effect in an already-running TUI within one poll, and a
// startup snapshot is exactly the bug §7 calls out for launch targets.
//
// On a read error the LAST GOOD resolver is reused rather than degrading to
// "nothing hidden": a transient DB error must not un-hide a client mid-share.
// A nil authority (Deps{} in tests, or a frontend that has none) means no
// filtering at all — hiding is opt-in, so an app with no project service
// behaves exactly as it did before this slice.
func (a *App) visibility() *projects.Resolver {
	if a.deps.Projects == nil {
		return nil
	}
	r, err := a.deps.Projects.Resolver()
	if err != nil {
		return a.res
	}
	a.res = r
	return r
}

// sessionDirs is the directory set §6.1 evaluates visibility over: cwd ∪
// add_dirs. A multi-repo session whose cwd sits in a visible project while it
// edits a hidden project's repo is hidden — the whole row goes with the one
// directory that must not be on screen.
//
// The cwd is included even when empty: an empty cwd cannot be attributed, and
// the resolver's fail-closed branch is what should decide that, not a silent
// skip here.
func sessionDirs(r store.SessionRow) []string {
	return append([]string{r.Cwd}, session.DecodeAddDirs(r.AddDirs)...)
}

func visibleSession(res *projects.Resolver, r store.SessionRow) bool {
	if res == nil {
		return true
	}
	return res.Visible(sessionDirs(r)...)
}

// filterSnapshot applies §6.1's predicate to one poll result. This one place
// covers the rail, the Finished/RECENT list and the wall: rebuildRows and
// applyWallOrder both read a.snap, so a surface added later inherits the
// filter instead of needing to remember it (§4: the surface that forgets a
// branch still passes its own test).
//
// It returns a filtered COPY and never writes: hiding is presentation, never
// behaviour (§6.2a). The engine has already polled these sessions and already
// persisted their status transitions by the time this runs.
//
// suppressed is how many of the needs-you transitions belonged to a hidden
// project. It is returned rather than discarded because §6.4 says attention
// must STILL escalate from a hidden project, degraded to a label-free body:
// naming the client is the leak, but swallowing the signal means a demo in
// which the user never learns an agent is blocked. cmd/loom-gui/notify.go
// takes the same count for the same reason — the two frontends run against one
// DB and must not disagree about what a hidden project does.
func (a *App) filterSnapshot(snap status.Snapshot) (_ status.Snapshot, suppressed int) {
	res := a.visibility()
	if res == nil || !res.Filtering() {
		return snap, 0
	}

	live := make([]status.Row, 0, len(snap.Live))
	shown := make(map[string]bool, len(snap.Live))
	for _, r := range snap.Live {
		if !visibleSession(res, r.SessionRow) {
			continue
		}
		shown[r.Name] = true
		live = append(live, r)
	}

	// NewlyNeedsYou is filtered explicitly rather than left to
	// needsYouLabels' join against Live. The join would drop a hidden name
	// today as a side effect, but the field is the notification's identity
	// input and a future consumer that renders it directly would re-leak the
	// one thing hiding exists to conceal — a banner naming a client, raised
	// by the OTHER binary's terminal, mid-screen-share.
	newly := make([]string, 0, len(snap.NewlyNeedsYou))
	for _, name := range snap.NewlyNeedsYou {
		if shown[name] {
			newly = append(newly, name)
			continue
		}
		// Only a name that HAS a live row was really suppressed. A name with
		// no row at all cannot come from a real poll (both come from the same
		// pass over Live) and is dropped silently rather than escalated as a
		// phantom banner.
		if hasRow(snap.Live, name) {
			suppressed++
		}
	}

	snap.Live = live
	snap.NewlyNeedsYou = newly
	snap.Recent = a.visibleRecent(res, snap.Recent)
	return snap, suppressed
}

func hasRow(rows []status.Row, name string) bool {
	for _, r := range rows {
		if r.Name == name {
			return true
		}
	}
	return false
}

// visibleRecent re-reads the Finished list wide and trims after filtering.
// The engine's own Recent(10) was capped in SQL before anything knew about
// projects, so filtering the snapshot's copy in place would shrink the list
// by exactly the hidden rows. A store read failure degrades to filtering what
// the snapshot already carried: a short list is a cosmetic loss, showing a
// hidden project is not.
func (a *App) visibleRecent(res *projects.Resolver, fallback []store.SessionRow) []store.SessionRow {
	rows := fallback
	if st := a.deps.Store; st != nil {
		if wide, err := st.Recent(recentFetchLimit); err == nil {
			rows = wide
		}
	}
	out := make([]store.SessionRow, 0, recentDisplayLimit)
	for _, r := range rows {
		if !visibleSession(res, r) {
			continue
		}
		if out = append(out, r); len(out) == recentDisplayLimit {
			break
		}
	}
	return out
}

// visibleHits filters search results by the transcript's cwd. Transcripts
// carry no add_dirs (they are written by the indexer from JSONL Loom does not
// own), so cwd is the whole directory set available here — an unattributable
// cwd, including the one live row with cwd=”, is hidden while filtering.
func visibleHits(res *projects.Resolver, hits []store.SearchHit, limit int) []store.SearchHit {
	if res == nil || !res.Filtering() {
		if len(hits) > limit {
			return hits[:limit]
		}
		return hits
	}
	out := make([]store.SearchHit, 0, limit)
	for _, h := range hits {
		if !res.Visible(h.Cwd) {
			continue
		}
		if out = append(out, h); len(out) == limit {
			break
		}
	}
	return out
}

// visibleRelated is the recall/RELATED panel's half of the same rule. The
// panel is seeded from the launcher's selected repo, which is itself already
// scoped — but recall crosses projects by design (that is its value), so the
// hits must be filtered independently of the seed's project.
func visibleRelated(res *projects.Resolver, hits []memory.RelatedHit, limit int) []memory.RelatedHit {
	if res == nil || !res.Filtering() {
		if len(hits) > limit {
			return hits[:limit]
		}
		return hits
	}
	out := make([]memory.RelatedHit, 0, limit)
	for _, h := range hits {
		if !res.Visible(h.T.Cwd) {
			continue
		}
		if out = append(out, h); len(out) == limit {
			break
		}
	}
	return out
}

// visibleRepos scopes the launch surface — the launcher's project field and
// the fan-out checklist — to the projects currently on screen. Offering a
// hidden client's repo in a picker is the same leak as showing its session,
// and under solo the picker is the fastest way to accidentally launch back
// into the project just put out of view.
//
// A repo path no project claims is hidden while filtering, the resolver's
// fail-closed rule: a launch target that cannot be attributed cannot be
// proven safe to show.
func (a *App) visibleRepos() []registry.Repo {
	res := a.visibility()
	if res == nil || !res.Filtering() {
		return a.deps.Repos
	}
	out := make([]registry.Repo, 0, len(a.deps.Repos))
	for _, r := range a.deps.Repos {
		if res.Visible(r.Path) {
			out = append(out, r)
		}
	}
	return out
}
