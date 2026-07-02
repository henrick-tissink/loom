# Loom — Mission Control UI — Design

**Status:** Approved (direction chosen by user from 3 previews, 2026-07-03)
**Scope:** Visual pass only. No behavior changes — keys, polling, engine semantics untouched except one additive field (`status.Row.Activity`).

## Goal

Replace the bare-lines dashboard with the spec §4.1 "Mission Control" look: a framed, columned, information-dense cockpit panel. Reference render:

```
╭─ LOOM ───────────────────────── 4 live · 1 needs you ─╮
│                                                        │
│  NEEDS YOU ─────────────────────────────────────────  │
│  ▸ ● tavli       reply ready         sonnet·auto   2m  │
│                                                        │
│  RUNNING ───────────────────────────────────────────  │
│    ◐ parallax    ⏺ Edit              opus·plan    12s  │
│    ◐ volar       ⏺ Bash              sonnet·edits  4s  │
│                                                        │
│  IDLE ──────────────────────────────────────────────  │
│    ○ Innostream  your turn           fable·auto    8m  │
│                                                        │
│  RECENT ────────────────────────────────────────────  │
│    ✓ gloom       done                opus          1h  │
╰─ ↵ attach · n new · x kill · t tag · r reopen · q quit ─╯
```

## Elements

1. **Frame.** Rounded panel, full terminal width (fallback 80 when width unknown), hand-composed (lipgloss borders can't embed text): top line carries the accent `LOOM` wordmark + right-aligned dim counts; bottom line carries the keybar (`↵ attach · n new · x kill · t tag · r reopen · q quit`, with `/ search·soon · w workflows·soon` extra-dim appended when room). Body lines are `│ <content padded to width> │`. All width math via `lipgloss.Width` (ANSI-aware).
2. **Sections.** Labeled rules (`NEEDS YOU ────`) in dim; the NEEDS YOU label renders red only when the section is non-empty; empty sections omitted; blank line between sections. Empty state: centered dim `no sessions — press n to launch one`.
3. **Columns** `cursor(2) icon(2) project(12) activity(flex) model·mode(13) age(4)`, built as plain text per segment, truncated per segment BEFORE styling (styling after truncation slices ANSI), then styled and joined. Known accepted limit: wide-rune project names may misalign by a cell.
   - activity: Running → `⏺ <LastTool>`; Needs-you → `reply ready`; Idle → `your turn`; Recent → `done` / `error · exit N` / `ended` (done with exit -1); `seed failed` hint appended dim when `SeedStatus=="failed"`.
   - age: live rows = now − tmux `session_activity` (new `Activity int64` on `status.Row`, value the engine already reads); Recent = now − `ended_at`; format `4s/2m/1h/2d`, blank when source is 0/unset.
4. **Palette** (formalizes existing hues): accent `219` (wordmark, cursor), alert `203` (needs-you), running `214`, done `71`, meta `245`, chrome/dim `240`. No gradients, no background pills.
5. **Dialogs.** Launcher / kill-confirm / tag render inside the same frame via a shared `frame(title, right, body, keybar)` helper — mode switches stay inside the panel.
6. **Known limits (accepted):** no vertical scrolling (tall session lists overflow terminal height); no permission-detail parsing (data doesn't exist yet — Phase 2+).

## Testing

- `humanAge` table test (s/m/h/d boundaries, zero → blank).
- Frame invariant: with width set, every rendered line's `lipgloss.Width` == width exactly; at width 40 nothing overflows or panics.
- Existing `View()`/`Update` tests keep passing unchanged (chrome is additive; content substrings preserved).
- `Engine.Poll` threads tmux Activity into `Row.Activity` (asserted in status tests).
