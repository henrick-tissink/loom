package brief

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Meta describes one saved brief.
type Meta struct {
	ID           string `json:"id"` // "<project-slug>/<stamp>-<window>"
	ProjectRoot  string `json:"projectRoot"`
	ProjectName  string `json:"projectName"`
	Window       string `json:"window"`
	Since        int64  `json:"since"`
	Until        int64  `json:"until"`
	CreatedAt    int64  `json:"createdAt"`
	Model        string `json:"model"`
	Sessions     int    `json:"sessions"`
	Commits      int    `json:"commits"`
	OtherCommits int    `json:"otherCommits"`
	InputTokens  int    `json:"inputTokens"`
	DurationMS   int64  `json:"durationMs"`
}

// Archive keeps briefs on disk under <loom dir>/briefs, one directory per
// brief: brief.md (what Henrick reads), evidence.md (what it was written
// from, and what an "Ask about this" session reads), meta.json, and the
// rendered page once he asks for it. Files, not loom.db rows: a brief is a
// document, and a document he can open, grep and keep is worth more than a
// row only loom can show.
type Archive struct{ Dir string }

const (
	FileBrief    = "brief.md"
	FileEvidence = "evidence.md"
	FileMeta     = "meta.json"
	FilePage     = "brief.html"
)

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// ProjectSlug is the directory a project's briefs live in: a readable name
// plus a short hash of the root, so two projects with one name never mix.
func ProjectSlug(root, name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "project"
	}
	h := sha1.Sum([]byte(root))
	return s + "-" + hex.EncodeToString(h[:])[:8]
}

var idRe = regexp.MustCompile(`^[a-z0-9-]+/[0-9]{8}-[0-9]{6}-[a-z0-9]+$`)

// dirFor resolves a brief id to its directory, refusing anything that is not
// an id this archive minted (an id arrives from the frontend).
func (a Archive) dirFor(id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", fmt.Errorf("brief: bad id %q", id)
	}
	return filepath.Join(a.Dir, filepath.FromSlash(id)), nil
}

// Path is the path of one of a brief's files.
func (a Archive) Path(id, file string) (string, error) {
	d, err := a.dirFor(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, file), nil
}

// Save writes a new brief and returns its meta with the id filled in.
func (a Archive) Save(m Meta, brief, evidence string, now time.Time) (Meta, error) {
	m.ID = ProjectSlug(m.ProjectRoot, m.ProjectName) + "/" + now.Local().Format("20060102-150405") + "-" + m.Window
	m.CreatedAt = now.Unix()
	d, err := a.dirFor(m.ID)
	if err != nil {
		return m, err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return m, err
	}
	mj, _ := json.MarshalIndent(m, "", "  ")
	for name, body := range map[string][]byte{FileBrief: []byte(brief), FileEvidence: []byte(evidence), FileMeta: mj} {
		if err := os.WriteFile(filepath.Join(d, name), body, 0o600); err != nil {
			return m, err
		}
	}
	return m, nil
}

// List returns a project's briefs, newest first.
func (a Archive) List(root, name string) ([]Meta, error) {
	base := filepath.Join(a.Dir, ProjectSlug(root, name))
	ents, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(base, e.Name(), FileMeta))
		if err != nil {
			continue
		}
		var m Meta
		if json.Unmarshal(b, &m) == nil && m.ID != "" {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// Load returns a brief's meta and markdown.
func (a Archive) Load(id string) (Meta, string, error) {
	d, err := a.dirFor(id)
	if err != nil {
		return Meta{}, "", err
	}
	var m Meta
	b, err := os.ReadFile(filepath.Join(d, FileMeta))
	if err != nil {
		return m, "", err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, "", err
	}
	md, err := os.ReadFile(filepath.Join(d, FileBrief))
	return m, string(md), err
}

// Assemble wraps the writer's markdown into the saved brief: a header with
// the window and the evidence it came from, then the body, then the git
// facts verbatim and the evidence index, so every status line can be checked
// against what git said and every S-id resolved to a transcript.
func Assemble(b *Bundle, win Window, body string) string {
	var w strings.Builder
	fmt.Fprintf(&w, "# %s · %s\n\n", b.Scope.Name, win.Label)
	st := b.Stats()
	fmt.Fprintf(&w, "*%s → %s · written by Claude (%s) from %d sessions, %d of your commits and %d by others.*\n\n",
		stamp(b.Since), stamp(b.Until), WriterModel, st.Sessions, st.Commits, st.OtherCommits)
	w.WriteString(strings.TrimSpace(body))
	w.WriteString("\n\n---\n\n# Appendix\n\n")
	w.WriteString(strings.Replace(b.GitMarkdown(), "## Git facts", "## Git facts (verbatim, as collected)", 1))
	w.WriteString("## Evidence index\n\n")
	for _, s := range b.Sessions {
		kind := "session"
		if s.Subagent {
			kind = "subagent"
		}
		fmt.Fprintf(&w, "- **%s** · %s %s · %s · `%s`\n", s.ID, kind, s.SessionID, oneLine(orDash(s.Title)), s.File)
	}
	return w.String()
}
