package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestFrameWidthExact(t *testing.T) {
	for _, w := range []int{40, 60, 100} {
		out := frame(w, "LOOM", "4 live · 1 needs you", []string{"hello", ""}, "q quit")
		for i, line := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(line); lw != w {
				t.Errorf("width %d line %d: got %d cells: %q", w, i, lw, line)
			}
		}
	}
}

func TestFrameContainsParts(t *testing.T) {
	out := frame(60, "LOOM", "counts", []string{"body-line"}, "keybar-text")
	for _, want := range []string{"LOOM", "counts", "body-line", "keybar-text", "╭", "╰", "╮", "╯"} {
		if !strings.Contains(out, want) {
			t.Errorf("frame missing %q", want)
		}
	}
}

func TestFrameOverlongBodyLineIsHardClipped(t *testing.T) {
	long := strings.Repeat("x", 500)
	out := frame(40, "T", "", []string{long}, "")
	for _, line := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(line); lw != 40 {
			t.Errorf("overlong body not clipped: %d cells", lw)
		}
	}
}

// TestFrameLongKeybarIsTruncated is the reproduction for the Critical finding:
// a keybar wider than the available bottom-edge space must be ellipsized (not
// overflow the border), and the closing ╯ must still be present at the end
// of an exactly `width`-cell line.
func TestFrameLongKeybarIsTruncated(t *testing.T) {
	keybar := strings.Repeat("k", 60)
	out := frame(40, "LOOM", "", []string{"body"}, keybar)
	lines := strings.Split(out, "\n")
	bottom := lines[len(lines)-1]
	if lw := lipgloss.Width(bottom); lw != 40 {
		t.Fatalf("bottom edge width = %d, want 40: %q", lw, bottom)
	}
	if !strings.HasSuffix(stripAnsi(bottom), "╯") {
		t.Fatalf("bottom edge missing closing ╯: %q", bottom)
	}
	for i, line := range lines {
		if lw := lipgloss.Width(line); lw != 40 {
			t.Errorf("line %d width = %d, want 40: %q", i, lw, line)
		}
	}
}
