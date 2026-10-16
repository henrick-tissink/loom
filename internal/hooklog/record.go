// Package hooklog reads the logs the Agent Discipline hooks write
// (state/log/decisions.jsonl and allows.jsonl) and turns them into the
// numbers loom shows: per session what held it, per hook how often it
// intervenes, per project how often tests are re-run with no change.
//
// Loom never writes these files. The record shape is the hooks' contract
// (bundle/enforce/common.py: log and log_allow); unknown fields are ignored
// and a line that does not parse contributes nothing.
package hooklog

import (
	"encoding/json"
	"strings"
	"time"
)

// Record is one line of either log. Silent is true for allows.jsonl lines,
// which have no decision and no cwd.
type Record struct {
	TS        time.Time `json:"ts"`
	Hook      string    `json:"hook"`
	Event     string    `json:"event"`
	Decision  string    `json:"decision,omitempty"`
	Session   string    `json:"session"`
	AgentType string    `json:"agentType,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
	Command   string    `json:"command,omitempty"`
	Rev       string    `json:"rev,omitempty"`
	Silent    bool      `json:"silent"`
	Legacy    bool      `json:"legacy,omitempty"` // read from a pre-loom location
	// test_rerun: first | yes | no | unknown, and the gap to the previous run.
	Changed   string  `json:"changed,omitempty"`
	SameCmd   bool    `json:"sameCmd,omitempty"`
	SinceLast float64 `json:"sinceLastS,omitempty"`
	// stop_guard: the checks that failed, or the promise phrase it caught.
	Failed []string `json:"failed,omitempty"`
	Phrase string   `json:"phrase,omitempty"`
	// ctx_budget: the context size that triggered the nudge.
	Tokens int64 `json:"tokens,omitempty"`
}

// MaxCommand is how much of a command a record keeps (the hooks already
// cut test commands at 200; bash_guard logs the whole command).
const MaxCommand = 800

// Class buckets a record for the rates: hold (block_*, deny_*), nudge
// (nudge_*), test (test_rerun log_*), silent (allows.jsonl) or info
// (everything else: allow_*, inject_*, checkpoint_*).
func (r Record) Class() string {
	switch {
	case r.Silent:
		return "silent"
	case strings.HasPrefix(r.Decision, "block") || strings.HasPrefix(r.Decision, "deny"):
		return "hold"
	case strings.HasPrefix(r.Decision, "nudge"):
		return "nudge"
	case r.Hook == "test_rerun" && strings.HasPrefix(r.Decision, "log_test"):
		return "test"
	}
	return "info"
}

type rawRecord struct {
	TS        string   `json:"ts"`
	Hook      string   `json:"hook"`
	Event     string   `json:"event"`
	Decision  string   `json:"decision"`
	Session   string   `json:"session"`
	AgentType string   `json:"agent_type"`
	Cwd       string   `json:"cwd"`
	Command   string   `json:"command"`
	Rev       string   `json:"rev"`
	Changed   string   `json:"changed"`
	SameCmd   bool     `json:"same_cmd"`
	SinceLast *float64 `json:"since_last_s"`
	Failed    []string `json:"failed"`
	Phrase    string   `json:"phrase"`
	Tokens    int64    `json:"tokens"`
}

// The hooks stamp local time without a zone.
const tsLayout = "2006-01-02T15:04:05"

// Parse reads one log line. ok is false for a blank, malformed or
// timestamp-less line.
func Parse(line []byte, silent bool) (Record, bool) {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return Record{}, false
	}
	var raw rawRecord
	if err := json.Unmarshal(line, &raw); err != nil || raw.Hook == "" {
		return Record{}, false
	}
	ts, err := time.ParseInLocation(tsLayout, raw.TS, time.Local)
	if err != nil {
		return Record{}, false
	}
	cmd := raw.Command
	if len(cmd) > MaxCommand {
		cmd = cmd[:MaxCommand]
	}
	rec := Record{
		TS: ts, Hook: raw.Hook, Event: raw.Event, Decision: raw.Decision, Session: raw.Session,
		AgentType: raw.AgentType, Cwd: raw.Cwd, Command: cmd, Rev: raw.Rev, Silent: silent,
		Changed: raw.Changed, SameCmd: raw.SameCmd, Failed: raw.Failed, Phrase: raw.Phrase, Tokens: raw.Tokens,
	}
	if raw.SinceLast != nil {
		rec.SinceLast = *raw.SinceLast
	}
	return rec, true
}
