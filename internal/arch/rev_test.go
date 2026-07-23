package arch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

// touch rewrites a file with a distinct mtime. A test that only rewrote the
// bytes could pass on a filesystem with one-second mtime granularity while the
// probe it is testing was blind, so the mtime is set explicitly.
func touch(t *testing.T, path, body string, mod time.Time) {
	t.Helper()
	write(t, path, body)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// Rev is §7.4's whole contract in one table: it must move when the rendered
// set would render differently, and must NOT move otherwise — a probe that
// changed on every tick would refresh documents every 1.5s, which is the cost
// §7.5 forbids, and one that never changed would be the bug it was written to
// fix.
func TestRevMovesOnlyWhenTheSetWould(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name string
		// mutate runs between the two probes. want says whether the rev
		// should differ.
		mutate func(t *testing.T, root string)
		want   bool
	}{
		{
			name:   "nothing changes",
			mutate: func(t *testing.T, root string) {},
			want:   false,
		},
		{
			name: "a document's body changes",
			mutate: func(t *testing.T, root string) {
				touch(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Arch\n\nrewritten\n", base.Add(time.Hour))
			},
			want: true,
		},
		{
			name: "a document is edited to the SAME length",
			// The size half alone cannot see this; the mtime half must.
			mutate: func(t *testing.T, root string) {
				touch(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Brch\n", base.Add(time.Hour))
			},
			want: true,
		},
		{
			name: "a document is rewritten with its ORIGINAL mtime but new length",
			// The mtime half alone cannot see this; the size half must.
			mutate: func(t *testing.T, root string) {
				touch(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Arch, at length\n", base)
			},
			want: true,
		},
		{
			name: "a new decision record appears",
			// The case the probe exists for: no file we already knew about
			// changed, so nothing but a directory read can see it.
			mutate: func(t *testing.T, root string) {
				touch(t, filepath.Join(root, "docs", "decisions", "0002-new.md"), "# Two\n", base)
			},
			want: true,
		},
		{
			name: "a decision record is deleted",
			mutate: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "docs", "decisions", "0001-a.md")); err != nil {
					t.Fatal(err)
				}
			},
			want: true,
		},
		{
			name: "an unrelated non-markdown file changes",
			mutate: func(t *testing.T, root string) {
				touch(t, filepath.Join(root, "docs", "notes.txt"), "changed", base.Add(time.Hour))
			},
			want: false,
		},
		{
			name: "a markdown file outside the convention scan changes",
			mutate: func(t *testing.T, root string) {
				touch(t, filepath.Join(root, "README.md"), "# changed\n", base.Add(time.Hour))
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "Innostream")
			touch(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Arch\n", base)
			touch(t, filepath.Join(root, "docs", "decisions", "0001-a.md"), "# One\n", base)
			touch(t, filepath.Join(root, "docs", "notes.txt"), "notes", base)
			touch(t, filepath.Join(root, "README.md"), "# readme\n", base)

			r := res(oneProject(root), nil)
			req := Request{Root: root}

			before, ok := Rev(r, req)
			if !ok {
				t.Fatal("Rev refused a visible project")
			}
			tc.mutate(t, root)
			after, ok := Rev(r, req)
			if !ok {
				t.Fatal("Rev refused a visible project after mutation")
			}
			if got := before != after; got != tc.want {
				t.Fatalf("rev changed = %v, want %v (before=%d after=%d)", got, tc.want, before, after)
			}
		})
	}
}

// §3.1: the gate runs before any disk access, and a hidden project's rev is a
// constant. A rev that moved would report that a hidden client's files were
// being edited — the leak the marker exists to prevent, one bit at a time.
func TestRevHiddenIsZeroAndConstant(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	root := filepath.Join(t.TempDir(), "Innostream")
	touch(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Arch\n", base)

	r := res(oneProject(root, func(p *store.Project) { p.Hidden = true }), nil)
	req := Request{Root: root}

	before, ok := Rev(r, req)
	if ok || before != 0 {
		t.Fatalf("hidden project: got (%d, %v), want (0, false)", before, ok)
	}
	touch(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Arch, rewritten\n", base.Add(time.Hour))
	after, ok := Rev(r, req)
	if ok || after != 0 {
		t.Fatalf("hidden project after edit: got (%d, %v), want (0, false)", after, ok)
	}
}

// An unattributable root fails CLOSED, matching projectVisible's rule for
// Documents. The frontend supplies the root, so "never heard of it" is a value
// this can actually receive.
func TestRevUnknownRootFailsClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Innostream")
	write(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Arch\n")

	if rev, ok := Rev(res(nil, nil), Request{Root: root}); ok || rev != 0 {
		t.Fatalf("unknown root: got (%d, %v), want (0, false)", rev, ok)
	}
	if rev, ok := Rev(nil, Request{Root: root}); ok || rev != 0 {
		t.Fatalf("nil resolver: got (%d, %v), want (0, false)", rev, ok)
	}
}

// §4.1 holds on the probe as it holds on the payload: with a manifest the
// declared set IS the set, so the convention scan must not contribute. A rev
// that moved for a file the payload does not contain is a refresh that changes
// nothing on screen.
func TestRevWithManifestIgnoresTheConventionScan(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	root := filepath.Join(t.TempDir(), "Innostream")
	touch(t, filepath.Join(root, "docs", "design.md"), "# Declared\n", base)
	touch(t, filepath.Join(root, "docs", "decisions", "0001-a.md"), "# Scanned\n", base)

	r := res(oneProject(root), nil)
	req := Request{Root: root, Manifest: true, Declared: []Declared{{Path: "docs/design.md"}}}

	before, _ := Rev(r, req)
	touch(t, filepath.Join(root, "docs", "decisions", "0001-a.md"), "# Scanned, edited\n", base.Add(time.Hour))
	if after, _ := Rev(r, req); after != before {
		t.Fatal("a scanned file moved the rev of a manifest-declared set")
	}
	touch(t, filepath.Join(root, "docs", "design.md"), "# Declared, edited\n", base.Add(time.Hour))
	if after, _ := Rev(r, req); after == before {
		t.Fatal("a declared file did not move the rev")
	}
}

// A declared document that does not exist yet renders as a refusal today and a
// document tomorrow. Its APPEARANCE must move the rev, which it only does if
// the absent case is fingerprinted rather than skipped.
func TestRevSeesADeclaredDocumentAppear(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	root := filepath.Join(t.TempDir(), "Innostream")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	r := res(oneProject(root), nil)
	req := Request{Root: root, Manifest: true, Declared: []Declared{{Path: "docs/design.md"}}}

	before, _ := Rev(r, req)
	touch(t, filepath.Join(root, "docs", "design.md"), "# Declared\n", base)
	if after, _ := Rev(r, req); after == before {
		t.Fatal("a declared document appearing did not move the rev")
	}
}

// The rev must depend on WHICH file carries a size and mtime, not just on the
// multiset of sizes and mtimes: two documents swapping content is a real change
// and a path-blind fingerprint would miss it. This is what the field
// terminator in mix() buys.
func TestRevIsPathSensitive(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	root := filepath.Join(t.TempDir(), "Innostream")
	a := filepath.Join(root, "docs", "decisions", "0001-a.md")
	b := filepath.Join(root, "docs", "decisions", "0002-b.md")
	touch(t, a, "# aaaa\n", base)
	touch(t, b, "# bb\n", base.Add(time.Hour))

	r := res(oneProject(root), nil)
	req := Request{Root: root}

	before, _ := Rev(r, req)
	touch(t, a, "# bb\n", base.Add(time.Hour))
	touch(t, b, "# aaaa\n", base)
	if after, _ := Rev(r, req); after == before {
		t.Fatal("two documents swapping size and mtime did not move the rev")
	}
}

// Determinism: the same tree probed twice yields the same number, and the
// member-repo order is part of the request rather than a map iteration.
func TestRevIsDeterministic(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Innostream")
	repo := filepath.Join(tmp, "Innostream-api")
	touch(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Arch\n", base)
	touch(t, filepath.Join(repo, "docs", "ARCHITECTURE.md"), "# Api\n", base)

	r := res(oneProject(root), []store.ProjectRepo{{ProjectRoot: root, Path: repo}})
	req := Request{Root: root, Repos: []string{repo}}

	first, _ := Rev(r, req)
	for i := 0; i < 20; i++ {
		if got, _ := Rev(r, req); got != first {
			t.Fatalf("run %d: rev %d, want %d", i, got, first)
		}
	}
	// A member repo's documents are in the rendered set, so they are in the rev.
	touch(t, filepath.Join(repo, "docs", "ARCHITECTURE.md"), "# Api, edited\n", base.Add(time.Hour))
	if after, _ := Rev(r, req); after == first {
		t.Fatal("a member repo's document did not move the rev")
	}
}
