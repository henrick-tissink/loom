package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/secondopinion"
)

// A second opinion: Claude Code's own /code-review, run headless and
// read-only in the session's directory, its findings put on the review
// sheet. It runs on its own after the click (closing the sheet does not
// stop it), one at a time per session, and the last result is kept in
// ~/.loom/second-opinions/<claude session id>.json.

// SecondOpinionDTO is a session's latest second opinion.
type SecondOpinionDTO struct {
	Status   string                  `json:"status"` // running | done | failed | cancelled
	Level    string                  `json:"level"`
	Dir      string                  `json:"dir"`
	Started  int64                   `json:"started"`
	Finished int64                   `json:"finished,omitempty"`
	Findings []secondopinion.Finding `json:"findings"`
	None     bool                    `json:"none"`
	Text     string                  `json:"text,omitempty"`
	CostUSD  float64                 `json:"costUsd,omitempty"`
	Error    string                  `json:"error,omitempty"`
	// Changed: the directory's code is not what was reviewed any more.
	Changed bool `json:"changed"`
	// Print is the reviewed working tree's content fingerprint
	// (codePrint); stored, not shown.
	Print string `json:"print,omitempty"`
}

// reviewRunner runs one review; secondopinion.Run, or a fake in tests.
type reviewRunner func(ctx context.Context, dir, level string) (secondopinion.Result, error)

type secondOpinions struct {
	mu     sync.Mutex
	dir    string // where results are kept; "" keeps them in memory only
	base   context.Context
	run    reviewRunner
	now    func() time.Time
	byID   map[string]*SecondOpinionDTO
	cancel map[string]context.CancelFunc
	wg     sync.WaitGroup
}

func newSecondOpinions(base context.Context, dir string, run reviewRunner, now func() time.Time) *secondOpinions {
	return &secondOpinions{dir: dir, base: base, run: run, now: now,
		byID: map[string]*SecondOpinionDTO{}, cancel: map[string]context.CancelFunc{}}
}

func (s *secondOpinions) path(sid string) string {
	return filepath.Join(s.dir, sid+".json")
}

// get is sid's latest second opinion, or nil.
func (s *secondOpinions) get(sid string) *SecondOpinionDTO {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.byID[sid]
	if d == nil && s.dir != "" {
		if b, err := os.ReadFile(s.path(sid)); err == nil {
			var r SecondOpinionDTO
			if json.Unmarshal(b, &r) == nil && r.Status != "running" {
				d = &r
				s.byID[sid] = d
			}
		}
	}
	if d == nil {
		return nil
	}
	c := *d
	return &c
}

// start begins a review of dir for sid. One at a time per session.
func (s *secondOpinions) start(sid, dir, level string) (*SecondOpinionDTO, error) {
	if !secondopinion.ValidLevel(level) {
		return nil, fmt.Errorf("unknown review level %q", level)
	}
	s.mu.Lock()
	if _, busy := s.cancel[sid]; busy {
		s.mu.Unlock()
		return nil, errors.New("a second opinion is already running for this session")
	}
	ctx, cancel := context.WithCancel(s.base)
	d := &SecondOpinionDTO{Status: "running", Level: level, Dir: dir, Started: s.now().Unix(), Findings: []secondopinion.Finding{}, Print: codePrint(dir)}
	s.byID[sid], s.cancel[sid] = d, cancel
	out := *d
	s.wg.Add(1)
	s.mu.Unlock()
	if s.dir != "" {
		_ = os.Remove(s.path(sid)) // the old result is not this code's
	}
	go func() {
		defer s.wg.Done()
		res, err := s.run(ctx, dir, level)
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.cancel, sid)
		stopped := ctx.Err() == context.Canceled // read before cancel() below
		cancel()
		fin := *d
		fin.Finished = s.now().Unix()
		switch {
		case stopped:
			fin.Status, fin.Error = "cancelled", "cancelled"
		case err != nil:
			fin.Status, fin.Error = "failed", err.Error()
		default:
			fin.Status, fin.Findings, fin.None, fin.Text, fin.CostUSD = "done", res.Findings, res.None, res.Text, res.CostUSD
			if fin.Findings == nil {
				fin.Findings = []secondopinion.Finding{}
			}
		}
		s.byID[sid] = &fin
		s.save(sid, &fin)
	}()
	return &out, nil
}

// save keeps a finished result; caller holds mu.
func (s *secondOpinions) save(sid string, d *SecondOpinionDTO) {
	if s.dir == "" {
		return
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil || os.MkdirAll(s.dir, 0o700) != nil {
		return
	}
	tmp := s.path(sid) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path(sid))
	}
}

// stop cancels sid's running review.
func (s *secondOpinions) stop(sid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cancel[sid]
	if !ok {
		return errors.New("no second opinion is running for this session")
	}
	c()
	return nil
}

// codePrint fingerprints the content of dir's working tree: each file's
// blob id, from the index when git sees it unchanged, hashed from disk
// when it is modified or new (not ignored). Committing reviewed work
// leaves it as it was; editing any file, a new one included, changes it.
// Nothing is written to the repo (hash-object without -w). "" when dir is
// not a git repo.
func codePrint(dir string) string {
	git := func(stdin string, args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.quotepath=off"}, args...)...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		out, err := cmd.Output()
		return string(out), err
	}
	staged, err := git("", "ls-files", "-s", "-z")
	if err != nil {
		return ""
	}
	blob := map[string]string{}
	for _, e := range strings.Split(staged, "\x00") {
		meta, path, ok := strings.Cut(e, "\t") // "mode blob stage\tpath"
		if f := strings.Fields(meta); ok && len(f) == 3 {
			blob[path] = f[1]
		}
	}
	changed, err := git("", "ls-files", "-m", "-o", "--exclude-standard", "-z")
	if err != nil {
		return ""
	}
	var hashable []string
	for _, p := range strings.Split(changed, "\x00") {
		if p == "" {
			continue
		}
		if fi, err := os.Lstat(filepath.Join(dir, p)); err != nil {
			blob[p] = "deleted"
		} else if fi.Mode().IsRegular() && !strings.ContainsAny(p, "\n") {
			hashable = append(hashable, p)
		}
	}
	if len(hashable) > 0 {
		out, err := git(strings.Join(hashable, "\n")+"\n", "hash-object", "--stdin-paths")
		if err != nil {
			return ""
		}
		ids := strings.Fields(out)
		if len(ids) != len(hashable) {
			return ""
		}
		for i, p := range hashable {
			blob[p] = ids[i]
		}
	}
	paths := make([]string, 0, len(blob))
	for p := range blob {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00%s\x00", p, blob[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sessionReviewTarget is the session's Claude id and the directory a
// review reads (its working directory; add-dirs are not reviewed).
func (a *App) sessionReviewTarget(name string) (sid, dir string, err error) {
	e, ok := a.railRow(name)
	if !ok {
		return "", "", fmt.Errorf("%s is not on the rail any more", name)
	}
	if e.row.ClaudeSessionID == "" || e.row.Cwd == "" {
		return "", "", fmt.Errorf("%s has no Claude session or working directory yet", name)
	}
	return e.row.ClaudeSessionID, e.row.Cwd, nil
}

// StartSecondOpinion starts native /code-review at level on a session's
// working directory.
func (a *App) StartSecondOpinion(name, level string) (*SecondOpinionDTO, error) {
	if a.opinions == nil {
		return nil, errors.New("second opinions are not available")
	}
	sid, dir, err := a.sessionReviewTarget(name)
	if err != nil {
		return nil, err
	}
	return a.opinions.start(sid, dir, level)
}

// CancelSecondOpinion stops a session's running second opinion.
func (a *App) CancelSecondOpinion(name string) error {
	if a.opinions == nil {
		return errors.New("second opinions are not available")
	}
	sid, _, err := a.sessionReviewTarget(name)
	if err != nil {
		return err
	}
	return a.opinions.stop(sid)
}

// SecondOpinion is a session's latest second opinion, or nil; the sheet
// polls it while one runs.
func (a *App) SecondOpinion(name string) (*SecondOpinionDTO, error) {
	sid, _, err := a.sessionReviewTarget(name)
	if err != nil || a.opinions == nil {
		return nil, err
	}
	return a.opinionFor(sid), nil
}

// opinionFor reads sid's second opinion and whether its code has changed.
func (a *App) opinionFor(sid string) *SecondOpinionDTO {
	if a.opinions == nil || sid == "" {
		return nil
	}
	d := a.opinions.get(sid)
	if d == nil {
		return nil
	}
	if d.Status != "running" && d.Print != "" {
		d.Changed = codePrint(d.Dir) != d.Print
	}
	d.Print = ""
	return d
}
