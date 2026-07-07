# Harden Clear-Finished-Sessions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use `- [ ]` checkboxes.

**Goal:** Close the defects an adversarial review found in the merged dismiss/clear-finished feature: F2 (data-loss when dismissing an un-reaped row), F1 (stale bulk-clear count), plus test/quality items F3/F5/F6/F7/F8/F9.

**Architecture:** Deletes stay SQL-guarded to finished statuses. F2 adds a pane-liveness check before delete: a lingering **dead** pane is reaped so the status engine can't re-adopt a zero-metadata zombie row; a genuinely **live** pane (rare resurrection race) is left alone (not dismissed). F1 recomputes the confirm count each poll tick. The rest are dedupe + tests.

**Tech Stack:** Go, modernc sqlite (internal/store), Bubble Tea (internal/ui), tmux client (internal/tmux).

**Review source:** adversarial workflow synthesis, 2026-07-07.

## Global Constraints

- No new third-party dependencies.
- Every delete stays guarded by the terminal-status predicate `('done','error')`; a live row must remain unreachable via delete.
- Only the `sessions` table is deleted from; the memory archive (`transcripts`/`messages_fts`/`indexed_files`) is never touched.
- tmux calls are best-effort and nil-safe: `a.deps.Tmux == nil` and `a.deps.Store == nil` must never panic — they no-op.
- Run `go test ./...` green before each commit; commit per task.
- tmux API available: `PaneStatus(name) (tmux.PaneStatus{Dead bool,...}, error)` (errors when no session), `KillSession(name) error`, `HasSession(name) bool`.

---

### Task H1: Store — rename `recentSet`→`endedSet` (F8) + add `EndedNames` (F2 support)

**Files:**
- Modify: `internal/store/store.go`
- Test: `internal/store/store_test.go` (append)

**Interfaces:**
- Produces: `const endedSet` (replaces `recentSet`); `func (s *Store) EndedNames() ([]string, error)`.

- [ ] **Step 1: Write the failing test** — append to `store_test.go`:

```go
func TestEndedNames(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-live"))
	s.SetStatus("loom-live", "running")
	s.Upsert(row("loom-d1"))
	s.MarkEnded("loom-d1", "done", 0, 2000)
	s.Upsert(row("loom-d2"))
	s.MarkEnded("loom-d2", "error", 1, 2001)

	names, err := s.EndedNames()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if len(names) != 2 || !got["loom-d1"] || !got["loom-d2"] || got["loom-live"] {
		t.Fatalf("EndedNames = %v; want exactly loom-d1, loom-d2", names)
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/store/ -run TestEndedNames -v` → FAIL (`EndedNames` undefined).

- [ ] **Step 3: Rename the const and add the method.** In `internal/store/store.go`:
  - Rename `const recentSet = "('done','error')"` to `const endedSet = "('done','error')"` and change its comment to: `// endedSet is the terminal ('done'/'error') status set shared by Recent(), EndedNames, DeleteSession, DeleteEnded, and CountEnded.`
  - Update all existing references from `recentSet` to `endedSet` (in `Recent`, `DeleteSession`, `DeleteEnded`, `CountEnded`). There are no other references — grep to confirm zero `recentSet` remain.
  - Add after `CountEnded`:

```go
// EndedNames returns the session names of all finished rows (used to reap any
// lingering tmux panes before a bulk clear).
func (s *Store) EndedNames() ([]string, error) {
	rows, err := s.db.Query("SELECT name FROM sessions WHERE last_status IN " + endedSet)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}
```

- [ ] **Step 4: Verify pass** — `go test ./internal/store/ -run 'TestEndedNames|TestDelete|TestLiveRecent' -v` → PASS. Then `grep -n recentSet internal/store/store.go` → no matches.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "refactor(store): rename recentSet->endedSet; add EndedNames

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task H2: F2 — reap-before-delete on dismiss & bulk clear + doc correction

**Files:**
- Modify: `internal/ui/app.go` (viewConfirmKill recent-branch closure ~line 800; viewConfirmClear 'y' closure ~line 835)
- Modify: `docs/superpowers/specs/2026-07-06-clear-finished-sessions-design.md` (the "Known edge case" section)
- Test: `internal/ui/app_test.go` (append)

**Interfaces:**
- Consumes: `Store.DeleteSession`, `Store.DeleteEnded`, `Store.EndedNames` (H1), `Tmux.PaneStatus`, `Tmux.KillSession`.

**Behavior (both paths):** for each finished session name, before deleting its row:
- if `Tmux != nil` and `PaneStatus(name)` succeeds (a tmux session exists): if the pane is **not** Dead → the row is secretly live (resurrection race), so **skip deletion** of that row; if the pane **is** Dead → `KillSession(name)` to reap the lingering pane, then delete.
- if `PaneStatus` errors (no session) or `Tmux == nil` → just delete.

- [ ] **Step 1: Write the failing tests** — append to `app_test.go` (real-tmux pattern from `TestKillActsOnCapturedTargetNotLiveCursor`):

```go
// F6+F2: dismissing a finished row whose tmux session is genuinely LIVE must
// NOT delete the row and must NOT kill the live session.
func TestDismissSkipsRowWithLiveTmuxSession(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomhard%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := tm.NewSession("loom-live1", dir, "sleep 30", 80, 24); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// row claims done, but the tmux pane is alive (resurrection race)
	st.Upsert(store.SessionRow{Name: "loom-live1", ProjectLabel: "p", CreatedAt: 1, EndedAt: 2, ExitCode: 0, LastStatus: "done"})

	a := NewApp(Deps{Store: st, Tmux: tm})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{Recent: []store.SessionRow{{Name: "loom-live1", LastStatus: "done"}}}
	a.rebuildRows()
	a.cursor = 0

	a.Update(key("x"))
	_, cmd := a.Update(key("y"))
	if cmd != nil {
		cmd() // run the closure
	}
	if _, ok, _ := st.Get("loom-live1"); !ok {
		t.Fatal("row was deleted even though its tmux session is live")
	}
	if !tm.HasSession("loom-live1") {
		t.Fatal("live tmux session was killed by a dismiss")
	}
}

// F2: dismissing a finished row with NO tmux session deletes it (unchanged path).
func TestDismissDeletesRowWithNoTmuxSession(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomhard2%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Upsert(store.SessionRow{Name: "loom-gone", ProjectLabel: "p", CreatedAt: 1, EndedAt: 2, ExitCode: 0, LastStatus: "done"})

	a := NewApp(Deps{Store: st, Tmux: tm})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{Recent: []store.SessionRow{{Name: "loom-gone", LastStatus: "done"}}}
	a.rebuildRows()
	a.cursor = 0
	a.Update(key("x"))
	_, cmd := a.Update(key("y"))
	if cmd != nil {
		cmd()
	}
	if _, ok, _ := st.Get("loom-gone"); ok {
		t.Fatal("row with no tmux session was not deleted")
	}
}
```

- [ ] **Step 2: Verify they fail** — `go test ./internal/ui/ -run 'TestDismissSkipsRowWithLiveTmuxSession|TestDismissDeletesRowWithNoTmuxSession' -v` → `TestDismissSkipsRowWithLiveTmuxSession` FAILS (current code deletes unconditionally and never consults tmux on the recent path). The no-session test may already pass.

- [ ] **Step 3: Implement the dismiss closure.** In `internal/ui/app.go`, replace the recent-branch closure inside `case viewConfirmKill:` `if s == "y"` (the block `if a.actionTarget.recent { st := a.deps.Store; if st == nil { return a, nil } return a, func() tea.Msg { if err := st.DeleteSession(name); ... } }`) with:

```go
			if a.actionTarget.recent {
				st := a.deps.Store
				if st == nil {
					return a, nil
				}
				tm := a.deps.Tmux
				return a, func() tea.Msg {
					// Close the reap window (adversarial finding F2): if a
					// lingering tmux session still exists for this "finished"
					// row, a bare row delete lets the next poll re-adopt it as
					// a zero-metadata zombie. Reap a DEAD pane first; leave a
					// genuinely LIVE one alone (a resurrection race — it isn't
					// really finished, so don't dismiss it).
					if tm != nil {
						if ps, err := tm.PaneStatus(name); err == nil {
							if !ps.Dead {
								return pollNowMsg{}
							}
							_ = tm.KillSession(name)
						}
					}
					if err := st.DeleteSession(name); err != nil {
						return errMsg{err}
					}
					return pollNowMsg{}
				}
			}
```

- [ ] **Step 4: Implement the bulk-clear closure.** Replace the `case viewConfirmClear:` `y` closure (`return a, func() tea.Msg { if _, err := st.DeleteEnded(); err != nil {...} return pollNowMsg{} }`) with:

```go
			tm := a.deps.Tmux
			return a, func() tea.Msg {
				if tm == nil {
					if _, err := st.DeleteEnded(); err != nil {
						return errMsg{err}
					}
					return pollNowMsg{}
				}
				names, err := st.EndedNames()
				if err != nil {
					return errMsg{err}
				}
				for _, n := range names {
					if ps, e := tm.PaneStatus(n); e == nil {
						if !ps.Dead {
							continue // secretly live — don't clear a live session
						}
						_ = tm.KillSession(n) // reap dead lingering pane
					}
					if err := st.DeleteSession(n); err != nil {
						return errMsg{err}
					}
				}
				return pollNowMsg{}
			}
```

- [ ] **Step 5: Correct the design doc.** In `docs/superpowers/specs/2026-07-06-clear-finished-sessions-design.md`, replace the "Known edge case (benign, self-healing)" paragraph with an accurate description: a finished row whose tmux pane lingered un-reaped, if hard-deleted, would be re-adopted by the status engine as a fresh zero-metadata row (losing Tags/Model/Mode/Seed and true CreatedAt) — NOT a cosmetic reappearance. State the mitigation: dismiss/clear now reap a dead lingering pane before deleting and skip a genuinely-live one. Note the residual: a bulk clear racing a session that is finishing at that exact instant is skipped only if its pane already reads live; this is a sub-poll race with no data-safety impact (the guard still forbids deleting a live *row*).

- [ ] **Step 6: Verify pass** — `go test ./internal/ui/ -run 'TestDismiss|TestKill|TestBulkClear' -v` → PASS (new F2 tests plus all existing dismiss/kill/clear tests). Then `go test ./...` → all packages PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/ui/app.go internal/ui/app_test.go docs/superpowers/specs/2026-07-06-clear-finished-sessions-design.md
git commit -m "fix(ui): reap dead pane before dismiss/clear so engine can't re-adopt a zombie (F2)

Skip a genuinely-live pane (resurrection race). Correct the design doc.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task H3: F1 — recompute bulk-clear count each tick

**Files:**
- Modify: `internal/ui/app.go` (tickMsg view switch ~line 560)
- Test: `internal/ui/app_test.go` (append)

- [ ] **Step 1: Write the failing test** — append to `app_test.go`:

```go
// F1: while the clear confirm is open, the shown count must track reality so it
// can't under-report what DeleteEnded removes.
func TestClearCountRecomputesOnTick(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Upsert(store.SessionRow{Name: "loom-d1", CreatedAt: 1, EndedAt: 2, LastStatus: "done"})
	st.Upsert(store.SessionRow{Name: "loom-d2", CreatedAt: 1, EndedAt: 2, LastStatus: "error"})

	a := NewApp(Deps{Store: st})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{Recent: []store.SessionRow{{Name: "loom-d1", LastStatus: "done"}, {Name: "loom-d2", LastStatus: "error"}}}
	a.rebuildRows()
	a.Update(key("X"))
	if a.clearCount != 2 {
		t.Fatalf("clearCount = %d, want 2", a.clearCount)
	}
	// a third session finishes while the dialog is open
	st.Upsert(store.SessionRow{Name: "loom-d3", CreatedAt: 1, EndedAt: 2, LastStatus: "done"})
	a.Update(tickMsg{})
	if a.clearCount != 3 {
		t.Fatalf("clearCount after tick = %d, want 3 (must track reality)", a.clearCount)
	}
}
```

- [ ] **Step 2: Verify it fails** — `go test ./internal/ui/ -run TestClearCountRecomputesOnTick -v` → FAIL (clearCount stays 2; no viewConfirmClear tick case).

- [ ] **Step 3: Add the tick case.** In the `case tickMsg:` view switch in `internal/ui/app.go`, add before the closing `}` of the switch (alongside `viewPeek`/`viewSearch`/`viewWall`):

```go
		case viewConfirmClear:
			// keep the shown count honest while the dialog is open (F1): the
			// bulk delete removes every finished row, so the number must track
			// new sessions finishing under it. CountEnded is a cheap indexed
			// count, run synchronously like the 'X' handler does.
			if st := a.deps.Store; st != nil {
				if n, err := st.CountEnded(); err == nil {
					a.clearCount = int(n)
				}
			}
			return a, tea.Batch(a.pollCmd(), tickAfter())
```

- [ ] **Step 4: Verify pass** — `go test ./internal/ui/ -run 'TestClearCountRecomputesOnTick|TestBulkClear' -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ui/app.go internal/ui/app_test.go
git commit -m "fix(ui): recompute bulk-clear count each tick so it can't under-report (F1)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task H4: F7 + F9 — dedupe confirm cancel/quit; consistent nil-Store gating

**Files:**
- Modify: `internal/ui/app.go`

**Interfaces:** no signature changes; behavior-preserving refactor. Existing confirm tests plus the new F2/F1 tests must stay green.

- [ ] **Step 1: (refactor, guarded by existing tests — no new test first).** Confirm the safety net exists: `go test ./internal/ui/ -run 'TestKill|TestDismiss|TestBulkClear|TestClear' -v` → PASS before changes.

- [ ] **Step 2: F7 — extract the shared cancel/quit tail.** The `viewConfirmKill` and `viewConfirmClear` cases end with the identical block:

```go
		if s == "n" || msg.Type == tea.KeyEsc {
			a.view = viewDash
		}
		if s == "ctrl+c" {
			return a, tea.Quit
		}
		return a, nil
```

Add a helper method:

```go
// confirmCancel handles the shared non-'y' keys of the confirm dialogs
// (viewConfirmKill / viewConfirmClear): n/esc return to the dashboard, ctrl+c
// quits. It returns (model, cmd, handled=true) when it consumed the key.
func (a *App) confirmCancel(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	s := msg.String()
	if s == "n" || msg.Type == tea.KeyEsc {
		a.view = viewDash
		return a, nil, true
	}
	if s == "ctrl+c" {
		return a, tea.Quit, true
	}
	return a, nil, true // any other key is a no-op while a confirm is open
}
```

Then in both `case viewConfirmKill:` and `case viewConfirmClear:`, replace the trailing three `if`/`return` blocks (everything after the `if s == "y" { ... }` block) with:

```go
		m, cmd, _ := a.confirmCancel(msg)
		return m, cmd
```

- [ ] **Step 3: F9 — make the nil-Store gate consistent.** In the dashboard `case "x":` handler, only open the dismiss confirm for a recent row when the Store is available, mirroring the `case "X":` gate:

```go
	case "x":
		if r, ok := a.selected(); ok {
			if r.recent && a.deps.Store == nil {
				break // no store to dismiss from — stay consistent with 'X'
			}
			a.actionTarget = r
			a.view = viewConfirmKill
		}
```

(The live-kill path is unaffected — `r.recent` is false for live rows.) The now-redundant `if st == nil { return a, nil }` inside the recent 'y' branch may stay as defense-in-depth.

- [ ] **Step 4: Verify** — `go test ./internal/ui/ -run 'TestKill|TestDismiss|TestBulkClear|TestClear' -v` → PASS, then `go test ./...` → PASS. Behavior unchanged for all covered cases.

- [ ] **Step 5: Commit**

```bash
git add internal/ui/app.go
git commit -m "refactor(ui): share confirm cancel/quit; consistent nil-Store gating (F7,F9)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task H5: F3 + F5 — confirm-clear decline/quit and singular-copy tests

**Files:**
- Test: `internal/ui/app_test.go` (append)

- [ ] **Step 1: Write the tests** — append to `app_test.go`:

```go
// F3: the clear confirm's decline/quit branches must be locked down.
func TestClearConfirmDeclineAndQuit(t *testing.T) {
	mk := func() *App {
		st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		st.Upsert(store.SessionRow{Name: "loom-d1", CreatedAt: 1, EndedAt: 2, LastStatus: "done"})
		a := NewApp(Deps{Store: st})
		a.width, a.height = 100, 30
		a.snap = status.Snapshot{Recent: []store.SessionRow{{Name: "loom-d1", LastStatus: "done"}}}
		a.rebuildRows()
		a.Update(key("X"))
		if a.view != viewConfirmClear {
			t.Fatal("clear confirm did not open")
		}
		return a
	}
	// n -> dash, nothing deleted
	a := mk()
	a.Update(key("n"))
	if a.view != viewDash {
		t.Fatalf("n: view = %v, want dash", a.view)
	}
	// esc -> dash
	a = mk()
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewDash {
		t.Fatalf("esc: view = %v, want dash", a.view)
	}
	// ctrl+c -> quit
	a = mk()
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c returned no cmd")
	}
	if msg := cmd(); msg != tea.Quit() {
		t.Fatalf("ctrl+c: cmd = %T, want tea.Quit", msg)
	}
}

// F5: exactly one finished row renders the singular copy.
func TestClearConfirmSingularCopy(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Upsert(store.SessionRow{Name: "loom-d1", CreatedAt: 1, EndedAt: 2, LastStatus: "done"})
	a := NewApp(Deps{Store: st})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{Recent: []store.SessionRow{{Name: "loom-d1", LastStatus: "done"}}}
	a.rebuildRows()
	a.Update(key("X"))
	if a.clearCount != 1 {
		t.Fatalf("clearCount = %d, want 1", a.clearCount)
	}
	body := a.View()
	if !strings.Contains(body, "1 finished session ") && !strings.Contains(body, "1 finished session ?") {
		t.Fatalf("missing singular 'finished session':\n%s", body)
	}
	if strings.Contains(body, "1 finished sessions") {
		t.Fatalf("plural used for count 1:\n%s", body)
	}
}
```

(If `tea.KeyCtrlC` or `tea.Quit()` comparison needs ad/adjusting to match how the sibling `TestKillNeedsConfirm`/ctrl+c tests assert quit in this file, follow that file's existing convention.)

- [ ] **Step 2: Verify** — `go test ./internal/ui/ -run 'TestClearConfirmDeclineAndQuit|TestClearConfirmSingularCopy' -v` → PASS (behavior already implemented; these lock it). Then `go test ./...` → PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/ui/app_test.go
git commit -m "test(ui): cover clear-confirm decline/quit and singular copy (F3,F5)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-review notes
- **F2** (H2) — reap-if-dead/skip-if-live before delete on both dismiss and bulk; `EndedNames` (H1) enumerates the bulk set; doc corrected. Covers the important data-loss finding and F6 (live-session-survives test).
- **F1** (H3) — tick recompute; **F8** (H1) rename; **F7/F9** (H4) dedupe + gate; **F3/F5** (H5) tests.
- All deletes remain `('done','error')`-guarded; live-kill path untouched; nil-Tmux/nil-Store no-op.
- Type consistency: `EndedNames() ([]string, error)`, `endedSet`, `confirmCancel(tea.KeyMsg) (tea.Model, tea.Cmd, bool)` used consistently.
