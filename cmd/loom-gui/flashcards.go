package main

import (
	"github.com/henricktissink/loom/internal/flashcards"
	"github.com/henricktissink/loom/internal/store"
)

// FlashcardDTO is one study card for the GUI. WasDue marks a card drawn from the
// due set (a retention re-test) rather than a newly introduced card; the grade
// call passes it back so the review log records due-vs-first-exposure honestly.
type FlashcardDTO struct {
	ID     int64  `json:"id"`
	Part   string `json:"part"`
	Type   string `json:"type"`
	Front  string `json:"front"`
	Back   string `json:"back"`
	Status string `json:"status"`
	WasDue bool   `json:"wasDue"`
}

// CoverageDTO is per-part card counts for the coverage map (no "mastery %").
type CoverageDTO struct {
	Part   string `json:"part"`
	Total  int    `json:"total"`
	Active int    `json:"active"`
	Draft  int    `json:"draft"`
	Due    int    `json:"due"`
}

// FlashcardStatsDTO is the project's measured pass-rate (over due reviews) and coverage.
type FlashcardStatsDTO struct {
	PassRate float64       `json:"passRate"` // 0..1
	Reviews  int           `json:"reviews"`
	Parts    []CoverageDTO `json:"parts"`
}

// reviewer builds a Reviewer over the app's store with default session config.
func (a *App) reviewer() *flashcards.Reviewer {
	return &flashcards.Reviewer{Store: a.st, Cfg: flashcards.DefaultReviewConfig()}
}

// FlashcardCoverage returns per-part card counts for a project.
func (a *App) FlashcardCoverage(project string) []CoverageDTO {
	out := []CoverageDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	cov, err := a.reviewer().Coverage(project, a.now().Unix())
	if err != nil {
		return out
	}
	for _, c := range cov {
		out = append(out, CoverageDTO{Part: c.Part, Total: c.Total, Active: c.Active, Draft: c.Draft, Due: c.Due})
	}
	return out
}

// FlashcardStats returns the measured pass-rate (over due reviews) plus coverage.
func (a *App) FlashcardStats(project string) FlashcardStatsDTO {
	out := FlashcardStatsDTO{Parts: []CoverageDTO{}}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	rate, n, err := a.reviewer().PassRate(project, 0)
	if err != nil {
		return out
	}
	out.PassRate = rate
	out.Reviews = n
	out.Parts = a.FlashcardCoverage(project)
	return out
}

// FlashcardDrafts returns a project's uncurated cards.
func (a *App) FlashcardDrafts(project string) []FlashcardDTO {
	out := []FlashcardDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	drafts, err := a.st.DraftsForProject(project)
	if err != nil {
		return out
	}
	for _, c := range drafts {
		out = append(out, cardDTO(c, false))
	}
	return out
}

// FlashcardQueue returns the review-session queue (due cards, then capped new
// cards, interleaved), each marked WasDue from the due set.
func (a *App) FlashcardQueue(project string) []FlashcardDTO {
	out := []FlashcardDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	now := a.now().Unix()
	dayStart := now - now%86400
	due, err := a.st.DueReviewCards(project, now, 1000)
	if err != nil {
		return out
	}
	dueIDs := make(map[int64]bool, len(due))
	for _, c := range due {
		dueIDs[c.ID] = true
	}
	queue, err := a.reviewer().BuildQueue(project, now, dayStart)
	if err != nil {
		return out
	}
	for _, c := range queue {
		out = append(out, cardDTO(c, dueIDs[c.ID]))
	}
	return out
}

func cardDTO(c store.Flashcard, wasDue bool) FlashcardDTO {
	return FlashcardDTO{ID: c.ID, Part: c.Part, Type: c.Type, Front: c.Front, Back: c.Back, Status: c.Status, WasDue: wasDue}
}
