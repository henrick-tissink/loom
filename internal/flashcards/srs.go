package flashcards

import "math"

// SM-2 spaced repetition (spec §9), grade 1..4. Pure — persistence is separate.
type Grade int

const (
	GradeAgain Grade = 1 // forgot: reset
	GradeHard  Grade = 2 // recalled with difficulty
	GradeGood  Grade = 3 // recalled
	GradeEasy  Grade = 4 // trivial
)

func ValidGrade(g Grade) bool { return g >= GradeAgain && g <= GradeEasy }

// Review is one card's mutable SM-2 state (mirrors the flashcard_reviews row).
type Review struct {
	CardID       int64
	Ease         float64
	Interval     int   // days
	DueAt        int64 // unix seconds
	Reps         int
	Lapses       int
	LastGrade    int
	LastReviewed int64
	IntroducedAt int64
}

const (
	minEase     = 1.3
	startEase   = 2.5
	maxInterval = 365
	hardFactor  = 1.2 // Hard grows slowly and ease-independently, so Hard < Good
	easyBonus   = 1.3
	daySecs     = 86400
)

// NewReview is the state of a card that has never been reviewed.
func NewReview(cardID int64) Review { return Review{CardID: cardID, Ease: startEase} }

// Schedule applies one grade at time `now` and returns the next state. Again
// resets the card (reps→0, lapse++, ease−0.20, due tomorrow); a correct grade
// advances reps and schedules the next interval. Because ease ≥ 1.3 > hardFactor
// (1.2), a mature card always satisfies Hard < Good < Easy.
func Schedule(r Review, g Grade, now int64) Review {
	if !ValidGrade(g) {
		return r
	}
	switch g {
	case GradeAgain:
		r.Lapses++
		r.Reps = 0
		r.Ease = clampEase(r.Ease - 0.20)
		r.Interval = 1
	case GradeHard:
		r.Reps++
		r.Ease = clampEase(r.Ease - 0.15)
		r.Interval = nextInterval(r, g)
	case GradeGood:
		r.Reps++
		// ease unchanged (SM-2 q=4 → +0)
		r.Interval = nextInterval(r, g)
	case GradeEasy:
		r.Reps++
		r.Ease = clampEase(r.Ease + 0.15)
		r.Interval = nextInterval(r, g)
	}
	if r.Interval > maxInterval {
		r.Interval = maxInterval
	}
	if r.Interval < 1 {
		r.Interval = 1
	}
	r.LastGrade = int(g)
	r.LastReviewed = now
	r.DueAt = now + int64(r.Interval)*daySecs
	return r
}

// nextInterval assumes r.Reps was already incremented for this correct review.
// The first two correct reps use fixed steps (1 then 6 days); afterwards the
// interval compounds by ease (Good), a slow ease-independent factor (Hard), or
// ease plus a bonus (Easy).
func nextInterval(r Review, g Grade) int {
	switch {
	case r.Reps <= 1:
		return 1
	case r.Reps == 2:
		return 6
	default:
		prev := r.Interval
		if prev < 1 {
			prev = 1
		}
		switch g {
		case GradeHard:
			return int(math.Round(float64(prev) * hardFactor))
		case GradeEasy:
			return int(math.Round(float64(prev) * r.Ease * easyBonus))
		default: // Good
			return int(math.Round(float64(prev) * r.Ease))
		}
	}
}

func clampEase(e float64) float64 {
	if e < minEase {
		return minEase
	}
	return e
}
