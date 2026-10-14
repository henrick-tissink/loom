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
	if r.Title != "probe the flux" || r.CtxTokens != 40010 {
		t.Fatalf("row = title %q ctx %d", r.Title, r.CtxTokens)
	}
	persisted, _, _ := st.Get(name)
	if persisted.Title != "probe the flux" {
		t.Fatalf("title not persisted: %+v", persisted)
	}
	// bell fires exactly once: running → needs_you. The payload is the
	// session name (the store PK), not a rendered label — consumers join
	// against snap.Live for the label (spec §4).
	if len(snap.NewlyNeedsYou) != 1 || snap.NewlyNeedsYou[0] != name {
		t.Fatalf("NewlyNeedsYou = %v", snap.NewlyNeedsYou)
	}
	// and the join is possible: the name resolves to the Live row carrying
	// both label and cwd.
	if snap.Live[0].Name != snap.NewlyNeedsYou[0] || snap.Live[0].ProjectLabel != "parallax" {
		t.Fatalf("NewlyNeedsYou name does not join to Live: %+v", snap.Live[0])
	}
	snap2, _ := e.Poll(time.Now().Add(time.Hour))
	if len(snap2.NewlyNeedsYou) != 0 {
		t.Fatalf("bell repeated: %v", snap2.NewlyNeedsYou)
	}
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

// TestNewlyNeedsYouDistinguishesSameBasenameCwds pins the reason
// NewlyNeedsYou carries names instead of labels (spec §4). Two repos named
// `checkout` under different parents both render ProjectLabel "checkout" —
// routine once one project owns several repos — so the old pre-rendered
// payload emitted two identical strings and no consumer could tell which
// session (or which project) each belonged to. With names, both entries are
// distinct and each joins back to a Live row carrying its own cwd.
func TestNewlyNeedsYouDistinguishesSameBasenameCwds(t *testing.T) {
	tm, st, ccd := testEnv(t)
	root := t.TempDir()

	cases := []struct{ parent, id string }{
		{"alpha", "cccccccc-cccc-cccc-cccc-cccccccccc01"},
		{"beta", "cccccccc-cccc-cccc-cccc-cccccccccc02"},
	}
	wantCwds := map[string]string{} // session name → cwd
	for _, c := range cases {
		cwd := filepath.Join(root, c.parent, "checkout")
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		name := launchFake(t, tm, ccd, cwd, c.id)
		// same ProjectLabel for both: filepath.Base(cwd), exactly what orphan
		// adoption stores.
		st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: c.id,
			ProjectLabel: filepath.Base(cwd), Cwd: cwd, CreatedAt: 1,
			EndedAt: -1, ExitCode: -1, LastStatus: "running"})
		wantCwds[name] = cwd
	}

	e := NewEngine(tm, st, ccd)
	snap, err := e.Poll(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.NewlyNeedsYou) != 2 {
		t.Fatalf("NewlyNeedsYou = %v, want 2 entries", snap.NewlyNeedsYou)
	}
	byName := map[string]Row{}
	for _, r := range snap.Live {
		byName[r.Name] = r
	}
	seen := map[string]bool{}
	for _, n := range snap.NewlyNeedsYou {
		if seen[n] {
			t.Fatalf("duplicate identity in NewlyNeedsYou: %v", snap.NewlyNeedsYou)
		}
		seen[n] = true
		row, ok := byName[n]
		if !ok {
			t.Fatalf("%q does not join to snap.Live: %v", n, snap.NewlyNeedsYou)
		}
		if row.Cwd != wantCwds[n] {
			t.Fatalf("%q joined to cwd %q, want %q", n, row.Cwd, wantCwds[n])
		}
		if row.ProjectLabel != "checkout" {
			t.Fatalf("%q label = %q, want the colliding basename", n, row.ProjectLabel)
		}
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

// A Finder/Dock-launched GUI has no locale vars, which used to make tmux
// vis()-sanitize the \t separators in -F output to '_': every session name
// came back mangled ("loom-<uuid>_1783938662"), every PaneStatus failed, and
// MarkLiveOrphansEnded retired EVERY live session on EVERY poll — sessions
// vanished from the rail, threads couldn't be opened, and needs-you
// notifications re-fired in an endless loop as other processes resurrected
// the rows. A poll in a locale-less environment must behave identically to
// one with a locale.
func TestPollWithoutLocaleKeepsLiveSessions(t *testing.T) {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	name := launchFake(t, tm, ccd, cwd, id)
	// CreatedAt 1 is far older than the orphan grace window, so a mangled
	// (empty) aliveNames list would retire this row immediately.
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id, ProjectLabel: "p",
		Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "running"})

	e := NewEngine(tm, st, ccd)
	snap, err := e.Poll(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.Live {
		if r.Name == name {
			return // still live, with its exact (unmangled) name
		}
	}
	row, ok, _ := st.Get(name)
	t.Fatalf("live tmux session retired by a locale-less poll: Live=%+v store row=%+v ok=%v",
		snap.Live, row, ok)
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

// TestPollRebindsClaudeSessionIDFromRegistry pins the fix for the frozen-row
// bug: a pane whose claude ran /clear (or an in-pane resume) carries a NEW
// session id and a NEW transcript, and nothing in the old transcript says
// so. The engine must follow claude's own registry
// (<ccd>/sessions/<pid>.json, keyed by tmux target), persist the new id and
// re-point the reader — proven here by the title that only the NEW
// transcript carries.
func TestPollRebindsClaudeSessionIDFromRegistry(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	oldID := "11111111-1111-4111-8111-111111111111"
	newID := "22222222-2222-4222-8222-222222222222"
	name := launchFake(t, tm, ccd, cwd, oldID)
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: oldID, ProjectLabel: "p",
		Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "unknown"})

	e := NewEngine(tm, st, ccd)
	now := time.Now().Add(time.Hour)
	if _, err := e.Poll(now); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := st.Get(name); row.ClaudeSessionID != oldID {
		t.Fatalf("before rebind: ClaudeSessionID = %q, want %q", row.ClaudeSessionID, oldID)
	}

	// claude ran /clear: a new transcript appears and the registry now
	// reports the new id for this pane.
	newPath := transcript.Path(ccd, cwd, newID)
	if err := os.WriteFile(newPath, []byte(fakeTranscriptTitled), 0o644); err != nil {
		t.Fatal(err)
	}
	regDir := filepath.Join(ccd, "sessions")
	if err := os.MkdirAll(regDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":%q,"tmux":"%s:@1.%%1","status":"idle","updatedAt":1}`,
		os.Getpid(), newID, cwd, name)
	if err := os.WriteFile(filepath.Join(regDir, fmt.Sprintf("%d.json", os.Getpid())), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, err := e.Poll(now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	row, ok, _ := st.Get(name)
	if !ok || row.ClaudeSessionID != newID {
		t.Fatalf("after rebind: ClaudeSessionID = %q, want %q", row.ClaudeSessionID, newID)
	}
	if len(snap.Live) != 1 || snap.Live[0].ClaudeSessionID != newID {
		t.Fatalf("snapshot not rebound: %+v", snap.Live)
	}
	if snap.Live[0].Title != "probe the flux" {
		t.Fatalf("reader not re-pointed at the new transcript: Title = %q", snap.Live[0].Title)
	}

	// a registry entry that agrees with the row is a no-op on later polls
	if _, err := e.Poll(now.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := st.Get(name); row.ClaudeSessionID != newID {
		t.Fatalf("rebind not stable: %q", row.ClaudeSessionID)
	}
}

// TestPollMergesRegistryStatus: claude's own registry says the pane is busy
// (a parent waiting on subagents), so the transcript's ended turn must not
// read as needs_you; "waiting" names what the user has to answer.
func TestPollMergesRegistryStatus(t *testing.T) {
	tm, st, ccd := testEnv(t)
	cwd := t.TempDir()
	id := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	name := launchFake(t, tm, ccd, cwd, id)
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id, ProjectLabel: "p",
		Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "unknown"})
	regDir := filepath.Join(ccd, "sessions")
	if err := os.MkdirAll(regDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(status, waitingFor string) {
		body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"tmux":%q,"status":%q,"waitingFor":%q,"updatedAt":1}`,
			os.Getpid(), id, name+":@1.%1", status, waitingFor)
		if err := os.WriteFile(filepath.Join(regDir, "1.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := NewEngine(tm, st, ccd)
	now := time.Now().Add(time.Hour)

	write("busy", "")
	snap, err := e.Poll(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Live) != 1 || snap.Live[0].Status != Running || snap.Live[0].WaitingFor != "" {
		t.Fatalf("busy: got %+v", snap.Live)
	}
	if len(snap.NewlyNeedsYou) != 0 {
		t.Fatalf("busy must not ring the bell: %v", snap.NewlyNeedsYou)
	}

	write("waiting", "dialog open")
	snap, err = e.Poll(now)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Live[0].Status != NeedsYou || snap.Live[0].WaitingFor != "dialog open" {
		t.Fatalf("waiting: got %+v", snap.Live[0])
	}
	if len(snap.NewlyNeedsYou) != 1 {
		t.Fatalf("waiting must ring once: %v", snap.NewlyNeedsYou)
	}
}

const fakeTranscriptAsk = `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","timestamp":"2026-10-14T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Done. Merge it?"}],"stop_reason":"end_turn"}}
`

func TestPollCarriesAskTaskAndCheckpoint(t *testing.T) {
	tm, st, ccd := testEnv(t)
	t.Setenv("CLAUDE_STATE_DIR", filepath.Join(ccd, "state"))
	cwd := t.TempDir()
	id := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	name := launchFake(t, tm, ccd, cwd, id)
	if err := os.WriteFile(transcript.Path(ccd, cwd, id), []byte(fakeTranscriptAsk), 0o644); err != nil {
		t.Fatal(err)
	}
	st.Upsert(store.SessionRow{Name: name, ClaudeSessionID: id, ProjectLabel: "p",
		Cwd: cwd, CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "unknown"})
	for _, d := range []string{"tasks", "checkpoints"} {
		if err := os.MkdirAll(filepath.Join(ccd, "state", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	task := "# Task\nStatus: open\n\n## Outcome\nThe rail shows it.\n\n## Done when\n- [x] build: $ true\n- [ ] tests: $ true\n"
	if err := os.WriteFile(filepath.Join(ccd, "state", "tasks", id+".md"), []byte(task), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ccd, "state", "checkpoints", id+".md"), []byte("# Checkpoint\n\n## Next step\nhand back\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewEngine(tm, st, ccd)
	snap, err := e.Poll(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Live) != 1 {
		t.Fatalf("live = %+v", snap.Live)
	}
	r := snap.Live[0]
	if r.Status != NeedsYou || r.Ask != "Done. Merge it?" || r.Since != 1791972000 {
		t.Fatalf("ask: %+v", r)
	}
	if !r.HasTask || r.Task.Status != "open" || r.Task.Done != 1 || r.Task.Total != 2 || r.Task.Outcome != "The rail shows it." {
		t.Fatalf("task: %+v", r.Task)
	}
	if r.Checkpoint.WrittenAt == 0 || r.Checkpoint.NextStep != "hand back" {
		t.Fatalf("checkpoint: %+v", r.Checkpoint)
	}
}
