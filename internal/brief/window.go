// Package brief builds a project's study brief: a deep, per-task account of
// what was done in a time window, written by Claude from evidence loom
// collects deterministically (the session transcripts attributed to the
// project, and read-only git facts), and kept on disk so it can be re-read,
// asked about, and rendered as a page.
//
// The split of work is the design's (docs/ARCHITECTURE.md §10a): Go decides
// what happened (which records, which commits, which branch states); the
// model only writes the account, and every claim it makes cites an evidence
// id that points back into a transcript line.
package brief

import (
	"fmt"
	"strings"
	"time"
)

// Window is one of the brief's fixed look-back windows.
type Window struct {
	Key   string        // "24h", "3d", "5d", "1w"
	Label string        // "last 24 hours"
	Dur   time.Duration // how far back from now
}

// Windows are the choices the brief offers, in display order.
var Windows = []Window{
	{"24h", "last 24 hours", 24 * time.Hour},
	{"3d", "last 3 days", 3 * 24 * time.Hour},
	{"5d", "last 5 days", 5 * 24 * time.Hour},
	{"1w", "last week", 7 * 24 * time.Hour},
}

// ParseWindow returns the window for a key ("24h", "3d", "5d", "1w").
func ParseWindow(key string) (Window, error) {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, w := range Windows {
		if w.Key == k {
			return w, nil
		}
	}
	return Window{}, fmt.Errorf("unknown window %q (want 24h, 3d, 5d or 1w)", key)
}
