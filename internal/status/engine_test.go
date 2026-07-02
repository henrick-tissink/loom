package status

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
)

const fakeTranscript = `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"yo"}],"stop_reason":"end_turn"}}
{"type":"permission-mode","permissionMode":"default","sessionId":"x"}
`

func testEnv(t *testing.T) (*tmux.Client, *store.Store, string) {
	t.Helper()
	tm := &tmux.Client{Socket: fmt.Sprintf("loomeng%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return tm, st, t.TempDir() // third value = fake CLAUDE_CONFIG_DIR
}

// fakeClaude writes a transcript like claude would, then idles.
func launchFake(t *testing.T, tm *tmux.Client, ccd, cwd, id string) string {
	t.Helper()
	tpath := transcript.Path(ccd, cwd, id)
	if err := os.MkdirAll(filepath.Dir(tpath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tpath, []byte(fakeTranscript), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "loom-" + id
	if err := tm.NewSession(name, cwd, "sleep 60", 80, 24); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestPollLiveNeedsYou(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	name := launchFake(t, tm, ccd, cwd, id)
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id, ProjectLabel: "p",
		Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "unknown"})

	e := NewEngine(tm, st, ccd)
	// far-future "now" so session_activity from creation doesn't read as active
	snap, err := e.Poll(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Live) != 1 {
		t.Fatalf("Live = %+v", snap.Live)
	}
	// sidecar tail must not break NeedsYou (P0 guard at engine level)
	if snap.Live[0].Status != NeedsYou {
		t.Fatalf("Status = %v, want NeedsYou", snap.Live[0].Status)
	}
	if snap.Live[0].Activity <= 0 {
		t.Fatalf("Activity not threaded: %d", snap.Live[0].Activity)
	}
}

func TestPollDeadPaneClassifiesAndReaps(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	name := "loom-" + id
	if err := tm.NewSession(name, cwd, "exit 4", 80, 24); err != nil {
		t.Fatal(err)
	}
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id, ProjectLabel: "p",
		Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "running"})

	e := NewEngine(tm, st, ccd)
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, err := e.Poll(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Recent) == 1 {
			r := snap.Recent[0]
			if r.LastStatus != "error" || r.ExitCode != 4 {
				t.Fatalf("Recent = %+v (want error/4)", r)
			}
			if tm.HasSession(name) {
				t.Fatal("dead session not reaped")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never classified; snap=%+v", snap)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// TestPollDeadOrphanRecordedBeforeReap covers the case where a tmux session
// is found already-dead on the very first poll (e.g. loom just started and
// discovered a `loom-*` session left over from a prior run) with no store
// row backing it yet. It must be adopted into the store BEFORE it's reaped,
// so it leaves a history record instead of silently vanishing (spec §6
// "record before reap").
func TestPollDeadOrphanRecordedBeforeReap(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	name := "loom-" + id
	if err := tm.NewSession(name, cwd, "exit 5", 80, 24); err != nil {
		t.Fatal(err)
	}
	// deliberately no st.Upsert: this session has NO store row at all

	e := NewEngine(tm, st, ccd)
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, err := e.Poll(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Recent) == 1 {
			r := snap.Recent[0]
			if r.Name != name {
				t.Fatalf("Recent = %+v (want name %q)", r, name)
			}
			if r.LastStatus != "error" || r.ExitCode != 5 {
				t.Fatalf("Recent = %+v (want error/5)", r)
			}
			if tm.HasSession(name) {
				t.Fatal("dead orphan session not reaped")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan never adopted+classified; snap=%+v", snap)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func TestPollAdoptsOrphanAndRetiresVanished(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	launchFake(t, tm, ccd, cwd, id) // tmux session, NO store row → adopt
	st.Upsert(store.SessionRow{Name: "loom-gone", ClaudeSessionID: "gone",
		ProjectLabel: "p", Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1,
		LastStatus: "running"}) // store row, NO tmux → retire

	e := NewEngine(tm, st, ccd)
	if _, err := e.Poll(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	adopted, ok, _ := st.Get("loom-" + id)
	if !ok || adopted.ClaudeSessionID != id {
		t.Fatalf("orphan not adopted: %+v %v", adopted, ok)
	}
	gone, _, _ := st.Get("loom-gone")
	if gone.LastStatus != "done" {
		t.Fatalf("vanished row not retired: %+v", gone)
	}
}

// TestPollResurrectsTerminalRowWithLiveTmux guards finding 2b: a
// launch-vs-reconcile race can leave a store row stuck in a terminal status
// (done/error) while its tmux session is, in fact, still alive. tmux is the
// source of truth for liveness (spec §6) — Poll must flip such a row back to
// 'unknown' so it reappears in Live(), instead of hiding a live session
// forever behind a stale terminal status.
func TestPollResurrectsTerminalRowWithLiveTmux(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	name := launchFake(t, tm, ccd, cwd, id) // real tmux session, alive
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id, ProjectLabel: "p",
		Cwd: cwd, CreatedAt: 1, EndedAt: 5, ExitCode: 0, LastStatus: "done"}) // but store says terminal

	e := NewEngine(tm, st, ccd)
	snap, err := e.Poll(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range snap.Live {
		if r.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("terminal row with a live tmux session was not resurrected into Live: %+v", snap.Live)
	}
}

// TestPollConcurrentSafe guards finding 1: Engine.Poll must be safe to call
// concurrently (defense in depth — the UI's real fix is to never launch two
// overlapping poll loops, but the engine itself must not crash if it
// happens).
//
// `go test -race` alone does NOT give this test teeth: the store sits behind
// a single SQLite connection, and that connection's own internal
// serialization masks a deleted mu.Lock/Unlock in Poll — concurrent Polls
// still don't produce a detectable data race or crash on e.readers in
// practice, so the regression can slip through with -race clean. Instead,
// this test asserts mutual exclusion directly via Engine's test-only
// pollDepth/maxPollDepth atomic gauges (see engine.go): if two Polls ever
// run their critical sections concurrently, maxPollDepth will be >1.
func TestPollConcurrentSafe(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	name := launchFake(t, tm, ccd, cwd, id)
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id, ProjectLabel: "p",
		Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "unknown"})

	e := NewEngine(tm, st, ccd)
	const goroutines = 8
	const pollsEach = 5
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*pollsEach)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < pollsEach; j++ {
				if _, err := e.Poll(time.Now().Add(time.Hour)); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Poll error: %v", err)
	}

	if max := e.maxPollDepth.Load(); max != 1 {
		t.Fatalf("maxPollDepth = %d, want 1 (concurrent Poll critical sections overlapped — finding 1 regression)", max)
	}
}
