package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

func fixtureApp() *App {
	a := NewApp(Deps{})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{
		Live: []status.Row{
			{SessionRow: store.SessionRow{Name: "loom-b", ProjectLabel: "tavli"}, Status: status.NeedsYou},
			{SessionRow: store.SessionRow{Name: "loom-a", ProjectLabel: "parallax"}, Status: status.Running, LastTool: "Edit"},
			{SessionRow: store.SessionRow{Name: "loom-c", ProjectLabel: "volar"}, Status: status.Idle},
		},
		Recent: []store.SessionRow{
			{Name: "loom-d", ProjectLabel: "gloom", LastStatus: "done"},
		},
	}
	a.rebuildRows()
	return a
}

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// Attention queue: NeedsYou floats above Running above Idle; Recent last.
func TestRowOrdering(t *testing.T) {
	a := fixtureApp()
	got := make([]string, len(a.rows))
	for i, r := range a.rows {
		got[i] = r.name
	}
	want := []string{"loom-b", "loom-a", "loom-c", "loom-d"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestCursorMovesAndClamps(t *testing.T) {
	a := fixtureApp()
	a.Update(key("j"))
	a.Update(key("j"))
	if a.cursor != 2 {
		t.Fatalf("cursor = %d, want 2", a.cursor)
	}
	for i := 0; i < 10; i++ {
		a.Update(key("j"))
	}
	if a.cursor != 3 {
		t.Fatalf("cursor clamped = %d, want 3", a.cursor)
	}
	for i := 0; i < 10; i++ {
		a.Update(key("k"))
	}
	if a.cursor != 0 {
		t.Fatalf("cursor floor = %d, want 0", a.cursor)
	}
}

func TestNOpensLauncherAndEscCloses(t *testing.T) {
	a := fixtureApp()
	a.Update(key("n"))
	if a.view != viewLauncher {
		t.Fatalf("view = %v, want launcher", a.view)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewDash {
		t.Fatalf("view = %v, want dash after esc", a.view)
	}
}

func TestKillNeedsConfirm(t *testing.T) {
	a := fixtureApp()
	a.Update(key("x"))
	if a.view != viewConfirmKill {
		t.Fatalf("view = %v, want confirm", a.view)
	}
	// finding 5: the target is captured at confirm-open time.
	if a.actionTarget.name != "loom-b" {
		t.Fatalf("actionTarget = %+v, want loom-b captured at confirm-open time", a.actionTarget)
	}
	// rows reorder under the cursor (as they do every 1.5s poll): loom-b
	// (was NeedsYou, cursor 0) demoted to Idle, loom-a promoted to NeedsYou.
	a.snap.Live[0].Status = status.Idle
	a.snap.Live[1].Status = status.NeedsYou
	a.rebuildRows()
	if got, ok := a.selected(); !ok || got.name != "loom-a" {
		t.Fatalf("selected() after reorder = %+v, want loom-a now under the cursor", got)
	}
	if a.actionTarget.name != "loom-b" {
		t.Fatalf("actionTarget changed after snapshot update: %+v, want stable loom-b", a.actionTarget)
	}
	a.Update(key("n")) // decline
	if a.view != viewDash {
		t.Fatalf("view = %v, want dash after decline", a.view)
	}
}

// TestKillActsOnCapturedTargetNotLiveCursor is the full end-to-end guard for
// finding 5: press 'x' on one row, let a poll reorder the rows under the
// cursor, then press 'y' — the ORIGINAL row must be killed, not whatever the
// cursor now points at.
func TestKillActsOnCapturedTargetNotLiveCursor(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomapp%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := tm.NewSession("loom-b", dir, "sleep 30", 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := tm.NewSession("loom-a", dir, "sleep 30", 80, 24); err != nil {
		t.Fatal(err)
	}

	a := NewApp(Deps{Tmux: tm})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{
		Live: []status.Row{
			{SessionRow: store.SessionRow{Name: "loom-b", ProjectLabel: "tavli"}, Status: status.NeedsYou},
			{SessionRow: store.SessionRow{Name: "loom-a", ProjectLabel: "parallax"}, Status: status.Running},
		},
	}
	a.rebuildRows()

	a.Update(key("x")) // cursor 0 == loom-b (NeedsYou floats first)
	if a.view != viewConfirmKill || a.actionTarget.name != "loom-b" {
		t.Fatalf("confirm not opened on loom-b: view=%v target=%+v", a.view, a.actionTarget)
	}

	// simulate a poll landing while the confirm dialog is open: loom-b
	// demoted, loom-a promoted — cursor 0 now points at a DIFFERENT session.
	a.snap.Live[0].Status = status.Idle
	a.snap.Live[1].Status = status.NeedsYou
	a.rebuildRows()
	if got, ok := a.selected(); !ok || got.name != "loom-a" {
		t.Fatalf("selected() after reorder = %+v, want loom-a", got)
	}

	_, cmd := a.Update(key("y"))
	if cmd == nil {
		t.Fatal("'y' did not return a kill command")
	}
	msg := cmd()
	if _, ok := msg.(pollNowMsg); !ok {
		if em, ok := msg.(errMsg); ok {
			t.Fatalf("kill command errored: %v", em.err)
		}
		t.Fatalf("kill command returned %T, want pollNowMsg", msg)
	}

	if tm.HasSession("loom-b") {
		t.Fatal("loom-b (the ORIGINAL captured target) was not killed")
	}
	if !tm.HasSession("loom-a") {
		t.Fatal("loom-a (only the live cursor row, never confirmed) was killed instead")
	}
}

// TestTagSavesToCapturedTarget guards the tag half of finding 5: 't' must
// capture the target at open time and save to it, not to whatever selected()
// returns when enter is pressed.
func TestTagSavesToCapturedTarget(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Upsert(store.SessionRow{Name: "loom-b", ProjectLabel: "tavli", CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "needs_you"})
	st.Upsert(store.SessionRow{Name: "loom-a", ProjectLabel: "parallax", CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "running"})

	a := NewApp(Deps{Launcher: &session.Launcher{Store: st}})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{
		Live: []status.Row{
			{SessionRow: store.SessionRow{Name: "loom-b", ProjectLabel: "tavli"}, Status: status.NeedsYou},
			{SessionRow: store.SessionRow{Name: "loom-a", ProjectLabel: "parallax"}, Status: status.Running},
		},
	}
	a.rebuildRows()

	a.Update(key("t")) // cursor 0 == loom-b
	if a.view != viewTag || a.actionTarget.name != "loom-b" {
		t.Fatalf("tag view not opened on loom-b: view=%v target=%+v", a.view, a.actionTarget)
	}

	// reorder under the cursor while the tag dialog is open
	a.snap.Live[0].Status = status.Idle
	a.snap.Live[1].Status = status.NeedsYou
	a.rebuildRows()
	if got, ok := a.selected(); !ok || got.name != "loom-a" {
		t.Fatalf("selected() after reorder = %+v, want loom-a", got)
	}

	a.tag.SetValue("urgent")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	b, _, _ := st.Get("loom-b")
	if b.Tags != "urgent" {
		t.Fatalf("loom-b (captured target) Tags = %q, want urgent", b.Tags)
	}
	aa, _, _ := st.Get("loom-a")
	if aa.Tags != "" {
		t.Fatalf("loom-a (live cursor row, never opened for tagging) Tags = %q, want empty", aa.Tags)
	}
}

func TestViewRendersSections(t *testing.T) {
	a := fixtureApp()
	out := a.View()
	for _, want := range []string{"NEEDS YOU", "RUNNING", "IDLE", "RECENT", "parallax", "tavli", "Edit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("View() missing %q:\n%s", want, out)
		}
	}
}

// TestViewShowsSeedFailedHint guards finding 4: a row whose seed delivery
// failed must surface that in the dashboard, not vanish silently.
func TestViewShowsSeedFailedHint(t *testing.T) {
	a := fixtureApp()
	a.snap.Live[1].SeedStatus = "failed"
	a.rebuildRows()
	out := a.View()
	if !strings.Contains(out, "seed failed") {
		t.Fatalf("View() missing seed failed hint:\n%s", out)
	}
}

// TestLaunchAndResumeCmdsEmitPollNowNotTick is the ui-side regression guard
// for finding 1: launch and resume ALSO used to return a raw tickMsg (like
// kill), and the tickMsg handler responds with tea.Batch(pollCmd,
// tickAfter()) — so each launch/resume/kill permanently added another
// perpetual 1.5s tick loop, stacking concurrent Engine.Poll goroutines. Both
// commands must instead return the one-shot pollNowMsg. (Kill's half of this
// guard lives in TestKillActsOnCapturedTargetNotLiveCursor above.)
func TestLaunchAndResumeCmdsEmitPollNowNotTick(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomapp2%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	l := &session.Launcher{Tmux: tm, Store: st,
		ReadyTimeout: 200 * time.Millisecond, PollEvery: 50 * time.Millisecond}

	a := NewApp(Deps{Launcher: l, Projects: []registry.Project{{Label: "p", Path: dir}}, Tmux: tm})
	a.width, a.height = 80, 24

	// launch: 'n' opens the launcher form; enter submits the (no-seed) default recipe.
	a.Update(key("n"))
	if a.view != viewLauncher {
		t.Fatalf("view = %v, want launcher", a.view)
	}
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("launch did not return a command")
	}
	msg := cmd()
	if em, ok := msg.(errMsg); ok {
		t.Fatalf("launch command errored: %v", em.err)
	}
	if _, ok := msg.(pollNowMsg); !ok {
		t.Fatalf("launch command returned %T, want pollNowMsg (NOT tickMsg — finding 1)", msg)
	}

	// resume: seed a "recent" row and press 'r'.
	a.snap = status.Snapshot{
		Recent: []store.SessionRow{{Name: "loom-old", ProjectLabel: "p", Cwd: dir, LastStatus: "done"}},
	}
	a.rebuildRows()
	_, cmd = a.Update(key("r"))
	if cmd == nil {
		t.Fatal("resume did not return a command")
	}
	msg = cmd()
	if em, ok := msg.(errMsg); ok {
		t.Fatalf("resume command errored: %v", em.err)
	}
	if _, ok := msg.(pollNowMsg); !ok {
		t.Fatalf("resume command returned %T, want pollNowMsg (NOT tickMsg — finding 1)", msg)
	}
}

// TestViewWideRuneProjectLabelNoPanic is the end-to-end regression test for
// the MAJOR wide-rune finding: renderRow truncates/pads the project column
// by RUNE count (truncPlain/padPlain), so a CJK ProjectLabel can come out
// wider in CELLS than the column's budget without being clipped at all,
// pushing the whole row past `inner` and hitting frame()'s (now-fixed)
// hard-clip path. View() must never panic and every line must stay exactly
// a.width cells.
//
// Note: at width 40, renderRow's actW (inner-36) is <=0, so rows take the
// short "no activity/meta/age columns" branch and the overflow this finding
// describes doesn't reach frame() at that width — the row is short enough
// regardless of the label. Width 80 (inner=76, actW=40) is the narrowest
// width where the row is built with all columns and reliably reproduces the
// pre-fix panic, so that's what this test uses.
func TestViewWideRuneProjectLabelNoPanic(t *testing.T) {
	a := fixtureApp()
	a.width, a.height = 80, 24
	a.snap.Live[0].ProjectLabel = strings.Repeat("漢", 12)
	a.rebuildRows()
	out := a.View()
	for i, line := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(line); lw != a.width {
			t.Errorf("wide-rune project label line %d: got %d cells, want %d: %q", i, lw, a.width, line)
		}
	}
}

func TestViewFrameInvariantAllViews(t *testing.T) {
	a := fixtureApp()
	views := []func(){
		func() {},                     // dashboard
		func() { a.Update(key("n")) }, // launcher
		func() { a.Update(tea.KeyMsg{Type: tea.KeyEsc}); a.Update(key("x")) },                     // confirm
		func() { a.Update(key("n")); a.Update(tea.KeyMsg{Type: tea.KeyEsc}); a.Update(key("t")) }, // tag
	}
	for i, setup := range views {
		setup()
		for j, line := range strings.Split(a.View(), "\n") {
			if lw := lipgloss.Width(line); lw != a.width {
				t.Fatalf("view %d line %d: %d cells (want %d): %q", i, j, lw, a.width, line)
			}
		}
	}
}

// TestViewNarrowNoPanic is also the regression test for the Critical finding:
// at width 40 the real ~53-cell keybar must not push the border past width.
// Every rendered line — for every view, not just the dashboard — must be
// exactly a.width cells.
func TestViewNarrowNoPanic(t *testing.T) {
	a := fixtureApp()
	a.width, a.height = 40, 12

	assertExactWidth := func(t *testing.T, label string) {
		t.Helper()
		out := a.View()
		for i, line := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(line); lw != a.width {
				t.Errorf("%s line %d: got %d cells, want %d: %q", label, i, lw, a.width, line)
			}
		}
	}

	assertExactWidth(t, "viewDash")

	a.view = viewLauncher
	assertExactWidth(t, "viewLauncher")

	a.view = viewConfirmKill
	r, ok := a.selected()
	if !ok {
		t.Fatal("no row selected for viewConfirmKill fixture")
	}
	a.actionTarget = r
	assertExactWidth(t, "viewConfirmKill")

	a.view = viewTag
	assertExactWidth(t, "viewTag")
}

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

func TestPeekShowsError(t *testing.T) {
	a := fixtureApp()
	a.Update(key(" ")) // open peek
	if a.view != viewPeek {
		t.Fatalf("view = %v, want peek", a.view)
	}
	a.errStr = "attach failed: connection refused"
	out := a.View()
	if !strings.Contains(out, "attach failed: connection refused") {
		t.Fatalf("peek view missing error text:\n%s", out)
	}
}

func TestSnapMsgWithTransitionsEmitsNotify(t *testing.T) {
	a := fixtureApp()
	_, cmd := a.Update(snapMsg(status.Snapshot{NewlyNeedsYou: []string{"tavli · fix race"}}))
	if cmd == nil {
		t.Fatal("expected a notify command for transitions")
	}
}

// TestSlashOpensSearch: '/' from the dashboard opens viewSearch with a
// fresh, focused input and empty results (spec §6), and the frame invariant
// (every line exactly a.width cells) holds at both a wide and a narrow
// width.
func TestSlashOpensSearch(t *testing.T) {
	a := fixtureApp()
	a.Update(key("/"))
	if a.view != viewSearch {
		t.Fatalf("view = %v, want viewSearch", a.view)
	}
	if !a.searchInput.Focused() {
		t.Fatal("search input not focused")
	}
	if len(a.searchHits) != 0 {
		t.Fatalf("search results not empty on open: %+v", a.searchHits)
	}
	for _, width := range []int{100, 40} {
		a.width = width
		for i, line := range strings.Split(a.View(), "\n") {
			if lw := lipgloss.Width(line); lw != width {
				t.Fatalf("width %d line %d: got %d cells (want %d): %q", width, i, lw, width, line)
			}
		}
	}
}

// TestDebounceCmdCarriesSeq pins the shape of the debounce tick message
// itself (the 200ms delay is real but small enough to eat in a unit test).
func TestDebounceCmdCarriesSeq(t *testing.T) {
	msg := debounceCmd(42)()
	dm, ok := msg.(searchDebounceMsg)
	if !ok || dm.seq != 42 {
		t.Fatalf("debounceCmd(42)() = %#v, want searchDebounceMsg{seq:42}", msg)
	}
}

// TestSearchTypingBumpsSeqAndEmitsCmd: every keystroke that changes the
// input bumps searchSeq and returns a (debounce) command — checked without
// actually invoking the 200ms tick, which TestDebounceCmdCarriesSeq covers
// in isolation.
func TestSearchTypingBumpsSeqAndEmitsCmd(t *testing.T) {
	a := fixtureApp()
	a.Update(key("/"))
	seqBefore := a.searchSeq
	_, cmd := a.Update(key("w"))
	if a.searchSeq == seqBefore {
		t.Fatal("typing did not bump searchSeq")
	}
	if cmd == nil {
		t.Fatal("typing did not return a command")
	}
	if a.searchInput.Value() != "w" {
		t.Fatalf("input value = %q, want \"w\"", a.searchInput.Value())
	}
}

// TestSearchEmptyInputClearsResults: clearing the input back to empty
// clears results immediately (spec §6: "Empty input → results cleared, no
// query").
func TestSearchEmptyInputClearsResults(t *testing.T) {
	a := fixtureApp()
	a.Update(key("/"))
	a.Update(key("w"))
	a.searchHits = []store.SearchHit{{SessionID: "s1"}}
	a.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if a.searchInput.Value() != "" {
		t.Fatalf("input value = %q, want empty after backspace", a.searchInput.Value())
	}
	if a.searchHits != nil {
		t.Fatalf("results not cleared on empty input: %+v", a.searchHits)
	}
}

// TestSearchResultsRenderHits: a searchResultsMsg matching the current input
// renders each hit's project label, title, and snippet.
func TestSearchResultsRenderHits(t *testing.T) {
	a := fixtureApp()
	a.Update(key("/"))
	a.searchInput.SetValue("widget")
	hits := []store.SearchHit{
		{SessionID: "s1", Title: "fix the widget", ProjectDir: "-Users-h-Sauce-HappyPay",
			Cwd: "/w/happypay", Snippet: "talking about the \x01widget\x02 again", LastTS: time.Now().Unix()},
	}
	a.Update(searchResultsMsg{query: "widget", hits: hits})
	out := a.View()
	if !strings.Contains(out, "happypay") {
		t.Fatalf("project label missing:\n%s", out)
	}
	if !strings.Contains(out, "fix the widget") {
		t.Fatalf("title missing:\n%s", out)
	}
	if !strings.Contains(out, "widget") {
		t.Fatalf("snippet missing:\n%s", out)
	}
	if strings.ContainsAny(out, "\x01\x02") {
		t.Fatalf("rendered view leaked raw snippet markers:\n%s", out)
	}
}

// TestSearchStaleResultsDiscarded: a searchResultsMsg for a query that no
// longer matches the live input (the user kept typing) is discarded — same
// staleness discipline as peekMsg.
func TestSearchStaleResultsDiscarded(t *testing.T) {
	a := fixtureApp()
	a.Update(key("/"))
	a.searchInput.SetValue("newquery")
	a.Update(searchResultsMsg{query: "oldquery", hits: []store.SearchHit{{SessionID: "stale", Title: "STALE HIT"}}})
	if len(a.searchHits) != 0 {
		t.Fatalf("stale results applied: %+v", a.searchHits)
	}
}

// TestSearchEnterOpensDetail: '↵' on a selected result captures it as
// detailTarget and switches to viewDetail (Task 3 fleshes viewDetail out;
// Task 2 only needs the capture + a minimal body).
func TestSearchEnterOpensDetail(t *testing.T) {
	a := fixtureApp()
	a.Update(key("/"))
	a.searchHits = []store.SearchHit{{SessionID: "sess-1", Title: "hello"}}
	a.searchCursor = 0
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.view != viewDetail {
		t.Fatalf("view = %v, want viewDetail", a.view)
	}
	if a.detailTarget.SessionID != "sess-1" {
		t.Fatalf("detailTarget = %+v, want sess-1 captured", a.detailTarget)
	}
	for _, line := range strings.Split(a.View(), "\n") {
		if lw := lipgloss.Width(line); lw != a.width {
			t.Fatalf("viewDetail line width %d != %d", lw, a.width)
		}
	}
}

// TestSearchEscReturnsToDash: 'esc' from search goes back to the dashboard.
func TestSearchEscReturnsToDash(t *testing.T) {
	a := fixtureApp()
	a.Update(key("/"))
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewDash {
		t.Fatalf("view = %v, want viewDash after esc", a.view)
	}
}

// TestCtrlCQuitsFromSearchAndTag guards the red-team finding: ctrl+c must
// quit even from the free-text input views, checked BEFORE the keystroke is
// forwarded to the textinput (which would otherwise silently swallow it).
func TestCtrlCQuitsFromSearchAndTag(t *testing.T) {
	a := fixtureApp()
	a.Update(key("/"))
	if a.view != viewSearch {
		t.Fatalf("view = %v, want viewSearch", a.view)
	}
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c in search did not return a command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+c in search did not quit")
	}

	b := fixtureApp()
	b.Update(key("t"))
	if b.view != viewTag {
		t.Fatalf("view = %v, want viewTag", b.view)
	}
	_, cmd2 := b.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd2 == nil {
		t.Fatal("ctrl+c in tag did not return a command")
	}
	if _, ok := cmd2().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+c in tag did not quit")
	}
}

// TestTickMsgInSearchViewSchedulesStatusRefresh checks the tickMsg-in-
// viewSearch WIRING only (ONE tickAfter per tickMsg, plus the extra one-shot
// status-refresh cmd riding the same batch — finding 1 precedent) without
// invoking the real 1.5s tickAfter/pollInterval timer, which would make this
// test slow for no extra coverage (the timer's own shape is tea's, not
// ours).
func TestTickMsgInSearchViewSchedulesStatusRefresh(t *testing.T) {
	a := fixtureApp() // Deps{}: Engine nil → pollCmd() is nil, filtered out of the batch
	a.Update(key("/"))
	_, cmd := a.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tick in viewSearch returned no command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("tick in viewSearch cmd = %T, want tea.BatchMsg", cmd())
	}
	if len(batch) != 2 {
		t.Fatalf("batch len = %d, want 2 (tickAfter + searchStatusCmd; pollCmd nil-filtered)", len(batch))
	}
}

// TestSearchStatusMsgRefiresQueryOnActiveAndEdge drives the re-query
// decision directly (active, and the active→inactive edge) against a real
// store, without going through the slow tickAfter/pollInterval timer.
func TestSearchStatusMsgRefiresQueryOnActiveAndEdge(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertTranscript(store.Transcript{
		SessionID: "sess-1", ProjectDir: "loom", Cwd: "/w/loom", Title: "widget session",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceFileDocs(store.IndexedFile{Path: "/f1", SessionID: "sess-1", Size: 1, Mtime: 1},
		[]store.Doc{{Content: "let's fix the widget today", Role: "user", TS: 1}}); err != nil {
		t.Fatal(err)
	}

	a := NewApp(Deps{Store: st})
	a.width, a.height = 100, 30
	a.Update(key("/"))
	a.searchInput.SetValue("widget")

	// Active → re-fires the current query.
	_, cmd := a.Update(searchStatusMsg{active: true, count: 1})
	if cmd == nil {
		t.Fatal("active status did not re-fire the query")
	}
	msg := cmd()
	rm, ok := msg.(searchResultsMsg)
	if !ok || rm.query != "widget" {
		t.Fatalf("expected searchResultsMsg{query:\"widget\"}, got %#v", msg)
	}
	a.Update(rm)
	if len(a.searchHits) == 0 {
		t.Fatal("expected hits after the active-status re-fired query")
	}
	if a.searchCount != 1 || !a.searchActive {
		t.Fatalf("cached count/active not updated: count=%d active=%v", a.searchCount, a.searchActive)
	}

	// active → inactive edge also re-fires once more (self-healing
	// first-run: catches the final results after a sweep completes).
	_, cmd2 := a.Update(searchStatusMsg{active: false, count: 1})
	if cmd2 == nil {
		t.Fatal("active->inactive edge did not re-fire the query")
	}
	if a.searchActive { // sanity: cached flag now reflects inactive
		t.Fatal("searchActive should now be false")
	}
}

func TestRowShowsAge(t *testing.T) {
	a := fixtureApp()
	a.now = time.Unix(2000, 0)
	a.snap.Live[1].Activity = 2000 - 120 // parallax row: 2m ago
	a.rebuildRows()
	if !strings.Contains(a.View(), "2m") {
		t.Fatal("age column missing")
	}
}

// --- Task 3: detail view + actions --------------------------------------

// detailFixtureStore opens a throwaway store seeded with one transcript row
// and returns the SearchHit a real search would have produced for it (used
// to drive a.openDetail — the same capture path '/' → type → ↵ takes).
func detailFixtureStore(t *testing.T, tr store.Transcript) (*store.Store, store.SearchHit) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertTranscript(tr); err != nil {
		t.Fatal(err)
	}
	hit := store.SearchHit{
		SessionID: tr.SessionID, Title: tr.Title, Ask: tr.Ask,
		ProjectDir: tr.ProjectDir, Cwd: tr.Cwd, LastTS: tr.LastTS,
		Snippet: "the \x01vega\x02 hedge desk",
	}
	return st, hit
}

// newFakeSummarizer wires a memory.Summarizer at a fake `claude` script that
// always succeeds with a fixed summary — Task 1's fake-script precedent,
// trimmed to what these UI tests need (argv/env/budget are Task 1's job;
// here we only need a Summarize call the UI can drive through a tea.Cmd).
func newFakeSummarizer(t *testing.T, st *store.Store) *memory.Summarizer {
	t.Helper()
	script := "#!/bin/sh\necho 'FAKE SUMMARY'\n"
	path := filepath.Join(t.TempDir(), "fake-claude.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &memory.Summarizer{Store: st, Binary: path, WorkDir: t.TempDir()}
}

// TestDetailRendersTranscriptFieldsAndSummaryHint: opening detail on a
// session with no llm_summary yet shows every transcript field plus the
// "press s to summarize" hint, and holds the frame invariant.
func TestDetailRendersTranscriptFieldsAndSummaryHint(t *testing.T) {
	tr := store.Transcript{
		SessionID: "sess-1", ProjectDir: "-w-happypay", Cwd: "/w/happypay",
		Title: "fix the widget", Ask: "make the widget work", Outcome: "widget fixed",
		Files: "a.go\nb.go", FirstTS: 1000, LastTS: 2000, MsgCount: 12,
	}
	st, hit := detailFixtureStore(t, tr)
	a := NewApp(Deps{Store: st})
	a.width, a.height = 100, 30
	a.now = time.Unix(3000, 0)
	a.openDetail(hit)

	out := a.View()
	for _, want := range []string{
		"fix the widget", "happypay", "/w/happypay", "make the widget work",
		"widget fixed", "a.go", "b.go", "12 messages",
		"press s to summarize (uses plan quota)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
	for i, line := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(line); lw != a.width {
			t.Fatalf("line %d width %d != %d: %q", i, lw, a.width, line)
		}
	}
}

// TestResumeBlockedOnLiveRowNeverCallsResume is the P0 test: a live row for
// the session's claude_session_id must short-circuit to a hint WITHOUT ever
// calling Launcher.Resume. The Launcher here has a nil Tmux client, so if
// the guard were bypassed and Resume were actually invoked, cmd() would
// panic dereferencing a nil *tmux.Client — making "Resume was never called"
// a hard, unmissable assertion rather than a soft one.
func TestResumeBlockedOnLiveRowNeverCallsResume(t *testing.T) {
	tr := store.Transcript{SessionID: "sess-1", Cwd: "/w/proj"}
	st, hit := detailFixtureStore(t, tr)
	if err := st.Upsert(store.SessionRow{
		Name: "loom-live", ClaudeSessionID: "sess-1", ProjectLabel: "proj", Cwd: "/w/proj",
		CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "running",
	}); err != nil {
		t.Fatal(err)
	}
	a := NewApp(Deps{Store: st, Launcher: &session.Launcher{Store: st}})
	a.width, a.height = 100, 30
	a.openDetail(hit)

	_, cmd := a.Update(key("r"))
	if cmd == nil {
		t.Fatal("r did not return a command")
	}
	msg := cmd() // would panic here if the guard were bypassed
	rb, ok := msg.(resumeBlockedMsg)
	if !ok || rb.sessionID != "sess-1" {
		t.Fatalf("resume on live row = %#v, want resumeBlockedMsg{sess-1}", msg)
	}
	a.Update(rb)
	if a.view != viewDetail {
		t.Fatalf("view = %v, want viewDetail unchanged", a.view)
	}
	if !strings.Contains(a.detailHint, "already live") {
		t.Fatalf("detailHint = %q, want the already-live hint", a.detailHint)
	}
	if !strings.Contains(a.View(), "already live") {
		t.Fatal("hint not rendered in view")
	}
}

// TestResumeTerminalRowUsesThatRowNotSynthesized: a terminal (done/error)
// row for the session DOES get resumed, and with THAT row's fields
// (label/model/mode/tags) — not a freshly synthesized one. Uses a real
// throwaway tmux socket + Launcher (Phase-1 launch_test pattern), the
// cheapest honest way to observe which row Resume actually acted on.
func TestResumeTerminalRowUsesThatRowNotSynthesized(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomdetail%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tr := store.Transcript{SessionID: "sess-1", Cwd: dir}
	st, hit := detailFixtureStore(t, tr)
	if err := st.Upsert(store.SessionRow{
		Name: "loom-old", ClaudeSessionID: "sess-1", ProjectLabel: "myproj", Cwd: dir,
		Model: "opus", Mode: "plan", Tags: "keep-me",
		CreatedAt: 1, EndedAt: 5, ExitCode: 0, LastStatus: "done",
	}); err != nil {
		t.Fatal(err)
	}
	l := &session.Launcher{Tmux: tm, Store: st,
		ReadyTimeout: 200 * time.Millisecond, PollEvery: 50 * time.Millisecond}
	a := NewApp(Deps{Store: st, Launcher: l})
	a.width, a.height = 80, 24
	a.openDetail(hit)

	_, cmd := a.Update(key("r"))
	if cmd == nil {
		t.Fatal("r did not return a command")
	}
	msg := cmd()
	if em, ok := msg.(errMsg); ok {
		t.Fatalf("resume errored: %v", em.err)
	}
	if _, ok := msg.(pollNowMsg); !ok {
		t.Fatalf("resume result = %T, want pollNowMsg", msg)
	}

	live, err := st.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("live rows = %d, want 1", len(live))
	}
	if live[0].ProjectLabel != "myproj" || live[0].Model != "opus" || live[0].Mode != "plan" || live[0].Tags != "keep-me" {
		t.Fatalf("resumed row = %+v, want fields copied from the terminal row (not synthesized)", live[0])
	}
}

// TestResumeSynthesizesRowWhenNoneExists: no sessions row at all for the
// claude_session_id → a minimal row is synthesized from the transcript
// (Cwd, label=basename(cwd)) and THAT is resumed.
func TestResumeSynthesizesRowWhenNoneExists(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomdetail2%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tr := store.Transcript{SessionID: "sess-orphan", Cwd: dir}
	st, hit := detailFixtureStore(t, tr)
	l := &session.Launcher{Tmux: tm, Store: st,
		ReadyTimeout: 200 * time.Millisecond, PollEvery: 50 * time.Millisecond}
	a := NewApp(Deps{Store: st, Launcher: l})
	a.width, a.height = 80, 24
	a.openDetail(hit)

	_, cmd := a.Update(key("r"))
	if cmd == nil {
		t.Fatal("r did not return a command")
	}
	msg := cmd()
	if em, ok := msg.(errMsg); ok {
		t.Fatalf("resume errored: %v", em.err)
	}
	if _, ok := msg.(pollNowMsg); !ok {
		t.Fatalf("resume result = %T, want pollNowMsg", msg)
	}

	live, err := st.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("live rows = %d, want 1", len(live))
	}
	if live[0].ProjectLabel != filepath.Base(dir) {
		t.Fatalf("synthesized ProjectLabel = %q, want %q", live[0].ProjectLabel, filepath.Base(dir))
	}
	if live[0].ClaudeSessionID != "sess-orphan" {
		t.Fatalf("synthesized ClaudeSessionID = %q, want sess-orphan", live[0].ClaudeSessionID)
	}
}

// TestResumeDisabledWhenCwdEmptyOrFileMissing: r is a no-op (and omitted
// from the keybar) whenever the transcript has no known cwd or its file
// has vanished.
func TestResumeDisabledWhenCwdEmptyOrFileMissing(t *testing.T) {
	cases := []store.Transcript{
		{SessionID: "s1"}, // no cwd
		{SessionID: "s2", Cwd: "/w/x", FileMissing: true}, // file missing
	}
	for _, tr := range cases {
		st, hit := detailFixtureStore(t, tr)
		a := NewApp(Deps{Store: st})
		a.width, a.height = 100, 30
		a.openDetail(hit)
		if strings.Contains(a.View(), "r resume") {
			t.Fatalf("keybar shows r resume when disabled (cwd=%q missing=%v)", tr.Cwd, tr.FileMissing)
		}
		_, cmd := a.Update(key("r"))
		if cmd != nil {
			t.Fatalf("r produced a command while disabled (cwd=%q missing=%v)", tr.Cwd, tr.FileMissing)
		}
	}
}

// TestSummarizeConfirmThenRegenerate: press-twice-to-regenerate when a
// summary already exists (spec §5) — first 's' arms the confirm without
// calling Summarize; second 's' runs it (in a tea.Cmd) and, on success,
// re-fetches the transcript so the new summary shows immediately.
func TestSummarizeConfirmThenRegenerate(t *testing.T) {
	tr := store.Transcript{SessionID: "sess-1", Cwd: "/w/proj", LLMSummary: "old summary"}
	st, hit := detailFixtureStore(t, tr)
	sm := newFakeSummarizer(t, st)
	a := NewApp(Deps{Store: st, Summarizer: sm})
	a.width, a.height = 100, 30
	a.openDetail(hit)

	_, cmd := a.Update(key("s"))
	if cmd != nil {
		t.Fatal("first s with an existing summary should arm confirm, not return a command")
	}
	if !a.detailConfirmRegen {
		t.Fatal("detailConfirmRegen not armed")
	}
	if !strings.Contains(a.View(), "press s again") {
		t.Fatal("confirm hint not shown in view")
	}

	_, cmd = a.Update(key("s"))
	if cmd == nil {
		t.Fatal("second s did not return a command")
	}
	if !a.detailSummarizing {
		t.Fatal("detailSummarizing not set on the regenerate press")
	}
	if !strings.Contains(a.View(), "summarizing…") {
		t.Fatal("summarizing state not shown in view")
	}

	msg := cmd()
	sMsg, ok := msg.(summaryMsg)
	if !ok || sMsg.err != nil || sMsg.text != "FAKE SUMMARY" {
		t.Fatalf("summarize cmd result = %#v", msg)
	}
	a.Update(sMsg)
	if a.detailSummarizing {
		t.Fatal("detailSummarizing not cleared after success")
	}
	if a.detailTranscript.LLMSummary != "FAKE SUMMARY" {
		t.Fatalf("transcript not re-fetched after success: LLMSummary = %q", a.detailTranscript.LLMSummary)
	}
}

// TestSummarizeFirstPressStartsImmediatelyWhenEmpty: no existing summary →
// the first 's' press starts summarizing right away (no confirm needed).
func TestSummarizeFirstPressStartsImmediatelyWhenEmpty(t *testing.T) {
	tr := store.Transcript{SessionID: "sess-1", Cwd: "/w/proj"}
	st, hit := detailFixtureStore(t, tr)
	sm := newFakeSummarizer(t, st)
	a := NewApp(Deps{Store: st, Summarizer: sm})
	a.width, a.height = 100, 30
	a.openDetail(hit)

	_, cmd := a.Update(key("s"))
	if cmd == nil {
		t.Fatal("s with no existing summary should start summarizing immediately")
	}
	if a.detailConfirmRegen {
		t.Fatal("detailConfirmRegen should not be armed when there was no prior summary")
	}
	if !a.detailSummarizing {
		t.Fatal("detailSummarizing not set")
	}
}

// TestSummarizeNoopWhileInFlight: further 's' presses while a Summarize
// tea.Cmd is in flight no-op (spec §5).
func TestSummarizeNoopWhileInFlight(t *testing.T) {
	tr := store.Transcript{SessionID: "sess-1", Cwd: "/w/proj"}
	st, hit := detailFixtureStore(t, tr)
	sm := newFakeSummarizer(t, st)
	a := NewApp(Deps{Store: st, Summarizer: sm})
	a.width, a.height = 100, 30
	a.openDetail(hit)

	if _, cmd := a.Update(key("s")); cmd == nil {
		t.Fatal("s did not start summarizing")
	}
	if _, cmd := a.Update(key("s")); cmd != nil {
		t.Fatal("s while summarizing returned a command (should no-op)")
	}
}

// TestSummaryMsgStaleSessionDiscarded: a summaryMsg for a session the user
// has since navigated away from (opened a DIFFERENT session's detail) is
// discarded — same staleness discipline as searchResultsMsg/peekMsg.
func TestSummaryMsgStaleSessionDiscarded(t *testing.T) {
	trA := store.Transcript{SessionID: "sess-a", Cwd: "/w/a"}
	trB := store.Transcript{SessionID: "sess-b", Cwd: "/w/b", LLMSummary: "b summary"}
	st, hitA := detailFixtureStore(t, trA)
	if err := st.UpsertTranscript(trB); err != nil {
		t.Fatal(err)
	}
	hitB := store.SearchHit{SessionID: "sess-b", Cwd: "/w/b"}

	a := NewApp(Deps{Store: st})
	a.width, a.height = 100, 30
	a.openDetail(hitA) // as if sess-a's summarize were in flight
	a.openDetail(hitB) // user navigated to a different session's detail

	a.Update(summaryMsg{sessionID: "sess-a", text: "STALE"})
	if a.detailTranscript.SessionID != "sess-b" || a.detailTranscript.LLMSummary != "b summary" {
		t.Fatalf("stale summaryMsg applied over the current session: %+v", a.detailTranscript)
	}
}

// TestDetailEscReturnsToSearchPreservingState: esc goes back to viewSearch
// with the input/results exactly as they were, not reset.
func TestDetailEscReturnsToSearchPreservingState(t *testing.T) {
	tr := store.Transcript{SessionID: "sess-1", Cwd: "/w/proj"}
	st, hit := detailFixtureStore(t, tr)
	a := NewApp(Deps{Store: st})
	a.width, a.height = 100, 30
	a.Update(key("/"))
	a.searchInput.SetValue("widget")
	a.searchHits = []store.SearchHit{hit}
	a.searchCursor = 0

	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.view != viewDetail {
		t.Fatal("enter did not open detail")
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewSearch {
		t.Fatalf("esc from detail = %v, want viewSearch", a.view)
	}
	if a.searchInput.Value() != "widget" {
		t.Fatalf("search input lost: %q, want \"widget\"", a.searchInput.Value())
	}
	if len(a.searchHits) != 1 {
		t.Fatalf("search hits lost: %+v", a.searchHits)
	}
}

// TestDetailFrameInvariantPopulatedContent: the frame invariant at both 100
// and 40 cells wide, with EVERY optional body element populated at once
// (long title/ask/outcome, >8 files, a multi-line LLM summary, the
// confirm-regenerate hint, the resume-blocked hint, and a snippet with
// markers straddling a truncation cut) — the B2-review lesson that a
// placeholder-only frame check can hide breakage that only shows up once
// the body is actually full.
func TestDetailFrameInvariantPopulatedContent(t *testing.T) {
	tr := store.Transcript{
		SessionID: "sess-1", ProjectDir: "-Users-h-Sauce-HappyPay", Cwd: "/Users/h/Sauce/HappyPay",
		Title:   "a very long session title that should be truncated cleanly at narrow widths without corrupting the frame",
		Ask:     "a very long ask line describing exactly what the user wanted done in this particular session",
		Outcome: "a very long outcome line summarizing everything that happened during this rather long session",
		Files: strings.Join([]string{
			"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go", "i.go", "j.go",
		}, "\n"),
		LLMSummary:  "Goal: do the thing.\nOutcome: did the thing.\nKey decisions: used the thing.",
		FirstTS:     1000,
		LastTS:      500000,
		MsgCount:    250,
		FileMissing: true,
	}
	st, hit := detailFixtureStore(t, tr)
	hit.Snippet = "a very long snippet with \x01highlighted\x02 terms straddling the truncation boundary right about here"

	a := NewApp(Deps{Store: st})
	a.now = time.Unix(600000, 0)
	a.openDetail(hit)
	a.detailHint = "already live — attach from the dashboard"
	a.detailConfirmRegen = true

	for _, width := range []int{100, 40} {
		a.width, a.height = width, 30
		out := a.View()
		for i, line := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(line); lw != width {
				t.Fatalf("width %d line %d = %d cells (want %d): %q", width, i, lw, width, line)
			}
		}
		if strings.ContainsAny(out, "\x01\x02") {
			t.Fatalf("width %d: raw snippet markers leaked into the rendered view", width)
		}
	}
}
