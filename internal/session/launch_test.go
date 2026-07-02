package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

// fakeClaudeScript prints a trust dialog for 1s — including the "❯ 1. Yes,
// proceed" select-cursor line that ALSO contains the bare ReadyMarker glyph
// (finding 3 regression bait) — then clears and prints the real ready
// marker, then cats input to a sink file for assertion.
const fakeClaudeScript = `#!/bin/sh
echo "Do you trust the files in this folder?"
echo "❯ 1. Yes, proceed"
sleep 1
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
	dir := t.TempDir()
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
	script := filepath.Join(dir, "fake-claude.sh")
	os.WriteFile(script, []byte(fakeClaudeScript), 0o755)

	// launch manually with the fake command, then drive seedWhenReady directly
	id := NewSessionID()
	name := TmuxName(id)
	if err := l.Tmux.NewSession(name, dir, "'"+script+"' '"+sink+"'", 80, 24); err != nil {
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
