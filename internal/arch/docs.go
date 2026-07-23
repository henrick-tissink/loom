// Package arch renders the agent-authored architecture layer — outlines,
// decision records and interface contracts — for the project overview
// (spec docs/superpowers/specs/2026-07-22-orchestration-view-design.md §4).
//
// It renders and analyses. It never authors, never advances a run, and never
// writes into the user's workspace; every entry point here is read-only.
//
// Two properties carry the package:
//
//   - Documents are agent-authored input on a render path, which puts them in
//     the same trust class as transcript content (ARCHITECTURE.md §10). §4.2's
//     containment check is a SECURITY boundary, not tidiness: a `documents[]`
//     entry naming ~/.ssh/id_rsa or ../../other-client/secrets.md must never
//     be read, let alone rendered.
//   - Visibility (§3.1) is enforced on the PAYLOAD, not on the route. The
//     project overview is deliberately reachable while hidden — it is the
//     settings screen that carries Hide/Show — so there is no route-level
//     backstop. The gate below is the only gate.
package arch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/henricktissink/loom/internal/projects"
)

// MaxDocBytes is §4.2's size cap. Over-cap documents render their head with a
// visible truncation marker — the workflow-substitution precedent — rather
// than being dropped, because a dropped document is indistinguishable from a
// missing one and the user would debug the wrong thing.
const MaxDocBytes = 512 * 1024

// TruncationMarker is appended as a final paragraph of an over-cap document.
const TruncationMarker = "…[truncated at 512 KB]"

type DocKind string

const (
	KindArchitecture DocKind = "architecture"
	KindDecision     DocKind = "decision"
	KindContract     DocKind = "contract"
)

// ErrHidden is returned by every entry point for a project the §6 predicate
// suppresses, and for a root that cannot be attributed at all. It carries no
// path, no name and no count: a hidden state that renders differently for a
// project with documents than for one without is itself the leak, in one bit.
var ErrHidden = errors.New("hidden")

// Declared is one `documents[]` entry of a run manifest (§5.3). Path is
// relative to the project root unless absolute; either way it goes through the
// same admission check.
type Declared struct {
	Kind  DocKind
	Path  string
	Title string
	ID    string
}

// Request describes the tree a document set is drawn from.
type Request struct {
	Root  string   // project root, as stored in `projects.root`
	Repos []string // this project's `project_repos.path` entries, in list order

	Declared []Declared

	// Manifest says a run manifest exists for this project. §4.1: discovery is
	// declared first, DISCOVERED SECOND — and the convention scan runs ONLY
	// when there is no manifest. A rendering layer must not guess which
	// markdown file is the architecture when the orchestrator has said.
	Manifest bool
}

// Refusal is a document that was named but not admitted. §4.2 binds these to
// be VISIBLE: the frontend renders one .po-warn card per refusal naming the
// path and the rule it broke. Silently dropping one would be the worst of both
// worlds — no content and no explanation.
type Refusal struct {
	Path string
	Rule string
}

// Set is the whole document payload for one project.
type Set struct {
	// Hidden is §3.1.1's single marker. When true every other field is zero:
	// no count, no title, no path, no error text.
	Hidden bool

	Documents []Document
	Refusals  []Refusal
	// Warnings are non-fatal problems with the scan itself (an unreadable
	// member repo, say). Never fatal — a project with one bad directory must
	// still show its other documents.
	Warnings []string
}

// Document is one rendered file.
type Document struct {
	Kind DocKind
	Path string // absolute, physically resolved
	Rel  string // path relative to the tree that contained it, for display
	Name string // base name
	ID   string // contract id from the manifest, "" otherwise

	Title    string
	Declared bool // came from the manifest, not the convention scan

	Size    int64
	ModTime int64

	Meta Meta // decision front matter; zero for other kinds

	Blocks    []Block
	Outline   []OutlineEntry
	Truncated bool

	// Warnings are per-document, non-fatal, and rendered as chips. Malformed
	// front matter lands here; so does an unknown declared kind.
	Warnings []string

	// body is the raw source between load and finish. It is cleared once the
	// token tree exists so a Set never carries two copies of every document.
	body string
}

// Meta is §4.3's RESTRICTED front-matter subset: a leading `---` block of flat
// `key: value` scalar lines, keys title|status|date|supersedes|id. No nesting,
// no lists, no anchors — a full YAML parser is a dependency and an attack
// surface for a four-key need.
type Meta struct {
	Title      string
	Status     string
	Date       string
	Supersedes string
	ID         string

	// StatusKnown and DateFromMTime encode §4.3's binding rule: MISSING
	// METADATA IS STATED, NEVER INVENTED. With no `status:` the chip reads
	// "status unknown", not "accepted"; with no `date:` the card shows the file
	// mtime, LABELLED as mtime so it is not mistaken for a decision date. A
	// decision-record UI that confidently displays a fabricated "Accepted" is
	// worse than one that displays nothing.
	StatusKnown   bool
	DateFromMTime bool

	// Consequence is the one-line consequence shown on the collapsed card. It
	// is lifted from the document's own "Consequences" section, never
	// summarized: Loom does not paraphrase the user's decisions.
	Consequence string
}

// StatusText is what the chip renders.
func (m Meta) StatusText() string {
	if !m.StatusKnown {
		return "status unknown"
	}
	return m.Status
}

// scanDir is one entry of §4.1's fixed, ordered convention scan. Ordered,
// first-match and explicitly enumerated — slice 1 §3's discipline — not a
// heuristic crawl.
type scanDir struct {
	dir  string // relative to a tree root
	file string // "" means "every *.md in dir"
	kind DocKind
}

var conventionScan = []scanDir{
	{dir: "docs", file: "ARCHITECTURE.md", kind: KindArchitecture},
	{dir: "docs/architecture", kind: KindArchitecture},
	{dir: "docs/decisions", kind: KindDecision},
	{dir: "docs/adr", kind: KindDecision},
	{dir: "ADR", kind: KindDecision},
}

// Documents is the whole of §4 for one project: gate, admit, read, render.
//
// The visibility gate runs FIRST and BEFORE ANY DISK ACCESS. This is not
// defence in depth over an existing route gate — the route is deliberately
// open (§3.1) — so a future entry point added here without this call will pass
// every other test in this package and put a client's paths on a shared
// screen.
func Documents(res *projects.Resolver, req Request, cache *Cache) Set {
	if !projectVisible(res, req.Root) {
		return Set{Hidden: true}
	}

	set := Set{}
	seen := make(map[string]bool)

	// Declared first (§4.1), in manifest order.
	for _, d := range req.Declared {
		path := d.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(req.Root, path)
		}
		doc, ref, err := load(res, path, req.Root, cache)
		if err != nil {
			// A declared document under a project that is itself visible can
			// still resolve into a DIFFERENT, hidden project via a symlink.
			// That is the hidden payload's business, not a refusal card: a
			// refusal would echo the path.
			if errors.Is(err, ErrHidden) {
				continue
			}
			set.Refusals = append(set.Refusals, ref)
			continue
		}
		if seen[doc.Path] {
			continue
		}
		seen[doc.Path] = true
		doc.Declared = true
		doc.ID = d.ID
		doc.Kind, doc.Warnings = declaredKind(d.Kind, doc.Warnings)
		if d.Title != "" {
			doc.Title = d.Title
		}
		doc.Rel = relTo(req, doc.Path)
		finish(&doc)
		set.Documents = append(set.Documents, doc)
	}

	// Discovered second, and ONLY when there is no manifest (§4.1): with a
	// manifest the orchestrator has declared the document set and a scan would
	// be the guessing the rule forbids.
	if req.Manifest {
		return set
	}

	trees := append([]string{req.Root}, req.Repos...)
	for _, tree := range trees {
		if tree == "" {
			continue
		}
		for _, sd := range conventionScan {
			for _, path := range scanOne(tree, sd, &set) {
				doc, ref, err := load(res, path, req.Root, cache)
				if err != nil {
					if errors.Is(err, ErrHidden) {
						continue
					}
					set.Refusals = append(set.Refusals, ref)
					continue
				}
				if seen[doc.Path] {
					continue
				}
				seen[doc.Path] = true
				doc.Kind = sd.kind
				doc.Rel = relTo(req, doc.Path)
				finish(&doc)
				set.Documents = append(set.Documents, doc)
			}
		}
	}
	return set
}

// Open is the single-document entry point behind ProjectDocument(path). Its
// argument is a path, not a root, so it ATTRIBUTES BEFORE IT ADMITS (§3.1.2):
// the owning project is resolved and the visibility predicate evaluated first,
// so a refusal can never echo a path belonging to a hidden project. Containment
// is an ADDITIONAL check, not a replacement — both must pass.
func Open(res *projects.Resolver, path string, cache *Cache) (Document, Refusal, error) {
	doc, ref, err := load(res, path, "", cache)
	if err != nil {
		return Document{}, ref, err
	}
	doc.Kind = KindArchitecture
	finish(&doc)
	return doc, Refusal{}, nil
}

// projectVisible is §3.1's gate, evaluated through internal/projects and
// nowhere else. There is exactly ONE visibility predicate in Loom; a second
// implementation is how the surface that forgets a branch still passes its own
// test (slice 1 §4).
//
// FAIL CLOSED: an unattributable, unresolvable or unknown root is treated as
// hidden, matching slice 1 §6's rule for unattributable rows. Note this is
// stricter than ProjectVisible alone, which answers true for a root it has
// never heard of — correct for its callers, who hold a root that came out of
// the table, and wrong here, where the root arrives from the frontend.
func projectVisible(res *projects.Resolver, root string) bool {
	if res == nil || root == "" {
		return false
	}
	if _, known := res.Project(root); !known {
		return false
	}
	return res.ProjectVisible(root)
}

func scanOne(tree string, sd scanDir, set *Set) []string {
	dir := filepath.Join(tree, sd.dir)
	if sd.file != "" {
		p := filepath.Join(dir, sd.file)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return []string{p}
		}
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		// A missing convention directory is the normal case, not a problem;
		// anything else is worth saying out loud.
		if !os.IsNotExist(err) {
			set.Warnings = append(set.Warnings, fmt.Sprintf("cannot scan %s: %v", dir, err))
		}
		return nil
	}
	var out []string
	for _, e := range ents {
		// Non-recursive beyond the enumerated globs (§4.1). A symlinked entry
		// is not skipped here — it is admitted or refused by containment on
		// its PHYSICAL form, which is the check that matters.
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out // os.ReadDir sorts by filename, so the order is deterministic
}

// load is §4.2's admission check followed by the read. Every path that reaches
// the disk goes through here; there is no second site.
//
// wantRoot is the project the document must belong to; "" means "any visible
// project", which is Open's case (§3.1.2 — it attributes rather than being
// told). Requiring the match is what makes the sibling-prefix shape safe:
// `…/HappyPay/HappyPay` is a raw string prefix of `…/HappyPayCoreApi`, so a
// document in the second must not render on the first's overview.
func load(res *projects.Resolver, path, wantRoot string, cache *Cache) (Document, Refusal, error) {
	// 1. Abs + Clean. This alone defeats `../../other-client/secrets.md`:
	//    Clean folds the traversal, and the folded path then fails containment.
	abs := projects.Canonical(path)

	// A lexical pre-gate, so a path that already attributes to a hidden project
	// costs no disk access at all and produces no refusal text (§3.1.2).
	if a, ok := res.Attribute(abs); ok && !projectVisible(res, a.Root) {
		return Document{}, Refusal{}, ErrHidden
	}

	// 3. Extension is `.md`. Checked before any disk access — it is free, and
	//    it keeps id_rsa from even being stat-ed.
	if !strings.EqualFold(filepath.Ext(abs), ".md") {
		return Document{}, Refusal{Path: abs, Rule: "not a .md file"}, errRefused
	}

	// 1b. EvalSymlinks — the PHYSICAL form. A symlink out of the tree is the
	//     obvious escape, and it is invisible to any purely lexical check. The
	//     physicalDir lesson applies in the same direction it did at launch:
	//     the physical path is the identity that matters.
	phys, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Document{}, Refusal{Path: abs, Rule: fmt.Sprintf("unreadable: %v", err)}, errRefused
	}

	// 2. Segment-wise containment in the project root or one of its
	//    `project_repos.path` entries, using the matcher internal/projects
	//    already owns. Attribute IS that matcher — longest match over
	//    {projects.root} ∪ {project_repos.path}, segment-wise, symlink-aware on
	//    the target side. No new path logic: a second implementation is a
	//    second bug, and this one would be the bug that leaks a file.
	//
	//    An out-of-root member repo is ADMITTED by this, correctly: slice 1
	//    allows a repo outside its project's root, and Attribute resolves it to
	//    the owning project just the same.
	a, ok := res.Attribute(phys)
	if !ok {
		return Document{}, Refusal{Path: abs, Rule: "path is outside every project"}, errRefused
	}
	if !projectVisible(res, a.Root) {
		return Document{}, Refusal{}, ErrHidden
	}
	// Ordered after the visibility check on purpose: a refusal naming a path
	// that belongs to a hidden project would be the leak the check exists to
	// prevent, one error string at a time.
	if wantRoot != "" && a.Root != wantRoot {
		return Document{}, Refusal{Path: abs, Rule: "path is outside this project"}, errRefused
	}

	f, err := os.Open(phys)
	if err != nil {
		return Document{}, Refusal{Path: phys, Rule: fmt.Sprintf("unreadable: %v", err)}, errRefused
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Document{}, Refusal{Path: phys, Rule: fmt.Sprintf("unreadable: %v", err)}, errRefused
	}
	if !st.Mode().IsRegular() {
		// A fifo would block the render path forever; a device file is worse.
		return Document{}, Refusal{Path: phys, Rule: "not a regular file"}, errRefused
	}

	doc := Document{
		Path:    phys,
		Name:    filepath.Base(phys),
		Size:    st.Size(),
		ModTime: st.ModTime().Unix(),
	}
	if cache != nil {
		if body, trunc, ok := cache.get(phys, doc.Size, doc.ModTime); ok {
			doc.body, doc.Truncated = body, trunc
			return doc, Refusal{}, nil
		}
	}

	// 4. Size cap. Read one byte past it so "exactly at the cap" is not
	//    reported as truncated.
	buf := make([]byte, MaxDocBytes+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Document{}, Refusal{Path: phys, Rule: fmt.Sprintf("unreadable: %v", err)}, errRefused
	}
	body := string(buf[:n])
	if n > MaxDocBytes {
		body = body[:MaxDocBytes]
		// Cut back to a line boundary so the head never ends mid-construct
		// (or mid-rune, which would render as a replacement character).
		if nl := strings.LastIndexByte(body, '\n'); nl > 0 {
			body = body[:nl]
		}
		doc.Truncated = true
	}
	doc.body = body
	if cache != nil {
		cache.put(phys, doc.Size, doc.ModTime, body, doc.Truncated)
	}
	return doc, Refusal{}, nil
}

// errRefused is the internal sentinel; callers read the Refusal, which carries
// the path and the broken rule for the .po-warn card.
var errRefused = errors.New("refused")

// Refused reports whether err came from §4.2's admission check, as opposed to
// the hidden payload.
func Refused(err error) bool { return errors.Is(err, errRefused) }

// finish parses the body once the kind is known. Front matter is stripped for
// every kind — a leading `---` block is metadata wherever it appears, and
// rendering it as a table would be worse than dropping it.
func finish(doc *Document) {
	body := doc.body
	meta, rest, warns := parseFrontMatter(body)
	doc.Warnings = append(doc.Warnings, warns...)
	if len(warns) > 0 {
		// Malformed front matter renders as BODY TEXT plus a warning chip,
		// never fatally: the content is the point, the metadata is the garnish.
		rest = body
		meta = Meta{}
	}
	if meta.Date == "" {
		meta.DateFromMTime = true
	}
	doc.Meta = meta

	if doc.Truncated {
		rest = rest + "\n\n" + TruncationMarker + "\n"
	}
	doc.Blocks = Render(rest)
	doc.Outline = Outline(doc.Blocks)
	doc.Meta.Consequence = consequence(doc.Blocks)

	if doc.Title == "" {
		doc.Title = meta.Title
	}
	if doc.Title == "" {
		doc.Title = firstHeading(doc.Blocks)
	}
	if doc.Title == "" {
		doc.Title = doc.Name
	}
	if doc.ID == "" {
		doc.ID = meta.ID
	}
	doc.body = ""
}

func firstHeading(blocks []Block) string {
	for _, b := range blocks {
		if b.Kind == BlockHeading {
			return PlainText(b.Inline)
		}
	}
	return ""
}

// consequence lifts the first paragraph under a "Consequence(s)" heading. It
// is a quotation, not a summary: Loom does not paraphrase a decision record,
// because a paraphrase on a glance surface is a confident wrong sentence.
func consequence(blocks []Block) string {
	for i, b := range blocks {
		if b.Kind != BlockHeading {
			continue
		}
		h := strings.ToLower(strings.TrimSpace(PlainText(b.Inline)))
		if !strings.HasPrefix(h, "consequence") {
			continue
		}
		for _, n := range blocks[i+1:] {
			switch n.Kind {
			case BlockParagraph:
				return firstLine(PlainText(n.Inline))
			case BlockList:
				if len(n.Items) > 0 {
					return firstLine(PlainText(n.Items[0].Inline))
				}
			case BlockHeading:
				return ""
			}
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// declaredKind validates a manifest-declared kind. An unrecognized kind falls
// back to `architecture` WITH a warning rather than refusing the document: the
// file passed containment, so the content is admissible, and losing it over a
// typo in a generated manifest would be the renderer being clever.
func declaredKind(k DocKind, warns []string) (DocKind, []string) {
	switch k {
	case KindArchitecture, KindDecision, KindContract:
		return k, warns
	case "":
		return KindArchitecture, append(warns, "manifest declared no kind — rendered as architecture")
	default:
		return KindArchitecture, append(warns, fmt.Sprintf("unknown document kind %q — rendered as architecture", string(k)))
	}
}

// relTo is display only. It tries each tree in BOTH its stored and its
// physical form: a stored root under /var and a document path under
// /private/var are the same directory, and a Rel between them would otherwise
// come back as a ladder of `..` on every mac.
func relTo(req Request, path string) string {
	best := ""
	var trees []string
	for _, t := range append([]string{req.Root}, req.Repos...) {
		if t == "" {
			continue
		}
		trees = append(trees, t)
		if p, err := filepath.EvalSymlinks(t); err == nil && p != t {
			trees = append(trees, p)
		}
	}
	for _, tree := range trees {
		rel, err := filepath.Rel(tree, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if best == "" || len(rel) < len(best) {
			best = rel
		}
	}
	if best == "" {
		return path
	}
	return best
}

// parseFrontMatter reads §4.3's restricted subset. Warnings are returned
// rather than errors: there is no fatal outcome here by design.
func parseFrontMatter(body string) (Meta, string, []string) {
	lines := splitLines(body)
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return Meta{}, body, nil
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return Meta{}, body, []string{"front matter: opening --- has no closing ---"}
	}

	var m Meta
	var warns []string
	for _, l := range lines[1:end] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if l != strings.TrimLeft(l, " \t") {
			// Indentation means nesting, and nesting means YAML. The subset is
			// flat scalars precisely so no YAML engine is needed.
			warns = append(warns, "front matter: nested value is outside the supported subset")
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			warns = append(warns, fmt.Sprintf("front matter: %q is not key: value", l))
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		switch k {
		case "title":
			m.Title = v
		case "status":
			m.Status = v
			m.StatusKnown = v != ""
		case "date":
			m.Date = v
		case "supersedes":
			m.Supersedes = v
		case "id":
			m.ID = v
		default:
			warns = append(warns, fmt.Sprintf("front matter: key %q is outside the supported subset", k))
		}
	}
	return m, strings.Join(lines[end+1:], "\n"), warns
}

// Cache is §4.4's in-memory document cache, keyed on (path, size, mtime) — the
// same fingerprint discipline `indexed_files` uses. Documents are read from
// disk at render time and NEVER copied into loom.db: the file is the source of
// truth, and a persisted copy is a stale render waiting for a git checkout.
type Cache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

type cacheEntry struct {
	size    int64
	modTime int64
	body    string
	trunc   bool
}

func NewCache() *Cache { return &Cache{m: make(map[string]cacheEntry)} }

func (c *Cache) get(path string, size, mod int64) (string, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[path]
	if !ok || e.size != size || e.modTime != mod {
		return "", false, false
	}
	return e.body, e.trunc, true
}

func (c *Cache) put(path string, size, mod int64, body string, trunc bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]cacheEntry)
	}
	c.m[path] = cacheEntry{size: size, modTime: mod, body: body, trunc: trunc}
}
