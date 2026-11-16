package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/transcript"
)

// Drift and stall signals beside the spinning chip, from the current
// turn's counts (transcript/drift.go). No detector of this kind has
// published precision, so the chip is advisory and the caps are fixed:
// three failing test runs is the "three strikes" rule; the rest are set
// well past an ordinary turn.

const (
	driftTestFails = 3
	driftLongTurn  = 45 * time.Minute
	driftCalls     = 150
	driftOutTokens = 150_000
	driftAgrees    = 2
)

// DriftDTO is one signal.
type DriftDTO struct {
	Kind   string `json:"kind"`   // tests | long | calls | tokens | agree
	Label  string `json:"label"`  // short, for the chip
	Detail string `json:"detail"` // a sentence, for the tooltip and the Nudge
}

// driftSignals reads a live session's turn. The turn's own counts (tests,
// length, calls, tokens) count only while it is still running: a turn that
// handed back is done, and a Nudge saying "before you continue" would come
// after the fact. Agreement spans turns, so it shows at any status. A spin
// already says "the same failure again", so the test signal stays out of
// its way.
func driftSignals(t transcript.TurnStats, status string, spinning bool, now time.Time) []DriftDTO {
	var out []DriftDTO
	running := status == "running"
	if running && t.TestFails >= driftTestFails && !spinning {
		out = append(out, DriftDTO{Kind: "tests", Label: fmt.Sprintf("tests failing ×%d", t.TestFails),
			Detail: fmt.Sprintf("%d test runs in a row have failed this turn (last: `%s`).", t.TestFails, t.LastTest)})
	}
	if running && t.Start > 0 {
		if d := now.Sub(time.Unix(t.Start, 0)); d >= driftLongTurn {
			out = append(out, DriftDTO{Kind: "long", Label: "turn " + fmtDur(d),
				Detail: fmt.Sprintf("This turn has run for %s without handing back.", fmtDur(d))})
		}
	}
	if running && t.Calls >= driftCalls {
		out = append(out, DriftDTO{Kind: "calls", Label: fmt.Sprintf("%d tool calls", t.Calls),
			Detail: fmt.Sprintf("This turn has made %d tool calls.", t.Calls)})
	}
	if running && t.OutTokens >= driftOutTokens {
		out = append(out, DriftDTO{Kind: "tokens", Label: fmt.Sprintf("%dk tokens out", t.OutTokens/1000),
			Detail: fmt.Sprintf("This turn has written %dk output tokens.", t.OutTokens/1000)})
	}
	if t.Agrees >= driftAgrees {
		out = append(out, DriftDTO{Kind: "agree", Label: fmt.Sprintf("agreeing ×%d", t.Agrees),
			Detail: fmt.Sprintf("It has opened with agreement %d times in its last few turns (latest: %q).", t.Agrees, t.AgreeSample)})
	}
	return out
}

func fmtDur(d time.Duration) string {
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// driftPrompt asks for evidence and one verifiable next step, or the
// blocker: an out-of-band check-in, never "keep going".
func driftPrompt(ds []DriftDTO) string {
	var b strings.Builder
	b.WriteString("loom flagged this session:\n")
	agree := false
	for _, d := range ds {
		fmt.Fprintf(&b, "- %s\n", d.Detail)
		agree = agree || d.Kind == "agree"
	}
	b.WriteString("\nBefore you continue, stop and reply briefly:\n")
	b.WriteString("1. What is done so far, with the evidence: the command you ran and what it printed.\n")
	b.WriteString("2. The one next step, and the check that will show it worked.\n")
	if agree {
		b.WriteString("3. Where you agreed with a correction: what you are now doing differently, and how you will verify it rather than take my word for it.\n")
	}
	b.WriteString("\nIf you are blocked, or unsure the approach is right, say so and stop instead of trying something else.")
	return b.String()
}
