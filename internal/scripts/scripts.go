// Package scripts holds a project's setup and run scripts and the ports
// each workspace runs on.
//
// A workspace is where a session works: a claude worktree's root, or the
// checkout itself. Each workspace has at most one setup job and one run
// job, tmux sessions on loom's server named by Job, so they outlive loom
// the way sessions do and the engine sees them in the same list-panes
// call. A run gets a block of Block ports from LOOM_PORT, the same block
// every time for the same workspace while it is free.
//
// The scripts are the person's, set in loom and kept in
// ~/.loom/scripts.json; nothing here writes to a repository.
package scripts

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Config is one project's scripts. Either may be empty.
type Config struct {
	Setup string `json:"setup"`
	Run   string `json:"run"`
}

type file struct {
	Projects map[string]Config `json:"projects"` // project root → scripts
	Ports    map[string]int    `json:"ports"`    // workspace → first port of its block
}

// Ports are handed out in blocks of Block from First, below Last.
const (
	Block = 10
	First = 42000
	Last  = 49000
)

// Store is ~/.loom/scripts.json.
type Store struct {
	mu   sync.Mutex
	path string
	f    file
	// Free reports whether a port can be listened on; tests replace it.
	Free func(port int) bool
}

// Open reads the store; a missing file is an empty store.
func Open(path string) (*Store, error) {
	s := &Store{path: path, f: file{Projects: map[string]Config{}, Ports: map[string]int{}}, Free: portFree}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.f.Projects == nil {
		s.f.Projects = map[string]Config{}
	}
	if s.f.Ports == nil {
		s.f.Ports = map[string]int{}
	}
	return s, nil
}

// save writes atomically; callers hold mu.
func (s *Store) save() error {
	b, err := json.MarshalIndent(s.f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Get returns a project's scripts.
func (s *Store) Get(root string) Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Projects[filepath.Clean(root)]
}

// Set stores a project's scripts; both empty forgets the project.
func (s *Store) Set(root string, c Config) error {
	c.Setup, c.Run = strings.TrimSpace(c.Setup), strings.TrimSpace(c.Run)
	s.mu.Lock()
	defer s.mu.Unlock()
	root = filepath.Clean(root)
	if c == (Config{}) {
		delete(s.f.Projects, root)
	} else {
		s.f.Projects[root] = c
	}
	return s.save()
}

// For finds the scripts for a directory: the configured project root that
// is the directory or contains it (a worktree sits inside its repository),
// the deepest one winning. ok is false when no project has scripts.
func (s *Store) For(dir string) (root string, c Config, ok bool) {
	dir = filepath.Clean(dir)
	s.mu.Lock()
	defer s.mu.Unlock()
	for r, cfg := range s.f.Projects {
		if (dir == r || strings.HasPrefix(dir, r+string(filepath.Separator))) && len(r) > len(root) {
			root, c, ok = r, cfg, true
		}
	}
	return root, c, ok
}

// Port returns the workspace's block, keeping the one it had when its first
// port is still free (or busy because its own run holds it: pass running)
// and otherwise taking the lowest block whose ports are all free. Blocks of
// workspaces that no longer exist are given back first.
func (s *Store) Port(workspace string, running bool) (int, error) {
	workspace = filepath.Clean(workspace)
	s.mu.Lock()
	defer s.mu.Unlock()
	for w := range s.f.Ports {
		if _, err := os.Stat(w); err != nil && w != workspace {
			delete(s.f.Ports, w)
		}
	}
	if p, ok := s.f.Ports[workspace]; ok && (running || s.Free(p)) {
		return p, nil
	}
	taken := map[int]bool{}
	for w, p := range s.f.Ports {
		if w != workspace {
			taken[p] = true
		}
	}
	for p := First; p+Block <= Last; p += Block {
		if taken[p] || !s.blockFree(p) {
			continue
		}
		s.f.Ports[workspace] = p
		return p, s.save()
	}
	return 0, fmt.Errorf("no free block of %d ports between %d and %d", Block, First, Last)
}

func (s *Store) blockFree(p int) bool {
	for i := 0; i < Block; i++ {
		if !s.Free(p + i) {
			return false
		}
	}
	return true
}

// Ports returns the workspace → port map, for display.
func (s *Store) Ports() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.f.Ports))
	for k, v := range s.f.Ports {
		out[k] = v
	}
	return out
}

// Job kinds.
const (
	Setup = "setup"
	Run   = "run"
)

// JobPrefix starts every job's tmux name. It is not the sessions' "loom-",
// so the engine never adopts a job as a session.
const JobPrefix = "loomjob-"

// Job is the tmux session name of a workspace's job. The path is hashed:
// '.' and ':' break tmux targets.
func Job(kind, workspace string) string {
	sum := sha1.Sum([]byte(filepath.Clean(workspace)))
	return JobPrefix + kind + "-" + hex.EncodeToString(sum[:6])
}

// Command is the shell line tmux runs for a job: the script in the
// person's login shell (their PATH, nvm, and so on), with the workspace's
// variables. port 0 leaves LOOM_PORT out (setup runs before any port).
func Command(script, shell, root, workspace string, port int) string {
	if shell == "" {
		shell = "/bin/zsh"
	}
	env := []string{"LOOM_ROOT_PATH=" + root, "LOOM_WORKSPACE_PATH=" + workspace}
	if port > 0 {
		env = append(env, fmt.Sprintf("LOOM_PORT=%d", port))
	}
	sort.Strings(env)
	args := append([]string{"exec", "env"}, env...)
	args = append(args, shell, "-lc", script)
	q := make([]string, len(args))
	for i, a := range args {
		if i < 2 {
			q[i] = a
			continue
		}
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}
