# Loom — Cockpit QoL Sprint — Design

**Status:** Approved (user green-lit Tier 1, 2026-07-03)
**Scope:** Five daily-feel features. No new dependencies. Engine/store changes are additive; UI keeps the Mission Control frame system and its invariants (exact-width lines; truncate plain before style; clip by cells).

## Features

### 1. Session titles
Claude writes `{"type":"ai-title","aiTitle":"…","sessionId":"…"}` records into the transcript. Capture the latest one in the classifier; persist to a new `title` store column (migration v3 — titles must survive session death for RECENT/resume); thread through `status.Row.Title`.
**Display:** appended to the activity cell: `reply ready · add vega hedge to strategy` (state first, title after, truncated as one plain string before styling). Recent rows: `done · <title>`. Blank title → today's text unchanged.

### 2. Context meter
The **last assistant record's** `message.usage` is the current context footprint (input_tokens + cache_creation_input_tokens + cache_read_input_tokens + output_tokens). It is NOT a running sum across messages. Classifier captures it; threaded as `status.Row.CtxTokens`.
**Display:** new right-aligned 4-cell column between activity and model·mode: `640`, `82k`, `823k`, `1.0M`; blank when 0. Fixed row budget grows 36 → 41; the existing `actW <= 0` narrow degrade covers small widths. **No %-of-limit coloring or warnings** — context limits vary by model/plan and a wrong guess is worse than a neutral number (YAGNI'd).

### 3. Attention bell
Engine detects the transition `last_status != needs_you → needs_you` during Poll and returns the affected sessions in `Snapshot.NewlyNeedsYou []string` (project label, `· title` appended when known). UI fires a `tea.Cmd` per snapshot containing entries: macOS → `osascript -e 'display notification … with title "Loom" sound name "Glass"'`; other OS → BEL (`\a`) to stderr. No config toggle (YAGNI).
**Accepted limits:** (a) polls are suspended while attached full-screen, so bells for other sessions arrive on detach; (b) a session that finished while Loom was closed bells once on next startup (arguably a feature); (c) two Loom instances may both bell.

### 4. Scrolling
Dashboard body lines exceed the frame's interior at ~8+ sessions. Window the body **lines** (rules/spacers included) to `height − 2`, keeping the cursor's line visible (cursor-centered, clamped), replacing clipped edges with dim `… N more ↑` / `… N more ↓` markers that never cover the cursor line. `height == 0` (unknown) → no windowing. Peek/launcher/dialog views are naturally short; only the dashboard windows.

### 5. Peek
`space` on a **live** row opens a framed read-only view: title `peek · <project>`, body = tail of `capture-pane` output (plain text — no `-e`, so no ANSI), each line cell-truncated to the frame interior, last `height−2` lines. Re-captures on every poll tick while open (content stays live). Keys: `space`/`esc` → dashboard; `↵` → attach the peeked session (uses the same captured-target discipline as kill/tag: the target is pinned at open, not the live cursor). `space` on a Recent row: no-op (no pane).

## Interface changes

- `transcript.Reader.Poll()` returns a struct now: `ReaderSnapshot{State, LastTool, Title string→, CtxTokens int64}` (tuple was already at 3; existing reader/engine call sites and their tests updated — the ONLY sanctioned existing-test edits in this sprint, plus the store cols).
- `store`: migration v3 `ALTER TABLE sessions ADD COLUMN title TEXT NOT NULL DEFAULT ''`; `SessionRow.Title`; `SetTitle(name, title)`.
- `status.Row`: `+ Title string, CtxTokens int64`; `Snapshot.NewlyNeedsYou []string`.
- `ui`: `humanTokens(int64) string`, `padLeft(s string, w int)`, `windowBody(body []string, cursorLine, maxH int) []string`, `viewPeek` mode, notify cmd.

## Testing

- Classifier: fixtures with real-shape `ai-title` + assistant `usage` records → Title/CtxTokens captured; sidecar-immunity regression still green.
- Store: v3 migrates an existing v2 DB (title readable as ''), SetTitle roundtrip.
- Engine: title persisted on change; NewlyNeedsYou fires exactly once per transition (second Poll → empty).
- UI: humanTokens table; windowBody (cursor at top/middle/bottom, markers correct, cursor never hidden, len ≤ maxH); peek open/refresh/attach/esc + no-op on recent; frame invariant suite extended to the peek view; all existing UI tests pass unmodified.
