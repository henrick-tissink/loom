package main

import (
	"github.com/henricktissink/loom/internal/cleanup"
	"github.com/henricktissink/loom/internal/registry"
)

// RepoCleanupDTO previews the build junk a project's repo(s) hold — only paths
// that are BOTH git-ignored and a known-junk pattern.
type RepoCleanupDTO struct {
	Entries []cleanup.Entry `json:"entries"`
	Total   int64           `json:"total"` // total bytes across entries
}

// RepoCleanupPreview scans a project's root and child repos for removable junk.
func (a *App) RepoCleanupPreview(projectRoot string) RepoCleanupDTO {
	out := RepoCleanupDTO{Entries: []cleanup.Entry{}}
	defer func() { _ = recover() }()
	roots := append([]string{projectRoot}, registry.ChildRepos(projectRoot)...)
	entries, err := cleanup.Scan(roots)
	if err != nil {
		return out
	}
	if entries != nil {
		out.Entries = entries
	}
	for _, e := range entries {
		out.Total += e.Size
	}
	return out
}

// RepoCleanupRun deletes the given absolute paths — but Remove re-scans and only
// deletes ones still confirmed as junk, so a stale path can't delete anything
// outside the set. Returns bytes freed.
func (a *App) RepoCleanupRun(projectRoot string, abs []string) (int64, error) {
	roots := append([]string{projectRoot}, registry.ChildRepos(projectRoot)...)
	return cleanup.Remove(roots, abs)
}
