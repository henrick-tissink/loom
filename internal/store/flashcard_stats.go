package store

// PartStat is per-manifest-part card counts for the coverage view.
type PartStat struct {
	Part                      string
	Total, Active, Draft, Due int
}

// PartStats returns per-part counts for a project, ordered by Part.
func (s *Store) PartStats(project string, now int64) ([]PartStat, error) {
	rows, err := s.db.Query(`SELECT c.part,
			COUNT(*),
			SUM(CASE WHEN c.status='active' THEN 1 ELSE 0 END),
			SUM(CASE WHEN c.status='draft'  THEN 1 ELSE 0 END),
			SUM(CASE WHEN c.status='active' AND r.due_at IS NOT NULL AND r.due_at <= ? THEN 1 ELSE 0 END)
		FROM flashcards c
		LEFT JOIN flashcard_reviews r ON r.card_id = c.id
		WHERE c.project=?
		GROUP BY c.part ORDER BY c.part`, now, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PartStat
	for rows.Next() {
		var p PartStat
		if err := rows.Scan(&p.Part, &p.Total, &p.Active, &p.Draft, &p.Due); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PassRate counts graded reviews for a project since sinceTs; passed = grade >= 3.
func (s *Store) PassRate(project string, sinceTs int64) (passed, total int, err error) {
	err = s.db.QueryRow(`SELECT
			COALESCE(SUM(CASE WHEN l.grade >= 3 THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM flashcard_review_log l
		JOIN flashcards c ON c.id = l.card_id
		WHERE c.project=? AND l.reviewed_at >= ?`, project, sinceTs).Scan(&passed, &total)
	return passed, total, err
}
