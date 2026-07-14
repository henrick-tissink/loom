package main

import (
	"os/exec"
	"strings"
)

// DiffDTO is a session's working-tree changes vs HEAD, for the in-app review
// panel. Error is non-empty (and the rest empty) when cwd isn't a git repo.
type DiffDTO struct {
	Stat      string   `json:"stat"`      // `git diff HEAD --stat` summary
	Patch     string   `json:"patch"`     // full `git diff HEAD`
	Untracked []string `json:"untracked"` // new files not yet tracked
	Dirty     bool     `json:"dirty"`     // any tracked change or untracked file
	Error     string   `json:"error"`     // set when cwd isn't a git work tree
}

// gitDiff collects the uncommitted changes in cwd (tracked vs HEAD, plus
// untracked file paths). Shells out; a non-repo or git failure degrades to a
// DiffDTO with Error set rather than a hard failure.
func gitDiff(cwd string) DiffDTO {
	if cwd == "" {
		return DiffDTO{Error: "no working directory for this session"}
	}
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return DiffDTO{Error: "not a git repository"}
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
	d := DiffDTO{
		Stat:      strings.TrimRight(string(stat), "\n"),
		Patch:     string(patch),
		Untracked: untracked,
	}
	d.Dirty = strings.TrimSpace(d.Patch) != "" || len(untracked) > 0
	return d
}

// SessionDiff returns the uncommitted changes in a session's working directory
// so the change can be reviewed inside loom before opening an editor.
func (a *App) SessionDiff(name string) DiffDTO {
	cwd := ""
	if a.st != nil {
		if row, ok, _ := a.st.Get(name); ok {
			cwd = row.Cwd
		}
	}
	return gitDiff(cwd)
}
