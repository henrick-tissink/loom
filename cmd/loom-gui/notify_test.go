package main

import (
	"testing"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

func TestNotifier_needsYou_single(t *testing.T) {
	var gotT, gotB string
	n := &notifier{run: func(title, body string) { gotT, gotB = title, body }}
	n.needsYou([]string{"loom · fix the walker"}, 0)
	if gotT != "loom" || gotB != "loom · fix the walker needs you" {
		t.Fatalf("got title=%q body=%q", gotT, gotB)
	}
}

func TestNotifier_needsYou_multiple(t *testing.T) {
	var gotB string
	n := &notifier{run: func(_, body string) { gotB = body }}
	n.needsYou([]string{"a", "b", "c"}, 0)
	if gotB != "3 sessions need you" {
		t.Fatalf("got %q", gotB)
	}
}

func TestNotifier_needsYou_emptyNoop(t *testing.T) {
	called := false
	n := &notifier{run: func(_, _ string) { called = true }}
	n.needsYou(nil, 0)
	if called {
		t.Fatal("must not fire for empty list")
	}
}

// liveRow is a Live entry with just the fields needsYouLabels reads.
func liveRow(name, label, title, cwd string) status.Row {
	return status.Row{SessionRow: store.SessionRow{Name: name, ProjectLabel: label, Cwd: cwd},
		Title: title}
}

// TestNeedsYouLabels covers the join the engine no longer does for us
// (spec §4): names in, display strings out, byte-identical to what the
// engine used to pre-render.
func TestNeedsYouLabels(t *testing.T) {
	tests := []struct {
		name string
		snap status.Snapshot
		want []string
	}{
		{
			name: "label and title",
			snap: status.Snapshot{
				NewlyNeedsYou: []string{"loom-a"},
				Live:          []status.Row{liveRow("loom-a", "loom", "fix the walker", "/w/loom")},
			},
			want: []string{"loom · fix the walker"},
		},
		{
			name: "untitled session renders the bare label",
			snap: status.Snapshot{
				NewlyNeedsYou: []string{"loom-a"},
				Live:          []status.Row{liveRow("loom-a", "loom", "", "/w/loom")},
			},
			want: []string{"loom"},
		},
		{
			name: "only the flipped sessions, in engine order",
			snap: status.Snapshot{
				NewlyNeedsYou: []string{"loom-b", "loom-a"},
				Live: []status.Row{
					liveRow("loom-a", "one", "", "/w/one"),
					liveRow("loom-b", "two", "", "/w/two"),
					liveRow("loom-c", "three", "", "/w/three"),
				},
			},
			want: []string{"two", "one"},
		},
		{
			// the collision that motivated the shape change: identical
			// rendering, distinct identities — the count is still right.
			name: "same-basename repos in different projects",
			snap: status.Snapshot{
				NewlyNeedsYou: []string{"loom-a", "loom-b"},
				Live: []status.Row{
					liveRow("loom-a", "checkout", "", "/w/alpha/checkout"),
					liveRow("loom-b", "checkout", "", "/w/beta/checkout"),
				},
			},
			want: []string{"checkout", "checkout"},
		},
		{
			// cannot arise from a real poll; must degrade, not announce an
			// empty banner.
			name: "name with no Live row is dropped",
			snap: status.Snapshot{NewlyNeedsYou: []string{"loom-gone"}},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := needsYouLabels(tt.snap, nil)
			if len(got) != len(tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %q, want %q", got, tt.want)
				}
			}
		})
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
