// Memory store (Phase 2, spec docs/superpowers/specs/2026-07-03-memory-design.md
// §3/§4): the searchable archive of every claude transcript. Three tables
// (migration v4, store.go): `transcripts` (one row per session — the L2
// distillation), `indexed_files` (per-source-file fingerprint + the
// contiguous FTS rowid range it owns, enabling indexed rowid-range deletes
// instead of a full FTS scan on re-index), and `messages_fts` (the FTS5
// virtual table of indexed doc text).
package store

import (
	"database/sql"
	"strings"
)

// Transcript is one row of the `transcripts` table: the L2 distillation of a
// claude session (auto fields) plus the on-demand LLM summary.
type Transcript struct {
	SessionID, ProjectDir, Cwd, Title, Ask, Outcome, Files, LLMSummary string
	FirstTS, LastTS, MsgCount, SummaryAt                               int64
	FileMissing                                                        bool
}

// IndexedFile is one row of the `indexed_files` table: a source transcript
// file's fingerprint (size, mtime) and the contiguous messages_fts rowid
// range its docs occupy. FirstRowid/LastRowid are 0 when the file has no
// docs (e.g. a file that only produced a doc count of zero after filtering).
type IndexedFile struct {
	Path, SessionID                    string
	Size, Mtime, FirstRowid, LastRowid int64
}

// Doc is one row of the `messages_fts` table, as read back out (for the
// Plan B summarizer's input budget).
type Doc struct {
	Content, Role string
	TS            int64
}

// SearchHit is one row of a search result: the best-ranked matching doc for
// a session, joined with that session's transcript fields.
type SearchHit struct {
	SessionID, Snippet, Title, ProjectDir, Cwd, Ask string
	LastTS                                          int64
}

const transcriptCols = "session_id, project_dir, cwd, title, first_ts, last_ts, msg_count, ask, outcome, files, file_missing, llm_summary, summary_at"

// UpsertTranscript inserts or fully replaces a session's distillation row.
// The llm_summary and summary_at fields are set only via SetLLMSummary;
// on conflict, we do not overwrite them (SetLLMSummary is the sole writer).
func (s *Store) UpsertTranscript(t Transcript) error {
	_, err := s.db.Exec(`INSERT INTO transcripts (`+transcriptCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET
			project_dir=excluded.project_dir, cwd=excluded.cwd, title=excluded.title,
			first_ts=excluded.first_ts, last_ts=excluded.last_ts, msg_count=excluded.msg_count,
			ask=excluded.ask, outcome=excluded.outcome, files=excluded.files,
			file_missing=excluded.file_missing`,
		t.SessionID, t.ProjectDir, t.Cwd, t.Title, t.FirstTS, t.LastTS, t.MsgCount,
		t.Ask, t.Outcome, t.Files, t.FileMissing, t.LLMSummary, t.SummaryAt)
	return err
}

func (s *Store) GetTranscript(sessionID string) (Transcript, bool, error) {
	var t Transcript
	err := s.db.QueryRow("SELECT "+transcriptCols+" FROM transcripts WHERE session_id=?", sessionID).Scan(
		&t.SessionID, &t.ProjectDir, &t.Cwd, &t.Title, &t.FirstTS, &t.LastTS, &t.MsgCount,
		&t.Ask, &t.Outcome, &t.Files, &t.FileMissing, &t.LLMSummary, &t.SummaryAt)
	if err == sql.ErrNoRows {
		return Transcript{}, false, nil
	}
	if err != nil {
		return Transcript{}, false, err
	}
	return t, true, nil
}

func (s *Store) SetLLMSummary(sessionID, summary string, at int64) error {
	_, err := s.db.Exec("UPDATE transcripts SET llm_summary=?, summary_at=? WHERE session_id=?", summary, at, sessionID)
	return err
}

func (s *Store) SetFileMissing(sessionID string, missing bool) error {
	_, err := s.db.Exec("UPDATE transcripts SET file_missing=? WHERE session_id=?", missing, sessionID)
	return err
}

func (s *Store) TranscriptCount() (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM transcripts").Scan(&n)
	return n, err
}

// ListTranscripts returns every session's distillation row. The indexer's
// file_missing sweep needs this (rather than just the sessions touched in
// the current pass): a project dir's main file can vanish entirely between
// sweeps, in which case it won't appear in any directory listing at all, so
// every KNOWN session must be checked against the filesystem, not just the
// ones seen this pass.
func (s *Store) ListTranscripts() ([]Transcript, error) {
	rows, err := s.db.Query("SELECT " + transcriptCols + " FROM transcripts")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transcript
	for rows.Next() {
		var t Transcript
		if err := rows.Scan(&t.SessionID, &t.ProjectDir, &t.Cwd, &t.Title, &t.FirstTS, &t.LastTS, &t.MsgCount,
			&t.Ask, &t.Outcome, &t.Files, &t.FileMissing, &t.LLMSummary, &t.SummaryAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetIndexedFile(path string) (IndexedFile, bool, error) {
	f := IndexedFile{Path: path}
	err := s.db.QueryRow("SELECT session_id, size, mtime, first_rowid, last_rowid FROM indexed_files WHERE path=?", path).
		Scan(&f.SessionID, &f.Size, &f.Mtime, &f.FirstRowid, &f.LastRowid)
	if err == sql.ErrNoRows {
		return IndexedFile{}, false, nil
	}
	if err != nil {
		return IndexedFile{}, false, err
	}
	return f, true, nil
}

// ReplaceFileDocs re-indexes one source file's docs in a single transaction:
// delete the file's OLD rowid range from messages_fts (per f.FirstRowid/
// LastRowid — the fingerprint the CALLER read via GetIndexedFile before
// re-parsing; skipped when FirstRowid==0, i.e. no prior docs), insert the
// new docs, then upsert the indexed_files fingerprint with f's new size/
// mtime and the NEW contiguous rowid range captured from the batch insert.
//
// Rowid-range deletes are safe (never touch another file's docs) because
// each file's docs are inserted in one tx on the single connection
// (Store.Open sets SetMaxOpenConns(1)) — rowids are contiguous per file.
func (s *Store) ReplaceFileDocs(f IndexedFile, docs []Doc) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if f.FirstRowid > 0 {
		if _, err = tx.Exec("DELETE FROM messages_fts WHERE rowid BETWEEN ? AND ?", f.FirstRowid, f.LastRowid); err != nil {
			return err
		}
	}

	var firstRowid, lastRowid int64
	for _, d := range docs {
		var res sql.Result
		res, err = tx.Exec("INSERT INTO messages_fts (content, session_id, role, ts) VALUES (?,?,?,?)",
			d.Content, f.SessionID, d.Role, d.TS)
		if err != nil {
			return err
		}
		var id int64
		id, err = res.LastInsertId()
		if err != nil {
			return err
		}
		if firstRowid == 0 {
			firstRowid = id
		}
		lastRowid = id
	}

	_, err = tx.Exec(`INSERT INTO indexed_files (path, session_id, size, mtime, first_rowid, last_rowid)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			session_id=excluded.session_id, size=excluded.size, mtime=excluded.mtime,
			first_rowid=excluded.first_rowid, last_rowid=excluded.last_rowid`,
		f.Path, f.SessionID, f.Size, f.Mtime, firstRowid, lastRowid)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteFileDocs removes a source file's docs entirely (indexed rowid-range
// delete, no full FTS scan) and drops its indexed_files fingerprint row. A
// no-op (nil error) when the path isn't indexed.
func (s *Store) DeleteFileDocs(path string) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	var firstRowid, lastRowid int64
	err = tx.QueryRow("SELECT first_rowid, last_rowid FROM indexed_files WHERE path=?", path).Scan(&firstRowid, &lastRowid)
	if err == sql.ErrNoRows {
		tx.Rollback()
		return nil // nothing indexed for this path; nothing to do
	}
	if err != nil {
		return err
	}

	if firstRowid > 0 {
		if _, err = tx.Exec("DELETE FROM messages_fts WHERE rowid BETWEEN ? AND ?", firstRowid, lastRowid); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM indexed_files WHERE path=?", path); err != nil {
		return err
	}
	return tx.Commit()
}

// SessionDocs returns all indexed docs for a session (Plan B summarizer
// input), using a two-step public API approach that avoids touching fts5's
// undocumented internal shadow table. The first query reads rowid ranges
// per file via indexed_files (an ordinary table), then the second queries
// the ranges from messages_fts (public FTS5 virtual table API). This two-step
// approach avoids the multi-range join full-scan problem (~997ms → ~355µs
// measured) that occurs when joining directly against messages_fts's range
// conditions.
func (s *Store) SessionDocs(sessionID string) ([]Doc, error) {
	// Step 1: Get rowid ranges for all files in this session
	rows, err := s.db.Query(`SELECT first_rowid, last_rowid FROM indexed_files
		WHERE session_id = ? ORDER BY first_rowid`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type rowidRange struct {
		first, last int64
	}
	var ranges []rowidRange
	for rows.Next() {
		var r rowidRange
		if err := rows.Scan(&r.first, &r.last); err != nil {
			return nil, err
		}
		// Skip ranges where first_rowid==0 (file has no docs)
		if r.first > 0 {
			ranges = append(ranges, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Step 2: For each range, fetch docs in rowid order
	var out []Doc
	for _, r := range ranges {
		docs, err := s.db.Query(`SELECT content, role, ts FROM messages_fts
			WHERE rowid BETWEEN ? AND ? ORDER BY rowid`, r.first, r.last)
		if err != nil {
			return nil, err
		}
		defer docs.Close()
		for docs.Next() {
			var d Doc
			if err := docs.Scan(&d.Content, &d.Role, &d.TS); err != nil {
				return nil, err
			}
			out = append(out, d)
		}
		if err := docs.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// searchSQL is the red-team-verified shape (spec §4). The naive
// `GROUP BY + snippet()/bm25()` is rejected by SQLite ("unable to use
// function … in the requested context") — including via flattened
// subqueries. MATERIALIZED is load-bearing: it forces the CTE to be
// computed once as a temp table before the outer GROUP BY, which is what
// makes snippet()/rank legal here. The bare `h.snip` column selected
// alongside `min(h.r)` rides SQLite's documented min()/max() argmin
// guarantee: when a query has exactly one bare min()/max() aggregate, every
// other bare column is taken from the same row that produced that min/max.
const searchSQL = `
WITH hits AS MATERIALIZED (
  SELECT session_id, snippet(messages_fts, 0, char(1), char(2), '…', 12) AS snip, rank AS r
  FROM messages_fts WHERE messages_fts MATCH ?
)
SELECT h.session_id, h.snip, min(h.r) AS best, t.title, t.project_dir, t.cwd, t.last_ts, t.ask
FROM hits h JOIN transcripts t ON t.session_id = h.session_id
GROUP BY h.session_id ORDER BY best LIMIT ?`

// SearchSessions runs a sanitized FTS5 query and returns the single
// best-ranked hit per session, best session first. Malformed/empty input
// NEVER surfaces as an error (spec §4, second defense: the sanitizer already
// quotes every term, but any residual FTS5 syntax error — or scan error —
// is swallowed here and reported as zero results instead).
func (s *Store) SearchSessions(rawQuery string, limit int) ([]SearchHit, error) {
	q := sanitizeFTSQuery(rawQuery)
	if q == "" {
		return nil, nil
	}
	rows, err := s.db.Query(searchSQL, q, limit)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var best float64
		if err := rows.Scan(&h.SessionID, &h.Snippet, &best, &h.Title, &h.ProjectDir, &h.Cwd, &h.LastTS, &h.Ask); err != nil {
			return nil, nil
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, nil
	}
	return out, nil
}

// sanitizeFTSQuery turns free-text user input into a safe FTS5 MATCH query
// (spec §4): split on whitespace, quote each term (doubling any embedded
// quote), and append a trailing `*` after the LAST term's closing quote only
// (prefix-match the final, possibly-still-being-typed term). Quoting every
// term as an FTS5 string literal neutralizes syntax like `-`, `(foo)`, and
// keywords like NEAR that would otherwise be parsed as query operators.
func sanitizeFTSQuery(raw string) string {
	terms := strings.Fields(raw)
	if len(terms) == 0 {
		return ""
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " ") + "*"
}

// RecentTranscriptsByProjectDir returns a project's most recent sessions,
// newest first (spec §6): the recall panel's default (no seed typed yet)
// and the recency fallback used by internal/memory.Related when the recall
// query builder can't produce a usable FTS expression, or produces one but
// zero hits qualify. Backed by idx_transcripts_project (migration v6).
func (s *Store) RecentTranscriptsByProjectDir(projectDir string, limit int) ([]Transcript, error) {
	rows, err := s.db.Query(
		"SELECT "+transcriptCols+" FROM transcripts WHERE project_dir=? ORDER BY last_ts DESC LIMIT ?",
		projectDir, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transcript
	for rows.Next() {
		var t Transcript
		if err := rows.Scan(&t.SessionID, &t.ProjectDir, &t.Cwd, &t.Title, &t.FirstTS, &t.LastTS, &t.MsgCount,
			&t.Ask, &t.Outcome, &t.Files, &t.FileMissing, &t.LLMSummary, &t.SummaryAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SearchSessionsRaw is SearchSessions' sibling for recall (spec §2):
// internal/memory.Related builds its OWN FTS5 match expression
// (buildRecallQuery) rather than going through sanitizeFTSQuery, whose
// implicit-AND + trailing-`*` shape returns ZERO sessions for
// natural-sentence seeds (verified on the real index) — recall owns its
// query text and hands over an already-built expression. Reuses the exact
// same verified MATERIALIZED-CTE shape (searchSQL) as SearchSessions,
// un-sanitized, with a caller-supplied limit (recall fetches a wider set
// than it displays, spec §6, so the same-project boost can promote hits
// ranked lower by raw bm25). Matched-term counts aren't available at the
// SQL level (rank/bm25 don't expose a per-term match count) — the caller
// computes those client-side from the returned Snippet/Title/Ask.
//
// SearchSessions itself is untouched; this is new, separate code that
// happens to reuse the same searchSQL constant and scan shape.
func (s *Store) SearchSessionsRaw(matchExpr string, limit int) ([]SearchHit, error) {
	if matchExpr == "" {
		return nil, nil
	}
	rows, err := s.db.Query(searchSQL, matchExpr, limit)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var best float64
		if err := rows.Scan(&h.SessionID, &h.Snippet, &best, &h.Title, &h.ProjectDir, &h.Cwd, &h.LastTS, &h.Ask); err != nil {
			return nil, nil
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, nil
	}
	return out, nil
}

// GetLatestByClaudeSessionID returns the sessions-table row (live or
// terminal) with the given claude_session_id and the highest created_at —
// used for resume-collision detection (spec §6): several store rows can
// share a claude_session_id across resumes.
func (s *Store) GetLatestByClaudeSessionID(id string) (SessionRow, bool, error) {
	r, err := scanOne(s.db.QueryRow(
		"SELECT "+cols+" FROM sessions WHERE claude_session_id=? ORDER BY created_at DESC LIMIT 1", id))
	if err == sql.ErrNoRows {
		return SessionRow{}, false, nil
	}
	if err != nil {
		return SessionRow{}, false, err
	}
	return r, true, nil
}
