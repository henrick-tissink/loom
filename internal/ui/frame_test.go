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
