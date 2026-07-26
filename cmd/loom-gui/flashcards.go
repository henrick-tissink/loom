package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

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

// flashProjectKey is the store key for a project: its ROOT's basename, matching
// the CLI's projectName(root). The frontend hands the bridge a project ROOT
// (path); cards are keyed on the basename, so every project-scoped method must
// derive this — otherwise the Study pane queries the wrong key and shows nothing.
func flashProjectKey(projectRoot string) string {
	return filepath.Base(strings.TrimRight(projectRoot, "/"))
}

// reviewer builds a Reviewer over the app's store with default session config.
func (a *App) reviewer() *flashcards.Reviewer {
	return &flashcards.Reviewer{Store: a.st, Cfg: flashcards.DefaultReviewConfig()}
}

// FlashcardCoverage returns per-part card counts for a project (arg is the root).
func (a *App) FlashcardCoverage(projectRoot string) []CoverageDTO {
	out := []CoverageDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	cov, err := a.reviewer().Coverage(flashProjectKey(projectRoot), a.now().Unix())
	if err != nil {
		return out
	}
	for _, c := range cov {
		out = append(out, CoverageDTO{Part: c.Part, Total: c.Total, Active: c.Active, Draft: c.Draft, Due: c.Due})
	}
	return out
}

// FlashcardStats returns the measured pass-rate (over due reviews) plus coverage.
func (a *App) FlashcardStats(projectRoot string) FlashcardStatsDTO {
	out := FlashcardStatsDTO{Parts: []CoverageDTO{}}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	rate, n, err := a.reviewer().PassRate(flashProjectKey(projectRoot), 0)
	if err != nil {
		return out
	}
	out.PassRate = rate
	out.Reviews = n
	out.Parts = a.FlashcardCoverage(projectRoot)
	return out
}

// FlashcardDrafts returns a project's uncurated cards.
func (a *App) FlashcardDrafts(projectRoot string) []FlashcardDTO {
	out := []FlashcardDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	drafts, err := a.st.DraftsForProject(flashProjectKey(projectRoot))
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
func (a *App) FlashcardQueue(projectRoot string) []FlashcardDTO {
	out := []FlashcardDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	project := flashProjectKey(projectRoot)
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

var errFlashNoStore = errors.New("flashcards: no store")

// FlashcardGrade records a grade (1..4) for a card and reports whether the card
// was auto-suspended as a leech. wasDue must be the value the queue reported for
// this card (retention vs. first-exposure), so the review log stays honest.
func (a *App) FlashcardGrade(cardID int64, grade int, wasDue bool) (suspended bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			suspended, err = false, fmt.Errorf("flashcards: grade panicked: %v", r)
		}
	}()
	if a.st == nil {
		return false, errFlashNoStore
	}
	g := flashcards.Grade(grade)
	if !flashcards.ValidGrade(g) {
		return false, fmt.Errorf("flashcards: invalid grade %d", grade)
	}
	return a.reviewer().Record(cardID, g, wasDue, a.now().Unix())
}

// FlashcardActivate curates one draft card into the active deck.
func (a *App) FlashcardActivate(cardID int64) error {
	if a.st == nil {
		return errFlashNoStore
	}
	return a.st.SetCardStatus(cardID, "active", a.now().Unix())
}

// FlashcardActivateAll activates every draft in a project and returns the count.
func (a *App) FlashcardActivateAll(projectRoot string) (int, error) {
	if a.st == nil {
		return 0, errFlashNoStore
	}
	drafts, err := a.st.DraftsForProject(flashProjectKey(projectRoot))
	if err != nil {
		return 0, err
	}
	now := a.now().Unix()
	n := 0
	for _, c := range drafts {
		if err := a.st.SetCardStatus(c.ID, "active", now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// FlashcardEdit rewrites a card's text, recomputing its stem and answer hashes.
func (a *App) FlashcardEdit(cardID int64, front, back string) error {
	if a.st == nil {
		return errFlashNoStore
	}
	return a.st.EditCardText(cardID, front, back, flashcards.StemHash(front), flashcards.Hash(back), a.now().Unix())
}

// FlashcardKill deletes a card and its review history (curation reject).
func (a *App) FlashcardKill(cardID int64) error {
	if a.st == nil {
		return errFlashNoStore
	}
	return a.st.DeleteCard(cardID)
}

// FlashcardStaleParts returns the parts (part IDs) that carry a stale card —
// what the "regenerate all stale" banner counts and what the coverage view marks.
func (a *App) FlashcardStaleParts(projectRoot string) []string {
	out := []string{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	parts, err := a.st.StalePartsForProject(flashProjectKey(projectRoot))
	if err != nil {
		return out
	}
	return append(out, parts...)
}

// ---- generation (slice 6b): async, one job at a time, progress via events ----
//
// Generation runs the hardened claude -p pipeline (author + cited-source verify),
// which is minutes-long — so it runs off the main thread in a goroutine, guarded
// to one job at a time (the summarize precedent, app.go sumBusy), emitting
// "flashcards:progress" per part and "flashcards:done" at the end. The frontend
// listens and refreshes the coverage view. REPLACE semantics: a (re)generated
// part's existing cards are wiped before re-authoring (user's choice).

// GenScopesDTO is the part count each no-path scope would cover, for the dialog.
type GenScopesDTO struct {
	All       int `json:"all"`
	Docs      int `json:"docs"`
	Uncovered int `json:"uncovered"`
}

// FlashcardGenerateScopes returns how many parts each no-path scope covers, so
// the Generate dialog can be honest up front ("whole project — 120 parts").
func (a *App) FlashcardGenerateScopes(projectRoot string) GenScopesDTO {
	out := GenScopesDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	parts, err := flashcards.BuildManifest(projectRoot)
	if err != nil {
		return out
	}
	covered := map[string]bool{}
	if cp, err := a.st.PartsForProject(flashProjectKey(projectRoot)); err == nil {
		for _, id := range cp {
			covered[id] = true
		}
	}
	for _, p := range parts {
		out.All++
		if p.Kind == flashcards.PartDoc {
			out.Docs++
		}
		if !covered[p.ID] {
			out.Uncovered++
		}
	}
	return out
}

// FlashcardGenerate (re)generates, in the background, the manifest parts a scope
// selects — a no-path preset ("docs" = the architecture's sections, "uncovered",
// "all") or "path" + a filter to target a file/subtree. Returns once queued, or
// an error if a job is already running / the scope selects nothing.
func (a *App) FlashcardGenerate(projectRoot, scope, filter string) error {
	if a.st == nil {
		return errFlashNoStore
	}
	gs := flashcards.GenScope(scope)
	if gs == flashcards.ScopePath && strings.TrimSpace(filter) == "" {
		return fmt.Errorf("flashcards: a path filter is required")
	}
	parts, err := flashcards.BuildManifest(projectRoot)
	if err != nil {
		return err
	}
	project := flashProjectKey(projectRoot)
	var covered map[string]bool
	if gs == flashcards.ScopeUncovered {
		cp, err := a.st.PartsForProject(project)
		if err != nil {
			return err
		}
		covered = make(map[string]bool, len(cp))
		for _, id := range cp {
			covered[id] = true
		}
	}
	todo := flashcards.SelectParts(parts, gs, filter, covered)
	if len(todo) == 0 {
		return fmt.Errorf("flashcards: nothing to generate for that scope")
	}
	return a.startGeneration(project, todo)
}

// FlashcardGenerateCancel asks a running job to stop after the part it is on.
func (a *App) FlashcardGenerateCancel() {
	a.genMu.Lock()
	if a.genBusy {
		a.genCancel = true
	}
	a.genMu.Unlock()
}

// FlashcardRegenerateStale regenerates every part carrying a stale card, in the
// background, returning how many parts were queued. Orphan-stale parts (gone from
// the manifest) can't be regenerated and are skipped — kill those cards instead.
func (a *App) FlashcardRegenerateStale(projectRoot string) (int, error) {
	if a.st == nil {
		return 0, errFlashNoStore
	}
	project := flashProjectKey(projectRoot)
	staleParts, err := a.st.StalePartsForProject(project)
	if err != nil {
		return 0, err
	}
	if len(staleParts) == 0 {
		return 0, nil
	}
	parts, err := flashcards.BuildManifest(projectRoot)
	if err != nil {
		return 0, err
	}
	byID := make(map[string]flashcards.Part, len(parts))
	for _, p := range parts {
		byID[p.ID] = p
	}
	var todo []flashcards.Part
	for _, id := range staleParts {
		if p, ok := byID[id]; ok {
			todo = append(todo, p)
		}
	}
	if len(todo) == 0 {
		return 0, nil
	}
	if err := a.startGeneration(project, todo); err != nil {
		return 0, err
	}
	return len(todo), nil
}

// startGeneration claims the one-job guard and runs the pipeline over todo in a
// background goroutine; errors immediately if a job is already running.
func (a *App) startGeneration(project string, todo []flashcards.Part) error {
	a.genMu.Lock()
	if a.genBusy {
		a.genMu.Unlock()
		return fmt.Errorf("flashcards: a generation job is already running")
	}
	a.genBusy = true
	a.genCancel = false
	a.genMu.Unlock()
	go a.runGeneration(project, todo)
	return nil
}

func (a *App) runGeneration(project string, todo []flashcards.Part) {
	defer func() {
		_ = recover()
		a.genMu.Lock()
		a.genBusy = false
		a.genCancel = false
		a.genMu.Unlock()
		a.emitFlash("flashcards:done", map[string]any{"project": project})
	}()
	pl := &flashcards.Pipeline{
		Store: a.st,
		Gen:   &flashcards.Generator{Binary: "claude", WorkDir: a.loomDir},
		Ver:   &flashcards.Verifier{Binary: "claude", WorkDir: a.loomDir},
	}
	for _, p := range todo {
		a.genMu.Lock()
		stop := a.genCancel
		a.genMu.Unlock()
		if stop {
			return // user asked to stop; the deferred "done" fires
		}
		a.emitFlash("flashcards:progress", map[string]any{"project": project, "part": p.ID, "running": true})
		deleted, stored, rejected, err := pl.RegeneratePart(project, p, a.now().Unix())
		ev := map[string]any{"project": project, "part": p.ID, "running": false, "deleted": deleted, "stored": stored, "rejected": rejected}
		if err != nil {
			ev["error"] = err.Error()
		}
		a.emitFlash("flashcards:progress", ev)
	}
}

// emitFlash emits a flashcards event to the frontend when the window is up.
func (a *App) emitFlash(event string, data any) {
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, event, data)
	}
}
