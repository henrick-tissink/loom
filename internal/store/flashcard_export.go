package store

// ExportCards returns a project's ACTIVE deck (drafts and suspended cards
// excluded), ordered by part then id — the deterministic set an export renders.
func (s *Store) ExportCards(project string) ([]Flashcard, error) {
	rows, err := s.db.Query("SELECT "+flashcardCols+" FROM flashcards WHERE project=? AND status='active' ORDER BY part, id", project)
	if err != nil {
		return nil, err
	}
	return scanCards(rows)
}
