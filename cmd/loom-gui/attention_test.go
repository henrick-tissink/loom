package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/taskfile"
)

var attnNow = time.Unix(1_800_000_000, 0)

func fixedNow() time.Time { return attnNow }

func doneTask(mod int64) taskfile.Task { return taskfile.Task{Status: "done", Modified: mod} }

func TestAttentionReviewReady(t *testing.T) {
	a := newAttention("", fixedNow)
	mod := attnNow.Unix() - 3600
	if !a.reviewReady("s", doneTask(mod), true) {
		t.Fatal("a fresh done task is not ready for review")
	}
	for _, c := range []struct {
		name string
		tk   taskfile.Task
		has  bool
	}{
		{"no task file", taskfile.Task{}, false},
		{"open task", taskfile.Task{Status: "open", Modified: mod}, true},
		{"consultation", taskfile.Task{Status: "consultation", Modified: mod}, true},
		{"older than the window", doneTask(attnNow.Add(-reviewWindow - time.Hour).Unix()), true},
	} {
		if a.reviewReady("s", c.tk, c.has) {
			t.Errorf("%s: ready for review", c.name)
		}
	}
	if err := a.markReviewed("s", doneTask(mod), true); err != nil {
		t.Fatal(err)
	}
	if a.reviewReady("s", doneTask(mod), true) {
		t.Error("still ready after it was reviewed")
	}
	// the task file is rewritten (a new task, or more work): back again
	if !a.reviewReady("s", doneTask(attnNow.Unix()+60), true) {
		t.Error("a newer version of the task file is not ready")
	}
	if a.reviewReady("", doneTask(mod), true) {
		t.Error("ready with no session id")
	}
	var nilA *attention
	if nilA.reviewReady("s", doneTask(mod), true) || nilA.snoozed("s", 0) {
		t.Error("a nil store marks something")
	}
}

func TestAttentionSnoozeAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.json")
	a := newAttention(path, fixedNow)
	if err := a.setSnooze("s", true, 500); err != nil {
		t.Fatal(err)
	}
	if !a.snoozed("s", 500) || a.snoozed("s", 900) {
		t.Fatal("snooze is not tied to the ask it was set on")
	}
	if err := a.setSnooze("u", true, 0); err != nil || !a.snoozed("u", 0) {
		t.Fatal("an ask of unknown time cannot be snoozed")
	}
	if err := a.markReviewed("s", doneTask(100), true); err != nil {
		t.Fatal(err)
	}
	b := newAttention(path, fixedNow)
	if !b.snoozed("s", 500) || b.reviewReady("s", doneTask(100), true) {
		t.Fatalf("marks not kept: %+v", b.marks)
	}
	if err := b.setSnooze("u", false, 0); err != nil || b.snoozed("u", 0) {
		t.Fatal("unsnooze failed")
	}
	if _, ok := b.marks["u"]; ok {
		t.Error("an empty mark is kept")
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := newAttention(path, fixedNow); c.marks == nil || c.snoozed("s", 500) {
		t.Error("a broken file is not read as empty")
	}
}

func attnRow(name, sid string, since int64, tk taskfile.Task, has bool) status.Row {
	return status.Row{SessionRow: store.SessionRow{Name: name, ClaudeSessionID: sid}, Since: since, Task: tk, HasTask: has}
}

func TestAttentionApplyLiveAndCount(t *testing.T) {
	a := newAttention("", fixedNow)
	mod := attnNow.Unix() - 60
	_ = a.setSnooze("s1", true, 700)
	dtos := []SessionDTO{
		{Name: "asking", Status: "needs_you"},
		{Name: "snoozed", Status: "needs_you"},
		{Name: "newer ask", Status: "needs_you"},
		{Name: "done", Status: "needs_you"},
		{Name: "still running", Status: "running"},
	}
	rows := []status.Row{
		attnRow("asking", "s0", 100, taskfile.Task{}, false),
		attnRow("snoozed", "s1", 700, taskfile.Task{}, false),
		attnRow("newer ask", "s1", 800, taskfile.Task{}, false), // same id, a newer ask
		attnRow("done", "s2", 900, doneTask(mod), true),
		attnRow("still running", "s3", 0, doneTask(mod), true),
	}
	a.applyLive(dtos, rows)
	want := map[string][2]bool{ // snoozed, ready
		"asking": {false, false}, "snoozed": {true, false}, "newer ask": {false, false},
		"done": {false, true}, "still running": {false, false},
	}
	for _, d := range dtos {
		if w := want[d.Name]; d.Snoozed != w[0] || d.ReviewReady != w[1] {
			t.Errorf("%s: snoozed %v ready %v", d.Name, d.Snoozed, d.ReviewReady)
		}
	}
	if n := needsYouCount(dtos); n != 3 {
		t.Errorf("needsYouCount = %d, want 3 (the snoozed ask is left out)", n)
	}
}

func writeTask(t *testing.T, dir, sid, statusWord string) {
	t.Helper()
	p := filepath.Join(dir, "tasks", sid+".md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Task\nSession: " + sid + "\nStatus: " + statusWord + "\n\n## Outcome\nx\n\n## Done when\n- [x] a: done\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAttentionFinishedAndMarkReviewed(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "sid-done", "done")
	writeTask(t, dir, "sid-open", "open")
	tasks := taskfile.NewCache(dir)
	a := &App{attn: newAttention("", time.Now), tasks: tasks}
	fin := []FinishedDTO{{Name: "f-done"}, {Name: "f-open"}, {Name: "f-none"}}
	sids := map[string]string{"f-done": "sid-done", "f-open": "sid-open", "f-none": "sid-none"}
	a.attn.applyFinished(fin, sids, tasks)
	if fin[0].TaskStatus != "done" || !fin[0].ReviewReady || fin[1].TaskStatus != "open" || fin[1].ReviewReady || fin[2].TaskStatus != "" || fin[2].ReviewReady {
		t.Fatalf("finished = %+v", fin)
	}
	// a live session is found on the rail; unknown names are skipped
	a.noteRail([]SessionDTO{{Name: "f-done", Status: "needs_you"}}, []status.Row{attnRow("f-done", "sid-done", 10, taskfile.Task{}, false)})
	if err := a.MarkReviewed([]string{"f-done", "nobody"}); err != nil {
		t.Fatal(err)
	}
	fin[0].ReviewReady = false
	a.attn.applyFinished(fin, sids, tasks)
	if fin[0].ReviewReady {
		t.Error("still ready after MarkReviewed")
	}
	if err := a.Snooze("f-done", true); err != nil || !a.attn.snoozed("sid-done", 10) {
		t.Fatalf("Snooze: %v", err)
	}
	a.noteRail([]SessionDTO{{Name: "untimed", Status: "needs_you"}}, []status.Row{attnRow("untimed", "sid-u", 0, taskfile.Task{}, false)})
	if err := a.Snooze("untimed", true); err == nil {
		t.Error("snoozed an ask with no time: it would never come back")
	}
	if err := a.Snooze("nobody", true); err == nil {
		t.Error("snoozed a session that is not live")
	}
	a.noteRail([]SessionDTO{{Name: "busy", Status: "running"}}, []status.Row{attnRow("busy", "sid-b", 0, taskfile.Task{}, false)})
	if err := a.Snooze("busy", true); err == nil {
		t.Error("snoozed a session that is not waiting")
	}
	if err := (&App{}).MarkReviewed([]string{"x"}); err == nil {
		t.Error("marks without a store")
	}
}
