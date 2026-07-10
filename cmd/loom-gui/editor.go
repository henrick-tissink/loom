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

// OpenInEditor resolves path against the session's cwd and opens it in the
// user's editor at the given line. Errors (surfaced but harmless) if the file
// can't be found, so clicking a non-file token does nothing.
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
	argv := editorArgv(pickEditor(exec.LookPath), abs, line)
	// Fire and forget — the editor runs independently of loom — but reap the
	// child (GUI editor CLIs fork-and-exit) so it doesn't linger as a zombie.
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
