package main

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// RepoDiff is one repo's working-tree changes vs HEAD. Error is non-empty
// (and the rest empty) when the directory isn't a git work tree.
type RepoDiff struct {
	Path      string   `json:"path"`
	Label     string   `json:"label"`
	Stat      string   `json:"stat"`      // `git diff HEAD --stat` summary
	Patch     string   `json:"patch"`     // full `git diff HEAD`
	Untracked []string `json:"untracked"` // new files not yet tracked
	Dirty     bool     `json:"dirty"`     // any tracked change or untracked file
	Error     string   `json:"error"`     // set when the path isn't a git work tree
}

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
// untracked file paths). Shells out; a non-repo or git failure degrades to a
// RepoDiff with Error set rather than a hard failure.
func gitDiff(cwd string) RepoDiff {
	if cwd == "" {
		return RepoDiff{Error: "no working directory for this session"}
	}
	d := RepoDiff{Path: cwd}
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		d.Error = "not a git repository"
		return d
	}
	stat, _ := exec.Command("git", "-C", cwd, "diff", "HEAD", "--stat").Output()
	patch, _ := exec.Command("git", "-C", cwd, "diff", "HEAD").Output()
	untr, _ := exec.Command("git", "-C", cwd, "ls-files", "--others", "--exclude-standard").Output()

	var untracked []string
	for _, l := range strings.Split(strings.TrimSpace(string(untr)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			untracked = append(untracked, l)
		}
	}
	d.Stat = strings.TrimRight(string(stat), "\n")
	d.Patch = string(patch)
	d.Untracked = untracked
	d.Dirty = strings.TrimSpace(d.Patch) != "" || len(untracked) > 0
	return d
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
