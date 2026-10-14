package status

import (
	"testing"

	"github.com/henricktissink/loom/internal/transcript"
)

func TestFuse(t *testing.T) {
	cases := []struct {
		ts     transcript.State
		active bool
		want   Status
	}{
		{transcript.StateRunning, false, Running},
		{transcript.StateRunning, true, Running},
		{transcript.StateNeedsYou, false, NeedsYou},
		{transcript.StateNeedsYou, true, Running}, // pane still emitting: a background task is running, not waiting on you
		{transcript.StateIdle, true, Running},     // streaming: JSONL lags the pane
		{transcript.StateIdle, false, Idle},
		{transcript.StateUnknown, true, Running},
		{transcript.StateUnknown, false, Unknown},
	}
	for _, c := range cases {
		if got := Fuse(c.ts, c.active); got != c.want {
			t.Errorf("Fuse(%v, %v) = %v, want %v", c.ts, c.active, got, c.want)
		}
	}
}

func TestMerge(t *testing.T) {
	cases := []struct {
		fused      Status
		reg, wf    string
		want       Status
		wantReason string
	}{
		{NeedsYou, "busy", "", Running, ""}, // parent waiting on subagents
		{Unknown, "busy", "", Running, ""},
		{Running, "waiting", "dialog open", NeedsYou, "dialog open"},
		{Idle, "waiting", "", NeedsYou, ""},
		{Unknown, "idle", "", Idle, ""}, // never prompted: no transcript yet
		{NeedsYou, "idle", "", NeedsYou, ""},
		{Running, "idle", "", Running, ""},
		{NeedsYou, "shell", "", NeedsYou, "shell"},
		{NeedsYou, "", "", NeedsYou, ""},
		{Running, "weird", "x", Running, ""},
	}
	for _, c := range cases {
		got, reason := Merge(c.fused, c.reg, c.wf)
		if got != c.want || reason != c.wantReason {
			t.Errorf("Merge(%v, %q, %q) = %v, %q; want %v, %q", c.fused, c.reg, c.wf, got, reason, c.want, c.wantReason)
		}
	}
}
