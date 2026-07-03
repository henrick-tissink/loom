package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestRenderSnippetTable(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		max  int
		// wantContains are plain substrings the rendered (ANSI-wrapped)
		// output must still contain unbroken (Render just wraps prefix/text/
		// suffix, so an un-truncated substring always survives verbatim).
		wantContains []string
		exactWidth   bool // assert lipgloss.Width(out) == max exactly
	}{
		{
			name:         "no markers",
			raw:          "hello world this is a plain snippet",
			max:          100,
			wantContains: []string{"hello world this is a plain snippet"},
		},
		{
			name:         "markers mid-string",
			raw:          "before \x01widget\x02 after",
			max:          100,
			wantContains: []string{"before", "widget", "after"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := renderSnippet(c.raw, c.max)
			if strings.ContainsAny(out, "\x01\x02") {
				t.Fatalf("output still contains control markers: %q", out)
			}
			for _, want := range c.wantContains {
				if !strings.Contains(out, want) {
					t.Fatalf("output missing %q: %q", want, out)
				}
			}
			if lw := lipgloss.Width(out); lw > c.max {
				t.Fatalf("width %d exceeds max %d: %q", lw, c.max, out)
			}
		})
	}
}

// TestRenderSnippetNoMarkersWidthMatchesPlain: with no highlights, the
// rendered width must equal the plain (unstyled) text's width — ANSI
// wrapping must never add cells.
func TestRenderSnippetNoMarkersWidthMatchesPlain(t *testing.T) {
	raw := "hello world this is a plain snippet"
	out := renderSnippet(raw, 100)
	if lw, pw := lipgloss.Width(out), lipgloss.Width(raw); lw != pw {
		t.Fatalf("width = %d, want %d (plain width, no truncation): %q", lw, pw, out)
	}
}

// TestRenderSnippetMarkersWidthMatchesPlain: styling a highlighted run must
// not change the line's rendered cell width vs. the marker-stripped plain
// text.
func TestRenderSnippetMarkersWidthMatchesPlain(t *testing.T) {
	raw := "before \x01widget\x02 after"
	plain := "before widget after"
	out := renderSnippet(raw, 100)
	if lw, pw := lipgloss.Width(out), lipgloss.Width(plain); lw != pw {
		t.Fatalf("width = %d, want %d (plain width): %q", lw, pw, out)
	}
}

// TestRenderSnippetStraddlingCut is the P0 fixture (spec §6): a highlighted
// span that straddles the truncation cut must close AT the cut — output is
// exactly `max` cells, contains no raw control bytes, and the surviving
// (bisected) portion of the highlight is still present.
func TestRenderSnippetStraddlingCut(t *testing.T) {
	// plain = "aaaa bbbbbbbbbb cccc" (20 runes); highlight spans the 10 b's
	// (rune indices 5..14). max=10 truncates mid-highlight (cut at rune 9),
	// surviving highlighted run = "bbbb".
	raw := "aaaa \x01bbbbbbbbbb\x02 cccc"
	out := renderSnippet(raw, 10)

	if strings.ContainsAny(out, "\x01\x02") {
		t.Fatalf("output contains raw markers: %q", out)
	}
	if lw := lipgloss.Width(out); lw != 10 {
		t.Fatalf("width = %d, want exactly 10: %q", lw, out)
	}
	if !strings.Contains(out, "bbbb") {
		t.Fatalf("surviving (bisected) highlight run missing: %q", out)
	}
	if strings.Contains(out, "bbbbbbbbbb") {
		t.Fatalf("full highlight run should have been truncated: %q", out)
	}
	if strings.Contains(out, "cccc") {
		t.Fatalf("text past the cut should have been dropped: %q", out)
	}
}

// TestRenderSnippetCJK exercises wide (2-cell) runes: truncation must be
// cell-aware (not rune-count-aware, like truncPlain) so the result never
// overshoots `max` cells, and must never panic on multi-byte rune slicing.
func TestRenderSnippetCJK(t *testing.T) {
	// plain = 10 CJK runes (20 cells): "漢字漢字漢字漢字漢字"; highlight the
	// middle two.
	raw := "漢字漢字\x01漢字\x02漢字漢字"
	for _, max := range []int{1, 2, 9, 10, 11, 19, 20, 30} {
		out := renderSnippet(raw, max)
		if strings.ContainsAny(out, "\x01\x02") {
			t.Fatalf("max=%d: output contains raw markers: %q", max, out)
		}
		if lw := lipgloss.Width(out); lw > max {
			t.Fatalf("max=%d: width %d exceeds max: %q", max, lw, out)
		}
	}
	// Comfortably wide: no truncation, full text (sans markers) present.
	out := renderSnippet(raw, 100)
	if !strings.Contains(out, "漢字漢字漢字漢字漢字") {
		t.Fatalf("untruncated CJK text missing: %q", out)
	}
}

func TestRenderSnippetMaxZeroOrNegative(t *testing.T) {
	if out := renderSnippet("anything", 0); out != "" {
		t.Fatalf("max=0: got %q, want empty", out)
	}
	if out := renderSnippet("anything", -5); out != "" {
		t.Fatalf("max<0: got %q, want empty", out)
	}
}

func TestRenderSnippetEmpty(t *testing.T) {
	if out := renderSnippet("", 20); out != "" {
		t.Fatalf("empty raw: got %q, want empty", out)
	}
}
