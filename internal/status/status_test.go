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
		{transcript.StateNeedsYou, true, NeedsYou},
		{transcript.StateIdle, true, Running}, // streaming: JSONL lags the pane
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
