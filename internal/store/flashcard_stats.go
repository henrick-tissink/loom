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

// PassRate counts DUE reviews for a project since sinceTs; passed = grade >= 3.
// Only was_due reviews count (spec §9: a rolling pass-rate on *due* reviews) — a
// card's first-exposure learning grade (was_due=0) is not a retention signal and
// is excluded, so a freshly-graded deck can't read as "100% retained".
func (s *Store) PassRate(project string, sinceTs int64) (passed, total int, err error) {
	err = s.db.QueryRow(`SELECT
			COALESCE(SUM(CASE WHEN l.grade >= 3 THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM flashcard_review_log l
		JOIN flashcards c ON c.id = l.card_id
		WHERE c.project=? AND l.reviewed_at >= ? AND l.was_due = 1`, project, sinceTs).Scan(&passed, &total)
	return passed, total, err
}

// StrugglingCard is an active card racking up lapses — heading for auto-suspend.
type StrugglingCard struct {
	ID     int64  `json:"id"`
	Part   string `json:"part"`
	Front  string `json:"front"`
	Lapses int    `json:"lapses"`
}

// LapsingCards returns active cards with at least minLapses lapses (worst first),
// so a card the scheduler is about to suspend can be fixed or killed in time.
func (s *Store) LapsingCards(project string, minLapses int) ([]StrugglingCard, error) {
	rows, err := s.db.Query(`SELECT c.id, c.part, c.front, r.lapses
		FROM flashcards c JOIN flashcard_reviews r ON r.card_id = c.id
		WHERE c.project=? AND c.status='active' AND r.lapses >= ?
		ORDER BY r.lapses DESC, c.id LIMIT 50`, project, minLapses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StrugglingCard
	for rows.Next() {
		var sc StrugglingCard
		if err := rows.Scan(&sc.ID, &sc.Part, &sc.Front, &sc.Lapses); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// DayStat is one day's due-review tally for the recall trend sparkline.
type DayStat struct {
	Day    int64 `json:"day"`    // unix-day index (reviewed_at / 86400)
	Passed int   `json:"passed"` // grade >= 3, matching PassRate
	Total  int   `json:"total"`
}

// DailyReviewStats returns per-day due-review pass tallies since sinceTs.
func (s *Store) DailyReviewStats(project string, sinceTs int64) ([]DayStat, error) {
	rows, err := s.db.Query(`SELECT l.reviewed_at/86400 AS d,
			COALESCE(SUM(CASE WHEN l.grade >= 3 THEN 1 ELSE 0 END),0), COUNT(*)
		FROM flashcard_review_log l JOIN flashcards c ON c.id = l.card_id
		WHERE c.project=? AND l.was_due=1 AND l.reviewed_at >= ?
		GROUP BY d ORDER BY d`, project, sinceTs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayStat
	for rows.Next() {
		var d DayStat
		if err := rows.Scan(&d.Day, &d.Passed, &d.Total); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GradeCounts tallies grades 1..4 over a project's reviews since sinceTs — the
// shape of a deck (all-Easy vs mostly-Hard), which a pass-rate number hides.
func (s *Store) GradeCounts(project string, sinceTs int64) ([4]int, error) {
	var out [4]int
	rows, err := s.db.Query(`SELECT l.grade, COUNT(*)
		FROM flashcard_review_log l JOIN flashcards c ON c.id = l.card_id
		WHERE c.project=? AND l.reviewed_at >= ? GROUP BY l.grade`, project, sinceTs)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var g, n int
		if err := rows.Scan(&g, &n); err != nil {
			return out, err
		}
		if g >= 1 && g <= 4 {
			out[g-1] = n
		}
	}
	return out, rows.Err()
}
