package transcript

import (
	"fmt"
	"testing"
)

func TestIsTestCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"go test ./...": true,
		"cd /r && deadline 15m bash -c 'go test ./...'":  true,
		"uv run pytest tests/ -q":                        true,
		"npm run test:unit":                              true,
		"npx vitest run":                                 true,
		"cargo nextest run":                              true,
		"dotnet test Bankenstein.Tests":                  true,
		"make check":                                     true,
		"go build ./... \\\n  && go test ./internal/...": true,
		"grep -rn 'go test' docs":                        false,
		"go build ./...":                                 false,
		// a heredoc's body is a script, not commands
		"python3 -I - <<'EOF'\np='internal/transcript/spin_test.go'\nprint('go test')\nEOF": false,
	} {
		if got := IsTestCommand(cmd); got != want {
			t.Errorf("IsTestCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func bgUse(id, cmd string) string {
	return fmt.Sprintf(`{"type":"assistant","cwd":"/repo","message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q,"run_in_background":true}}],"stop_reason":"tool_use"}}`, id, cmd)
}

func notification(id string, summary string) string {
	note := fmt.Sprintf("<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>%s</tool-use-id>\n<status>completed</status>\n<summary>%s</summary>\n</task-notification>", id, summary)
	return fmt.Sprintf(`{"type":"user","promptId":"p2","timestamp":"2026-11-07T10:05:00Z","message":{"role":"user","content":%q}}`, note)
}

func TestScanActivity(t *testing.T) {
	ls := []string{prompt("p1", "fix it")}
	ls = bash(ls, "p1", "t1", "go test ./vega/", fmt.Sprintf(goFail, 1, 1, 1), true)
	ls = append(ls, use("e1", "Edit", map[string]string{"file_path": "/repo/vega/hedge.go"}), result("p1", "e1", "ok", false))
	// a pipe hides the exit code; the output still says FAIL
	ls = bash(ls, "p1", "t2", "go test ./vega/ 2>&1 | tail -3", "--- FAIL: TestHedge (0.01s)\nFAIL\nFAIL\tgithub.com/x/vega\t0.1s", false)
	ls = bash(ls, "p1", "t3", "go test ./vega/", "ok  \tgithub.com/x/vega\t0.2s", false)
	ls = bash(ls, "p1", "x1", "go build ./...", "", false)
	ls = bash(ls, "p1", "t4", "go test ./...", "The user doesn't want to proceed with this tool use. The tool use was rejected", true)
	ls = append(ls, bgUse("t5", "deadline 15m go test ./... -count=1"),
		result("p1", "t5", "Command running in background with ID: b1.", false))
	ls = append(ls, bgUse("t6", "deadline 15m uv run pytest"),
		result("p1", "t6", "Command running in background with ID: b2.", false))
	ls = append(ls, notification("t5", `Background command "suite" completed (exit code 2)`))

	act, err := ScanActivity(writeLines(t, ls), 0)
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		cmd, status string
		exit        int
	}
	var runs []got
	for _, r := range act.Runs {
		runs = append(runs, got{r.Command, r.Status, r.Exit})
	}
	want := []got{
		{"go test ./vega/", "failed", 1},
		{"go test ./vega/ 2>&1 | tail -3", "failed", 0},
		{"go test ./vega/", "passed", 0},
		{"deadline 15m go test ./... -count=1", "failed", 2},
		{"deadline 15m uv run pytest", "unknown", -1},
	}
	if fmt.Sprint(runs) != fmt.Sprint(want) || act.Total != 5 {
		t.Fatalf("runs = %v (total %d)\nwant %v", runs, act.Total, want)
	}
	if s := act.Runs[0].Sample; s != "--- FAIL: TestHedge (0.01s)" {
		t.Fatalf("failing sample = %q", s)
	}
	if s := act.Runs[2].Sample; s != "ok github.com/x/vega 0.2s" {
		t.Fatalf("passing sample = %q", s)
	}
	if len(act.Edits) != 1 || act.Edits[0].Path != "/repo/vega/hedge.go" {
		t.Fatalf("edits = %+v", act.Edits)
	}
}

func TestScanActivitySinceAndCap(t *testing.T) {
	ls := []string{prompt("p1", "x")}
	for i := 0; i < MaxRuns+5; i++ {
		ls = bash(ls, "p1", fmt.Sprint("t", i), fmt.Sprintf("go test ./p%d/", i), "ok", false)
	}
	act, _ := ScanActivity(writeLines(t, ls), 0)
	if act.Total != MaxRuns+5 || len(act.Runs) != MaxRuns || act.Runs[0].Command != "go test ./p5/" {
		t.Fatalf("total %d, kept %d, first %q", act.Total, len(act.Runs), act.Runs[0].Command)
	}
	// every result is stamped 2026-11-07T10:00:00Z; a later start keeps none
	act, _ = ScanActivity(writeLines(t, ls), parseTS("2026-11-07T10:00:01Z"))
	if act.Total != 0 {
		t.Fatalf("runs before since: %d", act.Total)
	}
	if act, err := ScanActivity("/nonexistent/x.jsonl", 0); err != nil || act.Total != 0 {
		t.Fatalf("missing file: %+v, %v", act, err)
	}
}
