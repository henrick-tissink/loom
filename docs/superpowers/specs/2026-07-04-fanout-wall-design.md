# Loom — Phase 4: Fan-out + Wall (v1) — Design

**Status:** Revision 2 (hardened after lean red-team: 2 Critical / 5 Important / 6 Minor, all folded; timing claims measured and confirmed)
**Date:** 2026-07-04
**Scope:** Final piece of the vision. Fan-out = one recipe across many projects. Wall = read-only grid of live sessions. Headless pipelines permanently out (cron/SDK territory).

## 1. What this delivers

- **`N` on the dashboard** → fan-out form: project checklist + shared model/mode/seed → one real session per selected project, group-tagged.
- **`W` on the dashboard** → the wall: 2-column read-only grid of live pane tails, `↵` attaches, `esc` back.
- (`N`/`W` verified unbound today; both join the dashboard keybar's width-elision tier.)

## 2. Fan-out

1. **`fanoutForm` is its OWN form** (not launcher reuse — a checklist cannot live in the 4-line cycle form; only `modelOptions`/`modeOptions`/`optLabel`/`cycle` + a seed textinput are shared). Focus ∈ {0 checklist, 1 model, 2 mode, 3 seed}: `tab`/`shift-tab` cycle fields; when checklist focused, `↓/↑` scroll it internally and `space` toggles (✓); on fields 1–3, `↓/↑` do nothing (tab is field-nav — one dialect per zone, stated); `space` on seed TYPES a space (launcher precedent, tested); `↵` launches from ANY focus (empty selection → no-op with inline hint); `esc` → dashboard.
2. **Launch semantics:** mint `groupID` (6-hex). Sequentially per selected project (measured: 5 × NewSession+upsert ≈ 36ms — one async tea.Cmd): `Launcher.Launch(recipe)` then `Store.SetTags(name, "fan:"+groupID)` — the two-step workflow precedent; **a SetTags failure is COUNTED in the result as `launched, untagged`** (never invisible). Result msg `fanResultMsg{group string, results []fanResult{Project, Name string, Err error, Untagged bool}}`.
3. **In-flight + view transition (I2):** `fanInFlight` guard set at `↵` (second `↵` no-ops); the view STAYS on the fan-out form until `fanResultMsg` lands, then transitions to `viewDash` with the summary in **`fanHint`** — a dedicated persistent field (C1: `errStr` is wiped by every `snapMsg`; `fanHint` follows the `wfHint` discipline — cleared on next dashboard keypress, NOT on polls; binding test: survives a `snapMsg`). Format: `fan #a1b2c3: 4/5 launched · failed: tavli (bad cwd) · volar launched untagged`. The result msg also fires `pollCmd` (M6) so the swarm appears immediately.
4. **Group affordance (I3, honest):** dashboard rows whose Tags contain `fan:` render a dim `· fan` marker in the activity cell (the seed-failed precedent). The group ID itself surfaces in the tag editor (`t`) and the fanHint; no dedicated group view in v1 (§6). `r`-resume copies Tags verbatim, so a resumed fan session rejoins its group — intended.
5. No new storage; no migration; RELATED panel absent in fan-out mode.

## 3. Wall

1. **Content & order:** live sessions from the engine snapshot, ordered by **CreatedAt then name — STABLE** (I4: attention-order reshuffles every poll and teleports the grid under the reader; attention is the dashboard's job). Cell = header line (icon + project + title/tool hint) + last `tailH` lines of `CapturePane`.
2. **Layout:** 2 columns, 1-col gutter; when `inner` splits unevenly the extra cell goes to the RIGHT column (M1 corrected); every composed line exactly frame-width. `cellH = 1 header + tailH + 1 separator`; `tailH = clamp(6, 1, bodyH−2)` (M2 tiny-terminal degrade); page = rows-that-fit × 2; page indicator in the frame's right annotation (`3–6 of 9`).
3. **Refresh:** per poll tick while open, ONE one-shot tea.Cmd captures the VISIBLE page's panes sequentially (measured 6 ≈ 44ms) → `wallMsg{captures}`; stale/vanished entries dropped; tick handler keeps exactly ONE `tickAfter` (peekCmd batching precedent).
4. **Capture errors (I5):** a per-session CapturePane error keeps the cell, renders `(pane unavailable)` (peek precedent), gates `↵` off; the cell disappears on the next snapshot (the engine reaps dead panes within one poll — a "dead cell" only exists inside that ≤1.5s window, stated honestly).
5. **Selection:** keyed by session NAME; survives reorders/pagination; if the selected session vanishes, selection moves to nearest neighbor. Keys: `↓/↑`/`j/k` select · `↵` attach (live cells only) · `esc` dashboard · `q`/`ctrl+c` quit. Read-only.

## 4. Architecture

`internal/ui/fanout.go` + `internal/ui/wall.go`, two new view modes in app.go. No engine/store/session/workflow package changes (SetTags already exists). Deps unchanged.

## 5. Testing (binding)

- Fan-out: every §2.1 focus/keystroke rule (incl. space-on-seed-types, ↵-empty-selection no-op); groupID tag on every launched session (throwaway tmux + fake claude); partial failure (invalid cwd project → counted failed, others succeed) AND untagged accounting (inject a SetTags failure if cheaply possible — else unit-test the result-assembly path, disclose); **fanHint survives snapMsg**, cleared on keypress; double-↵ in-flight guard; fanResult fires poll.
- Wall: layout exact-width at 100/46/odd (CJK content); tiny-terminal tailH clamp; pagination + indicator; stable order (status flip does NOT reorder — test); selection-by-name across death/pagination; capture-error cell (`↵` gated); stale capture discard; ONE tickAfter; zero-Deps.
- E2E: scratch env, fan-out 3 fake-claude sessions → dashboard shows `· fan` markers + fanHint → `W` wall shows three live tails → attach one → F12 → wall again → captures at 100/46.

## 6. Accepted limits (v1)

Wall cells are colorless (`capture-pane` without `-e`) and show the LEFT edge of panes rendered at full width (Claude's chrome truncates right; tails are input-box-heavy — TrimRight applied; a future `-J`/wider-cell pass can improve) · captures refresh visible page only · uniform recipes (no per-project seed) · no group view beyond the `· fan` marker + tag editor + fanHint · no remote hosts · read-only wall · headless pipelines never.
