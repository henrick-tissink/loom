package projects

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/store"
)

// Store is the slice of the store this service writes. Narrowed to an
// interface so the reconciliation rules can be tested without a DB, and so it
// is obvious at a glance that the service never reaches for session rows —
// attribution is computed from paths, never stored (§4).
type Store interface {
	ListProjects() ([]store.Project, error)
	ListAllProjectRepos() ([]store.ProjectRepo, error)
	UpsertProject(store.Project) error
	UpsertProjectRepo(store.ProjectRepo) error
	SetProjectMissing(root string, missing bool, now int64) error
	SetProjectRepoMissing(path string, missing bool) error
}

// Service owns discovery + DB reconciliation and is queried READ-THROUGH.
// loom.db is the runtime source of truth for launch targets: registry.Discover
// used to run once at startup into a by-value slice that buildRecipe, Fanout
// and workflow.LoadAll all validated against, so a project created in-app was
// listed but not launchable, and hide/rename/reassign never reached it.
type Service struct {
	st Store

	mu       sync.Mutex
	warnings []string
}

func New(st Store) *Service { return &Service{st: st} }

// Reconcile folds one discovery pass into the store and then sweeps `missing`.
// It is never fatal: per-row failures (the label collision §2 names, and any
// other write error) are recorded as warnings and the pass continues, because
// one bad row must not cost the user every other project on this launch.
func (s *Service) Reconcile(ps []registry.Project) error {
	now := time.Now().Unix()
	s.mu.Lock()
	s.warnings = nil
	s.mu.Unlock()

	for _, p := range ps {
		if p.Root == store.UngroupedRoot {
			// The reserved row is seeded by migration and owns no directory;
			// a discovered project can never legitimately claim its root.
			continue
		}
		root := Canonical(p.Root)
		// Upsert-without-clobber: discovery inserts what is absent and NEVER
		// overwrites a user-set name, hidden, solo or membership — the
		// discipline UpsertTranscript uses to protect llm_summary. The store
		// enforces it; passing the discovered values here is safe precisely
		// because of that.
		if err := s.st.UpsertProject(store.Project{
			Root: root, Name: p.Name, Origin: "discovered",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			s.warn("project %q not registered: %v", root, err)
			continue
		}
		for _, m := range p.Repos {
			err := s.st.UpsertProjectRepo(store.ProjectRepo{
				Path: Canonical(m.Path), ProjectRoot: root, Label: m.Label, AddedAt: now,
			})
			switch {
			case errors.Is(err, store.ErrLabelTaken):
				// Skip the insert and warn. Aborting would make discovery
				// fatal, and overwriting would silently steal a label a saved
				// workflow definition already resolves through.
				s.warn("repo %q skipped: label %q already in use", m.Path, m.Label)
			case err != nil:
				s.warn("repo %q not registered: %v", m.Path, err)
			}
		}
	}
	return s.Sweep()
}

// Sweep flags rows whose directory has vanished by stat-ing EVERY known row —
// the way memory/indexer.go sweeps file_missing — rather than diffing against
// the current scan set. Diffing would flag every out-of-root member on every
// pass: such a member is absent from the workspace scan by construction, which
// is exactly why it was added by hand.
//
// The flag self-clears, so a repo that comes back needs no user gesture. A
// stat error that is NOT "does not exist" (a permission bit, a stalled mount)
// leaves the row alone: degrading to "present" keeps a launchable project
// launchable, whereas degrading to "missing" dims a project the user can
// perfectly well open.
func (s *Service) Sweep() error {
	now := time.Now().Unix()

	ps, err := s.st.ListProjects()
	if err != nil {
		return err
	}
	for _, p := range ps {
		if p.Root == store.UngroupedRoot {
			continue // reserved row owns no directory
		}
		if missing := gone(p.Root); missing != p.Missing {
			if err := s.st.SetProjectMissing(p.Root, missing, now); err != nil {
				return err
			}
		}
	}

	repos, err := s.st.ListAllProjectRepos()
	if err != nil {
		return err
	}
	for _, m := range repos {
		if missing := gone(m.Path); missing != m.Missing {
			if err := s.st.SetProjectRepoMissing(m.Path, missing); err != nil {
				return err
			}
		}
	}
	return nil
}

func gone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// Resolver reads the project tables and builds §4's authority. It is queried
// read-through on every call rather than cached: the caching version is what
// made an in-app created project unlaunchable until restart, and rebuilding is
// two indexed reads over a table with tens of rows.
func (s *Service) Resolver() (*Resolver, error) {
	ps, err := s.st.ListProjects()
	if err != nil {
		return nil, err
	}
	repos, err := s.st.ListAllProjectRepos()
	if err != nil {
		return nil, err
	}
	return NewResolver(ps, repos), nil
}

// Warnings reports the non-fatal problems from the last Reconcile, for the
// project overview to surface. Discovery is never fatal, so this is the only
// place a skipped row becomes visible — dropping it would make a repo silently
// absent from the launcher with no explanation anywhere.
func (s *Service) Warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.warnings...)
}

func (s *Service) warn(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.warnings = append(s.warnings, fmt.Sprintf(format, args...))
}

// TargetKind distinguishes a project ROOT launch (cwd = root, no add-dirs)
// from a repo launch. The surviving target keeps the association so a
// single-repo project can still be launched as a project (§5).
type TargetKind string

const (
	TargetRoot TargetKind = "root"
	TargetRepo TargetKind = "repo"
)

// Target is one launchable directory.
//
// Label and ProjectName are deliberately two fields, not one. Label is the
// STABLE identifier saved workflow definitions on disk resolve through, so §2
// binds it to the directory basename — a root target's label is the root's
// basename, never projects.name, because name is user-editable and a rename
// must not invalidate a workflow. ProjectName is the mutable display name and
// is what a picker or rail header should show.
type Target struct {
	Kind        TargetKind
	Path        string // absolute; the set's key
	Label       string
	ProjectRoot string
	ProjectName string
	Missing     bool
}

// LaunchTargets is the launch surface: project roots ∪ repo paths, as a SET
// keyed by absolute path. The root row is suppressed when a member repo's path
// equals the root — by the path rule, not len(repos)==1, because §3's
// transcript-only rule and out-of-root membership both produce roots that are
// also repos WITH siblings. The repo row is the survivor: it carries the label
// that saved workflow definitions resolve through, plus the project_root that
// still lets it launch as a project.
//
// Missing targets are returned, flagged: they render dimmed and
// non-launchable, stay in the rail, and self-clear (§7).
func (s *Service) LaunchTargets() ([]Target, error) {
	ps, err := s.st.ListProjects()
	if err != nil {
		return nil, err
	}
	repos, err := s.st.ListAllProjectRepos()
	if err != nil {
		return nil, err
	}
	names := make(map[string]store.Project, len(ps))
	for _, p := range ps {
		names[p.Root] = p
	}

	set := make(map[string]Target, len(ps)+len(repos))
	for _, m := range repos {
		p := names[m.ProjectRoot]
		set[m.Path] = Target{
			Kind: TargetRepo, Path: m.Path, Label: m.Label,
			ProjectRoot: m.ProjectRoot, ProjectName: p.Name,
			Missing: m.Missing || p.Missing,
		}
	}
	for _, p := range ps {
		if p.Root == store.UngroupedRoot {
			continue // reserved row is not launchable
		}
		if _, taken := set[p.Root]; taken {
			continue
		}
		set[p.Root] = Target{
			Kind: TargetRoot, Path: p.Root, Label: filepath.Base(p.Root),
			ProjectRoot: p.Root, ProjectName: p.Name, Missing: p.Missing,
		}
	}

	out := make([]Target, 0, len(set))
	for _, t := range set {
		out = append(out, t)
	}
	sortTargets(out)
	return out, nil
}

// sortTargets gives the set a stable order (by path) so callers, tests and the
// picker do not have to cope with Go's randomized map iteration.
func sortTargets(ts []Target) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Path < ts[j].Path })
}
