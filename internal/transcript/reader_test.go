package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReaderMissingFile(t *testing.T) {
	r := NewReader(filepath.Join(t.TempDir(), "nope.jsonl"))
	rs, err := r.Poll()
	if err != nil || rs.State != StateUnknown {
		t.Fatalf("Poll = %v, %v (want Unknown, nil)", rs.State, err)
	}
}

func TestReaderIncremental(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := NewReader(p)

	f.WriteString(lineUserPrompt + "\n")
	if rs, _ := r.Poll(); rs.State != StateIdle {
		t.Fatalf("after prompt: %v, want Idle", rs.State)
	}

	f.WriteString(lineAsstToolUse + "\n")
	if rs, _ := r.Poll(); rs.State != StateRunning || rs.LastTool != "Edit" {
		t.Fatalf("after tool_use: %v/%q, want Running/Edit", rs.State, rs.LastTool)
	}

	// partial line: state must hold until the newline arrives
	half := lineAsstEndTurn[:20]
	f.WriteString(half)
	if rs, _ := r.Poll(); rs.State != StateRunning {
		t.Fatalf("after partial: %v, want Running (unchanged)", rs.State)
	}
	f.WriteString(lineAsstEndTurn[20:] + "\n")
	if rs, _ := r.Poll(); rs.State != StateNeedsYou {
		t.Fatalf("after completion: %v, want NeedsYou", rs.State)
	}
}

func TestReaderTruncationResets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(lineUserPrompt+"\n"+lineAsstEndTurn+"\n"), 0o644)
	r := NewReader(p)
	if rs, _ := r.Poll(); rs.State != StateNeedsYou {
		t.Fatalf("initial: %v", rs.State)
	}
	// replace with a shorter file
	os.WriteFile(p, []byte(lineUserPrompt+"\n"), 0o644)
	if rs, _ := r.Poll(); rs.State != StateIdle {
		t.Fatalf("after truncate: %v, want Idle (reset+reread)", rs.State)
	}
}

func TestReaderSnapshotCarriesTitleAndCtx(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(lineUserPrompt+"\n"+lineAiTitle+"\n"+lineAsstUsage+"\n"), 0o644)
	rs, err := NewReader(p).Poll()
	if err != nil {
		t.Fatal(err)
	}
	if rs.Title != "add vega hedge to strategy" || rs.CtxTokens != 80612 || rs.State != StateNeedsYou {
		t.Fatalf("snapshot = %+v", rs)
	}
}
