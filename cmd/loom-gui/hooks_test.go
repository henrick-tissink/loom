package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/hooklog"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/taskfile"
	"github.com/henricktissink/loom/internal/tmux"
)

// Logs live under t.TempDir(); nothing here reads the machine's own.

func hookReader(t *testing.T, decisions, allows []string) *hooklog.Reader {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "decisions.jsonl"), []byte(strings.Join(decisions, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "allows.jsonl"), []byte(strings.Join(allows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := hooklog.New(dir)
	r.Refresh()
	return r
}

func rec(ts, hook, event, decision, sid, cwd, extra string) string {
	return `{"ts": "` + ts + `", "hook": "` + hook + `", "event": "` + event + `", "decision": "` + decision + `", "session": "` + sid + `", "agent_type": null, "cwd": "` + cwd + `"` + extra + `}`
}

func allow(ts, hook, sid string) string {
	return `{"ts": "` + ts + `", "hook": "` + hook + `", "event": "PreToolUse", "session": "` + sid + `"}`
}

func TestHooksForCountsSinceTaskStartAndMarksChecks(t *testing.T) {
	r := hookReader(t, []string{
		rec("2026-10-16T10:00:00", "bash_guard", "PreToolUse", "deny_no_deadline", "sid1", visRoot, `, "command": "sleep 1"`),
		rec("2026-10-16T10:01:00", "bash_guard", "PreToolUse", "deny_no_deadline", "sid1", visRoot, `, "command": "sleep 2"`),
		rec("2026-10-16T10:02:00", "bash_guard", "PreToolUse", "deny_no_deadline", "sid1", visRoot, `, "command": "sleep 3"`),
		rec("2026-10-16T10:03:00", "ctx_budget", "UserPromptSubmit", "nudge_task_file", "sid1", visRoot, ``),
		rec("2026-10-16T10:04:00", "stop_guard", "Stop", "block_done_check_failed", "sid1", visRoot, `, "failed": ["tests"]`),
		rec("2026-10-16T10:05:00", "stop_guard", "SubagentStop", "block_unaccounted_tasks", "sid1", visRoot, ``),
	}, []string{allow("2026-10-16T10:00:30", "bash_guard", "sid1")})
	row := status.Row{SessionRow: store.SessionRow{Name: "loom-1", ClaudeSessionID: "sid1", Cwd: visRoot}}

	// No task file: everything counts; the same hold three times is "held".
	h := hooksFor(r, row)
	if h.Holds != 5 || h.Nudges != 1 || h.Held == nil || h.Held.Count != 3 || h.Held.Command != "sleep 3" || !h.ChecksFailed {
		t.Fatalf("no task: %+v held %+v", h, h.Held)
	}
	// A task file started after two denies resets the window below the threshold.
	row.HasTask = true
	row.Task = taskfile.Task{Status: "open", StartedAt: time.Date(2026, 10, 16, 10, 1, 30, 0, time.Local).Unix()}
	h = hooksFor(r, row)
	if h.Holds != 3 || h.Held != nil || !h.ChecksFailed {
		t.Fatalf("since task: %+v", h)
	}
	// The verified marker clears the failed mark.
	row.Task.Verified = true
	if h = hooksFor(r, row); h.ChecksFailed {
		t.Fatal("verified task must not show checks failed")
	}
	// So does an edit to the task file after the verdict; one before it does not.
	row.Task.Verified = false
	row.Task.Modified = time.Date(2026, 10, 16, 10, 4, 30, 0, time.Local).Unix()
	if h = hooksFor(r, row); h.ChecksFailed {
		t.Fatal("task edited after the verdict must not show checks failed")
	}
	row.Task.Modified = time.Date(2026, 10, 16, 10, 3, 30, 0, time.Local).Unix()
	if h = hooksFor(r, row); !h.ChecksFailed {
		t.Fatal("task edited before the verdict keeps the failed mark")
	}
	// A hand-back let through because the failure was already reported keeps ✗.
	r2 := hookReader(t, []string{
		rec("2026-10-16T10:04:00", "stop_guard", "Stop", "block_done_check_failed", "sid1", visRoot, ``),
		rec("2026-10-16T10:06:00", "stop_guard", "Stop", "allow_done_checks_failed_already_nudged", "sid1", visRoot, ``),
	}, nil)
	if h = hooksFor(r2, row); !h.ChecksFailed {
		t.Fatal("already-nudged hand-back keeps checks failed")
	}
	row.Task.Verified, row.Task.Modified = true, 0
	// No session id: nothing.
	if h = hooksFor(r, status.Row{}); h.Holds != 0 || h.Held != nil {
		t.Fatalf("no sid: %+v", h)
	}

	snap := status.Snapshot{Live: []status.Row{row, {SessionRow: store.SessionRow{Name: "loom-2", ClaudeSessionID: "sid2", Cwd: visRoot}}}}
	dtos := snapshotToDTOs(snap, nil)
	applyHooks(dtos, snap, r)
	if dtos[0].Holds != 3 || !dtos[0].Verified || dtos[0].ChecksFailed || dtos[1].Holds != 0 {
		t.Fatalf("dtos: %+v %+v", dtos[0], dtos[1])
	}
	applyHooks(dtos, snap, nil) // a nil reader is a no-op
}

func TestVisibleRecordsHidesByCwdAndDropsAllowsWhenFiltering(t *testing.T) {
	r := hookReader(t, []string{
		rec("2026-10-16T10:00:00", "bash_guard", "PreToolUse", "deny_no_deadline", "a", visRepo, ``),
		rec("2026-10-16T10:01:00", "bash_guard", "PreToolUse", "deny_no_deadline", "b", hidRepo, ``),
		rec("2026-10-16T10:02:00", "test_rerun", "PreToolUse", "log_test_rerun_no_change", "b", hidRepo, `, "command": "go test", "changed": "no", "same_cmd": true, "since_last_s": 30`),
	}, []string{allow("2026-10-16T10:00:30", "bash_guard", "a")})
	all := r.Records()
	got, hidden := visibleRecords(testResolver(), all)
	if len(got) != 4 || hidden {
		t.Fatalf("nothing hidden: %d %v", len(got), hidden)
	}
	got, hidden = visibleRecords(testResolver(hidRoot), all)
	if len(got) != 1 || !hidden || got[0].Session != "a" {
		t.Fatalf("hidden project: %+v %v", got, hidden)
	}
	if got, _ = visibleRecords(nil, all); len(got) != 4 {
		t.Fatal("nil resolver shows everything")
	}
}

func TestHookRatesAndRerunsWindows(t *testing.T) {
	r := hookReader(t, []string{
		rec("2026-10-10T10:00:00", "bash_guard", "PreToolUse", "deny_no_deadline", "a", visRepo, ``),
		rec("2026-10-16T10:00:00", "bash_guard", "PreToolUse", "deny_detach", "a", visRepo, `, "rev": "r3"`),
		rec("2026-10-16T10:01:00", "test_rerun", "PreToolUse", "log_test_first", "a", visRepo, `, "command": "go test", "changed": "first"`),
		rec("2026-10-16T10:02:00", "test_rerun", "PreToolUse", "log_test_rerun_no_change", "a", visRepo, `, "command": "go test", "changed": "no", "same_cmd": true, "since_last_s": 60`),
	}, []string{allow("2026-10-10T10:00:30", "bash_guard", "a"), allow("2026-10-16T10:00:30", "bash_guard", "a")})
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.Local)
	a := newApp(nil, tmux.New(), nil, nil, nil, func() time.Time { return now })
	a.hooks = &hookState{reader: r} // no layout: no ledger, so the only mark is the first r3 record
	rates := a.HookRates("change")
	if rates.Window != "change" || len(rates.Marks) != 1 || !rates.Marks[0].Inferred || rates.Since == 0 {
		t.Fatalf("rates window: %+v", rates)
	}
	if len(rates.Rows) != 2 || rates.Rows[0].Hook != "bash_guard" || len(rates.Rows[0].Spans) != 1 || rates.Rows[0].Spans[0].Runs != 2 || rates.Rows[0].Spans[0].Holds != 1 {
		t.Fatalf("rates since change: %+v", rates.Rows)
	}
	all := a.HookRates("all")
	if all.Since != 0 || len(all.Rows[0].Spans) != 2 || all.Rows[0].Spans[0].Runs != 2 || all.Rows[0].Spans[1].Mark == "" {
		t.Fatalf("rates all: %+v", all.Rows[0].Spans)
	}
	if w := a.HookRates("7d"); w.Window != "7d" || w.Since != now.Add(-7*24*time.Hour).Unix() {
		t.Fatalf("7d: %+v", w)
	}
	rr := a.HookReruns("all")
	if len(rr.Rows) != 1 || rr.Rows[0].Runs != 2 || rr.Rows[0].NoChange != 1 || rr.Rows[0].MedianGapS != 60 {
		t.Fatalf("reruns: %+v", rr.Rows)
	}
	// No hooks state at all: empty, never nil, never a panic.
	b := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if x := b.HookRates("all"); x.Rows == nil || x.Marks == nil {
		t.Fatal("nil state must marshal as []")
	}
	if p := b.SessionHooks("loom-1"); p.Classes == nil || p.Recent == nil {
		t.Fatal("nil state panel must marshal as []")
	}
}
