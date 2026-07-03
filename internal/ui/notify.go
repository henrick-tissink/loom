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
func notifyCmd(items []string) tea.Cmd {
	return func() tea.Msg {
		if runtime.GOOS == "darwin" {
			script := fmt.Sprintf("display notification %q with title \"Loom\" sound name \"Glass\"",
				strings.Join(items, ", "))
			_ = exec.Command("osascript", "-e", script).Run()
		} else {
			_, _ = os.Stderr.WriteString("\a")
		}
		return nil
	}
}
