package main

import (
	"fmt"
	"strings"

	"github.com/henricktissink/loom/internal/transcript"
)

// A session "spinning": the same tool failure, three or more times in the
// current turn (transcript/spin.go). The row says so, and Nudge offers a
// message asking the agent to stop and rethink before it tries again.

// SpinDTO is a session's repeating failure.
type SpinDTO struct {
	Tool    string `json:"tool"`
	Count   int    `json:"count"`
	Sample  string `json:"sample"`  // the failure's telling line
	Subject string `json:"subject"` // the last failing command or file
	First   int64  `json:"first"`   // unix seconds
	Last    int64  `json:"last"`
}

func spinDTO(s *transcript.Spin) *SpinDTO {
	if s == nil {
		return nil
	}
	return &SpinDTO{Tool: s.Tool, Count: s.Count, Sample: s.Sample, Subject: s.Subject, First: s.First, Last: s.Last}
}

// noteSpins keeps the rail's current spins for NudgePrompt, which composes
// from what the operator is looking at rather than polling again (a poll
// outside ListSessions would swallow a needs-you bell).
func (a *App) noteSpins(dtos []SessionDTO) {
	m := map[string]SpinDTO{}
	for _, d := range dtos {
		if d.Spin != nil {
			m[d.Name] = *d.Spin
		}
	}
	a.spinMu.Lock()
	a.spins = m
	a.spinMu.Unlock()
}

// NudgePrompt composes the Nudge message for a spinning session, for the
// operator to edit and send.
func (a *App) NudgePrompt(name string) (string, error) {
	a.spinMu.Lock()
	sp, ok := a.spins[name]
	a.spinMu.Unlock()
	if !ok {
		return "", fmt.Errorf("the session is no longer repeating a failure")
	}
	return nudgePrompt(sp), nil
}

// nudgePrompt names the failure and asks the three questions that break a
// retry loop (what failed, what would fix it, is this the same approach
// again), with a way out: report the blocker instead of another attempt.
func nudgePrompt(s SpinDTO) string {
	what := s.Tool
	if s.Subject != "" {
		if s.Tool == "Bash" {
			what += ": " + "`" + s.Subject + "`"
		} else {
			what += " on " + s.Subject
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You have hit the same failure %d times in this turn (%s):\n\n", s.Count, what)
	if s.Sample != "" {
		fmt.Fprintf(&b, "    %s\n\n", s.Sample)
	}
	b.WriteString("Before you try again, stop and answer briefly:\n")
	b.WriteString("1. What exactly failed, and why?\n")
	b.WriteString("2. What specific change would fix it?\n")
	b.WriteString("3. Are you repeating an approach that has already failed?\n\n")
	b.WriteString("If you can't name a different approach, stop retrying and tell me what is blocking you.")
	return b.String()
}
