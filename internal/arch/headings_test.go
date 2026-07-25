package arch

import (
	"strings"
	"testing"
)

func TestHeadingsIgnoresFencesAndStripsFormatting(t *testing.T) {
	src := "# Top\nintro\n" +
		"```\n# not a heading\n```\n" +
		"## `Foo()` and **bold** [link](x)\nbody one\n" +
		"### deep\ndeeper\n" +
		"## Second\nbody two\n"
	hs := Headings(src)
	if len(hs) != 4 {
		t.Fatalf("headings = %d, want 4 (fenced # ignored): %+v", len(hs), hs)
	}
	if strings.ContainsAny(hs[1].Text, "`*[]") {
		t.Fatalf("heading text not stripped of markdown: %q", hs[1].Text)
	}
	if !strings.Contains(hs[1].Text, "Foo()") || !strings.Contains(hs[1].Text, "bold") || !strings.Contains(hs[1].Text, "link") {
		t.Fatalf("heading text missing content: %q", hs[1].Text)
	}
	// The formatted H2's body spans its own text and the deeper H3, and stops at
	// the next H2.
	if !strings.Contains(hs[1].Body, "body one") || !strings.Contains(hs[1].Body, "deeper") || strings.Contains(hs[1].Body, "body two") {
		t.Fatalf("H2 body boundary wrong: %q", hs[1].Body)
	}
	if hs[3].Text != "Second" {
		t.Fatalf("last heading = %q, want Second", hs[3].Text)
	}
}
