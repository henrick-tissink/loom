package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/delegate"
)

// fakeResolver satisfies delegate.Resolver for any project name, returning a
// fixed two-repo scope so the schema doc's example resolves without a store.
type fakeResolver struct{ scope delegate.Scope }

func (f fakeResolver) ResolveProject(string) (delegate.Scope, error) { return f.scope, nil }

// extractExampleJSON pulls the single ```json fenced block out of the doc.
func extractExampleJSON(t *testing.T) string {
	t.Helper()
	const fence = "```json"
	i := strings.Index(ManifestSchemaDoc, fence)
	if i < 0 {
		t.Fatal("schema doc has no ```json example block")
	}
	rest := ManifestSchemaDoc[i+len(fence):]
	j := strings.Index(rest, "```")
	if j < 0 {
		t.Fatal("unterminated ```json block")
	}
	return strings.TrimSpace(rest[:j])
}

// TestManifestSchemaDocExampleLoads is the plan's correctness anchor: the very
// manifest example we hand the orchestrator must load through delegate.LoadAll
// with zero errors, so we never teach the model a format that doesn't parse.
func TestManifestSchemaDocExampleLoads(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "example.json"), []byte(extractExampleJSON(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	repoAPI, repoWeb := t.TempDir(), t.TempDir()
	res := fakeResolver{scope: delegate.Scope{
		Root: dir, Name: "atlas",
		Repos: map[string]string{"api": repoAPI, "web": repoWeb},
	}}
	ms, errs := delegate.LoadAll(dir, res)
	if len(errs) != 0 {
		t.Fatalf("example manifest must load clean, got errors: %+v", errs)
	}
	if len(ms) != 1 || ms[0].Name != "example" {
		t.Fatalf("want 1 manifest named 'example', got %+v", ms)
	}
}
