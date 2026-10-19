package main

import (
	"os/exec"
	"strings"

	"github.com/henricktissink/loom/internal/ccsessions"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/tmux"
)

// backgroundJobID reports whether a row name is a background session's
// (status.BackgroundPrefix + job id) and returns the job id.
func backgroundJobID(name string) (string, bool) {
	id, ok := strings.CutPrefix(name, status.BackgroundPrefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// backgroundAttachCmd opens a background session in the stage terminal. It
// is the same hand-off as a loom pane's `tmux attach`: closing the terminal
// kills only this client, and claude's supervisor keeps the session (and
// restarts one it had stopped, from its saved conversation).
func backgroundAttachCmd(jobID string) *exec.Cmd {
	cmd := exec.Command("claude", "attach", jobID)
	cmd.Env = tmux.ClientEnv()
	return cmd
}

// stopBackground is overridable so tests never run the real claude.
var stopBackground = ccsessions.Stop
