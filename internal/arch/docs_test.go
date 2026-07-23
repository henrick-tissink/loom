package arch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/store"
)

// --- fixtures ---

func write(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func res(ps []store.Project, repos []store.ProjectRepo) *projects.Resolver {
	return projects.NewResolver(ps, repos)
}

func oneProject(root string, mods ...func(*store.Project)) []store.Project {
	p := store.Project{Root: root, Name: "P", Origin: "discovered"}
	for _, m := range mods {
		m(&p)
	}
	return []store.Project{p}
}

func titles(set Set) []string {
	var out []string
	for _, d := range set.Documents {
		out = append(out, d.Title)
	}
	return out
}

// --- §4.2 containment ---

// Containment is a SECURITY boundary. Every case here is hostile input in the
// shape a manifest can legally take, and each must be refused VISIBLY.
func TestContainment(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Innostream")
	outside := filepath.Join(tmp, "other-client")

	write(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Inside\n")
	write(t, filepath.Join(root, "docs", "notes.txt"), "not markdown")
	write(t, filepath.Join(outside, "secrets.md"), "# Client secrets\n")

	// A symlink out of the tree — the obvious escape, and invisible to any
	// purely lexical check.
	if err := os.Symlink(filepath.Join(outside, "secrets.md"), filepath.Join(root, "docs", "leak.md")); err != nil {
		t.Fatal(err)
	}

	r := res(oneProject(root), nil)

	tests := []struct {
		name     string
		path     string
		wantRule string
	}{
		{"plain relative traversal", "../other-client/secrets.md", "path is outside every project"},
		{"deep traversal", "docs/../../other-client/secrets.md", "path is outside every project"},
		{"absolute escape", filepath.Join(outside, "secrets.md"), "path is outside every project"},
		{"symlink escape", "docs/leak.md", "path is outside every project"},
		{"non-markdown", "docs/notes.txt", "not a .md file"},
		// A path that does not exist cannot be proven contained, so it is
		// refused too — and the refusal says which rule it broke.
		{"nonexistent absolute path", "/etc/ssh/ssh_config.md", "unreadable"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := Documents(r, Request{Root: root, Manifest: true, Declared: []Declared{
				{Kind: KindArchitecture, Path: tc.path},
			}}, nil)
			if len(set.Documents) != 0 {
				t.Fatalf("admitted a document it must refuse: %#v", set.Documents[0].Path)
			}
			if len(set.Refusals) != 1 {
				t.Fatalf("refusals = %#v; a refusal must be VISIBLE, never silent", set.Refusals)
			}
			if !strings.HasPrefix(set.Refusals[0].Rule, tc.wantRule) {
				t.Fatalf("rule = %q, want %q", set.Refusals[0].Rule, tc.wantRule)
			}
			if set.Refusals[0].Path == "" {
				t.Fatal("refusal must name the path it refused")
			}
		})
	}

	t.Run("a contained document is admitted", func(t *testing.T) {
		set := Documents(r, Request{Root: root, Manifest: true, Declared: []Declared{
			{Kind: KindArchitecture, Path: "docs/ARCHITECTURE.md"},
		}}, nil)
		if len(set.Documents) != 1 || len(set.Refusals) != 0 {
			t.Fatalf("set = %#v / %#v", titles(set), set.Refusals)
		}
	})
}

// An out-of-root member repo MUST be admitted: slice 1 explicitly allows a
// project to own a repo outside its root, and the resolver attributes it to the
// owning project all the same.
func TestContainmentAdmitsOutOfRootRepo(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Innostream")
	repo := filepath.Join(tmp, "elsewhere", "ballista")
	write(t, filepath.Join(root, "README.md"), "x")
	write(t, filepath.Join(repo, "docs", "ARCHITECTURE.md"), "# Ballista\n")

	r := res(oneProject(root), []store.ProjectRepo{{Path: repo, ProjectRoot: root, Label: "Innostream/ballista"}})

	set := Documents(r, Request{Root: root, Repos: []string{repo}}, nil)
	if len(set.Documents) != 1 {
		t.Fatalf("out-of-root member repo not admitted: %#v %#v", titles(set), set.Refusals)
	}
	if set.Documents[0].Title != "Ballista" {
		t.Fatalf("title = %q", set.Documents[0].Title)
	}
	if set.Documents[0].Rel != filepath.Join("docs", "ARCHITECTURE.md") {
		t.Fatalf("rel = %q", set.Documents[0].Rel)
	}
}

// The exact live shape slice 1 §9 pins: `…/HappyPay/HappyPay` is a raw string
// prefix of five real siblings. A raw-prefix containment check would render a
// different project's documents on this project's overview.
func TestContainmentSiblingPrefix(t *testing.T) {
	tmp := t.TempDir()
	mine := filepath.Join(tmp, "HappyPay", "HappyPay")
	sibling := filepath.Join(tmp, "HappyPay", "HappyPayCoreApi")
	write(t, filepath.Join(mine, "docs", "ARCHITECTURE.md"), "# Mine\n")
	write(t, filepath.Join(sibling, "docs", "ARCHITECTURE.md"), "# Sibling\n")

	ps := []store.Project{
		{Root: mine, Name: "HappyPay"},
		{Root: sibling, Name: "HappyPayCoreApi"},
	}
	set := Documents(res(ps, nil), Request{Root: mine, Manifest: true, Declared: []Declared{
		{Kind: KindArchitecture, Path: filepath.Join(sibling, "docs", "ARCHITECTURE.md")},
	}}, nil)

	if len(set.Documents) != 0 {
		t.Fatalf("sibling-prefix document admitted: %#v", set.Documents[0].Path)
	}
	if len(set.Refusals) != 1 || set.Refusals[0].Rule != "path is outside this project" {
		t.Fatalf("refusals = %#v", set.Refusals)
	}
}

func TestOversizeDocumentIsTruncatedVisibly(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "P")
	big := strings.Repeat("lorem ipsum dolor sit amet\n", (MaxDocBytes/27)+5000)
	write(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Big\n\n"+big)

	set := Documents(res(oneProject(root), nil), Request{Root: root}, nil)
	if len(set.Documents) != 1 {
		t.Fatalf("documents = %d", len(set.Documents))
	}
	d := set.Documents[0]
	if !d.Truncated {
		t.Fatal("oversize document not marked truncated")
	}
	last := d.Blocks[len(d.Blocks)-1]
	if PlainText(last.Inline) != TruncationMarker {
		t.Fatalf("last block = %q; truncation must be VISIBLE", PlainText(last.Inline))
	}
	// The head is rendered, not dropped.
	if d.Blocks[0].Kind != BlockHeading || PlainText(d.Blocks[0].Inline) != "Big" {
		t.Fatalf("head not rendered: %#v", d.Blocks[0])
	}
}

func TestUnreadableAndIrregularFilesRefuse(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "P")
	write(t, filepath.Join(root, "README.md"), "x")
	r := res(oneProject(root), nil)

	set := Documents(r, Request{Root: root, Manifest: true, Declared: []Declared{
		{Kind: KindArchitecture, Path: "docs/nope.md"},
	}}, nil)
	if len(set.Refusals) != 1 || !strings.HasPrefix(set.Refusals[0].Rule, "unreadable") {
		t.Fatalf("refusals = %#v", set.Refusals)
	}
}

// --- §4.1 declared first, discovered second ---

func TestDeclaredBeforeDiscovered(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "P")
	write(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Discovered arch\n")
	write(t, filepath.Join(root, "docs", "decisions", "0001-a.md"), "# Discovered decision\n")
	write(t, filepath.Join(root, "docs", "atlas", "OUTLINE.md"), "# Atlas outline\n")

	r := res(oneProject(root), nil)

	t.Run("declared entries come first, in manifest order", func(t *testing.T) {
		set := Documents(r, Request{Root: root, Declared: []Declared{
			{Kind: KindArchitecture, Path: "docs/atlas/OUTLINE.md"},
			{Kind: KindContract, Path: "docs/ARCHITECTURE.md", Title: "Declared arch"},
		}}, nil)
		got := titles(set)
		// Declared order is manifest order; the second declared entry is the
		// same file the convention scan would find, so the scan must not emit
		// a duplicate of it.
		want := []string{"Atlas outline", "Declared arch", "Discovered decision"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
		if !set.Documents[0].Declared || !set.Documents[1].Declared || set.Documents[2].Declared {
			t.Fatalf("Declared flags wrong: %#v", got)
		}
		// The declared kind wins over the convention directory's kind.
		if set.Documents[1].Kind != KindContract {
			t.Fatalf("declared kind lost: %q", set.Documents[1].Kind)
		}
	})

	t.Run("a manifest suppresses the convention scan entirely", func(t *testing.T) {
		set := Documents(r, Request{Root: root, Manifest: true, Declared: []Declared{
			{Kind: KindArchitecture, Path: "docs/atlas/OUTLINE.md"},
		}}, nil)
		if got := titles(set); !reflect.DeepEqual(got, []string{"Atlas outline"}) {
			t.Fatalf("titles = %v; the scan must not run when a manifest declared the set", got)
		}
	})

	t.Run("no manifest and no declarations still fills the block", func(t *testing.T) {
		set := Documents(r, Request{Root: root}, nil)
		if got := titles(set); !reflect.DeepEqual(got, []string{"Discovered arch", "Discovered decision"}) {
			t.Fatalf("titles = %v", got)
		}
	})
}

// The convention scan is an ordered, explicitly enumerated, first-match rule
// set — slice 1 §3's discipline — not a heuristic crawl.
func TestConventionScanKindsAndOrder(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "P")
	write(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# One\n")
	write(t, filepath.Join(root, "docs", "architecture", "b.md"), "# Two\n")
	write(t, filepath.Join(root, "docs", "decisions", "0001.md"), "# Three\n")
	write(t, filepath.Join(root, "docs", "adr", "0002.md"), "# Four\n")
	write(t, filepath.Join(root, "ADR", "0003.md"), "# Five\n")
	// Not in the enumerated set, and not recursive beyond it.
	write(t, filepath.Join(root, "docs", "guide.md"), "# Not scanned\n")
	write(t, filepath.Join(root, "docs", "decisions", "old", "0000.md"), "# Not scanned\n")

	set := Documents(res(oneProject(root), nil), Request{Root: root}, nil)

	wantTitles := []string{"One", "Two", "Three", "Four", "Five"}
	if got := titles(set); !reflect.DeepEqual(got, wantTitles) {
		t.Fatalf("titles = %v, want %v", got, wantTitles)
	}
	wantKinds := []DocKind{KindArchitecture, KindArchitecture, KindDecision, KindDecision, KindDecision}
	for i, d := range set.Documents {
		if d.Kind != wantKinds[i] {
			t.Fatalf("kind[%d] = %q, want %q", i, d.Kind, wantKinds[i])
		}
	}
}

func TestDeclaredKinds(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "P")
	write(t, filepath.Join(root, "a.md"), "# A\n")
	r := res(oneProject(root), nil)

	tests := []struct {
		name     string
		kind     DocKind
		want     DocKind
		wantWarn string
	}{
		{"architecture", KindArchitecture, KindArchitecture, ""},
		{"decision", KindDecision, KindDecision, ""},
		{"contract", KindContract, KindContract, ""},
		{"empty falls back with a warning", "", KindArchitecture, "declared no kind"},
		{"unknown falls back with a warning", "diagram", KindArchitecture, "unknown document kind"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := Documents(r, Request{Root: root, Manifest: true, Declared: []Declared{
				{Kind: tc.kind, Path: "a.md", ID: "ledger-v2"},
			}}, nil)
			if len(set.Documents) != 1 {
				t.Fatalf("documents = %#v", set.Refusals)
			}
			d := set.Documents[0]
			if d.Kind != tc.want {
				t.Fatalf("kind = %q, want %q", d.Kind, tc.want)
			}
			if d.ID != "ledger-v2" {
				t.Fatalf("id = %q", d.ID)
			}
			if tc.wantWarn == "" {
				if len(d.Warnings) != 0 {
					t.Fatalf("warnings = %v", d.Warnings)
				}
			} else if len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], tc.wantWarn) {
				t.Fatalf("warnings = %v, want one containing %q", d.Warnings, tc.wantWarn)
			}
		})
	}
}

// --- §4.3 front matter ---

func TestFrontMatter(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		check func(t *testing.T, d Document)
	}{
		{
			name: "full block",
			body: "---\ntitle: Event bus\nstatus: accepted\ndate: 2026-03-04\nsupersedes: 0002\nid: 0003\n---\n\n# Body heading\n\n## Consequences\n\nThe ledger publishes.\n",
			check: func(t *testing.T, d Document) {
				if d.Title != "Event bus" || d.Meta.Status != "accepted" || d.Meta.Date != "2026-03-04" {
					t.Fatalf("meta = %#v", d.Meta)
				}
				if d.Meta.Supersedes != "0002" || d.ID != "0003" {
					t.Fatalf("meta = %#v id=%q", d.Meta, d.ID)
				}
				if d.Meta.DateFromMTime {
					t.Fatal("date was declared; it must not be labelled as mtime")
				}
				if d.Meta.Consequence != "The ledger publishes." {
					t.Fatalf("consequence = %q", d.Meta.Consequence)
				}
				// The front matter itself must not reach the token tree.
				if d.Blocks[0].Kind != BlockHeading || PlainText(d.Blocks[0].Inline) != "Body heading" {
					t.Fatalf("front matter leaked into the body: %#v", d.Blocks[0])
				}
			},
		},
		{
			name: "missing status is stated, never invented",
			body: "---\ntitle: T\ndate: 2026-03-04\n---\n\nbody\n",
			check: func(t *testing.T, d Document) {
				if d.Meta.StatusKnown {
					t.Fatal("StatusKnown true with no status: line")
				}
				if d.Meta.StatusText() != "status unknown" {
					t.Fatalf("status text = %q", d.Meta.StatusText())
				}
			},
		},
		{
			name: "empty status counts as unknown",
			body: "---\nstatus:\n---\n\nbody\n",
			check: func(t *testing.T, d Document) {
				if d.Meta.StatusKnown || d.Meta.StatusText() != "status unknown" {
					t.Fatalf("meta = %#v", d.Meta)
				}
			},
		},
		{
			name: "missing date falls back to mtime, labelled",
			body: "---\ntitle: T\nstatus: accepted\n---\n\nbody\n",
			check: func(t *testing.T, d Document) {
				if !d.Meta.DateFromMTime {
					t.Fatal("missing date must be labelled as mtime")
				}
				if d.ModTime == 0 {
					t.Fatal("mtime not carried, so the card has nothing to label")
				}
			},
		},
		{
			name: "unterminated block renders as body text plus a warning",
			body: "---\ntitle: T\nstatus: accepted\n\n# Heading\n",
			check: func(t *testing.T, d Document) {
				if len(d.Warnings) == 0 {
					t.Fatal("malformed front matter must warn")
				}
				if d.Meta.Title != "" || d.Meta.StatusKnown {
					t.Fatalf("meta parsed from a malformed block: %#v", d.Meta)
				}
				// The unparsed block renders as ordinary body text: a `---`
				// rule followed by the lines themselves.
				if len(d.Blocks) != 3 || d.Blocks[0].Kind != BlockRule ||
					PlainText(d.Blocks[1].Inline) != "title: T\nstatus: accepted" {
					t.Fatalf("body = %#v", d.Blocks)
				}
			},
		},
		{
			name: "nested value is outside the subset",
			body: "---\ntitle: T\nauthors:\n  - a\n---\n\nbody\n",
			check: func(t *testing.T, d Document) {
				if len(d.Warnings) == 0 {
					t.Fatal("nesting must warn rather than being silently dropped")
				}
				if d.Meta.Title != "" {
					t.Fatalf("meta = %#v; a malformed block is body text", d.Meta)
				}
			},
		},
		{
			name: "unsupported key warns and is ignored",
			body: "---\ntitle: T\nauthor: h\n---\n\nbody\n",
			check: func(t *testing.T, d Document) {
				if len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], `key "author"`) {
					t.Fatalf("warnings = %v", d.Warnings)
				}
			},
		},
		{
			name: "no front matter at all",
			body: "# Just a heading\n",
			check: func(t *testing.T, d Document) {
				if len(d.Warnings) != 0 {
					t.Fatalf("warnings = %v", d.Warnings)
				}
				if d.Title != "Just a heading" {
					t.Fatalf("title = %q", d.Title)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "P")
			write(t, filepath.Join(root, "docs", "decisions", "0003.md"), tc.body)
			set := Documents(res(oneProject(root), nil), Request{Root: root}, nil)
			if len(set.Documents) != 1 {
				t.Fatalf("documents = %d (%#v)", len(set.Documents), set.Refusals)
			}
			tc.check(t, set.Documents[0])
		})
	}
}

func TestTitleFallbackChain(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "P")
	write(t, filepath.Join(root, "docs", "decisions", "0009-no-title.md"), "just prose, no heading\n")
	set := Documents(res(oneProject(root), nil), Request{Root: root}, nil)
	if set.Documents[0].Title != "0009-no-title.md" {
		t.Fatalf("title = %q; a document must never render untitled", set.Documents[0].Title)
	}
}

// --- §3.1 visibility: the gate is on the payload, and it fails closed ---

func TestVisibilityGate(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Innostream")
	other := filepath.Join(tmp, "Public")
	write(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Atlas re-architecture\n\nledger-v2 contract\n")
	write(t, filepath.Join(other, "docs", "ARCHITECTURE.md"), "# Public\n")

	tests := []struct {
		name       string
		ps         []store.Project
		root       string
		nilRes     bool
		wantHidden bool
	}{
		{
			name: "visible project renders",
			ps:   []store.Project{{Root: root, Name: "Innostream"}, {Root: other, Name: "Public"}},
			root: root,
		},
		{
			name:       "hidden project returns the empty payload",
			ps:         []store.Project{{Root: root, Name: "Innostream", Hidden: true}, {Root: other, Name: "Public"}},
			root:       root,
			wantHidden: true,
		},
		{
			name:       "another project's solo suppresses this one",
			ps:         []store.Project{{Root: root, Name: "Innostream"}, {Root: other, Name: "Public", Solo: true}},
			root:       root,
			wantHidden: true,
		},
		{
			name: "solo on this project does not suppress it",
			ps:   []store.Project{{Root: root, Name: "Innostream", Solo: true}, {Root: other, Name: "Public"}},
			root: root,
		},
		{
			name:       "unknown root fails closed even with nothing hidden",
			ps:         []store.Project{{Root: other, Name: "Public"}},
			root:       root,
			wantHidden: true,
		},
		{
			name:       "empty root fails closed",
			ps:         []store.Project{{Root: root, Name: "Innostream"}},
			root:       "",
			wantHidden: true,
		},
		{
			name:       "nil resolver fails closed",
			ps:         []store.Project{{Root: root, Name: "Innostream"}},
			root:       root,
			nilRes:     true,
			wantHidden: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r *projects.Resolver
			if !tc.nilRes {
				r = res(tc.ps, nil)
			}
			set := Documents(r, Request{Root: tc.root, Declared: []Declared{
				{Kind: KindArchitecture, Path: "docs/ARCHITECTURE.md", Title: "Atlas outline"},
			}}, nil)

			if !tc.wantHidden {
				if set.Hidden || len(set.Documents) == 0 {
					t.Fatalf("expected a populated payload, got %#v", set)
				}
				return
			}
			// Field by field: no rev, no title, no counts, no paths, no error.
			if !reflect.DeepEqual(set, Set{Hidden: true}) {
				t.Fatalf("hidden payload carries more than the marker: %#v", set)
			}
		})
	}
}

// The strongest available form of the leak test: substring search over the
// MARSHALLED bytes, not a struct walk. A field added later that carries a path
// or a body fails this without anyone remembering to update it.
func TestHiddenPayloadLeaksNothingWhenMarshalled(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Innostream")
	repo := filepath.Join(root, "bankenstein")
	write(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Atlas re-architecture\n\nSECRETBODY ledger\n")
	write(t, filepath.Join(repo, "docs", "decisions", "0003-event-bus.md"), "---\ntitle: Event bus\n---\n\nSECRETBODY\n")

	req := Request{Root: root, Repos: []string{repo}, Declared: []Declared{
		{Kind: KindArchitecture, Path: "docs/ARCHITECTURE.md", Title: "Atlas outline"},
	}}

	visible := Documents(res(oneProject(root), []store.ProjectRepo{{Path: repo, ProjectRoot: root}}), req, nil)
	if len(visible.Documents) == 0 {
		t.Fatal("fixture produced nothing to hide")
	}

	hidden := Documents(res(oneProject(root, func(p *store.Project) { p.Hidden = true }),
		[]store.ProjectRepo{{Path: repo, ProjectRoot: root}}), req, nil)

	raw, err := json.Marshal(hidden)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"SECRETBODY", "Atlas", "Innostream", "bankenstein", "ARCHITECTURE.md", "Event bus", tmp} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("hidden payload leaks %q: %s", needle, raw)
		}
	}

	// Round-trip: unhiding restores a byte-identical payload (slice 1's
	// solo↔hidden restore discipline).
	again := Documents(res(oneProject(root), []store.ProjectRepo{{Path: repo, ProjectRoot: root}}), req, nil)
	a, _ := json.Marshal(visible)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatal("unhide did not restore an identical payload")
	}
}

// §3.1.2: Open's argument is a path, not a root, so it attributes before it
// admits — and a refusal must never echo a path from a hidden project.
func TestOpenAttributesBeforeItAdmits(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Innostream")
	doc := write(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Atlas\n")

	t.Run("visible", func(t *testing.T) {
		d, _, err := Open(res(oneProject(root), nil), doc, nil)
		if err != nil {
			t.Fatal(err)
		}
		if d.Title != "Atlas" {
			t.Fatalf("title = %q", d.Title)
		}
	})

	t.Run("hidden project refuses without echoing the path", func(t *testing.T) {
		r := res(oneProject(root, func(p *store.Project) { p.Hidden = true }), nil)
		d, ref, err := Open(r, doc, nil)
		if !errors.Is(err, ErrHidden) {
			t.Fatalf("err = %v, want ErrHidden", err)
		}
		if !reflect.DeepEqual(d, Document{}) || !reflect.DeepEqual(ref, Refusal{}) {
			t.Fatalf("hidden Open returned content: %#v / %#v", d, ref)
		}
	})

	t.Run("unattributable path fails closed", func(t *testing.T) {
		outside := write(t, filepath.Join(tmp, "loose", "x.md"), "# Loose\n")
		_, ref, err := Open(res(oneProject(root), nil), outside, nil)
		if !Refused(err) {
			t.Fatalf("err = %v, want a refusal", err)
		}
		if ref.Rule != "path is outside every project" {
			t.Fatalf("rule = %q", ref.Rule)
		}
	})
}

// A document that symlinks into a DIFFERENT, hidden project must vanish
// silently rather than produce a refusal card naming that project's path.
func TestDeclaredDocumentResolvingIntoHiddenProjectIsSilent(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Visible")
	secret := filepath.Join(tmp, "Client")
	write(t, filepath.Join(secret, "docs", "ARCHITECTURE.md"), "# Client internals\n")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(secret, "docs", "ARCHITECTURE.md"), filepath.Join(root, "docs", "ARCHITECTURE.md")); err != nil {
		t.Fatal(err)
	}

	ps := []store.Project{{Root: root, Name: "Visible"}, {Root: secret, Name: "Client", Hidden: true}}
	set := Documents(res(ps, nil), Request{Root: root, Manifest: true, Declared: []Declared{
		{Kind: KindArchitecture, Path: "docs/ARCHITECTURE.md"},
	}}, nil)

	if len(set.Documents) != 0 {
		t.Fatalf("hidden project's document rendered: %#v", set.Documents)
	}
	raw, _ := json.Marshal(set)
	if strings.Contains(string(raw), "Client") {
		t.Fatalf("refusal echoed a hidden project's path: %s", raw)
	}
}

// --- cache (§4.4: the file is the source of truth; nothing is persisted) ---

func TestCacheKeyedOnSizeAndMTime(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "P")
	path := write(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# One\n")
	r := res(oneProject(root), nil)
	c := NewCache()

	first := Documents(r, Request{Root: root}, c)
	if first.Documents[0].Title != "One" {
		t.Fatalf("title = %q", first.Documents[0].Title)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Same size, same mtime, different bytes: the fingerprint says unchanged,
	// so the cached body is served. This is the `indexed_files` discipline.
	write(t, path, "# Two\n")
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	cached := Documents(r, Request{Root: root}, c)
	if cached.Documents[0].Title != "One" {
		t.Fatalf("cache miss on an unchanged fingerprint: %q", cached.Documents[0].Title)
	}

	// A size change invalidates.
	write(t, path, "# Three and longer\n")
	fresh := Documents(r, Request{Root: root}, c)
	if fresh.Documents[0].Title != "Three and longer" {
		t.Fatalf("stale render after the file changed: %q", fresh.Documents[0].Title)
	}
}

func TestScanIsNeverFatal(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "P")
	write(t, filepath.Join(root, "docs", "ARCHITECTURE.md"), "# Fine\n")
	// A member repo that does not exist at all: the project must still render.
	set := Documents(res(oneProject(root), nil), Request{Root: root, Repos: []string{filepath.Join(tmp, "gone")}}, nil)
	if len(set.Documents) != 1 {
		t.Fatalf("a missing member repo cost the project its documents: %#v", set)
	}
}
