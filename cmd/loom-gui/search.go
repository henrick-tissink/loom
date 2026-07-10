package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/henricktissink/loom/internal/store"
)

// SearchHitDTO is one memory-search result for the search modal.
type SearchHitDTO struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	Project   string `json:"project"`
	Ask       string `json:"ask"`
	Snippet   string `json:"snippet"` // FTS snippet; \x01..\x02 wrap matched terms
	Cwd       string `json:"cwd"`
}

func searchHitsToDTOs(hits []store.SearchHit) []SearchHitDTO {
	out := make([]SearchHitDTO, 0, len(hits))
	for _, h := range hits {
		proj := ""
		if h.Cwd != "" {
			proj = filepath.Base(h.Cwd)
		}
		out = append(out, SearchHitDTO{
			SessionID: h.SessionID,
			Title:     h.Title,
			Project:   proj,
			Ask:       h.Ask,
			Snippet:   flattenLines(h.Snippet),
			Cwd:       h.Cwd,
		})
	}
	return out
}

func flattenLines(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

// SearchSessions runs a full-text search over the indexed session history.
func (a *App) SearchSessions(query string) []SearchHitDTO {
	out := []SearchHitDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	hits, err := a.st.SearchSessions(query, 25)
	if err != nil {
		return out
	}
	return searchHitsToDTOs(hits)
}

// ResumeSearchHit resumes a past session found via search — `claude --resume
// <id>` in its recorded cwd — and returns the new tmux session name.
func (a *App) ResumeSearchHit(sessionID, cwd string) (string, error) {
	if a.launcher == nil {
		return "", fmt.Errorf("resume unavailable")
	}
	if sessionID == "" {
		return "", fmt.Errorf("missing session id")
	}
	if cwd == "" {
		// Without a cwd, tmux would silently start in the server's own directory
		// rather than the intended project — refuse instead of resuming wrong.
		return "", fmt.Errorf("session has no recorded directory")
	}
	row := store.SessionRow{ClaudeSessionID: sessionID, Cwd: cwd, ProjectLabel: filepath.Base(cwd)}
	return a.launcher.Resume(row, 120, 32, a.now())
}
