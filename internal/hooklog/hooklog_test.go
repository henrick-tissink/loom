package hooklog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLog(t *testing.T, path string, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

const (
	deny1  = `{"ts": "2026-10-10T10:00:00", "hook": "bash_guard", "decision": "deny_no_deadline", "event": "PreToolUse", "session": "s1", "agent_type": null, "cwd": "/w/a", "command": "sleep 5"}`
	deny2  = `{"ts": "2026-10-10T10:01:00", "hook": "bash_guard", "decision": "deny_no_deadline", "event": "PreToolUse", "session": "s1", "agent_type": null, "cwd": "/w/a", "command": "sleep 6"}`
	deny3  = `{"ts": "2026-10-10T10:02:00", "hook": "bash_guard", "decision": "deny_no_deadline", "event": "PreToolUse", "session": "s1", "agent_type": null, "cwd": "/w/a", "command": "sleep 7"}`
	nudge  = `{"ts": "2026-10-10T10:03:00", "hook": "ctx_budget", "decision": "nudge_task_file", "event": "UserPromptSubmit", "session": "s1", "agent_type": null, "cwd": "/w/a"}`
	sub    = `{"ts": "2026-10-10T10:04:00", "hook": "stop_guard", "decision": "block_unaccounted_tasks", "event": "SubagentStop", "session": "s1", "agent_type": "Explore", "cwd": "/w/a", "count": 2}`
	stopOK = `{"ts": "2026-10-10T10:05:00", "hook": "stop_guard", "decision": "allow_done_checks_passed", "event": "Stop", "session": "s1", "agent_type": null, "cwd": "/w/a", "commands": 2}`
	test1  = `{"ts": "2026-10-10T10:06:00", "hook": "test_rerun", "decision": "log_test_first", "event": "PreToolUse", "session": "s2", "agent_type": null, "cwd": "/w/b", "command": "go test ./...", "same_cmd": false, "changed": "first", "since_last_s": null, "run_no": 1, "git": true}`
	test2  = `{"ts": "2026-10-10T10:07:00", "hook": "test_rerun", "decision": "log_test_rerun_no_change", "event": "PreToolUse", "session": "s2", "agent_type": null, "cwd": "/w/b", "command": "go test ./...", "same_cmd": true, "changed": "no", "since_last_s": 60, "run_no": 2, "git": true}`
	test3  = `{"ts": "2026-10-10T10:09:00", "hook": "test_rerun", "decision": "log_test_rerun_no_change", "event": "PreToolUse", "session": "s2", "agent_type": null, "cwd": "/w/b", "command": "go test ./pkg", "same_cmd": false, "changed": "no", "since_last_s": 120, "run_no": 3, "git": true}`
	r3deny = `{"ts": "2026-10-15T16:00:00", "hook": "bash_guard", "decision": "deny_detach", "event": "PreToolUse", "session": "s3", "agent_type": null, "cwd": "/w/a", "rev": "r3", "command": "nohup x &"}`
	allow1 = `{"ts": "2026-10-10T10:00:30", "hook": "bash_guard", "event": "PreToolUse", "session": "s1", "bg": false}`
	allow2 = `{"ts": "2026-10-15T16:00:30", "hook": "bash_guard", "event": "PreToolUse", "session": "s3", "rev": "r3", "bg": false}`
	allowS = `{"ts": "2026-10-10T10:05:30", "hook": "stop_guard", "event": "Stop", "session": "s1", "tasks": 0, "claim": false}`
)

func ts(s string) time.Time {
	t, _ := time.ParseInLocation(tsLayout, s, time.Local)
	return t
}

func TestParse(t *testing.T) {
	r, ok := Parse([]byte(test2), false)
	if !ok || r.Hook != "test_rerun" || r.Changed != "no" || !r.SameCmd || r.SinceLast != 60 || r.Class() != "test" {
		t.Fatalf("parse: %+v %v", r, ok)
	}
	a, ok := Parse([]byte(allow1), true)
	if !ok || !a.Silent || a.Class() != "silent" || a.Cwd != "" {
		t.Fatalf("allow: %+v", a)
	}
	if _, ok := Parse([]byte(`{"hook": "x"}`), false); ok {
		t.Fatal("no ts must not parse")
	}
	if _, ok := Parse([]byte(`not json`), false); ok {
		t.Fatal("garbage must not parse")
	}
	long := strings.Replace(deny1, "sleep 5", strings.Repeat("x", 2000), 1)
	if r, _ := Parse([]byte(long), false); len(r.Command) != MaxCommand {
		t.Fatalf("command not capped: %d", len(r.Command))
	}
	for dec, want := range map[string]string{"deny_x": "hold", "block_x": "hold", "nudge_x": "nudge", "allow_x": "info", "inject_checkpoint": "info"} {
		if got := (Record{Decision: dec}).Class(); got != want {
			t.Errorf("%s: %s, want %s", dec, got, want)
		}
	}
}

func TestReaderIncrementalAndTruncation(t *testing.T) {
	dir := t.TempDir()
	dec := filepath.Join(dir, "decisions.jsonl")
	writeLog(t, dec, deny1, "garbage line", deny2)
	writeLog(t, filepath.Join(dir, "allows.jsonl"), allow1)
	r := New(dir)
	r.Refresh()
	if n := len(r.Records()); n != 3 {
		t.Fatalf("first read: %d records", n)
	}
	// A partial line is not read until its newline lands.
	appendLog(t, dec, deny3[:20])
	r.Refresh()
	if n := len(r.Records()); n != 3 {
		t.Fatalf("partial line counted: %d", n)
	}
	appendLog(t, dec, deny3[20:]+"\n"+nudge+"\n")
	r.Refresh()
	recs := r.Records()
	if len(recs) != 5 || recs[4].Decision != "nudge_task_file" {
		t.Fatalf("incremental: %d %+v", len(recs), recs[len(recs)-1])
	}
	// Order is by timestamp across both files.
	if recs[0].Decision != "deny_no_deadline" || !recs[1].Silent {
		t.Fatalf("merge order: %+v", recs[:2])
	}
	// Truncation restarts the file.
	writeLog(t, dec, stopOK)
	r.Refresh()
	recs = r.Records()
	if len(recs) != 2 || recs[1].Decision != "allow_done_checks_passed" {
		t.Fatalf("after truncation: %d %+v", len(recs), recs)
	}
	// Rotation: a new inode at the same size.
	if err := os.Remove(dec); err != nil {
		t.Fatal(err)
	}
	writeLog(t, dec, test1)
	r.Refresh()
	recs = r.Records()
	if len(recs) != 2 || recs[1].Hook != "test_rerun" {
		t.Fatalf("after rotation: %+v", recs)
	}
	// A vanished file drops its records.
	if err := os.Remove(dec); err != nil {
		t.Fatal(err)
	}
	r.Refresh()
	if recs = r.Records(); len(recs) != 1 || !recs[0].Silent {
		t.Fatalf("after removal: %+v", recs)
	}
}

func TestReaderLegacyReadOnce(t *testing.T) {
	dir, legacy := t.TempDir(), t.TempDir()
	writeLog(t, filepath.Join(legacy, "decisions.jsonl"), deny1)
	r := New(dir)
	r.AddLegacyDir(legacy)
	r.AddLegacyDir(legacy) // idempotent
	r.Refresh()
	recs := r.Records()
	if len(recs) != 1 || !recs[0].Legacy {
		t.Fatalf("legacy: %+v", recs)
	}
	appendLog(t, filepath.Join(legacy, "decisions.jsonl"), deny2+"\n")
	r.Refresh()
	if len(r.Records()) != 1 {
		t.Fatal("legacy dir must not be re-read")
	}
	if got := r.BySession("s1"); len(got) != 1 {
		t.Fatalf("by session: %d", len(got))
	}
	if got := r.BySession(""); got != nil {
		t.Fatal("empty sid must give nothing")
	}
}

func parseAll(t *testing.T, silent bool, lines ...string) []Record {
	t.Helper()
	var out []Record
	for _, l := range lines {
		r, ok := Parse([]byte(l), silent)
		if !ok {
			t.Fatalf("bad fixture: %s", l)
		}
		out = append(out, r)
	}
	return out
}

func TestSessionStatsAndHeld(t *testing.T) {
	recs := parseAll(t, false, deny1, deny2, deny3, nudge, sub, stopOK)
	recs = append(recs, parseAll(t, true, allow1, allowS)...)
	st := Session(recs, time.Time{})
	// SubagentStop holds count on the parent session; the allow_* and silent lines do not.
	if st.Holds != 4 || st.Nudges != 1 || st.Held == nil || st.Held.Decision != "deny_no_deadline" || st.Held.Count != 3 || st.Held.Command != "sleep 7" {
		t.Fatalf("stats: %+v held %+v", st, st.Held)
	}
	if st.LastStop == nil || st.LastStop.Decision != "allow_done_checks_passed" {
		t.Fatalf("last stop: %+v", st.LastStop)
	}
	if len(st.Groups) != 2 || st.Groups[0].Count != 3 || st.Groups[1].Hook != "stop_guard" {
		t.Fatalf("groups: %+v", st.Groups)
	}
	// A task started after two of the denies resets the count below the threshold.
	st = Session(recs, ts("2026-10-10T10:01:30"))
	if st.Holds != 2 || st.Held != nil {
		t.Fatalf("since task start: holds %d held %+v", st.Holds, st.Held)
	}
	cls := Classes(recs, time.Time{})
	want := map[string][2]int{"bash_guard|hold": {3, 4}, "ctx_budget|nudge": {1, 1}, "stop_guard|hold": {1, 3}}
	if len(cls) != len(want) {
		t.Fatalf("classes: %+v", cls)
	}
	for _, c := range cls {
		w, ok := want[c.Hook+"|"+c.Class]
		if !ok || c.Count != w[0] || c.Of != w[1] {
			t.Fatalf("class %+v, want %v", c, w)
		}
	}
}

func TestRatesSplitAtMarksNeverSums(t *testing.T) {
	recs := parseAll(t, false, deny1, deny2, deny3, r3deny)
	recs = append(recs, parseAll(t, true, allow1, allow2)...)
	now := ts("2026-10-16T12:00:00")
	mark := Mark{At: ts("2026-10-15T15:50:00"), Label: "setup r3"}
	rows := Rates(recs, time.Time{}, []Mark{mark}, now)
	if len(rows) != 1 || rows[0].Hook != "bash_guard" || rows[0].Rev != "r3" {
		t.Fatalf("rows: %+v", rows)
	}
	sp := rows[0].Spans
	if len(sp) != 2 {
		t.Fatalf("spans: %+v", sp)
	}
	if sp[0].Runs != 4 || sp[0].Holds != 3 || sp[0].Silent != 1 || sp[0].Mark != "" || !sp[0].To.Equal(mark.At) {
		t.Fatalf("span 0: %+v", sp[0])
	}
	if sp[1].Runs != 2 || sp[1].Holds != 1 || sp[1].Mark != "setup r3" || sp[1].Rev != "r3" || !sp[1].To.Equal(now) || sp[1].Decisions["deny_detach"] != 1 {
		t.Fatalf("span 1: %+v", sp[1])
	}
	if len(sp[0].Days) != 1 || sp[0].Days[0].Day != "2026-10-10" || sp[0].Days[0].Holds != 3 {
		t.Fatalf("days: %+v", sp[0].Days)
	}
	// "since change": only the last span.
	last, ok := LastMark([]Mark{mark})
	if !ok {
		t.Fatal("last mark")
	}
	rows = Rates(recs, last.At, []Mark{mark}, now)
	if len(rows) != 1 || len(rows[0].Spans) != 1 || rows[0].Spans[0].Runs != 2 || rows[0].Spans[0].Mark != "setup r3" || !rows[0].Spans[0].From.Equal(mark.At) {
		t.Fatalf("since change: %+v", rows)
	}
	// A window opening after the mark still names the change it sits after.
	rows = Rates(recs, mark.At.Add(time.Minute), []Mark{mark}, now)
	if len(rows) != 1 || rows[0].Spans[0].Mark != "setup r3" || !rows[0].Spans[0].From.Equal(mark.At.Add(time.Minute)) {
		t.Fatalf("window after mark: %+v", rows)
	}
	// A window with no records gives no rows.
	if rows = Rates(recs, now, nil, now); len(rows) != 0 {
		t.Fatalf("empty window: %+v", rows)
	}
}

func TestInferredMarks(t *testing.T) {
	legacy := t.TempDir()
	// The owner's hand edits: backups named with stamps, and the .py mtimes.
	for _, n := range []string{"bash_guard.py.bak-20261013170807", "backup-20261013210359"} {
		if err := os.WriteFile(filepath.Join(legacy, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	py := filepath.Join(legacy, "stop_guard.py")
	if err := os.WriteFile(py, []byte("#"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := ts("2026-10-14T23:31:00")
	if err := os.Chtimes(py, edit, edit); err != nil {
		t.Fatal(err)
	}
	recs := parseAll(t, false, deny1, r3deny)
	solid := []Mark{{At: ts("2026-10-15T15:50:31"), Label: "setup local → r3"}}
	marks := InferredMarks(recs, []string{legacy}, solid, time.Hour)
	// 13 Oct 17:08 and 21:03 are hours apart: two marks; 14 Oct one mark;
	// the first r3 record (15 Oct 16:00) is within an hour of setup: dropped.
	if len(marks) != 3 {
		t.Fatalf("marks: %+v", marks)
	}
	if marks[0].At.Format("2006-01-02 15:04") != "2026-10-13 17:08" || marks[1].At.Format("2006-01-02 15:04") != "2026-10-13 21:03" || !marks[2].At.Equal(edit) {
		t.Fatalf("mark times: %+v", marks)
	}
	for _, m := range marks {
		if !m.Inferred {
			t.Fatalf("not inferred: %+v", m)
		}
	}
	// Without the ledger, the r3 record is a mark of its own.
	marks = InferredMarks(recs, nil, nil, time.Hour)
	if len(marks) != 1 || marks[0].Label != "first r3 record" {
		t.Fatalf("rev mark: %+v", marks)
	}
}

func TestReruns(t *testing.T) {
	recs := parseAll(t, false, test1, test2, test3, deny1)
	rows := Reruns(recs, time.Time{})
	if len(rows) != 1 {
		t.Fatalf("rows: %+v", rows)
	}
	p := rows[0]
	if p.Project != "/w/b" || p.Runs != 3 || p.First != 1 || p.NoChange != 2 || p.NoChangeSame != 1 || p.MedianGapS != 120 {
		t.Fatalf("row: %+v", p)
	}
	if len(p.Commands) != 2 || p.Commands[0] != "go test ./pkg" {
		t.Fatalf("commands: %+v", p.Commands)
	}
	if rows = Reruns(recs, ts("2026-10-10T10:08:00")); rows[0].Runs != 1 {
		t.Fatalf("window: %+v", rows)
	}
}
