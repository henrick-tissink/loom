package main

import (
	"fmt"

	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
)

// ProjectDTO is the flat view of a discovered workspace project for the form.
type ProjectDTO struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

func projectsToDTOs(ps []registry.Project) []ProjectDTO {
	out := make([]ProjectDTO, 0, len(ps))
	for _, p := range ps {
		out = append(out, ProjectDTO{Label: p.Label, Path: p.Path})
	}
	return out
}

// Allowed values mirror the TUI launcher's option sets verbatim ("" = default).
var allowedModels = map[string]bool{"": true, "opus": true, "sonnet": true, "fable": true}
var allowedModes = map[string]bool{"": true, "plan": true, "acceptEdits": true, "auto": true, "bypassPermissions": true}

// buildRecipe validates the form inputs and resolves the project path to a
// discovered project, returning a launch Recipe. It errors on an unknown
// project, model, or mode so a malformed request never reaches the launcher.
func buildRecipe(projects []registry.Project, projectPath, model, mode, seed string) (session.Recipe, error) {
	if !allowedModels[model] {
		return session.Recipe{}, fmt.Errorf("unknown model %q", model)
	}
	if !allowedModes[mode] {
		return session.Recipe{}, fmt.Errorf("unknown mode %q", mode)
	}
	for _, p := range projects {
		if p.Path == projectPath {
			return session.Recipe{
				ProjectLabel: p.Label,
				Cwd:          p.Path,
				Model:        model,
				Mode:         mode,
				Seed:         seed,
			}, nil
		}
	}
	return session.Recipe{}, fmt.Errorf("unknown project %q", projectPath)
}
