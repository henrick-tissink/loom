package main

import (
	"testing"

	"github.com/henricktissink/loom/internal/workflow"
)

func TestRunStepLabel(t *testing.T) {
	def := `{"name":"plan-exec","steps":[{"label":"plan"},{"label":"execute"},{"label":"review"}]}`

	label, count, defErr := runStepLabel(def, 1)
	if defErr {
		t.Fatal("valid def flagged as defErr")
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	if label != "step 2/3 execute" {
		t.Errorf("label = %q, want %q", label, "step 2/3 execute")
	}

	// Out-of-range step index still renders the counter, no label, no panic.
	if l, _, _ := runStepLabel(def, 9); l != "step 10/3 " {
		t.Errorf("oob label = %q", l)
	}

	// Malformed / empty JSON → defErr.
	if _, _, e := runStepLabel("{bad", 0); !e {
		t.Error("malformed json should be defErr")
	}
	if _, _, e := runStepLabel(`{"name":"x","steps":[]}`, 0); !e {
		t.Error("no steps should be defErr")
	}
}

func TestDefsToDTOs(t *testing.T) {
	defs := []workflow.Definition{
		{Name: "a", Path: "/w/a.json", Steps: []workflow.Step{{Label: "s1", Project: "/Users/me/Sauce/loom"}, {Label: "s2"}}},
		{Name: "b", Path: "/w/b.json", Steps: nil},
	}
	got := defsToDTOs(defs)
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].Name != "a" || got[0].Path != "/w/a.json" || got[0].Steps != 2 || got[0].Project != "loom" {
		t.Errorf("a: %+v", got[0])
	}
	if got[1].Steps != 0 || got[1].Project != "" {
		t.Errorf("b (no steps): %+v", got[1])
	}
}

func TestDefsToDTOs_emptyNonNil(t *testing.T) {
	if defsToDTOs(nil) == nil {
		t.Fatal("want non-nil slice")
	}
	if loadErrsToDTOs(nil) == nil {
		t.Fatal("want non-nil slice")
	}
}
