// Package ccsessions reads Claude Code's own session registry: one JSON file
// per live `claude` process under <claude config dir>/sessions/<pid>.json.
//
// It exists for one reason. Loom names a tmux session after the claude
// session id it launched with and tails that id's transcript. When `/clear`
// or an in-pane resume runs, claude mints a NEW session id and a NEW
// transcript file, and nothing in the old transcript says so — the pane's
// row would freeze on whatever status the old file last showed. The
// registry is the only place the pane's CURRENT id is written, keyed by the
// tmux target claude observed itself running in.
//
// The file shape is undocumented (observed on Claude Code 2.1.29x), so this
// reader is deliberately tolerant: a missing directory, an unreadable or
// malformed file, an entry without a tmux target, or an entry whose pid is
// gone all simply contribute nothing.
package ccsessions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Entry is the subset of a registry file loom reads.
type Entry struct {
	PID         int
	SessionID   string
	Cwd         string
	TmuxTarget  string // "session:@window.%pane" as claude writes it
	TmuxSession string // the session part of TmuxTarget
	Status      string // busy | idle | waiting | shell (observed values)
	WaitingFor  string
	Name        string
	UpdatedAt   int64 // unix milliseconds
	// StatusUpdatedAt is when Status last changed (unix milliseconds): for a
	// waiting pane, how long it has been waiting.
	StatusUpdatedAt int64
	StartedAt       int64 // unix milliseconds the process started
	// Kind is how claude was started: "interactive", or "bg" for a
	// background session run by claude's own supervisor (`claude --bg`,
	// agent view). JobID is a background session's short id, the one
	// `claude attach|stop <id>` take; "" for every other kind.
	Kind  string
	JobID string
	// BridgeSessionID is set while the session is under Remote Control
	// (/remote-control): its claude.ai/code session id, "session_…".
	// claude removes it on disconnect.
	BridgeSessionID string
}

// RemoteURL is where a session under Remote Control is opened on the web
// and in the Claude app, or "" when it is not under Remote Control (or the
// id is not one claude writes, so no odd string ever becomes a link).
func (e Entry) RemoteURL() string {
	id := e.BridgeSessionID
	if !strings.HasPrefix(id, "session_") || len(id) > 100 {
		return ""
	}
	for _, c := range id {
		if !(c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return ""
		}
	}
	return "https://claude.ai/code/" + id
}

// Dir is the registry directory under a claude config dir.
func Dir(claudeConfigDir string) string {
	return filepath.Join(claudeConfigDir, "sessions")
}

// pidAlive reports whether a process exists. Overridable so tests can mark
// an entry dead without needing a pid that is guaranteed free.
var pidAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	// EPERM means the process exists but belongs to someone else — alive.
	return err == nil || errors.Is(err, syscall.EPERM)
}

// List scans dir and returns every entry whose process is still alive,
// under tmux or not. A registry file can outlive its process (claude killed
// without cleanup), so liveness filters here, once, for every reader.
func List(dir string) []Entry {
	var out []Entry
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		e, ok := readFile(filepath.Join(dir, de.Name()))
		if !ok || !pidAlive(e.PID) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// ByTmuxSession returns, per tmux SESSION name, the freshest live entry. A
// pane can be re-entered by a new process (`claude --resume` typed into the
// same window), so updatedAt breaks ties. Entries not under tmux have
// nothing to key on and are left out; List has them.
func ByTmuxSession(dir string) map[string]Entry {
	out := map[string]Entry{}
	for _, e := range List(dir) {
		if e.TmuxSession == "" {
			continue
		}
		if prev, dup := out[e.TmuxSession]; dup && prev.UpdatedAt >= e.UpdatedAt {
			continue
		}
		out[e.TmuxSession] = e
	}
	return out
}

func readFile(path string) (Entry, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, false
	}
	var f struct {
		PID        int    `json:"pid"`
		SessionID  string `json:"sessionId"`
		Cwd        string `json:"cwd"`
		Tmux       string `json:"tmux"`
		Status     string `json:"status"`
		WaitingFor string `json:"waitingFor"`
		Name       string `json:"name"`
		UpdatedAt  int64  `json:"updatedAt"`
		StatusAt   int64  `json:"statusUpdatedAt"`
		StartedAt  int64  `json:"startedAt"`
		Kind       string `json:"kind"`
		JobID      string `json:"jobId"`
		Bridge     string `json:"bridgeSessionId"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.SessionID == "" {
		return Entry{}, false
	}
	e := Entry{PID: f.PID, SessionID: f.SessionID, Cwd: f.Cwd, TmuxTarget: f.Tmux,
		Status: f.Status, WaitingFor: f.WaitingFor, Name: f.Name, UpdatedAt: f.UpdatedAt,
		StatusUpdatedAt: f.StatusAt, StartedAt: f.StartedAt, Kind: f.Kind, JobID: f.JobID,
		BridgeSessionID: f.Bridge}
	if i := strings.IndexByte(f.Tmux, ':'); i > 0 {
		e.TmuxSession = f.Tmux[:i]
	} else {
		e.TmuxSession = f.Tmux
	}
	return e, true
}
