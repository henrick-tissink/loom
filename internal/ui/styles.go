// Package ui is Loom's Bubble Tea TUI: the home dashboard and launcher.
package ui

import "github.com/charmbracelet/lipgloss"

var (
	styTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("219"))
	stySection  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	styCursor   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("219"))
	styNeedsYou = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styRunning  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styIdle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styDone     = lipgloss.NewStyle().Foreground(lipgloss.Color("71"))
	styErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	styHelp     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styMeta     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
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
