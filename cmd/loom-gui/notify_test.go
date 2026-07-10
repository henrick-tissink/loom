package main

import "testing"

func TestNotifier_needsYou_single(t *testing.T) {
	var gotT, gotB string
	n := &notifier{run: func(title, body string) { gotT, gotB = title, body }}
	n.needsYou([]string{"loom · fix the walker"})
	if gotT != "loom" || gotB != "loom · fix the walker needs you" {
		t.Fatalf("got title=%q body=%q", gotT, gotB)
	}
}

func TestNotifier_needsYou_multiple(t *testing.T) {
	var gotB string
	n := &notifier{run: func(_, body string) { gotB = body }}
	n.needsYou([]string{"a", "b", "c"})
	if gotB != "3 sessions need you" {
		t.Fatalf("got %q", gotB)
	}
}

func TestNotifier_needsYou_emptyNoop(t *testing.T) {
	called := false
	n := &notifier{run: func(_, _ string) { called = true }}
	n.needsYou(nil)
	if called {
		t.Fatal("must not fire for empty list")
	}
}

func TestASQuote_escapes(t *testing.T) {
	if got := asQuote(`a"b\c`); got != `"a\"b\\c"` {
		t.Fatalf("got %s", got)
	}
	if got := asQuote("line1\nline2"); got != `"line1 line2"` {
		t.Fatalf("got %s", got)
	}
}
