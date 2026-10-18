package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/brief"
	"github.com/henricktissink/loom/internal/session"
)

// briefState is the GUI's side of project briefs (internal/brief): the
// service, the last collected bundle per project and window (an estimate
// collects; Write reuses it if it is fresh), and which briefs are being
// written right now, so a re-render shows "Writing…" instead of a second
// button press starting a second Opus run.
type briefState struct {
	svc *brief.Service

	mu      sync.Mutex
	bundles map[string]cachedBundle
	running map[string]time.Time // key root|window -> started
}

type cachedBundle struct {
	b  *brief.Bundle
	at time.Time
}

// bundleTTL is how long an estimate's bundle stands in for a fresh collect.
const bundleTTL = 5 * time.Minute

func newBriefState(svc *brief.Service) *briefState {
	return &briefState{svc: svc, bundles: map[string]cachedBundle{}, running: map[string]time.Time{}}
}

// BriefWindowDTO is one window choice.
type BriefWindowDTO struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// BriefEstimateDTO is what writing a brief would cost, shown before the click.
type BriefEstimateDTO struct {
	Window       string `json:"window"`
	Label        string `json:"label"`
	Since        int64  `json:"since"`
	Until        int64  `json:"until"`
	Sessions     int    `json:"sessions"`
	Commits      int    `json:"commits"`
	OtherCommits int    `json:"otherCommits"`
	InputTokens  int    `json:"inputTokens"`
	Model        string `json:"model"`
}

// BriefPartDTO is one rendered slice of a brief; Task > 0 marks a task.
type BriefPartDTO struct {
	Task  int    `json:"task"`
	Title string `json:"title"`
	HTML  string `json:"html"`
}

// BriefDTO is an opened brief.
type BriefDTO struct {
	Meta  brief.Meta     `json:"meta"`
	Parts []BriefPartDTO `json:"parts"`
	Path  string         `json:"path"`
}

func (a *App) briefsReady() error {
	if a.briefs == nil || a.briefs.svc == nil {
		return fmt.Errorf("briefs unavailable")
	}
	return nil
}

// briefScope resolves a root to its scope, refusing anything not visible
// right now. Hiding fails closed: no resolver means no brief.
func (a *App) briefScope(root string) (brief.Scope, error) {
	if err := a.briefsReady(); err != nil {
		return brief.Scope{}, err
	}
	r := a.resolver()
	if r == nil || !r.Visible(root) {
		return brief.Scope{}, fmt.Errorf("project not available")
	}
	return a.briefs.svc.Scope(root)
}

// briefMeta loads a brief by id and applies the same visibility rule to the
// project it belongs to.
func (a *App) briefMeta(id string) (brief.Meta, string, error) {
	if err := a.briefsReady(); err != nil {
		return brief.Meta{}, "", err
	}
	m, src, err := a.briefs.svc.Archive.Load(id)
	if err != nil {
		return m, "", err
	}
	if _, err := a.briefScope(m.ProjectRoot); err != nil {
		return m, "", err
	}
	return m, src, nil
}

// BriefWindows lists the window choices.
func (a *App) BriefWindows() []BriefWindowDTO {
	out := make([]BriefWindowDTO, 0, len(brief.Windows))
	for _, w := range brief.Windows {
		out = append(out, BriefWindowDTO{Key: w.Key, Label: w.Label})
	}
	return out
}

// BriefList returns a project's saved briefs, newest first.
func (a *App) BriefList(root string) ([]brief.Meta, error) {
	sc, err := a.briefScope(root)
	if err != nil {
		return nil, err
	}
	ms, err := a.briefs.svc.Archive.List(sc.Root, sc.Name)
	if ms == nil {
		ms = []brief.Meta{}
	}
	return ms, err
}

// BriefRunning lists the windows whose brief is being written for root.
func (a *App) BriefRunning(root string) []string {
	if a.briefs == nil {
		return []string{}
	}
	a.briefs.mu.Lock()
	defer a.briefs.mu.Unlock()
	out := []string{}
	for _, w := range brief.Windows {
		if _, ok := a.briefs.running[root+"|"+w.Key]; ok {
			out = append(out, w.Key)
		}
	}
	return out
}

// BriefEstimate collects the evidence for a window and reports its size.
// No Claude call: this is the "what will it cost" step before Write.
func (a *App) BriefEstimate(root, window string) (BriefEstimateDTO, error) {
	sc, err := a.briefScope(root)
	if err != nil {
		return BriefEstimateDTO{}, err
	}
	w, err := brief.ParseWindow(window)
	if err != nil {
		return BriefEstimateDTO{}, err
	}
	b, err := a.briefs.svc.Collect(context.Background(), sc, w)
	if err != nil {
		return BriefEstimateDTO{}, err
	}
	a.briefs.mu.Lock()
	a.briefs.bundles[root+"|"+w.Key] = cachedBundle{b: b, at: a.now()}
	a.briefs.mu.Unlock()
	st := b.Stats()
	return BriefEstimateDTO{
		Window: w.Key, Label: w.Label, Since: b.Since.Unix(), Until: b.Until.Unix(),
		Sessions: st.Sessions, Commits: st.Commits, OtherCommits: st.OtherCommits,
		InputTokens: st.InputTokens, Model: brief.WriterModel,
	}, nil
}

// BriefGenerate writes and saves a brief (minutes; uses Claude quota). It
// reuses the estimate's bundle when that is fresh, so the brief is written
// from exactly the evidence the estimate counted.
func (a *App) BriefGenerate(root, window string) (brief.Meta, error) {
	sc, err := a.briefScope(root)
	if err != nil {
		return brief.Meta{}, err
	}
	w, err := brief.ParseWindow(window)
	if err != nil {
		return brief.Meta{}, err
	}
	key := root + "|" + w.Key
	a.briefs.mu.Lock()
	if _, busy := a.briefs.running[key]; busy {
		a.briefs.mu.Unlock()
		return brief.Meta{}, fmt.Errorf("this brief is already being written")
	}
	a.briefs.running[key] = a.now()
	cached, ok := a.briefs.bundles[key]
	a.briefs.mu.Unlock()
	defer func() {
		a.briefs.mu.Lock()
		delete(a.briefs.running, key)
		delete(a.briefs.bundles, key)
		a.briefs.mu.Unlock()
	}()

	b := cached.b
	if !ok || a.now().Sub(cached.at) > bundleTTL {
		if b, err = a.briefs.svc.Collect(context.Background(), sc, w); err != nil {
			return brief.Meta{}, err
		}
	}
	return a.briefs.svc.Generate(context.Background(), b, w)
}

// BriefOpen renders a saved brief for the reader, split at its tasks so
// each can carry its own "Ask about this".
func (a *App) BriefOpen(id string) (BriefDTO, error) {
	m, src, err := a.briefMeta(id)
	if err != nil {
		return BriefDTO{}, err
	}
	parts := brief.Split(src)
	out := BriefDTO{Meta: m, Parts: make([]BriefPartDTO, 0, len(parts))}
	out.Path, _ = a.briefs.svc.Archive.Path(id, brief.FileBrief)
	for _, p := range parts {
		out.Parts = append(out.Parts, BriefPartDTO{Task: p.Task, Title: p.Title, HTML: brief.HTML(p.MD)})
	}
	return out, nil
}

// BriefAsk opens a fenced study session about task n of a brief and returns
// its tmux name. mode is "explain" (answer my questions) or "review" (an
// independent review of the task's change). The session can only read: the
// project's repos and the brief's own directory, with no shell, web or MCP.
func (a *App) BriefAsk(id string, n int, mode string) (string, error) {
	if a.launcher == nil {
		return "", fmt.Errorf("launcher unavailable")
	}
	m, _, err := a.briefMeta(id)
	if err != nil {
		return "", err
	}
	sc, err := a.briefScope(m.ProjectRoot)
	if err != nil {
		return "", err
	}
	seed, dir, err := a.briefs.svc.AskSeed(id, n, mode)
	if err != nil {
		return "", err
	}
	cwd := sc.Root
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		if len(sc.Repos) == 0 {
			return "", fmt.Errorf("project directory is missing")
		}
		cwd = sc.Repos[0]
	}
	addDirs := []string{dir}
	for _, r := range sc.Repos {
		if r != cwd && !strings.HasPrefix(r, cwd+string(filepath.Separator)) {
			addDirs = append(addDirs, r) // a member repo outside the root
		}
	}
	return a.launcher.Launch(session.Recipe{
		ProjectLabel: sc.Name, Cwd: cwd, Model: brief.WriterModel,
		Seed: seed, AddDirs: addDirs, Fenced: true,
	}, launchCols, launchRows, a.now())
}

// BriefArtefact renders a brief as a self-contained page next to it and
// opens it in the default browser. The page is the same text, re-rendered.
func (a *App) BriefArtefact(id string) (string, error) {
	m, src, err := a.briefMeta(id)
	if err != nil {
		return "", err
	}
	p, err := a.briefs.svc.Archive.Path(id, brief.FilePage)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(brief.Page(m, src)), 0o600); err != nil {
		return "", err
	}
	return p, exec.Command("open", p).Start()
}

// BriefReveal shows a brief's folder (brief.md, evidence.md, the page) in Finder.
func (a *App) BriefReveal(id string) error {
	if _, _, err := a.briefMeta(id); err != nil {
		return err
	}
	p, err := a.briefs.svc.Archive.Path(id, brief.FileBrief)
	if err != nil {
		return err
	}
	return exec.Command("open", "-R", p).Start()
}
