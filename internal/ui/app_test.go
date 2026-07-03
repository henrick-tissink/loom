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

func TestSnapMsgWithTransitionsEmitsNotify(t *testing.T) {
	a := fixtureApp()
	_, cmd := a.Update(snapMsg(status.Snapshot{NewlyNeedsYou: []string{"tavli · fix race"}}))
	if cmd == nil {
		t.Fatal("expected a notify command for transitions")
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
