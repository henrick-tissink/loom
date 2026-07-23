package orchestrator

import (
	"os"
	"path/filepath"
)

// noteFile is one of the three agent-authored notes (§3) as read at assembly.
type noteFile struct {
	Name    string
	Path    string
	Content string
	Bytes   int
	SHA256  string
	Exists  bool
	// PreExisting marks a file that is on disk but unknown to state.json. §3:
	// assembly includes it verbatim like any other note and labels it
	// "pre-existing — treat as authoritative, do not rewrite wholesale". Loom
	// never truncates or moves a file it did not create, and the label is how
	// a fresh orchestrator is told not to.
	PreExisting bool
	// ReadErr is a rendered reason the file could not be read (a permission
	// bit, a directory where a file should be). It is NOT "does not exist" —
	// that is Exists=false. Surfaced in the brief rather than swallowed: a note
	// silently missing from a brief is the failure this whole slice is trying
	// not to have.
	ReadErr string
}

// readNotes reads the fixed three-file layout out of notesDir, in the fixed
// order NoteFiles declares. Order is part of the determinism guarantee (§5.3):
// a directory listing would hand back whatever the filesystem felt like.
//
// prev/prevOK decide the pre-existing label only. A first-ever spawn has no
// state.json, so every note already on disk is correctly flagged pre-existing —
// which is exactly right: those files were written by a human or by an
// orchestrator whose ledger was lost, and either way this generation did not
// write them.
func readNotes(notesDir string, prev State, prevOK bool) []noteFile {
	out := make([]noteFile, 0, len(NoteFiles))
	for _, name := range NoteFiles {
		n := noteFile{Name: name, Path: filepath.Join(notesDir, name)}
		if notesDir == "" {
			out = append(out, n)
			continue
		}
		b, err := os.ReadFile(n.Path)
		switch {
		case err == nil:
			n.Exists = true
			n.Content = string(b)
			n.Bytes = len(b)
			n.SHA256 = digest(b)
			_, known := prev.Notes[name]
			n.PreExisting = !prevOK || !known
		case os.IsNotExist(err):
			// Absent is the normal bootstrap state, not an error.
		default:
			n.ReadErr = err.Error()
		}
		out = append(out, n)
	}
	return out
}

// anyNotes reports whether the project has a brain yet — the test §8's "first
// spawn is not a special mode" hangs on. Bootstrap and steady state are one
// code path; only the rendered wording differs.
func anyNotes(notes []noteFile) bool {
	for _, n := range notes {
		if n.Exists {
			return true
		}
	}
	return false
}
