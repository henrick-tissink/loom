package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// frame hand-composes a rounded panel (lipgloss borders can't embed text):
// top border carries the title + right-aligned annotation, bottom the keybar.
// Every returned line is exactly `width` terminal cells.
func frame(width int, title, right string, body []string, keybar string) string {
	if width < 20 {
		width = 20
	}
	inner := width - 4 // "│ " … " │"

	var b strings.Builder
	b.WriteString(frameEdge(width, "╭", "╮", styTitle.Render(title), styMeta.Render(right)))
	b.WriteByte('\n')
	for _, line := range body {
		w := lipgloss.Width(line)
		if w > inner {
			// Defensive hard clip: builders should pre-fit lines; a styled
			// overflow is clipped unstyled rather than corrupting the frame.
			line = truncPlain(stripAnsi(line), inner)
			w = lipgloss.Width(line)
		}
		b.WriteString(styChrome.Render("│ ") + line + strings.Repeat(" ", inner-w) + styChrome.Render(" │"))
		b.WriteByte('\n')
	}
	b.WriteString(frameEdge(width, "╰", "╯", styHelp.Render(keybar), ""))
	return b.String()
}

// frameEdge builds "╭─ <left> ──…── <right> ─╮" at exactly `width` cells.
func frameEdge(width int, open, close, left, right string) string {
	var mid strings.Builder
	used := 2 // open+close corners
	if lw := lipgloss.Width(left); lw > 0 {
		mid.WriteString(styChrome.Render("─ ") + left + styChrome.Render(" "))
		used += lw + 3
	} else {
		mid.WriteString(styChrome.Render("─"))
		used++
	}
	rw := lipgloss.Width(right)
	fill := width - used - rw
	if rw > 0 {
		fill -= 3 // " <right> ─" spacing: leading space + trailing " ─"
	}
	if fill < 0 {
		fill = 0
	}
	mid.WriteString(styChrome.Render(strings.Repeat("─", fill)))
	if rw > 0 {
		mid.WriteString(" " + right + styChrome.Render(" ─"))
	}
	line := styChrome.Render(open) + mid.String() + styChrome.Render(close)
	// Exactness guard: pad or clip stray cells (styled-width math edge cases).
	if d := width - lipgloss.Width(line); d > 0 {
		line = strings.TrimSuffix(line, styChrome.Render(close)) +
			styChrome.Render(strings.Repeat("─", d)+close)
	}
	return line
}

// stripAnsi removes SGR sequences (defensive clip path only).
func stripAnsi(s string) string {
	var out strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
