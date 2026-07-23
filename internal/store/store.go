// Package store owns Loom's SQLite state. Two sources of truth (spec §6):
// tmux for LIVE sessions, this store for HISTORY — terminal rows are never
// deleted by reconciliation.
package store

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

// Open applies the mandatory concurrency pragmas (spec §5): WAL for
// cross-process safety, busy_timeout against SQLITE_BUSY, and a single
// connection so one process never self-contends.
func Open(path string) (*Store, error) { return OpenWithDriver("sqlite", path) }

// OpenWithDriver is Open over a caller-supplied database/sql driver name.
//
// It exists for one reason, and it is worth stating so nobody mistakes it for
// configuration: some bindings promise a QUERY COUNT rather than a value —
// orchestrator §13 binds "ListProjectDetails issues one orchestrator query for
// N projects (no N+1)" — and a promise about how many statements ran cannot be
// asserted from outside the driver. A test registers a counting driver that
// wraps modernc's and passes its name here.
//
// The rejected alternative was a counter on Store itself, incremented in every
// query path: that is production code carrying a test's apparatus through every
// call site, and it would drift out of date the first time someone added a
// query without the increment. This adds nothing to the hot path.
//
// Production calls Open. Nothing else should call this.
func OpenWithDriver(driverName, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	migrations := []string{
		// v1
		`CREATE TABLE sessions (
			name              TEXT PRIMARY KEY,
			claude_session_id TEXT NOT NULL,
			project_label     TEXT NOT NULL,
			cwd               TEXT NOT NULL,
			model             TEXT NOT NULL DEFAULT '',
			mode              TEXT NOT NULL DEFAULT '',
			seed              TEXT NOT NULL DEFAULT '',
			tags              TEXT NOT NULL DEFAULT '',
			created_at        INTEGER NOT NULL,
			ended_at          INTEGER NOT NULL DEFAULT -1,
			exit_code         INTEGER NOT NULL DEFAULT -1,
			last_status       TEXT NOT NULL DEFAULT 'unknown'
		)`,
		// v2: track whether an async seed (session/launch.go seedWhenReady) was
		// actually delivered, so a silently-dropped seed (timeout/send error) is
		// visible instead of vanishing. '' = no seed or not yet resolved,
		// 'sent' = delivered, 'failed' = timed out or the tmux send failed.
		`ALTER TABLE sessions ADD COLUMN seed_status TEXT NOT NULL DEFAULT ''`,
		// v3: session title, captured from the transcript's ai-title sidecar
		// record and persisted so it survives restarts (spec: session titles).
		`ALTER TABLE sessions ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
		// v4: memory store (spec §3) — searchable archive of every claude
		// transcript (main + subagent), incrementally indexed per source file.
		// IF NOT EXISTS on every object is belt-and-braces re-entrancy: it
		// combines with the per-migration transaction below so a DB where
		// these objects exist but user_version is stale (a pre-fix partial
		// apply) still opens cleanly instead of bricking (see
		// TestMigrationsAreTransactional).
		`CREATE TABLE IF NOT EXISTS transcripts (
			session_id  TEXT PRIMARY KEY,
			project_dir TEXT NOT NULL,
			cwd         TEXT NOT NULL DEFAULT '',
			title       TEXT NOT NULL DEFAULT '',
			first_ts    INTEGER NOT NULL DEFAULT 0,
			last_ts     INTEGER NOT NULL DEFAULT 0,
			msg_count   INTEGER NOT NULL DEFAULT 0,
			ask         TEXT NOT NULL DEFAULT '',
			outcome     TEXT NOT NULL DEFAULT '',
			files       TEXT NOT NULL DEFAULT '',
			file_missing INTEGER NOT NULL DEFAULT 0,
			llm_summary TEXT NOT NULL DEFAULT '',
			summary_at  INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS indexed_files (
			path        TEXT PRIMARY KEY,
			session_id  TEXT NOT NULL,
			size        INTEGER NOT NULL,
			mtime       INTEGER NOT NULL,
			first_rowid INTEGER NOT NULL DEFAULT 0,
			last_rowid  INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_indexed_files_session ON indexed_files(session_id);
		CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			content, session_id UNINDEXED, role UNINDEXED, ts UNINDEXED
		)`,
		// v5: workflow runs (spec §2.4, docs/superpowers/specs/
		// 2026-07-03-workflows-design.md) — persisted state for saved
		// multi-step workflow chains. session_names is a JSON array
		// (invariant len==step_idx+1, enforced by the runner, not the
		// store); pending_seed is the undelivered continue/fork seed
		// (§2.9). IF NOT EXISTS for the same re-entrancy reason as v4.
		`CREATE TABLE IF NOT EXISTS workflow_runs (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT NOT NULL,
			def_json      TEXT NOT NULL,
			step_idx      INTEGER NOT NULL,
			session_names TEXT NOT NULL,
			pending_seed  TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		)`,
		// v6: recall (spec docs/superpowers/specs/2026-07-04-recall-design.md
		// §6) — supports RecentTranscriptsByProjectDir, the same-project
		// recency query used both as the panel's default (no seed typed yet)
		// and as the fallback when the recall query builder can't produce a
		// usable FTS expression or zero hits qualify. IF NOT EXISTS for the
		// same re-entrancy reason as v4/v5.
		`CREATE INDEX IF NOT EXISTS idx_transcripts_project ON transcripts(project_dir, last_ts)`,
		// v7: projects (spec docs/superpowers/specs/
		// 2026-07-22-projects-foundation-design.md §7) — a project is a named
		// set of repos plus a root directory, orthogonal to a repo. root is the
		// PK because attribution (§4) is a longest-prefix match over
		// {projects.root} ∪ {project_repos.path}; project_repos.path is a PK
		// because §2's exclusivity ("one repo belongs to exactly one project")
		// is enforced by the schema rather than by every writer remembering to.
		// The partial unique index is what makes "at most one project is solo"
		// a database fact — solo lives here and not in a GUI settings file so
		// the TUI reads the same flag off the same DB (§6.1).
		// IF NOT EXISTS for the same re-entrancy reason as v4/v5/v6.
		//
		// Ungrouped is seeded here as a reserved row rather than computed as a
		// bucket: §6's visibility predicate keys on project rows, and a
		// computed bucket forces a second branch into every surface — a
		// surface that forgets one still passes its test. root='' is chosen so
		// it can never win §4's longest-prefix match (the resolver excludes it
		// explicitly; an empty root would otherwise prefix everything).
		`CREATE TABLE IF NOT EXISTS projects (
			root       TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			origin     TEXT NOT NULL,
			hidden     INTEGER NOT NULL DEFAULT 0,
			solo       INTEGER NOT NULL DEFAULT 0,
			missing    INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_solo ON projects(solo) WHERE solo = 1;
		CREATE TABLE IF NOT EXISTS project_repos (
			path         TEXT PRIMARY KEY,
			project_root TEXT NOT NULL,
			label        TEXT NOT NULL,
			missing      INTEGER NOT NULL DEFAULT 0,
			added_at     INTEGER NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_project_repos_label ON project_repos(label);
		CREATE INDEX IF NOT EXISTS idx_project_repos_project ON project_repos(project_root);
		INSERT OR IGNORE INTO projects (root, name, origin, hidden, solo, missing, created_at, updated_at)
			VALUES ('', 'Ungrouped', 'reserved', 0, 0, 0, strftime('%s','now'), strftime('%s','now'))`,
		// v8: multi-repo launch (§5). add_dirs is a JSON array of the extra
		// directories passed as `--add-dir` at launch; without persisting it a
		// resumed multi-repo session silently comes back seeing one repo, and
		// the failure only surfaces when a sibling write fails mid-turn.
		//
		// This ALTER gets its OWN migration slot, never folded into v7: ALTER
		// TABLE has no IF NOT EXISTS, so bundling it would make the whole v7
		// slot non-re-entrant and a replay from a stale user_version (§9)
		// would fail on the ALTER even though every CREATE beside it is
		// idempotent. Same standalone-ALTER shape as v2/v3.
		`ALTER TABLE sessions ADD COLUMN add_dirs TEXT NOT NULL DEFAULT ''`,
		// v9: rail collapse state (§8). It lives here beside hidden/solo rather
		// than in a GUI settings file or localStorage, for the same reason solo
		// does: §8 says "alongside the other project flags, not in a third
		// store", and a third store is where the two frontends start
		// disagreeing about the same project.
		//
		// Own slot, again because ALTER is not re-entrant — folding it into v7
		// would break a replay from a stale user_version even though every
		// CREATE in that slot is idempotent.
		`ALTER TABLE projects ADD COLUMN collapsed INTEGER NOT NULL DEFAULT 0`,
		// v10: orchestrators (spec docs/superpowers/specs/
		// 2026-07-22-orchestrator-brief-design.md §9) — at most one
		// orchestrator session per project, enforced by the PK rather than by a
		// UI guard, because two Loom instances against one DB is a supported
		// state and a UI guard is per-process.
		//
		// The row is a POINTER, not a record: everything in it is rederivable
		// by scanning live sessions for the `orch` tag and attributing their
		// cwds, so losing the table costs a singleton guarantee for one poll
		// interval and never a session or a note.
		//
		// session_name='' means "claim in flight" — the claim is taken BEFORE
		// the launch (the AdvanceRunCAS claim-before-side-effect discipline),
		// so a launch that fails after a successful claim strands a row that
		// the orchestrator sweep deletes once it is older than its grace.
		// ended_at=-1 means live; a terminated orchestrator's row is KEPT so
		// the overview can say "last orchestrator ran Tuesday", and a new
		// spawn overwrites it (see ClaimOrchestrator's conflict predicate).
		`CREATE TABLE IF NOT EXISTS orchestrators (
			project_root      TEXT PRIMARY KEY,
			session_name      TEXT NOT NULL DEFAULT '',
			claude_session_id TEXT NOT NULL DEFAULT '',
			spawned_at        INTEGER NOT NULL,
			ended_at          INTEGER NOT NULL DEFAULT -1
		)`,
		// v11: the project's notes directory (orchestrator spec §3) — where the
		// agent-authored brain (loom-map.md / loom-decisions.md / loom-open.md)
		// lives. Empty means "not materialized yet"; the first spawn writes the
		// literal path back here rather than deriving it on read, because
		// RepointProject changes `root` and a derived default would silently
		// relocate a project's whole brain on a directory rename.
		//
		// Own slot: ALTER has no IF NOT EXISTS, same reason as v2/v3/v8/v9.
		`ALTER TABLE projects ADD COLUMN notes_dir TEXT NOT NULL DEFAULT ''`,
		// v12: delegation (spec docs/superpowers/specs/
		// 2026-07-22-delegation-design.md §13.1) — orchestration runs, their
		// tasks, and the artifacts tasks publish.
		//
		// RENUMBERED FROM THE SPEC'S v10/v11 (deviation, deliberate): both the
		// orchestrator spec §9 and the delegation spec §13.1 were written
		// against a v9 head and both allocated v10/v11 for different DDL. Two
		// slots cannot hold four migrations. The orchestrator pair keeps v10/v11
		// (it is the earlier slice) and delegation moves to v12/v13. The failure
		// this avoids is the one the orchestrator spec §9 describes for its own
		// renumbering: migrate() loops `for i := v; i < len(migrations)`, so a
		// DB already at the colliding version would skip the new slot entirely
		// and open cleanly WITHOUT the tables — green on a fresh DB, broken on
		// a real one.
		//
		// manifest_json is a SNAPSHOT (workflow_runs.def_json precedent): a run
		// replays what it was created from even if the on-disk manifest is
		// edited or deleted underneath it. base_shas pins one commit per repo at
		// run creation so every child of a run branches from the same place.
		//
		// delegation_runs.integration and delegation_tasks.spawn_snapshot carry
		// no writer in slice 3a — §§10 and 12.3.3 are deferred until 3a has run
		// on a real initiative. They are declared here anyway because adding a
		// column later costs a standalone ALTER slot, and the whole point of
		// landing the schema in one pass is that no later slice has to touch
		// the migration list.
		//
		// delegation_amendments (§11.3) is deliberately ABSENT: amendments are
		// part of the deferred §§9-12 block, and an empty table is an invitation
		// to write to it. It gets its own slot when that work is unparked.
		`CREATE TABLE IF NOT EXISTS delegation_runs (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			slug          TEXT NOT NULL UNIQUE,
			name          TEXT NOT NULL,
			project_root  TEXT NOT NULL,
			manifest_json TEXT NOT NULL,
			base_shas     TEXT NOT NULL,
			integration   TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_druns_project ON delegation_runs(project_root, created_at);
		CREATE TABLE IF NOT EXISTS delegation_tasks (
			run_id         INTEGER NOT NULL,
			task_id        TEXT NOT NULL,
			state          TEXT NOT NULL,
			session_name   TEXT NOT NULL DEFAULT '',
			repo_label     TEXT NOT NULL,
			worktree       TEXT NOT NULL DEFAULT '',
			branch         TEXT NOT NULL DEFAULT '',
			base_sha       TEXT NOT NULL DEFAULT '',
			base_producers TEXT NOT NULL DEFAULT '',
			check_status   TEXT NOT NULL DEFAULT '',
			check_exit     INTEGER NOT NULL DEFAULT 0,
			check_out      TEXT NOT NULL DEFAULT '',
			check_at       INTEGER NOT NULL DEFAULT 0,
			branch_head    TEXT NOT NULL DEFAULT '',
			block_json     TEXT NOT NULL DEFAULT '',
			pending_seed   TEXT NOT NULL DEFAULT '',
			divergence     TEXT NOT NULL DEFAULT '',
			spawn_snapshot TEXT NOT NULL DEFAULT '',
			flags          TEXT NOT NULL DEFAULT '',
			updated_at     INTEGER NOT NULL,
			PRIMARY KEY (run_id, task_id)
		);
		CREATE INDEX IF NOT EXISTS idx_dtasks_state ON delegation_tasks(state);
		CREATE TABLE IF NOT EXISTS delegation_artifacts (
			run_id       INTEGER NOT NULL,
			artifact_id  TEXT NOT NULL,
			task_id      TEXT NOT NULL,
			path         TEXT NOT NULL,
			fingerprint  TEXT NOT NULL DEFAULT '',
			commit_sha   TEXT NOT NULL DEFAULT '',
			published_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (run_id, artifact_id)
		)`,
		// v13: the reverse lookup from a session row to the delegation task it
		// belongs to, as "<runID>:<taskID>" (delegation spec §13.1/§14.1).
		//
		// It exists because attribution needs it and tags cannot serve: a
		// child's cwd is `~/.loom/worktrees/…`, which matches no
		// {projects.root} ∪ {project_repos.path} target, so Resolver.Attribute
		// fails and Visible fails CLOSED — the moment anything is hidden or
		// soloed, every delegation child vanishes from the rail, including when
		// the user soloed the run's own project. §14.1's override resolves
		// runID → delegation_runs.project_root instead, keyed on identity
		// rather than on path geometry.
		//
		// Own slot: ALTER has no IF NOT EXISTS, same reason as v2/v3/v8/v9/v11.
		`ALTER TABLE sessions ADD COLUMN delegation TEXT NOT NULL DEFAULT ''`,
	}
	for i := v; i < len(migrations); i++ {
		if err := s.applyMigration(i+1, migrations[i]); err != nil {
			return err
		}
	}
	return nil
}

// isStandaloneAddColumn reports whether a migration slot is exactly one
// `ALTER TABLE <t> ADD [COLUMN] …` — the only shape whose "already applied"
// error is safe to swallow (see applyMigration).
//
// Deliberately conservative: anything with a second statement, an embedded
// newline, or an unexpected word order answers false, and false only means the
// error surfaces as a migration failure. A false POSITIVE is the dangerous
// direction, because it would let a partially-applied slot commit.
func isStandaloneAddColumn(ddl string) bool {
	s := strings.TrimSuffix(strings.TrimSpace(ddl), ";")
	if strings.ContainsAny(s, ";\n") {
		return false
	}
	f := strings.Fields(strings.ToUpper(s))
	return len(f) >= 5 && f[0] == "ALTER" && f[1] == "TABLE" && f[3] == "ADD"
}

// applyMigration runs one migration's DDL and its user_version bump inside a
// single transaction (runner fix, spec §3): a crash or error between the two
// under the old two-Exec-calls approach could leave objects created but the
// version un-bumped, bricking the next Open(). Verified: virtual-table
// creation and PRAGMA user_version both work fine inside a modernc sqlite tx.
func (s *Store) applyMigration(version int, ddl string) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	if _, err = tx.Exec(ddl); err != nil {
		// A replay from a stale user_version (see TestMigrationsAreTransactional)
		// re-runs DDL that is already applied. Every CREATE carries IF NOT
		// EXISTS; ALTER TABLE ADD COLUMN has no such clause, so its
		// already-applied signal is this error, and re-adding a column that
		// exists is semantically a no-op — swallow it and bump the version
		// rather than bricking Open().
		//
		// Gated on the slot being a STANDALONE ALTER rather than applied to
		// every migration: in a multi-statement slot the same swallow would
		// commit a half-applied migration, silently stranding every statement
		// after the failing one and bumping user_version over the damage. That
		// is exactly the failure applyMigration's single transaction exists to
		// prevent, so the exemption is scoped to the one statement shape whose
		// re-run is provably a no-op — which is also why the house rule keeps
		// each ALTER alone in its slot (v2/v3/v8/v9).
		if !(isStandaloneAddColumn(ddl) && strings.Contains(err.Error(), "duplicate column name")) {
			return fmt.Errorf("migration %d: %w", version, err)
		}
		err = nil
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("migration %d: pragma user_version: %w", version, err)
	}
	return tx.Commit()
}

type SessionRow struct {
	Name            string
	ClaudeSessionID string
	ProjectLabel    string
	Cwd             string
	Model           string
	Mode            string
	Seed            string
	Tags            string
	CreatedAt       int64
	EndedAt         int64 // -1 = still live
	ExitCode        int64 // -1 = unknown
	LastStatus      string
	SeedStatus      string // '', 'sent', 'failed' — see migration v2
	Title           string // ai-generated session title — see migration v3
	// AddDirs is a JSON array of extra directories the session was launched
	// with (`--add-dir`), '' when there are none — see migration v8. Kept as
	// the raw JSON string, not []string, so SessionRow stays comparable and
	// the store stays a dumb pipe (the same discipline as workflow_runs.
	// session_names, whose invariants the runner owns).
	AddDirs string
	// Delegation is "<runID>:<taskID>" for a delegation child, '' otherwise —
	// see migration v13. It is the identity attribution keys on: a child's cwd
	// is a worktree under ~/.loom and matches no project target, so a
	// path-based resolver fails closed on it. Kept as the raw composite string
	// for the same reason AddDirs is kept as raw JSON: the store stays a dumb
	// pipe and SessionRow stays comparable.
	Delegation string
}

const cols = "name, claude_session_id, project_label, cwd, model, mode, seed, tags, created_at, ended_at, exit_code, last_status, seed_status, title, add_dirs, delegation"

func (s *Store) Upsert(r SessionRow) error {
	_, err := s.db.Exec(`INSERT INTO sessions (`+cols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			claude_session_id=excluded.claude_session_id,
			project_label=excluded.project_label, cwd=excluded.cwd,
			model=excluded.model, mode=excluded.mode, seed=excluded.seed,
			tags=excluded.tags, created_at=excluded.created_at,
			ended_at=excluded.ended_at, exit_code=excluded.exit_code,
			last_status=excluded.last_status, seed_status=excluded.seed_status,
			title=excluded.title, add_dirs=excluded.add_dirs,
			delegation=excluded.delegation`,
		r.Name, r.ClaudeSessionID, r.ProjectLabel, r.Cwd, r.Model, r.Mode,
		r.Seed, r.Tags, r.CreatedAt, r.EndedAt, r.ExitCode, r.LastStatus, r.SeedStatus,
		r.Title, r.AddDirs, r.Delegation)
	return err
}

// SetSessionDelegation links a session row to the delegation task it is the
// child of, as "<runID>:<taskID>" (migration v13). Separate from Upsert
// because the linkage is written AFTER the launch — Launcher.Launch mints the
// session id itself, so the task↔session binding cannot be part of the row's
// first write, and the delegation spec §13.3 discloses that window rather than
// pretending it closed it.
func (s *Store) SetSessionDelegation(name, delegation string) error {
	_, err := s.db.Exec("UPDATE sessions SET delegation=? WHERE name=?", delegation, name)
	return err
}

func (s *Store) SetStatus(name, status string) error {
	_, err := s.db.Exec("UPDATE sessions SET last_status=? WHERE name=?", status, name)
	return err
}

// SetTitle persists the AI-generated session title (spec: session titles),
// captured by the engine from the transcript's ai-title sidecar record.
func (s *Store) SetTitle(name, title string) error {
	_, err := s.db.Exec("UPDATE sessions SET title=? WHERE name=?", title, name)
	return err
}

// SetSeedStatus records the outcome of an async seed delivery (spec §3.2,
// finding 4): 'sent' once SendLiteral+SendEnter both succeed, 'failed' on
// timeout or a tmux send error. Never silently drops the outcome.
func (s *Store) SetSeedStatus(name, status string) error {
	_, err := s.db.Exec("UPDATE sessions SET seed_status=? WHERE name=?", status, name)
	return err
}

func (s *Store) MarkEnded(name, status string, exitCode, endedAt int64) error {
	_, err := s.db.Exec(
		"UPDATE sessions SET last_status=?, exit_code=?, ended_at=? WHERE name=?",
		status, exitCode, endedAt, name)
	return err
}

func (s *Store) SetClaudeSessionID(name, id string) error {
	_, err := s.db.Exec("UPDATE sessions SET claude_session_id=? WHERE name=?", id, name)
	return err
}

func (s *Store) SetTags(name, tags string) error {
	_, err := s.db.Exec("UPDATE sessions SET tags=? WHERE name=?", tags, name)
	return err
}

// SessionTag is one (claude session id, tags) pair — the only two columns a
// tag-membership question needs.
type SessionTag struct {
	ClaudeSessionID string
	Tags            string
}

// TaggedSessions returns every session row that carries any tag at all and has
// a claude session id, live or finished, in one query.
//
// It replaces the Live() ∪ Recent(CountEnded()) reconstruction the orchestrator
// echo-chamber guard used to do: three round-trips, two of them full-table, both
// materializing every column of every session row in the install to look at one
// field. This is one scan of two columns over the (usually small) tagged subset.
//
// It deliberately returns the RAW tags string rather than filtering by tag here.
// Tags are a free-form field (`orch`, `fan:<group>`, `wf:<id>`) with no schema,
// and membership is a TOKEN test, not a substring test — a `tags LIKE '%orch%'`
// filter would silently swallow a user's own "orchid" or "reorch" tag and drop
// their real sessions out of every brief with nothing anywhere saying why. No
// SQLite index can express a token test over a free-form column, so pushing the
// predicate down would buy nothing and cost correctness. The tokenizer stays in
// the one package that owns the convention, and this stays a narrow read.
func (s *Store) TaggedSessions() ([]SessionTag, error) {
	rows, err := s.db.Query(
		"SELECT claude_session_id, tags FROM sessions WHERE tags != '' AND claude_session_id != ''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionTag
	for rows.Next() {
		var t SessionTag
		if err := rows.Scan(&t.ClaudeSessionID, &t.Tags); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Get(name string) (SessionRow, bool, error) {
	r, err := scanOne(s.db.QueryRow("SELECT "+cols+" FROM sessions WHERE name=?", name))
	if err == sql.ErrNoRows {
		return SessionRow{}, false, nil
	}
	return r, err == nil, err
}

// GetByClaudeSessionID finds the loom row behind a claude conversation id.
// Search results carry only the conversation id and a cwd, so without this
// lookup a search-resume has to synthesise a row — and a synthesised row has
// an empty AddDirs, silently bringing a multi-repo session back single-repo
// (spec §5 names search-resume as one of the three reachable resume paths).
//
// claude_session_id is NOT unique: every Resume writes a NEW sessions row
// carrying the same conversation id, so a conversation resumed twice has three
// rows. The newest wins — it holds the add-dir set Resume last filtered to
// still-existing directories, so a repo that vanished stays vanished instead
// of being resurrected from the original launch row.
//
// The empty id is refused rather than matched: rows acquire their id
// asynchronously from the transcript, so an empty id would match every session
// that has not been correlated yet.
func (s *Store) GetByClaudeSessionID(id string) (SessionRow, bool, error) {
	if id == "" {
		return SessionRow{}, false, nil
	}
	r, err := scanOne(s.db.QueryRow("SELECT "+cols+
		" FROM sessions WHERE claude_session_id=? ORDER BY created_at DESC LIMIT 1", id))
	if err == sql.ErrNoRows {
		return SessionRow{}, false, nil
	}
	return r, err == nil, err
}

const liveSet = "('running','needs_you','idle','unknown')"

// endedSet is the terminal ('done'/'error') status set shared by Recent(),
// EndedNames, DeleteSession, DeleteEnded, and CountEnded.
const endedSet = "('done','error')"

func (s *Store) Live() ([]SessionRow, error) {
	return s.query("SELECT " + cols + " FROM sessions WHERE last_status IN " + liveSet +
		" ORDER BY created_at DESC")
}

func (s *Store) Recent(limit int) ([]SessionRow, error) {
	return s.query(fmt.Sprintf("SELECT "+cols+" FROM sessions WHERE last_status IN "+endedSet+
		" ORDER BY ended_at DESC LIMIT %d", limit))
}

// DeleteSession removes a single finished row. The status guard makes deleting
// a live row (or an unknown name) a no-op, so a live session can never be
// removed via this path.
func (s *Store) DeleteSession(name string) error {
	_, err := s.db.Exec(
		"DELETE FROM sessions WHERE name = ? AND last_status IN "+endedSet, name)
	return err
}

// DeleteEnded removes every finished row and returns the number deleted.
func (s *Store) DeleteEnded() (int64, error) {
	res, err := s.db.Exec("DELETE FROM sessions WHERE last_status IN " + endedSet)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountEnded reports how many finished rows exist (drives the bulk-clear
// confirm; the snapshot's Recent list is capped at 10 and would undercount).
func (s *Store) CountEnded() (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT count(*) FROM sessions WHERE last_status IN " + endedSet).Scan(&n)
	return n, err
}

// EndedNames returns the session names of all finished rows (used to reap any
// lingering tmux panes before a bulk clear).
func (s *Store) EndedNames() ([]string, error) {
	rows, err := s.db.Query("SELECT name FROM sessions WHERE last_status IN " + endedSet)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// MarkLiveOrphansEnded retires live rows with no tmux backing to history as
// 'done' (exit unknown). NEVER deletes — history survives restarts (spec §6).
//
// graceUnix protects rows created after that cutoff: a session just launched
// (Launch/Resume writes its store row, THEN creates the tmux session — or the
// reverse, depending on the caller) can otherwise be observed by a poll that
// races the tmux session's own creation and get retired as an orphan before
// it's ever seen alive. Callers should pass something like now-5s so only
// rows old enough to have had a fair chance to appear in tmux are eligible.
func (s *Store) MarkLiveOrphansEnded(liveTmuxNames []string, graceUnix, endedAt int64) error {
	placeholders := make([]string, len(liveTmuxNames))
	args := []any{endedAt, graceUnix}
	for i, n := range liveTmuxNames {
		placeholders[i] = "?"
		args = append(args, n)
	}
	q := "UPDATE sessions SET last_status='done', exit_code=-1, ended_at=? WHERE last_status IN " + liveSet +
		" AND created_at < ?"
	if len(liveTmuxNames) > 0 {
		q += " AND name NOT IN (" + strings.Join(placeholders, ",") + ")"
	}
	_, err := s.db.Exec(q, args...)
	return err
}

func (s *Store) query(q string, args ...any) ([]SessionRow, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(&r.Name, &r.ClaudeSessionID, &r.ProjectLabel, &r.Cwd,
			&r.Model, &r.Mode, &r.Seed, &r.Tags, &r.CreatedAt, &r.EndedAt,
			&r.ExitCode, &r.LastStatus, &r.SeedStatus, &r.Title, &r.AddDirs,
			&r.Delegation); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanOne(row rowScanner) (SessionRow, error) {
	var r SessionRow
	err := row.Scan(&r.Name, &r.ClaudeSessionID, &r.ProjectLabel, &r.Cwd,
		&r.Model, &r.Mode, &r.Seed, &r.Tags, &r.CreatedAt, &r.EndedAt,
		&r.ExitCode, &r.LastStatus, &r.SeedStatus, &r.Title, &r.AddDirs,
		&r.Delegation)
	return r, err
}
