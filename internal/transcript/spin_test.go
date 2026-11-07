package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeLines(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Record shapes follow real transcripts (2026-11): a failed Bash result is
// "Exit code N\n<output>" with is_error, a tool error is wrapped in
// <tool_use_error>, and every user record of a turn carries its promptId.

func prompt(pid, text string) string {
	return fmt.Sprintf(`{"type":"user","promptId":%q,"message":{"role":"user","content":%q}}`, pid, text)
}

func use(id, name string, input map[string]string) string {
	in, _ := json.Marshal(input)
	return fmt.Sprintf(`{"type":"assistant","cwd":"/repo","message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}],"stop_reason":"tool_use"}}`, id, name, in)
}

func result(pid, id, content string, isErr bool) string {
	return fmt.Sprintf(`{"type":"user","promptId":%q,"timestamp":"2026-11-07T10:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q,"is_error":%v}]}}`, pid, id, content, isErr)
}

// bash appends one Bash call and its result.
func bash(lines []string, pid, id, cmd, out string, isErr bool) []string {
	return append(lines, use(id, "Bash", map[string]string{"command": cmd}), result(pid, id, out, isErr))
}

const goFail = "Exit code 1\n=== RUN   TestHedge\n--- FAIL: TestHedge (0.%02ds)\n    hedge_test.go:41: got %d, want 4\nFAIL\nFAIL\tgithub.com/x/vega\t0.%03ds"

func TestSpinSameBashFailureThreeTimes(t *testing.T) {
	ls := []string{prompt("p1", "fix the hedge")}
	for i := 1; i <= 3; i++ {
		ls = bash(ls, "p1", fmt.Sprintf("b%d", i), "go test ./vega/", fmt.Sprintf(goFail, i, i+1, i*7), true)
		ls = append(ls, use(fmt.Sprintf("e%d", i), "Edit", map[string]string{"file_path": "/repo/vega/hedge.go"}),
			result("p1", fmt.Sprintf("e%d", i), "ok", false))
	}
	sp := feed(t, ls...).Spin()
	if sp == nil {
		t.Fatal("no spin after the same failure three times")
	}
	if sp.Tool != "Bash" || sp.Count != 3 {
		t.Fatalf("spin = %+v, want Bash ×3", sp)
	}
	if want := "exit 1 · --- FAIL: TestHedge (0.03s)"; sp.Sample != want {
		t.Fatalf("sample = %q, want %q", sp.Sample, want)
	}
	if sp.Subject != "go test ./vega/" {
		t.Fatalf("subject = %q", sp.Subject)
	}
}

func TestSpinNeedsThree(t *testing.T) {
	ls := []string{prompt("p1", "x")}
	ls = bash(ls, "p1", "b1", "go test ./...", fmt.Sprintf(goFail, 1, 1, 1), true)
	ls = bash(ls, "p1", "b2", "go test ./...", fmt.Sprintf(goFail, 2, 2, 2), true)
	if sp := feed(t, ls...).Spin(); sp != nil {
		t.Fatalf("spin after two failures: %+v", sp)
	}
}

func TestSpinDifferentFailuresDoNotAdd(t *testing.T) {
	ls := []string{prompt("p1", "x")}
	for i, out := range []string{"Exit code 1\n./a.go:3: undefined: Foo", "Exit code 1\n./a.go:9: missing return", "Exit code 1\n--- FAIL: TestBar"} {
		ls = bash(ls, "p1", fmt.Sprint("b", i), "go test ./...", out, true)
	}
	if sp := feed(t, ls...).Spin(); sp != nil {
		t.Fatalf("three different failures read as a spin: %+v", sp)
	}
}

// The same compile error behind different commands is still one failure.
func TestSpinSameOutputDifferentCommands(t *testing.T) {
	ls := []string{prompt("p1", "x")}
	for i, cmd := range []string{"go build ./...", "go test ./vega/", "go vet ./vega/"} {
		ls = bash(ls, "p1", fmt.Sprint("b", i), cmd, fmt.Sprintf("Exit code 1\n# vega\n./hedge.go:%d:2: undefined: delta", 10+i), true)
	}
	sp := feed(t, ls...).Spin()
	if sp == nil || sp.Count != 3 {
		t.Fatalf("spin = %+v, want ×3", sp)
	}
}

// With no output, failures are told apart by their command.
func TestSpinEmptyOutputKeysOnCommand(t *testing.T) {
	ls := []string{prompt("p1", "x")}
	for i, cmd := range []string{"grep -q a f", "grep -q b f", "test -f x"} {
		ls = bash(ls, "p1", fmt.Sprint("b", i), cmd, "Exit code 1", true)
	}
	if sp := feed(t, ls...).Spin(); sp != nil {
		t.Fatalf("three different silent commands read as a spin: %+v", sp)
	}
	ls = []string{prompt("p1", "x")}
	for i := 0; i < 3; i++ {
		ls = bash(ls, "p1", fmt.Sprint("b", i), "curl -sf localhost:42000/health", "Exit code 7", true)
	}
	sp := feed(t, ls...).Spin()
	if sp == nil || sp.Sample != "exit 7" {
		t.Fatalf("spin = %+v, want the silent command ×3 with sample \"exit 7\"", sp)
	}
}

func TestSpinResetsOnNewPrompt(t *testing.T) {
	ls := []string{prompt("p1", "x")}
	for i := 0; i < 3; i++ {
		ls = bash(ls, "p1", fmt.Sprint("b", i), "make", "Exit code 2\nmake: *** No rule to make target", true)
	}
	ls = append(ls, prompt("p2", "try something else"))
	if sp := feed(t, ls...).Spin(); sp != nil {
		t.Fatalf("spin survived a new prompt: %+v", sp)
	}
}

// Stop-hook feedback and a compaction summary keep the turn's promptId:
// the agent sent back to work is still in the same turn.
func TestSpinSurvivesStopHookFeedback(t *testing.T) {
	ls := []string{prompt("p1", "x")}
	ls = bash(ls, "p1", "b0", "make", "Exit code 2\nmake: *** No rule to make target", true)
	ls = bash(ls, "p1", "b1", "make", "Exit code 2\nmake: *** No rule to make target", true)
	ls = append(ls, `{"type":"user","isMeta":true,"promptId":"p1","message":{"role":"user","content":"Stop hook feedback:\nchecks fail"}}`)
	ls = bash(ls, "p1", "b2", "make", "Exit code 2\nmake: *** No rule to make target", true)
	if sp := feed(t, ls...).Spin(); sp == nil || sp.Count != 3 {
		t.Fatalf("spin = %+v, want ×3 across the stop hook's feedback", sp)
	}
}

// Without promptIds, a person's message starts the turn.
func TestSpinFallbackTurnWithoutPromptID(t *testing.T) {
	ls := []string{lineUserPrompt}
	for i := 0; i < 3; i++ {
		ls = bash(ls, "", fmt.Sprint("b", i), "make", "Exit code 2\nmake: *** No rule", true)
	}
	if sp := feed(t, ls...).Spin(); sp == nil {
		t.Fatal("no spin without promptIds")
	}
	if sp := feed(t, append(ls, lineUserPrompt)...).Spin(); sp != nil {
		t.Fatalf("spin survived a new prompt without promptIds: %+v", sp)
	}
}

func TestSpinClearedBySuccessOfSameCommand(t *testing.T) {
	ls := []string{prompt("p1", "x")}
	for i := 0; i < 3; i++ {
		ls = bash(ls, "p1", fmt.Sprint("b", i), "go test ./vega/", fmt.Sprintf(goFail, i, i, i), true)
	}
	ls = bash(ls, "p1", "ok", "go   test ./vega/", "ok  github.com/x/vega 0.2s", false)
	if sp := feed(t, ls...).Spin(); sp != nil {
		t.Fatalf("spin after the command passed: %+v", sp)
	}
}

func TestSpinFadesWhenTheAgentMovesOn(t *testing.T) {
	ls := []string{prompt("p1", "x")}
	for i := 0; i < 3; i++ {
		ls = bash(ls, "p1", fmt.Sprint("b", i), "go test ./vega/", fmt.Sprintf(goFail, i, i, i), true)
	}
	for i := 0; i < spinRecent-1; i++ {
		ls = append(ls, use(fmt.Sprint("r", i), "Read", map[string]string{"file_path": "/repo/a.go"}), result("p1", fmt.Sprint("r", i), "x", false))
	}
	if sp := feed(t, ls...).Spin(); sp == nil {
		t.Fatal("spin gone before spinRecent results")
	}
	ls = append(ls, use("rN", "Read", map[string]string{"file_path": "/repo/a.go"}), result("p1", "rN", "x", false))
	if sp := feed(t, ls...).Spin(); sp != nil {
		t.Fatalf("spin still shown %d results after its last hit: %+v", spinRecent, sp)
	}
}

func TestSpinIgnoresPersonAndHookFailures(t *testing.T) {
	for _, out := range []string{
		"The user doesn't want to proceed with this tool use. The tool use was rejected",
		"PreToolUse:Bash hook error: Blocked: background commands need a deadline.",
		"Blocked: background commands need a deadline.",
		"[Request interrupted by user for tool use]",
		"claude-sonnet-4[1m] is temporarily unavailable (timed out), so auto mode cannot determine",
		"Permission for this action was denied by the Claude Code auto mode classifier. Reason: Blocked by classifier. You may try other tools.",
	} {
		ls := []string{prompt("p1", "x")}
		for i := 0; i < 4; i++ {
			ls = bash(ls, "p1", fmt.Sprint("b", i), "sleep 60 &", out, true)
		}
		if sp := feed(t, ls...).Spin(); sp != nil {
			t.Fatalf("%q counted as a spin: %+v", out, sp)
		}
	}
}

// A file tool's failure is per file: the same edit failing on one file
// is a loop, the same message on three files is not.
func TestSpinFileToolKeysOnFile(t *testing.T) {
	const notFound = "<tool_use_error>String to replace not found in file.\nString: delta := 0</tool_use_error>"
	ls := []string{prompt("p1", "x")}
	for i, f := range []string{"/repo/a.go", "/repo/b.go", "/repo/c.go"} {
		ls = append(ls, use(fmt.Sprint("e", i), "Edit", map[string]string{"file_path": f}), result("p1", fmt.Sprint("e", i), notFound, true))
	}
	if sp := feed(t, ls...).Spin(); sp != nil {
		t.Fatalf("one message on three files read as a spin: %+v", sp)
	}
	ls = []string{prompt("p1", "x")}
	for i := 0; i < 3; i++ {
		ls = append(ls, use(fmt.Sprint("e", i), "Edit", map[string]string{"file_path": "/repo/a.go"}), result("p1", fmt.Sprint("e", i), notFound, true))
	}
	sp := feed(t, ls...).Spin()
	if sp == nil || sp.Tool != "Edit" || sp.Subject != "/repo/a.go" || !strings.HasPrefix(sp.Sample, "String to replace not found") {
		t.Fatalf("spin = %+v, want Edit on /repo/a.go", sp)
	}
}

func TestEditedFiles(t *testing.T) {
	ls := []string{prompt("p1", "x"),
		use("e1", "Edit", map[string]string{"file_path": "/repo/b.go"}), result("p1", "e1", "ok", false),
		use("w1", "Write", map[string]string{"file_path": "docs/new.md"}), result("p1", "w1", "ok", false),
		use("e2", "Edit", map[string]string{"file_path": "/repo/fail.go"}), result("p1", "e2", "<tool_use_error>String to replace not found</tool_use_error>", true),
		use("r1", "Read", map[string]string{"file_path": "/repo/read.go"}), result("p1", "r1", "x", false),
		use("n1", "NotebookEdit", map[string]string{"notebook_path": "/repo/a.ipynb"}), result("p1", "n1", "ok", false),
		prompt("p2", "next"),
		use("e3", "Edit", map[string]string{"file_path": "/repo/b.go"}), result("p2", "e3", "ok", false),
	}
	var got []string
	for _, e := range feed(t, ls...).Edited() {
		got = append(got, e.Path)
	}
	want := []string{"/repo/a.ipynb", "/repo/b.go", "/repo/docs/new.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edited = %v, want %v", got, want)
	}
}

func TestReaderSnapshotCarriesSpinAndEdits(t *testing.T) {
	ls := []string{prompt("p1", "x")}
	for i := 0; i < 3; i++ {
		ls = bash(ls, "p1", fmt.Sprint("b", i), "make", "Exit code 2\nmake: *** No rule", true)
	}
	ls = append(ls, use("e1", "Edit", map[string]string{"file_path": "/repo/a.go"}), result("p1", "e1", "ok", false))
	path := writeLines(t, ls)
	rs, err := NewReader(path).Poll()
	if err != nil {
		t.Fatal(err)
	}
	if rs.Spin == nil || rs.Spin.Count != 3 || len(rs.Edited) != 1 || rs.Edited[0].Path != "/repo/a.go" {
		t.Fatalf("snapshot spin %+v edited %v", rs.Spin, rs.Edited)
	}
}
