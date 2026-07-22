package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/henricktissink/loom/internal/registry"
)

func writeDef(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name+".json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func testRepos() []registry.Repo {
	return []registry.Repo{
		{Label: "parallax", Path: "/w/parallax"},
		{Label: "loom", Path: "/w/loom"},
	}
}

func TestLoadAllValidDefinitionResolvesProjectPathAndBakesAllSteps(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "plan-execute-review", `{
		"name": "plan-execute-review",
		"steps": [
			{"label": "plan", "project": "parallax", "model": "opus", "mode": "plan", "seed": "Plan it.", "relation": "fresh"},
			{"label": "execute", "model": "sonnet", "mode": "acceptEdits", "relation": "fork", "seed": "Go. {{prev.outcome}}"},
			{"label": "review", "relation": "fresh", "seed": "/code-review"}
		]
	}`)

	defs, errs := LoadAll(dir, testRepos())
	if len(errs) != 0 {
		t.Fatalf("errs = %+v, want none", errs)
	}
	if len(defs) != 1 {
		t.Fatalf("defs = %+v, want 1", defs)
	}
	d := defs[0]
	if d.Name != "plan-execute-review" {
		t.Fatalf("Name = %q", d.Name)
	}
	if d.Path != filepath.Join(dir, "plan-execute-review.json") {
		t.Fatalf("Path = %q", d.Path)
	}
	if len(d.Steps) != 3 {
		t.Fatalf("Steps = %+v", d.Steps)
	}
	// Step 1's project is resolved to the registry's absolute path, not left as the label.
	if d.Steps[0].Project != "/w/parallax" {
		t.Fatalf("Steps[0].Project = %q, want resolved absolute path", d.Steps[0].Project)
	}
	if d.Steps[1].Project != "" {
		t.Fatalf("Steps[1].Project = %q, want empty (inherit)", d.Steps[1].Project)
	}
}

func TestLoadAllNameMustMatchFilenameStem(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "foo", `{"name":"bar","steps":[{"label":"a","project":"loom","relation":"fresh","seed":"x"}]}`)
	defs, errs := LoadAll(dir, testRepos())
	if len(defs) != 0 || len(errs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 0 defs 1 err", defs, errs)
	}
}

func TestLoadAllZeroStepsRejected(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "empty", `{"name":"empty","steps":[]}`)
	defs, errs := LoadAll(dir, testRepos())
	if len(defs) != 0 || len(errs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 0 defs 1 err", defs, errs)
	}
}

func TestLoadAllStep1MissingProjectRejected(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "noproj", `{"name":"noproj","steps":[{"label":"a","relation":"fresh","seed":"x"}]}`)
	defs, errs := LoadAll(dir, testRepos())
	if len(defs) != 0 || len(errs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 0 defs 1 err", defs, errs)
	}
}

func TestLoadAllStep1UnknownProjectRejected(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "badproj", `{"name":"badproj","steps":[{"label":"a","project":"nonexistent","relation":"fresh","seed":"x"}]}`)
	defs, errs := LoadAll(dir, testRepos())
	if len(defs) != 0 || len(errs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 0 defs 1 err", defs, errs)
	}
}

func TestLoadAllUnknownModelRejected(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "badmodel", `{"name":"badmodel","steps":[{"label":"a","project":"loom","model":"gpt5","relation":"fresh","seed":"x"}]}`)
	defs, errs := LoadAll(dir, testRepos())
	if len(defs) != 0 || len(errs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 0 defs 1 err", defs, errs)
	}
}

func TestLoadAllUnknownModeRejected(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "badmode", `{"name":"badmode","steps":[{"label":"a","project":"loom","mode":"yolo","relation":"fresh","seed":"x"}]}`)
	defs, errs := LoadAll(dir, testRepos())
	if len(defs) != 0 || len(errs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 0 defs 1 err", defs, errs)
	}
}

func TestLoadAllStep2EmptyOrUnknownRelationRejected(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "norelation", `{"name":"norelation","steps":[
		{"label":"a","project":"loom","relation":"fresh","seed":"x"},
		{"label":"b","seed":"y"}
	]}`)
	defs, errs := LoadAll(dir, testRepos())
	if len(defs) != 0 || len(errs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 0 defs 1 err (missing step-2 relation)", defs, errs)
	}

	dir2 := t.TempDir()
	writeDef(t, dir2, "badrelation", `{"name":"badrelation","steps":[
		{"label":"a","project":"loom","relation":"fresh","seed":"x"},
		{"label":"b","relation":"teleport","seed":"y"}
	]}`)
	defs2, errs2 := LoadAll(dir2, testRepos())
	if len(defs2) != 0 || len(errs2) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 0 defs 1 err (bad step-2 relation)", defs2, errs2)
	}
}

func TestLoadAllTemplateTokenTypoRejected(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "typo", `{"name":"typo","steps":[
		{"label":"a","project":"loom","relation":"fresh","seed":"x"},
		{"label":"b","relation":"fork","seed":"Prior said: {{prev.result}}"}
	]}`)
	defs, errs := LoadAll(dir, testRepos())
	if len(defs) != 0 || len(errs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 0 defs 1 err (unwhitelisted template token)", defs, errs)
	}
}

func TestLoadAllTemplateWhitelistedTokensAccepted(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "goodtokens", `{"name":"goodtokens","steps":[
		{"label":"a","project":"loom","relation":"fresh","seed":"x"},
		{"label":"b","relation":"fork","seed":"{{prev.outcome}} {{prev.title}} {{prev.ask}}"}
	]}`)
	defs, errs := LoadAll(dir, testRepos())
	if len(errs) != 0 || len(defs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 1 def 0 errs", defs, errs)
	}
}

// TestLoadAllMultilineSeedNormalizedToSingleLine guards the debt-sweep fix:
// a literal \n/\r/\t authored in a step's seed JSON (e.g. copy-pasted
// multi-line prompt text) must be collapsed to single spaces at LOAD time,
// before it ever reaches SendLiteral on a continue delivery (run.go's
// sendPendingSeed) — an un-normalized \n there acts as a premature Enter,
// submitting the prompt early.
func TestLoadAllMultilineSeedNormalizedToSingleLine(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "multiline", `{"name":"multiline","steps":[
		{"label":"a","project":"loom","relation":"fresh","seed":"line one\nline two\r\nline three\t\tend"}
	]}`)
	defs, errs := LoadAll(dir, testRepos())
	if len(errs) != 0 || len(defs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 1 def 0 errs", defs, errs)
	}
	want := "line one line two line three end"
	if got := defs[0].Steps[0].Seed; got != want {
		t.Fatalf("Seed = %q, want %q (newlines/tabs collapsed to single spaces)", got, want)
	}
}

func TestLoadAllMalformedJSONRejectedNotPanicked(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "broken", `{not valid json`)
	defs, errs := LoadAll(dir, testRepos())
	if len(defs) != 0 || len(errs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 0 defs 1 err", defs, errs)
	}
}

func TestLoadAllIgnoresNonJSONFilesAndSubdirs(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "valid", `{"name":"valid","steps":[{"label":"a","project":"loom","relation":"fresh","seed":"x"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	defs, errs := LoadAll(dir, testRepos())
	if len(errs) != 0 {
		t.Fatalf("errs = %+v, want none (non-.json and dirs ignored)", errs)
	}
	if len(defs) != 1 || defs[0].Name != "valid" {
		t.Fatalf("defs = %+v, want just 'valid'", defs)
	}
}

func TestLoadAllMissingDirReturnsEmptyNotError(t *testing.T) {
	defs, errs := LoadAll(filepath.Join(t.TempDir(), "nope"), testRepos())
	if defs != nil || errs != nil {
		t.Fatalf("defs=%+v errs=%+v, want nil, nil for a missing dir", defs, errs)
	}
}

func TestLoadAllSortedByNameAndErrorsByPath(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "zzz", `{"name":"zzz","steps":[{"label":"a","project":"loom","relation":"fresh","seed":"x"}]}`)
	writeDef(t, dir, "aaa", `{"name":"aaa","steps":[{"label":"a","project":"loom","relation":"fresh","seed":"x"}]}`)
	writeDef(t, dir, "zbad", `{not json`)
	writeDef(t, dir, "abad", `{also not json`)

	defs, errs := LoadAll(dir, testRepos())
	if len(defs) != 2 || defs[0].Name != "aaa" || defs[1].Name != "zzz" {
		t.Fatalf("defs = %+v, want [aaa, zzz]", defs)
	}
	if len(errs) != 2 || errs[0].Path > errs[1].Path {
		t.Fatalf("errs = %+v, want sorted by Path", errs)
	}
}

func TestLoadAllStep1MissingLabelRejected(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "nolabel", `{"name":"nolabel","steps":[{"project":"loom","relation":"fresh","seed":"x"}]}`)
	defs, errs := LoadAll(dir, testRepos())
	if len(defs) != 0 || len(errs) != 1 {
		t.Fatalf("defs=%+v errs=%+v, want 0 defs 1 err", defs, errs)
	}
}
