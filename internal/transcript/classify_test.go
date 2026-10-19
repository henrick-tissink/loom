package transcript

import (
	"strings"
	"testing"
)

// Line shapes verified against real ~/.claude transcripts on 2026-07-02.
const (
	lineUserPrompt       = `{"type":"user","message":{"role":"user","content":"add a vega hedge"}}`
	lineAsstToolUse      = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{}}],"stop_reason":"tool_use"}}`
	lineToolResult       = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	lineAsstEndTurn      = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn"}}`
	linePermMode         = `{"type":"permission-mode","permissionMode":"default","sessionId":"x"}`
	lineMode             = `{"type":"mode","mode":"normal","sessionId":"x"}`
	lineSnapshot         = `{"type":"file-history-snapshot","messageId":"m1","snapshot":{},"isSnapshotUpdate":false}`
	lineSystem           = `{"type":"system","subtype":"hook","content":"x"}`
	lineSidechainAst     = `{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"tool_use","id":"s1","name":"Bash","input":{}}]}}`
	lineAiTitle          = `{"type":"ai-title","aiTitle":"add vega hedge to strategy","sessionId":"x"}`
	lineSidechainAiTitle = `{"type":"ai-title","aiTitle":"sidechain title","isSidechain":true,"sessionId":"x"}`
	lineAsstUsage        = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn","usage":{"input_tokens":12,"cache_creation_input_tokens":100,"cache_read_input_tokens":80000,"output_tokens":500}}}`
)

func feed(t *testing.T, lines ...string) *Classifier {
	t.Helper()
	c := &Classifier{}
	for _, l := range lines {
		c.Feed([]byte(l))
	}
	return c
}

func TestEmptyIsUnknown(t *testing.T) {
	if s := (&Classifier{}).State(); s != StateUnknown {
		t.Fatalf("state = %v, want Unknown", s)
	}
}

func TestUserPromptIsIdle(t *testing.T) {
	if s := feed(t, lineUserPrompt).State(); s != StateIdle {
		t.Fatalf("state = %v, want Idle", s)
	}
}

func TestPendingToolUseIsRunning(t *testing.T) {
	c := feed(t, lineUserPrompt, lineAsstToolUse)
	if c.State() != StateRunning {
		t.Fatalf("state = %v, want Running", c.State())
	}
	if c.LastTool() != "Edit" {
		t.Fatalf("LastTool = %q, want Edit", c.LastTool())
	}
}

func TestToolResultIsRunning(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstToolUse, lineToolResult).State(); s != StateRunning {
		t.Fatalf("state = %v, want Running", s)
	}
}

func TestEndTurnIsNeedsYou(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstToolUse, lineToolResult, lineAsstEndTurn).State(); s != StateNeedsYou {
		t.Fatalf("state = %v, want NeedsYou", s)
	}
}

// P0 regression guard (spec §7): sidecar tail must NOT change classification.
func TestSidecarTailDoesNotReclassify(t *testing.T) {
	c := feed(t, lineUserPrompt, lineAsstEndTurn, linePermMode, lineMode, lineSnapshot, lineSystem)
	if c.State() != StateNeedsYou {
		t.Fatalf("state = %v, want NeedsYou (permission-mode is NOT a permission prompt)", c.State())
	}
}

func TestSidechainRecordsIgnored(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstEndTurn, lineSidechainAst).State(); s != StateNeedsYou {
		t.Fatalf("state = %v, want NeedsYou (sidechain ignored)", s)
	}
}

func TestGarbageLineIgnored(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstEndTurn, `{"type":`).State(); s != StateNeedsYou {
		t.Fatalf("state = %v, want NeedsYou (partial line ignored)", s)
	}
}

func TestTitleCaptured(t *testing.T) {
	c := feed(t, lineUserPrompt, lineAiTitle, lineAsstEndTurn)
	if c.Title() != "add vega hedge to strategy" {
		t.Fatalf("Title = %q", c.Title())
	}
	// ai-title is a sidecar: it must not change turn state
	if c.State() != StateNeedsYou {
		t.Fatalf("state = %v, want NeedsYou", c.State())
	}
}

func TestSidechainAiTitleNotCaptured(t *testing.T) {
	c := feed(t, lineUserPrompt, lineSidechainAiTitle, lineAsstEndTurn)
	if c.Title() != "" {
		t.Fatalf("Title = %q, want empty (sidechain ai-title should not set title)", c.Title())
	}
	// sidechain ai-title must not change turn state
	if c.State() != StateNeedsYou {
		t.Fatalf("state = %v, want NeedsYou", c.State())
	}
}

func TestCtxTokensFromLastUsage(t *testing.T) {
	c := feed(t, lineUserPrompt, lineAsstUsage)
	if c.CtxTokens() != 80112 { // 12+100+80000; output tokens are not context
		t.Fatalf("CtxTokens = %d, want 80112", c.CtxTokens())
	}
	if c.State() != StateNeedsYou {
		t.Fatalf("state = %v", c.State())
	}
}

func TestUsageAbsentLeavesCtx(t *testing.T) {
	c := feed(t, lineUserPrompt, lineAsstUsage, lineToolResult, lineAsstToolUse)
	if c.CtxTokens() != 80112 { // later assistant WITHOUT usage keeps prior value
		t.Fatalf("CtxTokens = %d", c.CtxTokens())
	}
}

// --- regression: "needs you while busy" (2026-07-25) ---
// Claude Code emits thinking-only and text-only assistant messages MID-TURN,
// before the tool call lands. They carry stop_reason "tool_use" and have no
// tool_use block. Verified against a real transcript: 332/332 such records were
// followed by more assistant activity, never a user turn — Claude was still
// working. The classifier used to call these needs_you (absence of a tool_use
// block), so loom notified "needs you" while the session was busy.
const (
	lineAsstThinkingMidTurn = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me consider the options"}],"stop_reason":"tool_use"}}`
	lineAsstTextMidTurn     = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Let me check the file first."}],"stop_reason":"tool_use"}}`
)

func TestThinkingMidTurnIsRunning(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstThinkingMidTurn).State(); s != StateRunning {
		t.Fatalf("thinking mid-turn (stop_reason tool_use): state = %v, want Running (still busy)", s)
	}
}

func TestTextMidTurnIsRunning(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstTextMidTurn).State(); s != StateRunning {
		t.Fatalf("text mid-turn (stop_reason tool_use): state = %v, want Running (still busy)", s)
	}
}

func TestEndTurnTextIsNeedsYou(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstEndTurn).State(); s != StateNeedsYou {
		t.Fatalf("end_turn text: state = %v, want NeedsYou (genuine wait)", s)
	}
}

// A thinking-only message can carry stop_reason "end_turn" yet is still
// mid-turn — the text answer (or a tool) always follows. Verified 225/225 such
// records were followed by more assistant activity, never a user turn.
const lineAsstThinkingEndTurn = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"final considerations"}],"stop_reason":"end_turn"}}`

func TestThinkingEndTurnIsRunning(t *testing.T) {
	if s := feed(t, lineUserPrompt, lineAsstThinkingEndTurn).State(); s != StateRunning {
		t.Fatalf("thinking-only end_turn: state = %v, want Running (text answer still to come)", s)
	}
}

// TestGoal follows the record shapes claude 2.1.303 wrote for a real /goal:
// set, checks, a pause, a resume, met; then a second goal cleared early.
func TestGoal(t *testing.T) {
	att := func(ts, body string) string {
		return `{"type":"attachment","timestamp":"2026-10-19T11:43:` + ts + `Z","attachment":{"type":"goal_status",` + body + `}}`
	}
	c := feed(t,
		att("19.0", `"met":false,"sentinel":true,"condition":"C"`),
		lineUserPrompt,
		att("29.0", `"met":false,"condition":"C","reason":"not yet:\n the file is missing"`),
		att("33.0", `"met":false,"condition":"C","reason":"still missing"`),
	)
	if g := c.Goal(); g.State != "active" || g.Checks != 2 || g.Reason != "still missing" || g.Condition != "C" {
		t.Fatalf("active: %+v", g)
	}
	c.Feed([]byte(`{"type":"system","subtype":"informational","timestamp":"2026-10-19T11:43:53Z","content":"Goal paused · goal checks kept finding it unmet this turn · send a message to continue"}`))
	if g := c.Goal(); g.State != "paused" || !strings.HasPrefix(g.Reason, "Goal paused") {
		t.Fatalf("paused: %+v", g)
	}
	c.Feed([]byte(`{"type":"user","isMeta":true,"message":{"role":"user","content":"meta"}}`))
	if c.Goal().State != "paused" {
		t.Fatal("a meta record resumed the goal")
	}
	c.Feed([]byte(lineUserPrompt))
	c.Feed([]byte(att("59.0", `"met":true,"condition":"C","reason":"done","iterations":4`)))
	if g := c.Goal(); g.State != "met" || g.Checks != 4 {
		t.Fatalf("met: %+v", g)
	}
	c.Feed([]byte(att("59.5", `"met":false,"sentinel":true,"condition":"D"`)))
	c.Feed([]byte(att("59.9", `"met":true,"sentinel":true,"condition":"D"`)))
	if g := c.Goal(); g.State != "cleared" || g.Condition != "D" || g.Checks != 0 {
		t.Fatalf("cleared: %+v", g)
	}
	if c.State() != StateIdle {
		t.Fatalf("goal records moved the turn state: %v", c.State())
	}
}
