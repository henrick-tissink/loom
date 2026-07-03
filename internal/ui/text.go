package ui

import (
	"fmt"
	"strings"
	"time"
)

// humanAge renders a compact age ("4s","2m","1h","2d"); blank when unset.
func humanAge(now time.Time, unix int64) string {
	if unix <= 0 {
		return ""
	}
	d := now.Unix() - unix
	if d < 0 {
		d = 0
	}
	switch {
	case d < 60:
		return fmt.Sprintf("%ds", d)
	case d < 3600:
		return fmt.Sprintf("%dm", d/60)
	case d < 86400:
		return fmt.Sprintf("%dh", d/3600)
	default:
		return fmt.Sprintf("%dd", d/86400)
	}
}

// truncPlain truncates PLAIN (unstyled) text to max runes, ellipsizing.
// Styling must happen after truncation — never slice styled strings.
func truncPlain(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// padPlain right-pads plain text to w runes; never truncates.
func padPlain(s string, w int) string {
	if n := w - len([]rune(s)); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// humanTokens renders a compact token count ("640","82k","1.2M"); blank ≤0.
func humanTokens(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
}

// padLeft left-pads plain text to w runes; never truncates.
func padLeft(s string, w int) string {
	if n := w - len([]rune(s)); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}
