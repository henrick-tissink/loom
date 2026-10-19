package ccsessions

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Agent is one background session as `claude agents --json --all` reports
// it. That command is the supported way to read claude's supervisor from
// outside (the files under <ccd>/jobs are documented as unstable), and it is
// the only place a background session that has no process is listed: the
// supervisor stops a finished or idle one after about an hour, and it stays
// in agent view, resumable, until someone runs `claude rm`.
type Agent struct {
	ID        string // the short id `claude attach|stop <id>` take
	SessionID string
	Cwd       string
	Name      string
	State     string // working | blocked | done | failed | stopped
	PID       int    // 0 when no process runs it
	StartedAt int64  // unix milliseconds
}

// ParseAgents reads the JSON array `claude agents --json` prints and keeps
// the background sessions; interactive ones are the registry's to report.
// ok is false for a malformed document.
func ParseAgents(raw []byte) (out []Agent, ok bool) {
	var in []struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
		Name      string `json:"name"`
		State     string `json:"state"`
		Kind      string `json:"kind"`
		PID       int    `json:"pid"`
		StartedAt int64  `json:"startedAt"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, false
	}
	for _, a := range in {
		if a.Kind != "background" || a.ID == "" || a.SessionID == "" {
			continue
		}
		out = append(out, Agent{ID: a.ID, SessionID: a.SessionID, Cwd: a.Cwd, Name: a.Name,
			State: a.State, PID: a.PID, StartedAt: a.StartedAt})
	}
	return out, true
}

// AgentsCache runs `claude agents --json --all` at most once per Every and
// hands out the last good answer in between. The command takes a few hundred
// milliseconds, too slow to run on every 1.5 s poll, and a background
// session's state changes on the scale of turns, not ticks. A failed run
// keeps the previous list: a hiccup must not make every background row
// vanish and reappear.
type AgentsCache struct {
	// Run executes `claude agents --json --all` and returns its stdout.
	Run   func(ctx context.Context) ([]byte, error)
	Every time.Duration

	mu   sync.Mutex
	last []Agent
	at   time.Time
}

// List returns the background sessions, refreshing them when the last run
// is older than Every.
func (c *AgentsCache) List(now time.Time) []Agent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && now.Sub(c.at) < c.Every {
		return c.last
	}
	c.at = now
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := c.Run(ctx)
	if err != nil {
		return c.last
	}
	if got, ok := ParseAgents(raw); ok {
		c.last = got
	}
	return c.last
}

// RunAgents is AgentsCache.Run for the real claude binary on PATH.
func RunAgents(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "claude", "agents", "--json", "--all").Output()
}

// Stop runs `claude stop <jobID>`: the supervisor ends the session's
// process and keeps its conversation, so `claude attach` reopens it.
func Stop(jobID string) error {
	out, err := exec.Command("claude", "stop", jobID).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("claude stop %s: %s", jobID, msg)
		}
		return fmt.Errorf("claude stop %s: %w", jobID, err)
	}
	return nil
}
