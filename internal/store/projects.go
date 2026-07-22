package store

// Projects (spec docs/superpowers/specs/2026-07-22-projects-foundation-design.md
// §7). A project is a named set of repos plus a root directory — an axis
// distinct from a repo. This file owns the rows only: attribution and
// visibility (§4/§6) live in a resolver package above the store, so nothing
// here knows what "hidden" means beyond persisting the flag.

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
)

type Project struct {
	Root      string // absolute, Abs+Clean canonical; '' is the reserved Ungrouped row
	Name      string
	Origin    string // 'discovered' | 'created' | 'reserved'
	Hidden    bool
	Solo      bool
	Missing   bool
	Collapsed bool // rail section collapsed (§8) — a project flag, not a GUI-local setting
	CreatedAt int64
	UpdatedAt int64
}

type ProjectRepo struct {
	Path        string // absolute, canonical; PK enforces §2 exclusivity
	ProjectRoot string
	Label       string
	Missing     bool
	AddedAt     int64
}

// UngroupedRoot is the reserved project seeded by migration v7. §4's
// longest-prefix scan must exclude it — an empty root prefixes everything.
const UngroupedRoot = ""

var (
	// ErrLabelTaken reports a collision on the UNIQUE label index. §2 makes
	// this non-fatal by contract: discovery skips the conflicting insert and
	// surfaces a warning, so the error is a distinguishable value rather than
	// an opaque constraint failure the reconciler would have to abort on.
	ErrLabelTaken = errors.New("store: repo label already taken")
	// ErrNoProject reports a write naming a project root with no row. Writing
	// the membership anyway would strand the repo under a root nothing lists.
	ErrNoProject = errors.New("store: no such project")
	// ErrRootConflict reports a re-point whose target root (or one of its
	// rewritten member paths) is already occupied. Merging the two silently
	// would destroy one project's curated membership under a PK, so the whole
	// transaction is refused and the user is told.
	ErrRootConflict = errors.New("store: target root already in use")
	// ErrReservedRoot guards the Ungrouped row against being re-pointed or
	// reassigned away — it is addressed by its literal '' root everywhere.
	ErrReservedRoot = errors.New("store: reserved project root")
)

const projectCols = "root, name, origin, hidden, solo, missing, collapsed, created_at, updated_at"

// UpsertProject inserts a project and NEVER updates an existing row. Discovery
// re-runs on every launch and must not clobber a user-set name, hidden, solo
// or membership — the discipline UpsertTranscript uses to protect llm_summary,
// pushed one step further because here EVERY mutable field has a user-facing
// setter. `missing` is likewise not refreshed from here: it is owned by the
// sweep (SetProjectMissing), which stats every known row rather than diffing
// against the scan set (§7 retirement).
func (s *Store) UpsertProject(p Project) error {
	_, err := s.db.Exec(`INSERT INTO projects (`+projectCols+`)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(root) DO NOTHING`,
		p.Root, p.Name, p.Origin, p.Hidden, p.Solo, p.Missing, p.Collapsed, p.CreatedAt, p.UpdatedAt)
	return err
}

func (s *Store) GetProject(root string) (Project, bool, error) {
	var p Project
	err := s.db.QueryRow("SELECT "+projectCols+" FROM projects WHERE root=?", root).Scan(
		&p.Root, &p.Name, &p.Origin, &p.Hidden, &p.Solo, &p.Missing, &p.Collapsed, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return Project{}, false, nil
	}
	if err != nil {
		return Project{}, false, err
	}
	return p, true, nil
}

// ListProjects returns every project row including the reserved Ungrouped one.
// Callers order for display (§7: needs-you first, then name, Ungrouped last);
// the store returns a stable name order so tests and the sweep are
// deterministic.
func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query("SELECT " + projectCols + " FROM projects ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.Root, &p.Name, &p.Origin, &p.Hidden, &p.Solo,
			&p.Missing, &p.Collapsed, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProjectName is the only writer of name — discovery derives labels from
// the root basename (§2), so a rename never invalidates saved workflows.
func (s *Store) SetProjectName(root, name string, now int64) error {
	_, err := s.db.Exec("UPDATE projects SET name=?, updated_at=? WHERE root=?", name, now, root)
	return err
}

func (s *Store) SetProjectHidden(root string, hidden bool, now int64) error {
	_, err := s.db.Exec("UPDATE projects SET hidden=?, updated_at=? WHERE root=?", hidden, now, root)
	return err
}

// SetProjectSolo makes root the single soloed project, clearing any other, in
// one transaction. The clear must precede the set or the partial unique index
// (idx_projects_solo) rejects the moment two rows hold solo=1 — and clearing
// afterwards would leave the old solo winning on a mid-flight crash, i.e. a
// project the user just took OUT of solo silently on screen. `hidden` is never
// touched, so leaving solo restores the prior state exactly (§6.1).
func (s *Store) SetProjectSolo(root string, solo bool, now int64) error {
	return s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE projects SET solo=0, updated_at=? WHERE solo=1", now); err != nil {
			return err
		}
		if !solo {
			return nil
		}
		_, err := tx.Exec("UPDATE projects SET solo=1, updated_at=? WHERE root=?", now, root)
		return err
	})
}

// SetProjectCollapsed persists the rail section's collapse state. It does NOT
// touch updated_at: collapse is a view gesture, and letting it bump the row's
// timestamp would make "when did this project last change" answer "whenever I
// last folded the rail".
func (s *Store) SetProjectCollapsed(root string, collapsed bool) error {
	_, err := s.db.Exec("UPDATE projects SET collapsed=? WHERE root=?", collapsed, root)
	return err
}

func (s *Store) SetProjectMissing(root string, missing bool, now int64) error {
	_, err := s.db.Exec("UPDATE projects SET missing=?, updated_at=? WHERE root=?", missing, now, root)
	return err
}

const repoCols = "path, project_root, label, missing, added_at"

// UpsertProjectRepo inserts a membership row and, like UpsertProject, never
// updates an existing one: membership is user-curated (reassignment is an
// explicit gesture) and `missing` belongs to the sweep. A label collision from
// a DIFFERENT path is returned as ErrLabelTaken so the reconciler can skip and
// warn instead of aborting the pass (§2: discovery is never fatal).
func (s *Store) UpsertProjectRepo(r ProjectRepo) error {
	_, err := s.db.Exec(`INSERT INTO project_repos (`+repoCols+`)
		VALUES (?,?,?,?,?)
		ON CONFLICT(path) DO NOTHING`,
		r.Path, r.ProjectRoot, r.Label, r.Missing, r.AddedAt)
	return labelErr(err)
}

// labelErr maps the UNIQUE-label failure to the sentinel. Matching on the
// column rather than on the driver's code is deliberate: the same code covers
// the path PK, and those two failures have opposite handling — a path
// conflict is the normal re-discovery case (absorbed by DO NOTHING), a label
// conflict is the skip-and-warn case.
func labelErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "project_repos.label") {
		return ErrLabelTaken
	}
	return err
}

func (s *Store) GetProjectRepo(path string) (ProjectRepo, bool, error) {
	var r ProjectRepo
	err := s.db.QueryRow("SELECT "+repoCols+" FROM project_repos WHERE path=?", path).Scan(
		&r.Path, &r.ProjectRoot, &r.Label, &r.Missing, &r.AddedAt)
	if err == sql.ErrNoRows {
		return ProjectRepo{}, false, nil
	}
	if err != nil {
		return ProjectRepo{}, false, err
	}
	return r, true, nil
}

func (s *Store) ListProjectRepos(root string) ([]ProjectRepo, error) {
	return s.queryRepos("SELECT "+repoCols+" FROM project_repos WHERE project_root=? ORDER BY label", root)
}

// ListAllProjectRepos backs the `missing` sweep, which stats EVERY known row
// rather than diffing against the current scan set — an out-of-root member is
// absent from every scan and diffing would flag it missing on each pass (§7).
func (s *Store) ListAllProjectRepos() ([]ProjectRepo, error) {
	return s.queryRepos("SELECT " + repoCols + " FROM project_repos ORDER BY label")
}

func (s *Store) queryRepos(q string, args ...any) ([]ProjectRepo, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectRepo
	for rows.Next() {
		var r ProjectRepo
		if err := rows.Scan(&r.Path, &r.ProjectRoot, &r.Label, &r.Missing, &r.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SetProjectRepoMissing(path string, missing bool) error {
	_, err := s.db.Exec("UPDATE project_repos SET missing=? WHERE path=?", missing, path)
	return err
}

// ReassignRepo moves one repo to another project, relabelling it in the same
// statement (the label carries the project segment, §2). "Remove repo from
// project" is defined as reassignment to a single-repo project at the repo's
// own path — the row must persist or discovery re-absorbs it next launch.
// The target project must already exist: writing the membership anyway would
// strand the repo under a root nothing lists.
func (s *Store) ReassignRepo(path, newRoot, newLabel string) error {
	return s.tx(func(tx *sql.Tx) error {
		if err := projectExists(tx, newRoot); err != nil {
			return err
		}
		res, err := tx.Exec("UPDATE project_repos SET project_root=?, label=? WHERE path=?",
			newRoot, newLabel, path)
		if err != nil {
			return labelErr(err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// RepointProject moves a project to a new root in ONE transaction: the root
// itself, every member's project_root, and the member paths that lived under
// the old root. root is the PK, so a plain `mv` on disk followed by discovery
// would mint a second project row and strand the first with its curated
// membership — this is the action that repairs a renamed directory.
//
// Prefix rewriting is segment-wise, never a raw string prefix: `…/HappyPay`
// is a string prefix of the sibling `…/HappyPayCoreApi` (§4). Members outside
// the old root keep their paths — they were added deliberately and the
// directory move says nothing about them.
//
// Conflict rule: if the target root already holds a project, or any rewritten
// member path is already owned by a repo that is not itself moving, the whole
// re-point is refused with ErrRootConflict and nothing changes. Merging would
// silently destroy one side's membership under the PK, and a partial move is
// worse than no move.
//
// `missing` is deliberately left alone: the sweep stats every known row and
// self-clears it, so clearing it here would only duplicate that authority.
func (s *Store) RepointProject(oldRoot, newRoot string, now int64) error {
	if oldRoot == newRoot {
		return nil
	}
	if oldRoot == UngroupedRoot || newRoot == UngroupedRoot {
		return ErrReservedRoot
	}
	return s.tx(func(tx *sql.Tx) error {
		if err := projectExists(tx, oldRoot); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow("SELECT count(*) FROM projects WHERE root=?", newRoot).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrRootConflict
		}

		// Deepest path first: when the new root sits UNDER the old one
		// (`/a` → `/a/inner`), a shallow member rewritten early would land on
		// a path a not-yet-moved sibling still occupies.
		rows, err := tx.Query(
			"SELECT path FROM project_repos WHERE project_root=? ORDER BY length(path) DESC", oldRoot)
		if err != nil {
			return err
		}
		var moves [][2]string // {old, new}
		moving := map[string]bool{}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			if np, ok := rewriteUnder(p, oldRoot, newRoot); ok {
				moves = append(moves, [2]string{p, np})
				moving[p] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, m := range moves {
			var owner string
			err := tx.QueryRow("SELECT path FROM project_repos WHERE path=?", m[1]).Scan(&owner)
			if err == nil && !moving[owner] {
				return ErrRootConflict
			}
			if err != nil && err != sql.ErrNoRows {
				return err
			}
		}

		if _, err := tx.Exec("UPDATE projects SET root=?, updated_at=? WHERE root=?",
			newRoot, now, oldRoot); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE project_repos SET project_root=? WHERE project_root=?",
			newRoot, oldRoot); err != nil {
			return err
		}
		for _, m := range moves {
			if _, err := tx.Exec("UPDATE project_repos SET path=? WHERE path=?",
				m[1], m[0]); err != nil {
				return err
			}
		}
		return nil
	})
}

// rewriteUnder re-bases p from oldRoot onto newRoot, segment-wise. Reports
// false when p does not live under oldRoot, which leaves out-of-root members
// untouched.
func rewriteUnder(p, oldRoot, newRoot string) (string, bool) {
	if p == oldRoot {
		return newRoot, true
	}
	sep := string(filepath.Separator)
	if strings.HasPrefix(p, oldRoot+sep) {
		return newRoot + p[len(oldRoot):], true
	}
	return "", false
}

func projectExists(tx *sql.Tx, root string) error {
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM projects WHERE root=?", root).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNoProject
	}
	return nil
}

// tx runs fn in a transaction, rolling back on any error. Every statement
// inside fn MUST go through the passed *sql.Tx: Open pins the pool to one
// connection, so a stray s.db call from inside a transaction deadlocks.
func (s *Store) tx(fn func(*sql.Tx) error) (err error) {
	t, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			t.Rollback()
		}
	}()
	if err = fn(t); err != nil {
		return err
	}
	return t.Commit()
}
