package store

// ReviewRow is one row of flashcard_reviews: a card's current SM-2 state.
type ReviewRow struct {
	CardID                     int64
	Ease                       float64
	Interval                   int
	DueAt                      int64
	Reps, Lapses, LastGrade    int
	LastReviewed, IntroducedAt int64
}

const reviewCols = "card_id, ease, interval, due_at, reps, lapses, last_grade, last_reviewed, introduced_at"

func (s *Store) GetReview(cardID int64) (ReviewRow, bool, error) {
	var r ReviewRow
	err := s.db.QueryRow("SELECT "+reviewCols+" FROM flashcard_reviews WHERE card_id=?", cardID).Scan(
		&r.CardID, &r.Ease, &r.Interval, &r.DueAt, &r.Reps, &r.Lapses, &r.LastGrade, &r.LastReviewed, &r.IntroducedAt)
	if err == errNoRows {
		return ReviewRow{}, false, nil
	}
	if err != nil {
		return ReviewRow{}, false, err
	}
	return r, true, nil
}

// PutReview upserts a card's SM-2 state by card_id.
func (s *Store) PutReview(r ReviewRow) error {
	_, err := s.db.Exec(`INSERT INTO flashcard_reviews
		(card_id, ease, interval, due_at, reps, lapses, last_grade, last_reviewed, introduced_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(card_id) DO UPDATE SET
			ease=excluded.ease, interval=excluded.interval, due_at=excluded.due_at,
			reps=excluded.reps, lapses=excluded.lapses, last_grade=excluded.last_grade,
			last_reviewed=excluded.last_reviewed, introduced_at=excluded.introduced_at`,
		r.CardID, r.Ease, r.Interval, r.DueAt, r.Reps, r.Lapses, r.LastGrade, r.LastReviewed, r.IntroducedAt)
	return err
}

// AppendReviewLog records one graded review event (append-only history).
func (s *Store) AppendReviewLog(cardID int64, grade int, wasDue bool, at int64) error {
	due := 0
	if wasDue {
		due = 1
	}
	_, err := s.db.Exec("INSERT INTO flashcard_review_log (card_id, grade, was_due, reviewed_at) VALUES (?,?,?,?)",
		cardID, grade, due, at)
	return err
}

// IntroducedSince counts cards in a project first reviewed at or after sinceTs.
func (s *Store) IntroducedSince(project string, sinceTs int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM flashcard_reviews r
		JOIN flashcards c ON c.id = r.card_id
		WHERE c.project=? AND r.introduced_at >= ?`, project, sinceTs).Scan(&n)
	return n, err
}

// DueReviewCards returns active cards whose review is due at or before now.
func (s *Store) DueReviewCards(project string, now int64, limit int) ([]Flashcard, error) {
	rows, err := s.db.Query(`SELECT `+prefixed(flashcardCols, "c.")+` FROM flashcards c
		JOIN flashcard_reviews r ON r.card_id = c.id
		WHERE c.project=? AND c.status='active' AND r.due_at <= ?
		ORDER BY r.due_at LIMIT ?`, project, now, limit)
	if err != nil {
		return nil, err
	}
	return scanCards(rows)
}

// NewActiveCards returns active cards that have never been reviewed.
func (s *Store) NewActiveCards(project string, limit int) ([]Flashcard, error) {
	rows, err := s.db.Query(`SELECT `+prefixed(flashcardCols, "c.")+` FROM flashcards c
		LEFT JOIN flashcard_reviews r ON r.card_id = c.id
		WHERE c.project=? AND c.status='active' AND r.card_id IS NULL
		ORDER BY c.id LIMIT ?`, project, limit)
	if err != nil {
		return nil, err
	}
	return scanCards(rows)
}
