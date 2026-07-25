package flashcards

import "github.com/henricktissink/loom/internal/store"

// ReviewConfig bounds a review session (spec §9): a daily cap on newly
// introduced cards, and a lapse count past which a card is auto-suspended.
type ReviewConfig struct {
	NewPerDay      int
	LeechThreshold int
}

func DefaultReviewConfig() ReviewConfig { return ReviewConfig{NewPerDay: 20, LeechThreshold: 8} }

// Reviewer composes the store primitives into a review session.
type Reviewer struct {
	Store *store.Store
	Cfg   ReviewConfig
}

// BuildQueue returns the cards to study now: everything due, then up to the
// remaining daily budget of new cards, reordered so adjacent cards avoid sharing
// a Part (sibling interference) when the queue contains more than one part.
func (rv *Reviewer) BuildQueue(project string, now, dayStart int64) ([]store.Flashcard, error) {
	due, err := rv.Store.DueReviewCards(project, now, 1000)
	if err != nil {
		return nil, err
	}
	introduced, err := rv.Store.IntroducedSince(project, dayStart)
	if err != nil {
		return nil, err
	}
	budget := rv.Cfg.NewPerDay - introduced
	var news []store.Flashcard
	if budget > 0 {
		if news, err = rv.Store.NewActiveCards(project, budget); err != nil {
			return nil, err
		}
	}
	return interleaveByPart(append(due, news...)), nil
}

// interleaveByPart greedily reorders so no two adjacent cards share a Part when
// a different-part card is available. Stable for a single part (returns as-is).
func interleaveByPart(cards []store.Flashcard) []store.Flashcard {
	if len(cards) < 3 {
		return cards
	}
	remaining := append([]store.Flashcard(nil), cards...)
	out := make([]store.Flashcard, 0, len(remaining))
	var lastPart string
	for len(remaining) > 0 {
		pick := 0
		for i, c := range remaining {
			if c.Part != lastPart {
				pick = i
				break
			}
		}
		out = append(out, remaining[pick])
		lastPart = remaining[pick].Part
		remaining = append(remaining[:pick], remaining[pick+1:]...)
	}
	return out
}

// Record grades one card: applies SM-2, persists state, appends the log, and
// suspends the card if its lapse count reaches the leech threshold.
func (rv *Reviewer) Record(cardID int64, g Grade, wasDue bool, now int64) (suspended bool, err error) {
	row, ok, err := rv.Store.GetReview(cardID)
	if err != nil {
		return false, err
	}
	var r Review
	if ok {
		r = fromRow(row)
	} else {
		r = NewReview(cardID)
		r.IntroducedAt = now // first review: stamp once
	}
	r = Schedule(r, g, now)
	if err := rv.Store.PutReview(toRow(r)); err != nil {
		return false, err
	}
	if err := rv.Store.AppendReviewLog(cardID, int(g), wasDue, now); err != nil {
		return false, err
	}
	if rv.Cfg.LeechThreshold > 0 && r.Lapses >= rv.Cfg.LeechThreshold {
		if err := rv.Store.SetCardStatus(cardID, "suspended", now); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func fromRow(r store.ReviewRow) Review {
	return Review{CardID: r.CardID, Ease: r.Ease, Interval: r.Interval, DueAt: r.DueAt,
		Reps: r.Reps, Lapses: r.Lapses, LastGrade: r.LastGrade, LastReviewed: r.LastReviewed, IntroducedAt: r.IntroducedAt}
}

func toRow(r Review) store.ReviewRow {
	return store.ReviewRow{CardID: r.CardID, Ease: r.Ease, Interval: r.Interval, DueAt: r.DueAt,
		Reps: r.Reps, Lapses: r.Lapses, LastGrade: r.LastGrade, LastReviewed: r.LastReviewed, IntroducedAt: r.IntroducedAt}
}
