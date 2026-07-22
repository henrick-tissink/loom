package ui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	"github.com/henricktissink/loom/internal/transcript"
	"github.com/henricktissink/loom/internal/workflow"
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

	a := NewApp(Deps{Launcher: l, Repos: []registry.Repo{{Label: "p", Path: dir}}, Tmux: tm})
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

// The engine reports session names (spec §4); the label is joined back from
// the same snapshot's Live rows before the banner is raised.
func TestSnapMsgWithTransitionsEmitsNotify(t *testing.T) {
	a := fixtureApp()
	snap := status.Snapshot{
		NewlyNeedsYou: []string{"loom-1"},
		Live: []status.Row{{
			SessionRow: store.SessionRow{Name: "loom-1", ProjectLabel: "tavli", Cwd: "/w/tavli"},
			Title:      "fix race",
		}},
	}
	_, cmd := a.Update(snapMsg(snap))
	if cmd == nil {
		t.Fatal("expected a notify command for transitions")
	}
	if got := needsYouLabels(snap); len(got) != 1 || got[0] != "tavli · fix race" {
		t.Fatalf("needsYouLabels = %q", got)
	}
}

// A name that resolves to no Live row cannot come from a real poll; it must
// not raise a banner with an empty body.
func TestSnapMsgUnjoinableTransitionDoesNotNotify(t *testing.T) {
	a := fixtureApp()
	_, cmd := a.Update(snapMsg(status.Snapshot{NewlyNeedsYou: []string{"loom-gone"}}))
	if cmd != nil {
		t.Fatal("notified for a name with no Live row")
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
	a.openDetail(hit, viewSearch)

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
	a.openDetail(hit, viewSearch)

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
	a.openDetail(hit, viewSearch)

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
	a.openDetail(hit, viewSearch)

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
		a.openDetail(hit, viewSearch)
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
	a.openDetail(hit, viewSearch)

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
	a.openDetail(hit, viewSearch)

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
	a.openDetail(hit, viewSearch)

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
	a.openDetail(hitA, viewSearch) // as if sess-a's summarize were in flight
	a.openDetail(hitB, viewSearch) // user navigated to a different session's detail

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
	a.openDetail(hit, viewSearch)
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

// --- Task 3: workflows view (`w`) ---------------------------------------

// TestWOpensWorkflowsFromDashAndLoads: 'w' from the dashboard opens
// viewWorkflows and returns wfLoadCmd — with Deps{} (nil Runner, empty
// WorkflowsDir) that must resolve to an empty, error-free load (nil-safety
// contract), and the frame invariant holds.
func TestWOpensWorkflowsFromDashAndLoads(t *testing.T) {
	a := fixtureApp()
	_, cmd := a.Update(key("w"))
	if a.view != viewWorkflows {
		t.Fatalf("view = %v, want viewWorkflows", a.view)
	}
	if cmd == nil {
		t.Fatal("w did not return a load command")
	}
	msg := cmd()
	lm, ok := msg.(wfLoadedMsg)
	if !ok {
		t.Fatalf("cmd() = %T, want wfLoadedMsg", msg)
	}
	if lm.err != nil {
		t.Fatalf("load errored with Deps{} (no Runner): %v", lm.err)
	}
	a.Update(lm)
	if len(a.wfRuns) != 0 || len(a.wfDefs) != 0 {
		t.Fatalf("expected empty runs/defs with nil Runner and no WorkflowsDir, got %+v / %+v", a.wfRuns, a.wfDefs)
	}
	for i, line := range strings.Split(a.View(), "\n") {
		if lw := lipgloss.Width(line); lw != a.width {
			t.Fatalf("workflows view line %d width %d != %d", i, lw, a.width)
		}
	}
}

// --- cursor math across sections (incl. one/both empty) -----------------

func TestWFCursorClampsWhenBothSectionsEmpty(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	if _, ok := a.wfSelected(); ok {
		t.Fatal("wfSelected should be false with both sections empty")
	}
	a.Update(key("j"))
	a.Update(key("k"))
	if a.wfCursor != 0 {
		t.Fatalf("cursor = %d, want 0", a.wfCursor)
	}
	out := a.View()
	if !strings.Contains(out, "no active runs") || !strings.Contains(out, "no workflow definitions found") {
		t.Fatalf("empty-state text missing:\n%s", out)
	}
}

func TestWFCursorMovesAcrossRunsAndDefsSections(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	a.wfRuns = []wfRunRow{
		{run: store.RunRow{ID: 1, Name: "r1"}},
		{run: store.RunRow{ID: 2, Name: "r2"}},
	}
	a.wfDefs = []workflow.Definition{{Name: "d1", Steps: []workflow.Step{{Label: "a", Project: "/x"}}}}

	for i := 0; i < 5; i++ {
		a.Update(key("j"))
	}
	if a.wfCursor != 2 {
		t.Fatalf("cursor = %d, want clamped to 2 (last entry, 3 total)", a.wfCursor)
	}
	e, ok := a.wfSelected()
	if !ok || e.kind != wfEntryDef || e.def.Name != "d1" {
		t.Fatalf("selected = %+v, want the def row", e)
	}
	for i := 0; i < 5; i++ {
		a.Update(key("k"))
	}
	if a.wfCursor != 0 {
		t.Fatalf("cursor = %d, want floor 0", a.wfCursor)
	}
	e2, ok2 := a.wfSelected()
	if !ok2 || e2.kind != wfEntryRun || e2.run.run.Name != "r1" {
		t.Fatalf("selected = %+v, want r1", e2)
	}
}

func TestWFCursorOneSectionEmpty(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	a.wfDefs = []workflow.Definition{{Name: "d1", Steps: []workflow.Step{{Label: "a", Project: "/x"}}}}
	if e, ok := a.wfSelected(); !ok || e.kind != wfEntryDef {
		t.Fatalf("selected = %+v, want the only def (runs empty)", e)
	}

	b := NewApp(Deps{})
	b.width, b.height = 80, 24
	b.view = viewWorkflows
	b.wfRuns = []wfRunRow{{run: store.RunRow{ID: 1, Name: "r1"}}}
	if e, ok := b.wfSelected(); !ok || e.kind != wfEntryRun {
		t.Fatalf("selected = %+v, want the only run (defs empty)", e)
	}
}

// --- rendering: runs, defs, load errors, markers -------------------------

func TestWFViewRendersRunsDefsAndLoadErrors(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	a.wfRuns = []wfRunRow{{
		run:        store.RunRow{ID: 3, Name: "plan-execute-review"},
		stepLabel:  "step 2/3 execute",
		resolvedOK: true,
		resolved:   store.SessionRow{LastStatus: "running"},
	}}
	a.wfDefs = []workflow.Definition{{Name: "plan-execute-review", Steps: []workflow.Step{
		{Label: "plan", Project: "/w/parallax"}, {Label: "execute"}, {Label: "review"},
	}}}
	a.wfLoadErrs = []workflow.LoadError{{Path: "/w/.loom/workflows/bad.json", Err: "invalid JSON"}}

	out := a.View()
	for _, want := range []string{
		"plan-execute-review#3", "step 2/3 execute", "running",
		"3 steps", "parallax", "bad.json", "invalid JSON",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestWFRunRowShowsPendingSeedAndSeedFailedMarkers(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	a.wfRuns = []wfRunRow{
		{run: store.RunRow{ID: 1, Name: "r1", PendingSeed: "hello"}, resolvedOK: true, resolved: store.SessionRow{LastStatus: "idle"}},
		{run: store.RunRow{ID: 2, Name: "r2"}, resolvedOK: true, resolved: store.SessionRow{LastStatus: "idle", SeedStatus: "failed"}},
	}
	out := a.View()
	if !strings.Contains(out, "seed pending") {
		t.Fatalf("missing seed pending marker:\n%s", out)
	}
	if !strings.Contains(out, "seed FAILED") {
		t.Fatalf("missing seed FAILED marker:\n%s", out)
	}
}

// TestWFCorruptRunRendersDimRedAndAbandonable guards spec §2.12: a run
// whose def_json failed to parse renders (dim-red, checked via the
// corrupt-run message) but is still abandonable via 'x'.
func TestWFCorruptRunRendersDimRedAndAbandonable(t *testing.T) {
	a := NewApp(Deps{Runner: &workflow.Runner{}})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	a.wfRuns = []wfRunRow{{run: store.RunRow{ID: 9, Name: "corrupt"}, defErr: true}}
	out := a.View()
	if !strings.Contains(out, "corrupt run definition") {
		t.Fatalf("view missing corrupt-run message:\n%s", out)
	}
	a.wfCursor = 0
	a.Update(key("x"))
	if a.view != viewWFConfirmAbandon {
		t.Fatalf("x on corrupt run: view = %v, want viewWFConfirmAbandon", a.view)
	}
}

// --- dead-attach hint / live attach ---------------------------------------

// TestWFAttachHintOnDeadRun: ↵ on a run whose current step did not resolve
// live shows the dead-attach hint (spec §2.8) instead of a raw tmux error,
// and returns no command.
func TestWFAttachHintOnDeadRun(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	a.wfRuns = []wfRunRow{{run: store.RunRow{ID: 1, Name: "wf1"}, resolvedOK: false}}
	a.wfCursor = 0
	_, cmd := a.Update(key("enter"))
	if cmd != nil {
		t.Fatal("attach on a dead run returned a command, want nil")
	}
	if !strings.Contains(a.wfHint, "step session ended") {
		t.Fatalf("wfHint = %q, want the dead-attach hint", a.wfHint)
	}
	if !strings.Contains(a.View(), "step session ended") {
		t.Fatal("hint not rendered in the workflows view")
	}
}

// TestWFAttachOnLiveRunReturnsCommand: ↵ on a resolved-live run attaches
// (returns a non-nil tea.ExecProcess command) — the live counterpart to the
// dead-attach hint test above.
func TestWFAttachOnLiveRunReturnsCommand(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomwfui%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := tm.NewSession("loom-live", dir, "sleep 30", 80, 24); err != nil {
		t.Fatal(err)
	}

	a := NewApp(Deps{Tmux: tm})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	a.wfRuns = []wfRunRow{{
		run: store.RunRow{ID: 1, Name: "wf1"}, resolvedOK: true, live: true,
		resolved: store.SessionRow{Name: "loom-live"},
	}}
	a.wfCursor = 0
	_, cmd := a.Update(key("enter"))
	if cmd == nil {
		t.Fatal("attach on a live-resolved run did not return a command")
	}
}

// --- confirm previews (spec §2.11) ---------------------------------------

func TestWFConfirmShowsForkSubstitutedSnippetAndStepNumber(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 100, 24
	a.view = viewWFConfirm
	a.wfTarget = store.RunRow{Name: "wf1", StepIdx: 0}
	a.wfPreview = workflow.StepPreview{Label: "execute", Relation: "fork",
		Seed: "Execute the plan just written. Prior step concluded: build X."}
	out := a.View()
	for _, want := range []string{"fork", "step 2", "Execute the plan"} {
		if !strings.Contains(out, want) {
			t.Fatalf("confirm missing %q:\n%s", want, out)
		}
	}
}

func TestWFConfirmContinueWording(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 100, 24
	a.view = viewWFConfirm
	a.wfTarget = store.RunRow{Name: "wf1", StepIdx: 1}
	a.wfPreview = workflow.StepPreview{Label: "review", Relation: "continue", Seed: "go"}
	out := a.View()
	if !strings.Contains(out, "sends into current session") {
		t.Fatalf("confirm missing continue wording:\n%s", out)
	}
}

func TestWFConfirmFinishVariant(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 100, 24
	a.view = viewWFConfirm
	a.wfTarget = store.RunRow{Name: "wf1"}
	a.wfPreview = workflow.StepPreview{Finish: true}
	out := a.View()
	if !strings.Contains(out, "finish run wf1") {
		t.Fatalf("confirm missing finish variant:\n%s", out)
	}
}

func TestWFConfirmUnavailableWarning(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 100, 24
	a.view = viewWFConfirm
	a.wfTarget = store.RunRow{Name: "wf1"}
	a.wfPreview = workflow.StepPreview{Label: "b", Relation: "fresh", Seed: "go", Unavailable: true}
	out := a.View()
	if !strings.Contains(out, "unavailable") {
		t.Fatalf("confirm missing unavailable warning:\n%s", out)
	}
}

func TestWFConfirmLoadingAndPreviewErrorStates(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 100, 24
	a.view = viewWFConfirm
	a.wfTarget = store.RunRow{Name: "wf1"}
	a.wfPreviewLoading = true
	if !strings.Contains(a.View(), "computing preview") {
		t.Fatal("missing loading state")
	}
	a.wfPreviewLoading = false
	a.wfPreviewErr = "workflow: corrupt run definition snapshot: bad json"
	out := a.View()
	if !strings.Contains(out, "corrupt run definition snapshot") {
		t.Fatalf("missing preview error:\n%s", out)
	}
	if strings.Contains(out, "y confirm") {
		t.Fatal("y confirm should not be offered when the preview errored")
	}
}

func TestWFConfirmDeadOffersFork(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 100, 24
	a.view = viewWFConfirm
	a.wfTarget = store.RunRow{Name: "wf1"}
	a.wfContinueDead = true
	out := a.View()
	if !strings.Contains(out, "f fork from transcript instead") {
		t.Fatalf("missing dead-continue fork offer:\n%s", out)
	}
}

// --- errStr in body / frame invariants ------------------------------------

func TestWFErrStrRenderedInBody(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	a.errStr = "workflow: run advanced elsewhere"
	if !strings.Contains(a.View(), "workflow: run advanced elsewhere") {
		t.Fatal("errStr not rendered in the workflows body")
	}
}

func TestWFViewFrameInvariantEmptyAndPopulated(t *testing.T) {
	a := NewApp(Deps{})
	a.view = viewWorkflows
	assertWidth := func(t *testing.T, label string, width int) {
		t.Helper()
		for i, line := range strings.Split(a.View(), "\n") {
			if lw := lipgloss.Width(line); lw != width {
				t.Fatalf("%s width %d line %d = %d cells: %q", label, width, i, lw, line)
			}
		}
	}
	for _, width := range []int{100, 40} {
		a.width, a.height = width, 24
		assertWidth(t, "empty", width)
	}

	a.wfRuns = []wfRunRow{{
		run:        store.RunRow{ID: 1, Name: "a-very-long-workflow-run-name-indeed", PendingSeed: "x"},
		stepLabel:  "step 2/3 a rather long step label describing the step in detail",
		resolvedOK: true,
		resolved:   store.SessionRow{LastStatus: "needs_you", SeedStatus: "failed"},
	}}
	a.wfDefs = []workflow.Definition{{Name: "another-rather-long-definition-name", Steps: []workflow.Step{
		{Label: "a", Project: "/very/long/path/to/some/project/dir"}, {Label: "b"}, {Label: "c"}, {Label: "d"},
	}}}
	a.wfLoadErrs = []workflow.LoadError{
		{Path: "/w/.loom/workflows/a-really-long-bad-file-name.json", Err: "a long parse error message describing exactly what went wrong here"},
	}
	for _, width := range []int{100, 40} {
		a.width, a.height = width, 24
		assertWidth(t, "populated", width)
	}
}

func TestWFConfirmAndAbandonFrameInvariant(t *testing.T) {
	a := NewApp(Deps{})
	a.wfTarget = store.RunRow{ID: 5, Name: "plan-execute-review"}
	a.wfPreview = workflow.StepPreview{Label: "execute", Relation: "fork", Seed: strings.Repeat("x", 200), Unavailable: true}
	for _, width := range []int{100, 40} {
		a.width, a.height = width, 24
		a.view = viewWFConfirm
		for i, line := range strings.Split(a.View(), "\n") {
			if lw := lipgloss.Width(line); lw != width {
				t.Fatalf("confirm width %d line %d = %d cells: %q", width, i, lw, line)
			}
		}
		a.view = viewWFConfirmAbandon
		for i, line := range strings.Split(a.View(), "\n") {
			if lw := lipgloss.Width(line); lw != width {
				t.Fatalf("abandon width %d line %d = %d cells: %q", width, i, lw, line)
			}
		}
	}
}

// --- in-flight guards (spec §2.6) -----------------------------------------

// TestWFAdvanceDoublePressGuardedInFlight: a second 'y' press before the
// first advance's result has returned is a no-op (nil command), guarding
// against a double launch/CAS from a double press.
func TestWFAdvanceDoublePressGuardedInFlight(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	runID, err := st.InsertRun("wf1", `{"name":"wf1","steps":[{"label":"a","project":"/x","relation":"fresh"},{"label":"b","relation":"fresh","seed":"go"}]}`, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceRunCAS(runID, 0, 0, []string{"loom-x"}, "", 100); err != nil {
		t.Fatal(err)
	}
	run, _, err := st.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}

	a := NewApp(Deps{Runner: &workflow.Runner{Store: st}})
	a.width, a.height = 80, 24
	a.view = viewWFConfirm
	a.wfTarget = run
	a.wfPreview = workflow.StepPreview{Label: "b", Relation: "fresh", Seed: "go"}

	_, cmd1 := a.Update(key("y"))
	if cmd1 == nil {
		t.Fatal("first y press did not return a command")
	}
	_, cmd2 := a.Update(key("y"))
	if cmd2 != nil {
		t.Fatal("second y press before the first resolved should be guarded (nil command)")
	}
}

// TestWFStaleAdvanceResultDoesNotMutateADifferentRunsOpenConfirm is the
// regression test for the case-wfActionMsg staleness bug: run A's confirm
// opens and its advance fires ('y'), but the result is still in flight when
// the user cancels (esc) and opens run B's confirm instead. A's delayed
// result (ErrContinueDead) then arrives. It must NOT mutate B's now-open
// confirm — not wfContinueDead, not view, not wfTarget — only clear A's own
// in-flight guard and surface an A-qualified errStr. A subsequent 'f' press
// must be a no-op (B's wfContinueDead was never armed), never a forced fork
// fired against B.
func TestWFStaleAdvanceResultDoesNotMutateADifferentRunsOpenConfirm(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	runAID, err := st.InsertRun("wfA", `{"name":"wfA","steps":[{"label":"a","project":"/x","relation":"fresh"},{"label":"b","relation":"continue","seed":"go"}]}`, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceRunCAS(runAID, 0, 0, []string{"loom-a"}, "", 100); err != nil {
		t.Fatal(err)
	}
	runA, _, err := st.GetRun(runAID)
	if err != nil {
		t.Fatal(err)
	}
	runBID, err := st.InsertRun("wfB", `{"name":"wfB","steps":[{"label":"a","project":"/x","relation":"fresh"},{"label":"b","relation":"fresh","seed":"go"}]}`, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceRunCAS(runBID, 0, 0, []string{"loom-b"}, "", 100); err != nil {
		t.Fatal(err)
	}
	runB, _, err := st.GetRun(runBID)
	if err != nil {
		t.Fatal(err)
	}

	a := NewApp(Deps{Runner: &workflow.Runner{Store: st}})
	a.width, a.height = 80, 24

	// A's confirm opens, 'y' fires A's advance (still in flight — the result
	// hasn't arrived yet).
	a.view = viewWFConfirm
	a.wfTarget = runA
	a.wfPreview = workflow.StepPreview{Label: "b", Relation: "continue", Seed: "go"}
	_, cmd := a.Update(key("y"))
	if cmd == nil {
		t.Fatal("y press on A did not return a command")
	}
	if !a.wfInFlight[runA.ID] {
		t.Fatal("A's advance should be marked in-flight")
	}

	// User cancels A's confirm before the result arrives.
	a.Update(key("esc"))
	if a.view != viewWorkflows {
		t.Fatalf("view after esc = %v, want viewWorkflows", a.view)
	}
	if !a.wfInFlight[runA.ID] {
		t.Fatal("esc must not release A's in-flight guard — nothing has resolved yet")
	}

	// User opens B's confirm (wfPressN's own effects, applied directly).
	a.wfTarget = runB
	a.wfPreview = workflow.StepPreview{Label: "b", Relation: "fresh", Seed: "go"}
	a.wfPreviewLoading = false
	a.wfPreviewErr = ""
	a.wfContinueDead = false
	a.view = viewWFConfirm

	// A's delayed advance result finally arrives: ErrContinueDead for A.
	stale := wfActionMsg{kind: wfActionAdvance, runID: runA.ID, runName: runA.Name, err: workflow.ErrContinueDead}
	a.Update(stale)

	if a.wfInFlight[runA.ID] {
		t.Fatal("stale result must still clear its OWN in-flight guard")
	}
	if a.wfContinueDead {
		t.Fatal("stale result for A must not arm B's fork-demotion recovery")
	}
	if a.view != viewWFConfirm {
		t.Fatalf("view = %v, want still viewWFConfirm (B's confirm must stay open, untouched)", a.view)
	}
	if a.wfTarget.ID != runB.ID {
		t.Fatalf("wfTarget = %+v, want still B (untouched)", a.wfTarget)
	}
	if !strings.Contains(a.errStr, "wfA") {
		t.Fatalf("errStr = %q, want it to name the stale run (A), not silently drop the error", a.errStr)
	}

	// 'f' must be a no-op: wfContinueDead was never armed for B.
	_, fCmd := a.Update(key("f"))
	if fCmd != nil {
		t.Fatal("f after a stale A result must not fire a forced fork against B (wfContinueDead not armed)")
	}
	if a.wfTarget.ID != runB.ID {
		t.Fatalf("wfTarget after f = %+v, want still B", a.wfTarget)
	}
}

// TestWFStartDoublePressGuardedInFlight mirrors the guard above for the
// def-row start action (keyed by definition path, since a brand-new run has
// no id yet).
func TestWFStartDoublePressGuardedInFlight(t *testing.T) {
	a := NewApp(Deps{Runner: &workflow.Runner{}})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	a.wfDefs = []workflow.Definition{{Name: "d1", Path: "/w/.loom/workflows/d1.json",
		Steps: []workflow.Step{{Label: "a", Project: "/x", Relation: ""}}}}
	a.wfCursor = 0

	_, cmd1 := a.Update(key("enter"))
	if cmd1 == nil {
		t.Fatal("first enter did not return a command")
	}
	_, cmd2 := a.Update(key("enter"))
	if cmd2 != nil {
		t.Fatal("second enter before the first resolved should be guarded (nil command)")
	}
}

// --- abandon flow ----------------------------------------------------------

func TestWFAbandonFlowCallsAbandonAndReloads(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	runID, err := st.InsertRun("wf1", `{"name":"wf1","steps":[{"label":"a","project":"/x","relation":"fresh"}]}`, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceRunCAS(runID, 0, 0, []string{"loom-x"}, "", 100); err != nil {
		t.Fatal(err)
	}
	run, _, err := st.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}

	runner := &workflow.Runner{Store: st}
	a := NewApp(Deps{Runner: runner})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	a.wfRuns = []wfRunRow{{run: run}}
	a.wfCursor = 0

	a.Update(key("x"))
	if a.view != viewWFConfirmAbandon {
		t.Fatalf("view = %v, want viewWFConfirmAbandon", a.view)
	}
	if a.wfTarget.ID != runID {
		t.Fatalf("wfTarget not captured: %+v", a.wfTarget)
	}

	_, cmd := a.Update(key("y"))
	if cmd == nil {
		t.Fatal("y did not return a command")
	}
	msg := cmd()
	am, ok := msg.(wfActionMsg)
	if !ok || am.err != nil {
		t.Fatalf("abandon result = %#v", msg)
	}
	_, cmd2 := a.Update(am)
	if cmd2 == nil {
		t.Fatal("successful abandon did not trigger a reload command")
	}
	msg2 := cmd2()
	lm, ok := msg2.(wfLoadedMsg)
	if !ok {
		t.Fatalf("reload cmd() = %T, want wfLoadedMsg", msg2)
	}
	a.Update(lm)

	updated, ok, err := st.GetRun(runID)
	if err != nil || !ok {
		t.Fatalf("run vanished: ok=%v err=%v", ok, err)
	}
	if updated.Status != "abandoned" {
		t.Fatalf("run status = %q, want abandoned", updated.Status)
	}
	if len(a.wfRuns) != 0 {
		t.Fatalf("abandoned run still shown in RUNS after reload: %+v", a.wfRuns)
	}
}

// TestWFAbandonYStaysOnConfirmUntilResultAndFreshBranchHandlesError guards
// the debt-sweep dead-branch fix: previously 'y' switched a.view to
// viewWorkflows BEFORE abandonCmd's result landed, which made the
// wfActionMsg staleness gate's `m.kind == wfActionAbandon && a.view ==
// viewWFConfirmAbandon` branch permanently unreachable — every abandon
// result, success or failure, took the "stale" path instead (same end
// state on success, but a run-name-qualified error string on failure
// instead of the fresh path's plain one). This asserts both halves of the
// fix: (1) the confirm view is still open immediately after 'y' — the fix
// no longer flips it early — and (2) a failing abandon result is handled
// via the FRESH branch (plain err.Error(), no "run name#id:" prefix).
func TestWFAbandonYStaysOnConfirmUntilResultAndFreshBranchHandlesError(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	runID, err := st.InsertRun("wf1", `{"name":"wf1","steps":[{"label":"a","project":"/x","relation":"fresh"}]}`, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceRunCAS(runID, 0, 0, []string{"loom-x"}, "", 100); err != nil {
		t.Fatal(err)
	}
	run, _, err := st.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}

	runner := &workflow.Runner{Store: st}
	a := NewApp(Deps{Runner: runner})
	a.width, a.height = 80, 24
	a.view = viewWorkflows
	a.wfRuns = []wfRunRow{{run: run}}
	a.wfCursor = 0

	a.Update(key("x"))
	if a.view != viewWFConfirmAbandon {
		t.Fatalf("view = %v, want viewWFConfirmAbandon", a.view)
	}

	_, cmd := a.Update(key("y"))
	if cmd == nil {
		t.Fatal("y did not return a command")
	}
	if a.view != viewWFConfirmAbandon {
		t.Fatalf("view = %v immediately after 'y', want still viewWFConfirmAbandon until the result lands", a.view)
	}

	// Simulate a concurrent finish landing while the abandon is in flight:
	// AbandonRunCAS will now be rejected (status no longer 'running').
	if err := st.SetRunStatus(runID, "done", 150); err != nil {
		t.Fatal(err)
	}

	msg := cmd()
	am, ok := msg.(wfActionMsg)
	if !ok || am.err == nil {
		t.Fatalf("abandon result = %#v, want a non-nil error (status raced to done)", msg)
	}

	_, cmd2 := a.Update(am)
	if a.view != viewWorkflows {
		t.Fatalf("view = %v after failed abandon result, want viewWorkflows", a.view)
	}
	if a.errStr != am.err.Error() {
		t.Fatalf("errStr = %q, want plain %q (fresh branch, no run-name prefix)", a.errStr, am.err.Error())
	}
	if cmd2 == nil {
		t.Fatal("failed abandon did not trigger a reload command")
	}
}

// --- start flow (real backend, spec §2.10/§4) -----------------------------

// wfE2EDeps builds a real Runner (a throwaway tmux socket + a PATH-injected
// fake `claude` binary standing in for the real one, no seam to substitute
// it directly — same technique as internal/workflow's testRunner, Task 2)
// plus a workflows dir holding one valid single-step definition named "demo"
// whose step 1 project resolves against the registry Projects also
// returned here.
func wfE2EDeps(t *testing.T) Deps {
	t.Helper()
	binDir := t.TempDir()
	script := filepath.Join(binDir, "claude")
	content := "#!/bin/sh\necho \"\xe2\x9d\xaf\"\ncat >/dev/null\n" // bare ready marker, then sink stdin
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tm := &tmux.Client{Socket: fmt.Sprintf("loomwfe2e%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ccd := t.TempDir()
	l := &session.Launcher{
		Tmux: tm, Store: st, ClaudeConfigDir: ccd,
		ClaudeJSONPath: filepath.Join(t.TempDir(), ".claude.json"),
		ReadyMarker:    session.DefaultReadyMarker,
		TrustMarker:    session.DefaultTrustMarker,
		ReadyTimeout:   5 * time.Second,
		PollEvery:      50 * time.Millisecond,
	}
	runner := &workflow.Runner{Store: st, Launcher: l, ClaudeConfigDir: ccd}

	projDir := t.TempDir()
	wfDir := t.TempDir()
	defJSON := `{"name":"demo","steps":[{"label":"plan","project":"p","model":"","mode":"","seed":"start here","relation":""}]}`
	if err := os.WriteFile(filepath.Join(wfDir, "demo.json"), []byte(defJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	return Deps{Runner: runner, WorkflowsDir: wfDir,
		Repos: []registry.Repo{{Label: "p", Path: projDir}}, Tmux: tm}
}

// TestWFStartFlowRunAppearsAndStaysInView drives the full start flow
// end-to-end (spec §2.10/§4): open workflows, ↵ on the "demo" definition
// launches step 1 for real (fake claude via PATH), and the resulting run
// appears in RUNS on reload — the view never leaves viewWorkflows ("run
// appears, stay in view").
func TestWFStartFlowRunAppearsAndStaysInView(t *testing.T) {
	deps := wfE2EDeps(t)
	a := NewApp(deps)
	a.width, a.height = 80, 24

	_, loadCmd := a.Update(key("w"))
	lm, ok := loadCmd().(wfLoadedMsg)
	if !ok || lm.err != nil {
		t.Fatalf("initial load = %#v", lm)
	}
	a.Update(lm)
	if len(a.wfDefs) != 1 || a.wfDefs[0].Name != "demo" {
		t.Fatalf("wfDefs = %+v, want [demo]", a.wfDefs)
	}
	a.wfCursor = 0 // the only entry: the "demo" definition (no runs yet)

	_, startCmd := a.Update(key("enter"))
	if startCmd == nil {
		t.Fatal("enter on the definition did not return a start command")
	}
	sm, ok := startCmd().(wfStartMsg)
	if !ok {
		t.Fatalf("start cmd() = %T, want wfStartMsg", sm)
	}
	if sm.err != nil {
		t.Fatalf("Start failed: %v", sm.err)
	}

	_, reloadCmd := a.Update(sm)
	if a.view != viewWorkflows {
		t.Fatalf("view = %v, want viewWorkflows (stay in view after start)", a.view)
	}
	if reloadCmd == nil {
		t.Fatal("successful start did not trigger a reload command")
	}
	lm2, ok := reloadCmd().(wfLoadedMsg)
	if !ok || lm2.err != nil {
		t.Fatalf("reload after start = %#v", lm2)
	}
	a.Update(lm2)

	if len(a.wfRuns) != 1 {
		t.Fatalf("wfRuns = %+v, want exactly 1 run to have appeared", a.wfRuns)
	}
	if a.wfRuns[0].run.Name != "demo" {
		t.Fatalf("run name = %q, want demo", a.wfRuns[0].run.Name)
	}
}

// --- Task 2: launcher RELATED panel (spec §3-§6) ------------------------

// recallLauncherFixture seeds a store with two projects' worth of sessions:
// projA has three recency-ordered sessions (s3 newest .. s1 oldest), one of
// which (s2) is ALSO independently reachable by a real recall query sharing
// ≥2 content terms ("card","monitoring") with its indexed doc; projB has one
// session, existing purely so project-field / staleness-key tests have a
// second project to switch to. Returns the App (ready to open the launcher)
// plus both registry.Repo values.
func recallLauncherFixture(t *testing.T) (a *App, projA, projB registry.Repo) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	dirA := filepath.Join(t.TempDir(), "alpha")
	dirB := filepath.Join(t.TempDir(), "beta")
	projA = registry.Repo{Label: "alpha", Path: dirA}
	projB = registry.Repo{Label: "beta", Path: dirB}
	pdA := transcript.ProjectDirName(dirA)
	pdB := transcript.ProjectDirName(dirB)

	seed := func(id, pd, cwd, title, ask, outcome, doc string, lastTS int64) {
		t.Helper()
		if err := st.UpsertTranscript(store.Transcript{
			SessionID: id, ProjectDir: pd, Cwd: cwd, Title: title, Ask: ask,
			Outcome: outcome, FirstTS: lastTS, LastTS: lastTS,
		}); err != nil {
			t.Fatal(err)
		}
		if doc != "" {
			if err := st.ReplaceFileDocs(store.IndexedFile{Path: "/f-" + id, SessionID: id, Size: 1, Mtime: 1},
				[]store.Doc{{Content: doc, Role: "user", TS: lastTS}}); err != nil {
				t.Fatal(err)
			}
		}
	}

	seed("s1", pdA, dirA, "first session", "early ask", "did the first thing", "", 100)
	seed("s2", pdA, dirA, "card monitoring fix", "fix the card monitoring alerts",
		"fixed the card monitoring thresholds", "card monitoring alert thresholds bug", 200)
	seed("s3", pdA, dirA, "third session", "later ask", "did the third thing", "", 300)
	seed("s4", pdB, dirB, "beta session", "beta ask", "beta outcome", "", 100)

	l := &session.Launcher{Store: st}
	a = NewApp(Deps{Store: st, Repos: []registry.Repo{projA, projB}, Launcher: l})
	a.width, a.height = 100, 30
	return a, projA, projB
}

// openLauncherAndDrain opens the launcher and, if a panel query cmd was
// returned, runs it and applies the result inline — the same "invoke the
// cmd manually" pattern existing tests use for tea.Cmds.
func openLauncherAndDrain(a *App) {
	_, cmd := a.Update(key("n"))
	if cmd != nil {
		a.Update(cmd())
	}
}

// --- §3 focus model: every transition, verbatim --------------------------

func TestLauncherFocusTabCyclesFormOnlyWrapping(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	if a.form.focus != 0 || a.panelFocused {
		t.Fatalf("initial focus = form:%d panel:%v, want form:0 panel:false", a.form.focus, a.panelFocused)
	}
	for i, want := range []int{1, 2, 3, 0} {
		a.Update(tea.KeyMsg{Type: tea.KeyTab})
		if a.form.focus != want || a.panelFocused {
			t.Fatalf("tab %d: focus = form:%d panel:%v, want form:%d panel:false", i, a.form.focus, a.panelFocused, want)
		}
	}
	for i, want := range []int{3, 2, 1, 0} {
		a.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
		if a.form.focus != want || a.panelFocused {
			t.Fatalf("shift-tab %d: focus = form:%d panel:%v, want form:%d panel:false", i, a.form.focus, a.panelFocused, want)
		}
	}
}

// TestLauncherTabFromPanelReturnsToForm: tab "never enters the panel" (spec
// §3) — pressed while panel-focused, it clears panelFocused and cycles the
// form field forward from wherever form.focus sits (seed(3), since that's
// how the panel is entered).
func TestLauncherTabFromPanelReturnsToForm(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	a.form.setFocus(3)
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // seed(3) -> panel[0]
	if !a.panelFocused {
		t.Fatal("down from seed did not enter the panel")
	}
	a.Update(tea.KeyMsg{Type: tea.KeyTab})
	if a.panelFocused {
		t.Fatal("tab did not clear panelFocused")
	}
	if a.form.focus != 0 { // cycle(3,1,4) wraps to 0
		t.Fatalf("form.focus after tab-from-panel = %d, want 0", a.form.focus)
	}
}

func TestLauncherDownUpFormOnlyNoWrap(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	// Empty the panel so down-from-seed can't leave the form — isolates
	// this test to the FORM-only ↓/↑ transitions.
	a.panelHits = nil
	a.includes = nil

	for i, want := range []int{1, 2, 3} {
		a.Update(tea.KeyMsg{Type: tea.KeyDown})
		if a.form.focus != want {
			t.Fatalf("down %d: focus = %d, want %d", i, a.form.focus, want)
		}
	}
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // seed(3), empty panel: no-op
	if a.form.focus != 3 || a.panelFocused {
		t.Fatalf("down at seed with empty panel = form:%d panel:%v, want form:3 panel:false", a.form.focus, a.panelFocused)
	}
	for i, want := range []int{2, 1, 0} {
		a.Update(tea.KeyMsg{Type: tea.KeyUp})
		if a.form.focus != want {
			t.Fatalf("up %d: focus = %d, want %d", i, a.form.focus, want)
		}
	}
	a.Update(tea.KeyMsg{Type: tea.KeyUp}) // project(0): no-op, no wrap
	if a.form.focus != 0 {
		t.Fatalf("up at project(0) = %d, want 0 (no wrap)", a.form.focus)
	}
}

func TestLauncherDownFromSeedEntersPanelAndUpLeavesIt(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	n := a.panelLen()
	if n == 0 {
		t.Fatal("fixture produced an empty panel; test needs rows")
	}
	a.form.setFocus(3)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	if !a.panelFocused || a.panelCursor != 0 {
		t.Fatalf("down from seed = panelFocused:%v cursor:%d, want true/0", a.panelFocused, a.panelCursor)
	}
	for i := 1; i < n; i++ {
		a.Update(tea.KeyMsg{Type: tea.KeyDown})
		if a.panelCursor != i {
			t.Fatalf("panel down %d: cursor = %d, want %d", i, a.panelCursor, i)
		}
	}
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // at bottom: no-op, no wrap
	if a.panelCursor != n-1 {
		t.Fatalf("panel down at bottom = %d, want no-op at %d", a.panelCursor, n-1)
	}
	for i := n - 2; i >= 0; i-- {
		a.Update(tea.KeyMsg{Type: tea.KeyUp})
		if a.panelCursor != i {
			t.Fatalf("panel up: cursor = %d, want %d", a.panelCursor, i)
		}
	}
	a.Update(tea.KeyMsg{Type: tea.KeyUp}) // panel[0] -> seed(3)
	if a.panelFocused || a.form.focus != 3 {
		t.Fatalf("up from panel[0] = panelFocused:%v form.focus:%d, want false/3", a.panelFocused, a.form.focus)
	}
}

func TestLauncherEnterFormFocusedLaunches(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.view != viewDash {
		t.Fatalf("view after form-focused enter = %v, want viewDash", a.view)
	}
	if cmd == nil {
		t.Fatal("form-focused enter did not return a launch command")
	}
}

func TestLauncherEnterPanelFocusedOpensDetail(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	a.form.setFocus(3)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.view != viewDetail {
		t.Fatalf("view after panel-focused enter = %v, want viewDetail", a.view)
	}
	if a.detailReturn != viewLauncher {
		t.Fatalf("detailReturn = %v, want viewLauncher", a.detailReturn)
	}
}

func TestLauncherSpacePanelFocusedTogglesInclude(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	a.form.setFocus(3)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	row, ok := a.panelSelected()
	if !ok {
		t.Fatal("no panel row selected")
	}
	a.Update(key(" "))
	if _, on := a.includes[row.t.SessionID]; !on {
		t.Fatalf("space did not include %s", row.t.SessionID)
	}
	a.Update(key(" ")) // re-toggle off
	if _, on := a.includes[row.t.SessionID]; on {
		t.Fatal("second space did not un-include")
	}
}

// TestLauncherSpaceInSeedTypesASpace is the spec §3 binding test: space
// while the SEED field (not the panel) is focused inserts a literal space
// into the textinput and toggles NOTHING.
func TestLauncherSpaceInSeedTypesASpace(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	a.form.setFocus(3)
	a.form.seed.SetValue("hello")
	a.form.seed.CursorEnd()
	a.Update(key(" "))
	a.Update(key("world"))
	if got := a.form.seed.Value(); got != "hello world" {
		t.Fatalf("seed value = %q, want %q", got, "hello world")
	}
	if len(a.includes) != 0 {
		t.Fatalf("space-in-seed toggled an include: %+v", a.includes)
	}
}

func TestLauncherEscReturnsToDashFromAnyFocus(t *testing.T) {
	for _, enterPanel := range []bool{false, true} {
		a, _, _ := recallLauncherFixture(t)
		openLauncherAndDrain(a)
		if enterPanel {
			a.form.setFocus(3)
			a.Update(tea.KeyMsg{Type: tea.KeyDown})
			if !a.panelFocused {
				t.Fatal("fixture setup: expected panel focus")
			}
		}
		a.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if a.view != viewDash {
			t.Fatalf("esc (panelFocused=%v): view = %v, want viewDash", enterPanel, a.view)
		}
	}
}

// --- §4 includes: pinning, SessionID keying, project-change clear, max-3 -

func TestLauncherIncludesPinnedAcrossRerank(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	a.form.setFocus(3)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	row0, ok := a.panelSelected()
	if !ok {
		t.Fatal("no panel row")
	}
	a.Update(key(" ")) // include it

	// A fresh query result that no longer contains this session at all.
	a.panelHits = []memory.RelatedHit{{T: store.Transcript{SessionID: "unrelated", Title: "unrelated"}}}
	a.clampPanelCursor()

	rows := a.panelRows()
	if len(rows) == 0 || rows[0].t.SessionID != row0.t.SessionID || !rows[0].included {
		t.Fatalf("included row not pinned at top after rerank: %+v", rows)
	}
	a.panelCursor = 0
	a.Update(key(" ")) // re-toggle off
	if _, on := a.includes[row0.t.SessionID]; on {
		t.Fatal("re-toggle did not un-include the pinned row")
	}
}

func TestLauncherIncludeKeyedBySessionIDNotPosition(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	rows := a.panelRows()
	if len(rows) < 2 {
		t.Fatal("fixture needs >=2 panel rows")
	}
	target := rows[1].t.SessionID

	a.form.setFocus(3)
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // panel[0]
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // panel[1]
	a.Update(key(" "))                      // include rows[1]

	// Reorder panelHits (simulating a re-rank shuffle) — the include must
	// still be keyed to `target`'s SessionID, never "whatever is now at
	// index 1".
	reordered := make([]memory.RelatedHit, len(a.panelHits))
	for i, h := range a.panelHits {
		reordered[len(a.panelHits)-1-i] = h
	}
	a.panelHits = reordered
	a.clampPanelCursor()

	if _, on := a.includes[target]; !on {
		t.Fatalf("include lost after reorder: includes=%+v, want %s present", a.includes, target)
	}
}

func TestLauncherProjectChangeClearsIncludes(t *testing.T) {
	a, _, projB := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	a.form.setFocus(3)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(key(" ")) // include something from projA
	if len(a.includes) == 0 {
		t.Fatal("fixture setup: expected an include")
	}
	a.Update(tea.KeyMsg{Type: tea.KeyTab}) // back to form; cycle(3,1,4) wraps to project(0)
	if a.panelFocused || a.form.focus != 0 {
		t.Fatalf("test setup: focus = form:%d panel:%v, want form:0 panel:false", a.form.focus, a.panelFocused)
	}

	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRight}) // projA -> projB
	if len(a.includes) != 0 {
		t.Fatalf("includes not cleared on project change: %+v", a.includes)
	}
	if cmd == nil {
		t.Fatal("project change did not refire the panel query")
	}
	msg, ok := cmd().(panelResultsMsg)
	if !ok {
		t.Fatalf("project-change cmd() = %T, want panelResultsMsg", msg)
	}
	if want := transcript.ProjectDirName(projB.Path); msg.projectDir != want {
		t.Fatalf("refired query projectDir = %q, want projB's %q", msg.projectDir, want)
	}
}

func TestLauncherIncludeMaxThree(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	// Pad to >=4 distinct rows (the fixture's own same-project data has 3).
	a.panelHits = append(a.panelHits, memory.RelatedHit{T: store.Transcript{SessionID: "extra", Title: "extra"}})

	a.form.setFocus(3)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	for i := 0; i < 4; i++ {
		a.Update(key(" "))
		if i < 3 {
			a.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
	}
	if len(a.includes) != includeCap {
		t.Fatalf("includes = %d, want cap %d: %+v", len(a.includes), includeCap, a.includes)
	}
}

// --- §4 slash-seed warning -------------------------------------------------

func TestLauncherSlashSeedWarningBlocksDroppedAtLaunch(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	a.form.setFocus(3)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(key(" "))                    // include one entry
	a.Update(tea.KeyMsg{Type: tea.KeyUp}) // back to seed(3)
	a.Update(key("/deploy"))
	if a.form.seed.Value() != "/deploy" {
		t.Fatalf("seed = %q, want /deploy", a.form.seed.Value())
	}
	if a.panelWarn == "" {
		t.Fatal("slash seed with includes did not warn")
	}
	if out := a.View(); !strings.Contains(out, "⚠") {
		t.Fatalf("warning not rendered:\n%s", out)
	}

	seed, warned := buildSeedWithRecall(a.form.seed.Value(), a.includeSnapshot(), a.deps.Repos)
	if !warned || seed != "/deploy" {
		t.Fatalf("buildSeedWithRecall(%q, includes) = (%q,%v), want (\"/deploy\",true) — blocks dropped",
			a.form.seed.Value(), seed, warned)
	}
}

func TestLauncherWarningClearsWhenIncludesEmptied(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	a.form.setFocus(3)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(key(" "))
	a.Update(tea.KeyMsg{Type: tea.KeyUp})
	a.Update(key("/deploy"))
	if a.panelWarn == "" {
		t.Fatal("expected warning")
	}
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(key(" ")) // un-include
	if a.panelWarn != "" {
		t.Fatalf("warning not cleared after un-including: %q", a.panelWarn)
	}
}

// --- §4 seed assembly (buildSeedWithRecall): caps, marker format, safety -

func TestBuildSeedWithRecallMarkerFormatAndOrder(t *testing.T) {
	includes := []store.Transcript{
		{SessionID: "s1", ProjectDir: "-Users-h-Sauce-alpha", Cwd: "/Users/h/Sauce/alpha", Title: "fix parser", Outcome: "fixed the tokenizer bug"},
		{SessionID: "s2", ProjectDir: "-Users-h-Sauce-beta", Cwd: "/Users/h/Sauce/beta", Ask: "add caching", Outcome: "added an LRU cache"},
	}
	seed, warned := buildSeedWithRecall("continue this", includes, nil)
	if warned {
		t.Fatal("warned=true for a plain seed")
	}
	if !strings.HasPrefix(seed, "continue this") {
		t.Fatalf("seed lost its original prefix: %q", seed)
	}
	want1 := memory.RecallMarker + "alpha·fix parser]: fixed the tokenizer bug"
	want2 := memory.RecallMarker + "beta·add caching]: added an LRU cache"
	i1 := strings.Index(seed, want1)
	i2 := strings.Index(seed, want2)
	if i1 < 0 || i2 < 0 {
		t.Fatalf("seed = %q, want both markers %q and %q present", seed, want1, want2)
	}
	if i1 > i2 {
		t.Fatalf("markers out of includes order: %q", seed)
	}
}

func TestBuildSeedWithRecallZeroIncludesUnchanged(t *testing.T) {
	seed, warned := buildSeedWithRecall("hello", nil, nil)
	if seed != "hello" || warned {
		t.Fatalf("got (%q,%v), want (\"hello\",false)", seed, warned)
	}
}

func TestBuildSeedWithRecallSlashSeedDropsBlocks(t *testing.T) {
	includes := []store.Transcript{{SessionID: "s1", Title: "t", Outcome: "o"}}
	seed, warned := buildSeedWithRecall("/run-thing", includes, nil)
	if seed != "/run-thing" || !warned {
		t.Fatalf("got (%q,%v), want (\"/run-thing\",true)", seed, warned)
	}
}

func TestBuildSeedWithRecallOutcomeTruncatedByteSafe(t *testing.T) {
	huge := strings.Repeat("x", outcomeCap*2)
	includes := []store.Transcript{{SessionID: "s1", Title: "t", Outcome: huge}}
	seed, _ := buildSeedWithRecall("seed", includes, nil)
	if !strings.Contains(seed, recallTruncMarker) {
		t.Fatal("expected truncation marker in the assembled seed")
	}
	if len(seed) > seedInvariantMax {
		t.Fatalf("seed len = %d, exceeds invariant %d", len(seed), seedInvariantMax)
	}
}

func TestBuildSeedWithRecallStripsCRLFForSingleLineOutput(t *testing.T) {
	includes := []store.Transcript{{SessionID: "s1", Title: "line1\nline2", Outcome: "out1\r\nout2"}}
	seed, _ := buildSeedWithRecall("seed", includes, nil)
	if strings.ContainsAny(seed, "\n\r") {
		t.Fatalf("seed contains a newline, breaking the tmux send-keys single-line invariant: %q", seed)
	}
}

func TestBuildSeedWithRecallDefensiveCapAtIncludeCap(t *testing.T) {
	var includes []store.Transcript
	for i := 0; i < 5; i++ {
		includes = append(includes, store.Transcript{SessionID: fmt.Sprintf("s%d", i), Title: fmt.Sprintf("t%d", i), Outcome: fmt.Sprintf("o%d", i)})
	}
	seed, _ := buildSeedWithRecall("seed", includes, nil)
	if n := strings.Count(seed, memory.RecallMarker); n != includeCap {
		t.Fatalf("marker count = %d, want defensive cap %d", n, includeCap)
	}
}

func TestBuildSeedWithRecallNeverPanicsOnPathologicalInput(t *testing.T) {
	huge := strings.Repeat("z", 1_000_000)
	includes := []store.Transcript{
		{SessionID: "s1", ProjectDir: "x", Cwd: huge, Title: huge, Ask: huge, Outcome: huge},
		{SessionID: "s2", ProjectDir: "x", Cwd: huge, Title: huge, Ask: huge, Outcome: huge},
		{SessionID: "s3", ProjectDir: "x", Cwd: huge, Title: huge, Ask: huge, Outcome: huge},
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("buildSeedWithRecall panicked on pathological (but cap-bounded) input: %v", r)
		}
	}()
	seed, _ := buildSeedWithRecall("seed", includes, nil)
	if len(seed) > seedInvariantMax {
		t.Fatalf("seed len = %d, exceeds invariant %d", len(seed), seedInvariantMax)
	}
}

// --- §6 M3: registry reverse-match else basename(cwd) ---------------------

func TestRelatedLabelRegistryReverseMatchElseBasenameCwd(t *testing.T) {
	proj := registry.Repo{Label: "alpha-custom-label", Path: "/Users/h/Sauce/alpha"}
	t1 := store.Transcript{ProjectDir: transcript.ProjectDirName(proj.Path), Cwd: ""} // cwd empty on purpose
	if got := relatedLabel([]registry.Repo{proj}, t1); got != "alpha-custom-label" {
		t.Fatalf("reverse-match label = %q, want alpha-custom-label", got)
	}
	t2 := store.Transcript{ProjectDir: "no-match", Cwd: "/Users/h/Sauce/other"}
	if got := relatedLabel([]registry.Repo{proj}, t2); got != "other" {
		t.Fatalf("fallback label = %q, want other (basename cwd)", got)
	}
}

// --- §5 detail round-trip: origin-tracked esc, r hidden -------------------

func TestLauncherDetailRoundTripPreservesAllStateAndHidesResume(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)

	a.form.repoIdx = 1
	a.form.modeIdx = 2
	a.form.setFocus(3)
	a.form.seed.SetValue("some seed text")
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // enter panel[0]
	a.Update(key(" "))                      // include row0
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // move to row1

	wantCursor := a.panelCursor
	wantPanelFocused := a.panelFocused
	wantIncludes := len(a.includes)
	wantHits := len(a.panelHits)
	wantSeed := a.form.seed.Value()
	wantModeIdx := a.form.modeIdx
	wantProjIdx := a.form.repoIdx

	a.Update(tea.KeyMsg{Type: tea.KeyEnter}) // panel-focused -> detail
	if a.view != viewDetail {
		t.Fatal("enter on panel row did not open detail")
	}
	if a.detailReturn != viewLauncher {
		t.Fatalf("detailReturn = %v, want viewLauncher", a.detailReturn)
	}

	// r must be hidden/no-op for a launcher-origin detail (spec §5), even
	// though the fixture's transcript has a real, resumable cwd.
	beforeView := a.view
	_, rcmd := a.Update(key("r"))
	if a.view != beforeView || rcmd != nil {
		t.Fatal("'r' was not hidden/no-op for a launcher-origin detail")
	}
	if out := a.View(); strings.Contains(out, "r resume") {
		t.Fatalf("keybar shows r resume for a launcher-origin detail:\n%s", out)
	}

	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewLauncher {
		t.Fatalf("esc from detail = %v, want viewLauncher", a.view)
	}
	if a.panelFocused != wantPanelFocused || a.panelCursor != wantCursor {
		t.Fatalf("panel focus/cursor lost: focused=%v cursor=%d, want %v/%d",
			a.panelFocused, a.panelCursor, wantPanelFocused, wantCursor)
	}
	if len(a.includes) != wantIncludes {
		t.Fatalf("includes lost: %d, want %d", len(a.includes), wantIncludes)
	}
	if len(a.panelHits) != wantHits {
		t.Fatalf("panelHits lost: %d, want %d", len(a.panelHits), wantHits)
	}
	if a.form.seed.Value() != wantSeed {
		t.Fatalf("seed lost: %q, want %q", a.form.seed.Value(), wantSeed)
	}
	if a.form.modeIdx != wantModeIdx || a.form.repoIdx != wantProjIdx {
		t.Fatalf("form fields lost: mode=%d proj=%d, want %d/%d", a.form.modeIdx, a.form.repoIdx, wantModeIdx, wantProjIdx)
	}
}

// --- §6 freshness: staleness key, debounce --------------------------------

// TestPanelStalenessKeyDiscardsStaleProjectResult is the spec §6 I6 binding
// test: the staleness key is (seed,projectDir), not seed alone — a stale
// result for a project the launcher has since switched away from must be
// discarded even though its seed still matches the live input.
func TestPanelStalenessKeyDiscardsStaleProjectResult(t *testing.T) {
	a, projA, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a) // loads projA's (index 0) recency panel
	freshA := append([]memory.RelatedHit(nil), a.panelHits...)
	if len(freshA) == 0 {
		t.Fatal("fixture: expected projA panel hits")
	}

	_, switchCmd := a.Update(tea.KeyMsg{Type: tea.KeyRight}) // projA -> projB
	if switchCmd == nil {
		t.Fatal("project switch did not refire the panel query")
	}
	a.Update(switchCmd())
	freshB := append([]memory.RelatedHit(nil), a.panelHits...)
	if len(freshB) == 0 {
		t.Fatal("fixture: expected projB panel hits")
	}

	stale := panelResultsMsg{
		seed:       "",
		projectDir: transcript.ProjectDirName(projA.Path),
		hits:       []memory.RelatedHit{{T: store.Transcript{SessionID: "should-not-apply"}}},
	}
	a.Update(stale)
	if !reflect.DeepEqual(a.panelHits, freshB) {
		t.Fatalf("stale projA result overwrote the live projB panel: got %+v, want %+v", a.panelHits, freshB)
	}
}

// TestPanelDebounceStaleSeqDiscarded: a panelDebounceMsg carrying an older
// generation than a.panelSeq (a newer keystroke has since bumped it) fires
// no query at all.
func TestPanelDebounceStaleSeqDiscarded(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	a.form.setFocus(3)

	_, cmd1 := a.Update(key("x"))
	seqAfterFirst := a.panelSeq
	if cmd1 == nil {
		t.Fatal("seed keystroke did not return a command")
	}
	batch1, ok := cmd1().(tea.BatchMsg)
	if !ok {
		t.Fatalf("seed keystroke cmd = %T, want tea.BatchMsg", cmd1())
	}
	var debounce1 panelDebounceMsg
	found := false
	for _, c := range batch1 {
		if c == nil {
			continue
		}
		if dm, ok := c().(panelDebounceMsg); ok {
			debounce1, found = dm, true
		}
	}
	if !found || debounce1.seq != seqAfterFirst {
		t.Fatalf("debounce1 = %+v found=%v, want seq %d", debounce1, found, seqAfterFirst)
	}

	a.Update(key("y")) // a second keystroke bumps panelSeq again
	if _, cmd := a.Update(debounce1); cmd != nil {
		t.Fatal("stale debounce generation fired a query instead of being discarded")
	}
}

// --- §6 M4: both recency-preview and real-snippet row shapes --------------

func TestLauncherPanelBothM4Shapes(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a) // empty seed -> recency fallback: no snippet
	rows := a.panelRows()
	if len(rows) == 0 {
		t.Fatal("expected recency-fallback rows")
	}
	for _, r := range rows {
		if r.snippet != "" {
			t.Fatalf("recency-fallback row has a non-empty snippet (M4 shape violated): %+v", r)
		}
	}

	q := "card monitoring alert thresholds"
	msg, ok := a.panelQueryCmd(q, a.currentProjectDir())().(panelResultsMsg)
	if !ok {
		t.Fatal("panelQueryCmd did not return panelResultsMsg")
	}
	a.form.seed.SetValue(q)
	a.Update(msg)

	found := false
	for _, r := range a.panelRows() {
		if r.t.SessionID == "s2" {
			found = true
			if r.snippet == "" {
				t.Fatalf("real recall hit s2 has an empty snippet (M4 shape violated): %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("expected s2 to qualify for the real recall query")
	}
}

// --- frame invariants, populated panel + pinned includes ------------------

// TestLauncherFrameInvariantPopulatedPanelAndIncludes: the frame invariant
// at both 100 and 40 cells wide (same "100/40" precedent as
// TestDetailFrameInvariantPopulatedContent), with the panel populated, an
// include pinned, and the slash-seed warning line all rendering at once.
func TestLauncherFrameInvariantPopulatedPanelAndIncludes(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	a.form.setFocus(3)
	a.form.seed.SetValue("a very long seed prompt for testing frame width invariants across the launcher panel")
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // enter panel
	a.Update(key(" "))                      // include row0 (pinned rendering path)
	if len(a.includes) == 0 {
		t.Fatal("fixture: expected an include for this test")
	}
	a.panelWarn = "slash-command seed — related context will NOT be appended"

	for _, width := range []int{100, 40} {
		a.width, a.height = width, 30
		out := a.View()
		for i, line := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(line); lw != width {
				t.Fatalf("width %d line %d = %d cells (want %d): %q", width, i, lw, width, line)
			}
		}
	}
}

func TestLauncherPanelRendersIncludeCheckboxesAndSectionHeader(t *testing.T) {
	a, _, _ := recallLauncherFixture(t)
	openLauncherAndDrain(a)
	a.form.setFocus(3)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(key(" "))
	out := a.View()
	if !strings.Contains(out, "RELATED") {
		t.Fatalf("RELATED section header missing:\n%s", out)
	}
	if !strings.Contains(out, "[x]") {
		t.Fatalf("included checkbox missing:\n%s", out)
	}
}

// --- zero-Deps (nil Store) safety -----------------------------------------

func TestLauncherZeroDepsNilStoreSafe(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 100, 30
	openLauncherAndDrain(a) // "n" -> panelQueryCmd returns nil (no store, no projects)
	if a.view != viewLauncher {
		t.Fatal("launcher did not open with zero Deps")
	}
	for _, msg := range []tea.Msg{
		tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyDown},
		tea.KeyMsg{Type: tea.KeyDown},
		tea.KeyMsg{Type: tea.KeyUp},
		key(" "),
		tea.KeyMsg{Type: tea.KeyEnter},
	} {
		a.Update(msg)
	}
	if a.view != viewDash {
		t.Fatalf("form-focused enter with zero Deps should launch-noop to dash, view = %v", a.view)
	}

	openLauncherAndDrain(a)
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewDash {
		t.Fatalf("esc with zero Deps = %v, want viewDash", a.view)
	}
}

func TestPanelQueryCmdNilWhenStoreOrProjectDirEmpty(t *testing.T) {
	a := NewApp(Deps{})
	if cmd := a.panelQueryCmd("seed", ""); cmd != nil {
		t.Fatal("panelQueryCmd with empty projectDir must be nil")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a2 := NewApp(Deps{Store: st})
	if cmd := a2.panelQueryCmd("seed", ""); cmd != nil {
		t.Fatal("panelQueryCmd with empty projectDir must be nil even with a Store")
	}
	if cmd := a2.panelQueryCmd("seed", "some-dir"); cmd == nil {
		t.Fatal("panelQueryCmd with a Store and projectDir must be non-nil")
	}
}

// --- Fan-out (`N`) — spec §2, plan Task 1 ----------------------------------

func fanoutRepos() []registry.Repo {
	return []registry.Repo{{Label: "alpha", Path: "/tmp/alpha"}, {Label: "beta", Path: "/tmp/beta"}}
}

func TestNKeyOpensFanoutAndEscCloses(t *testing.T) {
	a := fixtureApp()
	a.Update(key("N"))
	if a.view != viewFanout {
		t.Fatalf("view = %v, want viewFanout", a.view)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewDash {
		t.Fatalf("view = %v, want dash after esc", a.view)
	}
}

// TestFanoutFocusTabCyclesFourZonesWrapping guards spec §2.1: tab/shift-tab
// cycle the 4 focus zones (0 checklist .. 3 seed), wrapping both directions.
func TestFanoutFocusTabCyclesFourZonesWrapping(t *testing.T) {
	a := NewApp(Deps{Repos: fanoutRepos()})
	a.width, a.height = 100, 30
	a.Update(key("N"))
	if a.fanForm.focus != 0 {
		t.Fatalf("initial focus = %d, want 0 (checklist)", a.fanForm.focus)
	}
	for want := 1; want <= 3; want++ {
		a.Update(tea.KeyMsg{Type: tea.KeyTab})
		if a.fanForm.focus != want {
			t.Fatalf("after %d tabs focus = %d, want %d", want, a.fanForm.focus, want)
		}
	}
	a.Update(tea.KeyMsg{Type: tea.KeyTab}) // wraps 3 -> 0
	if a.fanForm.focus != 0 {
		t.Fatalf("tab from seed did not wrap to 0: focus = %d", a.fanForm.focus)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyShiftTab}) // wraps 0 -> 3
	if a.fanForm.focus != 3 {
		t.Fatalf("shift-tab from checklist did not wrap to 3: focus = %d", a.fanForm.focus)
	}
}

// TestFanoutChecklistDownUpScrollsAndSpaceToggles guards spec §2.1: when the
// checklist is focused, ↓/↑ move its own cursor (no wrap) and space toggles
// the hovered project.
func TestFanoutChecklistDownUpScrollsAndSpaceToggles(t *testing.T) {
	a := NewApp(Deps{Repos: fanoutRepos()})
	a.width, a.height = 100, 30
	a.Update(key("N"))

	a.Update(tea.KeyMsg{Type: tea.KeyUp}) // no-op: already at top, no wrap
	if a.fanForm.listCur != 0 {
		t.Fatalf("listCur = %d, want 0 (no wrap upward)", a.fanForm.listCur)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	if a.fanForm.listCur != 1 {
		t.Fatalf("listCur = %d, want 1 after one down", a.fanForm.listCur)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // no-op: already at bottom (2 projects), no wrap
	if a.fanForm.listCur != 1 {
		t.Fatalf("listCur = %d, want 1 (no wrap downward past last project)", a.fanForm.listCur)
	}
	a.Update(key(" ")) // toggles beta (listCur==1)
	if !a.fanForm.checked[1] {
		t.Fatal("space did not toggle the hovered (index 1) project")
	}
	if a.fanForm.checked[0] {
		t.Fatal("space toggled the wrong project")
	}
	a.Update(key(" ")) // toggling again un-checks it
	if a.fanForm.checked[1] {
		t.Fatal("second space did not un-toggle")
	}
}

// TestFanoutDownUpNoopOnModelModeFields guards spec §2.1: "on fields 1-3,
// ↓/↑ do nothing (tab is field-nav — one dialect per zone, stated)".
func TestFanoutDownUpNoopOnModelModeFields(t *testing.T) {
	a := NewApp(Deps{Repos: fanoutRepos()})
	a.width, a.height = 100, 30
	a.Update(key("N"))
	a.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> focus 1 (model)
	if a.fanForm.focus != 1 {
		t.Fatalf("focus = %d, want 1", a.fanForm.focus)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(tea.KeyMsg{Type: tea.KeyUp})
	if a.fanForm.focus != 1 || a.fanForm.modelIdx != 0 {
		t.Fatalf("down/up mutated model field: focus=%d modelIdx=%d", a.fanForm.focus, a.fanForm.modelIdx)
	}
}

// TestFanoutSpaceInSeedTypesASpace guards spec §2.1: "space on seed TYPES a
// space (launcher precedent, tested)".
func TestFanoutSpaceInSeedTypesASpace(t *testing.T) {
	a := NewApp(Deps{Repos: fanoutRepos()})
	a.width, a.height = 100, 30
	a.Update(key("N"))
	for i := 0; i < 3; i++ {
		a.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> focus 3 (seed)
	}
	if a.fanForm.focus != 3 {
		t.Fatalf("focus = %d, want 3 (seed)", a.fanForm.focus)
	}
	a.Update(key("a"))
	a.Update(key(" "))
	a.Update(key("b"))
	if got := a.fanForm.seed.Value(); got != "a b" {
		t.Fatalf("seed value = %q, want %q", got, "a b")
	}
}

// TestFanoutEnterEmptySelectionNoop guards spec §2.1: "↵ ... empty selection
// -> no-op with inline hint".
func TestFanoutEnterEmptySelectionNoop(t *testing.T) {
	a := NewApp(Deps{Repos: fanoutRepos(), Launcher: &session.Launcher{}})
	a.width, a.height = 100, 30
	a.Update(key("N"))
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("enter with empty selection must not return a command")
	}
	if a.view != viewFanout {
		t.Fatalf("view = %v, want to stay on viewFanout", a.view)
	}
	if a.fanForm.hint == "" {
		t.Fatal("empty-selection enter must set an inline form hint")
	}
}

// fanoutTestHarness spins up a throwaway tmux server + store + Launcher —
// same shape as session.testLauncher / TestLaunchAndResumeCmdsEmitPollNowNotTick
// above — for tests that need a REAL Launch+SetTags round trip ("throwaway
// tmux + fake claude", spec §5).
func fanoutTestHarness(t *testing.T) (*session.Launcher, *store.Store, *tmux.Client) {
	t.Helper()
	tm := &tmux.Client{Socket: fmt.Sprintf("loomfanout%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	l := &session.Launcher{Tmux: tm, Store: st, ReadyTimeout: 200 * time.Millisecond, PollEvery: 50 * time.Millisecond}
	return l, st, tm
}

// TestFanoutLaunchTagsEverySelectedProject is the binding groupID test (spec
// §2.2/§5): every launched session gets the SAME "fan:"+groupID tag via the
// two-step Launch-then-SetTags workflow.
func TestFanoutLaunchTagsEverySelectedProject(t *testing.T) {
	l, st, _ := fanoutTestHarness(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	projects := []registry.Repo{{Label: "alpha", Path: dirA}, {Label: "beta", Path: dirB}}

	a := NewApp(Deps{Launcher: l, Repos: projects})
	a.width, a.height = 80, 24
	a.Update(key("N"))
	a.Update(key(" "))                      // toggle alpha (listCur 0)
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // -> beta
	a.Update(key(" "))                      // toggle beta
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter with 2 selected projects must return a command")
	}
	if !a.fanInFlight {
		t.Fatal("fanInFlight must be set the moment the launch command is fired")
	}
	if a.view != viewFanout {
		t.Fatal("view must stay on viewFanout until fanResultMsg lands (spec §2.3 I2)")
	}

	msg := cmd()
	res, ok := msg.(fanResultMsg)
	if !ok {
		t.Fatalf("cmd() returned %T, want fanResultMsg", msg)
	}
	if len(res.results) != 2 {
		t.Fatalf("results = %d, want 2", len(res.results))
	}
	for _, r := range res.results {
		if r.Err != nil {
			t.Fatalf("project %s: unexpected launch error: %v", r.Project, r.Err)
		}
		if r.Untagged {
			t.Fatalf("project %s: unexpectedly untagged", r.Project)
		}
		row, ok, err := st.Get(r.Name)
		if err != nil || !ok {
			t.Fatalf("store row for %s missing: ok=%v err=%v", r.Name, ok, err)
		}
		if row.Tags != "fan:"+res.group {
			t.Fatalf("project %s Tags = %q, want fan:%s", r.Project, row.Tags, res.group)
		}
	}

	// Delivering the result msg: view transitions to dash, fanHint carries
	// the summary, in-flight guard clears.
	a.Update(res)
	if a.view != viewDash {
		t.Fatalf("view after fanResultMsg = %v, want viewDash", a.view)
	}
	if a.fanInFlight {
		t.Fatal("fanInFlight must clear once fanResultMsg lands")
	}
	if !strings.Contains(a.fanHint, "2/2 launched") {
		t.Fatalf("fanHint = %q, want it to report 2/2 launched", a.fanHint)
	}
	if !strings.Contains(a.fanHint, res.group) {
		t.Fatalf("fanHint = %q, want it to contain the group id %s", a.fanHint, res.group)
	}
}

// TestFanoutPartialFailureCountedFailedOthersSucceed guards spec §5: "partial
// failure (invalid cwd project -> counted failed, others succeed)".
//
// Disclosed deviation from the letter of the spec's test description: an
// "invalid cwd" is NOT a reproducible real Launch failure in this
// environment — verified empirically (see the shell probes run while
// building this test): tmux 3.7b's `new-session -c <dir>` exits 0 and
// silently falls back to another cwd for a missing directory, an
// unreadable directory, a non-directory path, and even a >4000-byte path;
// only a duplicate SESSION NAME makes tmux fail new-session, and session
// names are internal random UUIDs (session.NewSessionID) the ui package
// cannot predict to engineer a collision without modifying internal/session
// or internal/tmux (out of scope: zero non-ui package changes). So this
// exercises the REAL fanLaunchCmdWith sequential/counting/continuation
// logic — the two-step Launch-then-SetTags workflow, real Store writes for
// the succeeding project — with only the single Launch call for the "bad"
// project stubbed to return an error, via the fanLaunchFn seam documented
// on fanLaunchCmd. This is the same category of disclosed unit-level
// fallback the spec itself explicitly allows for the sibling untagged-
// accounting case.
func TestFanoutPartialFailureCountedFailedOthersSucceed(t *testing.T) {
	l, st, _ := fanoutTestHarness(t)
	goodDir := t.TempDir()
	projects := []registry.Repo{{Label: "good", Path: goodDir}, {Label: "bad", Path: t.TempDir()}}

	a := NewApp(Deps{Launcher: l, Repos: projects})
	a.width, a.height = 80, 24

	badCwdErr := errors.New("bad cwd")
	stub := func(r session.Recipe, w, h int, now time.Time) (string, error) {
		if r.ProjectLabel == "bad" {
			return "", badCwdErr
		}
		return l.Launch(r, w, h, now) // "good" goes through the REAL launcher
	}
	items := make([]fanLaunchItem, len(projects))
	for i, p := range projects {
		items[i] = fanLaunchItem{Repo: p, Recipe: a.fanForm.recipeFor(p)}
	}
	cmd := a.fanLaunchCmdWith(stub, items, a.width, a.height)
	if cmd == nil {
		t.Fatal("expected a launch command")
	}
	res := cmd().(fanResultMsg)
	if len(res.results) != 2 {
		t.Fatalf("results = %d, want 2", len(res.results))
	}
	var goodErr, badErr error
	var name string
	for _, r := range res.results {
		switch r.Project {
		case "good":
			goodErr = r.Err
			name = r.Name
		case "bad":
			badErr = r.Err
		}
	}
	if goodErr != nil {
		t.Fatalf("good project unexpectedly failed: %v", goodErr)
	}
	if badErr == nil {
		t.Fatal("bad project must be counted as failed, not silently succeed")
	}
	if !errors.Is(badErr, badCwdErr) {
		t.Fatalf("badErr = %v, want the stubbed error surfaced verbatim", badErr)
	}
	row, ok, err := st.Get(name)
	if err != nil || !ok || row.Tags != "fan:"+res.group {
		t.Fatalf("good project (the ONE that continued past the failure) was not tagged: row=%+v ok=%v err=%v", row, ok, err)
	}

	hint := formatFanHint(res.group, res.results)
	if !strings.Contains(hint, "1/2 launched") {
		t.Fatalf("hint = %q, want 1/2 launched", hint)
	}
	if !strings.Contains(hint, "failed: bad (bad cwd)") {
		t.Fatalf("hint = %q, want a failed:bad clause", hint)
	}
}

// TestFanoutUntaggedAccountingResultAssembly covers spec §5's "inject a
// SetTags failure if cheaply possible — else unit-test the result-assembly
// path, disclose": a live SetTags failure is not cheaply injectable against a
// real store (any name Launch just created will always accept a tag write),
// so this unit-tests formatFanHint's assembly of a launched-but-untagged
// result directly, per the spec's own disclosed fallback.
func TestFanoutUntaggedAccountingResultAssembly(t *testing.T) {
	results := []fanResult{
		{Project: "tavli", Name: "loom-1", Err: errors.New("bad cwd")},
		{Project: "volar", Name: "loom-2", Untagged: true},
		{Project: "parallax", Name: "loom-3"},
	}
	hint := formatFanHint("a1b2c3", results)
	want := "fan #a1b2c3: 2/3 launched · failed: tavli (bad cwd) · volar launched untagged"
	if hint != want {
		t.Fatalf("hint = %q, want %q", hint, want)
	}
}

// TestFanoutHintSurvivesSnapMsgClearedOnDashKeypress is the binding test for
// spec §2.3/C1: fanHint is a DEDICATED field that survives a snapMsg (which
// wipes errStr) and is cleared only by the next dashboard keypress.
func TestFanoutHintSurvivesSnapMsgClearedOnDashKeypress(t *testing.T) {
	a := fixtureApp()
	a.fanHint = "fan #abcdef: 1/1 launched"
	a.Update(snapMsg(a.snap)) // a poll landing must NOT clear fanHint
	if a.fanHint == "" {
		t.Fatal("fanHint was wiped by a snapMsg — must survive polls (spec §2.3 C1)")
	}
	a.Update(key("j")) // any dashboard keypress clears it
	if a.fanHint != "" {
		t.Fatalf("fanHint = %q, want cleared after a dashboard keypress", a.fanHint)
	}
}

// TestFanoutDoubleEnterInFlightGuardNoops guards spec §2.3 I2: a second ↵
// while a launch group is still in flight is a no-op.
func TestFanoutDoubleEnterInFlightGuardNoops(t *testing.T) {
	l, _, _ := fanoutTestHarness(t)
	projects := []registry.Repo{{Label: "alpha", Path: t.TempDir()}}
	a := NewApp(Deps{Launcher: l, Repos: projects})
	a.width, a.height = 80, 24
	a.Update(key("N"))
	a.Update(key(" "))
	_, cmd1 := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd1 == nil {
		t.Fatal("first enter must return a command")
	}
	_, cmd2 := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd2 != nil {
		t.Fatal("second enter while in flight must no-op (no command)")
	}
	if a.view != viewFanout {
		t.Fatal("view must remain viewFanout across the double-enter no-op")
	}
}

// TestFanoutResultMsgFiresPollCmd guards spec §2.3: "the result msg also
// fires pollCmd (M6) so the swarm appears immediately."
func TestFanoutResultMsgFiresPollCmd(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomfanoutpoll%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	a := NewApp(Deps{Engine: status.NewEngine(tm, st, t.TempDir())})
	a.width, a.height = 80, 24
	_, cmd := a.Update(fanResultMsg{group: "abcdef", results: []fanResult{{Project: "p", Name: "loom-x"}}})
	if a.view != viewDash {
		t.Fatalf("view = %v, want viewDash", a.view)
	}
	if cmd == nil {
		t.Fatal("fanResultMsg must fire pollCmd (a non-nil command)")
	}
	switch cmd().(type) {
	case snapMsg, errMsg:
		// pollCmd fired and ran Engine.Poll — either outcome proves it fired.
	default:
		t.Fatal("fanResultMsg's command did not invoke Engine.Poll")
	}
}

// TestFanoutMarkerRenderedInActivityCell guards spec §2.4: dashboard rows
// whose Tags contain "fan:" render a dim "· fan" marker in the activity
// cell (the seed-failed precedent).
func TestFanoutMarkerRenderedInActivityCell(t *testing.T) {
	a := fixtureApp()
	a.snap.Live[1].Tags = "fan:a1b2c3"
	a.rebuildRows()
	out := a.View()
	if !strings.Contains(out, "· fan") {
		t.Fatalf("View() missing the · fan marker:\n%s", out)
	}
}

// TestFanoutKeybarNEntryElisionTier guards the plan's "keybar N entry in the
// elision tier": N fan-out only appears once the frame is wide enough,
// alongside / search and w workflows.
func TestFanoutKeybarNEntryElisionTier(t *testing.T) {
	a := fixtureApp()
	a.width, a.height = 40, 24
	if strings.Contains(a.View(), "N fan-out") {
		t.Fatal("narrow dashboard must elide the N fan-out keybar entry")
	}
	a.width = 140
	if !strings.Contains(a.View(), "N fan-out") {
		t.Fatal("wide dashboard must show the N fan-out keybar entry")
	}
}

// TestFanoutViewFrameInvariant guards the frame exact-width invariant for
// the new view (same discipline as TestViewFrameInvariantAllViews /
// TestViewNarrowNoPanic, added separately since existing tests stay
// unmodified).
func TestFanoutViewFrameInvariant(t *testing.T) {
	a := NewApp(Deps{Repos: fanoutRepos()})
	for _, w := range []int{40, 80, 100} {
		a.width, a.height = w, 24
		a.Update(key("N"))
		for i, line := range strings.Split(a.View(), "\n") {
			if lw := lipgloss.Width(line); lw != a.width {
				t.Fatalf("width %d line %d: %d cells (want %d): %q", w, i, lw, a.width, line)
			}
		}
	}
}

// --- Critical review fix: fanLaunch data race / uniform-recipe split ------

// TestFanoutLaunchSnapshotsRecipeAgainstConcurrentFormMutation is the
// race-shaped regression test for the critical finding: fanLaunch used to
// capture a.fanForm.recipeFor — a bound method value over the live,
// still-interactive *fanoutForm — into the launch tea.Cmd. Because the form
// STAYS open and interactive after ↵ (spec §2.3), that cmd's goroutine and
// any subsequent keystroke read/wrote the same fanForm fields unsynchronized
// (a data race, catchable by -race), and a keystroke landing mid-launch
// (model/mode/seed change) could split the "uniform recipe across the
// group" invariant. The fix (fanLaunchItem) snapshots every project's
// recipe synchronously in fanLaunch, before the cmd is even built/returned,
// so the cmd closure holds only value copies — nothing left for a
// concurrent keystroke to race against or mutate underneath it.
//
// This fires ↵ to get the cmd, then invokes the cmd CONCURRENTLY with a
// second goroutine hammering the form with mutating keys (tab/shift-tab,
// arrows, typing) — exactly the shapes the finding called out — and asserts
// every launched project's persisted recipe (model/mode/seed) matches the
// snapshot taken at Enter-time, proving no mutation after ↵ could have
// reached what actually got launched.
func TestFanoutLaunchSnapshotsRecipeAgainstConcurrentFormMutation(t *testing.T) {
	l, st, _ := fanoutTestHarness(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	projects := []registry.Repo{{Label: "alpha", Path: dirA}, {Label: "beta", Path: dirB}}

	a := NewApp(Deps{Launcher: l, Repos: projects})
	a.width, a.height = 80, 24
	a.Update(key("N"))
	a.Update(key(" "))                       // toggle alpha (listCur 0)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})  // -> beta
	a.Update(key(" "))                       // toggle beta
	a.Update(tea.KeyMsg{Type: tea.KeyTab})   // -> model field (focus 1)
	a.Update(tea.KeyMsg{Type: tea.KeyRight}) // modelIdx 0 -> 1
	a.Update(tea.KeyMsg{Type: tea.KeyTab})   // -> mode field (focus 2)
	a.Update(tea.KeyMsg{Type: tea.KeyRight}) // modeIdx 0 -> 1
	a.Update(tea.KeyMsg{Type: tea.KeyTab})   // -> seed field (focus 3)
	a.Update(key("h"))
	a.Update(key("i"))

	wantModel, wantMode, wantSeed := modelOptions[1], modeOptions[1], "hi"

	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter with 2 selected projects must return a command")
	}

	var wg sync.WaitGroup
	var msg tea.Msg
	wg.Add(2)
	go func() {
		defer wg.Done()
		msg = cmd()
	}()
	go func() {
		defer wg.Done()
		// Form-mutating keys, fired as fast as possible right after ↵ and
		// racing the cmd's own goroutine — tab/shift-tab, arrows, and
		// typing, exactly the shapes named in the finding.
		a.Update(tea.KeyMsg{Type: tea.KeyTab})
		a.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
		a.Update(tea.KeyMsg{Type: tea.KeyLeft})
		a.Update(tea.KeyMsg{Type: tea.KeyRight})
		a.Update(tea.KeyMsg{Type: tea.KeyDown})
		a.Update(tea.KeyMsg{Type: tea.KeyUp})
		a.Update(key("z"))
	}()
	wg.Wait()

	res, ok := msg.(fanResultMsg)
	if !ok {
		t.Fatalf("cmd() returned %T, want fanResultMsg", msg)
	}
	if len(res.results) != 2 {
		t.Fatalf("results = %d, want 2", len(res.results))
	}
	for _, r := range res.results {
		if r.Err != nil {
			t.Fatalf("project %s: unexpected launch error: %v", r.Project, r.Err)
		}
		row, ok, err := st.Get(r.Name)
		if err != nil || !ok {
			t.Fatalf("store row for %s missing: ok=%v err=%v", r.Name, ok, err)
		}
		if row.Model != wantModel || row.Mode != wantMode || row.Seed != wantSeed {
			t.Fatalf("project %s launched with model=%q mode=%q seed=%q, want the pre-Enter snapshot model=%q mode=%q seed=%q — the uniform-recipe invariant split",
				r.Project, row.Model, row.Mode, row.Seed, wantModel, wantMode, wantSeed)
		}
	}
}

// TestFanoutFormFrozenWhileInFlight is fix #2's own regression test (the
// "braces" half of belt-and-braces): while fanInFlight is true, every key
// except esc/ctrl+c must no-op against fanForm — none of its fields may
// change, including a second ↵.
func TestFanoutFormFrozenWhileInFlight(t *testing.T) {
	a := NewApp(Deps{Repos: fanoutRepos()})
	a.width, a.height = 80, 24
	a.Update(key("N"))
	a.Update(key(" ")) // select alpha, so there's non-zero state to protect
	a.fanInFlight = true
	before := a.fanForm

	a.Update(tea.KeyMsg{Type: tea.KeyTab})
	a.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(tea.KeyMsg{Type: tea.KeyUp})
	a.Update(tea.KeyMsg{Type: tea.KeyLeft})
	a.Update(tea.KeyMsg{Type: tea.KeyRight})
	a.Update(key(" "))
	a.Update(key("z"))
	a.Update(tea.KeyMsg{Type: tea.KeyEnter}) // must NOT fire a second launch

	if a.fanForm.focus != before.focus ||
		a.fanForm.modelIdx != before.modelIdx ||
		a.fanForm.modeIdx != before.modeIdx ||
		a.fanForm.listCur != before.listCur ||
		a.fanForm.seed.Value() != before.seed.Value() ||
		a.fanForm.hint != before.hint {
		t.Fatalf("form mutated while fanInFlight:\nbefore: focus=%d modelIdx=%d modeIdx=%d listCur=%d seed=%q hint=%q\nafter:  focus=%d modelIdx=%d modeIdx=%d listCur=%d seed=%q hint=%q",
			before.focus, before.modelIdx, before.modeIdx, before.listCur, before.seed.Value(), before.hint,
			a.fanForm.focus, a.fanForm.modelIdx, a.fanForm.modeIdx, a.fanForm.listCur, a.fanForm.seed.Value(), a.fanForm.hint)
	}
	if !reflect.DeepEqual(a.fanForm.checked, before.checked) {
		t.Fatalf("checked map mutated while fanInFlight: before=%v after=%v", before.checked, a.fanForm.checked)
	}
	if a.view != viewFanout {
		t.Fatal("view must not have changed — esc was never pressed in this test")
	}
}

// TestFanoutEscWhileInFlightReturnsToDashButLaunchContinues covers the
// documented esc-while-in-flight decision (see updateFanoutKeys' doc
// comment): esc is deliberately let through the freeze — it returns to the
// dashboard immediately — but the in-flight launch keeps running in the
// background, and its fanResultMsg still lands and still sets fanHint once
// it completes, even though the view has already moved on.
func TestFanoutEscWhileInFlightReturnsToDashButLaunchContinues(t *testing.T) {
	l, _, _ := fanoutTestHarness(t)
	projects := []registry.Repo{{Label: "alpha", Path: t.TempDir()}}
	a := NewApp(Deps{Launcher: l, Repos: projects})
	a.width, a.height = 80, 24
	a.Update(key("N"))
	a.Update(key(" "))
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter must return a command")
	}
	if !a.fanInFlight {
		t.Fatal("fanInFlight must be set the moment the launch command is fired")
	}

	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewDash {
		t.Fatal("esc while in-flight must return to the dashboard immediately")
	}
	if !a.fanInFlight {
		t.Fatal("esc while in-flight must NOT cancel the launch — fanInFlight stays true")
	}

	// The launch's own result msg still lands and still sets fanHint, even
	// though the view moved on to the dashboard before it arrived.
	msg := cmd()
	res, ok := msg.(fanResultMsg)
	if !ok {
		t.Fatalf("cmd() returned %T, want fanResultMsg", msg)
	}
	a.Update(res)
	if a.fanInFlight {
		t.Fatal("fanInFlight must clear once fanResultMsg lands")
	}
	if a.fanHint == "" {
		t.Fatal("fanHint must be set once fanResultMsg lands, even after an early esc")
	}
}

// TestFanoutHintRenderedInViewAfterResultLands closes reviewer minor (c):
// once fanResultMsg lands, its fanHint text must actually appear in the
// rendered View() output, not just be set on the struct field.
func TestFanoutHintRenderedInViewAfterResultLands(t *testing.T) {
	a := fixtureApp()
	a.Update(fanResultMsg{group: "abc123", results: []fanResult{{Project: "p", Name: "loom-x"}}})
	if a.fanHint == "" {
		t.Fatal("fanHint must be set after fanResultMsg lands")
	}
	out := a.View()
	if !strings.Contains(out, "fan #abc123") {
		t.Fatalf("View() output missing the fanHint text after fanResultMsg landed:\n%s", out)
	}
}

// TestFanoutEnterFromSeedFocusLaunches closes reviewer minor (d): ↵ must
// launch from ANY focus zone (spec §2.1 — "↵ launches from ANY focus"),
// including focus 3 (seed), not just the checklist default.
func TestFanoutEnterFromSeedFocusLaunches(t *testing.T) {
	l, _, _ := fanoutTestHarness(t)
	projects := []registry.Repo{{Label: "alpha", Path: t.TempDir()}}
	a := NewApp(Deps{Launcher: l, Repos: projects})
	a.width, a.height = 80, 24
	a.Update(key("N"))
	a.Update(key(" ")) // select alpha
	for i := 0; i < 3; i++ {
		a.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> focus 3 (seed)
	}
	if a.fanForm.focus != 3 {
		t.Fatalf("focus = %d, want 3 (seed) before enter", a.fanForm.focus)
	}
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter from seed focus must launch (return a command)")
	}
	if !a.fanInFlight {
		t.Fatal("fanInFlight must be set after enter from seed focus")
	}
	if a.view != viewFanout {
		t.Fatal("view must stay on viewFanout until fanResultMsg lands")
	}
}

// --- Wall (`W`) — spec §3 -------------------------------------------------

// wallFixtureSnap builds three live rows with distinct CreatedAt values
// (200/100/300) so a naive "input order" assertion would be wrong — the
// wall's own stable sort (CreatedAt then Name) must reorder them to
// a(100) < b(200) < c(300) regardless of this slice's own order.
func wallFixtureSnap() status.Snapshot {
	return status.Snapshot{Live: []status.Row{
		{SessionRow: store.SessionRow{Name: "loom-b", ProjectLabel: "tavli", CreatedAt: 200}, Status: status.NeedsYou},
		{SessionRow: store.SessionRow{Name: "loom-a", ProjectLabel: "parallax", CreatedAt: 100}, Status: status.Running, LastTool: "Edit"},
		{SessionRow: store.SessionRow{Name: "loom-c", ProjectLabel: "volar", CreatedAt: 300}, Status: status.Idle},
	}}
}

func wallContainsSubstring(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func wallFixtureApp() *App {
	a := NewApp(Deps{})
	a.width, a.height = 100, 30
	a.Update(snapMsg(wallFixtureSnap()))
	return a
}

// TestWOpensWallFromDash: `W` opens viewWall (spec §1/§4 — verified unbound,
// distinct from lowercase `w` workflows).
func TestWOpensWallFromDash(t *testing.T) {
	a := wallFixtureApp()
	a.Update(key("W"))
	if a.view != viewWall {
		t.Fatalf("view = %v, want viewWall", a.view)
	}
}

// TestWallStableOrderCreatedAtThenNameNotAttentionOrder guards spec §3.1 I4:
// the wall's order is CreatedAt-then-Name, NOT the dashboard's attention
// order (NeedsYou/Running/Idle) — and a status flip does NOT reorder it.
func TestWallStableOrderCreatedAtThenNameNotAttentionOrder(t *testing.T) {
	a := wallFixtureApp()
	got := make([]string, len(a.wallOrder))
	for i, r := range a.wallOrder {
		got[i] = r.Name
	}
	want := []string{"loom-a", "loom-b", "loom-c"} // CreatedAt 100,200,300
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wallOrder = %v, want %v (CreatedAt-then-Name, not attention order)", got, want)
	}

	// Flip loom-a's status to NeedsYou (would jump to the front of the
	// dashboard's attention order) and re-poll: the wall order must NOT
	// change.
	snap := wallFixtureSnap()
	snap.Live[1].Status = status.NeedsYou // loom-a
	a.Update(snapMsg(snap))
	got2 := make([]string, len(a.wallOrder))
	for i, r := range a.wallOrder {
		got2[i] = r.Name
	}
	if !reflect.DeepEqual(got2, want) {
		t.Fatalf("wallOrder reordered on a status flip: %v, want unchanged %v", got2, want)
	}
}

// TestWallSelectionSurvivesInputReshuffle: the wall's own sort makes its
// order independent of a.snap.Live's own slice order — feeding the SAME
// sessions back in a different slice order must not move the selection.
func TestWallSelectionSurvivesInputReshuffle(t *testing.T) {
	a := wallFixtureApp()
	a.wallSelected = "loom-b"
	snap := wallFixtureSnap()
	snap.Live[0], snap.Live[2] = snap.Live[2], snap.Live[0] // reshuffle input order
	a.Update(snapMsg(snap))
	if a.wallSelected != "loom-b" {
		t.Fatalf("wallSelected = %q, want unchanged \"loom-b\" across an input reshuffle", a.wallSelected)
	}
}

// TestWallSelectionNearestNeighborOnDeath guards spec §3.5: when the
// selected session vanishes, selection moves to its nearest neighbor (here:
// loom-b at index 1 dies; loom-a at index 0 is the nearest survivor).
func TestWallSelectionNearestNeighborOnDeath(t *testing.T) {
	a := wallFixtureApp()
	a.wallSelected = "loom-b"
	snap := wallFixtureSnap()
	snap.Live = []status.Row{snap.Live[1], snap.Live[2]} // drop loom-b (a,c remain)
	a.Update(snapMsg(snap))
	if a.wallSelected != "loom-a" {
		t.Fatalf("wallSelected = %q, want nearest-neighbor \"loom-a\" after loom-b vanished", a.wallSelected)
	}
}

// TestWallSelectionClearedWhenAllVanish: no panic, no dangling selection,
// when the whole live set disappears.
func TestWallSelectionClearedWhenAllVanish(t *testing.T) {
	a := wallFixtureApp()
	a.wallSelected = "loom-b"
	a.Update(snapMsg(status.Snapshot{}))
	if a.wallSelected != "" {
		t.Fatalf("wallSelected = %q, want \"\" once every session has vanished", a.wallSelected)
	}
	a.view = viewWall
	out := a.View() // must not panic
	if !strings.Contains(out, "no live sessions") {
		t.Fatalf("empty wall must render the empty-state message:\n%s", out)
	}
}

// TestWallColumnWidthsExtraToRightColumn guards spec §3.2 ("M1 corrected"):
// when the 2-column split is uneven, the extra cell goes to the RIGHT
// column.
func TestWallColumnWidthsExtraToRightColumn(t *testing.T) {
	// inner=42 -> usable (minus 1-col gutter) = 41, odd -> 20/21.
	colL, colR := wallColumnWidths(42)
	if colL != 20 || colR != 21 {
		t.Fatalf("wallColumnWidths(42) = (%d,%d), want (20,21) — extra cell to the RIGHT column", colL, colR)
	}
	// even split: no extra either way.
	colL, colR = wallColumnWidths(43) // usable=42, even
	if colL != 21 || colR != 21 {
		t.Fatalf("wallColumnWidths(43) = (%d,%d), want (21,21)", colL, colR)
	}
}

// TestWallTailHClampTinyTerminal guards spec §3.2 M2: tailH = clamp(6,1,
// netBodyH-2) never goes below 1 or panics at a tiny terminal height. Unlike
// the shipped version of this test, perPage is NOT expected to stay >= 2
// here: at this height no row's cellH fits in netBodyH, so perPage
// correctly collapses to 0 and renderWall falls back to its degenerate
// single-line body — the fix for the frame overflowing a.height (a forced
// perPage>=2 at this height used to render one line taller than a.height).
func TestWallTailHClampTinyTerminal(t *testing.T) {
	a := wallFixtureApp()
	a.height = 4 // bodyH=2, netBodyH=1 -> tailH clamps to its floor, 1; no row fits.
	perPage, tailH := a.wallPageSize()
	if tailH != 1 {
		t.Fatalf("tailH = %d, want 1 (clamped) at a tiny terminal", tailH)
	}
	if perPage != 0 {
		t.Fatalf("perPage = %d, want 0 (no row fits) at this tiny terminal height", perPage)
	}
	a.view = viewWall
	out := a.View() // must not panic
	lines := strings.Split(out, "\n")
	if len(lines) > a.height {
		t.Fatalf("tiny-terminal wall rendered %d lines, want <= a.height (%d):\n%s", len(lines), a.height, out)
	}
	for i, line := range lines {
		if lw := lipgloss.Width(line); lw != a.width {
			t.Fatalf("tiny-terminal wall line %d = %d cells, want %d: %q", i, lw, a.width, line)
		}
	}
}

// TestWallPaginationIndicatorFollowsSelection guards spec §3.2: paging
// follows the selection (there is no separate "next page" key) — a tiny
// height forcing perPage=2 with 3 sessions splits into two pages; selecting
// the last session must show page 2's indicator and hide the first page's
// labels.
func TestWallPaginationIndicatorFollowsSelection(t *testing.T) {
	a := wallFixtureApp()
	a.height = 6 // netBodyH=3 -> tailH clamps to 1, cellH=3, rowsThatFit=1, perPage=2
	perPage, _ := a.wallPageSize()
	if perPage != 2 {
		t.Fatalf("perPage = %d, want 2 for this fixture height", perPage)
	}
	a.wallSelected = "loom-c" // index 2 -> page 1 (start=2,end=3)
	a.view = viewWall
	out := a.View()
	if !strings.Contains(out, "3–3 of 3") {
		t.Fatalf("View() missing the page-2 indicator \"3–3 of 3\":\n%s", out)
	}
	if strings.Contains(out, "parallax") || strings.Contains(out, "tavli") {
		t.Fatalf("page 2 must not render page 1's cells:\n%s", out)
	}
	if !strings.Contains(out, "volar") {
		t.Fatalf("page 2 must render the selected session's cell:\n%s", out)
	}
}

// TestWallCursorMovesAcrossPageBoundary: ↓ from the last row of page 1
// crosses onto page 2, updating both the indicator and the selection.
func TestWallCursorMovesAcrossPageBoundary(t *testing.T) {
	a := wallFixtureApp()
	a.height = 6 // perPage=2, same fixture as above
	a.wallSelected = "loom-b"
	a.view = viewWall
	a.Update(key("j"))
	if a.wallSelected != "loom-c" {
		t.Fatalf("wallSelected = %q, want \"loom-c\" after moving down from the last row of page 1", a.wallSelected)
	}
	out := a.View()
	if !strings.Contains(out, "3–3 of 3") {
		t.Fatalf("View() did not follow the selection onto page 2:\n%s", out)
	}
}

// TestWallUpDownClampNoWrap: at either end of wallOrder, further movement in
// the same direction is a no-op (no wrap — same discipline as the dashboard
// cursor).
func TestWallUpDownClampNoWrap(t *testing.T) {
	a := wallFixtureApp()
	a.view = viewWall
	a.wallSelected = "loom-a" // index 0
	a.Update(key("k"))
	if a.wallSelected != "loom-a" {
		t.Fatalf("up from the first row moved selection to %q, want no-op", a.wallSelected)
	}
	a.wallSelected = "loom-c" // last index
	a.Update(key("j"))
	if a.wallSelected != "loom-c" {
		t.Fatalf("down from the last row moved selection to %q, want no-op", a.wallSelected)
	}
}

// TestWallLayoutExactWidthCJK guards spec §5: "layout exact-width at
// 100/46/odd (CJK content)" — a CJK project label and CJK captured pane
// content must not overshoot the frame at any of these widths.
func TestWallLayoutExactWidthCJK(t *testing.T) {
	a := wallFixtureApp()
	a.snap.Live[0].ProjectLabel = "日本語プロジェクト"
	a.snap.Live[0].Title = "続けて作業する"
	a.rebuildRows()
	a.applyWallOrder()
	a.wallCaptures = map[string]wallCapture{
		"loom-b": {lines: []string{"漢字とひらがなを含む出力行です", "second line", strings.Repeat("字", 60)}},
	}
	a.view = viewWall
	for _, w := range []int{100, 46, 47} {
		a.width = w
		out := a.View()
		for i, line := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(line); lw != w {
				t.Fatalf("width %d line %d = %d cells (want %d): %q", w, i, lw, w, line)
			}
		}
	}
}

// TestWallCaptureErrorCellRendersUnavailableAndGatesEnter guards spec §3.4:
// a known capture error keeps the cell, renders "(pane unavailable)", and
// gates ↵ off for that specific session.
func TestWallCaptureErrorCellRendersUnavailableAndGatesEnter(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomwallerr%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	a := NewApp(Deps{Tmux: tm})
	a.width, a.height = 100, 30
	a.Update(snapMsg(wallFixtureSnap()))
	a.wallSelected = "loom-a"
	a.wallCaptures = map[string]wallCapture{"loom-a": {err: true}}
	a.view = viewWall

	out := a.View()
	if !strings.Contains(out, "(pane unavailable)") {
		t.Fatalf("View() missing \"(pane unavailable)\":\n%s", out)
	}
	_, cmd := a.Update(key("enter"))
	if cmd != nil {
		t.Fatal("enter on a capture-error cell must be gated off (nil command)")
	}
}

// TestWallRealCaptureLiveAndErrorCells drives wallCaptureCmd against a real
// throwaway tmux session (a plain shell pane — no fake-claude launcher
// needed, CapturePane doesn't care what's running) alongside a row whose
// name has no backing tmux session at all: one page's capture must produce
// one successful, content-bearing result and one error result. Also proves
// ↵ attaches (returns a non-nil command, per the TestWFAttachOnLiveRunReturnsCommand
// precedent — never actually invoked, so no real attach happens in-test) on
// the live cell once its capture has landed clean.
func TestWallRealCaptureLiveAndErrorCells(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomwallcap%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := tm.NewSession("loom-wall-live", dir, "sh -c 'printf hello-wall-test; sleep 30'", 80, 24); err != nil {
		t.Fatal(err)
	}

	a := NewApp(Deps{Tmux: tm})
	a.width, a.height = 100, 30
	a.Update(snapMsg(status.Snapshot{Live: []status.Row{
		{SessionRow: store.SessionRow{Name: "loom-wall-live", ProjectLabel: "live", CreatedAt: 1}, Status: status.Running},
		{SessionRow: store.SessionRow{Name: "loom-wall-dead", ProjectLabel: "dead", CreatedAt: 2}, Status: status.Running},
	}}))
	a.wallSelected = "loom-wall-live"
	a.view = viewWall

	// The shell command needs a moment to actually run before its output
	// shows up in a capture — same "poll until it lands" discipline as
	// internal/tmux's own TestKillSessionMarksDeadWithExitCode.
	deadline := time.Now().Add(5 * time.Second)
	var live wallCapture
	for {
		msg, ok := a.wallCaptureCmd()().(wallMsg)
		if !ok {
			t.Fatalf("wallCaptureCmd's result = %T, want wallMsg", msg)
		}
		a.Update(msg)
		var capOK bool
		live, capOK = a.wallCaptures["loom-wall-live"]
		if capOK && !live.err && wallContainsSubstring(live.lines, "hello-wall-test") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("captured lines never showed \"hello-wall-test\": %+v", live)
		}
		time.Sleep(100 * time.Millisecond)
	}
	dead, ok := a.wallCaptures["loom-wall-dead"]
	if !ok || !dead.err {
		t.Fatalf("loom-wall-dead capture = %+v, want err=true (no backing tmux session)", dead)
	}

	out := a.View()
	if !strings.Contains(out, "hello-wall-test") {
		t.Fatalf("View() missing the live cell's captured content:\n%s", out)
	}

	_, attachCmd := a.Update(key("enter"))
	if attachCmd == nil {
		t.Fatal("enter on the clean-captured live cell must return a command")
	}
}

// TestWallStaleCaptureGenerationDiscarded guards spec §3.3: a wallMsg
// carrying an older generation than the CURRENT a.wallSeq is discarded in
// full, even though its session name is still alive.
func TestWallStaleCaptureGenerationDiscarded(t *testing.T) {
	a := wallFixtureApp()
	a.view = viewWall
	a.wallSeq = 5 // a newer capture has "since been fired"
	a.Update(wallMsg{gen: 4, results: []wallCaptureResult{{name: "loom-a", lines: []string{"stale"}}}})
	if _, ok := a.wallCaptures["loom-a"]; ok {
		t.Fatal("a stale-generation wallMsg must not populate wallCaptures")
	}
	a.Update(wallMsg{gen: 5, results: []wallCaptureResult{{name: "loom-a", lines: []string{"fresh"}}}})
	got, ok := a.wallCaptures["loom-a"]
	if !ok || len(got.lines) == 0 || got.lines[0] != "fresh" {
		t.Fatalf("a fresh-generation wallMsg was not applied: %+v", got)
	}
}

// TestWallVanishedEntryDroppedFromCapture guards spec §3.3/§3.4: even within
// a fresh-generation wallMsg, a result whose name is no longer in
// a.wallOrder (the session ended between cmd-fire and landing) is dropped.
func TestWallVanishedEntryDroppedFromCapture(t *testing.T) {
	a := wallFixtureApp()
	a.view = viewWall
	a.Update(wallMsg{gen: a.wallSeq, results: []wallCaptureResult{
		{name: "loom-a", lines: []string{"ok"}},
		{name: "loom-ghost", lines: []string{"should be dropped"}},
	}})
	if _, ok := a.wallCaptures["loom-a"]; !ok {
		t.Fatal("a live session's capture result must be applied")
	}
	if _, ok := a.wallCaptures["loom-ghost"]; ok {
		t.Fatal("a vanished session's capture result must be dropped")
	}
}

// TestWallTickFiresOneTickAfterPlusCapture guards the ONE-tickAfter
// discipline (the peekCmd/searchStatusCmd batching precedent, spec §5's
// binding "ONE-tickAfter sweep"): a tickMsg while viewWall is open returns a
// batch of exactly tickAfter + wallCaptureCmd (pollCmd nil-filtered, Engine
// is nil here) — never a second, independent tick chain.
func TestWallTickFiresOneTickAfterPlusCapture(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomwalltick%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	a := NewApp(Deps{Tmux: tm}) // Engine nil -> pollCmd() nil, filtered out of the batch
	a.width, a.height = 100, 30
	a.Update(snapMsg(wallFixtureSnap()))
	a.wallSelected = "loom-a"
	a.view = viewWall

	_, cmd := a.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tick in viewWall returned no command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("tick in viewWall cmd = %T, want tea.BatchMsg", cmd())
	}
	if len(batch) != 2 {
		t.Fatalf("batch len = %d, want 2 (tickAfter + wallCaptureCmd; pollCmd nil-filtered)", len(batch))
	}
}

// TestWallKeybarWEntryElisionTier mirrors TestFanoutKeybarNEntryElisionTier:
// `W wall` only appears once the frame is wide enough.
func TestWallKeybarWEntryElisionTier(t *testing.T) {
	a := fixtureApp()
	a.width, a.height = 40, 24
	if strings.Contains(a.View(), "W wall") {
		t.Fatal("narrow dashboard must elide the W wall keybar entry")
	}
	a.width = 140
	if !strings.Contains(a.View(), "W wall") {
		t.Fatal("wide dashboard must show the W wall keybar entry")
	}
}

// TestWallViewFrameInvariantEmptyAndPopulated guards the exact-width
// invariant (TestViewFrameInvariantAllViews/TestViewNarrowNoPanic precedent)
// for both the empty-wall and populated-wall states, across widths — AND,
// for the populated case, sweeps a.height from 3 to 30 asserting the frame
// never renders MORE lines than a.height. A prior bug forced a full row to
// render even when it couldn't fit (rows floored to 1) and failed to count
// the leading blank body line against the height budget, so the wall
// overflowed a.height at every height in this sweep from 3 through 11.
func TestWallViewFrameInvariantEmptyAndPopulated(t *testing.T) {
	for _, w := range []int{40, 80, 100} {
		a := NewApp(Deps{})
		a.width, a.height = w, 24
		a.view = viewWall
		for i, line := range strings.Split(a.View(), "\n") {
			if lw := lipgloss.Width(line); lw != w {
				t.Fatalf("empty wall, width %d line %d = %d cells (want %d): %q", w, i, lw, w, line)
			}
		}

		b := wallFixtureApp()
		b.width = w
		b.view = viewWall
		for h := 3; h <= 30; h++ {
			b.height = h
			out := b.View()
			lines := strings.Split(out, "\n")
			if len(lines) > h {
				t.Fatalf("populated wall, width %d height %d rendered %d lines, want <= %d:\n%s", w, h, len(lines), h, out)
			}
			for i, line := range lines {
				if lw := lipgloss.Width(line); lw != w {
					t.Fatalf("populated wall, width %d height %d line %d = %d cells (want %d): %q", w, h, i, lw, w, line)
				}
			}
		}
	}
}

// TestWallEscReturnsToDash / TestWallQCtrlCQuits round out the basic
// navigation contract (spec §3.5).
func TestWallEscReturnsToDash(t *testing.T) {
	a := wallFixtureApp()
	a.view = viewWall
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewDash {
		t.Fatalf("view = %v, want viewDash after esc", a.view)
	}
}

func TestWallQCtrlCQuits(t *testing.T) {
	a := wallFixtureApp()
	a.view = viewWall
	_, cmd := a.Update(key("q"))
	if cmd == nil {
		t.Fatal("q in viewWall did not return a command")
	}
	_, cmd2 := a.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd2 == nil {
		t.Fatal("ctrl+c in viewWall did not return a command")
	}
}

// TestWallZeroDepsNoPanic: Deps{} — opening the wall, ticking, moving the
// selection, pressing enter, and closing it must all be no-ops that never
// panic (Deps nil-safety contract, same as every other view).
func TestWallZeroDepsNoPanic(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 100, 30
	a.Update(key("W"))
	if a.view != viewWall {
		t.Fatal("W did not open the wall with Deps{}")
	}
	a.Update(tickMsg(time.Now()))
	a.Update(key("j"))
	a.Update(key("k"))
	_, cmd := a.Update(key("enter"))
	if cmd != nil {
		t.Fatal("enter with Deps{} (nil Tmux, no sessions) must be a no-op")
	}
	_ = a.View()
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.view != viewDash {
		t.Fatal("esc did not return to the dashboard")
	}
}

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

func TestKeybarShowsDismissAndClear(t *testing.T) {
	a := fixtureApp()
	a.width, a.height = 150, 30 // fixtureApp defaults to 100x30; widen locally so the suffix clears the width gate (fixture already has a recent row)
	body := a.View()
	if !strings.Contains(body, "x kill/dismiss") {
		t.Fatalf("keybar missing 'x kill/dismiss':\n%s", body)
	}
	if !strings.Contains(body, "X clear") {
		t.Fatalf("keybar missing 'X clear':\n%s", body)
	}
}

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

// F2: dismissing a finished row whose tmux session is a DEAD lingering pane
// (remain-on-exit) must reap it (KillSession) then delete the row, closing
// the window where a bare delete lets the next poll re-adopt a zombie.
func TestDismissReapsDeadPane(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomhard3%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := tm.NewSession("loom-dead", dir, "true", 80, 24); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ps, err := tm.PaneStatus("loom-dead")
		if err != nil {
			t.Fatalf("PaneStatus: %v", err)
		}
		if ps.Dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pane never went dead — is remain-on-exit on?")
		}
		time.Sleep(100 * time.Millisecond)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Upsert(store.SessionRow{Name: "loom-dead", ProjectLabel: "p", CreatedAt: 1, EndedAt: 2, ExitCode: 0, LastStatus: "done"})

	a := NewApp(Deps{Store: st, Tmux: tm})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{Recent: []store.SessionRow{{Name: "loom-dead", LastStatus: "done"}}}
	a.rebuildRows()
	a.cursor = 0

	a.Update(key("x"))
	_, cmd := a.Update(key("y"))
	if cmd == nil {
		t.Fatal("'y' returned no command")
	}
	cmd()

	if _, ok, _ := st.Get("loom-dead"); ok {
		t.Fatal("row with a dead lingering pane was not deleted")
	}
	if tm.HasSession("loom-dead") {
		t.Fatal("dead lingering pane was not reaped (KillSession) before delete")
	}
}

// F2: bulk clear must reap DEAD lingering panes and delete their rows, but
// skip (not kill, not delete) rows whose tmux session is secretly LIVE.
func TestBulkClearReapsDeadSkipsLive(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomhard4%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := tm.NewSession("loom-dead", dir, "true", 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := tm.NewSession("loom-live", dir, "sleep 30", 80, 24); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ps, err := tm.PaneStatus("loom-dead")
		if err != nil {
			t.Fatalf("PaneStatus: %v", err)
		}
		if ps.Dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pane never went dead — is remain-on-exit on?")
		}
		time.Sleep(100 * time.Millisecond)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Upsert(store.SessionRow{Name: "loom-dead", ProjectLabel: "p", CreatedAt: 1, EndedAt: 2, ExitCode: 0, LastStatus: "done"})
	st.Upsert(store.SessionRow{Name: "loom-live", ProjectLabel: "p", CreatedAt: 1, EndedAt: 2, ExitCode: 1, LastStatus: "error"})

	a := NewApp(Deps{Store: st, Tmux: tm})
	a.width, a.height = 100, 30
	a.snap = status.Snapshot{Recent: []store.SessionRow{
		{Name: "loom-dead", LastStatus: "done"},
		{Name: "loom-live", LastStatus: "error"},
	}}
	a.rebuildRows()

	a.Update(key("X"))
	if a.view != viewConfirmClear || a.clearCount != 2 {
		t.Fatalf("clear confirm not opened: view=%v count=%d", a.view, a.clearCount)
	}
	_, cmd := a.Update(key("y"))
	if cmd == nil {
		t.Fatal("'y' returned no command")
	}
	cmd()

	if _, ok, _ := st.Get("loom-dead"); ok {
		t.Fatal("dead-pane row was not deleted by bulk clear")
	}
	if tm.HasSession("loom-dead") {
		t.Fatal("dead lingering pane was not reaped by bulk clear")
	}
	if _, ok, _ := st.Get("loom-live"); !ok {
		t.Fatal("live-session row was deleted by bulk clear (should be skipped)")
	}
	if !tm.HasSession("loom-live") {
		t.Fatal("live tmux session was killed by bulk clear (should be skipped)")
	}
}

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

// H6 change 3: if the recomputed count hits 0 while the clear dialog is
// open (everything finished got removed out from under it), the dialog
// must auto-close back to the dashboard rather than show "clear 0 finished
// sessions".
func TestClearDialogClosesWhenCountHitsZero(t *testing.T) {
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
	if a.view != viewConfirmClear || a.clearCount != 1 {
		t.Fatalf("clear confirm not opened: view=%v count=%d", a.view, a.clearCount)
	}

	// the one finished row disappears out from under the open dialog
	if err := st.DeleteSession("loom-d1"); err != nil {
		t.Fatal(err)
	}
	a.Update(tickMsg{})
	if a.view != viewDash {
		t.Fatalf("view = %v, want viewDash (dialog should auto-close at 0)", a.view)
	}
}

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
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("ctrl+c: cmd = %T, want tea.QuitMsg", cmd())
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
