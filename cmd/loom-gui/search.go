package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/store"
)

// SearchHitDTO is one memory-search result for the search modal.
type SearchHitDTO struct {
	SessionID   string `json:"sessionId"`
	Title       string `json:"title"`
	Project     string `json:"project"` // cwd basename; display-only, like SessionDTO.Project
	ProjectRoot string `json:"projectRoot"`
	ProjectName string `json:"projectName"`
	Ask         string `json:"ask"`
	Snippet     string `json:"snippet"` // FTS snippet; \x01..\x02 wrap matched terms
	Cwd         string `json:"cwd"`
}

// searchHitsToDTOs maps hits and drops the ones §6 hides (leak surface 3).
// A search hit carries only a cwd — a transcript has no add_dirs — so
// visibility is evaluated over that alone; an empty cwd fails closed.
func searchHitsToDTOs(hits []store.SearchHit, res *projects.Resolver) []SearchHitDTO {
	out := make([]SearchHitDTO, 0, len(hits))
	for _, h := range hits {
		if !visible(res, searchDirs(h)...) {
			continue
		}
		proj := ""
		if h.Cwd != "" {
			proj = filepath.Base(h.Cwd)
		}
		at := attribute(res, h.Cwd)
		out = append(out, SearchHitDTO{
			SessionID:   h.SessionID,
			Title:       h.Title,
			Project:     proj,
			ProjectRoot: at.Root,
			ProjectName: at.Name,
			Ask:         h.Ask,
			Snippet:     flattenLines(h.Snippet),
			Cwd:         h.Cwd,
		})
	}
	return out
}

// searchDirs is a hit's directory set. Split out so the empty-cwd row (one
// exists in the live DB) reaches Visible as an empty set and fails closed,
// rather than as [""] which would never match any target either way — the
// distinction matters only if the fail-closed rule ever changes shape.
func searchDirs(h store.SearchHit) []string {
	if h.Cwd == "" {
		return nil
	}
	return []string{h.Cwd}
}

func flattenLines(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

// searchLimit is the displayed result count; searchFetch over-fetches so that
// filtering hidden projects out still fills the page. SQL applies the LIMIT,
// so trimming before the filter would silently shorten every search under solo.
const (
	searchLimit = 25
	searchFetch = searchLimit * 4
)

// SearchSessions runs a full-text search over the indexed session history.
func (a *App) SearchSessions(query string) []SearchHitDTO {
	out := []SearchHitDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	hits, err := a.st.SearchSessions(query, searchFetch)
	if err != nil {
		return out
	}
	out = searchHitsToDTOs(hits, a.resolver())
	if len(out) > searchLimit {
		out = out[:searchLimit]
	}
	return out
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
	return a.launcher.Resume(resumeRow(a.st, sessionID, cwd), 120, 32, a.now())
}

// resumeRow picks the row a search-resume launches from. The real loom row
// behind the conversation wins because it carries AddDirs, and a synthesised
// one cannot: resuming a scoped multi-repo session from SEARCH otherwise came
// back silently single-repo, invisible until a sibling write failed mid-turn.
// §5 names search-resume as one of the three reachable resume paths.
//
// The lookup is best-effort by design. Transcripts outlive sessions rows — the
// memory index is rebuilt from JSONL Loom does not own, and finished rows are
// deletable — so a hit with no row must still resume, single-repo, from the cwd
// the search result carries. A stored row with an empty cwd is likewise
// rejected in favour of the synthesised one, since Resume would refuse it.
func resumeRow(st *store.Store, sessionID, cwd string) store.SessionRow {
	if st != nil {
		if row, ok, err := st.GetByClaudeSessionID(sessionID); err == nil && ok && row.Cwd != "" {
			return row
		}
	}
	return store.SessionRow{ClaudeSessionID: sessionID, Cwd: cwd, ProjectLabel: filepath.Base(cwd)}
}
