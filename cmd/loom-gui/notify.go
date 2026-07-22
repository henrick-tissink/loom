package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/status"
)

// notifier fires native macOS notifications via osascript. The run func is
// injectable so tests don't spawn a process.
type notifier struct {
	run func(title, body string)
}

func newNotifier() *notifier { return &notifier{run: osascriptNotify} }

// needsYou fires one notification summarizing the sessions that just flipped
// to needs-you. No-op on an empty transition — the engine's NewlyNeedsYou is
// once-only, so this fires exactly once per transition.
//
// It takes rendered labels rather than the engine's session names because
// rendering and project filtering belong above the engine: the engine reports
// identity only (spec §4). onSnapshot does the join.
//
// suppressed counts the flipped sessions belonging to a HIDDEN project.
// They are not dropped outright: §6.4 requires attention to still escalate,
// degraded to a label-free body. Naming them is the leak; silently swallowing
// them would mean a demo where the user never learns an agent is blocked.
func (n *notifier) needsYou(labels []string, suppressed int) {
	total := len(labels) + suppressed
	if total == 0 {
		return
	}
	var body string
	switch {
	case total == 1 && len(labels) == 1:
		body = labels[0] + " needs you"
	case total == 1:
		body = "1 session needs you"
	default:
		body = fmt.Sprintf("%d sessions need you", total)
	}
	n.run("loom", body)
}

// needsYouLabels renders "ProjectLabel · Title" for each session the engine
// reported as newly needs-you. The engine reports session names (spec §4) —
// a pre-rendered label cannot be attributed to a project, since ProjectLabel
// is filepath.Base(cwd) for adopted orphans — so the label is recovered here
// by joining against the same snapshot's Live rows. A name with no Live row
// cannot happen from a real poll (both come from the same pass over Live);
// it is dropped rather than announced as an empty banner.
//
// Sessions belonging to a hidden project are counted in `suppressed` instead
// of labelled: notifications are leak surface 10 (§6.3), and the label is the
// leak — ProjectLabel and Title both name the client's work.
func needsYouLabels(snap status.Snapshot, res *projects.Resolver) (labels []string, suppressed int) {
	for _, name := range snap.NewlyNeedsYou {
		for _, r := range snap.Live {
			if r.Name != name {
				continue
			}
			if !visible(res, sessionDirs(r.SessionRow)...) {
				suppressed++
				break
			}
			label := r.ProjectLabel
			if r.Title != "" {
				label += " · " + r.Title
			}
			labels = append(labels, label)
			break
		}
	}
	return labels, suppressed
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
