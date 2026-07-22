package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
)

// fakeClaudeScript prints a trust dialog for 1s — including the "❯ 1. Yes,
// proceed" select-cursor line that ALSO contains the bare ReadyMarker glyph
// (finding 3 regression bait) — then clears and prints the real ready
// marker, then cats input to a sink file for assertion.
//
// During the 1s dialog phase, stdin is ALSO consumed (into a second,
// dialog-sink file, $2) by a backgrounded `cat` that is killed the instant
// the dialog phase ends. This gives the trust-ordering regression real
// teeth: previously a seed sent during the (buggy) ready-first dialog phase
// just sat buffered in the pty until the final `exec cat > "$1"` started, so
// every assertion passed even against the old, unfixed ordering. Now, if the
// seed is sent while the dialog is still showing, those bytes are consumed
// by the dialog-phase cat and permanently lost from $2 (macOS lacks a
// `timeout` binary, hence the manual background-cat + sleep + kill instead
// of `timeout 1 cat`) — so the dialog-sink file must be empty at the end.
//
// `exec 3<&0` before backgrounding is required: POSIX shells default an
// asynchronous (`&`) command's stdin to /dev/null unless the command
// explicitly redirects it, so a bare `(cat > "$2") &` silently reads
// /dev/null instead of the pty and never captures anything — confirmed by
// direct tmux experiments while building this test. Duplicating stdin onto
// fd 3 first and having the background cat read `<&3` counts as an
// explicit redirection, so it keeps the pty attachment.
const fakeClaudeScript = `#!/bin/sh
echo "Do you trust the files in this folder?"
echo "❯ 1. Yes, proceed"
exec 3<&0
cat <&3 > "$2" &
dialogcat=$!
sleep 1
kill "$dialogcat" 2>/dev/null
wait "$dialogcat" 2>/dev/null
exec 3<&-
clear 2>/dev/null || printf '\033[2J'
echo "❯"
exec cat > "$1"
`

func testLauncher(t *testing.T) (*Launcher, string) {
	t.Helper()
	tm := &tmux.Client{Socket: fmt.Sprintf("loomln%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// Physical path: Launch/Resume store the symlink-resolved cwd (physicalDir),
	// and on macOS t.TempDir() sits under /var, itself a symlink to /private/var.
	// Resolving here lets assertions compare against `dir` directly instead of
	// every test re-deriving the resolved form.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := &Launcher{
		Tmux: tm, Store: st,
		ClaudeConfigDir: t.TempDir(),
		ClaudeJSONPath:  filepath.Join(t.TempDir(), ".claude.json"),
		ReadyMarker:     DefaultReadyMarker,
		TrustMarker:     DefaultTrustMarker,
		ReadyTimeout:    10 * time.Second,
		PollEvery:       100 * time.Millisecond,
	}
	return l, dir
}

func TestLaunchCreatesSessionAndRow(t *testing.T) {
	l, dir := testLauncher(t)
	r := Recipe{ProjectLabel: "p", Cwd: dir, Model: "opus", Mode: "plan"}
	name, err := l.Launch(r, 120, 40, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !l.Tmux.HasSession(name) {
		t.Fatal("no tmux session")
	}
	row, ok, _ := l.Store.Get(name)
	if !ok || row.Model != "opus" || row.Mode != "plan" || row.LastStatus != "unknown" {
		t.Fatalf("row = %+v %v", row, ok)
	}
	id, _ := SessionIDFromTmuxName(name)
	if row.ClaudeSessionID != id {
		t.Fatalf("id mismatch: %q vs %q", row.ClaudeSessionID, id)
	}
}

// The launch command is what the recipe says — verified via a stub command.
// It also guards finding 3: the trust-dialog phase renders a "❯ 1. Yes,
// proceed" cursor line containing the bare ReadyMarker glyph, and the seed
// must NOT fire while that dialog is showing — only once it's dismissed and
// the real ready prompt appears.
func TestSeedWaitsForTrustThenReady(t *testing.T) {
	l, dir := testLauncher(t)
	sink := filepath.Join(dir, "sink.txt")
	dialogSink := filepath.Join(dir, "dialog-sink.txt")
	script := filepath.Join(dir, "fake-claude.sh")
	os.WriteFile(script, []byte(fakeClaudeScript), 0o755)

	// launch manually with the fake command, then drive seedWhenReady directly
	id := NewSessionID()
	name := TmuxName(id)
	if err := l.Tmux.NewSession(name, dir, "'"+script+"' '"+sink+"' '"+dialogSink+"'", 80, 24); err != nil {
		t.Fatal(err)
	}
	// a store row is required for SetSeedStatus (finding 4) to have somewhere
	// to land; Launch() would normally create it, but this test drives
	// seedWhenReady directly against a hand-rolled tmux session.
	if err := l.Store.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id,
		ProjectLabel: "p", Cwd: dir, CreatedAt: 1, EndedAt: -1, ExitCode: -1,
		LastStatus: "unknown"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { l.seedWhenReady(name, "hello seed"); close(done) }()

	// While the trust dialog (with its ReadyMarker-containing cursor line) is
	// showing, the seed must not be sent yet.
	time.Sleep(400 * time.Millisecond)
	if b, _ := os.ReadFile(sink); len(b) != 0 {
		t.Fatalf("seed sent during trust-dialog phase (finding 3 regression): sink = %q", b)
	}

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("seed goroutine never finished")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(sink)
		if string(b) == "hello seed\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink = %q, want seed after ready marker", b)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// finding 4: a successfully delivered seed must be recorded, not silently
	// dropped/untracked.
	row, ok, err := l.Store.Get(name)
	if err != nil || !ok {
		t.Fatalf("Store.Get(%q) = %v, %v, %v", name, row, ok, err)
	}
	if row.SeedStatus != "sent" {
		t.Fatalf("SeedStatus = %q, want sent", row.SeedStatus)
	}

	// The real regression teeth: if the seed had been sent during the
	// dialog phase (old, buggy ready-first ordering), those bytes would
	// have been consumed by the dialog-phase's backgrounded `cat` into
	// dialog-sink before it was killed, and would never reach $1. With the
	// correct trust-first ordering the seed is only ever sent after the
	// dialog phase's cat has already been killed, so dialog-sink must be
	// empty.
	db, err := os.ReadFile(dialogSink)
	if err != nil {
		t.Fatalf("reading dialog-sink: %v", err)
	}
	if len(db) != 0 {
		t.Fatalf("seed leaked into dialog-phase sink (finding 3 regression): dialog-sink = %q", db)
	}
}

// TestSeedWhenReadyRecordsFailureOnTimeout guards finding 4: when the ready
// marker never appears (session hangs, crashes, or is simply slow), the seed
// must not vanish silently — the outcome is recorded as 'failed' so the UI
// can surface it.
func TestSeedWhenReadyRecordsFailureOnTimeout(t *testing.T) {
	l, dir := testLauncher(t)
	l.ReadyTimeout = 300 * time.Millisecond
	l.PollEvery = 50 * time.Millisecond

	id := NewSessionID()
	name := TmuxName(id)
	// a session that never prints the ready marker
	if err := l.Tmux.NewSession(name, dir, "sleep 30", 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := l.Store.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id,
		ProjectLabel: "p", Cwd: dir, CreatedAt: 1, EndedAt: -1, ExitCode: -1,
		LastStatus: "unknown"}); err != nil {
		t.Fatal(err)
	}

	l.seedWhenReady(name, "never sent")

	row, ok, err := l.Store.Get(name)
	if err != nil || !ok {
		t.Fatalf("Store.Get(%q) = %v, %v, %v", name, row, ok, err)
	}
	if row.SeedStatus != "failed" {
		t.Fatalf("SeedStatus = %q, want failed", row.SeedStatus)
	}
}

func TestResumeCreatesFreshTmuxSession(t *testing.T) {
	l, dir := testLauncher(t)
	old := store.SessionRow{Name: "loom-old", ClaudeSessionID: "dddddddd-dddd-dddd-dddd-dddddddddddd",
		ProjectLabel: "p", Cwd: dir, Model: "opus", CreatedAt: 1, EndedAt: 5, ExitCode: 0, LastStatus: "done"}
	l.Store.Upsert(old)
	name, err := l.Resume(old, 80, 24, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if name == "loom-old" {
		t.Fatal("resume must mint a fresh tmux name")
	}
	if !l.Tmux.HasSession(name) {
		t.Fatal("no tmux session (claude missing is fine — the pane may die, but the session was created)")
	}
	row, ok, _ := l.Store.Get(name)
	if !ok || row.Model != "opus" || row.Cwd != dir {
		t.Fatalf("row = %+v %v", row, ok)
	}
	if row.ClaudeSessionID != old.ClaudeSessionID {
		t.Fatalf("ClaudeSessionID = %q, want unchanged %q (--resume appends to the same transcript — spike Finding 4)",
			row.ClaudeSessionID, old.ClaudeSessionID)
	}
}

// TestResumePreservesSessionIDNoGoroutineCorrection guards the spike-verified
// deviation from the brief: --resume appends to the SAME <uuid>.jsonl with the
// SAME sessionId, so there is no NewestSince-correction goroutine to race
// against. Immediately after Resume returns, the row must already carry the
// old ClaudeSessionID — nothing async should be needed or expected to change it.
func TestResumePreservesSessionIDNoGoroutineCorrection(t *testing.T) {
	l, dir := testLauncher(t)
	old := store.SessionRow{Name: "loom-old2", ClaudeSessionID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
		ProjectLabel: "p", Cwd: dir, Model: "sonnet", CreatedAt: 1, EndedAt: 5, ExitCode: 0, LastStatus: "done"}
	l.Store.Upsert(old)
	name, err := l.Resume(old, 80, 24, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	row, ok, _ := l.Store.Get(name)
	if !ok {
		t.Fatal("row missing immediately after Resume")
	}
	if row.ClaudeSessionID != old.ClaudeSessionID {
		t.Fatalf("ClaudeSessionID changed immediately after Resume (unexpected async correction): got %q, want %q",
			row.ClaudeSessionID, old.ClaudeSessionID)
	}
}

// TestWaitReadyBoundsTrustPendingWait guards spec §12's first defect: the
// trust branch used to `continue` without advancing any clock, so a dialog
// nobody answered pinned the goroutine forever and seedWhenReady never
// recorded an outcome. TrustTimeout is that bound; without it this test hangs
// until the package test binary's own timeout.
func TestWaitReadyBoundsTrustPendingWait(t *testing.T) {
	l, dir := testLauncher(t)
	l.ReadyTimeout = 10 * time.Second // deliberately long: the trust bound must be what fires
	l.TrustTimeout = 300 * time.Millisecond
	l.PollEvery = 50 * time.Millisecond

	id := NewSessionID()
	name := TmuxName(id)
	// A pane that shows the trust dialog and never advances past it. The text
	// is taken from l.TrustMarker rather than hardcoded: this fixture was
	// pinned to "Do you trust the files in this folder?" and silently stopped
	// exercising the trust path when the real dialog wording turned out to be
	// different (docs/spikes/2026-07-22-add-dir-spike.md). Deriving it from the
	// configured marker means the test cannot go stale that way again.
	if err := l.Tmux.NewSession(name, dir,
		fmt.Sprintf(`sh -c 'echo %q; sleep 60'`, l.TrustMarker), 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := l.Store.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id,
		ProjectLabel: "p", Cwd: dir, CreatedAt: 1, EndedAt: -1, ExitCode: -1,
		LastStatus: "unknown"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan bool, 1)
	go func() { done <- l.waitReady(name) }()
	select {
	case ready := <-done:
		if ready {
			t.Fatal("waitReady = true with the trust dialog still showing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitReady never returned: trust-pending wait is unbounded (spec §12)")
	}

	// And the seed's outcome must land as 'failed' rather than staying in limbo.
	l.seedWhenReady(name, "never sent")
	row, ok, err := l.Store.Get(name)
	if err != nil || !ok {
		t.Fatalf("Store.Get(%q) = %v, %v, %v", name, row, ok, err)
	}
	if row.SeedStatus != "failed" {
		t.Fatalf("SeedStatus = %q, want failed", row.SeedStatus)
	}
}

// TestLaunchRejectsUnusableDirs guards spec §12's second defect: `tmux
// new-session -c <nonexistent>` exits 0 and silently starts the pane in $HOME,
// so a stale path would otherwise run a real agent against the wrong tree.
func TestLaunchRejectsUnusableDirs(t *testing.T) {
	l, dir := testLauncher(t)
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(dir, "vanished")

	cases := []struct {
		name string
		r    Recipe
	}{
		{"missing cwd", Recipe{ProjectLabel: "p", Cwd: gone}},
		{"empty cwd", Recipe{ProjectLabel: "p"}},
		{"cwd is a file", Recipe{ProjectLabel: "p", Cwd: file}},
		{"missing add-dir", Recipe{ProjectLabel: "p", Cwd: dir, AddDirs: []string{gone}}},
		{"add-dir is a file", Recipe{ProjectLabel: "p", Cwd: dir, AddDirs: []string{file}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, err := l.Tmux.ListSessions()
			if err != nil {
				t.Fatal(err)
			}
			name, err := l.Launch(c.r, 80, 24, time.Now())
			if err == nil {
				t.Fatalf("Launch(%+v) = %q, nil; want an error", c.r, name)
			}
			if name != "" {
				t.Fatalf("Launch returned a session name %q on a rejected launch", name)
			}
			after, err := l.Tmux.ListSessions()
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("tmux sessions %d → %d: a rejected launch must create none", len(before), len(after))
			}
			if rows, err := l.Store.Live(); err == nil && len(rows) != 0 {
				t.Fatalf("rejected launch wrote %d rows", len(rows))
			}
		})
	}
}

// TestLaunchPersistsAddDirsAndResumeRoundTrips is the §5 end-to-end: the
// launch argv carries --add-dir, the row carries the JSON, and the resumed
// session's argv carries the same dirs (--resume does not restore them, so
// dropping them here is exactly the invisible single-repo regression §5
// describes).
func TestLaunchPersistsAddDirsAndResumeRoundTrips(t *testing.T) {
	l, dir := testLauncher(t)
	sib1, sib2 := filepath.Join(dir, "sib1"), filepath.Join(dir, "sib2")
	for _, d := range []string{sib1, sib2} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := Recipe{ProjectLabel: "p", Cwd: dir, Model: "opus", AddDirs: []string{sib1, sib2}}
	name, err := l.Launch(r, 80, 24, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	row, ok, _ := l.Store.Get(name)
	if !ok {
		t.Fatal("no row")
	}
	if want := `["` + sib1 + `","` + sib2 + `"]`; row.AddDirs != want {
		t.Fatalf("row.AddDirs = %q, want %q", row.AddDirs, want)
	}

	row.EndedAt, row.ExitCode, row.LastStatus = 5, 0, "done"
	if err := l.Store.Upsert(row); err != nil {
		t.Fatal(err)
	}
	rname, err := l.Resume(row, 80, 24, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rrow, ok, _ := l.Store.Get(rname)
	if !ok || rrow.AddDirs != row.AddDirs {
		t.Fatalf("resumed row.AddDirs = %q, want %q", rrow.AddDirs, row.AddDirs)
	}
	cmd := ResumeShellCommand(rrow.ClaudeSessionID, DecodeAddDirs(rrow.AddDirs))
	for _, d := range []string{sib1, sib2} {
		if !strings.Contains(cmd, "'--add-dir' '"+d+"'") {
			t.Fatalf("resume command lost %q: %s", d, cmd)
		}
	}
}

// TestResumeFiltersVanishedAddDirs: a moved repo degrades to a session that
// visibly no longer lists it, rather than failing the resume outright or
// re-passing a path claude would reject.
func TestResumeFiltersVanishedAddDirs(t *testing.T) {
	l, dir := testLauncher(t)
	alive := filepath.Join(dir, "alive")
	if err := os.Mkdir(alive, 0o755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(dir, "moved-away")

	old := store.SessionRow{Name: "loom-old3", ClaudeSessionID: "ffffffff-ffff-ffff-ffff-ffffffffffff",
		ProjectLabel: "p", Cwd: dir, AddDirs: `["` + alive + `","` + gone + `"]`,
		CreatedAt: 1, EndedAt: 5, ExitCode: 0, LastStatus: "done"}
	if err := l.Store.Upsert(old); err != nil {
		t.Fatal(err)
	}
	name, err := l.Resume(old, 80, 24, time.Now())
	if err != nil {
		t.Fatalf("Resume = %v; a vanished add-dir must degrade, not fail the resume", err)
	}
	row, ok, _ := l.Store.Get(name)
	if !ok {
		t.Fatal("no resumed row")
	}
	if want := `["` + alive + `"]`; row.AddDirs != want {
		t.Fatalf("resumed AddDirs = %q, want %q (vanished dir dropped)", row.AddDirs, want)
	}
}

// TestResumeRejectsVanishedCwd: unlike an add-dir, the primary repo cannot be
// dropped — tmux would silently start the agent in $HOME.
func TestResumeRejectsVanishedCwd(t *testing.T) {
	l, dir := testLauncher(t)
	old := store.SessionRow{Name: "loom-old4", ClaudeSessionID: "abababab-abab-abab-abab-abababababab",
		ProjectLabel: "p", Cwd: filepath.Join(dir, "gone"), CreatedAt: 1, EndedAt: 5,
		ExitCode: 0, LastStatus: "done"}
	if _, err := l.Resume(old, 80, 24, time.Now()); err == nil {
		t.Fatal("Resume with a vanished cwd must error")
	}
}

// TestSelectCursorPatternMatchesAnyNumberedDialog pins spec §5's generic
// hardening. The allowlist it replaced (TrustMarker's exact wording) would let
// every row in the "dialog" group below through as ready, because each one
// contains the bare ReadyMarker glyph and none of them says "Do you trust…" —
// and seedWhenReady would then type the seed into an open dialog.
func TestSelectCursorPatternMatchesAnyNumberedDialog(t *testing.T) {
	re := selectCursorPattern(DefaultReadyMarker)
	for _, tc := range []struct {
		name   string
		pane   string
		dialog bool
	}{
		{"trust dialog", "Do you trust the files in this folder?\n❯ 1. Yes, proceed\n  2. No", true},
		{"add-dir dialog", "Add /Users/x/ballista as a working directory?\n❯ 1. Yes\n  2. No", true},
		{"paren option", "Select a model\n ❯ 2) Sonnet", true},
		{"indented cursor", "    ❯ 10. Something", true},
		{"double digit", "❯ 12. Option", true},
		{"ready prompt", "some output\n❯ ", false},
		{"ready prompt with typed text", "❯ fix the build", false},
		{"pre-mount", "loading…", false},
		{"plain numbered list", "  1. not a cursor\n  2. either", false},
		// A glyph on one line and a number on the next is two separate lines,
		// not a cursor — \s would have spanned the newline and called it one.
		{"glyph then numbered line", "❯\n1. list item", false},
	} {
		if got := re.MatchString(tc.pane); got != tc.dialog {
			t.Errorf("%s: selectCursorPattern match = %v, want %v", tc.name, got, tc.dialog)
		}
	}
	if selectCursorPattern("") != nil {
		t.Error(`selectCursorPattern("") must be nil: an empty marker would call every numbered list a dialog`)
	}
}

// TestWaitReadyHoldsForANonTrustDialog is the behavioural half: a dialog whose
// wording TrustMarker does not know (the `--add-dir` prompt §5 warns about)
// must still hold the seed, and must still be bounded by TrustTimeout rather
// than pinning the goroutine forever.
func TestWaitReadyHoldsForANonTrustDialog(t *testing.T) {
	l, dir := testLauncher(t)
	l.ReadyTimeout = 10 * time.Second // the dialog bound must be what fires
	l.TrustTimeout = 300 * time.Millisecond
	l.PollEvery = 50 * time.Millisecond

	id := NewSessionID()
	name := TmuxName(id)
	if err := l.Tmux.NewSession(name, dir,
		`sh -c 'echo "Add /tmp/other as a working directory?"; echo "❯ 1. Yes"; echo "  2. No"; sleep 60'`,
		80, 24); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- l.waitReady(name) }()
	select {
	case ready := <-done:
		if ready {
			t.Fatal("waitReady = true with a numbered select dialog still showing (spec §5 seed corruption)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitReady never returned: the dialog-pending wait is unbounded")
	}
}

// TestLaunchStoresPhysicalCwd pins the finding from
// docs/spikes/2026-07-22-add-dir-spike.md: claude derives its transcript
// DIRECTORY from the physical cwd (getcwd() resolves symlinks), so a stored
// unresolved cwd makes transcript.Path() look somewhere claude never wrote —
// the session then sits at `unknown` forever with no title and no context
// gauge. Against the pre-fix code this fails with the /symlink/ form.
func TestLaunchStoresPhysicalCwd(t *testing.T) {
	l, dir := testLauncher(t)

	real := filepath.Join(dir, "real")
	sibReal := filepath.Join(dir, "sibreal")
	for _, d := range []string{real, sibReal} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link, sibLink := filepath.Join(dir, "link"), filepath.Join(dir, "siblink")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sibReal, sibLink); err != nil {
		t.Fatal(err)
	}

	name, err := l.Launch(Recipe{ProjectLabel: "p", Cwd: link, AddDirs: []string{sibLink}}, 80, 24, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	row, ok, _ := l.Store.Get(name)
	if !ok {
		t.Fatal("no row")
	}
	if row.Cwd != real {
		t.Errorf("row.Cwd = %q, want the physical path %q", row.Cwd, real)
	}
	if want := `["` + sibReal + `"]`; row.AddDirs != want {
		t.Errorf("row.AddDirs = %q, want %q", row.AddDirs, want)
	}

	// The point of resolving: the transcript path Loom will poll must be the
	// one claude actually writes, which is derived from the physical cwd.
	if got, want := transcript.ProjectDirName(row.Cwd), transcript.ProjectDirName(real); got != want {
		t.Errorf("ProjectDirName(row.Cwd) = %q, want %q", got, want)
	}
}
