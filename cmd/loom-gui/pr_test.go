package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/forge"
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
