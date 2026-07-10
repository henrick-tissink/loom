package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// notifier fires native macOS notifications via osascript. The run func is
// injectable so tests don't spawn a process.
type notifier struct {
	run func(title, body string)
}

func newNotifier() *notifier { return &notifier{run: osascriptNotify} }

// needsYou fires one notification summarizing the sessions that just flipped
// to needs-you. No-op on an empty list — the engine's NewlyNeedsYou is
// once-only, so this fires exactly once per transition.
func (n *notifier) needsYou(labels []string) {
	if len(labels) == 0 {
		return
	}
	var body string
	if len(labels) == 1 {
		body = labels[0] + " needs you"
	} else {
		body = fmt.Sprintf("%d sessions need you", len(labels))
	}
	n.run("loom", body)
}

func osascriptNotify(title, body string) {
	script := fmt.Sprintf("display notification %s with title %s", asQuote(body), asQuote(title))
	// Fire and forget — a failed notification must never disrupt polling — but
	// still reap the child so it doesn't linger as a zombie.
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}
}

// asQuote wraps s in an AppleScript string literal, escaping backslashes and
// double quotes and flattening newlines so arbitrary session labels can't
// break (or inject into) the script.
func asQuote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return "\"" + s + "\""
}
