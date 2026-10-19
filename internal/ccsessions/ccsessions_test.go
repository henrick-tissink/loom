package ccsessions

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEntry(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestByTmuxSessionMissingDir(t *testing.T) {
	got := ByTmuxSession(filepath.Join(t.TempDir(), "nope"))
	if len(got) != 0 {
		t.Fatalf("want empty map, got %v", got)
	}
}

func TestByTmuxSessionPicksFreshestLiveEntry(t *testing.T) {
	dir := t.TempDir()
	orig := pidAlive
	pidAlive = func(pid int) bool { return pid != 999 }
	t.Cleanup(func() { pidAlive = orig })

	// two live entries for the same pane: the newer updatedAt wins
	writeEntry(t, dir, "100.json", `{"pid":100,"sessionId":"s-old","tmux":"loom-A:@1.%1","status":"idle","updatedAt":10}`)
	writeEntry(t, dir, "101.json", `{"pid":101,"sessionId":"s-new","cwd":"/w","tmux":"loom-A:@1.%1","status":"waiting","waitingFor":"dialog open","name":"a-1","updatedAt":20,"statusUpdatedAt":15}`)
	// dead process: ignored even though it is the only entry for its pane
	writeEntry(t, dir, "999.json", `{"pid":999,"sessionId":"s-dead","tmux":"loom-B:@2.%2","updatedAt":30}`)
	// not under tmux: nothing to key on
	writeEntry(t, dir, "102.json", `{"pid":102,"sessionId":"s-plain","updatedAt":40}`)
	// malformed and non-json files contribute nothing
	writeEntry(t, dir, "103.json", `{"pid":103,`)
	writeEntry(t, dir, "notes.txt", `{"pid":104,"sessionId":"s-txt","tmux":"loom-C:@3.%3"}`)

	got := ByTmuxSession(dir)
	if len(got) != 1 {
		t.Fatalf("want exactly one pane, got %v", got)
	}
	e, ok := got["loom-A"]
	if !ok {
		t.Fatalf("want loom-A, got %v", got)
	}
	if e.SessionID != "s-new" || e.PID != 101 || e.Cwd != "/w" || e.TmuxTarget != "loom-A:@1.%1" ||
		e.Status != "waiting" || e.WaitingFor != "dialog open" || e.Name != "a-1" || e.UpdatedAt != 20 || e.StatusUpdatedAt != 15 {
		t.Fatalf("unexpected entry %+v", e)
	}
}

func TestListIncludesEntriesWithoutTmux(t *testing.T) {
	dir := t.TempDir()
	orig := pidAlive
	pidAlive = func(pid int) bool { return pid != 999 }
	t.Cleanup(func() { pidAlive = orig })
	writeEntry(t, dir, "101.json", `{"pid":101,"sessionId":"s-tmux","tmux":"loom-A:@1.%1","status":"idle","updatedAt":20}`)
	writeEntry(t, dir, "102.json", `{"pid":102,"sessionId":"s-plain","cwd":"/w","status":"waiting","startedAt":5000,"updatedAt":40}`)
	writeEntry(t, dir, "999.json", `{"pid":999,"sessionId":"s-dead","updatedAt":30}`)
	got := List(dir)
	if len(got) != 2 {
		t.Fatalf("want the two live entries, got %+v", got)
	}
	var plain *Entry
	for i := range got {
		if got[i].SessionID == "s-plain" {
			plain = &got[i]
		}
	}
	if plain == nil || plain.TmuxSession != "" || plain.Cwd != "/w" || plain.Status != "waiting" || plain.StartedAt != 5000 {
		t.Fatalf("plain entry = %+v", plain)
	}
	if List(filepath.Join(dir, "nope")) != nil {
		t.Fatal("missing dir → nil")
	}
}

func TestDir(t *testing.T) {
	if got := Dir("/home/x/.claude"); got != filepath.Join("/home/x/.claude", "sessions") {
		t.Fatalf("Dir = %q", got)
	}
}

func TestListReadsKindAndJobID(t *testing.T) {
	dir := t.TempDir()
	orig := pidAlive
	pidAlive = func(int) bool { return true }
	t.Cleanup(func() { pidAlive = orig })
	writeEntry(t, dir, "7.json", `{"pid":7,"sessionId":"s-bg","cwd":"/w","kind":"bg","jobId":"908c2218","status":"busy"}`)
	got := List(dir)
	if len(got) != 1 || got[0].Kind != "bg" || got[0].JobID != "908c2218" {
		t.Fatalf("entry = %+v", got)
	}
}
