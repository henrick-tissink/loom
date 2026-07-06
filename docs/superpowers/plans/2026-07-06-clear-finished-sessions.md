# Clear Finished Sessions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the user remove finished ("Recent") sessions from the loom dashboard — one at a time via a context-aware `x`, or all at once via shift-`X` — without touching live sessions or the memory archive.

**Architecture:** Three status-guarded delete methods on `store.Store`, wired into the existing `viewConfirmKill` flow (which branches on whether the captured row is `recent`) plus a new `viewConfirmClear` for bulk. The status engine is unchanged: it only re-adopts names with a live tmux session, and a finished row has none, so a deleted row stays deleted.

**Tech Stack:** Go, `modernc.org/sqlite` (via `internal/store`), Bubble Tea TUI (`internal/ui`), tmux client (`internal/tmux`).

**Spec:** `docs/superpowers/specs/2026-07-06-clear-finished-sessions-design.md`

## Global Constraints

- No new third-party dependencies.
- Every delete is guarded by the terminal-status predicate `last_status IN ('done','error')` — a live row must be unreachable via any delete path.
- Delete the `sessions` row only. Never touch `transcripts` / `messages_fts` / `indexed_files`.
- All UI paths are nil-safe on `a.deps.Store` and `a.deps.Tmux` (existing discipline): a nil dep makes the command a no-op, not a panic.
- Confirm/keybar copy is exact, verbatim as written in each step.
- Run the full suite (`go test ./...`) green before each commit; commit after each task.

---

### Task 1: Store delete methods

**Files:**
- Modify: `internal/store/store.go` (add `recentSet` const near `liveSet` ~line 236; add three methods after `Recent` ~line 246)
- Test: `internal/store/store_test.go` (append)

**Interfaces:**
- Consumes: existing `Open`, `Upsert`, `SetStatus`, `MarkEnded`, `Recent`, `Live`, `const cols`, `const liveSet`.
- Produces:
  - `func (s *Store) DeleteSession(name string) error`
  - `func (s *Store) DeleteEnded() (int64, error)`
  - `func (s *Store) CountEnded() (int64, error)`
  - `const recentSet = "('done','error')"`

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go`:

```go
func TestDeleteSessionRemovesFinishedRowOnly(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-live")) // last_status "unknown" == live
	s.Upsert(row("loom-done"))
	s.MarkEnded("loom-done", "done", 0, 2000)

	// deleting a live row is a no-op (status guard)
	if err := s.DeleteSession("loom-live"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("loom-live"); !ok {
		t.Fatal("live row was deleted — status guard failed")
	}
	// deleting a finished row removes it
	if err := s.DeleteSession("loom-done"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("loom-done"); ok {
		t.Fatal("finished row was not deleted")
	}
	// unknown name is a harmless no-op
	if err := s.DeleteSession("loom-nope"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteEndedAndCountEnded(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-live"))
	s.SetStatus("loom-live", "running")
	s.Upsert(row("loom-d1"))
	s.MarkEnded("loom-d1", "done", 0, 2000)
	s.Upsert(row("loom-d2"))
	s.MarkEnded("loom-d2", "error", 1, 2001)

	n, err := s.CountEnded()
	if err != nil || n != 2 {
		t.Fatalf("CountEnded = %d, %v; want 2", n, err)
	}
	deleted, err := s.DeleteEnded()
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteEnded = %d, %v; want 2", deleted, err)
	}
	if live, _ := s.Live(); len(live) != 1 || live[0].Name != "loom-live" {
		t.Fatalf("live row lost after DeleteEnded: %+v", live)
	}
	if n, _ := s.CountEnded(); n != 0 {
		t.Fatalf("CountEnded after clear = %d; want 0", n)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestDeleteSessionRemovesFinishedRowOnly|TestDeleteEndedAndCountEnded' -v`
Expected: FAIL — `a.DeleteSession undefined` / `s.DeleteEnded undefined` / `s.CountEnded undefined` (compile error).

- [ ] **Step 3: Add the const and methods**

In `internal/store/store.go`, add directly below `const liveSet = "('running','needs_you','idle','unknown')"`:

```go
const recentSet = "('done','error')" // the terminal set Recent() selects
```

Then add after the `Recent` method (~line 246):

```go
// DeleteSession removes a single finished row. The status guard makes deleting
// a live row (or an unknown name) a no-op, so a live session can never be
// removed via this path.
func (s *Store) DeleteSession(name string) error {
	_, err := s.db.Exec(
		"DELETE FROM sessions WHERE name = ? AND last_status IN "+recentSet, name)
	return err
}

// DeleteEnded removes every finished row and returns the number deleted.
func (s *Store) DeleteEnded() (int64, error) {
	res, err := s.db.Exec("DELETE FROM sessions WHERE last_status IN " + recentSet)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountEnded reports how many finished rows exist (drives the bulk-clear
// confirm; the snapshot's Recent list is capped at 10 and would undercount).
func (s *Store) CountEnded() (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT count(*) FROM sessions WHERE last_status IN " + recentSet).Scan(&n)
	return n, err
}
```

Optionally replace the inlined `('done','error')` in `Recent` with `recentSet` (single source of truth). Leave `Recent`'s `LIMIT` logic untouched.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestDeleteSessionRemovesFinishedRowOnly|TestDeleteEndedAndCountEnded' -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): DeleteSession/DeleteEnded/CountEnded for finished rows

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Context-aware `x` — dismiss a recent row

**Files:**
- Modify: `internal/ui/app.go` — `viewConfirmKill` commit branch (~line 794); confirm `View()` copy (~line 2050)
- Test: `internal/ui/app_test.go` (append)

**Interfaces:**
- Consumes: `Store.DeleteSession` (Task 1); existing `a.actionTarget uiRow` (has `.name`, `.label`, `.recent`), `a.deps.Store`, `a.deps.Tmux`, `errMsg`, `pollNowMsg`, `viewConfirmKill`, `styNeedsYou`, `styMeta`, `frame`.
- Produces: dismiss behaviour on `viewConfirmKill` for `recent` targets; a `viewConfirmKill` `View()` that renders kill vs dismiss copy.

- [ ] **Step 1: Write the failing tests**

Append to `internal/ui/app_test.go` (uses a real temp `Store`; no tmux needed — dismiss never calls tmux):

```go
func TestDismissRecentRowDeletesFromStore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Upsert(store.SessionRow{Name: "loom-d", ProjectLabel: "gloom", CreatedAt: 1, EndedAt: 2, ExitCode: 0, LastStatus: "done"})

	a := NewApp(Deps{Store: st})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{Recent: []store.SessionRow{
		{Name: "loom-d", ProjectLabel: "gloom", LastStatus: "done"},
	}}
	a.rebuildRows()
	a.cursor = 0 // the only row is the recent one

	a.Update(key("x"))
	if a.view != viewConfirmKill || !a.actionTarget.recent {
		t.Fatalf("confirm not opened on recent row: view=%v target=%+v", a.view, a.actionTarget)
	}
	// confirm copy must say "dismiss", not "kill"
	if body := a.View(); !strings.Contains(body, "dismiss") {
		t.Fatalf("confirm copy missing 'dismiss':\n%s", body)
	}

	_, cmd := a.Update(key("y"))
	if cmd == nil {
		t.Fatal("'y' returned no command")
	}
	if msg := cmd(); msg != (pollNowMsg{}) {
		t.Fatalf("dismiss returned %T, want pollNowMsg", msg)
	}
	if _, ok, _ := st.Get("loom-d"); ok {
		t.Fatal("recent row was not deleted from the store")
	}
}
```

If `strings` is not already imported in `app_test.go`, add it to the import block.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ui/ -run TestDismissRecentRowDeletesFromStore -v`
Expected: FAIL — the `y` branch calls `KillSession` (no `Tmux` dep → returns `a, nil`, so `cmd` is nil) and the row is not deleted; also the copy assertion fails (`View` renders "kill").

- [ ] **Step 3: Branch the confirm commit on `recent`**

In `internal/ui/app.go`, replace the `if s == "y"` block inside `case viewConfirmKill:` (~line 794) with:

```go
		if s == "y" {
			a.view = viewDash
			name := a.actionTarget.name
			if a.actionTarget.recent {
				st := a.deps.Store
				if st == nil {
					return a, nil
				}
				return a, func() tea.Msg {
					if err := st.DeleteSession(name); err != nil {
						return errMsg{err}
					}
					return pollNowMsg{}
				}
			}
			if a.deps.Tmux != nil {
				tm := a.deps.Tmux
				return a, func() tea.Msg {
					if err := tm.KillSession(name); err != nil {
						return errMsg{err}
					}
					return pollNowMsg{}
				}
			}
			return a, nil
		}
```

- [ ] **Step 4: Make the confirm copy context-aware**

In `internal/ui/app.go`, replace the `case viewConfirmKill:` block inside `View()` (~line 2050) with:

```go
	case viewConfirmKill:
		r := a.actionTarget
		title, verb := "kill session", "kill "
		if r.recent {
			title, verb = "dismiss session", "dismiss "
		}
		return frame(w, title, "",
			[]string{"", "  " + verb + styNeedsYou.Render(r.label) + styMeta.Render(" ("+r.name+")") + " ?", ""},
			"y confirm · n/esc cancel")
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/ui/ -run 'TestDismissRecentRowDeletesFromStore|TestKill' -v`
Expected: PASS (dismiss test plus the existing `TestKillNeedsConfirm` / `TestKillActsOnCapturedTargetNotLiveCursor` still green — live kill unchanged).

- [ ] **Step 6: Commit**

```bash
git add internal/ui/app.go internal/ui/app_test.go
git commit -m "feat(ui): x dismisses a finished row (context-aware confirm)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Bulk clear with shift-`X`

**Files:**
- Modify: `internal/ui/app.go` — view enum (~line 37); App struct field (~line 104); dashboard `X` key (~line 906); `updateKeys` view switch (~line 792, add `viewConfirmClear` case); `View()` (~line 2054, add `viewConfirmClear` case)
- Test: `internal/ui/app_test.go` (append)

**Interfaces:**
- Consumes: `Store.CountEnded`, `Store.DeleteEnded` (Task 1); `a.snap.Recent`, `a.deps.Store`, `errMsg`, `pollNowMsg`, `frame`.
- Produces: `viewConfirmClear` view constant; `a.clearCount int` field; `X` handler; clear confirm + commit.

- [ ] **Step 1: Write the failing test**

Append to `internal/ui/app_test.go`:

```go
func TestBulkClearDeletesAllFinished(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Upsert(store.SessionRow{Name: "loom-d1", ProjectLabel: "a", CreatedAt: 1, EndedAt: 2, ExitCode: 0, LastStatus: "done"})
	st.Upsert(store.SessionRow{Name: "loom-d2", ProjectLabel: "b", CreatedAt: 1, EndedAt: 2, ExitCode: 1, LastStatus: "error"})
	st.Upsert(store.SessionRow{Name: "loom-live", ProjectLabel: "c", CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "running"})

	a := NewApp(Deps{Store: st})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{
		Live:   []status.Row{{SessionRow: store.SessionRow{Name: "loom-live", ProjectLabel: "c"}, Status: status.Running}},
		Recent: []store.SessionRow{{Name: "loom-d1", LastStatus: "done"}, {Name: "loom-d2", LastStatus: "error"}},
	}
	a.rebuildRows()

	a.Update(key("X"))
	if a.view != viewConfirmClear || a.clearCount != 2 {
		t.Fatalf("clear confirm not opened: view=%v count=%d", a.view, a.clearCount)
	}
	if body := a.View(); !strings.Contains(body, "2") {
		t.Fatalf("clear confirm missing count:\n%s", body)
	}

	_, cmd := a.Update(key("y"))
	if cmd == nil {
		t.Fatal("'y' returned no command")
	}
	if msg := cmd(); msg != (pollNowMsg{}) {
		t.Fatalf("clear returned %T, want pollNowMsg", msg)
	}
	if n, _ := st.CountEnded(); n != 0 {
		t.Fatalf("finished rows remain after clear: %d", n)
	}
	if live, _ := st.Live(); len(live) != 1 {
		t.Fatalf("live row lost in bulk clear: %+v", live)
	}
}

func TestBulkClearInertWhenNoRecent(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{Live: []status.Row{
		{SessionRow: store.SessionRow{Name: "loom-a", ProjectLabel: "p"}, Status: status.Running},
	}}
	a.rebuildRows()
	a.Update(key("X"))
	if a.view != viewDash {
		t.Fatalf("X opened a confirm with no recent rows: view=%v", a.view)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ui/ -run 'TestBulkClear' -v`
Expected: FAIL — `viewConfirmClear` undefined / `a.clearCount` undefined (compile error).

- [ ] **Step 3: Add the view constant and struct field**

In `internal/ui/app.go`, add `viewConfirmClear` to the view `const` iota block, immediately after `viewConfirmKill`:

```go
	viewConfirmKill
	viewConfirmClear
```

Add a field to the `App` struct, next to `actionTarget` (~line 104):

```go
	clearCount   int // finished-row count captured when the clear confirm opens
```

- [ ] **Step 4: Add the `X` dashboard handler**

In `internal/ui/app.go`, add a case to the dashboard `switch msg.String()` immediately after the `case "x":` block (~line 906):

```go
	case "X":
		if len(a.snap.Recent) > 0 && a.deps.Store != nil {
			if n, err := a.deps.Store.CountEnded(); err == nil && n > 0 {
				a.clearCount = int(n)
				a.view = viewConfirmClear
			}
		}
```

- [ ] **Step 5: Add the `viewConfirmClear` key handler**

In `internal/ui/app.go`, add a case to the `updateKeys` view switch (~line 792, right after the `case viewConfirmKill:` block):

```go
	case viewConfirmClear:
		s := msg.String()
		if s == "y" {
			a.view = viewDash
			st := a.deps.Store
			if st == nil {
				return a, nil
			}
			return a, func() tea.Msg {
				if _, err := st.DeleteEnded(); err != nil {
					return errMsg{err}
				}
				return pollNowMsg{}
			}
		}
		if s == "n" || msg.Type == tea.KeyEsc {
			a.view = viewDash
		}
		if s == "ctrl+c" {
			return a, tea.Quit
		}
		return a, nil
```

- [ ] **Step 6: Add the `viewConfirmClear` view**

In `internal/ui/app.go`, add a case to the `View()` switch immediately after the `viewConfirmKill` case (~line 2054):

```go
	case viewConfirmClear:
		msg := fmt.Sprintf("  clear %d finished session", a.clearCount)
		if a.clearCount != 1 {
			msg += "s"
		}
		return frame(w, "clear finished", "",
			[]string{"", styNeedsYou.Render(msg) + " ?", ""},
			"y confirm · n/esc cancel")
```

(`fmt` is already imported in `app.go`.)

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/ui/ -run 'TestBulkClear' -v`
Expected: PASS (both tests).

- [ ] **Step 8: Commit**

```bash
git add internal/ui/app.go internal/ui/app_test.go
git commit -m "feat(ui): shift-X clears all finished sessions

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Keybar copy

**Files:**
- Modify: `internal/ui/app.go` — dashboard keybar (~line 2151)
- Test: `internal/ui/app_test.go` (append)

**Interfaces:**
- Consumes: existing `frame`, `viewDash` render path, `fixtureApp` (has a recent row).
- Produces: keybar showing `x kill/dismiss` and `X clear`.

- [ ] **Step 1: Write the failing test**

Append to `internal/ui/app_test.go`:

```go
func TestKeybarShowsDismissAndClear(t *testing.T) {
	a := fixtureApp() // 100x30, has a recent row; wide enough for the suffix
	body := a.View()
	if !strings.Contains(body, "x kill/dismiss") {
		t.Fatalf("keybar missing 'x kill/dismiss':\n%s", body)
	}
	if !strings.Contains(body, "X clear") {
		t.Fatalf("keybar missing 'X clear':\n%s", body)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ui/ -run TestKeybarShowsDismissAndClear -v`
Expected: FAIL — keybar still reads `x kill` and has no `X clear`.

- [ ] **Step 3: Update the keybar strings**

In `internal/ui/app.go` (~line 2151), replace:

```go
	keybar := "↵ attach · space peek · n new · x kill · t tag · r reopen · q quit"
	suffix := " · / search · w workflows · N fan-out · W wall"
```

with:

```go
	keybar := "↵ attach · space peek · n new · x kill/dismiss · t tag · r reopen · q quit"
	suffix := " · X clear · / search · w workflows · N fan-out · W wall"
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/ui/ -run TestKeybarShowsDismissAndClear -v`
Expected: PASS.

- [ ] **Step 5: Full suite + commit**

Run: `go test ./...`
Expected: PASS (all packages).

```bash
git add internal/ui/app.go internal/ui/app_test.go
git commit -m "feat(ui): keybar shows x kill/dismiss and X clear

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Manual verification (after Task 4)

1. Build: `go build -o loom ./cmd/loom` — expect no errors.
2. In a real terminal with finished sessions on the dashboard: select a finished row, press `x` → confirm reads `dismiss … ?` → `y` → the row disappears and does not return on the next poll.
3. Press `X` → confirm reads `clear N finished sessions ?` → `y` → all finished rows disappear; live sessions remain.
4. Confirm `/` search still finds a dismissed session's transcript (memory archive intact).

## Self-review notes

- **Spec coverage:** store methods (Task 1) ✔; context-aware `x`/dismiss + copy (Task 2) ✔; bulk `X` + `viewConfirmClear` + `CountEnded` count (Task 3) ✔; static keybar copy (Task 4) ✔; memory archive untouched (no task deletes transcript tables) ✔; status-engine unchanged (no task touches it, verified in spec) ✔.
- **Type consistency:** `DeleteSession(name string) error`, `DeleteEnded() (int64, error)`, `CountEnded() (int64, error)`, `recentSet`, `viewConfirmClear`, `clearCount int` used identically across tasks.
- **Guards:** every delete carries `last_status IN ('done','error')`; `X` gated on `len(a.snap.Recent) > 0` and `CountEnded > 0`; all commands nil-safe on `Store`.
