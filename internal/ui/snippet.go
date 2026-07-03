package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// FTS5's snippet() (spec §4/§6) wraps each matched term with these two C0
// control bytes: `char(1)` opens a highlight, `char(2)` closes it. All other
// C0 control chars are stripped from indexed text at index time (spec §2),
// so these two are safe to treat as the ONLY structural markers here.
const (
	hitOpen  = '\x01'
	hitClose = '\x02'
)

// span is a highlighted run, as rune-offsets into the PLAIN (marker-
// stripped) text — end is exclusive.
type span struct{ start, end int }

// renderSnippet turns a raw FTS snippet (plain text containing \x01/\x02
// highlight markers) into a styled line at most `max` cells wide (spec §6).
//
// Pipeline: strip the markers in one pass over runes, recording the
// rune-range each pair bracketed -> truncate the pure plain text to `max`
// CELLS (not runes: CJK runes are 2 cells wide, so a naive rune-count cap —
// like truncPlain elsewhere — can overshoot the terminal-cell budget) ->
// rebuild the line, re-applying styHit only to the portion of each range
// that survived truncation. A range bisected by the cut closes AT the cut
// (never left dangling past the visible text). The non-highlighted runs are
// rendered in styMeta (dim base) per spec — done here, per-segment, rather
// than by wrapping the whole returned string in styMeta afterward: nesting
// lipgloss Renders like that embeds the inner style's SGR reset mid-string,
// which would clobber the outer dim style for any text following a
// highlight.
func renderSnippet(raw string, max int) string {
	if max <= 0 {
		return ""
	}

	plain, spans := stripHitMarkers(raw)
	text, cut, truncated := truncCells(plain, max)

	var b strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp.start >= cut {
			continue // this whole highlight fell past the cut
		}
		end := sp.end
		if end > cut {
			end = cut // bisected range closes at the cut
		}
		if sp.start > pos {
			b.WriteString(styMeta.Render(string(text[pos:sp.start])))
		}
		if end > sp.start {
			b.WriteString(styHit.Render(string(text[sp.start:end])))
		}
		pos = end
	}
	if pos < len(text) {
		b.WriteString(styMeta.Render(string(text[pos:])))
	}
	if truncated {
		b.WriteString(styMeta.Render("…"))
	}
	return b.String()
}

// stripHitMarkers removes \x01/\x02 pairs from raw, returning the plain
// runes plus the rune-range each pair bracketed. An unterminated \x01 (never
// observed from snippet() in practice, but defensive) closes at the end of
// the text rather than being dropped silently.
func stripHitMarkers(raw string) ([]rune, []span) {
	var plain []rune
	var spans []span
	open := -1
	for _, r := range raw {
		switch r {
		case hitOpen:
			open = len(plain)
		case hitClose:
			if open >= 0 {
				spans = append(spans, span{open, len(plain)})
				open = -1
			}
		default:
			plain = append(plain, r)
		}
	}
	if open >= 0 {
		spans = append(spans, span{open, len(plain)})
	}
	return plain, spans
}

// truncCells caps plain to `max` terminal CELLS, reserving one cell for a
// trailing ellipsis when truncation is needed. Returns the surviving runes,
// the rune-index cut point (== len(plain) when nothing was cut), and whether
// truncation happened.
func truncCells(plain []rune, max int) (text []rune, cut int, truncated bool) {
	full := 0
	for _, r := range plain {
		full += lipgloss.Width(string(r))
	}
	if full <= max {
		return plain, len(plain), false
	}
	budget := max - 1 // reserve a cell for "…"
	w, i := 0, 0
	for i < len(plain) {
		rw := lipgloss.Width(string(plain[i]))
		if w+rw > budget {
			break
		}
		w += rw
		i++
	}
	return plain[:i], i, true
}
