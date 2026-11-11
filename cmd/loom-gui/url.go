package main

import (
	"fmt"
	"net/url"
	"runtime"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// isHTTPURL reports whether s is an http(s) URL — the only schemes we let a
// terminal ⌘-click open, so clicking terminal text can't fire file:,
// javascript:, or other schemes.
func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https")
}

// OpenURL opens an http(s) URL detected in the terminal in the user's default
// browser.
func (a *App) OpenURL(rawurl string) error {
	if !isHTTPURL(rawurl) {
		return fmt.Errorf("refusing to open non-http URL")
	}
	if a.ctx == nil {
		return fmt.Errorf("app not ready")
	}
	if runtime.GOOS == "linux" {
		return desktopOpen(rawurl)
	}
	wruntime.BrowserOpenURL(a.ctx, rawurl)
	return nil
}
