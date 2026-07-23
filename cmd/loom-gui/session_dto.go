package main

import (
	"github.com/henricktissink/loom/internal/delegate"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

// SessionDTO is the flat, JSON-friendly view of a live session the frontend renders.
// ProjectRoot/ProjectName are SERVER-COMPUTED attribution (§4): the rail
// groups by them. Project stays what it always was — ProjectLabel, a display
// string — and is never an attribution input: it is filepath.Base(cwd) for
// adopted orphans, so two same-basename repos in different projects are
// indistinguishable through it, which the multi-repo model makes routine.
type SessionDTO struct {
	Name        string `json:"name"`
	Project     string `json:"project"`
	ProjectRoot string `json:"projectRoot"`
	ProjectName string `json:"projectName"`
	Title       string `json:"title"`
	Status      string `json:"status"`    // running | needs_you | idle | done | error | unknown
	CtxTokens   int64  `json:"ctxTokens"` // approx context tokens in the last turn; 0 = unknown
	LastTool    string `json:"lastTool"`  // tool the session is currently running, if any
}

// snapshotToDTOs flattens a status.Snapshot's live rows into SessionDTOs,
// dropping the rows §6 hides — the rail is leak surface 1. Filtering happens
// HERE rather than in the engine: the engine keeps polling and transitioning
// hidden sessions (§6.2a), it just stops being rendered. A nil resolver means
// nothing is hidden. Always returns a non-nil slice so it marshals to [].
func snapshotToDTOs(s status.Snapshot, at *delegate.Attributor) []SessionDTO {
	out := make([]SessionDTO, 0, len(s.Live))
	for _, r := range s.Live {
		if !at.Visible(r.SessionRow) {
			continue
		}
		att, _ := at.Attribute(r.SessionRow)
		out = append(out, SessionDTO{
			Name:        r.Name,
			Project:     r.ProjectLabel,
			ProjectRoot: att.Root,
			ProjectName: att.Name,
			Title:       r.Title,
			Status:      string(r.Status),
			CtxTokens:   r.CtxTokens,
			LastTool:    r.LastTool,
		})
	}
	return out
}

// FinishedDTO is the flat view of a finished (ended) session for the rail's
// Finished group — resumable and dismissable.
type FinishedDTO struct {
	Name        string `json:"name"`
	Project     string `json:"project"`
	ProjectRoot string `json:"projectRoot"`
	ProjectName string `json:"projectName"`
	Title       string `json:"title"`
	Status      string `json:"status"` // done | error
	EndedAt     int64  `json:"endedAt"`
	Summary     string `json:"summary"` // stored LLM summary, or "" if none yet
}

// recentToDTOs maps store rows to FinishedDTOs, skipping still-live rows
// (EndedAt < 0) and deriving done/error from the exit code. summaryFor (may be
// nil) supplies each row's stored LLM summary by claude session id. Non-nil
// slice. res (may be nil) applies §6's visibility predicate — the Finished
// list is leak surface 2; the caller over-fetches and trims after this.
func recentToDTOs(rows []store.SessionRow, summaryFor func(string) string, at *delegate.Attributor) []FinishedDTO {
	out := make([]FinishedDTO, 0, len(rows))
	for _, r := range rows {
		if r.EndedAt < 0 {
			continue // still live — belongs in the live rail, not Finished
		}
		if !at.Visible(r) {
			continue
		}
		st := "done"
		if r.ExitCode > 0 {
			st = "error"
		}
		summary := ""
		if summaryFor != nil {
			summary = summaryFor(r.ClaudeSessionID)
		}
		att, _ := at.Attribute(r)
		out = append(out, FinishedDTO{
			Name: r.Name, Project: r.ProjectLabel, Title: r.Title,
			ProjectRoot: att.Root, ProjectName: att.Name,
			Status: st, EndedAt: r.EndedAt, Summary: summary,
		})
	}
	return out
}
