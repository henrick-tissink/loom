package transcript

import (
	"fmt"
	"testing"
)

func say(id, text string, out int64) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"role":"assistant","content":[{"type":"text","text":%q}],"usage":{"output_tokens":%d},"stop_reason":"end_turn"}}`, id, text, out)
}

func TestTurnStatsTestRunsInARow(t *testing.T) {
	ls := []string{`{"type":"user","promptId":"p1","timestamp":"2026-11-07T09:00:00Z","message":{"role":"user","content":"fix it"}}`}
	// different failures each time: not a spin, but three failing runs
	for i, out := range []string{"Exit code 1\n--- FAIL: TestA", "Exit code 1\n./a.go:3: undefined: Foo", "Exit code 2\npanic: nil map"} {
		ls = bash(ls, "p1", fmt.Sprint("b", i), "go test ./...", out, true)
	}
	ls = bash(ls, "p1", "g1", "git status", "Exit code 128\nfatal: no repo", true) // not a test run
	ls = bash(ls, "p1", "d1", "go test ./x", "The user doesn't want to proceed with this tool use.", true)
	c := feed(t, ls...)
	ts := c.TurnStats()
	if ts.TestFails != 3 || ts.LastTest != "go test ./..." || ts.Calls != 5 {
		t.Fatalf("stats = %+v", ts)
	}
	if ts.Start == 0 {
		t.Fatal("no turn start")
	}
	if c.Spin() != nil {
		t.Fatal("different failures made a spin")
	}
	// a piped run exits 0 but its output says FAIL: still a failure; a
	// background run with no result yet changes nothing
	ls = bash(ls, "p1", "pp", "go test ./... 2>&1 | tail -5", "--- FAIL: TestA (0.00s)\nFAIL\nFAIL\tx\t0.1s", false)
	ls = append(ls, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"bg","name":"Bash","input":{"command":"go test ./...","run_in_background":true}}],"stop_reason":"tool_use"}}`, result("p1", "bg", "Command running in background with ID: b1", false))
	if ts := feed(t, ls...).TurnStats(); ts.TestFails != 4 {
		t.Fatalf("piped/background: %+v", ts)
	}
	// a passing run ends the streak
	ls = bash(ls, "p1", "b9", "go test ./...", "ok  x 0.1s", false)
	if ts := feed(t, ls...).TurnStats(); ts.TestFails != 0 {
		t.Fatalf("after a pass: %+v", ts)
	}
	// a heredoc that mentions go test is not a test run
	ls2 := []string{prompt("p1", "x")}
	for i := 0; i < 3; i++ {
		ls2 = bash(ls2, "p1", fmt.Sprint("h", i), "cat > x.sh <<'EOF'\ngo test ./...\nEOF", "Exit code 1\nboom", true)
	}
	if ts := feed(t, ls2...).TurnStats(); ts.TestFails != 0 {
		t.Fatalf("heredoc counted: %+v", ts)
	}
}

func TestTurnStatsResetOnNewTurn(t *testing.T) {
	ls := []string{prompt("p1", "x")}
	for i := 0; i < 3; i++ {
		ls = bash(ls, "p1", fmt.Sprint("b", i), "go test ./...", "Exit code 1\nFAIL", true)
	}
	ls = append(ls, prompt("p2", "try again"))
	ts := feed(t, ls...).TurnStats()
	if ts.TestFails != 0 || ts.Calls != 0 {
		t.Fatalf("stats carried into a new turn: %+v", ts)
	}
}

func TestTurnStatsOutputTokensOncePerMessage(t *testing.T) {
	ls := []string{prompt("p1", "x"),
		// one API message written as two records, both carrying its usage
		say("m1", "Looking.", 900), say("m1", "Done.", 1000),
		say("m2", "More.", 500),
		say("", "no id", 7),
	}
	if got := feed(t, ls...).TurnStats().OutTokens; got != 1507 {
		t.Fatalf("out tokens = %d, want 1507", got)
	}
}

func TestTurnStatsAgreementsLookBackFiveTurns(t *testing.T) {
	ls := []string{prompt("p1", "that's wrong"), say("a1", "You're absolutely right — the test was skipped.", 1),
		prompt("p2", "and this"), say("a2", "Good catch. I'll fix it.", 1),
		prompt("p3", "ok"), say("a3", "Right away; I agree it's cleaner.", 1)} // not an opener
	ts := feed(t, ls...).TurnStats()
	if ts.Agrees != 2 || ts.AgreeSample != "Good catch. I'll fix it." {
		t.Fatalf("stats = %+v", ts)
	}
	for i := 4; i <= 7; i++ {
		ls = append(ls, prompt(fmt.Sprint("p", i), "go on"), say(fmt.Sprint("a", i), "Working on it.", 1))
	}
	// p1 and p2 have left the five-turn window
	if ts := feed(t, ls...).TurnStats(); ts.Agrees != 0 || ts.AgreeSample != "" {
		t.Fatalf("old agreements kept: %+v", ts)
	}
}
