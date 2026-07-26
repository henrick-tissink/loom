package store

// DeleteCardsForPart removes every card of one project part — and its review
// state and log rows — in a single transaction (replace semantics: a regenerate
// wipes the part before re-authoring). Children are deleted before the parent,
// the same order DeleteCard uses. Returns the number of cards deleted.
func (s *Store) DeleteCardsForPart(project, part string) (deleted int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	sub := "SELECT id FROM flashcards WHERE project=? AND part=?"
	for _, q := range []string{
		"DELETE FROM flashcard_review_log WHERE card_id IN (" + sub + ")",
		"DELETE FROM flashcard_reviews WHERE card_id IN (" + sub + ")",
	} {
		if _, err = tx.Exec(q, project, part); err != nil {
			return 0, err
		}
	}
	res, e := tx.Exec("DELETE FROM flashcards WHERE project=? AND part=?", project, part)
	if e != nil {
		err = e
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// StalePartsForProject returns the distinct parts that carry at least one stale
// card, ordered by part — the set a "regenerate all stale" action iterates.
func (s *Store) StalePartsForProject(project string) ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT part FROM flashcards WHERE project=? AND status='stale' ORDER BY part", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
