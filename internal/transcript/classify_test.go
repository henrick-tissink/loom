package transcript

import "testing"

// Line shapes verified against real ~/.claude transcripts on 2026-07-02.
const (
	lineUserPrompt   = `{"type":"user","message":{"role":"user","content":"add a vega hedge"}}`
	lineAsstToolUse  = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{}}],"stop_reason":"tool_use"}}`
	lineToolResult   = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	lineAsstEndTurn  = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn"}}`
	linePermMode     = `{"type":"permission-mode","permissionMode":"default","sessionId":"x"}`
	lineMode         = `{"type":"mode","mode":"normal","sessionId":"x"}`
	lineSnapshot     = `{"type":"file-history-snapshot","messageId":"m1","snapshot":{},"isSnapshotUpdate":false}`
	lineSystem       = `{"type":"system","subtype":"hook","content":"x"}`
	lineSidechainAst = `{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"tool_use","id":"s1","name":"Bash","input":{}}]}}`
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
