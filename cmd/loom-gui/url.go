package main

import (
	"fmt"
	"strings"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// isHTTPURL reports whether s is an http(s) URL — the only schemes we let a
// terminal ⌘-click open, so clicking terminal text can't fire file:,
// javascript:, or other schemes.
func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
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
	wruntime.BrowserOpenURL(a.ctx, rawurl)
	return nil
}
