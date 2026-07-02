// Package registry discovers workspace projects (spec §5).
package registry

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henricktissink/loom/internal/transcript"
)

type Project struct {
	Label string // directory basename, shown in the UI
	Path  string // absolute path, used as cwd
}

// Discover lists workspace subdirs that look like projects: they have .git or
// an existing claude transcript directory.
func Discover(workspaceRoot, claudeConfigDir string) ([]Project, error) {
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return nil, err
	}
	var ps []Project
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(workspaceRoot, e.Name())
		hasGit := false
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			hasGit = true
		}
		hasTranscripts := false
		tdir := filepath.Join(claudeConfigDir, "projects", transcript.ProjectDirName(path))
		if _, err := os.Stat(tdir); err == nil {
			hasTranscripts = true
		}
		if hasGit || hasTranscripts {
			ps = append(ps, Project{Label: e.Name(), Path: path})
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Label < ps[j].Label })
	return ps, nil
}
