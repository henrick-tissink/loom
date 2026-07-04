# Loom — Phase 2.5: Recall (L3, manual-pull) — Design

**Status:** Draft for lean red-team
**Date:** 2026-07-04
**Scope:** The original vision's L3, deliberately manual-pull: the launcher surfaces related past work; the user chooses what to pull into the new session's seed. NO automatic injection (the trust-poisoning trap the original design warned about). NO embeddings in v1 — FTS + recency; embeddings are a future upgrade with the same interfaces.

## 1. What this delivers

The launcher (`n`) gains a **RELATED** panel. Pick a project → the panel shows recent sessions from that project. Type a seed → it re-ranks live with FTS matches on your words. Toggle any entry → its distilled outcome is appended to the seed at launch as explicit context. One keypress opens the full detail view (existing) to read before deciding.

## 2. Design decisions

1. **Relevance v1 = blend, client-side:** up to 5 entries — FTS hits on the seed text (sanitized, existing `SearchSessions`) boosted +recency, same-project entries ranked above cross-project; when the seed is empty, fall back to the project's most recent transcripts. New store method `RecentTranscriptsByProjectDir(projectDir string, limit int) ([]Transcript, error)` (indexed by a new `idx_transcripts_project` on (project_dir, last_ts) — migration v6). Cross-project FTS hits are shown (they're often the gold — "I solved this in another repo") but marked with their project label.
2. **Pull-in is explicit and visible:** `tab`… no — the launcher's tab moves fields. Related panel keys: `↓/↑` continue past the seed field into the panel; `space` toggles include (✓); `enter` on a panel entry opens `viewDetail` for it (esc returns to the launcher with state intact). Included entries render in the seed preview line as `+2 related`.
3. **What gets appended:** for each included entry, a single-line block appended to the seed at launch: ` ── Related prior work [<project>·<title-or-ask>]: <outcome>` — outcome capped at 1.5KB/entry, max 3 entries includable, and the TOTAL seed re-checked against the existing 15KB send-keys ceiling (over → oldest-included dropped with a visible warning in the launcher, never silent truncation of the user's own text).
4. **Freshness:** panel queries run in `tea.Cmd`s with the established debounce/stale-discard discipline (seed keystrokes) and re-fire on project-field change. Nil-safe when Store is nil.
5. **No new indexing:** everything reads the Phase-2 tables. A session launched seconds ago won't appear (sweep latency) — accepted, consistent with §Memory's accepted limits.

## 3. Architecture

- `internal/store`: migration v6 (index only) + `RecentTranscriptsByProjectDir`.
- `internal/memory/recall.go`: `Related(store, projectDir, seedText string, limit int) ([]RelatedHit, error)` — the blend logic (pure, unit-testable): seed empty → recent; else SearchSessions + same-project boost + recency tiebreak; RelatedHit{Transcript, Snippet, SameProject}.
- `internal/ui/launcher.go` + `app.go`: RELATED panel state (entries, cursor extension, includes set), toggle/preview/detail-jump, seed assembly at launch (`buildSeedWithRecall`), warnings.

## 4. Testing

- recall.Related: empty-seed → recency order; seed → FTS-ranked with same-project first; limit honored; nil-safe empties.
- Seed assembly: caps per-entry/total, drop-oldest warning, single-line output (CleanText'd outcomes are single-line already — assert anyway).
- UI: panel renders/refreshes on project change + debounced seed typing (stale-discard test); toggle/include count; detail jump and return with launcher state intact; frame invariants 100/40 with populated panel; launch includes appended blocks (fake-claude sink assertion); zero-Deps safety.

## 5. Accepted limits

Relevance is lexical (FTS), not semantic — upgrade path: swap `Related`'s internals for embeddings later, same interface. Sweep-latency staleness. Max 3 includes. No auto-injection, by design.
