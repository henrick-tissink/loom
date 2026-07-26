package flashcards

import (
	"sort"

	"github.com/henricktissink/loom/internal/store"
)

// DeckCleanup summarizes the disposable cards in a project's deck: cards under
// parts that no longer exist (orphaned), cards under parts whose source drifted
// (stale), and never-curated drafts that sit under still-current parts. It is a
// preview — computing it mutates nothing.
type DeckCleanup struct {
	OrphanedParts []string `json:"orphanedParts"`
	StaleParts    []string `json:"staleParts"`
	Orphaned      int      `json:"orphaned"` // cards under orphaned parts
	Stale         int      `json:"stale"`    // cards under stale parts
	Drafts        int      `json:"drafts"`   // draft cards under current, non-stale parts
}

// InspectDeck classifies a project's cards without touching them. A part's cards
// all share a source_hash, so staleness/orphanhood is decided per part.
func InspectDeck(st *store.Store, project, projectRoot string) (DeckCleanup, error) {
	var out DeckCleanup
	cards, err := st.FlashcardsForProject(project)
	if err != nil {
		return out, err
	}
	parts, err := BuildManifest(projectRoot)
	if err != nil {
		return out, err
	}
	hash := make(map[string]string, len(parts))
	for _, p := range parts {
		hash[p.ID] = StructuralHash(p.SourceRef, p.Source)
	}
	byPart := map[string][]store.Flashcard{}
	for _, c := range cards {
		byPart[c.Part] = append(byPart[c.Part], c)
	}
	orphanSet, staleSet := map[string]bool{}, map[string]bool{}
	for part, cs := range byPart {
		h, ok := hash[part]
		if !ok {
			orphanSet[part] = true
			out.Orphaned += len(cs)
			continue
		}
		if len(cs) > 0 && cs[0].SourceHash != h {
			staleSet[part] = true
			out.Stale += len(cs)
		}
	}
	for p := range orphanSet {
		out.OrphanedParts = append(out.OrphanedParts, p)
	}
	for p := range staleSet {
		out.StaleParts = append(out.StaleParts, p)
	}
	sort.Strings(out.OrphanedParts)
	sort.Strings(out.StaleParts)
	for _, c := range cards {
		if c.Status == "draft" && !orphanSet[c.Part] && !staleSet[c.Part] {
			out.Drafts++
		}
	}
	return out, nil
}

// CleanDeck removes orphaned + stale parts' cards (always), and — only when
// dropDrafts is set — the remaining never-curated drafts. Returns the number of
// cards deleted. Part-level removals reuse the atomic cascade in
// DeleteCardsForPart; the draft sweep deletes card-by-card (each cascades).
func CleanDeck(st *store.Store, project, projectRoot string, dropDrafts bool) (int, error) {
	insp, err := InspectDeck(st, project, projectRoot)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, part := range append(append([]string{}, insp.OrphanedParts...), insp.StaleParts...) {
		n, derr := st.DeleteCardsForPart(project, part)
		if derr != nil {
			return deleted, derr
		}
		deleted += n
	}
	if dropDrafts {
		cards, err := st.FlashcardsForProject(project)
		if err != nil {
			return deleted, err
		}
		for _, c := range cards {
			if c.Status == "draft" {
				if err := st.DeleteCard(c.ID); err != nil {
					return deleted, err
				}
				deleted++
			}
		}
	}
	return deleted, nil
}
