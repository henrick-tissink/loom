package main

import (
	"strings"
	"testing"
	"time"
)

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"attach"}, {"kill"}, {"search"}} {
		err := run(args)
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("run(%v) = %v, want a usage error", args, err)
		}
	}
}

func TestSnippetAndAge(t *testing.T) {
	if got := snippet("a \x01hit\x02\n  b"); got != "a [hit] b" {
		t.Errorf("snippet = %q", got)
	}
	now := time.Unix(1_000_000, 0)
	for created, want := range map[int64]string{0: "?", 1_000_000 - 90: "2m", 1_000_000 - 3600*3 - 60*5: "3h05m", 1_000_000 - 86400*2: "2d"} {
		if got := age(now, created); got != want {
			t.Errorf("age(%d) = %q, want %q", created, got, want)
		}
	}
}
