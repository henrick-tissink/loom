package flashcards

import (
	"sort"

	"github.com/henricktissink/loom/internal/store"
)

// ChangedParts detects — WITHOUT mutating anything — which parts of a project
// have drifted from the current source since their cards were generated. It
// compares each part's stored source_hash (all a part's cards share one) to the
// part's current StructuralHash. Orphan parts (gone from the manifest) are NOT
// returned: they can't be regenerated, they're killed. The result is sorted.
//
// This is the live, read-only basis for the GUI's "N changed — regenerate" flow.
// CheckStale is its mutating CLI cousin (it flags status='stale' + drops cards
// from the queue); ChangedParts touches nothing, so opening Study to look at
// what changed never yanks a card from review on a hashing false positive.
func ChangedParts(st *store.Store, project, projectRoot string) ([]string, error) {
	cards, err := st.FlashcardsForProject(project)
	if err != nil {
		return nil, err
	}
	stored := make(map[string]string)
	for _, c := range cards {
		stored[c.Part] = c.SourceHash
	}
	parts, err := BuildManifest(projectRoot)
	if err != nil {
		return nil, err
	}
	cur := make(map[string]Part, len(parts))
	for _, p := range parts {
		cur[p.ID] = p
	}
	var changed []string
	for part, h := range stored {
		p, ok := cur[part]
		if !ok {
			continue // orphan — not regenerable
		}
		if StructuralHash(p.SourceRef, p.Source) != h {
			changed = append(changed, part)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// CheckResult summarizes a staleness reconcile.
type CheckResult struct {
	Checked int
	Stale   int // active card whose part's source drifted (source_hash mismatch)
	Orphan  int // active card whose part no longer exists in the manifest
}

// CheckStale reconciles a project's ACTIVE cards against the current source. It
// rebuilds the manifest and flags (status="stale") any active card whose part's
// structural source hash has drifted, or whose part is gone entirely (orphan).
// Stale cards drop out of the review queue until their part is regenerated.
func CheckStale(st *store.Store, project, projectRoot string, now int64) (CheckResult, error) {
	var res CheckResult
	parts, err := BuildManifest(projectRoot)
	if err != nil {
		return res, err
	}
	hashByPart := make(map[string]string, len(parts))
	for _, p := range parts {
		hashByPart[p.ID] = StructuralHash(p.SourceRef, p.Source)
	}
	cards, err := st.FlashcardsForProject(project)
	if err != nil {
		return res, err
	}
	for _, c := range cards {
		if c.Status != "active" {
			continue
		}
		res.Checked++
		cur, ok := hashByPart[c.Part]
		switch {
		case !ok:
			if err := st.SetCardStatus(c.ID, "stale", now); err != nil {
				return res, err
			}
			res.Orphan++
		case cur != c.SourceHash:
			if err := st.SetCardStatus(c.ID, "stale", now); err != nil {
				return res, err
			}
			res.Stale++
		}
	}
	return res, nil
}
