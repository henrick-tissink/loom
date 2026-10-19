package main

import (
	"fmt"

	"github.com/henricktissink/loom/internal/ccsessions"
)

// ControlFromPhone sends /remote-control to a live session. When claude
// answers with its consent dialog (first use), the operator confirms it in
// the stage terminal: loom opens the door, it does not walk through it.
func (a *App) ControlFromPhone(name string) error {
	if a.launcher != nil {
		if e, ok := ccsessions.ByTmuxSession(ccsessions.Dir(a.launcher.ClaudeConfigDir))[name]; ok && e.RemoteURL() != "" {
			return fmt.Errorf("already under Remote Control: %s", e.RemoteURL())
		}
	}
	return a.SendPrompt(name, "/remote-control")
}
