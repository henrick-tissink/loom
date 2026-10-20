package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/scripts"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/worktree"
)

// Setup and run scripts (internal/scripts). A project's scripts are set
// from its menu; a new worktree session runs the setup script in its
// worktree once claude has made it; any row can Run the run script in its
// workspace on that workspace's ports and Stop it. Jobs are tmux sessions
// on loom's server, so they outlive loom; the engine reports them in each
// snapshot and the rows read the last one.

type scriptsState struct {
	store *scripts.Store
	shell string // the login shell scripts run in

	mu   sync.Mutex
	jobs map[string]tmux.PaneStatus // the last snapshot's
	errs map[string]string          // workspace → why the automatic setup did not start
}

func newScriptsState(loomDir string) (*scriptsState, error) {
	st, err := scripts.Open(filepath.Join(loomDir, "scripts.json"))
	if err != nil {
		return nil, err
	}
	return &scriptsState{store: st, shell: os.Getenv("SHELL"), jobs: map[string]tmux.PaneStatus{}, errs: map[string]string{}}, nil
}

func (s *scriptsState) setJobs(j map[string]tmux.PaneStatus) {
	if j == nil {
		j = map[string]tmux.PaneStatus{}
	}
	s.mu.Lock()
	s.jobs = j
	s.mu.Unlock()
}

func (s *scriptsState) job(name string) (tmux.PaneStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.jobs[name]
	return p, ok
}

// workspace is where a row's jobs run: its worktree's root, or its cwd.
func workspace(cwd string) string {
	if _, _, root, ok := worktree.Parse(cwd); ok {
		return root
	}
	return filepath.Clean(cwd)
}

// JobDTO is one job's state; nil when it has never run.
type JobDTO struct {
	Running bool `json:"running"`
	Exit    int  `json:"exit"` // when it has finished
}

// JobsDTO is a row's scripts: present only when its project has some.
type JobsDTO struct {
	Workspace string  `json:"workspace"`
	HasSetup  bool    `json:"hasSetup"`
	HasRun    bool    `json:"hasRun"`
	Setup     *JobDTO `json:"setup,omitempty"`
	Run       *JobDTO `json:"run,omitempty"`
	Port      int     `json:"port,omitempty"` // the workspace's block, once it has one
	SetupErr  string  `json:"setupErr,omitempty"`
}

func (s *scriptsState) info(cwd string) *JobsDTO {
	if s == nil || cwd == "" {
		return nil
	}
	ws := workspace(cwd)
	_, c, ok := s.store.For(ws)
	if !ok {
		return nil
	}
	d := &JobsDTO{Workspace: ws, HasSetup: c.Setup != "", HasRun: c.Run != "", Port: s.store.Ports()[ws]}
	state := func(kind string) *JobDTO {
		p, ok := s.job(scripts.Job(kind, ws))
		if !ok {
			return nil
		}
		return &JobDTO{Running: !p.Dead, Exit: p.ExitCode}
	}
	d.Setup, d.Run = state(scripts.Setup), state(scripts.Run)
	s.mu.Lock()
	d.SetupErr = s.errs[ws]
	s.mu.Unlock()
	return d
}

// ScriptsDTO is a project's scripts for the editor.
type ScriptsDTO struct {
	Root  string `json:"root"`
	Setup string `json:"setup"`
	Run   string `json:"run"`
}

// ProjectScripts returns a project's scripts.
func (a *App) ProjectScripts(root string) (ScriptsDTO, error) {
	if a.scripts == nil {
		return ScriptsDTO{}, errors.New("scripts unavailable")
	}
	c := a.scripts.store.Get(root)
	return ScriptsDTO{Root: root, Setup: c.Setup, Run: c.Run}, nil
}

// SetProjectScripts saves a project's scripts; both empty clears them.
func (a *App) SetProjectScripts(root, setup, run string) error {
	if a.scripts == nil {
		return errors.New("scripts unavailable")
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	return a.scripts.store.Set(root, scripts.Config{Setup: setup, Run: run})
}

// rowCwd is a live or finished session's cwd.
func (a *App) rowCwd(name string) (string, error) {
	if a.st == nil {
		return "", errors.New("store unavailable")
	}
	row, ok, err := a.st.Get(name)
	if err != nil || !ok || row.Cwd == "" {
		return "", fmt.Errorf("no session %q", name)
	}
	return row.Cwd, nil
}

// startJob (re)starts a workspace's job: a finished one is cleared first,
// a running one is refused.
func (a *App) startJob(kind, ws string) (int, error) {
	if a.scripts == nil || a.tm == nil {
		return 0, errors.New("scripts unavailable")
	}
	root, c, ok := a.scripts.store.For(ws)
	script := c.Run
	if kind == scripts.Setup {
		script = c.Setup
	}
	if !ok || script == "" {
		return 0, fmt.Errorf("this project has no %s script", kind)
	}
	if fi, err := os.Stat(ws); err != nil || !fi.IsDir() {
		return 0, fmt.Errorf("%s does not exist", ws)
	}
	name := scripts.Job(kind, ws)
	if a.tm.HasSession(name) {
		if p, err := a.tm.PaneStatus(name); err == nil && !p.Dead {
			return 0, fmt.Errorf("the %s script is already running here", kind)
		}
		_ = a.tm.KillSession(name)
	}
	port := 0
	if kind == scripts.Run {
		var err error
		if port, err = a.scripts.store.Port(ws, false); err != nil {
			return 0, err
		}
	}
	if err := a.tm.EnsureServer(); err != nil {
		return 0, err
	}
	return port, a.tm.NewSession(name, ws, scripts.Command(script, a.scripts.shell, root, ws, port), 200, 50)
}

// RunScript starts the run script in a session's workspace and returns its
// first port.
func (a *App) RunScript(name string) (int, error) {
	cwd, err := a.rowCwd(name)
	if err != nil {
		return 0, err
	}
	return a.startJob(scripts.Run, workspace(cwd))
}

// RunSetup runs the setup script in a session's workspace again.
func (a *App) RunSetup(name string) error {
	cwd, err := a.rowCwd(name)
	if err != nil {
		return err
	}
	ws := workspace(cwd)
	if _, err = a.startJob(scripts.Setup, ws); err == nil {
		a.scripts.mu.Lock()
		delete(a.scripts.errs, ws)
		a.scripts.mu.Unlock()
	}
	return err
}

// StopJob kills a session's workspace job (kind setup or run).
func (a *App) StopJob(name, kind string) error {
	if a.tm == nil {
		return errors.New("tmux unavailable")
	}
	if kind != scripts.Setup && kind != scripts.Run {
		return fmt.Errorf("unknown job %q", kind)
	}
	cwd, err := a.rowCwd(name)
	if err != nil {
		return err
	}
	job := scripts.Job(kind, workspace(cwd))
	if !a.tm.HasSession(job) {
		return nil
	}
	return a.tm.KillSession(job)
}

// JobOutput is the last of a session's workspace job's log, scrollback
// included (a login shell may clear the screen before the script runs).
func (a *App) JobOutput(name, kind string) (string, error) {
	if a.tm == nil {
		return "", errors.New("tmux unavailable")
	}
	cwd, err := a.rowCwd(name)
	if err != nil {
		return "", err
	}
	job := scripts.Job(kind, workspace(cwd))
	if !a.tm.HasSession(job) {
		return "", fmt.Errorf("the %s script has not run here", kind)
	}
	out, err := a.tm.CaptureHistory(job, 2000)
	return strings.Trim(out, "\n"), err
}

// jobAlive names a live job in the workspace of cwd, or "".
func (a *App) jobAlive(cwd string) string {
	if a.tm == nil || cwd == "" {
		return ""
	}
	ws := workspace(cwd)
	for _, kind := range []string{scripts.Setup, scripts.Run} {
		name := scripts.Job(kind, ws)
		if !a.tm.HasSession(name) {
			continue
		}
		if p, err := a.tm.PaneStatus(name); err == nil && !p.Dead {
			return kind
		}
	}
	return ""
}

// checkedOut reports whether git has finished making the worktree at dir.
// `git worktree add` writes the .git file first and checks the tree out
// after, writing the worktree's index last; setup waits for that index so it
// never runs in a half-made directory.
func checkedOut(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return false
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	_, err = os.Stat(filepath.Join(gitdir, "index"))
	return err == nil
}

// setupAfterLaunch runs the setup script in a new worktree session's
// worktree once claude has created it. claude makes the worktree as it
// starts, so this waits for it to be checked out, up to a minute; a launch that
// never makes one is left alone.
func (a *App) setupAfterLaunch(name string) {
	if a.scripts == nil {
		return
	}
	cwd, err := a.rowCwd(name)
	if err != nil {
		return
	}
	if _, c, ok := a.scripts.store.For(cwd); !ok || c.Setup == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for {
		if checkedOut(cwd) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
	ws := workspace(cwd)
	_, err = a.startJob(scripts.Setup, ws)
	a.scripts.mu.Lock()
	if err != nil {
		a.scripts.errs[ws] = err.Error()
	} else {
		delete(a.scripts.errs, ws)
	}
	a.scripts.mu.Unlock()
}
