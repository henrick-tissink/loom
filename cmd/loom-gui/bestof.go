package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/bestof"
	"github.com/henricktissink/loom/internal/gitdiff"
	"github.com/henricktissink/loom/internal/scripts"
	"github.com/henricktissink/loom/internal/worktree"
)

// Best of N (internal/bestof). The launcher starts one seed as two or three
// worktree sessions; their rows carry the group, the compare view shows each
// attempt's whole change since the shared base side by side, and Keep stops
// the others and removes their worktrees (branches kept). Nothing judges the
// attempts: the person reads them and picks.

type bestofState struct {
	store *bestof.Store
}

func newBestofState(loomDir string) (*bestofState, error) {
	st, err := bestof.Open(filepath.Join(loomDir, "bestof.json"))
	if err != nil {
		return nil, err
	}
	return &bestofState{store: st}, nil
}

// BestOfDTO is a row's place in its group.
type BestOfDTO struct {
	Group string `json:"group"`
	Index int    `json:"index"`
	Of    int    `json:"of"`
	Kept  string `json:"kept,omitempty"`
}

// places is every grouped session's place, read once per list.
func (b *bestofState) places() map[string]bestof.Place {
	if b == nil {
		return nil
	}
	return b.store.Of()
}

func placeDTO(m map[string]bestof.Place, name string) *BestOfDTO {
	p, ok := m[name]
	if !ok {
		return nil
	}
	return &BestOfDTO{Group: p.Group, Index: p.Index, Of: p.Of, Kept: p.Kept}
}

// launchGap spaces the attempts' launches, so their claudes don't create
// worktrees in the same repository at the same instant.
var launchGap = 500 * time.Millisecond

// LaunchBestOf launches the same seed as n worktree sessions in one group
// and returns their names. A launch that fails part way keeps the attempts
// already started, grouped, and says how many.
func (a *App) LaunchBestOf(repoPath, model, mode, seed string, addDirs []string, n int) ([]string, error) {
	if a.launcher == nil || a.bestof == nil {
		return nil, errors.New("launcher unavailable")
	}
	if n < 2 || n > bestof.Max {
		return nil, fmt.Errorf("best of %d: pick 2 to %d attempts", n, bestof.Max)
	}
	if strings.TrimSpace(seed) == "" {
		return nil, errors.New("best of N needs a seed prompt: it is what the attempts are compared on")
	}
	r, err := buildRecipe(a.launchableTargets(), repoPath, model, mode, seed, addDirs)
	if err != nil {
		return nil, err
	}
	r.Worktree = true
	base, err := headOf(r.Cwd)
	if err != nil {
		return nil, fmt.Errorf("best of N needs a commit to start from: %w", err)
	}
	g := bestof.Group{ID: bestof.NewID(), Repo: r.Cwd, Base: base, Seed: seed, Created: a.now().Unix()}
	for i := 0; i < n; i++ {
		if i > 0 {
			time.Sleep(launchGap)
		}
		name, lerr := a.launcher.Launch(r, launchCols, launchRows, a.now())
		if name != "" {
			g.Sessions = append(g.Sessions, name)
			go a.setupAfterLaunch(name)
		}
		if lerr != nil {
			err = fmt.Errorf("launched %d of %d: %w", len(g.Sessions), n, lerr)
			break
		}
	}
	if len(g.Sessions) > 0 {
		if perr := a.bestof.store.Put(g); perr != nil && err == nil {
			err = fmt.Errorf("the attempts are running but their group was not saved: %w", perr)
		}
	}
	return g.Sessions, err
}

// headOf is the commit a repository is on.
func headOf(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("%s has no commit", dir)
	}
	return strings.TrimSpace(string(out)), nil
}

// AttemptDTO is one attempt in the compare view.
type AttemptDTO struct {
	Name      string           `json:"name"`
	Cwd       string           `json:"cwd"`
	Branch    string           `json:"branch"`
	Gone      bool             `json:"gone"`      // its worktree no longer exists
	Discarded bool             `json:"discarded"` // removed by Keep
	Diff      gitdiff.RepoDiff `json:"diff"`
}

// CompareDTO is a group for the compare view.
type CompareDTO struct {
	ID       string       `json:"id"`
	Seed     string       `json:"seed"`
	Base     string       `json:"base"`
	Created  int64        `json:"created"`
	Kept     string       `json:"kept,omitempty"`
	Attempts []AttemptDTO `json:"attempts"`
}

// BestOfCompare reads each attempt's change since the group's base, fresh.
func (a *App) BestOfCompare(id string) (CompareDTO, error) {
	if a.bestof == nil {
		return CompareDTO{}, errors.New("best of N unavailable")
	}
	g, ok := a.bestof.store.Get(id)
	if !ok {
		return CompareDTO{}, fmt.Errorf("no best-of group %s", id)
	}
	out := CompareDTO{ID: g.ID, Seed: g.Seed, Base: g.Base, Created: g.Created, Kept: g.Kept, Attempts: make([]AttemptDTO, len(g.Sessions))}
	var wg sync.WaitGroup
	for i, name := range g.Sessions {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			at := AttemptDTO{Name: name, Discarded: contains(g.Discarded, name)}
			cwd, err := a.rowCwd(name)
			if err != nil {
				at.Gone, at.Diff.Error = true, err.Error()
				out.Attempts[i] = at
				return
			}
			at.Cwd = cwd
			if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
				at.Gone, at.Diff.Error = true, "its worktree has been removed"
				out.Attempts[i] = at
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if st, err := worktree.Read(ctx, cwd); err == nil {
				at.Branch = st.Branch
			}
			at.Diff = gitdiff.Against(cwd, branchStart(cwd, at.Branch, g.Base))
			out.Attempts[i] = at
		}(i, name)
	}
	wg.Wait()
	return out, nil
}

// branchStart is the commit an attempt's branch was created at: the oldest
// entry of its reflog. claude may cut the branch from somewhere other than
// the repository's HEAD at launch (a remote's default branch, say), and
// measuring from there would count commits that are not the attempt's. With
// no reflog (logAllRefUpdates off) it falls back to the group's base.
func branchStart(dir, branch, fallback string) string {
	if branch == "" {
		return fallback
	}
	out, err := exec.Command("git", "-C", dir, "reflog", "show", "--format=%H", "refs/heads/"+branch, "--").Output()
	if err != nil {
		return fallback
	}
	lines := strings.Fields(string(out))
	if len(lines) == 0 {
		return fallback
	}
	return lines[len(lines)-1]
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// discardWait bounds how long Keep waits for a killed claude to exit and
// release its worktree.
var discardWait = 10 * time.Second

// KeepBestOf keeps one attempt and discards the others: their sessions and
// scripts are stopped and their worktrees removed even with work in them
// (the confirm in the GUI says so); their branches, and the commits on
// them, are kept. The kept attempt is not touched. An attempt that could
// not be discarded is named in the error and can be kept trying.
func (a *App) KeepBestOf(id, keep string) error {
	if a.bestof == nil {
		return errors.New("best of N unavailable")
	}
	g, ok := a.bestof.store.Get(id)
	if !ok {
		return fmt.Errorf("no best-of group %s", id)
	}
	if !contains(g.Sessions, keep) {
		return fmt.Errorf("%s is not an attempt in this group", keep)
	}
	if g.Kept != "" && g.Kept != keep {
		return fmt.Errorf("this group already kept %s", g.Kept)
	}
	g.Kept = keep
	var errs []string
	for _, name := range g.Sessions {
		if name == keep || contains(g.Discarded, name) {
			continue
		}
		if err := a.discardAttempt(name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		g.Discarded = append(g.Discarded, name)
	}
	if err := a.bestof.store.Put(g); err != nil {
		errs = append(errs, "saving the group: "+err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// discardAttempt stops an attempt's session and scripts and removes its
// worktree. A worktree already gone counts as discarded.
func (a *App) discardAttempt(name string) error {
	cwd, err := a.rowCwd(name)
	if err != nil {
		return err
	}
	if _, _, _, ok := worktree.Parse(cwd); !ok {
		return fmt.Errorf("%s is not a worktree, so it was left alone", cwd)
	}
	// Stop it first: an attempt kept before its claude has made the
	// worktree must not go on to make it and carry on working.
	if a.tm != nil {
		ws := workspace(cwd)
		for _, kind := range []string{scripts.Setup, scripts.Run} {
			if j := scripts.Job(kind, ws); a.tm.HasSession(j) {
				_ = a.tm.KillSession(j)
			}
		}
		if a.tm.HasSession(name) {
			if err := a.tm.KillSession(name); err != nil {
				return err
			}
		}
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), discardWait+10*time.Second)
	defer cancel()
	deadline := time.Now().Add(discardWait)
	for {
		err = worktree.Discard(ctx, cwd)
		// A killed claude takes a moment to exit and free its lock.
		if err == nil || !strings.Contains(err.Error(), "running claude") || time.Now().After(deadline) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}
