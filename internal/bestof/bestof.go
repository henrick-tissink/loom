// Package bestof keeps best-of-N groups: the same seed launched as several
// worktree sessions at once, so their attempts can be compared side by side
// and one kept.
//
// A group remembers the repository, the commit the attempts started from
// (each attempt's change is measured against it), the seed and the
// sessions, in launch order. Kept in ~/.loom/bestof.json; the sessions'
// own rows stay in loom.db, untouched.
package bestof

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Max is the most attempts one group holds.
const Max = 3

// Group is one best-of-N launch.
type Group struct {
	ID       string   `json:"id"`
	Repo     string   `json:"repo"`
	Base     string   `json:"base"` // the repository's HEAD at launch
	Seed     string   `json:"seed"`
	Created  int64    `json:"created"` // unix seconds
	Sessions []string `json:"sessions"`
	// Kept is the attempt the person chose; Discarded the attempts whose
	// worktrees loom removed for it.
	Kept      string   `json:"kept,omitempty"`
	Discarded []string `json:"discarded,omitempty"`
}

// Store is ~/.loom/bestof.json.
type Store struct {
	mu     sync.Mutex
	path   string
	groups map[string]Group
}

// Open reads the store; a missing file is an empty store.
func Open(path string) (*Store, error) {
	s := &Store{path: path, groups: map[string]Group{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Groups []Group `json:"groups"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, g := range f.Groups {
		s.groups[g.ID] = g
	}
	return s, nil
}

// save writes atomically, oldest group first; callers hold mu.
func (s *Store) save() error {
	gs := make([]Group, 0, len(s.groups))
	for _, g := range s.groups {
		gs = append(gs, g)
	}
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].Created != gs[j].Created {
			return gs[i].Created < gs[j].Created
		}
		return gs[i].ID < gs[j].ID
	})
	b, err := json.MarshalIndent(struct {
		Groups []Group `json:"groups"`
	}{gs}, "", "  ")
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

// NewID is a fresh group id.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Put stores a group, replacing one with the same id.
func (s *Store) Put(g Group) error {
	if g.ID == "" {
		return errors.New("bestof: a group needs an id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[g.ID] = g
	return s.save()
}

// Get returns a group.
func (s *Store) Get(id string) (Group, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	return g, ok
}

// Of maps each session in a group to its group and its 1-based place in it.
func (s *Store) Of() map[string]Place {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Place{}
	for _, g := range s.groups {
		for i, n := range g.Sessions {
			out[n] = Place{Group: g.ID, Index: i + 1, Of: len(g.Sessions), Kept: g.Kept}
		}
	}
	return out
}

// Place is a session's place in its group.
type Place struct {
	Group string
	Index int
	Of    int
	Kept  string // the group's kept session, once one is chosen
}

// Prune forgets the groups none of whose sessions still exist (the rows
// were dismissed), as reported by exists.
func (s *Store) Prune(exists func(name string) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id, g := range s.groups {
		live := false
		for _, n := range g.Sessions {
			if exists(n) {
				live = true
				break
			}
		}
		if !live {
			delete(s.groups, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}
