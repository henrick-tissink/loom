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

// fakeClaudeScript prints a trust dialog for 1s, then the ready marker
// (spike-verified "❯" prompt glyph, not the unverified "? for shortcuts"
// candidate), then cats input to a sink file for assertion.
const fakeClaudeScript = `#!/bin/sh
echo "Do you trust the files in this folder?"
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
	done := make(chan struct{})
	go func() { l.seedWhenReady(name, "hello seed"); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("seed goroutine never finished")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(sink)
		if string(b) == "hello seed\n" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink = %q, want seed after ready marker", b)
		}
		time.Sleep(100 * time.Millisecond)
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
