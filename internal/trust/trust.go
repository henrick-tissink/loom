// Package trust reads claude's per-project trust flags so Loom never fires a
// seed into the first-run trust dialog (spec §3.2 trust gate).
package trust

import (
	"encoding/json"
	"fmt"
	"os"
)

type projectEntry struct {
	HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
}

type claudeJSON struct {
	Projects map[string]projectEntry `json:"projects"`
}

func IsTrusted(claudeJSONPath, cwd string) (bool, error) {
	data, err := os.ReadFile(claudeJSONPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var cj claudeJSON
	if err := json.Unmarshal(data, &cj); err != nil {
		return false, fmt.Errorf("parse %s: %w", claudeJSONPath, err)
	}
	return cj.Projects[cwd].HasTrustDialogAccepted, nil
}
