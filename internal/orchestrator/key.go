// Package orchestrator assembles a project's brief and spawns the one claude
// session that holds that project's architecture (slice 2, spec
// docs/superpowers/specs/2026-07-22-orchestrator-brief-design.md).
//
// It sits below both frontends and above `store`, `projects`, `memory` and
// `session`. Nothing here reaches into `status` — the engine never learns about
// projects (slice 1 §6.2a), which is why an orchestrator is an ordinary
// SessionRow with a tag and why `ended_at` is stamped by Sweep here rather than
// by a hook there.
//
// Two invariants shape every file in this package:
//
//   - Loom writes only under ~/.loom; the agent writes only under the project's
//     notes_dir (§3). Neither ever writes where the other does, and Loom never
//     creates a directory in the user's workspace.
//   - Assembly spends zero LLM quota (§11). It is local file reads, a few
//     `git rev-parse` calls and two SQLite queries — memory.Summarizer, Loom's
//     only outbound call, is not on any path here.
package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// ProjectKey is the on-disk name for a project's Loom-owned directory:
// `<sanitized basename of root>-<sha256(root)[:8]>` (§3).
//
// Both halves earn their place. The hash disambiguates two projects whose roots
// share a basename (`~/work/api` and `~/oss/api` are not the same brain), and
// the basename keeps ~/.loom/projects greppable by a human — a directory named
// only by a hash is one nobody will ever open on purpose.
//
// The hash is over the root as given. Callers pass projects.Canonical'd roots
// (that is what the store holds), so two spellings of the same directory cannot
// arrive here; resolving symlinks would be worse than useless, because a root
// that vanishes would then stop hashing to its own notes.
func ProjectKey(root string) string {
	sum := sha256.Sum256([]byte(root))
	return sanitizeBase(filepath.Base(root)) + "-" + hex.EncodeToString(sum[:])[:8]
}

// sanitizeBase keeps a basename to characters that are safe in a path segment
// on every filesystem Loom runs on. Anything else collapses to '-'; a name that
// sanitizes away entirely becomes "project" rather than "", which would produce
// a directory called `-9f2c1ab4` that looks like a flag to every CLI tool.
func sanitizeBase(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-.")
	if s == "" {
		return "project"
	}
	return s
}

// Paths names every Loom-owned file for one project. Derived, never stored:
// these three live under ~/.loom and follow the root, unlike notes_dir which is
// materialized into the row precisely so it does NOT follow the root (§3).
type Paths struct {
	Dir        string // ~/.loom/projects/<key>
	Brief      string // …/brief.md
	State      string // …/state.json
	NotesDir   string // …/notes — the DEFAULT notes location, only used when the row is empty
	ProjectKey string
}

// PathsFor derives the Loom-owned paths for root under loomDir (~/.loom).
func PathsFor(loomDir, root string) Paths {
	key := ProjectKey(root)
	dir := filepath.Join(loomDir, "projects", key)
	return Paths{
		Dir:        dir,
		Brief:      filepath.Join(dir, "brief.md"),
		State:      filepath.Join(dir, "state.json"),
		NotesDir:   filepath.Join(dir, "notes"),
		ProjectKey: key,
	}
}

// NoteFiles is the fixed three-file layout (§3). Fixed rather than
// agent-chosen: it is what makes §8's digest-based drift detection possible at
// all, and what lets a FRESH orchestrator know where to look without being told
// by its predecessor.
var NoteFiles = []string{"loom-map.md", "loom-decisions.md", "loom-open.md"}
