package main

import (
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/gitdiff"
	"github.com/henricktissink/loom/internal/hooklog"
	"github.com/henricktissink/loom/internal/taskfile"
	"github.com/henricktissink/loom/internal/transcript"
)

func TestSplitCheck(t *testing.T) {
	for _, c := range []struct {
		text                 string
		ticked               bool
		name, cmd, body, evi string
	}{
		{"tests: $ go test ./... -count=1  # ok, 27 packages", true, "tests", "go test ./... -count=1", "tests: $ go test ./... -count=1", "ok, 27 packages"},
		{"tests: $ go test ./...", false, "tests", "go test ./...", "tests: $ go test ./...", ""},
		{"precision on real data: replayed over 80 transcripts # 1 flag", true, "precision on real data", "", "precision on real data: replayed over 80 transcripts", "1 flag"},
		{"looks right in the app", false, "looks right in the app", "", "looks right in the app", ""},
	} {
		name, cmd, body, evi := splitCheck(c.text, c.ticked)
		if name != c.name || cmd != c.cmd || body != c.body || evi != c.evi {
			t.Errorf("splitCheck(%q) = %q, %q, %q, %q", c.text, name, cmd, body, evi)
		}
	}
}

func reviewTask(verified bool, open ...bool) taskfile.Task {
	return taskfile.Task{
		Status: "done", Done: 4, Total: 4, Outcome: "the hedge holds", StartedAt: 1000, Modified: 2000, Verified: verified,
		Checks: []string{
			"build: $ go build ./...  # clean",
			"tests: $ go test ./vega/  # ok",
			"docs: $ grep -q hedge docs/MANUAL.md  # 0",
			"looks right: checked by eye # fine",
		},
		Open: append(open, make([]bool, 4-len(open))...),
	}
}

func proofs(r ReviewDTO) string {
	var ps []string
	for _, c := range r.Checks {
		ps = append(ps, c.Proof)
	}
	return strings.Join(ps, " ")
}

func hasRisk(r ReviewDTO, level, sub string) bool {
	for _, x := range r.Risks {
		if x.Level == level && strings.Contains(x.Text, sub) {
			return true
		}
	}
	return false
}

func TestReviewProofs(t *testing.T) {
	// verified: every command check ran under the guard; the eye check is
	// still the model's word
	r := buildReview(reviewIn{dto: SessionDTO{Name: "loom-1"}, task: reviewTask(true), hasTask: true})
	if got := proofs(r); got != "guard_passed guard_passed guard_passed observed" {
		t.Fatalf("verified proofs = %s", got)
	}
	if !hasRisk(r, "warn", "on the model's word: looks right") || hasRisk(r, "warn", "not run by the stop guard") {
		t.Fatalf("verified risks = %+v", r.Risks)
	}

	// the guard ran this version's checks and "tests" failed: it ran the
	// others too, and they passed. The model stopping again (already
	// nudged, no names) and a promise block are not runs.
	recs := []hooklog.Record{
		{TS: time.Unix(2001, 0), Hook: "stop_guard", Event: "Stop", Decision: "block_done_check_failed", Failed: []string{"tests"}},
		{TS: time.Unix(2002, 0), Hook: "stop_guard", Event: "Stop", Decision: "allow_done_checks_failed_already_nudged"},
		{TS: time.Unix(2003, 0), Hook: "stop_guard", Event: "Stop", Decision: "block_unbacked_promise"},
	}
	stop := lastGuardRun(recs)
	r = buildReview(reviewIn{dto: SessionDTO{Name: "loom-1"}, task: reviewTask(false), hasTask: true, guardRun: stop})
	if got := proofs(r); got != "guard_passed guard_failed guard_passed observed" {
		t.Fatalf("failed proofs = %s", got)
	}
	if !hasRisk(r, "bad", "ran tests on this version of the task file and it failed") || hasRisk(r, "warn", "not run by the stop guard") {
		t.Fatalf("failed risks = %+v", r.Risks)
	}

	// a run older than the file is about an earlier version
	stop.TS = time.Unix(1999, 0)
	r = buildReview(reviewIn{dto: SessionDTO{Name: "loom-1"}, task: reviewTask(false, false, true), hasTask: true, guardRun: stop})
	if got := proofs(r); got != "claimed open claimed observed" {
		t.Fatalf("stale-verdict proofs = %s", got)
	}
}

func TestReviewTestsAndRisks(t *testing.T) {
	act := transcript.Activity{
		Total: 3,
		Runs: []transcript.TestRun{
			{Command: "go test ./vega/", At: 1100, Exit: 1, Status: "failed", Sample: "--- FAIL: TestHedge"},
			{Command: "go test ./vega/", At: 1200, Exit: 0, Status: "passed", Sample: "ok"},
			{Command: "go test ./...", At: 1300, Exit: 1, Status: "failed", Sample: "FAIL x"},
		},
		Edits: []transcript.Edit{{Path: "/r/vega/hedge.go", At: 1150}, {Path: "/r/vega/book.go", At: 1250},
			{Path: "/home/.claude/state/tasks/s.md", At: 1260}, {Path: "/rx/other.go", At: 1270}}, // outside the session's directory
	}
	dto := SessionDTO{Name: "loom-1",
		Spin:     &SpinDTO{Tool: "Bash", Count: 3},
		Overlaps: []OverlapDTO{{With: "loom-2", Files: []string{"vega/book.go"}}},
		Held:     &hooklog.Held{Hook: "bash_guard", Decision: "deny_background", Count: 4},
	}
	repos := []gitdiff.RepoDiff{{Label: "vega", Path: "/r", Stat: "2 files changed", Files: []string{"vega/hedge.go"}, Untracked: []string{"vega/new.go"}}}
	r := buildReview(reviewIn{dto: dto, act: act, repos: repos, dirs: []string{"/r"}})
	for _, want := range []struct{ level, sub string }{
		{"bad", "repeating a failure: Bash, 3 times"},
		{"bad", "The last test run failed: go test ./... (FAIL x)"},
		{"warn", "1 file was edited after the last passing test run: book.go"},
		{"warn", "no task file"},
		{"warn", "1 new file in vega not tracked by git yet"},
		{"warn", "Session loom-2 also has pending changes to vega/book.go"},
		{"warn", "A hook held it 4 times: bash_guard deny background"},
	} {
		if !hasRisk(r, want.level, want.sub) {
			t.Errorf("no %s risk %q in %+v", want.level, want.sub, r.Risks)
		}
	}
	if r.Risks[0].Level != "bad" || len(r.Tests) != 3 || r.TestsTotal != 3 || len(r.Changes) != 1 {
		t.Fatalf("sheet = %+v", r)
	}

	// nothing ran at all
	r = buildReview(reviewIn{dto: SessionDTO{Name: "loom-1"}, task: reviewTask(true), hasTask: true})
	if !hasRisk(r, "warn", "No test command ran since the task started") {
		t.Fatalf("risks without tests = %+v", r.Risks)
	}
}

func TestReviewSheetNeedsARailRow(t *testing.T) {
	a := &App{}
	if _, err := a.ReviewSheet("loom-9"); err == nil {
		t.Fatal("a sheet for a session that is not on the rail")
	}
	a.noteRail([]SessionDTO{{Name: "loom-1"}}, nil)
	r, err := a.ReviewSheet("loom-1")
	if err != nil || r.Name != "loom-1" || !hasRisk(r, "warn", "no task file") {
		t.Fatalf("sheet = %+v, %v", r, err)
	}
}
