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
// an existing claude transcript directory. A subdir that is not itself a
// project is treated as a group dir and its immediate children are checked
// with the same predicate, labeled "group/child" (one extra level only;
// project dirs are never descended into).
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
		if isProject(path, claudeConfigDir) {
			ps = append(ps, Project{Label: e.Name(), Path: path})
			continue
		}
		children, err := os.ReadDir(path)
		if err != nil {
			continue // unreadable group dir shouldn't kill discovery
		}
		for _, c := range children {
			if !c.IsDir() || strings.HasPrefix(c.Name(), ".") {
				continue
			}
			cpath := filepath.Join(path, c.Name())
			if isProject(cpath, claudeConfigDir) {
				ps = append(ps, Project{Label: e.Name() + "/" + c.Name(), Path: cpath})
			}
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Label < ps[j].Label })
	return ps, nil
}

// isProject reports whether path has a .git (dir or worktree/submodule file)
// or an existing claude transcript directory.
func isProject(path, claudeConfigDir string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return true
	}
	tdir := filepath.Join(claudeConfigDir, "projects", transcript.ProjectDirName(path))
	_, err := os.Stat(tdir)
	return err == nil
}
