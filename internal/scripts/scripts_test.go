package scripts

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreForAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scripts.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("/w/app", Config{Setup: " npm ci ", Run: "npm run dev -- --port $LOOM_PORT"}); err != nil {
		t.Fatal(err)
	}
	s.Set("/w/app/packages/web", Config{Run: "pnpm dev"})
	s2, _ := Open(path)
	if c := s2.Get("/w/app"); c.Setup != "npm ci" {
		t.Fatalf("not persisted/trimmed: %+v", c)
	}
	for dir, want := range map[string]string{
		"/w/app":                     "/w/app",
		"/w/app/.claude/worktrees/x": "/w/app",
		"/w/app/packages/web/src":    "/w/app/packages/web",
		"/w/application":             "",
		"/w":                         "",
	} {
		root, _, ok := s2.For(dir)
		if root != want || ok != (want != "") {
			t.Errorf("For(%s) = %q %v, want %q", dir, root, ok, want)
		}
	}
	s2.Set("/w/app", Config{})
	if _, _, ok := s2.For("/w/app/x"); ok {
		t.Fatal("cleared project still has scripts")
	}
}

func TestPortBlocks(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.Mkdir(a, 0o755)
	os.Mkdir(b, 0o755)
	busy := map[int]bool{First + 3: true} // something else holds a port in the first block
	s, _ := Open(filepath.Join(dir, "scripts.json"))
	s.Free = func(p int) bool { return !busy[p] }

	pa, err := s.Port(a, false)
	if err != nil || pa != First+Block {
		t.Fatalf("a = %d %v (first block has a busy port)", pa, err)
	}
	pb, _ := s.Port(b, false)
	if pb != First+2*Block {
		t.Fatalf("b = %d", pb)
	}
	busy[pa] = true // a's own run holds it
	if p, _ := s.Port(a, true); p != pa {
		t.Fatalf("running a moved to %d", p)
	}
	s2, _ := Open(filepath.Join(dir, "scripts.json"))
	s2.Free = s.Free
	if p, _ := s2.Port(a, true); p != pa {
		t.Fatalf("block not kept across opens: %d", p)
	}
	// Not running and its port taken by someone else: a new block.
	if p, _ := s2.Port(a, false); p == pa || p == pb {
		t.Fatalf("a kept a port another process holds: %d", p)
	}
	// A workspace that is gone gives its block back.
	os.Remove(b)
	c := filepath.Join(dir, "c")
	os.Mkdir(c, 0o755)
	if p, _ := s2.Port(c, false); p != pb {
		t.Fatalf("c = %d, want b's freed %d", p, pb)
	}
}

func TestPortFree(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := l.Addr().(*net.TCPAddr).Port
	if portFree(p) {
		t.Fatal("a listened port reads free")
	}
	l.Close()
	if !portFree(p) {
		t.Fatal("a closed port reads busy")
	}
}

func TestJobAndCommand(t *testing.T) {
	j := Job(Run, "/w/app/.claude/worktrees/x.y")
	if !strings.HasPrefix(j, "loomjob-run-") || strings.ContainsAny(j, ".:") || j != Job(Run, "/w/app/.claude/worktrees/x.y/") {
		t.Fatalf("Job = %q", j)
	}
	if strings.HasPrefix(j, "loom-") {
		t.Fatal("a job name would be adopted as a session")
	}
	// The command runs under sh -c, as tmux runs it; the script sees the
	// variables, and quotes in paths and scripts survive.
	dir := t.TempDir()
	ws := filepath.Join(dir, "it's here")
	cmd := Command(`printf '%s|%s|%s' "$LOOM_PORT" "$LOOM_ROOT_PATH" "$LOOM_WORKSPACE_PATH"`, "/bin/sh", "/r", ws, 42010)
	out, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput()
	if err != nil || string(out) != "42010|/r|"+ws {
		t.Fatalf("%q: %q %v", cmd, out, err)
	}
	if c := Command("true", "", "/r", "/w", 0); strings.Contains(c, "LOOM_PORT") || !strings.Contains(c, "'/bin/zsh'") {
		t.Fatalf("setup command: %s", c)
	}
}
