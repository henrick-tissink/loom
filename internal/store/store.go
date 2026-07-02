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
	}
	for i := v; i < len(migrations); i++ {
		if _, err := s.db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return err
		}
	}
	return nil
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
}

const cols = "name, claude_session_id, project_label, cwd, model, mode, seed, tags, created_at, ended_at, exit_code, last_status, seed_status"

func (s *Store) Upsert(r SessionRow) error {
	_, err := s.db.Exec(`INSERT INTO sessions (`+cols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			claude_session_id=excluded.claude_session_id,
			project_label=excluded.project_label, cwd=excluded.cwd,
			model=excluded.model, mode=excluded.mode, seed=excluded.seed,
			tags=excluded.tags, created_at=excluded.created_at,
			ended_at=excluded.ended_at, exit_code=excluded.exit_code,
			last_status=excluded.last_status, seed_status=excluded.seed_status`,
		r.Name, r.ClaudeSessionID, r.ProjectLabel, r.Cwd, r.Model, r.Mode,
		r.Seed, r.Tags, r.CreatedAt, r.EndedAt, r.ExitCode, r.LastStatus, r.SeedStatus)
	return err
}

func (s *Store) SetStatus(name, status string) error {
	_, err := s.db.Exec("UPDATE sessions SET last_status=? WHERE name=?", status, name)
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

func (s *Store) Live() ([]SessionRow, error) {
	return s.query("SELECT " + cols + " FROM sessions WHERE last_status IN " + liveSet +
		" ORDER BY created_at DESC")
}

func (s *Store) Recent(limit int) ([]SessionRow, error) {
	return s.query(fmt.Sprintf("SELECT "+cols+" FROM sessions WHERE last_status IN ('done','error')"+
		" ORDER BY ended_at DESC LIMIT %d", limit))
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
			&r.ExitCode, &r.LastStatus, &r.SeedStatus); err != nil {
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
		&r.ExitCode, &r.LastStatus, &r.SeedStatus)
	return r, err
}
