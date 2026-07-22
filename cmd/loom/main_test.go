package main

import (
	"path/filepath"
	"testing"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/store"
)

// launchRepos is the TUI's whole launch surface, so what it drops and what
// label it carries are both load-bearing: the label is what saved workflow
// definitions resolve through (§2), and a missing path reaches tmux, which
// silently starts the session in $HOME instead of failing (§12).
func TestLaunchRepos(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for _, p := range []store.Project{
		// Named unlike its directory on purpose: the label must follow the
		// directory, so a rename cannot invalidate a workflow on disk.
		{Root: "/w/innostream", Name: "Innostream Rebuild", Origin: "discovered", CreatedAt: 1, UpdatedAt: 1},
		{Root: "/w/gone", Name: "Gone", Origin: "created", Missing: true, CreatedAt: 1, UpdatedAt: 1},
	} {
		if err := st.UpsertProject(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertProjectRepo(store.ProjectRepo{
		Path: "/w/innostream/ballista", ProjectRoot: "/w/innostream",
		Label: "innostream/ballista", AddedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	got := launchRepos(projects.New(st))
	labels := map[string]string{}
	for _, r := range got {
		labels[r.Path] = r.Label
	}
	if len(got) != 2 {
		t.Fatalf("repos = %+v, want the root and its repo (missing project dropped)", got)
	}
	if labels["/w/innostream"] != "innostream" {
		t.Errorf("root label = %q, want the directory basename", labels["/w/innostream"])
	}
	if labels["/w/innostream/ballista"] != "innostream/ballista" {
		t.Errorf("repo label = %q", labels["/w/innostream/ballista"])
	}
	if _, ok := labels["/w/gone"]; ok {
		t.Error("a missing project must not be offered as a launch target")
	}
	// The reserved Ungrouped row is seeded by the migration and is not a
	// directory; it must never reach a picker.
	if _, ok := labels[store.UngroupedRoot]; ok {
		t.Error("Ungrouped is not launchable")
	}
}
