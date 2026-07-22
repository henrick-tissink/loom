package main

import (
	"fmt"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/session"
)

// ProjectDTO is the flat view of one launch target for the form. The DTO name
// is the frontend's contract and stays as-is; what it is built from changed
// from a startup registry snapshot to loom.db's target set (§7), so a project
// created in-app is launchable without a restart.
//
// ProjectRoot/ProjectName let the picker group targets by project the same way
// the rail does; Missing marks a target that is dimmed and non-launchable.
type ProjectDTO struct {
	Label       string `json:"label"`
	Path        string `json:"path"`
	ProjectRoot string `json:"projectRoot"`
	ProjectName string `json:"projectName"`
	IsRoot      bool   `json:"isRoot"`
	Missing     bool   `json:"missing"`
}

func targetsToDTOs(ts []projects.Target) []ProjectDTO {
	out := make([]ProjectDTO, 0, len(ts))
	for _, t := range ts {
		out = append(out, ProjectDTO{
			Label: t.Label, Path: t.Path,
			ProjectRoot: t.ProjectRoot, ProjectName: t.ProjectName,
			IsRoot:  t.Kind == projects.TargetRoot,
			Missing: t.Missing,
		})
	}
	return out
}

// Allowed values mirror the TUI launcher's option sets verbatim ("" = default).
var allowedModels = map[string]bool{"": true, "opus": true, "sonnet": true, "fable": true}
var allowedModes = map[string]bool{"": true, "plan": true, "acceptEdits": true, "auto": true, "bypassPermissions": true}

// buildRecipe validates the form inputs against the CURRENT launch target set
// and returns a launch Recipe. It errors on an unknown target, model or mode
// so a malformed request never reaches the launcher.
//
// addDirs entries are validated the same way rather than passed through: every
// one becomes a `--add-dir` on the real command line, so an unvalidated entry
// is both a way to hand a session a directory the picker never offered and a
// way back into a project the user hid.
func buildRecipe(targets []projects.Target, repoPath, model, mode, seed string, addDirs []string) (session.Recipe, error) {
	if !allowedModels[model] {
		return session.Recipe{}, fmt.Errorf("unknown model %q", model)
	}
	if !allowedModes[mode] {
		return session.Recipe{}, fmt.Errorf("unknown mode %q", mode)
	}
	byPath := make(map[string]projects.Target, len(targets))
	for _, t := range targets {
		byPath[t.Path] = t
	}
	primary, ok := byPath[repoPath]
	if !ok {
		return session.Recipe{}, fmt.Errorf("unknown project %q", repoPath)
	}
	var dirs []string
	for _, d := range addDirs {
		if d == repoPath {
			continue // the primary is already the cwd; --add-dir'ing it is noise
		}
		if _, ok := byPath[d]; !ok {
			return session.Recipe{}, fmt.Errorf("unknown add-dir %q", d)
		}
		dirs = append(dirs, d)
	}
	return session.Recipe{
		ProjectLabel: primary.Label,
		Cwd:          primary.Path,
		Model:        model,
		Mode:         mode,
		Seed:         seed,
		AddDirs:      dirs,
	}, nil
}
