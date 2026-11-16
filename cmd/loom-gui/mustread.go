package main

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/mustread"
	"github.com/henricktissink/loom/internal/worktree"
)

// Files a person must read (internal/mustread): the review sheet lists them
// first and is not marked reviewed until each is viewed in the diff, and
// the diff puts them first. Tests passing is no evidence for them.

// MustReadDTO is one changed file a person must read.
type MustReadDTO struct {
	Repo   string `json:"repo"` // the diff section's path
	Label  string `json:"label"`
	File   string `json:"file"` // as the diff names it
	Path   string `json:"path"` // absolute, to open it
	Why    string `json:"why"`
	Source string `json:"source"` // ".loom/must-read" or "defaults"
}

// mustReadIn matches a diff's changed and new files against the rules of
// the repository dir is in, which load from its root. git names changed
// files from the root but new (untracked) ones from dir, so those are
// matched with dir's place in the repository prefixed; each is reported
// as the diff names it.
func mustReadIn(dir, label string, changed, untracked []string) []MustReadDTO {
	if dir == "" {
		return nil
	}
	root := dir
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if top, err := worktree.Toplevel(ctx, dir); err == nil && top != "" {
		root = top
	}
	cancel()
	rules, _ := mustread.Load(root) // an unreadable file falls back to the defaults
	prefix := ""
	if r, err := filepath.Rel(realPath(root), realPath(dir)); err == nil && r != "." && !strings.HasPrefix(r, "..") {
		prefix = filepath.ToSlash(r) + "/"
	}
	seen := map[string]bool{}
	var out []MustReadDTO
	add := func(f, rel, base string) {
		if seen[f] {
			return
		}
		seen[f] = true
		if r, ok := rules.Match(rel); ok {
			out = append(out, MustReadDTO{Repo: dir, Label: label, File: f, Path: filepath.Join(base, f), Why: r.Why, Source: rules.Source})
		}
	}
	for _, f := range changed {
		add(f, f, root)
	}
	for _, f := range untracked {
		add(f, prefix+f, dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// mustReadMap is a diff's must-read files by section path and file, for
// the diff view's ordering and badges.
func mustReadMap(ms []MustReadDTO) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, m := range ms {
		if out[m.Repo] == nil {
			out[m.Repo] = map[string]string{}
		}
		out[m.Repo][m.File] = m.Why
	}
	return out
}
