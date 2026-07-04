# Loom — Phase 4: Fan-out + Wall (v1) — Design

**Status:** Draft for lean red-team
**Date:** 2026-07-04
**Scope:** The final piece of the original vision, right-sized to the cockpit philosophy: launch one recipe across many projects at once (fan-out), and watch many live sessions at once, read-only (the wall — the "Model 3" view deliberately deferred on day one). Unattended headless pipelines are explicitly OUT (that's cron/SDK territory, not a cockpit's).

## 1. What this delivers

- **`N` (shift-n) on the dashboard** → the fan-out launcher: a project CHECKLIST (space toggles, like the RELATED panel), shared model/mode/seed → `↵` launches one real session per selected project, all tagged with a shared group id. The dashboard's attention queue already handles the resulting swarm.
- **`W` (shift-w) on the dashboard** → the wall: a read-only grid of live sessions (2 columns), each cell showing the session header + the live tail of its pane, refreshed every poll tick. `↵` on a cell attaches; `esc` back.

## 2. Fan-out design

1. **UI:** reuses the launcher form internals — fields become: projects (checklist: `↓/↑` move, `space` toggle, ✓ marks; the list is the registry), model, mode, seed. Focus model mirrors the §3 recall conventions (tab cycles fields; the checklist is field 0 and scrolls internally with ↓/↑ when focused — no separate panel zone needed since there's no detail-jump here). RELATED panel does NOT appear in fan-out mode (it's single-project machinery; YAGNI).
2. **Launch:** `↵` with ≥1 selected → for each selected project, `Launcher.Launch(recipe(project))` with Tags `fan:<groupID>` (groupID = short random hex, minted once per fan-out). **Partial failure is surfaced per-project:** launches run sequentially in one tea.Cmd; the result msg carries `[]fanResult{project, name, err}`; failures render in errStr as `fan: 3/5 launched · failed: tavli (…), volar (…)` — successes stand.
3. **Seed:** same seed for every project, delivered by the existing launch-time gated seeding. Empty seed allowed (just opens sessions).
4. **No new storage:** the `fan:<groupID>` tag on the sessions table is the entire persistence (dashboard-visible; searchable via tags later). No migration.

## 3. Wall design

1. **Content:** all LIVE sessions (from the existing engine snapshot — no new data source), ordered as the dashboard orders them (attention first). Cells hold: header line (status icon + project + title/tool hint, truncated) + the last `H` lines of `CapturePane` output (plain text; cell-truncated per line).
2. **Layout:** 2 columns; cell width = (inner−1)/2 with a 1-col gutter; **rows per page = whatever fits the terminal height** (cellH = header 1 + tail lines 6 + separator 1 → page size = max(1, bodyH/cellH) × 2). More sessions than fit → pages; `↓/↑`/`j/k` move selection (selection follows into pages); page indicator in the frame's right annotation (`3–6 of 9`).
3. **Refresh:** on each poll tick while the wall is open, ONE tea.Cmd captures the panes of the VISIBLE page's sessions (sequential CapturePane calls — ≤6 execs/1.5s; measured cost trivial) → `wallMsg{captures map[name]string}`; stale (session no longer visible/live) entries dropped. The tick handler stays ONE tickAfter; the capture Cmd is a one-shot batch member exactly like peekCmd.
4. **Selection stability:** selection is keyed by session NAME, not index; if the selected session dies/vanishes, selection moves to the nearest neighbor (never resets to top).
5. **Keys:** `↓/↑`/`j/k` select · `↵` attach (live only — dead cells show `ended` and don't attach) · `esc` dashboard · `q`/`ctrl+c` quit. Read-only: no kill/tag from the wall (dashboard's job).
6. **Frame invariants hold:** the grid is composed as exact-width lines (left cell + gutter + right cell padded to inner); odd inner widths give the right column the extra cell; CJK/control safety via the existing cell-truncation helpers.

## 4. Architecture

- `internal/ui/fanout.go` — checklist form state + launch-all cmd + result rendering. `internal/ui/wall.go` — grid layout + capture batching + selection. Both new view modes in app.go's state machine.
- No engine/store/workflow changes. `Deps` unchanged (uses existing Tmux/Launcher/Engine).

## 5. Testing

- Fan-out: checklist toggle/focus transitions; groupID tag on every launched session (throwaway tmux + fake claude); partial-failure surfacing (one project with an invalid cwd → its launch fails, others succeed, errStr lists it); empty-selection ↵ no-op with hint.
- Wall: layout math (2 cols exact width at 100/46/odd widths, CJK cell content); pagination (7 sessions × small height → pages, indicator); selection-by-name stability when a session dies between ticks; attach gated on live; stale capture discard; ONE tickAfter invariant; zero-Deps safety.
- E2E: scratch env, 3 fake-claude sessions via fan-out → wall shows all three live tails → captures at 100/46 → attach one → F12 back to wall.

## 6. Accepted limits (v1)

Wall is read-only · captures refresh only the visible page · fan-out recipes are uniform (no per-project seed overrides) · no fan-out group view (the dashboard tag is the group affordance) · no remote hosts · headless pipelines out of scope permanently (cron/SDK exist).
