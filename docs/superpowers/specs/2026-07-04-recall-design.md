# Loom — Phase 2.5: Recall (L3, manual-pull) — Design

**Status:** Revision 2 (hardened after lean red-team: 2 Critical / 6 Important / 7 Minor, all folded)
**Date:** 2026-07-04
**Scope:** L3 manual-pull recall in the launcher. NO auto-injection. NO embeddings (future upgrade behind the same `Related` interface).

## 1. What this delivers

The launcher (`n`) gains a **RELATED** panel: pick a project → recent sessions appear; type a seed → live re-rank via a recall-specific FTS query; `space` toggles entries in; included outcomes are appended to the seed at launch as explicit, visible context; `enter` on an entry opens the detail view to read first.

## 2. Relevance (C1 — recall-specific query builder; SearchSessions untouched)

`internal/memory/recall.go` builds its OWN FTS query (NOT `sanitizeFTSQuery`, whose implicit-AND + trailing-`*` returns ZERO sessions for natural-sentence seeds — verified on the real index):
- tokenize seed → drop tokens <4 chars and a small stopword list (the/this/that/with/for/from/into/have/will/what/when/where…);
- quote-escape each surviving term, join with `OR`, NO trailing `*`;
- rank = same-project DESC, then distinct-content-terms-matched DESC, then bm25, then recency; **require ≥2 matched content terms** for an FTS hit to show at all (kills the confident-noise failure: OR + stopwords surfacing unrelated sessions);
- fetch ~15 (display 5) so the same-project boost can promote hits ranked lower by raw bm25;
- <2 surviving tokens or zero qualifying hits → recency fallback (same-project `RecentTranscriptsByProjectDir`).
Blend: same-project above cross-project at equal match tier; cross-project hits shown (often the gold) with a project label per §6-M3.

## 3. Launcher focus model (C2 — binding, every transition tested)

Focus ∈ `{0 project, 1 model, 2 mode, 3 seed}` ∪ `panel[0..k-1]`:
- `tab`/`shift-tab`: cycle FORM fields only (0..3, wrapping) — never enter the panel.
- `↓` from seed(3) → `panel[0]`; `↑` from `panel[0]` → seed(3); `↓`/`↑` within panel move; `↓` at panel bottom = no-op (no wrap); `↑` from project(0) = no-op.
- `enter`: form-focused → LAUNCH; panel-focused → open detail (origin-tracked, §5).
- `space`: panel-focused → toggle include; seed-focused → types a space into the textinput (test asserts: inserts, toggles nothing).
- `esc`: anywhere in launcher → dashboard (unchanged).
- Contextual keybar per focus zone (`↵ launch` vs `↵ detail · space include · ↑ back to form`).

## 4. Includes & seed assembly

- **Includes keyed by SessionID** (`map[sessionID]store.Transcript` snapshot at toggle time — never positional). Included entries render PINNED at the panel top even when re-ranking drops them; re-toggle always possible. **Project-field change CLEARS includes** (different context; disclosed decision).
- Max 3 includes. Appended at launch, each: ` ── Related prior work [<label>·<title-or-ask>]: <outcome>` — outcome truncated byte-safe to 1.5KB with the existing `…[truncated]` marker precedent. Seed `CharLimit` is 500, so total ≈ ≤5.3KB — the 15KB send-keys ceiling cannot trip; **no drop-oldest mechanism** (deleted as unreachable); a cheap invariant assertion guards the math.
- **Slash-command seeds:** seed starting `/` with includes>0 → visible warning in the launcher + blocks NOT appended (a slash command with outcome text glued on is garbage arguments).
- **Echo-chamber guard (I4):** the extractor strips everything from the first ` ── Related prior work [` marker onward when computing a user doc/ask — pulled-in context never re-indexes as the new session's own text, so recall can't compound across generations. (Extractor change + fixture.)

## 5. Detail round-trip (I1)

`openDetail` gains an origin (`detailReturn view`); `esc` returns to the origin (launcher with ALL state intact: form fields, panel entries/cursor, includes, debounce seq — panel state lives on `App`, untouched by detail open/close). In launcher-origin details the `r` resume action is HIDDEN (spawning sessions mid-launcher-flow is a footgun; `s` summarize stays).

## 6. Freshness & bookkeeping

- Panel queries in `tea.Cmd`s; debounce on seed keystrokes; **staleness key = (seed, projectDir)** (I6 — seed-equality alone lets a fast project switch apply the old project's panel); re-fire on project change.
- Migration v6: `CREATE INDEX IF NOT EXISTS idx_transcripts_project ON transcripts(project_dir, last_ts)` (IF NOT EXISTS per house convention; verified clean + used by the planner on a real-DB copy).
- `RecentTranscriptsByProjectDir(projectDir string, limit int)` new store method.
- **M3 label source:** registry reverse-match (project whose `transcript.ProjectDirName(path)` == the hit's project_dir), else `filepath.Base(cwd)`, NEVER the raw encoded dir name.
- **M4:** recency-fallback entries have no snippet → render outcome preview instead (both shapes handled).
- **RelatedHit** = `{Transcript, Snippet string, SameProject bool, MatchedTerms int}`.

## 7. Testing (binding)

recall query builder (sentence seed → OR query, stopwords dropped, ≥2-term gate, noise seed → empty not garbage — fixtures mirror the red-team's real probes) · blend/boost/fetch-15 · every §3 focus transition incl. space-in-seed-types · includes pinning/SessionID keying/project-change clear · slash-seed warning path · seed assembly caps + invariant · extractor marker-strip fixture · detail round-trip state intact + `r` hidden · staleness (seed,project) discard · v6 on real-copy · frame invariants 100/40 with populated panel · zero-Deps safety · e2e fake-claude: launch with 2 includes → sink shows appended blocks.

## 8. Accepted limits

Lexical relevance (embeddings later, same interface) · sweep-latency staleness — and NO per-tick panel self-heal during an active index sweep (M5: a stale panel refreshes on the next keystroke/project change; the launcher is a transient surface) · max 3 includes · outcomes with `@path` tokens ride the existing send-keys precedent (M6) · small-corpus honesty (M7): same-project recency dominates until the archive grows — the cross-project FTS path is where the value concentrates, which is exactly what §2 fixes.
