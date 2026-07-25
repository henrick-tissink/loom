package flashcards

import "github.com/henricktissink/loom/internal/store"

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
