package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// frame hand-composes a rounded panel (lipgloss borders can't embed text):
// top border carries the title + right-aligned annotation, bottom the keybar.
// title, right, and keybar are PLAIN (unstyled) text — frame/frameEdge
// truncate them to fit `width` first and apply styles internally, so a
// styled string is never sliced mid-escape-sequence.
// Every returned line is exactly `width` terminal cells.
func frame(width int, title, right string, body []string, keybar string) string {
	if width < 20 {
		width = 20
	}
	inner := width - 4 // "│ " … " │"

	var b strings.Builder
	b.WriteString(frameEdge(width, "╭", "╮", title, styTitle, right, styMeta))
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
	b.WriteString(frameEdge(width, "╰", "╯", keybar, styHelp, "", styMeta))
	return b.String()
}

// frameEdge builds "╭─ <left> ──…── <right> ─╮" at exactly `width` cells.
// left/right are PLAIN text; frameEdge fits them to the available space
// before rendering, degrading in this order as room runs out:
//  1. drop the right annotation entirely
//  2. ellipsize left (the title or the keybar, whichever is passed in)
//
// Corners and at least one ─ fill dash on each side always survive.
func frameEdge(width int, open, close string, left string, leftStyle lipgloss.Style, right string, rightStyle lipgloss.Style) string {
	inner := width - 2 // between the two corners
	if inner < 0 {
		inner = 0
	}

	// cost of rendering a side's text incl. its surrounding chrome:
	// "─ " + left + " " on the left side, " " + right + " ─" on the right.
	// An empty left still contributes a single bare "─"; an empty right
	// contributes nothing (fill dashes run straight to the close corner).
	leftCost := func(w int) int {
		if w == 0 {
			return 1
		}
		return w + 3
	}
	rightCost := func(w int) int {
		if w == 0 {
			return 0
		}
		return w + 3
	}

	lw, rw := len([]rune(left)), len([]rune(right))
	fill := inner - leftCost(lw) - rightCost(rw)

	// Degrade 1: drop the right annotation entirely if it doesn't fit.
	if rw > 0 && fill < 1 {
		right, rw = "", 0
		fill = inner - leftCost(lw) - rightCost(rw)
	}
	// Degrade 2: ellipsize left until the mandatory fill dash fits.
	for fill < 1 && lw > 0 {
		lw--
		fill = inner - leftCost(lw) - rightCost(rw)
	}
	if fill < 0 {
		fill = 0
	}
	left = truncPlain(left, lw)

	var mid strings.Builder
	if lw > 0 {
		mid.WriteString(styChrome.Render("─ ") + leftStyle.Render(left) + styChrome.Render(" "))
	} else {
		mid.WriteString(styChrome.Render("─"))
	}
	mid.WriteString(styChrome.Render(strings.Repeat("─", fill)))
	if rw > 0 {
		mid.WriteString(styChrome.Render(" ") + rightStyle.Render(right) + styChrome.Render(" ─"))
	}
	return styChrome.Render(open) + mid.String() + styChrome.Render(close)
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
