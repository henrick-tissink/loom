package main

import (
	"fmt"
	"strings"

	"github.com/henricktissink/loom/internal/status"
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

type railEntry struct {
	dto SessionDTO
	row status.Row
}

// noteRail keeps the poll's visible rows (the DTOs; hidden rows have none)
// with their engine rows.
func (a *App) noteRail(dtos []SessionDTO, rows []status.Row) {
	byName := make(map[string]status.Row, len(rows))
	for _, r := range rows {
		byName[r.Name] = r
	}
	m := make(map[string]railEntry, len(dtos))
	for _, d := range dtos {
		m[d.Name] = railEntry{dto: d, row: byName[d.Name]}
	}
	a.railMu.Lock()
	a.rail = m
	a.railMu.Unlock()
}

// railRow is a visible row of the last poll.
func (a *App) railRow(name string) (railEntry, bool) {
	a.railMu.Lock()
	defer a.railMu.Unlock()
	e, ok := a.rail[name]
	return e, ok
}

// NudgePrompt composes the Nudge message for a spinning session, for the
// operator to edit and send.
func (a *App) NudgePrompt(name string) (string, error) {
	e, ok := a.railRow(name)
	if !ok || e.dto.Spin == nil {
		return "", fmt.Errorf("the session is no longer repeating a failure")
	}
	return nudgePrompt(*e.dto.Spin), nil
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
