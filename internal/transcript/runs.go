package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// What a session did, read from its whole transcript on demand (the review
// sheet), not on every poll: the test commands it ran with how they ended,
// and the files it wrote through its file tools.

// testCmd and notTest are the hooks' test_rerun patterns (TEST_CMD and
// NOT_TEST in enforce/test_rerun_log.py), so loom and the hooks agree on
// what a test run is.
var (
	testCmd = regexp.MustCompile(`(?:^|[\s;&|(]|bash\s+-c\s+['"])\s*(?:` +
		`(?:python3?|py)\s+-m\s+(?:pytest|unittest)` +
		`|pytest|py\.test|unittest` +
		`|(?:npm|pnpm|yarn|bun)\s+(?:run\s+)?test(?:s|:\w+|-\w+)?\b` +
		`|(?:npx|pnpx|bunx)\s+(?:vitest|jest|mocha|playwright\s+test|ava|tap|uvu)` +
		`|vitest|jest|mocha` +
		`|cargo\s+(?:test|nextest)` +
		`|go\s+test` +
		`|dotnet\s+test` +
		`|mix\s+test` +
		`|rspec|(?:bundle\s+exec\s+)?rake\s+test` +
		`|make\s+(?:test|check)\b` +
		`|node\s+--test` +
		`|swift\s+test` +
		`|ctest` +
		`|tox\b` +
		`|test_\w+\.py\b|\w+_test\.py\b` +
		`)`)
	notTest = regexp.MustCompile(`^\s*(?:cat|grep|tail|head|less|ls|echo|sed|awk|rg)\b`)
)

// IsTestCommand reports whether a shell command runs tests, by the hooks'
// definition, read on the command's first line only (with its backslash
// continuations): a heredoc's body is a script's text, and a script that
// edits spin_test.go or mentions `go test` is not a test run.
func IsTestCommand(cmd string) bool {
	cmd = firstLine(cmd)
	return testCmd.MatchString(cmd) && !notTest.MatchString(cmd)
}

func firstLine(cmd string) string {
	var b strings.Builder
	for _, ln := range strings.Split(strings.TrimSpace(cmd), "\n") {
		if l, cont := strings.CutSuffix(strings.TrimRight(ln, " \t"), "\\"); cont {
			b.WriteString(l)
			b.WriteByte(' ')
			continue
		}
		b.WriteString(ln)
		break
	}
	return b.String()
}

var (
	noteUseID  = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)
	noteStatus = regexp.MustCompile(`<status>([^<]+)</status>`)
	noteExit   = regexp.MustCompile(`\(exit code (\d+)\)`)
	// failSummary is a test runner's failing summary line: go (FAIL,
	// --- FAIL), pytest (N failed, FAILED), jest/vitest (Tests: N failed),
	// cargo (test result: FAILED), dotnet (Failed!).
	failSummary = regexp.MustCompile(`(?m)^\s*(?:FAIL(?:\s|:|$)|--- FAIL|FAILED\b|Failed!|test result: FAILED)|\b[1-9]\d* failed\b`)
)

// TestRun is one test command the session ran.
type TestRun struct {
	Command string `json:"command"`
	At      int64  `json:"at"` // unix seconds of its result
	// Exit is the command's exit code, -1 when unknown (it ran in the
	// background and no result has come).
	Exit int `json:"exit"`
	// Status is passed, failed or unknown: failed when it exited non-zero
	// or its output reads as a failing test summary (a `| tail` makes a
	// failing run exit 0).
	Status     string `json:"status"`
	Background bool   `json:"background,omitempty"`
	Sample     string `json:"sample"` // the telling line: an error, or the last line of a pass
}

// Activity is what the transcript shows the session did since a moment.
type Activity struct {
	Runs  []TestRun // oldest first, at most MaxRuns (the latest)
	Total int       // every test run since, Runs or not
	Edits []Edit    // files written through file tools, latest time per file, sorted by path
}

// MaxRuns is how many test runs Activity keeps.
const MaxRuns = 20

type runCall struct {
	call       toolCall
	command    string // as written, newlines kept
	background bool
}

// ScanActivity reads a transcript and returns its test runs and edits from
// since (unix seconds; 0 = all). A missing file is an empty Activity.
func ScanActivity(path string, since int64) (Activity, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Activity{}, nil
		}
		return Activity{}, err
	}
	defer f.Close()
	var act Activity
	calls := map[string]runCall{}
	edited := map[string]int64{}
	var runs []TestRun
	pending := map[string]int{} // a background run's tool use id → its index in runs
	br := bufio.NewReaderSize(f, 1<<16)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			var r record
			if json.Unmarshal(line, &r) == nil {
				at := parseTS(r.Timestamp)
				blocks := parseBlocks(r)
				if r.Type == "assistant" {
					for _, b := range blocks {
						if b.Type != "tool_use" || b.ID == "" {
							continue
						}
						var in struct {
							Command    string `json:"command"`
							Background bool   `json:"run_in_background"`
						}
						_ = json.Unmarshal(b.Input, &in)
						calls[b.ID] = runCall{call: callOf(b, r.Cwd), command: in.Command, background: in.Background}
					}
				} else if since == 0 || at >= since {
					noteFinished(r, runs, pending)
					for _, b := range blocks {
						if b.Type != "tool_result" {
							continue
						}
						rc, ok := calls[b.ToolUseID]
						if !ok {
							continue
						}
						delete(calls, b.ToolUseID)
						switch {
						case rc.call.path != "" && !b.IsError:
							edited[rc.call.path] = at
						case rc.call.name == "Bash" && IsTestCommand(rc.command):
							if run, ok := testRun(rc, contentText(b.Content), b.IsError, at); ok {
								if run.Exit == -1 && rc.background {
									pending[b.ToolUseID] = len(runs)
								}
								runs = append(runs, run)
							}
						}
					}
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return act, rerr
		}
	}
	act.Total = len(runs)
	if len(runs) > MaxRuns {
		runs = runs[len(runs)-MaxRuns:]
	}
	act.Runs = runs
	for p, at := range edited {
		act.Edits = append(act.Edits, Edit{Path: p, At: at})
	}
	sortEdits(act.Edits)
	return act, nil
}

// testRun reads a Bash result. ok is false for a run that never happened:
// the person declined it, a hook or the classifier blocked it.
func testRun(rc runCall, text string, isErr bool, at int64) (TestRun, bool) {
	text = strings.TrimSpace(toolErrTag.ReplaceAllString(text, ""))
	low := strings.ToLower(text)
	if isErr {
		for _, s := range notTheAgents {
			if strings.Contains(low, s) {
				return TestRun{}, false
			}
		}
		if strings.HasPrefix(low, "blocked:") {
			return TestRun{}, false
		}
	}
	run := TestRun{Command: cut(rc.call.subject, 300), At: at, Background: rc.background}
	lines := strings.Split(text, "\n")
	if m := exitLine.FindStringSubmatch(strings.TrimSpace(lines[0])); m != nil {
		run.Exit, _ = strconv.Atoi(m[1])
		text = strings.Join(lines[1:], "\n")
	} else if isErr {
		run.Exit = 1
	}
	switch {
	case rc.background && !isErr:
		run.Exit, run.Status = -1, "unknown"
		run.Sample = "ran in the background; no result yet"
	case run.Exit != 0:
		run.Status = "failed"
		run.Sample = cut(tellingLine(text), 160)
	case failSummary.MatchString(text):
		run.Status = "failed"
		run.Sample = cut(failLine(text), 160)
	default:
		run.Status = "passed"
		run.Sample = cut(lastLine(text), 160)
	}
	return run, true
}

func failLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if failSummary.MatchString(ln) {
			return strings.Join(strings.Fields(ln), " ")
		}
	}
	return tellingLine(s)
}

// noteFinished reads a background command's task notification (a user
// record whose content is a string) into the run it finished.
func noteFinished(r record, runs []TestRun, pending map[string]int) {
	if len(pending) == 0 {
		return
	}
	// a user record's message, or a queue-operation's own content
	raw := r.Content
	if r.Message != nil {
		raw = r.Message.Content
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || !strings.Contains(s, "<task-notification>") {
		return
	}
	m := noteUseID.FindStringSubmatch(s)
	if m == nil {
		return
	}
	i, ok := pending[m[1]]
	if !ok {
		return
	}
	delete(pending, m[1])
	run := &runs[i]
	run.At = parseTS(r.Timestamp)
	if e := noteExit.FindStringSubmatch(s); e != nil {
		run.Exit, _ = strconv.Atoi(e[1])
		run.Status = "passed"
		if run.Exit != 0 {
			run.Status = "failed"
		}
		run.Sample = "ran in the background: exit " + e[1]
		return
	}
	if st := noteStatus.FindStringSubmatch(s); st != nil {
		run.Sample = "ran in the background: " + st[1] // killed, stopped: still unknown
	}
}

func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(ls) - 1; i >= 0; i-- {
		if l := strings.Join(strings.Fields(ls[i]), " "); l != "" && !strings.HasPrefix(l, "Shell cwd was reset") {
			return l
		}
	}
	return ""
}
