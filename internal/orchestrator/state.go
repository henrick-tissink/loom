package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// stateSchema is the version stamped into state.json. It is a cache with a
// schema number, not a source of truth: §8 says an unparseable file is treated
// as absent and rewritten, because everything in it is rederivable and the
// notes — not this — are the brain.
const stateSchema = 1

// RepoStamp is what a repo looked like the last time an orchestrator was
// briefed. Branch and head only: §8's drift is deliberately coarse (a commit
// count, not a diff), and §14 discloses that a 200-commit refactor and a
// 200-commit dependency bump read identically.
type RepoStamp struct {
	Branch string `json:"branch"`
	Head   string `json:"head"`
}

// NoteStamp fingerprints one of the three notes files. The digest is what makes
// "notes edited" answerable without keeping a copy of content Loom does not
// author.
type NoteStamp struct {
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

// State is ~/.loom/projects/<key>/state.json (§8): the drift oracle plus the
// spawn ledger. Machine-only — nothing renders it verbatim.
//
// Stamped AT ASSEMBLY ONLY, never continuously. That is what makes drift the
// question a fresh orchestrator actually needs answered — "what changed since
// an orchestrator was last briefed on this project?" — rather than "what
// changed in the last poll interval", which nobody asked.
type State struct {
	Schema            int                  `json:"schema"`
	ProjectRoot       string               `json:"project_root"`
	NotesDir          string               `json:"notes_dir"`
	AssembledAt       int64                `json:"assembled_at"`
	SpawnCount        int                  `json:"spawn_count"`
	LastSession       string               `json:"last_session"`
	BriefBytes        int                  `json:"brief_bytes"`
	BriefSHA256       string               `json:"brief_sha256"`
	TruncatedSections []string             `json:"truncated_sections"`
	Repos             map[string]RepoStamp `json:"repos"`
	Notes             map[string]NoteStamp `json:"notes"`
}

// loadState reads state.json. A missing OR corrupt file yields ok=false and a
// zero State — §8, binding: "if state.json is corrupt or unparseable, it is
// treated as absent and rewritten". Returning an error instead would let a
// truncated write from a killed process permanently block spawning a session
// whose entire durable state lives somewhere else.
func loadState(path string) (State, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}, false
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, false
	}
	if s.Schema != stateSchema {
		// A future schema is as unreadable as a corrupt one, and guessing at a
		// newer shape is how a downgrade silently loses the spawn ledger.
		return State{}, false
	}
	return s, true
}

// saveState writes state.json atomically (temp + rename, the settings.go
// idiom), so a crash mid-write leaves the previous oracle intact rather than a
// half-file that loadState would then discard.
func saveState(path string, s State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// driftLines builds §8's `## Drift` report from the previous stamp and the
// current reading. It is a pure function of its arguments plus git, which is
// what lets §13's determinism test mean anything.
//
// Loom NEVER resolves drift — it states it, and `What to do` instructs the
// orchestrator to reconcile before starting new analysis. Auto-editing the
// notes would be Loom writing content it does not understand into files it does
// not author, and (in the in-repo configuration) into the user's workspace.
func driftLines(g gitRunner, prev State, repos []RepoState, notes []noteFile) []string {
	var out []string

	prevRepos := prev.Repos
	if prevRepos == nil {
		prevRepos = map[string]RepoStamp{}
	}

	// membership, first: a repo that appeared or vanished reframes every other
	// line below it.
	var added, removed []string
	cur := make(map[string]bool, len(repos))
	for _, r := range repos {
		cur[r.Path] = true
		if _, known := prevRepos[r.Path]; !known {
			added = append(added, r.Path)
		}
	}
	for p := range prevRepos {
		if !cur[p] {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	for _, p := range added {
		out = append(out, "membership changed: repo added — "+p)
	}
	for _, p := range removed {
		out = append(out, "membership changed: repo removed — "+p)
	}

	for _, r := range repos {
		old, known := prevRepos[r.Path]
		if !known || r.Missing || r.Err != "" {
			continue
		}
		if old.Head == r.Head {
			if old.Branch != r.Branch {
				out = append(out, fmt.Sprintf("%s: branch changed %s → %s", r.Label, old.Branch, r.Branch))
			}
			continue
		}
		if n, ok := commitsSince(g, r.Path, old.Head); ok {
			out = append(out, fmt.Sprintf("%s: %d commits since the last brief (%s → %s)",
				r.Label, n, short(old.Head), short(r.Head)))
			continue
		}
		out = append(out, fmt.Sprintf("%s: history rewritten or commit unknown (%s → %s)",
			r.Label, short(old.Head), short(r.Head)))
	}

	prevNotes := prev.Notes
	if prevNotes == nil {
		prevNotes = map[string]NoteStamp{}
	}
	for _, n := range notes {
		old, known := prevNotes[n.Name]
		if !known {
			continue
		}
		if !n.Exists {
			// Reported, never recreated. Loom does not author these files and
			// does not resurrect them; a deletion is a fact the orchestrator
			// (or the human who did it) has to account for.
			out = append(out, "notes missing: "+n.Name+" is gone (recorded at "+
				fmt.Sprintf("%d bytes", old.Bytes)+") — not recreated")
			continue
		}
		if old.SHA256 != n.SHA256 {
			// Labelled expected, not flagged as a problem: this fires whenever
			// the PREVIOUS orchestrator did its job, and it also catches a human
			// editing the notes between spawns, which is supported and
			// encouraged.
			out = append(out, "notes edited (expected): "+n.Name)
		}
	}
	return out
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
