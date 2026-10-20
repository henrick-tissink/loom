package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/hooklog"
)

var day = time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)

func rule(key string) Rule {
	return Rule{Text: "Read a file before you Write it.", Why: "Write failed, 21 times in 16 sessions.", Key: key, Since: "2026-10-20 11:30", Lapses: "2026-11-19"}
}

func TestFileRoundTrip(t *testing.T) {
	b, err := Add(nil, rule("tool/Write/aa"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "# Rules from repeated failures") {
		t.Fatalf("no header:\n%s", b)
	}
	b, err = Add(b, rule("hook/bash_guard/deny_detach"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "key tool/Write/aa\n\n-") {
		t.Fatalf("second rule not kept with the list:\n%s", b)
	}
	got := Parse(b)
	if len(got) != 2 || got[0] != rule("tool/Write/aa") || got[1].Key != "hook/bash_guard/deny_detach" {
		t.Fatalf("parse: %+v\n%s", got, b)
	}
	if _, err := Add(b, rule("tool/Write/aa")); err == nil {
		t.Fatal("a second rule for the same key was added")
	}

	// The person's own lines survive every edit.
	b = []byte(strings.Replace(string(b), "\n- Read", "\n- my own note, no why line\n\n- Read", 1))
	r, err := Renew(b, "tool/Write/aa", "2026-12-31")
	if err != nil {
		t.Fatal(err)
	}
	if x, _ := Find(r, "tool/Write/aa"); x.Lapses != "2026-12-31" || x.Since != "2026-10-20 11:30" {
		t.Fatalf("renew: %+v", x)
	}
	d, err := Remove(r, "tool/Write/aa")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(d), "tool/Write/aa") || strings.Count(string(d), "Read a file") != 1 || !strings.Contains(string(d), "- my own note, no why line") {
		t.Fatalf("remove took the wrong lines:\n%s", d)
	}
	if strings.Contains(string(d), "\n\n\n") {
		t.Fatalf("blank run left behind:\n%q", d)
	}
	// Blank runs elsewhere in the file are the person's and stay.
	own := []byte(string(d) + "\n\n\nmy notes\n")
	own, _ = Add(own, rule("tool/Read/cc"))
	if d2, _ := Remove(own, "tool/Read/cc"); !strings.Contains(string(d2), "\n\n\nmy notes") {
		t.Fatalf("remove tidied text it did not own:\n%q", d2)
	}
	if _, err := Remove(d, "nope"); err == nil {
		t.Fatal("removed a rule that is not there")
	}
}

func TestValidate(t *testing.T) {
	for name, mut := range map[string]func(*Rule){
		"empty":        func(r *Rule) { r.Text = "  " },
		"two lines":    func(r *Rule) { r.Text = "a\nb" },
		"heading":      func(r *Rule) { r.Text = "# a" },
		"import":       func(r *Rule) { r.Text = "see @docs/x.md" },
		"bad key":      func(r *Rule) { r.Key = "a key" },
		"bad date":     func(r *Rule) { r.Lapses = "soon" },
		"lapse before": func(r *Rule) { r.Lapses = "2026-10-01" },
	} {
		r := rule("k")
		mut(&r)
		if Validate(r) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := Validate(rule("k")); err != nil {
		t.Fatal(err)
	}
	if err := Validate(Rule{Text: "mail a@b.com", Key: "k", Since: "2026-10-20 09:00", Lapses: "2026-10-21"}); err != nil {
		t.Fatalf("an address is not an import: %v", err)
	}
	if w := CleanWhy("a\nb · c @x"); w != "a b - c at x" {
		t.Fatalf("CleanWhy = %q", w)
	}
}

func TestLapsed(t *testing.T) {
	r := rule("k")
	if r.Lapsed(time.Date(2026, 11, 19, 23, 0, 0, 0, time.Local)) {
		t.Fatal("lapsed on its lapse day")
	}
	if !r.Lapsed(time.Date(2026, 11, 20, 0, 1, 0, 0, time.Local)) {
		t.Fatal("not lapsed the day after")
	}
}

func ev(key, session string, at time.Time) Event {
	src := "tool"
	if strings.HasPrefix(key, "hook/") {
		src = "hook"
	}
	return Event{Key: key, Source: src, Title: key, Session: session, Cwd: "/w/" + session[:1], TS: at, Sample: "cmd " + session}
}

func TestMine(t *testing.T) {
	since, hookChange := day.Add(-Window), day.Add(-48*time.Hour)
	var evs []Event
	// 5 failures in 3 sessions: proposed.
	for i, s := range []string{"a1", "a1", "b1", "c1", "c1"} {
		evs = append(evs, ev("tool/Write/aa", s, day.Add(-time.Duration(5-i)*time.Hour)))
	}
	// 6 failures in 2 sessions: one stuck session is not a pattern.
	for i := 0; i < 6; i++ {
		evs = append(evs, ev("tool/Edit/bb", []string{"a1", "b1"}[i%2], day.Add(-time.Hour)))
	}
	// Hook holds, 5 in 3 sessions, but 3 before the last hook change.
	for i, s := range []string{"a1", "b1", "c1", "a1", "b1"} {
		at := day.Add(-time.Hour)
		if i < 3 {
			at = hookChange.Add(-time.Hour)
		}
		evs = append(evs, ev("hook/bash_guard/deny_no_deadline", s, at))
	}
	// Old failures outside the window.
	for _, s := range []string{"a1", "b1", "c1", "d1", "e1"} {
		evs = append(evs, ev("tool/Read/cc", s, since.Add(-time.Hour)))
	}
	got := Mine(evs, nil, since, hookChange)
	if len(got) != 1 || got[0].Key != "tool/Write/aa" || got[0].Count != 5 || got[0].Sessions != 3 {
		t.Fatalf("Mine = %+v", got)
	}
	p := got[0]
	if p.Samples[0] != "cmd c1" || len(p.Projects) != 3 || p.Projects[0] != "/w/a" && p.Projects[0] != "/w/c" {
		t.Fatalf("evidence: %+v", p)
	}
	if !strings.Contains(p.Why, "5 times in 3 sessions") {
		t.Fatalf("why: %q", p.Why)
	}
	// Without the hook change the holds qualify too (ties sort by key).
	if got := Mine(evs, nil, since, time.Time{}); len(got) != 2 || got[0].Key != "hook/bash_guard/deny_no_deadline" || got[0].Draft == "" {
		t.Fatalf("no mark: %+v", got)
	}
	// An adopted rule, lapsed or not, takes its key out of the proposals.
	if got := Mine(evs, []Rule{rule("tool/Write/aa")}, since, hookChange); len(got) != 0 {
		t.Fatalf("adopted key still proposed: %+v", got)
	}
}

func TestStatus(t *testing.T) {
	r := rule("tool/Write/aa") // since 2026-10-20 11:30
	since := time.Date(2026, 10, 20, 11, 30, 0, 0, time.Local)
	evs := []Event{
		ev("tool/Write/aa", "a1", since.Add(-24*time.Hour)),
		ev("tool/Write/aa", "b1", since.Add(-24*time.Hour)),
		ev("tool/Write/aa", "c1", since.Add(-Window-time.Hour)), // before the before-window
		ev("tool/Write/aa", "d1", since.Add(time.Hour)),
		ev("tool/Write/aa", "e1", since.Add(-time.Minute)), // earlier that day: before
		ev("tool/Edit/bb", "d1", since.Add(time.Hour)),
	}
	st := Status([]Rule{r}, evs, since.Add(40*24*time.Hour))
	if len(st) != 1 || st[0].After != 1 || st[0].AfterSessions != 1 || st[0].Before != 3 || !st[0].Lapsed {
		t.Fatalf("Status = %+v", st)
	}
}

func TestFromHooks(t *testing.T) {
	recs := []hooklog.Record{
		{Hook: "bash_guard", Decision: "deny_no_deadline", Session: "s", Command: "sleep 9"},
		{Hook: "stop_guard", Decision: "block_unbacked_promise", Session: "s"},
		{Hook: "ctx_budget", Decision: "nudge_task_file", Session: "s"},
		{Hook: "test_rerun", Decision: "log_test_rerun_no_change", Changed: "no", Session: "s"},
		{Hook: "test_rerun", Decision: "log_test_after_change", Changed: "yes", Session: "s"},
		{Hook: "bash_guard", Silent: true, Session: "s"},
	}
	got := FromHooks(recs)
	if len(got) != 3 {
		t.Fatalf("FromHooks = %+v", got)
	}
	if got[0].Title != "bash_guard denied: no deadline" || got[0].Sample != "sleep 9" || got[1].Title != "stop_guard blocked: unbacked promise" || got[2].Title != "tests re-run with no change" {
		t.Fatalf("titles: %+v", got)
	}
}

// The record shapes follow real claude 2.1.303 transcripts.
func TestScanErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "-w-p")
	os.MkdirAll(filepath.Join(dir, "sess1", "subagents"), 0o755)
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Write","input":{"file_path":"/w/p/a.go"}},{"type":"tool_use","id":"toolu_2","name":"Bash","input":{}}]}}`,
		`{"type":"user","cwd":"/w/p","timestamp":"2026-10-19T09:00:00.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"<tool_use_error>File has not been read yet. Read it first before writing to it.</tool_use_error>"}]}}`,
		`{"type":"user","cwd":"/w/p","timestamp":"2026-10-19T09:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","is_error":true,"content":"Exit code 1\nFAIL"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_3","name":"Read","input":{}}]}}`,
		`{"type":"user","cwd":"/w/p","timestamp":"2026-10-19T09:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_3","is_error":true,"content":[{"type":"text","text":"File does not exist. Note: your current working directory is /w/p/x."}]}]}}`,
		`{"type":"user","cwd":"/w/p","timestamp":"2026-10-19T09:00:03.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_3","content":"fine"}]}}`,
	}
	main := filepath.Join(dir, "sess1.jsonl")
	os.WriteFile(main, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	sub := filepath.Join(dir, "sess1", "subagents", "agent-x.jsonl")
	os.WriteFile(sub, []byte(lines[0]+"\n"+lines[1]), 0o644) // no trailing newline

	got, err := ScanErrors(main)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ScanErrors = %+v", got)
	}
	w, r := got[0], got[1]
	if w.Title != "Write failed: File has not been read yet. Read it first before writing to it." || w.Session != "sess1" || w.Cwd != "/w/p" || !strings.HasPrefix(w.Key, "tool/Write/") {
		t.Fatalf("write event: %+v", w)
	}
	if r.Title != "Read failed: File does not exist. Note: your current working directory is …" {
		t.Fatalf("read event: %+v", r)
	}
	if Draft(w.Key, w.Title) == "" || Draft(r.Key, r.Title) == "" {
		t.Fatal("no draft for a known error")
	}
	subEv, _ := ScanErrors(sub)
	if len(subEv) != 1 || subEv[0].Session != "sess1" || subEv[0].Key != w.Key {
		t.Fatalf("subagent: %+v", subEv)
	}
	if ps := Transcripts(filepath.Dir(dir), day.Add(-time.Hour*24*365)); len(ps) != 2 {
		t.Fatalf("Transcripts = %v", ps)
	}
}

func TestNormalizeAndSkip(t *testing.T) {
	a := Normalize("String to replace not found in file.\nString: foo")
	b := Normalize("<tool_use_error>String to replace not found in file.</tool_use_error>")
	if a != b || a != "String to replace not found in file." {
		t.Fatalf("%q vs %q", a, b)
	}
	if Normalize("Found 3 matches in ~/x/y.go") != Normalize("Found 12 matches in /a/b.go") {
		t.Fatal("numbers or paths kept apart the same failure")
	}
	for _, m := range []string{"Exit code 2", "The user doesn't want to proceed with this tool use.", "PreToolUse:Bash hook error: Blocked", "Permission for this action was denied by the Claude Code auto mode classifier."} {
		if !Skip("Bash", m) {
			t.Errorf("not skipped: %q", m)
		}
	}
	if Skip("Write", "File has not been read yet.") {
		t.Fatal("a real failure was skipped")
	}
}
