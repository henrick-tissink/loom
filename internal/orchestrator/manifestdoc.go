package orchestrator

import (
	_ "embed"
	"os"
	"path/filepath"
)

// ManifestSchemaDoc is the agent-facing reference for the delegation manifest
// format. Its embedded ```json example provably loads through delegate.LoadAll
// (manifestdoc_test.go) — we never teach the orchestrator a format that doesn't
// parse. Written into the orchestrator's notes dir at spawn so it lands in an
// --add-dir the session can read; the brief points the orchestrator at it.
//
//go:embed loom-manifest-schema.md
var ManifestSchemaDoc string

// ManifestSchemaFile is the basename the doc is written to (and the name the
// brief references).
const ManifestSchemaFile = "loom-manifest-schema.md"

// WriteManifestSchemaDoc writes the schema reference into dir and returns its
// path. dir is the orchestrator's notes dir (already readable + --add-dir'd).
func WriteManifestSchemaDoc(dir string) (string, error) {
	path := filepath.Join(dir, ManifestSchemaFile)
	if err := os.WriteFile(path, []byte(ManifestSchemaDoc), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
