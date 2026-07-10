package main

import (
	"testing"

	"github.com/henricktissink/loom/internal/registry"
)

var testProjects = []registry.Project{
	{Label: "loom", Path: "/ws/loom"},
	{Label: "group/api", Path: "/ws/group/api"},
}

func TestProjectsToDTOs(t *testing.T) {
	got := projectsToDTOs(testProjects)
	if len(got) != 2 || got[0] != (ProjectDTO{Label: "loom", Path: "/ws/loom"}) {
		t.Fatalf("mapping mismatch: %+v", got)
	}
	if projectsToDTOs(nil) == nil {
		t.Fatal("must return non-nil empty slice")
	}
}

func TestBuildRecipe_valid(t *testing.T) {
	r, err := buildRecipe(testProjects, "/ws/group/api", "sonnet", "acceptEdits", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.ProjectLabel != "group/api" || r.Cwd != "/ws/group/api" ||
		r.Model != "sonnet" || r.Mode != "acceptEdits" || r.Seed != "hello" {
		t.Fatalf("recipe mismatch: %+v", r)
	}
}

func TestBuildRecipe_defaultsAllowed(t *testing.T) {
	// empty model/mode/seed are all valid (== claude defaults, no seed)
	if _, err := buildRecipe(testProjects, "/ws/loom", "", "", ""); err != nil {
		t.Fatalf("empty model/mode/seed should be valid: %v", err)
	}
}

func TestBuildRecipe_rejectsUnknown(t *testing.T) {
	if _, err := buildRecipe(testProjects, "/ws/nope", "", "", ""); err == nil {
		t.Error("unknown project should error")
	}
	if _, err := buildRecipe(testProjects, "/ws/loom", "gpt", "", ""); err == nil {
		t.Error("unknown model should error")
	}
	if _, err := buildRecipe(testProjects, "/ws/loom", "", "yolo", ""); err == nil {
		t.Error("unknown mode should error")
	}
}
