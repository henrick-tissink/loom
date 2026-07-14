// Package status fuses transcript state with tmux liveness (spec §4.3, §6).
package status

import "github.com/henricktissink/loom/internal/transcript"

type Status string

const (
	Running  Status = "running"
	NeedsYou Status = "needs_you"
	Idle     Status = "idle"
	Done     Status = "done"
	Error    Status = "error"
	Unknown  Status = "unknown"
)

// Fuse combines the JSONL turn-boundary state with pane activity. Best-effort
// by design (spec §6): wrong fusion degrades a status label, never a session.
func Fuse(t transcript.State, paneActive bool) Status {
	switch t {
	case transcript.StateRunning:
		return Running
	case transcript.StateNeedsYou:
		if paneActive {
			// The turn ended in the JSONL, but the pane is still producing
			// output — a background task (e.g. a background shell) is still
			// running, or Claude is mid-render. The session isn't waiting on
			// you yet; only assert needs_you once the pane goes quiet. Idle
			// sessions at the prompt emit nothing, so this never suppresses a
			// genuine needs_you — it just debounces it past the active window.
			return Running
		}
		return NeedsYou
	case transcript.StateIdle:
		if paneActive {
			return Running // streaming: the pane is moving, JSONL lags
		}
		return Idle
	default:
		if paneActive {
			return Running
		}
		return Unknown
	}
}
