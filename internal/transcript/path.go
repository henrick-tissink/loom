// Package transcript locates and interprets claude's JSONL session transcripts.
package transcript

import (
	"path/filepath"
	"strings"
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
