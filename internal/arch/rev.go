package arch

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/henricktissink/loom/internal/projects"
)

// Rev is §7.4's document-freshness probe: a fingerprint over the (path, size,
// mtime) of the document set, computed WITHOUT reading, parsing or rendering a
// single byte of any of them.
//
// Why this exists at all. §7.4 says documents refresh on (size, mtime) change,
// "checked on the same tick". Without a probe the frontend's only way to ask
// "did anything change" is to call ProjectDocuments — which loads, front-matter
// parses and markdown-renders every document in the set. Paying that on a 1.5s
// poll is exactly what §7.5's cost ceiling forbids, so the shipped frontend
// refreshed documents on open / manual Refresh only and a decision record
// written while the overview was on screen simply did not appear.
//
// Why a HASH rather than a max-mtime or a count. A max mtime cannot see a
// document being deleted or reverted to an older copy, and a count cannot see a
// document being edited. The fingerprint is the same discipline `indexed_files`
// already uses for transcripts (store/memory.go): size AND mtime, together,
// because either alone has a well-known blind spot.
//
// Why the fingerprint deliberately covers paths that Documents would REFUSE.
// Rev stats candidates; it does not attribute or containment-check them. A
// declared entry pointing outside the project is refused by Documents and
// rendered as a refusal card — and that card's text is part of what the client
// is holding, so a change to the named file must still move the rev. Rev never
// opens anything, so admitting a path here reads nothing that Documents would
// not have stat'ed anyway.
//
// ok is false for a hidden or unattributable project, and then rev is 0. This
// is §3.1's gate again and it runs FIRST, before any disk access — a rev that
// moved for a hidden project would report, one bit at a time, that its files
// were being edited.
func Rev(res *projects.Resolver, req Request) (rev uint64, ok bool) {
	if !projectVisible(res, req.Root) {
		return 0, false
	}

	// FNV-1a 64. Hand-rolled rather than hash/fnv because the mixing is four
	// lines and this way the accumulator can be fed field-by-field with no
	// allocation on a path that runs every 1.5s.
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	mix := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= prime64
		}
		// A field terminator, so ("ab","c") and ("a","bc") cannot collide.
		h ^= 0
		h *= prime64
	}
	// stat is the only syscall this function makes per candidate. A path that
	// does not exist still mixes its name plus a miss marker: a declared
	// document appearing or disappearing MUST move the rev, and skipping the
	// absent case would make an appearance invisible until something else in
	// the set also changed.
	stat := func(path string) {
		mix(path)
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			mix("-")
			return
		}
		mix(strconv.FormatInt(st.Size(), 10))
		mix(strconv.FormatInt(st.ModTime().UnixNano(), 10))
	}

	for _, d := range req.Declared {
		path := d.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(req.Root, path)
		}
		stat(path)
	}
	// §4.1's rule holds here exactly as it holds in Documents: with a manifest
	// the declared set IS the set, and scanning would fingerprint files the
	// rendered payload does not contain — a rev that changed while the picture
	// did not, which is the same false alarm as a rev that never changes, only
	// more expensive.
	if req.Manifest {
		return h, true
	}

	// The scan half needs a ReadDir per convention directory, not a stat: a
	// NEW decision record is a new directory entry, and no amount of stat'ing
	// files we already know about can see one. That is more than §7.4's "one
	// stat per declared document", and it is the honest cost of the discovered
	// case — five ReadDirs per tree, on directories that mostly do not exist.
	// Dropping it would mean an ADR written while the overview is open never
	// appears, which is the case the probe was asked for.
	//
	// scanOne's own warnings are discarded here on purpose: an unreadable
	// convention directory is reported by Documents, on the path that renders
	// it. A probe that accumulated warnings would either duplicate them or
	// leak them into a payload that has no place to show them.
	var sink Set
	trees := append([]string{req.Root}, req.Repos...)
	for _, tree := range trees {
		if tree == "" {
			continue
		}
		for _, sd := range conventionScan {
			for _, path := range scanOne(tree, sd, &sink) {
				stat(path)
			}
		}
	}
	return h, true
}
