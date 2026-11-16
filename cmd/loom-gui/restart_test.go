package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/transcript"
)

func TestRestartBrief(t *testing.T) {
	in := restartIn{
		title:   "fix the parser",
		outcome: "The parser reads nested quotes.",
		scope:   "In: internal/parse. Out: the CLI.",
		notes:   "- quotes inside comments are out of scope",
		checks:  []string{"parse tests: $ go test ./internal/parse/", "docs: README says so"},
		open:    []bool{true, false},
		files:   []string{"internal/parse/quote.go", "internal/parse/quote_test.go"},
		failing: []transcript.TestRun{{Command: "go test ./internal/parse/\n  -run Quote", Status: "failed", Sample: "--- FAIL: TestNested"}},
		drift:   []DriftDTO{{Kind: "tests", Label: "3 test fails", Detail: "Tests have failed 3 times this turn."}},
	}
	got := restartBrief(in)
	for _, want := range []string{
		"because it was drifting.",
		"## Task\nThe parser reads nested quotes.\n",
		"## Scope\nIn: internal/parse. Out: the CLI.\n",
		"- [ ] parse tests: $ go test ./internal/parse/\n- [x] docs: README says so\n",
		"re-run them before relying on them",
		"- internal/parse/quote.go\n- internal/parse/quote_test.go\n",
		"- Failed: `go test ./internal/parse/ -run Quote`: --- FAIL: TestNested\n",
		"- Tests have failed 3 times this turn.\n",
		"## Notes from the earlier session\n- quotes inside comments are out of scope\n",
		"then name the one next step.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "fix the parser") {
		t.Error("the title was used beside the outcome")
	}

	// no task file: the title is the task; nothing failed; a spin leads
	got = restartBrief(restartIn{title: "fix the parser", spin: &SpinDTO{Tool: "Bash", Subject: "make", Count: 4, Sample: "exit 2"}, files: []string{"a.go"}, moreFile: 3})
	for _, want := range []string{"kept repeating a failure", "## Task\nfix the parser\n", "The same failure 4 times in a row (Bash make): exit 2", "- and 3 more (git status)"} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	for _, no := range []string{"## Done when", "## Scope", "## Notes"} {
		if strings.Contains(got, no) {
			t.Errorf("brief has an empty %s", no)
		}
	}
	if got := restartBrief(restartIn{}); !strings.Contains(got, "ran out of room") || !strings.Contains(got, "(not recorded: ask me)") {
		t.Errorf("empty brief:\n%s", got)
	}
}

func TestRestartLastFailures(t *testing.T) {
	runs := []transcript.TestRun{
		{Command: "go test ./a/", Status: "failed", Sample: "old"},
		{Command: "go test ./b/", Status: "failed", Sample: "b"},
		{Command: "go test ./a/", Status: "passed"}, // a later pass rules it back in
		{Command: "go test ./c/", Status: "failed", Sample: "c1"},
		{Command: "go test ./c/", Status: "failed", Sample: "c2"},
		{Command: "go test ./d/", Status: "unknown"},
	}
	got := lastFailures(runs, 5)
	if len(got) != 2 || got[0].Sample != "c2" || got[1].Sample != "b" {
		t.Fatalf("got %+v", got)
	}
	if got := lastFailures(runs, 1); len(got) != 1 || got[0].Sample != "c2" {
		t.Fatalf("capped: %+v", got)
	}
}

func TestRestartTaskSection(t *testing.T) {
	text := "# Task\nStatus: open\n\n## Outcome\nx\n\n## Scope\nIn: a.\nOut: b.\n\n## Done when\n- [ ] c\n\n## Notes\n(assumptions taken, decisions made, out-of-scope findings)\n"
	if got := taskSection(text, "Scope"); got != "In: a.\nOut: b." {
		t.Errorf("scope = %q", got)
	}
	if got := taskSection(text, "notes"); got != "" {
		t.Errorf("placeholder notes = %q", got)
	}
	if got := taskSection(text, "Missing"); got != "" {
		t.Errorf("missing = %q", got)
	}
}

func TestRestartEndAfterSeed(t *testing.T) {
	steps := func(st ...string) func() string {
		i := 0
		return func() string {
			v := st[min(i, len(st)-1)]
			i++
			return v
		}
	}
	killed := 0
	kill := func() error { killed++; return nil }
	if got := endAfterSeed(steps("", "", "sent"), kill, time.Second, time.Millisecond); got != "ended" || killed != 1 {
		t.Fatalf("sent: %q killed=%d", got, killed)
	}
	if got := endAfterSeed(steps("", "failed"), kill, time.Second, time.Millisecond); got != "the brief was not delivered" || killed != 1 {
		t.Fatalf("failed: %q killed=%d", got, killed)
	}
	if got := endAfterSeed(steps(""), kill, 5*time.Millisecond, time.Millisecond); got != "the brief was not delivered in time" || killed != 1 {
		t.Fatalf("timeout: %q killed=%d", got, killed)
	}
	if got := endAfterSeed(steps("sent"), func() error { return errors.New("no tmux") }, time.Second, time.Millisecond); got != "could not end it: no tmux" {
		t.Fatalf("kill error: %q", got)
	}
}

func TestRestartWithBriefRefuses(t *testing.T) {
	a := &App{}
	if _, err := a.RestartWithBrief("x", "  ", true); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty brief: %v", err)
	}
	if _, err := a.RestartWithBrief("x", "go", true); err == nil {
		t.Error("no launcher: no error")
	}
	if _, err := a.FreshSession("x"); err == nil {
		t.Error("FreshSession without a launcher: no error")
	}
}
