package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReaderMissingFile(t *testing.T) {
	r := NewReader(filepath.Join(t.TempDir(), "nope.jsonl"))
	s, _, err := r.Poll()
	if err != nil || s != StateUnknown {
		t.Fatalf("Poll = %v, %v (want Unknown, nil)", s, err)
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
	if s, _, _ := r.Poll(); s != StateIdle {
		t.Fatalf("after prompt: %v, want Idle", s)
	}

	f.WriteString(lineAsstToolUse + "\n")
	if s, tool, _ := r.Poll(); s != StateRunning || tool != "Edit" {
		t.Fatalf("after tool_use: %v/%q, want Running/Edit", s, tool)
	}

	// partial line: state must hold until the newline arrives
	half := lineAsstEndTurn[:20]
	f.WriteString(half)
	if s, _, _ := r.Poll(); s != StateRunning {
		t.Fatalf("after partial: %v, want Running (unchanged)", s)
	}
	f.WriteString(lineAsstEndTurn[20:] + "\n")
	if s, _, _ := r.Poll(); s != StateNeedsYou {
		t.Fatalf("after completion: %v, want NeedsYou", s)
	}
}

func TestReaderTruncationResets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(lineUserPrompt+"\n"+lineAsstEndTurn+"\n"), 0o644)
	r := NewReader(p)
	if s, _, _ := r.Poll(); s != StateNeedsYou {
		t.Fatalf("initial: %v", s)
	}
	// replace with a shorter file
	os.WriteFile(p, []byte(lineUserPrompt+"\n"), 0o644)
	if s, _, _ := r.Poll(); s != StateIdle {
		t.Fatalf("after truncate: %v, want Idle (reset+reread)", s)
	}
}
