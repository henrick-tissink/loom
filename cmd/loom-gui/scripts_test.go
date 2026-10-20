package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/scripts"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

// waitFile waits for a file to have content.
func waitFile(t *testing.T, p string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never written", p)
	return ""
}

func TestScriptsRunStopSetup(t *testing.T) {
	tm := &tmux.Client{Socket: fmt.Sprintf("loomscr%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := newApp(nil, tm, st, nil, nil, time.Now)
	sc, err := newScriptsState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc.shell = "/bin/sh"
	a.scripts = sc

	repo := t.TempDir()
	wt := filepath.Join(repo, ".claude", "worktrees", "w1")
	st.Upsert(store.SessionRow{Name: "loom-a", Cwd: repo, CreatedAt: 1, EndedAt: -1})
	st.Upsert(store.SessionRow{Name: "loom-b", Cwd: wt, CreatedAt: 1, EndedAt: -1})

	if _, err := a.RunScript("loom-a"); err == nil {
		t.Fatal("ran with no script")
	}
	if err := a.SetProjectScripts(repo, `printf ok > "$LOOM_WORKSPACE_PATH/setup.txt"; exit 7`,
		`printf '%s %s' "$LOOM_PORT" "$LOOM_ROOT_PATH" > port.txt; echo serving; sleep 30`); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.ProjectScripts(repo); !strings.HasPrefix(d.Run, "printf") {
		t.Fatalf("ProjectScripts = %+v", d)
	}

	port, err := a.RunScript("loom-a")
	if err != nil || port < scripts.First {
		t.Fatalf("RunScript: %d %v", port, err)
	}
	if got := waitFile(t, filepath.Join(repo, "port.txt")); got != strconv.Itoa(port)+" "+repo {
		t.Fatalf("the script saw %q", got)
	}
	if _, err := a.RunScript("loom-a"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second run: %v", err)
	}
	if out, _ := a.JobOutput("loom-a", "run"); !strings.Contains(out, "serving") {
		t.Fatalf("output %q", out)
	}
	if a.jobAlive(repo) != "run" {
		t.Fatal("jobAlive missed the run")
	}

	// The worktree is its own workspace: its own job and its own ports.
	if _, err := a.RunScript("loom-b"); err == nil {
		t.Fatal("ran in a worktree that does not exist yet")
	}
	go a.setupAfterLaunch("loom-b")
	time.Sleep(300 * time.Millisecond)
	// git writes .git before the checkout and the index after it: setup
	// waits for the index.
	gd := filepath.Join(repo, ".git", "worktrees", "w1")
	os.MkdirAll(wt, 0o755)
	os.MkdirAll(gd, 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gd+"\n"), 0o644)
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(wt, "setup.txt")); err == nil {
		t.Fatal("setup ran before the checkout finished")
	}
	os.WriteFile(filepath.Join(gd, "index"), []byte("DIRC"), 0o644)
	if waitFile(t, filepath.Join(wt, "setup.txt")) != "ok" {
		t.Fatal("setup did not run in the worktree")
	}
	pb, err := a.RunScript("loom-b")
	if err != nil || pb == port {
		t.Fatalf("worktree port %d (repo %d) %v", pb, port, err)
	}
	waitFile(t, filepath.Join(wt, "port.txt"))

	// The rows read the engine's job list; feed it what tmux says.
	deadline := time.Now().Add(5 * time.Second)
	for {
		panes, _ := tm.ListPanes()
		jobs := map[string]tmux.PaneStatus{}
		for _, p := range panes {
			jobs[p.Name] = p.PaneStatus
		}
		sc.setJobs(jobs)
		d := sc.info(wt)
		if d != nil && d.Setup != nil && !d.Setup.Running {
			if d.Setup.Exit != 7 || d.Run == nil || !d.Run.Running || d.Port != pb || d.Workspace != wt || !d.HasSetup || !d.HasRun {
				t.Fatalf("info: %+v setup %+v run %+v", d, d.Setup, d.Run)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup never finished: %+v", d)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := a.StopJob("loom-a", "run"); err != nil {
		t.Fatal(err)
	}
	if a.jobAlive(repo) != "" || tm.HasSession(scripts.Job(scripts.Run, repo)) {
		t.Fatal("Stop left the run")
	}
	// The same workspace gets its block back.
	os.Remove(filepath.Join(repo, "port.txt"))
	if p2, err := a.RunScript("loom-a"); err != nil || p2 != port {
		t.Fatalf("restart port %d, want %d (%v)", p2, port, err)
	}
	if sc.info(t.TempDir()) != nil {
		t.Fatal("a directory with no project scripts has jobs")
	}
}
