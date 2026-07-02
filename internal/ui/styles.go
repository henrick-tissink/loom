// Package ui is Loom's Bubble Tea TUI: the mission-control dashboard.
package ui

import "github.com/charmbracelet/lipgloss"

// The Mission Control palette (spec 2026-07-03) — the only colors allowed.
var (
	colAccent = lipgloss.Color("219") // wordmark, cursor
	colAlert  = lipgloss.Color("203") // needs-you
	colRun    = lipgloss.Color("214") // running
	colDone   = lipgloss.Color("71")  // done
	colMeta   = lipgloss.Color("245") // secondary text
	colChrome = lipgloss.Color("240") // frame, rules, help
)

var (
	styTitle    = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styCursor   = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styNeedsYou = lipgloss.NewStyle().Foreground(colAlert)
	styRunning  = lipgloss.NewStyle().Foreground(colRun)
	styIdle     = lipgloss.NewStyle().Foreground(colMeta)
	styDone     = lipgloss.NewStyle().Foreground(colDone)
	styErr      = lipgloss.NewStyle().Foreground(colAlert).Bold(true)
	styMeta     = lipgloss.NewStyle().Foreground(colMeta)
	styChrome   = lipgloss.NewStyle().Foreground(colChrome)
	styHelp     = lipgloss.NewStyle().Foreground(colChrome)
)

func statusIcon(status string) string {
	switch status {
	case "needs_you":
		return styNeedsYou.Render("●")
	case "running":
		return styRunning.Render("◐")
	case "idle":
		return styIdle.Render("○")
	case "done":
		return styDone.Render("✓")
	case "error":
		return styErr.Render("✗")
	default:
		return styIdle.Render("·")
	}
}
