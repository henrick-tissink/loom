# Loom — Phase 2: Memory (L1 + L2) — Design

**Status:** Draft for red-team review
**Date:** 2026-07-03
**Scope decision:** L1 (searchable archive) + L2 (distillation). L3 (related-work recall at launch) is deferred to Phase 2.5 — it needs L2's summaries to exist before it can be trusted.

## 1. What this delivers

The `/` key goes live. Every claude session the user has **ever** run — across all `$CLAUDE_CONFIG_DIR/projects/*` directories, including sessions predating Loom — becomes instantly searchable from the dashboard. Selecting a result shows a detail view with an automatic distillation (what was asked, what concluded, which files were touched) and an on-demand LLM summary. Any archived session can be resumed into a live cockpit session.

## 2. Design decisions (made, disclosed)

1. **Index the whole archive**, not just Loom-launched sessions — that's the "remembers everything" promise.
2. **Index only meaningful text:** user prompts (plain-text content) + assistant `text` blocks + titles. **Excluded:** tool_use inputs, tool_results (file dumps — enormous and noisy), thinking blocks, sidechain records. This keeps the FTS index roughly an order of magnitude smaller and results human-meaningful.
3. **Hybrid distillation (L2):**
   - **Auto, free, for everything:** deterministic extraction — *ask* (first user prompt, truncated), *outcome* (last assistant text, truncated), *files touched* (distinct paths from Edit/Write/NotebookEdit tool_use inputs), plus title/dates/message count. Stored on the transcript row, computed during indexing.
   - **LLM, on demand only:** `s` in the detail view extracts the session's indexed text (capped ~40k chars, head+tail sampling when over), pipes it via stdin to `claude -p <prompt> --no-session-persistence`, stores the result. NEVER `--resume` (would append a summary turn to the original transcript), NEVER automatic (backfilling hundreds of sessions would burn the user's plan).
4. **Incremental indexing by (size, mtime):** a changed file is delete-and-reindexed wholesale (transcripts are append-mostly; single-file reindex is cheap and always correct). Sweep runs as a background goroutine at Loom startup and every 10 minutes; search works immediately on whatever is indexed so far.
5. **SQLite FTS5 in the existing loom.db** (migration v4). **Load-bearing spike #1: verify modernc.org/sqlite (pure Go) actually ships FTS5.** Fallback if absent: a `messages(content, session_id, role, ts)` table searched via `LIKE` with lower() — slower, same interfaces, decided at the spike gate.
6. **Search grouped by session:** FTS `MATCH` ranked by bm25, one row per session (best-ranked snippet), top 50. Query sanitization: each user term double-quoted, last term gets `*` prefix-match; malformed queries yield empty results, never errors.

## 3. Architecture

```
internal/store       — migration v4 (transcripts + FTS5 tables); ALL SQL:
                       UpsertTranscript, ReplaceMessages(batch tx), SearchSessions,
                       GetTranscript, SetLLMSummary, TranscriptStats
internal/memory/
  extract.go         — JSONL record → IndexDoc{Role, Text, TS}; Distill{Ask, Outcome, Files}
  indexer.go         — sweep + incremental logic + atomic Status{Indexed, Total, Active}
  summarize.go       — extract text → pipe to `claude -p` (binary name injectable for tests)
internal/ui          — viewSearch (input + live results), viewDetail (distill/summary/snippets),
                       actions: ↵ detail, r resume, s summarize, esc back
cmd/loom             — starts indexer goroutine; keybar "/ search" goes live
```

### Store schema (migration v4)

```sql
CREATE TABLE transcripts (
  session_id  TEXT PRIMARY KEY,
  project_dir TEXT NOT NULL,      -- encoded dir name under projects/
  cwd         TEXT NOT NULL DEFAULT '',   -- from record `cwd` field when present
  title       TEXT NOT NULL DEFAULT '',
  first_ts    INTEGER NOT NULL DEFAULT 0, -- unix seconds
  last_ts     INTEGER NOT NULL DEFAULT 0,
  msg_count   INTEGER NOT NULL DEFAULT 0,
  size_bytes  INTEGER NOT NULL DEFAULT 0, -- incremental fingerprint
  mtime       INTEGER NOT NULL DEFAULT 0,
  ask         TEXT NOT NULL DEFAULT '',
  outcome     TEXT NOT NULL DEFAULT '',
  files       TEXT NOT NULL DEFAULT '',   -- newline-joined distinct paths
  llm_summary TEXT NOT NULL DEFAULT '',
  summary_at  INTEGER NOT NULL DEFAULT 0
);
CREATE VIRTUAL TABLE messages_fts USING fts5(
  content, session_id UNINDEXED, role UNINDEXED, ts UNINDEXED
);
```

Search: `SELECT session_id, snippet(messages_fts,0,X,Y,'…',12) ... WHERE messages_fts MATCH ? ORDER BY bm25(messages_fts) LIMIT ...` grouped to best-per-session, joined to `transcripts` for title/project/dates. Snippet highlight markers are `\x01`/`\x02`, replaced with accent styling at render time (plain text until then — the truncate-before-style rule holds).

### Indexer

- Sweep: `ReadDir(projects/)` → per dir `ReadDir(*.jsonl)` → skip when `(size, mtime)` matches the stored fingerprint → else parse full file line-by-line (reusing the partial-line-tolerant discipline), extract docs + distill, then in ONE transaction: delete old FTS rows for the session, insert new batch, upsert the transcript row.
- The engine's live-session Reader is untouched — indexing is a separate read path. A session being actively written is simply re-indexed next sweep (fingerprint changed).
- Status via atomics: `{Swept, Changed, Total int64; Active bool}` — the search view's frame right-annotation shows `N sessions indexed` and `(indexing…)` while active.
- Concurrency with the poll loop: same WAL DB, `SetMaxOpenConns(1)` serializes; indexer batches per file in one tx; busy_timeout 5000 absorbs contention. Expected DB growth: tens of MB (text-only indexing).

### Summarizer

- `Summarize(sessionID)`: pull the session's docs from FTS (or re-extract from file), join role-prefixed lines, cap ~40k chars (head 30k + tail 10k when over), run `claude -p <prompt> --no-session-persistence` with the text on stdin, 90s timeout, store via `SetLLMSummary`.
- Prompt: "Summarize this Claude Code session. Sections: Goal, Outcome, Key decisions, Files touched. Be concise (≤150 words)."
- The claude binary name is a struct field (injectable) so tests use a fake script.
- UI: `s` in detail view → "summarizing…" state in the detail body; result or error replaces it on completion (async `tea.Cmd`, one at a time — a second `s` while running is a no-op).

### Search / detail UX

- `/` from dashboard → `viewSearch`: framed; body = textinput row + results list; type-to-search debounced ~200ms (timer-based `tea.Tick` or query-on-keystroke — decided in plan; must not block the render loop: queries run in `tea.Cmd`s and stale results (query string mismatch) are discarded, the same discipline as `peekMsg`).
- Results: `▸ project · title-or-ask · age` + one snippet line, dim. `↓/↑` move (input keeps focus), `↵` detail, `esc` dashboard.
- `viewDetail`: title, project/cwd, date range, msg count, Ask / Outcome / Files block, LLM summary (or "press s to summarize"), matching snippets for the current query. Keys: `r` resume, `s` summarize, `esc` back to search.
- Resume of an archived session not in the sessions table: synthesize a `store.SessionRow` (`ClaudeSessionID` = session_id, `Cwd` = transcript cwd, label = `filepath.Base(cwd)`) and hand it to the existing `Launcher.Resume`. If `cwd` is empty (old records without the field) → disable `r` with a dim hint.

## 4. Spikes (before implementation)

1. **FTS5 in modernc.org/sqlite** — create virtual table, insert, MATCH, bm25, snippet(). Gate: if missing → LIKE fallback plan activates (same store interfaces).
2. **Record field reality check** — on real transcripts: `timestamp` format (ISO8601?), `cwd` field presence/name, thinking-block shape (to exclude), text-block extraction sanity.
3. **`claude -p` stdin pipe** — verify `echo text | claude -p "prompt" --no-session-persistence` returns a summary and does NOT create a session file.

## 5. Testing

- Extractor: real-shape fixtures (text/tool_use/tool_result/thinking/sidechain/ai-title) → docs + distill correctness.
- Store: v4 migration on a v3 DB copy; FTS roundtrip; search grouping/sanitization (malformed queries → empty, not error); snippet markers present.
- Indexer: temp CLAUDE_CONFIG_DIR with synthetic transcripts → full sweep, incremental no-op on unchanged, reindex on append, status counters.
- Summarizer: fake `claude` script capturing stdin → prompt+cap verified; no-session-file assertion.
- UI: search flow (type→results→detail→esc), stale-result discard, resume-synthesis row, summarize no-op-while-running, frame invariants for both new views.

## 6. Accepted limits

- Indexing latency: new content appears in search up to one sweep (~10 min) late; a manual refresh is `esc`+`/` (sweep also runs at startup). No file-watcher in v2.0.
- No cross-session semantic similarity (that's L3's problem, and likely embeddings — out of scope).
- LLM summaries cost plan usage — visible, user-triggered, one at a time by design.
- Search is text-match (FTS), not semantic.
