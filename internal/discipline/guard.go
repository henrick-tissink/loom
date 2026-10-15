package discipline

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Decision D5: only a human installs. The agent the hooks discipline can
// run `loom setup` through its Bash tool; these two checks are what stop
// it. Claude Code sets CLAUDECODE in every shell it starts, and its shells
// have no terminal attached. Neither check is a security boundary (a
// determined agent could unset the variable); the doctor's hash report
// is what makes any such change visible.

var (
	ErrAgent = errors.New("refusing: running inside a Claude Code session (CLAUDECODE is set). Install from a terminal you typed into, or from the loom GUI")
	ErrNoTTY = errors.New("refusing: no terminal attached. Run this from an interactive shell, or use the loom GUI")
)

// RefuseIfAgent returns ErrAgent or ErrNoTTY when a writing command must
// not proceed. stdin is the process's standard input.
func RefuseIfAgent(stdin *os.File) error {
	if os.Getenv("CLAUDECODE") != "" {
		return ErrAgent
	}
	if !isTerminal(stdin) {
		return ErrNoTTY
	}
	return nil
}

// isTerminal asks the kernel, not the file mode: /dev/null is a character
// device too, and a mode check would wave it through.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	_, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	return err == nil
}
