package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// resolveFile resolves path (possibly relative to cwd) to an existing absolute
// file path, or "" if it doesn't resolve to a regular file. This is the guard
// that keeps a click on a non-path token from launching anything.
func resolveFile(cwd, path string) string {
	if path == "" {
		return ""
	}
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	return ""
}

// pickEditor returns the first available GUI editor binary, or "open" (the
// macOS default-application opener) as a fallback. look is injected for tests.
func pickEditor(look func(string) (string, error)) string {
	for _, bin := range []string{"cursor", "code", "zed"} {
		if _, err := look(bin); err == nil {
			return bin
		}
	}
	return "open"
}

// editorArgv builds the command to open absPath at line for a given editor.
func editorArgv(bin, absPath string, line int) []string {
	target := absPath
	if line > 0 {
		target = fmt.Sprintf("%s:%d", absPath, line)
	}
	switch bin {
	case "cursor", "code":
		return []string{bin, "-g", target}
	case "zed":
		return []string{bin, target}
	default:
		return []string{"open", absPath} // macOS default app; no line support
	}
}

// editorApp maps an editor CLI to its macOS application name for `open -a`.
// Returns "" for the `open` fallback, which already activates the default app.
func editorApp(bin string) string {
	switch bin {
	case "cursor":
		return "Cursor"
	case "code":
		return "Visual Studio Code"
	case "zed":
		return "Zed"
	default:
		return ""
	}
}

// editorCommands returns the command(s) to open absPath at line AND surface the
// editor. A GUI editor launched from another GUI app (loom) opens the file in
// the background without stealing focus — macOS does not transfer foreground
// activation across the launch — so the file appears but the window never
// rises, and clicking a link looks like it did nothing. We follow the open
// with `open -a <App>` to raise it. The `open` fallback already activates its
// target, so it stays a single command.
func editorCommands(bin, absPath string, line int) [][]string {
	cmds := [][]string{editorArgv(bin, absPath, line)}
	if app := editorApp(bin); app != "" {
		cmds = append(cmds, []string{"open", "-a", app})
	}
	return cmds
}

// OpenInEditor resolves path against the session's cwd and opens it in the
// user's editor at the given line, bringing the editor to the foreground.
// Errors (surfaced but harmless) if the file can't be found, so clicking a
// non-file token does nothing.
func (a *App) OpenInEditor(sessionName, path string, line int) error {
	cwd := ""
	if a.st != nil {
		if row, ok, _ := a.st.Get(sessionName); ok {
			cwd = row.Cwd
		}
	}
	abs := resolveFile(cwd, path)
	if abs == "" {
		return fmt.Errorf("file not found: %s", path)
	}
	// Fire and forget — the editor runs independently of loom — but reap each
	// child (GUI editor CLIs fork-and-exit) so it doesn't linger as a zombie.
	// The open-at-line command must start before the activate so the file is
	// already loading when the window rises.
	for _, argv := range editorCommands(pickEditor(exec.LookPath), abs, line) {
		cmd := exec.Command(argv[0], argv[1:]...)
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { _ = cmd.Wait() }()
	}
	return nil
}
