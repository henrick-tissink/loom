// gen regenerates bundle/manifest.json from the bundle directory. Run via
// `go generate ./internal/discipline` from the repository root, or
// directly from the package directory. It keeps the manifest's history
// and tested-versions list and replaces the file list and the revision.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/henricktissink/loom/internal/discipline"
)

func main() {
	dir := "bundle"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	var m discipline.Manifest
	if b, err := os.ReadFile(manifestPath); err == nil {
		if err := json.Unmarshal(b, &m); err != nil {
			fail("parse existing manifest: %v", err)
		}
	}
	files, err := discipline.ComputeFiles(os.DirFS(dir))
	if err != nil {
		fail("walk %s: %v", dir, err)
	}
	rev, err := os.ReadFile(filepath.Join(dir, "REV"))
	if err != nil {
		fail("read REV: %v", err)
	}
	newRev := strings.TrimSpace(string(rev))
	if m.History == nil {
		m.History = map[string]map[string]string{}
	}
	if m.Rev != "" && m.Rev != newRev {
		// REV was bumped: the outgoing revision's hashes become history, so a
		// machine still on it reads "stale" (safe to upgrade), not "modified".
		if _, done := m.History[m.Rev]; !done && len(m.Files) > 0 {
			old := map[string]string{}
			for _, f := range m.Files {
				old[f.Path] = f.SHA256
			}
			m.History[m.Rev] = old
		}
	} else if _, released := m.History[newRev]; released && filesDiffer(m.Files, files) {
		// A revision that is already in history has shipped: its content is
		// frozen. Edit the bundle under a new REV.
		fail("bundle files changed but REV is still %s, which has shipped (it is in history); bump REV first", newRev)
	}
	m.Rev = newRev
	m.Files = files
	m.Registrations = "registrations.json"
	if len(m.ClaudeCodeTested) == 0 {
		m.ClaudeCodeTested = []string{"2.1.296", "2.1.300"}
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		fail("encode: %v", err)
	}
	if err := os.WriteFile(manifestPath, append(out, '\n'), 0o644); err != nil {
		fail("write: %v", err)
	}
	fmt.Printf("manifest: %s, %d files\n", m.Rev, len(files))
}

func filesDiffer(a, b []discipline.ManifestFile) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gen: "+format+"\n", args...)
	os.Exit(1)
}
