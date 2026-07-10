package main

import "github.com/henricktissink/loom/internal/status"

// SessionDTO is the flat, JSON-friendly view of a live session the frontend renders.
type SessionDTO struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Title   string `json:"title"`
	Status  string `json:"status"` // running | needs_you | idle | done | error | unknown
}

// snapshotToDTOs flattens a status.Snapshot's live rows into SessionDTOs.
// Always returns a non-nil slice so it marshals to [] rather than null.
func snapshotToDTOs(s status.Snapshot) []SessionDTO {
	out := make([]SessionDTO, 0, len(s.Live))
	for _, r := range s.Live {
		out = append(out, SessionDTO{
			Name:    r.Name,
			Project: r.ProjectLabel,
			Title:   r.Title,
			Status:  string(r.Status),
		})
	}
	return out
}
