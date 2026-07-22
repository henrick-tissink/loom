package ui

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// notifyCmd raises an attention signal for sessions that just flipped to
// needs-you: a macOS notification (with sound) or a terminal bell elsewhere.
//
// suppressed counts the transitions belonging to a HIDDEN project, which are
// escalated as a bare count and never named (§6.4). Both frontends implement
// the same contract — see cmd/loom-gui/notify.go's needsYou — because they run
// against one DB and ARCHITECTURE.md declares two live instances supported, so
// a disagreement here means hiding does something different depending on which
// window happens to be open.
func notifyCmd(items []string, suppressed int) tea.Cmd {
	return func() tea.Msg {
		if runtime.GOOS == "darwin" {
			_ = exec.Command("osascript", "-e", fmt.Sprintf(
				"display notification %q with title \"Loom\" sound name \"Glass\"",
				notifyBody(items, suppressed))).Run()
		} else {
			_, _ = os.Stderr.WriteString("\a")
		}
		return nil
	}
}

// notifyBody renders the banner text. Named sessions are listed as before;
// hidden ones only ever contribute to a count, so nothing in the string can be
// traced back to the project the user just put out of view.
func notifyBody(items []string, suppressed int) string {
	if len(items) == 0 {
		if suppressed == 1 {
			return "1 session needs you"
		}
		return fmt.Sprintf("%d sessions need you", suppressed)
	}
	body := strings.Join(items, ", ")
	if suppressed > 0 {
		body += fmt.Sprintf(" (+%d more)", suppressed)
	}
	return body
}
