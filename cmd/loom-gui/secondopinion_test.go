package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/secondopinion"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

// gate is a fake review that waits for release (or cancel) and returns res.
type gate struct {
	release chan struct{}
	res     secondopinion.Result
	err     error
	dirs    []string
}

func (g *gate) run(ctx context.Context, dir, level string) (secondopinion.Result, error) {
	g.dirs = append(g.dirs, dir+"@"+level)
	select {
	case <-g.release:
		return g.res, g.err
	case <-ctx.Done():
		return secondopinion.Result{}, ctx.Err()
	}
}

func clock() time.Time { return time.Unix(5000, 0) }

func TestSecondOpinionRunsKeepsAndReloads(t *testing.T) {
	keep := t.TempDir()
	g := &gate{release: make(chan struct{}), res: secondopinion.Result{Findings: []secondopinion.Finding{{File: "a.go", Line: 6, Summary: "off by one"}}, Text: "a.go:6 — off by one", CostUSD: 0.05}}
	s := newSecondOpinions(context.Background(), keep, g.run, clock)
	if _, err := s.start("sid1", "/repo", "ultra"); err == nil {
		t.Fatal("started an unoffered level")
	}
	d, err := s.start("sid1", "/repo", "medium")
	if err != nil || d.Status != "running" || d.Level != "medium" || d.Started != 5000 {
		t.Fatalf("start = %+v, %v", d, err)
	}
	if _, err := s.start("sid1", "/repo", "low"); err == nil {
		t.Fatal("two reviews at once for one session")
	}
	if got := s.get("sid1"); got == nil || got.Status != "running" {
		t.Fatalf("while running: %+v", got)
	}
	close(g.release)
	s.wg.Wait()
	got := s.get("sid1")
	if got.Status != "done" || len(got.Findings) != 1 || got.Findings[0].Line != 6 || got.CostUSD != 0.05 || got.Finished != 5000 {
		t.Fatalf("done = %+v", got)
	}
	// a fresh loom reads the kept result
	again := newSecondOpinions(context.Background(), keep, g.run, clock)
	if r := again.get("sid1"); r == nil || r.Status != "done" || r.Findings[0].Summary != "off by one" {
		t.Fatalf("reloaded = %+v", r)
	}
	if again.get("other") != nil {
		t.Error("a result for a session never reviewed")
	}
	// a new run drops the kept result: it is not this code's
	g2 := &gate{release: make(chan struct{})}
	again.run = g2.run
	if _, err := again.start("sid1", "/repo", "low"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(keep, "sid1.json")); !os.IsNotExist(err) {
		t.Errorf("the old result is still kept while the new one runs: %v", err)
	}
	if err := again.stop("sid1"); err != nil {
		t.Fatal(err)
	}
	again.wg.Wait()
	if r := again.get("sid1"); r.Status != "cancelled" {
		t.Errorf("cancelled = %+v", r)
	}
	if err := again.stop("sid1"); err == nil {
		t.Error("stopped a review that is not running")
	}
}

func TestSecondOpinionFailure(t *testing.T) {
	g := &gate{release: make(chan struct{}), err: errors.New("the review failed: Not logged in")}
	s := newSecondOpinions(context.Background(), "", g.run, clock)
	if _, err := s.start("sid", "/repo", "low"); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	s.wg.Wait()
	if r := s.get("sid"); r.Status != "failed" || !strings.Contains(r.Error, "Not logged in") || r.Findings == nil {
		t.Fatalf("failed = %+v", r)
	}
}

func TestSecondOpinionQuitStopsIt(t *testing.T) {
	base, quit := context.WithCancel(context.Background())
	g := &gate{release: make(chan struct{})}
	s := newSecondOpinions(base, "", g.run, clock)
	if _, err := s.start("sid", "/repo", "low"); err != nil {
		t.Fatal(err)
	}
	quit()
	s.wg.Wait()
	if r := s.get("sid"); r.Status != "cancelled" {
		t.Fatalf("after quit = %+v", r)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestSecondOpinionOnTheSheet(t *testing.T) {
	dir := gitRepo(t)
	g := &gate{release: make(chan struct{}), res: secondopinion.Result{None: true, Text: "(none)"}}
	a := &App{opinions: newSecondOpinions(context.Background(), "", g.run, clock)}
	if _, err := a.StartSecondOpinion("loom-1", "low"); err == nil {
		t.Fatal("a review for a session not on the rail")
	}
	a.noteRail([]SessionDTO{{Name: "loom-1"}, {Name: "loom-2"}}, []status.Row{
		{SessionRow: store.SessionRow{Name: "loom-1", ClaudeSessionID: "sid1", Cwd: dir}},
		{SessionRow: store.SessionRow{Name: "loom-2"}},
	})
	if _, err := a.StartSecondOpinion("loom-2", "low"); err == nil {
		t.Fatal("a review for a session with no Claude session")
	}
	if r, _ := a.ReviewSheet("loom-1"); r.SecondOpinion != nil {
		t.Fatalf("a second opinion before any ran: %+v", r.SecondOpinion)
	}
	if _, err := a.StartSecondOpinion("loom-1", "low"); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.SecondOpinion("loom-1"); d == nil || d.Status != "running" || d.Print != "" {
		t.Fatalf("polled = %+v", d)
	}
	close(g.release)
	a.opinions.wg.Wait()
	r, err := a.ReviewSheet("loom-1")
	if err != nil || r.SecondOpinion == nil || r.SecondOpinion.Status != "done" || !r.SecondOpinion.None || r.SecondOpinion.Changed {
		t.Fatalf("sheet = %+v, %v", r.SecondOpinion, err)
	}
	if g.dirs[0] != dir+"@low" {
		t.Errorf("reviewed %v", g.dirs)
	}
	// the session edits after the review: the sheet says so
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.SecondOpinion("loom-1"); !d.Changed {
		t.Error("a new file after the review did not mark it changed")
	}
}

func TestCodePrint(t *testing.T) {
	dir := gitRepo(t)
	p0 := codePrint(dir)
	if p0 == "" || codePrint(dir) != p0 {
		t.Fatal("print is empty or unstable")
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p1 := codePrint(dir)
	if p1 == p0 {
		t.Error("a new file did not change the print")
	}
	// editing a file git does not track yet is a change
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p2 := codePrint(dir)
	if p2 == p1 {
		t.Error("an edit to an untracked file did not change the print")
	}
	// committing the reviewed code is not
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "work"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if codePrint(dir) != p2 {
		t.Error("committing, with no edit, changed the print")
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if codePrint(dir) == p2 {
		t.Error("an edit to a tracked file did not change the print")
	}
	os.Remove(filepath.Join(dir, "f.txt"))
	if codePrint(dir) == p2 {
		t.Error("deleting a file did not change the print")
	}
	if codePrint(t.TempDir()) != "" {
		t.Error("a print for a directory that is not a repo")
	}
}
