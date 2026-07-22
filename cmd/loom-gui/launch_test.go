package main

import (
	"reflect"
	"testing"

	"github.com/henricktissink/loom/internal/projects"
)

// testTargets is the launch surface a picker would show for one multi-repo
// project (Innostream: root + two repos) and one single-repo project.
var testTargets = []projects.Target{
	{Kind: projects.TargetRepo, Path: "/ws/loom", Label: "loom", ProjectRoot: "/ws/loom", ProjectName: "loom"},
	{Kind: projects.TargetRoot, Path: "/ws/group", Label: "Innostream", ProjectRoot: "/ws/group", ProjectName: "Innostream"},
	{Kind: projects.TargetRepo, Path: "/ws/group/api", Label: "group/api", ProjectRoot: "/ws/group", ProjectName: "Innostream"},
	{Kind: projects.TargetRepo, Path: "/ws/group/web", Label: "group/web", ProjectRoot: "/ws/group", ProjectName: "Innostream"},
}

func TestTargetsToDTOs(t *testing.T) {
	got := targetsToDTOs(testTargets)
	if len(got) != 4 {
		t.Fatalf("want 4, got %d", len(got))
	}
	want := ProjectDTO{Label: "loom", Path: "/ws/loom", ProjectRoot: "/ws/loom", ProjectName: "loom"}
	if got[0] != want {
		t.Fatalf("mapping mismatch: %+v", got[0])
	}
	if !got[1].IsRoot {
		t.Errorf("root target must be flagged: %+v", got[1])
	}
	if targetsToDTOs(nil) == nil {
		t.Fatal("must return non-nil empty slice")
	}
}

func TestBuildRecipe_valid(t *testing.T) {
	r, err := buildRecipe(testTargets, "/ws/group/api", "sonnet", "acceptEdits", "hello", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.ProjectLabel != "group/api" || r.Cwd != "/ws/group/api" ||
		r.Model != "sonnet" || r.Mode != "acceptEdits" || r.Seed != "hello" {
		t.Fatalf("recipe mismatch: %+v", r)
	}
	if r.AddDirs != nil {
		t.Fatalf("AddDirs should stay nil when none are requested: %+v", r.AddDirs)
	}
}

func TestBuildRecipe_defaultsAllowed(t *testing.T) {
	// empty model/mode/seed are all valid (== claude defaults, no seed)
	if _, err := buildRecipe(testTargets, "/ws/loom", "", "", "", nil); err != nil {
		t.Fatalf("empty model/mode/seed should be valid: %v", err)
	}
}

func TestBuildRecipe_rejectsUnknown(t *testing.T) {
	if _, err := buildRecipe(testTargets, "/ws/nope", "", "", "", nil); err == nil {
		t.Error("unknown project should error")
	}
	if _, err := buildRecipe(testTargets, "/ws/loom", "gpt", "", "", nil); err == nil {
		t.Error("unknown model should error")
	}
	if _, err := buildRecipe(testTargets, "/ws/loom", "", "yolo", "", nil); err == nil {
		t.Error("unknown mode should error")
	}
}

// TestBuildRecipe_addDirs pins §5's scoped multi-repo shape and the rule that
// add-dirs are validated exactly like the primary: every entry becomes a real
// --add-dir on the command line, so an unvalidated one is a way past both the
// picker and §6's hiding.
func TestBuildRecipe_addDirs(t *testing.T) {
	tests := []struct {
		name    string
		primary string
		addDirs []string
		want    []string
		wantErr bool
	}{
		{name: "scoped multi-repo", primary: "/ws/group/api",
			addDirs: []string{"/ws/group/web"}, want: []string{"/ws/group/web"}},
		{name: "order preserved", primary: "/ws/loom",
			addDirs: []string{"/ws/group/web", "/ws/group/api"},
			want:    []string{"/ws/group/web", "/ws/group/api"}},
		{name: "primary repeated in add-dirs is dropped", primary: "/ws/group/api",
			addDirs: []string{"/ws/group/api", "/ws/group/web"}, want: []string{"/ws/group/web"}},
		{name: "cross-project add-dir allowed when it is a known target",
			primary: "/ws/loom", addDirs: []string{"/ws/group/api"}, want: []string{"/ws/group/api"}},
		{name: "unknown add-dir rejected", primary: "/ws/loom",
			addDirs: []string{"/etc"}, wantErr: true},
		{name: "empty add-dir rejected", primary: "/ws/loom",
			addDirs: []string{""}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := buildRecipe(testTargets, tt.primary, "", "", "", tt.addDirs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got recipe %+v", r)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(r.AddDirs, tt.want) {
				t.Fatalf("AddDirs = %v, want %v", r.AddDirs, tt.want)
			}
		})
	}
}
