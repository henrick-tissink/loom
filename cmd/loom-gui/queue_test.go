package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
)

func TestQueueLaunchesDueRunsAndHolds(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	if err := a.CreateProject(root, "Atlas", []string{root}); err != nil {
		t.Fatal(err)
	}
	a.queue = newQueueStore(filepath.Join(t.TempDir(), "queue.json"))
	var windows []UsageWindowDTO
	a.limits = func() []UsageWindowDTO { return windows }
	var launched []QueuedRun
	a.queueLaunch = func(r QueuedRun) (string, error) { launched = append(launched, r); return "loom-q" + r.ID[:4], nil }

	now := a.now()
	if err := a.QueueRun(root, "", "", "fix the flaky test", "", false, 0); err == nil {
		t.Fatal("a run without Done-when must be refused")
	}
	if err := a.QueueRun("/nowhere", "", "", "x", "tests pass", false, 0); err == nil {
		t.Fatal("an unknown target must be refused")
	}
	if err := a.QueueRun(root, "", "", "fix the flaky test", "flaky: $ go test ./x -count=20", false, 0); err != nil {
		t.Fatal(err)
	}
	later := a.now().Unix() + 3600
	if err := a.QueueRun(root, "", "", "write docs", "docs: $ grep -q x README.md", true, later); err != nil {
		t.Fatal(err)
	}

	// a full window holds every due run
	windows = []UsageWindowDTO{{Name: "five_hour", UsedPct: 97, ResetsAt: now.Unix() + 600}}
	a.queueTick(now)
	if len(launched) != 0 || !strings.Contains(a.Queue().Held, "five_hour") {
		t.Fatalf("launched while full: %+v, held %q", launched, a.Queue().Held)
	}
	// the window resets: the room-run launches, the timed one waits
	a.queueTick(now.Add(11 * time.Minute))
	if len(launched) != 1 || launched[0].Seed != "fix the flaky test" {
		t.Fatalf("launched = %+v", launched)
	}
	// its time passed long ago (a laptop slept): it launches, marked late
	a.queueTick(time.Unix(later+7200, 0))
	if len(launched) != 2 || !launched[1].Worktree {
		t.Fatalf("launched = %+v", launched)
	}
	q := a.Queue()
	if q.Runs[0].LateBy != 7200 || q.Runs[0].Session == "" || q.Runs[1].LateBy != 0 {
		t.Fatalf("runs = %+v", q.Runs)
	}
	// nothing launches twice, and a launched run can't be cancelled
	a.queueTick(time.Unix(later+7300, 0))
	if len(launched) != 2 {
		t.Fatalf("relaunched: %+v", launched)
	}
	if err := a.CancelQueued(q.Runs[0].ID); err == nil {
		t.Fatal("cancelled a launched run")
	}
	// the state survives a restart
	if got := newQueueStore(a.queue.path).snapshot(); len(got.Runs) != 2 || got.Runs[1].LaunchedAt == 0 {
		t.Fatalf("reloaded = %+v", got)
	}
}

func TestQueueSeedCarriesDoneWhen(t *testing.T) {
	s := queueSeed("  do it ", "a: $ true")
	if !strings.HasPrefix(s, "do it\n\nDone when") || !strings.HasSuffix(s, "a: $ true") {
		t.Fatalf("seed = %q", s)
	}
}

func TestResumeAtReset(t *testing.T) {
	cfg, cwd := t.TempDir(), t.TempDir()
	a := newTestApp(t)
	a.launcher = &session.Launcher{ClaudeConfigDir: cfg}
	a.queue = newQueueStore("")
	var sent []string
	a.queueSend = func(name, text string) error { sent = append(sent, name+":"+text); return nil }
	sid := "22222222-2222-2222-2222-222222222222"
	a.st.Upsert(store.SessionRow{Name: "loom-l", ClaudeSessionID: sid, Cwd: cwd, CreatedAt: 1, EndedAt: -1, LastStatus: "idle"})
	tr := transcript.Path(cfg, cwd, sid)
	os.MkdirAll(filepath.Dir(tr), 0o755)
	reset := int64(1_800_000_000)
	limit := `{"type":"assistant","error":"rate_limit","isApiErrorMessage":true,"quotaLimits":{"resetsAt":1800000000,"rateLimitType":"five_hour"},"message":{"content":[{"type":"text","text":"You've hit your limit"}]}}`
	os.WriteFile(tr, []byte(`{"type":"user","message":{"content":"go"}}`+"\n"+limit+"\n"), 0o644)

	d := a.limitFor("loom-l")
	if d == nil || d.ResetsAt != reset || d.Armed {
		t.Fatalf("limit = %+v", d)
	}
	if err := a.ArmResume("loom-l", true); err != nil {
		t.Fatal(err)
	}
	if d := a.limitFor("loom-l"); d == nil || !d.Armed {
		t.Fatalf("not armed: %+v", d)
	}
	a.queueTick(time.Unix(reset+10, 0)) // inside the grace
	if len(sent) != 0 {
		t.Fatalf("sent before the grace: %v", sent)
	}
	// a dialog open at reset time delays the resume; it does not disarm it
	reg := filepath.Join(cfg, "sessions", "1.json")
	os.MkdirAll(filepath.Dir(reg), 0o755)
	os.WriteFile(reg, []byte(fmt.Sprintf(`{"pid":%d,"sessionId":"%s","cwd":"%s","tmux":"loom-l:@1.%%1","status":"waiting","waitingFor":"dialog open","updatedAt":1}`, os.Getpid(), sid, cwd)), 0o644)
	a.queueTick(time.Unix(reset+resumeGrace+1, 0))
	if len(sent) != 0 || a.Queue().Resumes[0].Err != "" || !a.Queue().Resumes[0].Armed {
		t.Fatalf("dialog: sent %v, resumes %+v", sent, a.Queue().Resumes)
	}
	os.Remove(reg)
	// a live session on an unreset limit holds the queue (no usage tap)
	if h := a.limitsFull(time.Unix(reset-10, 0)); !strings.Contains(h, "usage limit") {
		t.Fatalf("held = %q", h)
	}
	if h := a.limitsFull(time.Unix(reset+1, 0)); h != "" {
		t.Fatalf("held after reset = %q", h)
	}
	a.queueTick(time.Unix(reset+resumeGrace+31, 0))
	if len(sent) != 1 || sent[0] != "loom-l:continue" {
		t.Fatalf("sent = %v", sent)
	}
	a.queueTick(time.Unix(reset+600, 0))
	if len(sent) != 1 {
		t.Fatalf("sent twice: %v", sent)
	}
	if q := a.Queue().Sessions["loom-l"]; q.Status != "idle" {
		t.Fatalf("digest session = %+v", q)
	}

	// armed, but someone typed in the meantime: nothing is sent
	os.WriteFile(tr, []byte(limit+"\n"), 0o644)
	a.queue = newQueueStore("")
	if err := a.ArmResume("loom-l", true); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user","message":{"content":"carry on"}}` + "\n")
	f.Close()
	a.queueTick(time.Unix(reset+resumeGrace+1, 0))
	if len(sent) != 1 || a.Queue().Resumes[0].Err == "" {
		t.Fatalf("resumed a session that moved on: %v %+v", sent, a.Queue().Resumes)
	}
	// a session not on a limit can't be armed
	if err := a.ArmResume("loom-l", true); err == nil {
		t.Fatal("armed without a limit")
	}
}

func TestQueueHeldRunIsNotLate(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	if err := a.CreateProject(root, "Atlas", []string{root}); err != nil {
		t.Fatal(err)
	}
	a.queue = newQueueStore("")
	full := true
	a.limits = func() []UsageWindowDTO {
		if full {
			return []UsageWindowDTO{{Name: "seven_day", UsedPct: 99, ResetsAt: 1 << 40}}
		}
		return nil
	}
	a.queueLaunch = func(r QueuedRun) (string, error) { return "loom-x", nil }
	at := a.now().Unix() + 60
	if err := a.QueueRun(root, "", "", "s", "d", false, at); err != nil {
		t.Fatal(err)
	}
	// loom runs throughout, ticking every 30 s; the window holds it an hour
	for ts := at - 30; ts < at+3600; ts += 30 {
		a.queueTick(time.Unix(ts, 0))
	}
	full = false
	a.queueTick(time.Unix(at+3630, 0))
	r := a.Queue().Runs[0]
	if r.LaunchedAt == 0 || r.LateBy != 0 {
		t.Fatalf("run = %+v", r)
	}
}
