package store

// DraftsForProject returns a project's uncurated cards, oldest id first.
func (s *Store) DraftsForProject(project string) ([]Flashcard, error) {
	rows, err := s.db.Query("SELECT "+flashcardCols+" FROM flashcards WHERE project=? AND status='draft' ORDER BY id", project)
	if err != nil {
		return nil, err
	}
	return scanCards(rows)
}

// SetCardStatus sets a card's status; activating also stamps curated_at.
func (s *Store) SetCardStatus(id int64, status string, at int64) error {
	if status == "active" {
		_, err := s.db.Exec("UPDATE flashcards SET status=?, curated_at=? WHERE id=?", status, at, id)
		return err
	}
	_, err := s.db.Exec("UPDATE flashcards SET status=? WHERE id=?", status, id)
	return err
}

// EditCardText rewrites a card's text and its recomputed hashes (the caller
// computes stemHash/answerHash — the store never imports the flashcards pkg).
func (s *Store) EditCardText(id int64, front, back, stemHash, answerHash string, at int64) error {
	_, err := s.db.Exec("UPDATE flashcards SET front=?, back=?, stem_hash=?, answer_hash=?, curated_at=? WHERE id=?",
		front, back, stemHash, answerHash, at, id)
	return err
}

// DeleteCard removes a card and its review state and log rows atomically (kill).
func (s *Store) DeleteCard(id int64) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	// Delete order (log → reviews → card) is a defensive convention: this schema
	// declares no FOREIGN KEY constraints, so the order is not required for
	// correctness, but keeping children-before-parent avoids surprises if FKs are
	// ever added.
	for _, q := range []string{
		"DELETE FROM flashcard_review_log WHERE card_id=?",
		"DELETE FROM flashcard_reviews WHERE card_id=?",
		"DELETE FROM flashcards WHERE id=?",
	} {
		if _, err = tx.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}
