package ui

import (
	"testing"
	"time"
)

func TestHumanAge(t *testing.T) {
	now := time.Unix(100_000, 0)
	cases := map[int64]string{
		0:               "", // unset → blank
		-5:              "", // negative source → blank
		100_000 - 4:     "4s",
		100_000 - 59:    "59s",
		100_000 - 60:    "1m",
		100_000 - 3599:  "59m",
		100_000 - 3600:  "1h",
		100_000 - 86399: "23h",
		100_000 - 86400: "1d",
		100_000 + 50:    "0s", // future timestamp clamps to zero
	}
	for unix, want := range cases {
		if got := humanAge(now, unix); got != want {
			t.Errorf("humanAge(%d) = %q, want %q", unix, got, want)
		}
	}
}

func TestTruncPad(t *testing.T) {
	if got := truncPlain("parallax", 12); got != "parallax" {
		t.Errorf("no-trunc = %q", got)
	}
	if got := truncPlain("trend-wood-consult", 12); got != "trend-wood-…" {
		t.Errorf("trunc = %q", got)
	}
	if got := truncPlain("abc", 1); got != "…" {
		t.Errorf("trunc1 = %q", got)
	}
	if got := truncPlain("abc", 0); got != "" {
		t.Errorf("trunc0 = %q", got)
	}
	if got := padPlain("ab", 4); got != "ab  " {
		t.Errorf("pad = %q", got)
	}
	if got := padPlain("abcde", 4); got != "abcde" {
		t.Errorf("pad-over = %q (padPlain never truncates)", got)
	}
}

func TestHumanTokens(t *testing.T) {
	cases := map[int64]string{0: "", -1: "", 640: "640", 1000: "1k", 80612: "80k", 823_400: "823k", 1_200_000: "1.2M"}
	for n, want := range cases {
		if got := humanTokens(n); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestPadLeft(t *testing.T) {
	if got := padLeft("82k", 4); got != " 82k" {
		t.Errorf("padLeft = %q", got)
	}
	if got := padLeft("1.0M", 4); got != "1.0M" {
		t.Errorf("padLeft exact = %q", got)
	}
	if got := padLeft("abcde", 4); got != "abcde" {
		t.Errorf("padLeft never truncates: %q", got)
	}
}
