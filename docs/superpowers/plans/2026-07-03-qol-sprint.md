# QoL Sprint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Five cockpit QoL features — session titles, context meter, attention bell, scrolling, peek — per spec `docs/superpowers/specs/2026-07-03-qol-sprint-design.md`.

**Architecture:** Classifier learns two more record shapes (`ai-title`, assistant `usage`); `Reader.Poll` returns a snapshot struct; store gains a persisted `title` (migration v3); the engine threads Title/CtxTokens and computes needs-you transitions; the UI adds a ctx column, appends titles to activity text, windows the dashboard body, notifies on transitions, and adds a peek view.

**Tech Stack:** Existing only (Go, lipgloss, bubbletea, modernc/sqlite, tmux). `osascript` at runtime on macOS.

## Global Constraints

- The ONLY existing tests that may be edited: transcript reader tests + engine call sites (forced by the `Reader.Poll` signature change) and store tests touched by the `cols` addition — every edit disclosed in the implementer report. All other existing tests pass byte-identical.
- UI invariants from the Mission Control pass are binding: every line exactly `App.width` cells; truncate PLAIN text before styling; clip by CELLS not runes.
- Context tokens = the LAST assistant record's `usage` sum (input + cache_creation + cache_read + output). Never a running sum.
- Bell fires on `last_status != "needs_you" && new == needs_you`, computed in the engine BEFORE SetStatus persists the new status.
- Peek/kill/tag all act on a target captured at open — never the live cursor.
- gofmt -w; conventional commits; `go vet ./... && go test -race -count=1 ./...` green at each task end.

## File Structure

```
internal/transcript/classify.go/_test  — ai-title + usage capture          (Task 1)
internal/transcript/reader.go/_test    — ReaderSnapshot return             (Task 1)
internal/store/store.go/_test          — migration v3, Title, SetTitle     (Task 2)
internal/status/engine.go/_test        — Title/CtxTokens/NewlyNeedsYou     (Task 2)
internal/ui/text.go/_test              — humanTokens, padLeft              (Task 3)
internal/ui/app.go/_test               — ctx column, titles, scroll, peek  (Task 3)
internal/ui/notify.go                  — notifyCmd                         (Task 3)
```

---

### Task 1: Classifier captures titles + usage; Reader returns a snapshot

**Files:**
- Modify: `internal/transcript/classify.go`, `internal/transcript/classify_test.go` (additive tests only)
- Modify: `internal/transcript/reader.go`, `internal/transcript/reader_test.go` (signature change — sanctioned edit)

**Interfaces:**
- Produces: `Classifier.Title() string`, `Classifier.CtxTokens() int64`, `type ReaderSnapshot struct { State State; LastTool, Title string; CtxTokens int64 }`, `Reader.Poll() (ReaderSnapshot, error)`.

- [ ] **Step 1: Write failing tests**

Add to `classify_test.go` (new consts + tests; existing tests untouched):
```go
const (
	lineAiTitle   = `{"type":"ai-title","aiTitle":"add vega hedge to strategy","sessionId":"x"}`
	lineAsstUsage = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn","usage":{"input_tokens":12,"cache_creation_input_tokens":100,"cache_read_input_tokens":80000,"output_tokens":500}}}`
)

func TestTitleCaptured(t *testing.T) {
	c := feed(t, lineUserPrompt, lineAiTitle, lineAsstEndTurn)
	if c.Title() != "add vega hedge to strategy" {
		t.Fatalf("Title = %q", c.Title())
	}
	// ai-title is a sidecar: it must not change turn state
	if c.State() != StateNeedsYou {
		t.Fatalf("state = %v, want NeedsYou", c.State())
	}
}

func TestCtxTokensFromLastUsage(t *testing.T) {
	c := feed(t, lineUserPrompt, lineAsstUsage)
	if c.CtxTokens() != 80612 { // 12+100+80000+500
		t.Fatalf("CtxTokens = %d, want 80612", c.CtxTokens())
	}
	if c.State() != StateNeedsYou {
		t.Fatalf("state = %v", c.State())
	}
}

func TestUsageAbsentLeavesCtx(t *testing.T) {
	c := feed(t, lineUserPrompt, lineAsstUsage, lineToolResult, lineAsstToolUse)
	if c.CtxTokens() != 80612 { // later assistant WITHOUT usage keeps prior value
		t.Fatalf("CtxTokens = %d", c.CtxTokens())
	}
}
```

Update `reader_test.go` for the new signature — mechanical rewrite of the three existing tests (`s, _, _ := r.Poll()` → `rs, _ := r.Poll()`; assert on `rs.State`, `rs.LastTool`), PLUS one new test:
```go
func TestReaderSnapshotCarriesTitleAndCtx(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(lineUserPrompt+"\n"+lineAiTitle+"\n"+lineAsstUsage+"\n"), 0o644)
	rs, err := NewReader(p).Poll()
	if err != nil {
		t.Fatal(err)
	}
	if rs.Title != "add vega hedge to strategy" || rs.CtxTokens != 80612 || rs.State != StateNeedsYou {
		t.Fatalf("snapshot = %+v", rs)
	}
}
```

- [ ] **Step 2: Run to verify failures**

`go test ./internal/transcript/ -count=1` — FAIL (Title/CtxTokens undefined; Poll signature).

- [ ] **Step 3: Implement**

`classify.go` — extend the record struct and Feed:
```go
type record struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	AiTitle     string `json:"aiTitle"`
	Message     *struct {
		Content json.RawMessage `json:"content"`
		Usage   *usage          `json:"usage"`
	} `json:"message"`
}

type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}
```
Classifier gains `title string` and `ctxTokens int64` fields plus getters `Title()`, `CtxTokens()`. In `Feed`, immediately after unmarshal:
```go
	if r.Type == "ai-title" {
		if r.AiTitle != "" {
			c.title = r.AiTitle
		}
		return // sidecar: never a turn boundary
	}
```
and inside the `case "assistant":` branch, before block handling:
```go
	if r.Message != nil && r.Message.Usage != nil {
		u := r.Message.Usage
		c.ctxTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens + u.OutputTokens
	}
```
(Absent usage on a later assistant record leaves the prior value — the spec's "last usage seen".)

`reader.go` — replace the tuple return:
```go
// ReaderSnapshot is the classifier state after consuming all complete lines.
type ReaderSnapshot struct {
	State     State
	LastTool  string
	Title     string
	CtxTokens int64
}

func (r *Reader) snap() ReaderSnapshot {
	return ReaderSnapshot{
		State: r.cls.State(), LastTool: r.cls.LastTool(),
		Title: r.cls.Title(), CtxTokens: r.cls.CtxTokens(),
	}
}

func (r *Reader) Poll() (ReaderSnapshot, error) { ... same body, every return becomes r.snap()/err ... }
```
Also update the ONE engine call site (`internal/status/engine.go`: `ts, tool, _ := rd.Poll()` → `rs, _ := rd.Poll()`; use `rs.State`, `rs.LastTool`) so the repo compiles — Task 2 builds on it properly.

- [ ] **Step 4: Verify pass, full suite, commit**

`go test -race -count=1 ./... && go vet ./...` — green.
```bash
git add internal/transcript internal/status && git commit -m "feat: classifier captures ai-title and context usage; Reader returns snapshot struct"
```

---

### Task 2: Store title (v3) + engine threading + transition detection

**Files:**
- Modify: `internal/store/store.go`, `internal/store/store_test.go`
- Modify: `internal/status/engine.go`, `internal/status/engine_test.go`

**Interfaces:**
- Produces: `store.SessionRow.Title`, `store.Store.SetTitle(name, title string) error`, `status.Row.{Title string, CtxTokens int64}`, `status.Snapshot.NewlyNeedsYou []string`.

- [ ] **Step 1: Write failing tests**

`store_test.go` additions:
```go
func TestMigrationV3TitleOnExistingDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loom.db")
	s, err := Open(p) // creates at latest version
	if err != nil {
		t.Fatal(err)
	}
	r := row("loom-t")
	r.Title = "hedge the vega"
	if err := s.Upsert(r); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(p) // reopen: migrations must be no-op idempotent
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, ok, _ := s2.Get("loom-t")
	if !ok || got.Title != "hedge the vega" {
		t.Fatalf("title roundtrip: %+v %v", got, ok)
	}
}

func TestSetTitle(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-a"))
	if err := s.SetTitle("loom-a", "new title"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get("loom-a")
	if got.Title != "new title" {
		t.Fatalf("Title = %q", got.Title)
	}
}
```
(The `row()` helper and any tests constructing `SessionRow` literals compile unchanged — `Title` zero-values. If `Get` comparisons use struct equality they still hold with Title "".)

`engine_test.go` additions — extend `fakeTranscript` handling by adding a SECOND fixture and a new test (existing tests untouched):
```go
const fakeTranscriptTitled = `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"ai-title","aiTitle":"probe the flux","sessionId":"x"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"yo"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":40000,"output_tokens":90}}}
`

func TestPollThreadsTitleCtxAndBellsOnce(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	tpath := transcript.Path(ccd, cwd, id)
	os.MkdirAll(filepath.Dir(tpath), 0o755)
	os.WriteFile(tpath, []byte(fakeTranscriptTitled), 0o644)
	name := "loom-" + id
	tm.NewSession(name, cwd, "sleep 60", 80, 24)
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id, ProjectLabel: "parallax",
		Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "running"})

	e := NewEngine(tm, st, ccd)
	snap, err := e.Poll(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r := snap.Live[0]
	if r.Title != "probe the flux" || r.CtxTokens != 40100 {
		t.Fatalf("row = title %q ctx %d", r.Title, r.CtxTokens)
	}
	persisted, _, _ := st.Get(name)
	if persisted.Title != "probe the flux" {
		t.Fatalf("title not persisted: %+v", persisted)
	}
	// bell fires exactly once: running → needs_you
	if len(snap.NewlyNeedsYou) != 1 || snap.NewlyNeedsYou[0] != "parallax · probe the flux" {
		t.Fatalf("NewlyNeedsYou = %v", snap.NewlyNeedsYou)
	}
	snap2, _ := e.Poll(time.Now().Add(time.Hour))
	if len(snap2.NewlyNeedsYou) != 0 {
		t.Fatalf("bell repeated: %v", snap2.NewlyNeedsYou)
	}
}
```

- [ ] **Step 2: Verify failures** — `go test ./internal/store/ ./internal/status/ -count=1` FAILs.

- [ ] **Step 3: Implement**

`store.go`: append migration v3 `ALTER TABLE sessions ADD COLUMN title TEXT NOT NULL DEFAULT ''`; add `Title` to `SessionRow` (last field), `", title"` to the `cols` const (LAST position), one more placeholder + `excluded.title` in Upsert, `&r.Title` LAST in both scan paths, and:
```go
func (s *Store) SetTitle(name, title string) error {
	_, err := s.db.Exec("UPDATE sessions SET title=? WHERE name=?", title, name)
	return err
}
```

`engine.go`: `Row` gains `Title string` and `CtxTokens int64`; `Snapshot` gains `NewlyNeedsYou []string`. In the live-row loop:
```go
		rs, _ := rd.Poll()
		if rs.Title != "" && rs.Title != r.Title {
			_ = e.st.SetTitle(r.Name, rs.Title)
			r.Title = rs.Title
		}
		paneActive := now.Unix()-activity[r.Name] <= int64(activeWindow/time.Second)
		st := Fuse(rs.State, paneActive)
		if st == NeedsYou && r.LastStatus != string(NeedsYou) {
			label := r.ProjectLabel
			if r.Title != "" {
				label += " · " + r.Title
			}
			newly = append(newly, label)
		}
		if string(st) != r.LastStatus { ... SetStatus as today ... }
		live = append(live, Row{SessionRow: r, Status: st, LastTool: rs.LastTool,
			Activity: activity[r.Name], Title: r.Title, CtxTokens: rs.CtxTokens})
```
(`newly` declared before the loop; `Snapshot{Live: live, Recent: recent, NewlyNeedsYou: newly}`.)

- [ ] **Step 4: Verify pass, full suite, commit**

`go test -race -count=1 ./... && go vet ./...` — green.
```bash
git add internal/store internal/status && git commit -m "feat: persisted session titles, ctx tokens, and needs-you transition events"
```

---

### Task 3: UI — ctx column, titles, scrolling, bell, peek

**Files:**
- Modify: `internal/ui/text.go`, `internal/ui/text_test.go`, `internal/ui/app.go`, `internal/ui/app_test.go`
- Create: `internal/ui/notify.go`

**Interfaces:**
- Produces: `humanTokens(int64) string`, `padLeft(s string, w int) string`, `windowBody(body []string, cursorLine, maxH int) []string`, `viewPeek`, `type peekMsg struct{ name, content string }`, `notifyCmd([]string) tea.Cmd`.

- [ ] **Step 1: Write failing tests**

`text_test.go` additions:
```go
func TestHumanTokens(t *testing.T) {
	cases := map[int64]string{0: "", -1: "", 640: "640", 1000: "1k", 80612: "80k", 823_400: "823k", 1_200_000: "1.2M"}
	for n, want := range cases {
		if got := humanTokens(n); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestPadLeft(t *testing.T) {
	if got := padLeft("82k", 4); got != " 82k" {
		t.Errorf("padLeft = %q", got)
	}
	if got := padLeft("1.0M", 4); got != "1.0M" {
		t.Errorf("padLeft exact = %q", got)
	}
	if got := padLeft("abcde", 4); got != "abcde" {
		t.Errorf("padLeft never truncates: %q", got)
	}
}
```

`app_test.go` additions (existing tests untouched):
```go
func TestWindowBody(t *testing.T) {
	body := make([]string, 30)
	for i := range body {
		body[i] = fmt.Sprintf("line%d", i)
	}
	// fits: unchanged
	if out := windowBody(body[:5], 2, 10); len(out) != 5 {
		t.Fatalf("no-window len = %d", len(out))
	}
	for _, cursor := range []int{0, 1, 15, 28, 29} {
		out := windowBody(body, cursor, 10)
		if len(out) != 10 {
			t.Fatalf("cursor %d: len = %d", cursor, len(out))
		}
		found := false
		for _, l := range out {
			if l == fmt.Sprintf("line%d", cursor) {
				found = true
			}
		}
		if !found {
			t.Fatalf("cursor %d line not visible: %v", cursor, out)
		}
	}
	mid := windowBody(body, 15, 10)
	if !strings.Contains(mid[0], "more ↑") || !strings.Contains(mid[9], "more ↓") {
		t.Fatalf("markers missing: first=%q last=%q", mid[0], mid[9])
	}
}

func TestTitleShownInActivity(t *testing.T) {
	a := fixtureApp()
	a.snap.Live[0].Title = "fix booking race"
	a.rebuildRows()
	if !strings.Contains(a.View(), "fix booking race") {
		t.Fatal("title missing from view")
	}
}

func TestCtxColumnShown(t *testing.T) {
	a := fixtureApp()
	a.snap.Live[1].CtxTokens = 80612
	a.rebuildRows()
	if !strings.Contains(a.View(), "80k") {
		t.Fatal("ctx column missing")
	}
}

func TestPeekFlow(t *testing.T) {
	a := fixtureApp()
	a.Update(key(" ")) // cursor on live row 0
	if a.view != viewPeek {
		t.Fatalf("view = %v, want peek", a.view)
	}
	if a.peekTarget.name != "loom-b" {
		t.Fatalf("peekTarget = %q (must be captured at open)", a.peekTarget.name)
	}
	a.Update(peekMsg{name: "loom-b", content: "hello from the pane\nline two"})
	if !strings.Contains(a.View(), "hello from the pane") {
		t.Fatal("peek content missing")
	}
	// stale peekMsg for another session is discarded
	a.Update(peekMsg{name: "loom-zzz", content: "WRONG"})
	if strings.Contains(a.View(), "WRONG") {
		t.Fatal("stale peek content applied")
	}
	// frame invariant holds in peek
	for _, line := range strings.Split(a.View(), "\n") {
		if lw := lipgloss.Width(line); lw != a.width {
			t.Fatalf("peek line width %d != %d", lw, a.width)
		}
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewDash {
		t.Fatal("esc did not close peek")
	}
}

func TestPeekNoopOnRecentRow(t *testing.T) {
	a := fixtureApp()
	for i := 0; i < 10; i++ {
		a.Update(key("j")) // land on the recent row (clamped)
	}
	a.Update(key(" "))
	if a.view != viewDash {
		t.Fatal("peek opened on recent row")
	}
}

func TestSnapMsgWithTransitionsEmitsNotify(t *testing.T) {
	a := fixtureApp()
	_, cmd := a.Update(snapMsg(status.Snapshot{NewlyNeedsYou: []string{"tavli · fix race"}}))
	if cmd == nil {
		t.Fatal("expected a notify command for transitions")
	}
}
```

- [ ] **Step 2: Verify failures** — `go test ./internal/ui/ -count=1` FAILs.

- [ ] **Step 3: Implement**

`text.go`:
```go
// humanTokens renders a compact token count ("640","82k","1.2M"); blank ≤0.
func humanTokens(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
}

// padLeft left-pads plain text to w runes; never truncates.
func padLeft(s string, w int) string {
	if n := w - len([]rune(s)); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}
```

`notify.go`:
```go
package ui

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// notifyCmd raises an attention signal for sessions that just flipped to
// needs-you: a macOS notification (with sound) or a terminal bell elsewhere.
func notifyCmd(items []string) tea.Cmd {
	return func() tea.Msg {
		if runtime.GOOS == "darwin" {
			script := fmt.Sprintf("display notification %q with title \"Loom\" sound name \"Glass\"",
				strings.Join(items, ", "))
			_ = exec.Command("osascript", "-e", script).Run()
		} else {
			_, _ = os.Stderr.WriteString("\a")
		}
		return nil
	}
}
```

`app.go` changes:
1. `uiRow` gains `title string`, `ctx int64`; `rebuildRows` threads `Title`/`CtxTokens` from live rows (recent rows: `title: r.Title` from the store row, ctx 0).
2. `activityText`: after computing the base state text and before the seed-failed suffix, `if r.title != "" { base += " · " + r.title }`.
3. `renderRow`: fixed budget 36 → 41; insert `styMeta.Render(padLeft(truncPlain(humanTokens(r.ctx), 4), 4))` between activity and meta columns.
4. Scrolling: while building dashboard body, record `cursorLine` (the body index of the cursor's row line); after assembling body (before error/tmux-hint lines are appended — include those first, THEN window):
```go
	if a.height > 2 {
		body = windowBody(body, cursorLine, a.height-2)
	}
```
`windowBody` (in app.go):
```go
// windowBody keeps at most maxH body lines with cursorLine visible,
// replacing clipped edges with dim "… N more" markers.
func windowBody(body []string, cursorLine, maxH int) []string {
	if maxH <= 2 || len(body) <= maxH {
		return body
	}
	off := cursorLine - maxH/2
	if off < 0 {
		off = 0
	}
	if off > len(body)-maxH {
		off = len(body) - maxH
	}
	out := make([]string, maxH)
	copy(out, body[off:off+maxH])
	if off > 0 {
		out[0] = styChrome.Render(fmt.Sprintf("… %d more ↑", off))
	}
	if rest := len(body) - off - maxH; rest > 0 {
		out[maxH-1] = styChrome.Render(fmt.Sprintf("… %d more ↓", rest))
	}
	return out
}
```
(Centering keeps the cursor line strictly inside the markers for maxH ≥ 3 — asserted by the test.)
5. Peek: add `viewPeek` to the view consts; App fields `peekTarget struct{ name, label string }`, `peekContent string`; message `type peekMsg struct{ name, content string }`.
   - Dash key `" "`: on a live selected row → capture target, `a.view = viewPeek`, `a.peekContent = ""`, return `a.peekCmd()`.
   - `peekCmd`:
```go
func (a *App) peekCmd() tea.Cmd {
	if a.deps.Tmux == nil {
		return nil
	}
	tm, name := a.deps.Tmux, a.peekTarget.name
	return func() tea.Msg {
		out, err := tm.CapturePane(name)
		if err != nil {
			return peekMsg{name: name, content: "(pane unavailable)"}
		}
		return peekMsg{name: name, content: out}
	}
}
```
   - `Update`: `case peekMsg:` apply only when `a.view == viewPeek && m.name == a.peekTarget.name`. `case tickMsg:` when `a.view == viewPeek`, return `tea.Batch(a.pollCmd(), tickAfter(), a.peekCmd())` (live refresh).
   - Peek keys: `esc`/`" "` → `viewDash`; `enter` → the same `tea.ExecProcess` attach as the dashboard, on `a.peekTarget.name`; `q`/`ctrl+c` quit.
   - `View` `case viewPeek:` body = last `height-2` lines of `strings.Split(strings.TrimRight(a.peekContent, "\n"), "\n")`, each `truncPlain(line, inner)` (capture-pane without `-e` is plain text); `frame(w, "peek · "+a.peekTarget.label, "", body, "space/esc back · ↵ attach · q quit")`.
6. Bell: in the `snapMsg` case, after `rebuildRows`, `if len(m.NewlyNeedsYou) > 0 { return a, notifyCmd(m.NewlyNeedsYou) }`.

- [ ] **Step 4: Verify pass, full suite, commit**

`go test -race -count=1 ./... && go vet ./... && gofmt -l .` — green/clean.
```bash
git add internal/ui && git commit -m "feat: ctx column, session titles, dashboard scrolling, attention bell, peek view"
```

---

### Task 4: Visual verification + polish

- [ ] **Step 1:** Build; capture real renders (throwaway tmux `-L loomviz`, kill-server after): dashboard at 100×30 and **100×12** (scrolling markers — if fewer than ~10 rows exist, temporarily seed the store DB copy… do NOT fabricate sessions on the product socket; instead verify scrolling via a width/height-constrained run showing the `… N more` markers only if enough real rows exist, otherwise rely on the unit tests and note it), peek open on a live session (`space`), launcher. Verify: ctx column right-aligned and blank-when-0; titles appear after state text; peek content live and frame exact; keybar updated if peek hint added (add `space peek` to the dashboard keybar if width allows — do it, it's discoverability).
- [ ] **Step 2:** Judge as a designer; fix nits with small diffs; re-run captures + `go test -race -count=1 ./...` after each.
- [ ] **Step 3:** Final commit `polish: qol sprint eyeball pass` (or state zero nits).

---

## Self-Review (performed during plan writing)

1. **Spec coverage:** titles capture/persist/display (T1/T2/T3) · ctx last-usage rule + column (T1/T3) · bell transition-in-engine + notify-in-UI + once-only test (T2/T3) · scrolling with markers + cursor-visible tests (T3) · peek with captured target, live refresh, recent no-op, frame invariant (T3) · sanctioned-test-edit boundary (constraints) · eyeball (T4).
2. **Placeholders:** none.
3. **Type consistency:** `ReaderSnapshot` fields match engine usage; `Row.Title/CtxTokens` match UI threading; `peekMsg`/`peekTarget` names consistent; store `cols` order (title LAST) matches scan order.
