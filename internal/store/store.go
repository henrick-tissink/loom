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
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
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
	}
	for i := v; i < len(migrations); i++ {
		if err := s.applyMigration(i+1, migrations[i]); err != nil {
			return err
		}
	}
	return nil
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
		return fmt.Errorf("migration %d: %w", version, err)
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
}

const cols = "name, claude_session_id, project_label, cwd, model, mode, seed, tags, created_at, ended_at, exit_code, last_status, seed_status, title"

func (s *Store) Upsert(r SessionRow) error {
	_, err := s.db.Exec(`INSERT INTO sessions (`+cols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			claude_session_id=excluded.claude_session_id,
			project_label=excluded.project_label, cwd=excluded.cwd,
			model=excluded.model, mode=excluded.mode, seed=excluded.seed,
			tags=excluded.tags, created_at=excluded.created_at,
			ended_at=excluded.ended_at, exit_code=excluded.exit_code,
			last_status=excluded.last_status, seed_status=excluded.seed_status,
			title=excluded.title`,
		r.Name, r.ClaudeSessionID, r.ProjectLabel, r.Cwd, r.Model, r.Mode,
		r.Seed, r.Tags, r.CreatedAt, r.EndedAt, r.ExitCode, r.LastStatus, r.SeedStatus, r.Title)
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

func (s *Store) Get(name string) (SessionRow, bool, error) {
	r, err := scanOne(s.db.QueryRow("SELECT "+cols+" FROM sessions WHERE name=?", name))
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
			&r.ExitCode, &r.LastStatus, &r.SeedStatus, &r.Title); err != nil {
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
		&r.ExitCode, &r.LastStatus, &r.SeedStatus, &r.Title)
	return r, err
}
