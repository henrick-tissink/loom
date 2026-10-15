package discipline

import (
	"fmt"
	"strings"
)

// UnifiedDiff renders a line diff of a → b with three lines of context,
// enough for the gate to show what a settings.json or CLAUDE.md write
// will change. Files here are small; the LCS table is quadratic and
// capped, beyond which a summary line is returned instead.
func UnifiedDiff(name, a, b string) string {
	if a == b {
		return ""
	}
	al := splitLines(a)
	bl := splitLines(b)
	const limit = 4000
	if len(al) > limit || len(bl) > limit {
		return fmt.Sprintf("--- %s\n+++ %s\n(diff too large: %d → %d lines)\n", name, name, len(al), len(bl))
	}
	// LCS lengths.
	n, m := len(al), len(bl)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	type op struct {
		kind byte // ' ', '-', '+'
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case al[i] == bl[j]:
			ops = append(ops, op{' ', al[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, op{'-', al[i]})
			i++
		default:
			ops = append(ops, op{'+', bl[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', al[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', bl[j]})
	}
	// Keep changed lines plus three lines of context on each side.
	const ctx = 3
	show := make([]bool, len(ops))
	for k, o := range ops {
		if o.kind != ' ' {
			for d := -ctx; d <= ctx; d++ {
				if k+d >= 0 && k+d < len(ops) {
					show[k+d] = true
				}
			}
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", name, name)
	gap := false
	for k, o := range ops {
		if !show[k] {
			gap = true
			continue
		}
		if gap {
			sb.WriteString("@@\n")
			gap = false
		}
		sb.WriteByte(o.kind)
		sb.WriteString(o.text)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
