package main

import (
	"path/filepath"

	"github.com/henricktissink/loom/internal/gitdiff"
)

// RepoDiff is one repo's working-tree changes vs HEAD. Error is non-empty
// (and the rest empty) when the directory isn't a git work tree.
//
// An alias, not a copy: the capture moved down to internal/gitdiff so
// internal/delegate can use it (ARCHITECTURE §3 — nothing in internal/ may
// import a frontend), and a parallel struct here would need a converter whose
// only job is to drift. The JSON keys the panel binds to are pinned there.
type RepoDiff = gitdiff.RepoDiff

// DiffDTO is a session's changes across ITS WHOLE DIRECTORY SET — cwd plus
// every --add-dir (§8). gitDiff runs against one directory, so the old
// single-repo shape showed a scoped multi-repo session an authoritative-looking
// diff covering only the primary repo.
//
// Sectioned rather than concatenated: the frontend splits a patch on
// /\n(?=diff --git )/, so any header injected between repos would be parsed as
// part of the preceding file's hunk and corrupt the whole view.
type DiffDTO struct {
	Repos []RepoDiff `json:"repos"`
}

// gitDiff collects the uncommitted changes in cwd (tracked vs HEAD, plus
// untracked file paths). A non-repo or git failure degrades to a RepoDiff with
// Error set rather than a hard failure.
func gitDiff(cwd string) RepoDiff {
	if cwd == "" {
		// gitdiff's own wording is generic because it has callers with no
		// session; the panel's reader has one, and the reason they are looking
		// at an empty diff is that *their session* has no directory.
		return RepoDiff{Error: "no working directory for this session"}
	}
	return gitdiff.WorkingTree(cwd)
}

// SessionDiff returns the uncommitted changes in every directory a session was
// launched with, so a cross-repo change can be reviewed inside loom — that
// review is what slices 3–4 lean on.
func (a *App) SessionDiff(name string) DiffDTO {
	out := DiffDTO{Repos: []RepoDiff{}}
	dirs := []string{""}
	if a.st != nil {
		if row, ok, _ := a.st.Get(name); ok {
			if d := sessionDirs(row); len(d) > 0 {
				dirs = d
			}
		}
	}
	labels := a.targetLabels()
	for _, dir := range dirs {
		d := gitDiff(dir)
		d.Label = labels[dir]
		if d.Label == "" && dir != "" {
			// A directory no project claims still gets a heading: the section
			// is unusable without one, and the basename is what the rest of
			// the UI falls back to for unattributed paths.
			d.Label = filepath.Base(dir)
		}
		out.Repos = append(out.Repos, d)
	}
	return out
}

// targetLabels indexes the known launch targets by path so each diff section
// is headed with the same label the launcher and saved workflows use, rather
// than a bare basename that two projects can share.
func (a *App) targetLabels() map[string]string {
	ts := a.targets()
	m := make(map[string]string, len(ts))
	for _, t := range ts {
		m[t.Path] = t.Label
	}
	return m
}
