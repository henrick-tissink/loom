package store

import (
	"database/sql"
	"strings"
)

// Flashcard is one row of the flashcards table (2026-07-25 flashcards slice 1).
// (Anchor, StemHash) is the stable identity: the natural source location plus a
// normalized question stem, never the card text — so regeneration re-links to
// the same row (spec §7/§8).
type Flashcard struct {
	ID                                        int64
	Project, Part, Anchor, StemHash           string
	Type, Front, Back                         string
	SourceRef, SourceHash, AnswerHash, Status string
	CreatedAt, CuratedAt                      int64
}

const flashcardCols = "id, project, part, anchor, stem_hash, type, front, back, source_ref, source_hash, answer_hash, status, created_at, curated_at"

// InsertFlashcard inserts one card. On an (anchor, stem_hash) conflict it is a
// no-op (inserted=false): the same fact re-generated is the same card, and its
// review progress must survive (spec §8).
func (s *Store) InsertFlashcard(c Flashcard) (id int64, inserted bool, err error) {
	res, err := s.db.Exec(`INSERT INTO flashcards
		(project, part, anchor, stem_hash, type, front, back, source_ref, source_hash, answer_hash, status, created_at, curated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(anchor, stem_hash) DO NOTHING`,
		c.Project, c.Part, c.Anchor, c.StemHash, c.Type, c.Front, c.Back,
		c.SourceRef, c.SourceHash, c.AnswerHash, c.Status, c.CreatedAt, c.CuratedAt)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, false, nil
	}
	id, _ = res.LastInsertId()
	return id, true, nil
}

// FlashcardsForProject returns every card for a project, oldest id first.
func (s *Store) FlashcardsForProject(project string) ([]Flashcard, error) {
	rows, err := s.db.Query("SELECT "+flashcardCols+" FROM flashcards WHERE project=? ORDER BY id", project)
	if err != nil {
		return nil, err
	}
	return scanCards(rows)
}

// errNoRows aliases sql.ErrNoRows so callers in this package need not import database/sql.
var errNoRows = sql.ErrNoRows

// prefixed rewrites a comma-separated column list with a table alias prefix,
// e.g. prefixed("id, project", "c.") == "c.id, c.project".
func prefixed(cols, prefix string) string {
	parts := strings.Split(cols, ", ")
	for i, p := range parts {
		parts[i] = prefix + p
	}
	return strings.Join(parts, ", ")
}

// scanCards scans a rows cursor selecting flashcardCols (in order) into Flashcards.
func scanCards(rows *sql.Rows) ([]Flashcard, error) {
	defer rows.Close()
	var out []Flashcard
	for rows.Next() {
		var c Flashcard
		if err := rows.Scan(&c.ID, &c.Project, &c.Part, &c.Anchor, &c.StemHash, &c.Type,
			&c.Front, &c.Back, &c.SourceRef, &c.SourceHash, &c.AnswerHash, &c.Status,
			&c.CreatedAt, &c.CuratedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
