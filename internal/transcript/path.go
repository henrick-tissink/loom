// Package transcript locates and interprets claude's JSONL session transcripts.
package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProjectDirName applies claude's encoding: every non-[a-zA-Z0-9] rune → '-'.
func ProjectDirName(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func Path(claudeConfigDir, cwd, sessionID string) string {
	return filepath.Join(claudeConfigDir, "projects", ProjectDirName(cwd), sessionID+".jsonl")
}

// NewestSince returns the session ID of the newest transcript in cwd's project
// dir modified after `since` — the fallback correlation when the session ID
// isn't deterministic (claude --resume mints a new one).
func NewestSince(claudeConfigDir, cwd string, since time.Time) (string, error) {
	dir := filepath.Join(claudeConfigDir, "projects", ProjectDirName(cwd))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var best string
	var bestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(since) && info.ModTime().After(bestMod) {
			best = strings.TrimSuffix(e.Name(), ".jsonl")
			bestMod = info.ModTime()
		}
	}
	return best, nil
}
