# Design: Dismiss / clear finished sessions from the loom dashboard

**Date:** 2026-07-06
**Status:** Approved (pending user review of this spec)

## Problem

The dashboard lists two kinds of rows from the `sessions` table:

- **Live** rows — backed by a real tmux session on the `-L loom` server.
- **Recent** rows — finished sessions (`last_status IN ('done','error')`), retained
  as history. `store.Recent(10)` feeds them into the snapshot.

The kill action (`x` → `y`) runs `tmux kill-session -t <name>`. On a live row that
works. On a **recent** row the tmux session is already gone (reaped by the status
engine), so `kill-session` returns `can't find session` and the row stays. There is
no command to remove finished rows, so they accumulate on the dashboard forever and
appear "unkillable".

## Goal

Let the user remove finished rows from the dashboard — one at a time or all at once —
without touching live sessions or the searchable memory archive.

## Decisions (from brainstorming)

1. **`x` becomes context-aware.** Live row → kill the tmux session (unchanged).
   Recent row → dismiss the row from history. Both flow through the existing
   `viewConfirmKill` `y/n` view; only the copy and the committed action differ.
2. **`X` (shift-x) = clear all finished.** A separate bulk command with its own
   confirm carrying a count.
3. **Delete scope = the `sessions` row only.** The `transcripts` / `messages_fts` /
   `indexed_files` memory archive is left intact. It is keyed by claude
   `session_id`, re-indexed from the on-disk `.jsonl` transcripts by the memory
   sweeper, and powers `/` search + recall — so deleting it here would be both
   destructive to search and futile (re-created on the next sweep).

## Architecture

Three layers, all changes localized. No status-engine changes.

### Store (`internal/store/store.go`)

Add three methods. Every delete is guarded by the terminal-status predicate
`last_status IN ('done','error')` (the exact set `Recent()` selects), so a **live
row is unreachable** regardless of any `ended_at` race.

```go
// DeleteSession removes a single finished row. A live row (or unknown name) is a
// no-op — the status guard makes killing a live session impossible via this path.
func (s *Store) DeleteSession(name string) error
    // DELETE FROM sessions WHERE name = ? AND last_status IN ('done','error')

// DeleteEnded removes every finished row and returns the number deleted.
func (s *Store) DeleteEnded() (int64, error)
    // DELETE FROM sessions WHERE last_status IN ('done','error')  → RowsAffected

// CountEnded returns how many finished rows exist (for the bulk-confirm prompt;
// snapshot.Recent is capped at 10 and would undercount).
func (s *Store) CountEnded() (int64, error)
    // SELECT count(*) FROM sessions WHERE last_status IN ('done','error')
```

Factor the predicate into a `const recentSet = "('done','error')"` alongside the
existing `liveSet`, and reuse it in `Recent()` too so the definition lives in one
place.

### UI (`internal/ui/app.go`)

- **`x` handler (~line 902):** unchanged. It still captures `actionTarget` and opens
  `viewConfirmKill` for any selected row; the branch happens at commit.
- **`viewConfirmKill` commit (~line 792):** on `y`, branch on `actionTarget.recent`:
  - live → `Tmux.KillSession(name)` then `pollNowMsg{}` (today's behaviour).
  - recent → `Store.DeleteSession(name)` then `pollNowMsg{}`.
  Both guard `Tmux != nil` / `Store != nil` (existing nil-safe discipline).
- **New `X` handler (dashboard keys):** shift-`X`, gated on `len(a.snap.Recent) > 0`.
  Calls `Store.CountEnded()`, stashes the count, opens a new `viewConfirmClear`.
- **New `viewConfirmClear`:** `y` → `Store.DeleteEnded()` → `pollNowMsg{}`;
  `n`/`esc` → back to dash; `ctrl+c` → quit (parity with the recent ctrl+c fix on
  the kill-confirm view).
- **Confirm copy (`View()`, ~line 2050):** context-aware —
  - kill: `kill <label> (<name>) ?`
  - dismiss: `dismiss <label> from history ?`
  - clear: `clear <N> finished sessions ?`
- **Keybar (~line 2151):** stays a static superset (matching the existing
  convention where `r reopen` always shows). Change `x kill` → `x kill/dismiss`
  and append `· X clear` to the width-gated suffix. No per-selection render logic.

### Status engine

No changes. Verified safe:

- `MarkEnded` is `UPDATE`-only, so it never re-inserts a deleted row.
- The engine only adopts (`Upsert`) a name that appears as a **live tmux session**.
  A finished row has no live tmux backing (it was reaped by `KillSession` after
  `MarkEnded`, or never had one via the orphan path), so a deleted row is not
  re-created.
- All components share the single `store.Open()` instance from `main.go`
  (engine, `Deps.Store`, `Launcher.Store`), so the delete is immediately visible
  to the next poll's `Recent()` / `Live()`.

## Data flow

```
select recent row → x → viewConfirmKill("dismiss … ?") → y
    → Store.DeleteSession(name)   [guarded WHERE last_status IN ('done','error')]
    → pollNowMsg → engine.Poll → Recent() (row gone) → dashboard re-renders

X (≥1 recent) → CountEnded() → viewConfirmClear("clear N … ?") → y
    → Store.DeleteEnded() → pollNowMsg → re-render
```

## Error handling

- Store errors surface via the existing `errMsg{err}` path (flashed in the frame),
  identical to how `KillSession` errors surface today.
- `DeleteSession` on a non-matching name/status is a silent no-op (0 rows) — not an
  error — so a stale selection can't produce a spurious failure message.
- Nil `Store` → the dismiss/clear commands no-op, consistent with the search/status
  paths.

## Known edge case (benign, self-healing)

If an earlier `KillSession` reap failed and a **dead tmux pane lingers** for a
finished session, dismissing its row lets the next poll re-adopt the name as a fresh
orphan row, then `MarkEnded` + `KillSession` reap it again. The row may briefly
reappear but converges within one or two polls / a second dismiss. This requires a
prior failed reap (rare) and is not worth pre-empting in a single-user tool. We do
**not** reap the tmux session on dismiss, because the engine's resurrection logic
shows a row displayed as `done` can, in a launch race, still be a *live* session —
reaping would risk killing it. Deleting only the row under the status guard is the
safe minimal operation.

## Testing (TDD)

**Store (`store_test.go`):**
- `DeleteSession` removes a `done`/`error` row; is a no-op on a live-status row and
  on an unknown name.
- `DeleteEnded` removes all terminal rows, leaves live rows, returns the correct
  count.
- `CountEnded` counts only terminal rows.

**UI (`app_test.go`, real temp `Store` + fake `Tmux`, per existing pattern):**
- `x`+`y` on a recent row calls `DeleteSession` (not `KillSession`) and the row is
  gone after the refresh.
- `x`+`y` on a live row still calls `KillSession`.
- `X`+`y` calls `DeleteEnded`; `X` is inert when no recent rows exist.
- Confirm copy switches between kill / dismiss / clear on the selected row.
- `viewConfirmClear`: `esc`/`n` cancels, `ctrl+c` quits.

## Out of scope

- Deleting transcripts / memory / on-disk `.jsonl` files.
- Undo after dismiss.
- Auto-expiry of old finished rows.

These are possible follow-ups, not part of this change.
