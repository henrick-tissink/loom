package transcript

import (
	"regexp"
	"strings"
)

// Turn stats for drift and stall signals: how long the current turn has
// run, how many tool calls it made, how many test runs in a row failed,
// how many output tokens it spent, and how often the agent opened with
// "you're right" lately (agreeing with a correction). The classifier only
// counts; the GUI decides what is too much (cmd/loom-gui/drift.go).

// TurnStats describes the current turn (see spin.go for what a turn is).
type TurnStats struct {
	Start     int64  // unix seconds of the record that opened it; 0 = unknown
	Calls     int    // tool calls made in it
	TestFails int    // test runs failed in a row, since the last passing one
	LastTest  string // the last failing test command
	OutTokens int64  // output tokens spent in it
	// Agrees counts assistant texts opening with an agreement ("You're
	// absolutely right") in this turn and the agreeWindow-1 before it;
	// AgreeSample is the latest such opening.
	Agrees      int
	AgreeSample string
}

// agreeWindow is how many turns, this one included, Agrees looks back.
const agreeWindow = 5

var (
	agreeRe = regexp.MustCompile(`(?i)^(you'?re|you are) (absolutely |completely |totally |quite |entirely )?(right|correct)\b|^good (catch|point|call)\b|^fair point\b`)
)

// TurnStats is the current turn's counts.
func (c *Classifier) TurnStats() TurnStats {
	t := TurnStats{Start: c.turnStart, Calls: c.turnCalls, TestFails: c.testFails, LastTest: c.lastTest, AgreeSample: c.agreeSample}
	for _, n := range c.outByMsg {
		t.OutTokens += n
	}
	t.OutTokens += c.outLoose
	t.Agrees = c.agreeNow
	for _, n := range c.agreePast {
		t.Agrees += n
	}
	return t
}

// startTurnStats opens a turn's counts; the agreement count of the turn
// that ended moves into the look-back window.
func (c *Classifier) startTurnStats(at int64) {
	c.turnStart, c.turnCalls, c.testFails, c.lastTest = at, 0, 0, ""
	c.outByMsg, c.outLoose = nil, 0
	c.agreePast = append(c.agreePast, c.agreeNow)
	if len(c.agreePast) > agreeWindow-1 {
		c.agreePast = c.agreePast[len(c.agreePast)-(agreeWindow-1):]
	}
	c.agreeNow = 0
	sum := 0
	for _, n := range c.agreePast {
		sum += n
	}
	if sum == 0 {
		c.agreeSample = ""
	}
}

// noteAssistant counts an assistant record's tool calls, output tokens
// (once per API message: claude writes one record per content block, each
// carrying the message's usage) and agreement openers.
func (c *Classifier) noteAssistant(id string, u *usage, blocks []block) {
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			c.turnCalls++
		case "text":
			t := strings.TrimSpace(b.Text)
			if agreeRe.MatchString(t) {
				c.agreeNow++
				l, _, _ := strings.Cut(t, "\n")
				c.agreeSample = cut(strings.TrimSpace(l), sampleLimit)
			}
		}
	}
	if u == nil {
		return
	}
	if id == "" {
		c.outLoose += u.OutputTokens
		return
	}
	if c.outByMsg == nil {
		c.outByMsg = map[string]int64{}
	}
	if u.OutputTokens > c.outByMsg[id] {
		c.outByMsg[id] = u.OutputTokens
	}
}

// noteTestRun folds a finished Bash call that ran tests, judged as the
// review sheet judges it (runs.go testRun): a non-zero exit or a runner's
// failing summary in the output fails it, so a piped `go test | tail`
// that failed counts too. A failure extends the streak, a pass ends it; a
// background run with no result yet, and a call that never ran (declined,
// blocked), change nothing.
func (c *Classifier) noteTestRun(call toolCall, isError bool, text string) {
	if !call.test {
		return
	}
	run, ok := testRun(runCall{call: call, background: call.background}, text, isError, 0)
	if !ok {
		return
	}
	switch run.Status {
	case "failed":
		c.testFails++
		c.lastTest = cut(call.subject, subjectLimit)
	case "passed":
		c.testFails, c.lastTest = 0, ""
	}
}
