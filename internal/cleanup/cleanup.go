// Package cleanup finds and removes disposable build junk from a repo — the
// intersection of "git already ignores it" AND "it matches a known-junk
// pattern". That intersection is the safety guarantee: a tracked file is never
// touched (git clean -X only ever lists ignored paths), and a git-ignored file
// that is not build junk (a .env, a local config) is never touched either (it
// won't match a junk pattern).
package cleanup

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// junkDirs are directory basenames that are pure build/dependency/cache output.
var junkDirs = map[string]bool{
	"node_modules": true, "dist": true, "build": true, "target": true,
	"__pycache__": true, ".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true,
	".next": true, ".turbo": true, ".gradle": true, ".parcel-cache": true,
	"coverage": true, ".nyc_output": true,
}

// junkFile reports whether a basename is a throwaway file.
func junkFile(name string) bool {
	switch name {
	case ".DS_Store", "Thumbs.db", "npm-debug.log", "yarn-error.log":
		return true
	}
	switch filepath.Ext(name) {
	case ".log", ".pyc", ".pyo":
		return true
	}
	return false
}

// Entry is one removable path.
type Entry struct {
	Repo  string `json:"repo"`  // the repo root it lives under
	Rel   string `json:"rel"`   // path relative to Repo
	Abs   string `json:"abs"`   // absolute path
	Size  int64  `json:"size"`  // bytes (walked, for dirs)
	IsDir bool   `json:"isDir"`
}

// isJunk decides whether a git-ignored path is build junk we may remove: any
// path segment is a junk dir, or the basename is a junk file.
func isJunk(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if junkDirs[seg] {
			return true
		}
	}
	return junkFile(filepath.Base(rel))
}

// Scan lists removable junk across the given repo roots. It asks git which paths
// are ignored (`git clean -Xdn` — dry-run, ignored-only, directories collapsed),
// then keeps only those matching a junk pattern. A non-git root or a git error
// yields nothing for that root: we only ever remove what git confirms it ignores.
func Scan(roots []string) ([]Entry, error) {
	var out []Entry
	for _, root := range roots {
		outBytes, err := exec.Command("git", "-C", root, "clean", "-Xdn").Output()
		if err != nil {
			continue // not a git repo, or git unavailable — skip
		}
		sc := bufio.NewScanner(strings.NewReader(string(outBytes)))
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "Would remove ") {
				continue
			}
			rel := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, "Would remove ")), "/")
			if rel == "" || !isJunk(rel) {
				continue
			}
			abs := filepath.Join(root, rel)
			fi, serr := os.Stat(abs)
			if serr != nil {
				continue
			}
			e := Entry{Repo: root, Rel: rel, Abs: abs, IsDir: fi.IsDir()}
			if fi.IsDir() {
				e.Size = dirSize(abs)
			} else {
				e.Size = fi.Size()
			}
			out = append(out, e)
		}
	}
	return out, nil
}

func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, e := d.Info(); e == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// Remove deletes the given absolute paths, but only ones a fresh Scan of roots
// still reports as junk — so a stale or hand-crafted path from a caller can
// never delete anything outside the confirmed set. Returns bytes freed.
func Remove(roots, abs []string) (int64, error) {
	safe, err := Scan(roots)
	if err != nil {
		return 0, err
	}
	allowed := make(map[string]int64, len(safe))
	for _, e := range safe {
		allowed[e.Abs] = e.Size
	}
	var freed int64
	for _, p := range abs {
		sz, ok := allowed[p]
		if !ok {
			continue // not in the confirmed junk set — refuse
		}
		if err := os.RemoveAll(p); err != nil {
			return freed, err
		}
		freed += sz
	}
	return freed, nil
}
