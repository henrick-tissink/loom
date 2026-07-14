package main

import (
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

// SessionDTO is the flat, JSON-friendly view of a live session the frontend renders.
type SessionDTO struct {
	Name      string `json:"name"`
	Project   string `json:"project"`
	Title     string `json:"title"`
	Status    string `json:"status"` // running | needs_you | idle | done | error | unknown
	CtxTokens int64  `json:"ctxTokens"` // approx context tokens in the last turn; 0 = unknown
	LastTool  string `json:"lastTool"`  // tool the session is currently running, if any
}

// snapshotToDTOs flattens a status.Snapshot's live rows into SessionDTOs.
// Always returns a non-nil slice so it marshals to [] rather than null.
func snapshotToDTOs(s status.Snapshot) []SessionDTO {
	out := make([]SessionDTO, 0, len(s.Live))
	for _, r := range s.Live {
		out = append(out, SessionDTO{
			Name:      r.Name,
			Project:   r.ProjectLabel,
			Title:     r.Title,
			Status:    string(r.Status),
			CtxTokens: r.CtxTokens,
			LastTool:  r.LastTool,
		})
	}
	return out
}

// FinishedDTO is the flat view of a finished (ended) session for the rail's
// Finished group — resumable and dismissable.
type FinishedDTO struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Title   string `json:"title"`
	Status  string `json:"status"` // done | error
	EndedAt int64  `json:"endedAt"`
	Summary string `json:"summary"` // stored LLM summary, or "" if none yet
}

// recentToDTOs maps store rows to FinishedDTOs, skipping still-live rows
// (EndedAt < 0) and deriving done/error from the exit code. summaryFor (may be
// nil) supplies each row's stored LLM summary by claude session id. Non-nil
// slice.
func recentToDTOs(rows []store.SessionRow, summaryFor func(string) string) []FinishedDTO {
	out := make([]FinishedDTO, 0, len(rows))
	for _, r := range rows {
		if r.EndedAt < 0 {
			continue // still live — belongs in the live rail, not Finished
		}
		st := "done"
		if r.ExitCode > 0 {
			st = "error"
		}
		summary := ""
		if summaryFor != nil {
			summary = summaryFor(r.ClaudeSessionID)
		}
		out = append(out, FinishedDTO{
			Name: r.Name, Project: r.ProjectLabel, Title: r.Title,
			Status: st, EndedAt: r.EndedAt, Summary: summary,
		})
	}
	return out
}
