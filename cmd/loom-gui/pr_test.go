package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/forge"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
)

func TestPRCache(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(tr, []byte(`{"type":"user","gitBranch":"feat/x"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	calls := 0
	var fail error
	c := newPRCache(func() time.Time { return now }, func(_ context.Context, d, b string) (*forge.PR, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if d != dir || b != "feat/x" {
			t.Errorf("lookup(%q, %q)", d, b)
		}
		if fail != nil {
			return nil, fail
		}
		return &forge.PR{Kind: "github", Number: 7, State: "open", Checks: "running"}, nil
	})
	// the first ask never waits on the network: nothing yet, a lookup starts
	if d := c.info(dir, tr, true); d != nil {
		t.Fatalf("first ask answered synchronously: %+v", d)
	}
	c.wg.Wait()
	d := c.info(dir, tr, true)
	if d == nil || d.Number != 7 || d.Checks != "running" || d.Branch != "feat/x" {
		t.Fatalf("after the lookup: %+v", d)
	}
	// a second row on the same branch shares the answer, inside the TTL
	c.info(dir, tr, false)
	c.wg.Wait()
	if calls != 1 {
		t.Fatalf("lookups inside the TTL: %d", calls)
	}
	// past the TTL a failure keeps the last answer and says so
	now = now.Add(2 * time.Minute)
	fail = errors.New("gh: not logged in")
	c.info(dir, tr, true)
	c.wg.Wait()
	d = c.info(dir, tr, true)
	if calls != 2 || d == nil || d.Number != 7 || d.Err != "gh: not logged in" {
		t.Fatalf("failed refresh: calls %d, %+v", calls, d)
	}
	// no branch in the transcript: no lookup at all
	none := filepath.Join(dir, "n.jsonl")
	os.WriteFile(none, []byte(`{"type":"user"}`+"\n"), 0o644)
	if d := c.info(dir, none, true); d != nil {
		t.Fatalf("no branch: %+v", d)
	}
	c.wg.Wait()
	if calls != 2 {
		t.Fatalf("looked up without a branch: %d", calls)
	}
}

func TestPRCacheNeverAnswered(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "s.jsonl")
	os.WriteFile(tr, []byte(`{"type":"user","gitBranch":"feat/x"}`+"\n"), 0o644)
	c := newPRCache(time.Now, func(context.Context, string, string) (*forge.PR, error) {
		return &forge.PR{Kind: "gitlab"}, errors.New("glab is not installed")
	})
	c.info(dir, tr, true)
	c.wg.Wait()
	// a forge that never answered is shown as a failure, not as "no PR",
	// and names the forge
	if d := c.info(dir, tr, true); d == nil || d.Number != 0 || d.Kind != "gitlab" || d.Err != "glab is not installed" {
		t.Fatalf("got %+v", d)
	}
}

func TestPRCacheBranchIncremental(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "s.jsonl")
	os.WriteFile(tr, []byte(`{"type":"user","gitBranch":"feat/x"}`+"\n"), 0o644)
	c := newPRCache(time.Now, func(context.Context, string, string) (*forge.PR, error) { return nil, nil })
	if b := c.branch(tr); b != "feat/x" {
		t.Fatalf("first read: %q", b)
	}
	f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0)
	// appended records without a branch keep the known one; a new branch wins
	f.WriteString(`{"type":"user","x":"` + strings.Repeat("y", 10<<10) + `"}` + "\n")
	if b := c.branch(tr); b != "feat/x" {
		t.Fatalf("branch-less append: %q", b)
	}
	f.WriteString(`{"type":"user","gitBranch":"feat/y"}` + "\n")
	f.Close()
	if b := c.branch(tr); b != "feat/y" {
		t.Fatalf("new branch appended: %q", b)
	}
}

func TestReviewPrompt(t *testing.T) {
	cfg, cwd := t.TempDir(), t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := newApp(nil, tmux.New(), st, &session.Launcher{ClaudeConfigDir: cfg}, nil, time.Now)
	sid := "11111111-1111-1111-1111-111111111111"
	st.Upsert(store.SessionRow{Name: "loom-a", ClaudeSessionID: sid, Cwd: cwd, CreatedAt: 1, EndedAt: 2, LastStatus: "done"})
	tr := transcript.Path(cfg, cwd, sid)
	os.MkdirAll(filepath.Dir(tr), 0o755)

	if _, err := app.ReviewPrompt("loom-a"); err == nil || !strings.Contains(err.Error(), "no git branch") {
		t.Fatalf("no branch on record: %v", err)
	}
	os.WriteFile(tr, []byte(`{"type":"user","gitBranch":"feat/x"}`+"\n"), 0o644)
	answers := map[string]string{
		"git remote get-url origin": "https://github.com/a/b\n",
		"git symbolic-ref":          "origin/main\n",
		"gh pr list":                `[{"number":7,"title":"Fix it","state":"OPEN","url":"https://github.com/a/b/pull/7","statusCheckRollup":[]}]`,
		"gh api graphql":            `{"data":{"repository":{"pullRequest":{"commits":{"nodes":[]},"reviews":{"nodes":[]},"comments":{"nodes":[]},"reviewThreads":{"nodes":[{"isResolved":false,"path":"a.go","line":3,"comments":{"nodes":[{"author":{"login":"rev"},"body":"nil check"}]}}]}}}}}`,
	}
	app.forgeRun = func(_ context.Context, _, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		for k, v := range answers {
			if strings.HasPrefix(cmd, k) {
				return []byte(v), nil
			}
		}
		return nil, errors.New("unexpected " + cmd)
	}
	p, err := app.ReviewPrompt("loom-a")
	if err != nil || !strings.Contains(p, "PR #7 (Fix it)") || !strings.Contains(p, "1. a.go:3\n   rev: nil check") {
		t.Fatalf("prompt %q, %v", p, err)
	}
	answers["gh pr list"] = "[]"
	if _, err := app.ReviewPrompt("loom-a"); err == nil || !strings.Contains(err.Error(), "no pull or merge request for branch feat/x") {
		t.Fatalf("no PR: %v", err)
	}
	if err := app.SendPrompt("loom-a", "  "); err == nil {
		t.Fatal("an empty message is refused")
	}
	// a pending permission dialog: the paste's Enter would answer it
	os.MkdirAll(filepath.Join(cfg, "sessions"), 0o755)
	reg := fmt.Sprintf(`{"pid":%d,"sessionId":"%s","cwd":"%s","tmux":"loom-a:@1.%%1","status":"waiting","waitingFor":"dialog open","updatedAt":1}`, os.Getpid(), sid, cwd)
	os.WriteFile(filepath.Join(cfg, "sessions", fmt.Sprintf("%d.json", os.Getpid())), []byte(reg), 0o644)
	if err := app.SendPrompt("loom-a", "review"); err == nil || !strings.Contains(err.Error(), "waiting on a dialog (dialog open)") {
		t.Fatalf("send into an open dialog: %v", err)
	}
}
