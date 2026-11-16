package main

import (
	"fmt"
)

// Claude Code's own commands, run in a session's pane from its header
// rather than rebuilt in loom: the review, the recap and the rewind picker
// are claude's, and change as claude ships. loom only types the command.

// nativeCommands is what SendCommand will type; anything else is refused,
// so the menu can't become a way to send arbitrary text.
var nativeCommands = map[string]bool{
	"/code-review": true,
	"/recap":       true,
	"/rewind":      true,
	"/goal clear":  true,
}

// commandRefusal is why a command can't be typed into a session now, or "".
// status is the rail row's; dialog is what an open dialog waits on.
func commandRefusal(cmd, status, dialog string, background, external bool) string {
	switch {
	case !nativeCommands[cmd]:
		return fmt.Sprintf("%q is not one of the commands loom runs", cmd)
	case external:
		return "loom didn't start this session; type it in its own terminal"
	case background:
		return "a background session has no pane to type into; attach to it"
	case dialog != "":
		return "the session is waiting on a dialog (" + dialog + "); attach and answer it first"
	case status == "running":
		return "the session is mid-turn; run it once it hands back"
	case status != "needs_you" && status != "idle":
		return "the session is not live"
	}
	return ""
}

// SendCommand types one of claude's own slash commands into a live session
// at rest and presses Enter. It is typed, not pasted, so claude reads it as
// a command.
func (a *App) SendCommand(name, cmd string) error {
	if a.tm == nil {
		return fmt.Errorf("tmux unavailable")
	}
	e, ok := a.railRow(name)
	if !ok {
		return fmt.Errorf("%s is not on the rail any more", name)
	}
	_, bg := backgroundJobID(name)
	if why := commandRefusal(cmd, e.dto.Status, a.dialogOpen(name), bg, e.dto.External); why != "" {
		return fmt.Errorf("%s", why)
	}
	if err := a.tm.SendLiteral(name, cmd); err != nil {
		return err
	}
	return a.tm.SendEnter(name)
}
