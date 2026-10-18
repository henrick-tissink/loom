package brief

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

// Service is what the CLI and the GUI share: resolve a project, collect its
// evidence, have the brief written, keep it.
type Service struct {
	Store           *store.Store
	ClaudeConfigDir string
	Archive         Archive
	Writer          Writer
	Run             Runner           // ExecRunner when nil
	Now             func() time.Time // time.Now when nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Scope resolves a project by its root path or (case-insensitively) its
// name. Hidden projects are refused: hiding fails closed on every surface,
// and a brief is a surface (docs/ARCHITECTURE.md §8).
func (s *Service) Scope(rootOrName string) (Scope, error) {
	ps, err := s.Store.ListProjects()
	if err != nil {
		return Scope{}, err
	}
	want := strings.TrimSpace(rootOrName)
	var hit *store.Project
	for i, p := range ps {
		if p.Root == "" {
			continue // Ungrouped has no root to brief
		}
		if p.Root == filepath.Clean(want) || strings.EqualFold(p.Name, want) {
			hit = &ps[i]
			break
		}
	}
	if hit == nil {
		return Scope{}, fmt.Errorf("no project %q", rootOrName)
	}
	if hit.Hidden {
		return Scope{}, fmt.Errorf("project %q is hidden", hit.Name)
	}
	repos, err := s.Store.ListProjectRepos(hit.Root)
	if err != nil {
		return Scope{}, err
	}
	sc := Scope{Root: hit.Root, Name: hit.Name}
	for _, r := range repos {
		if !r.Missing {
			sc.Repos = append(sc.Repos, r.Path)
		}
	}
	return sc, nil
}

// Collect gathers a scope's evidence for a window ending now.
func (s *Service) Collect(ctx context.Context, sc Scope, win Window) (*Bundle, error) {
	until := s.now()
	return Collect(ctx, Options{
		Scope: sc, Since: until.Add(-win.Dur), Until: until,
		ClaudeConfigDir: s.ClaudeConfigDir, Run: s.Run,
	})
}

// Generate writes and saves a brief from a collected bundle.
func (s *Service) Generate(ctx context.Context, b *Bundle, win Window) (Meta, error) {
	if len(b.Sessions) == 0 && len(b.Git.Mine) == 0 {
		return Meta{}, fmt.Errorf("nothing to brief: no session worked in %s and you made no commits in the %s", b.Scope.Name, win.Label)
	}
	start := s.now()
	evidence := b.Markdown()
	body, err := s.Writer.Write(ctx, evidence)
	if err != nil {
		return Meta{}, err
	}
	st := b.Stats()
	m := Meta{
		ProjectRoot: b.Scope.Root, ProjectName: b.Scope.Name, Window: win.Key,
		Since: b.Since.Unix(), Until: b.Until.Unix(), Model: WriterModel,
		Sessions: st.Sessions, Commits: st.Commits, OtherCommits: st.OtherCommits,
		InputTokens: st.InputTokens, DurationMS: s.now().Sub(start).Milliseconds(),
	}
	return s.Archive.Save(m, Assemble(b, win, body), evidence, s.now())
}

// Ask modes.
const (
	AskExplain = "explain"
	AskReview  = "review"
)

// AskSeed writes task n's section next to its brief and returns the seed for
// a fenced study session about it, plus the directory the session must be
// allowed to read. The seed is one line (it is typed into claude's prompt)
// and carries SeedMarker so the session never shows up in a later brief.
func (s *Service) AskSeed(id string, n int, mode string) (seed, briefDir string, err error) {
	_, src, err := s.Archive.Load(id)
	if err != nil {
		return "", "", err
	}
	p, ok := Task(src, n)
	if !ok {
		return "", "", fmt.Errorf("brief %s has no task %d", id, n)
	}
	askPath, err := s.Archive.Path(id, fmt.Sprintf("task-%d.md", n))
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(askPath, []byte(p.MD+"\n"), 0o600); err != nil {
		return "", "", err
	}
	evPath, _ := s.Archive.Path(id, FileEvidence)
	briefPath, _ := s.Archive.Path(id, FileBrief)
	switch mode {
	case AskReview:
		seed = fmt.Sprintf("%s Review this task independently. Read %s (the task, from my brief %s), then the commits, diff excerpts and transcript entries it cites in %s (evidence ids like S3:L120 are entries there), then the current code in this repository. Report what the agent did not: bugs, untested paths, risky assumptions, and places where what was claimed and what the code does disagree. Cite file:line, and say how sure you are of each finding.",
			SeedMarker, askPath, briefPath, evPath)
	default:
		seed = fmt.Sprintf("%s I am studying a task from my project brief so I can explain it accurately. Read %s (the task's section; the whole brief is %s) and use %s (the evidence it cites; ids like S3:L120 are entries there) and this repository's code to answer my questions. Tell me in two lines that you are ready, then wait for my questions. Keep answers grounded in the evidence and the code, cite them, and say when something is not in the evidence.",
			SeedMarker, askPath, briefPath, evPath)
	}
	return seed, filepath.Dir(askPath), nil
}
