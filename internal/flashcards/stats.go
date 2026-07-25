package flashcards

// Coverage is per-part progress for the coverage map (spec §9): counts only —
// there is deliberately no "mastery %".
type Coverage struct {
	Part                      string
	Total, Active, Draft, Due int
}

// Coverage returns per-part card counts for a project.
func (rv *Reviewer) Coverage(project string, now int64) ([]Coverage, error) {
	stats, err := rv.Store.PartStats(project, now)
	if err != nil {
		return nil, err
	}
	out := make([]Coverage, len(stats))
	for i, s := range stats {
		out[i] = Coverage{Part: s.Part, Total: s.Total, Active: s.Active, Draft: s.Draft, Due: s.Due}
	}
	return out, nil
}

// PassRate is the MEASURED fraction of graded reviews (since sinceTs) that were
// recalled (grade >= Good) — an honest retention signal, not card existence.
// Returns 0 when there are no reviews yet.
func (rv *Reviewer) PassRate(project string, sinceTs int64) (rate float64, n int, err error) {
	passed, total, err := rv.Store.PassRate(project, sinceTs)
	if err != nil || total == 0 {
		return 0, total, err
	}
	return float64(passed) / float64(total), total, nil
}
