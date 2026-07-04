// Launcher RELATED panel (Phase 2.5 Recall, spec
// docs/superpowers/specs/2026-07-04-recall-design.md §3-§6): the launcher
// form (launcher.go) gains a panel of recall hits below it. This file holds
// the panel's own state helpers (panelRow assembly, include toggling, the
// M3 project-label helper) and the launch-time seed assembly
// (buildSeedWithRecall, spec §4). The focus-model DISPATCH (tab/down/up/
// enter/space/esc) lives in app.go's updateLauncherKeys, alongside the rest
// of the launcher's tea.Msg plumbing (panelQueryCmd, panelResultsMsg,
// panelDebounceMsg) — kept together with the App.Update switch it feeds.
package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
)

// includeCap is the max number of RELATED-panel entries the launcher lets
// the user pull in at once (spec §4).
const includeCap = 3

// panelDisplayLimit is how many recall hits the panel shows (spec §2/§6:
// "fetch ~15 (display 5)" — Related() itself does the fetch-15/display-N
// split; this is the N the launcher asks for).
const panelDisplayLimit = 5

// panelRow is one row of the RELATED panel's combined display list (spec
// §4): included entries render PINNED at the top (in toggle order) even
// when the current query's re-rank no longer surfaces them; the remaining,
// non-included query hits follow. Exactly one of the hit-only fields
// (snippet/sameProject) is meaningful when included is false; an included
// row's snippet is always "" (M4: pinned includes render an outcome
// preview, same shape as a recency-fallback hit with no snippet).
type panelRow struct {
	t           store.Transcript
	snippet     string
	sameProject bool
	included    bool
}

// panelRows composes the launcher's combined display list (spec §4):
// pinned includes (in includeOrder — insertion order, the launcher's own
// disclosed choice for deterministic marker-append order at launch) first,
// then panelHits with any SessionID already pinned above skipped (never
// shown twice).
func (a *App) panelRows() []panelRow {
	rows := make([]panelRow, 0, len(a.includeOrder)+len(a.panelHits))
	for _, id := range a.includeOrder {
		if t, ok := a.includes[id]; ok {
			rows = append(rows, panelRow{t: t, included: true})
		}
	}
	for _, h := range a.panelHits {
		if _, ok := a.includes[h.T.SessionID]; ok {
			continue // already shown pinned above
		}
		rows = append(rows, panelRow{t: h.T, snippet: h.Snippet, sameProject: h.SameProject})
	}
	return rows
}

func (a *App) panelLen() int { return len(a.panelRows()) }

// panelSelected returns the row under panelCursor, or ok=false when the
// panel is empty or the cursor is out of range (clampPanelCursor should keep
// the latter from happening, but this stays defensive).
func (a *App) panelSelected() (panelRow, bool) {
	rows := a.panelRows()
	if a.panelCursor < 0 || a.panelCursor >= len(rows) {
		return panelRow{}, false
	}
	return rows[a.panelCursor], true
}

// clampPanelCursor keeps panelCursor in range after the display list's
// length changes (a query result landing, or an include toggle) — same
// clamp discipline as searchCursor/wfCursor elsewhere.
func (a *App) clampPanelCursor() {
	if n := a.panelLen(); a.panelCursor >= n {
		a.panelCursor = max(0, n-1)
	}
}

// togglePanelInclude toggles the hovered panel row's inclusion (spec §4):
// SessionID-keyed (never positional), so a re-rank that reorders or drops a
// hit never silently un-includes it, and re-toggling the same session is
// always possible whether it's currently showing pinned or as a fresh query
// hit. Max includeCap (3) — toggling ON past the cap is a silent no-op (a
// hard cap, not a queue with eviction).
func (a *App) togglePanelInclude() {
	row, ok := a.panelSelected()
	if !ok {
		return
	}
	id := row.t.SessionID
	if _, on := a.includes[id]; on {
		delete(a.includes, id)
		a.includeOrder = removeString(a.includeOrder, id)
	} else {
		if len(a.includes) >= includeCap {
			return
		}
		if a.includes == nil {
			a.includes = map[string]store.Transcript{}
		}
		a.includes[id] = row.t
		a.includeOrder = append(a.includeOrder, id)
	}
	a.clampPanelCursor()
	a.recomputeWarn()
}

func removeString(ss []string, s string) []string {
	out := make([]string, 0, len(ss))
	for _, x := range ss {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// recomputeWarn recomputes the slash-seed warning (spec §4): a seed
// starting "/" with includes>0 gets NO blocks appended at launch (a slash
// command's argument line isn't the place to glue outcome text) — this is
// the live warning shown in the launcher while that condition holds.
// Called after every seed keystroke and every include toggle.
func (a *App) recomputeWarn() {
	if strings.HasPrefix(a.form.seed.Value(), "/") && len(a.includes) > 0 {
		a.panelWarn = "slash-command seed — related context will NOT be appended"
	} else {
		a.panelWarn = ""
	}
}

// includeSnapshot returns the included transcripts in includeOrder (spec
// §4: SessionID-keyed map snapshotted at toggle time; this is the ordered
// view buildSeedWithRecall consumes at launch).
func (a *App) includeSnapshot() []store.Transcript {
	out := make([]store.Transcript, 0, len(a.includeOrder))
	for _, id := range a.includeOrder {
		if t, ok := a.includes[id]; ok {
			out = append(out, t)
		}
	}
	return out
}

// relatedLabel resolves the short project label for a RELATED-panel row or
// included transcript (spec §6 M3): registry reverse-match — the project
// whose transcript.ProjectDirName(Path) equals t.ProjectDir — preferred
// (works even when t.Cwd is empty, since it keys off project_dir); else
// filepath.Base(t.Cwd); NEVER the raw encoded project_dir (e.g.
// "-Users-h-Sauce-loom").
func relatedLabel(projects []registry.Project, t store.Transcript) string {
	for _, p := range projects {
		if transcript.ProjectDirName(p.Path) == t.ProjectDir {
			return p.Label
		}
	}
	return filepath.Base(t.Cwd)
}

// --- Seed assembly (spec §4) ---------------------------------------------

// outcomeCap/labelCap/titleCap bound each included entry's contribution to
// the assembled seed (spec §4): outcomeCap (1.5KB) is the spec's own number;
// labelCap/titleCap are this implementation's defensive bounds so
// assertSeedInvariant can never legitimately fire regardless of how long a
// transcript's title/ask or a project's label happen to be (neither is
// otherwise bounded by the store schema).
const (
	outcomeCap = 1536
	labelCap   = 100
	titleCap   = 200
)

// recallTruncMarker mirrors the workflow/run.go truncateBytes precedent's
// visible truncation marker (same text, copied — see truncateBytes below
// for the copy-vs-export disclosure).
const recallTruncMarker = "…[truncated]"

// seedInvariantMax is the cheap runtime invariant buildSeedWithRecall
// asserts (spec §4: "a cheap invariant assertion guards the math"). Worst
// case: seed (textinput CharLimit 500 runes, ≤4 bytes/rune ≈ 2000 bytes) +
// includeCap (3) entries, each ≤ len(memory.RecallMarker) + labelCap +
// titleCap + outcomeCap + a few literal bytes (≈1900B) ≈ 2000 + 3*1900 =
// 7700 bytes — comfortably under both this cap and the real 15KB send-keys
// ceiling (workflow/run.go) it exists to keep this assembly nowhere near.
const seedInvariantMax = 8 * 1024

// buildSeedWithRecall appends each included transcript's marker (spec §4:
// ` ── Related prior work [<label>·<title-or-ask>]: <outcome>`,
// memory.RecallMarker being the " ── Related prior work [" prefix shared
// with the extractor's echo-chamber guard) to seed, in includes' order.
//
// Slash-command seeds (spec §4): seed starting "/" with len(includes)>0
// returns seed UNCHANGED with warned=true — a command's argument line isn't
// the place to glue outcome text onto, so the blocks are dropped entirely
// rather than corrupting the command. warned=false whenever includes is
// empty (nothing to warn about) or the assembly proceeds normally.
//
// Deviates from the brief's declared 2-arg signature
// (buildSeedWithRecall(seed string, includes []store.Transcript)) by taking
// projects too — disclosed in the task report: store.Transcript has no
// field to carry a pre-resolved label, and the M3 registry-reverse-match
// label helper needs the registry project list, so either every included
// Transcript would need extending with a label field it doesn't otherwise
// need, or this function takes the list it resolves labels from. The latter
// keeps buildSeedWithRecall pure and keeps the marker's label consistent
// with what M3 shows in the panel itself.
func buildSeedWithRecall(seed string, includes []store.Transcript, projects []registry.Project) (string, bool) {
	if len(includes) == 0 {
		return seed, false
	}
	if strings.HasPrefix(seed, "/") {
		return seed, true
	}
	if len(includes) > includeCap {
		includes = includes[:includeCap] // defensive; togglePanelInclude already hard-caps at includeCap
	}

	var b strings.Builder
	b.WriteString(seed)
	for _, t := range includes {
		label := truncateBytes(stripCRLF(relatedLabel(projects, t)), labelCap, recallTruncMarker)
		titleOrAsk := t.Title
		if titleOrAsk == "" {
			titleOrAsk = t.Ask
		}
		titleOrAsk = truncateBytes(stripCRLF(titleOrAsk), titleCap, recallTruncMarker)
		outcome := truncateBytes(stripCRLF(t.Outcome), outcomeCap, recallTruncMarker)

		b.WriteString(memory.RecallMarker)
		b.WriteString(label)
		b.WriteString("·")
		b.WriteString(titleOrAsk)
		b.WriteString("]: ")
		b.WriteString(outcome)
	}
	out := b.String()
	assertSeedInvariant(out)
	return out, false
}

// assertSeedInvariant panics if out exceeds seedInvariantMax (spec §4: "a
// cheap invariant assertion guards the math"). This is deliberately NOT a
// silent extra truncation: the caps above already bound the assembled
// seed's size well under both this and the real 15KB send-keys ceiling, so
// tripping this means the caps themselves were changed inconsistently — a
// bug to surface loudly, not user input to quietly absorb.
func assertSeedInvariant(out string) {
	if len(out) > seedInvariantMax {
		panic(fmt.Sprintf("buildSeedWithRecall: assembled seed %d bytes exceeds invariant cap %d", len(out), seedInvariantMax))
	}
}

// stripCRLF and truncateBytes are copied (not exported) from
// workflow/run.go's identical helpers — disclosed per the brief's "copy or
// export" choice: both are small (≈10-line), self-contained, and workflow's
// versions are unexported, so exporting one for this single external caller
// seemed like more new public surface than a local copy. Semantics are
// identical: strip embedded \n/\r (a literal newline mid-seed would submit
// early via tmux send-keys — spec §4 "single-line output"), then trim to at
// most max BYTES, appending marker (trimming further to make room for it)
// and never splitting a multi-byte rune.
func stripCRLF(s string) string {
	if !containsCRLF(s) {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			continue
		}
		b = append(b, s[i])
	}
	return string(b)
}

func containsCRLF(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return true
		}
	}
	return false
}

func truncateBytes(s string, max int, marker string) string {
	if len(s) <= max {
		return s
	}
	cut := max - len(marker)
	if cut < 0 {
		cut = 0
	}
	if cut > len(s) {
		cut = len(s)
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}
