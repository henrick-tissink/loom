package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// hydratePATH augments PATH so a double-clicked app finds the same tools a
// terminal does. Apps launched from Finder/Dock/Spotlight inherit only a
// minimal PATH (/usr/bin:/bin:/usr/sbin:/sbin), which omits Homebrew
// (/opt/homebrew/bin — tmux) and ~/.local/bin (claude). We best-effort merge
// the login shell's PATH and prepend common bin dirs, then set it on the
// process so CheckBinaries and every tmux-spawned claude session resolve
// correctly.
func hydratePATH() {
	home, _ := os.UserHomeDir()
	os.Setenv("PATH", buildPATH(os.Getenv("PATH"), loginShellPATH(), home))
}

// hydrateLocale defaults LC_CTYPE to UTF-8 when the process has no locale
// vars (empty counts as unset). Finder/Dock-launched bundles inherit no
// LANG/LC_*, and everything loom spawns — the tmux server, claude sessions,
// attach clients — misbehaves in a non-UTF-8 locale (tmux replaces multibyte
// glyphs and even the \t separators in -F command output with '_'). The
// tmux package also defends itself per-command; this covers every other
// descendant process. An existing locale is left untouched.
func hydrateLocale() {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if os.Getenv(k) != "" {
			return
		}
	}
	os.Setenv("LC_CTYPE", "UTF-8")
}

// buildPATH merges the current PATH, the login-shell PATH, and well-known bin
// dirs into a single deduplicated list, preserving first-seen order. Pure and
// unit-testable.
func buildPATH(current, shellPATH, home string) string {
	seen := map[string]bool{}
	var dirs []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		dirs = append(dirs, p)
	}
	for _, p := range filepath.SplitList(current) {
		add(p)
	}
	for _, p := range filepath.SplitList(shellPATH) {
		add(p)
	}
	for _, p := range []string{
		"/opt/homebrew/bin",
		"/usr/local/bin",
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "go", "bin"),
		filepath.Join(home, "bin"),
	} {
		add(p)
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// loginShellPATH returns the PATH a login shell would set (respecting the
// user's nvm/homebrew/rc config). Best-effort: empty string on any failure,
// bounded by a short timeout so a slow rc file can't hang startup.
func loginShellPATH() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, shell, "-l", "-c", `printf %s "$PATH"`).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
