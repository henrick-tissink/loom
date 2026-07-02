package status

import (
	"fmt"
	"os"
	"path/filepath"
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
