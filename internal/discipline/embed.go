// Package discipline carries the Agent Discipline Handbook bundle — the
// rules, the enforcement hooks, their suites and the helper scripts — as
// the loom binary's own copy, and knows how a machine's install compares
// to it. See docs/DISCIPLINE.md.
//
// Phase 1 (this package today): the embedded bundle, its manifest, the
// target layout on a machine, and a read-only doctor. Nothing here writes
// outside the repository; install and upgrade are phase 2.
package discipline

import (
	"embed"
	"io/fs"
)

//go:generate go run ./gen

// bundleFS is the bundle directory verbatim. The plain (non-`all:`) embed
// skips files and directories whose names start with "." or "_", which is
// what keeps __pycache__ out.
//
//go:embed bundle
var bundleFS embed.FS

// Bundle returns the bundle as a filesystem rooted at the bundle directory,
// so paths read "enforce/stop_guard.py", "bin/deadline", "REV".
func Bundle() fs.FS {
	sub, err := fs.Sub(bundleFS, "bundle")
	if err != nil {
		panic("discipline: embedded bundle missing: " + err.Error())
	}
	return sub
}
