package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/forge"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/worktree"
)

func TestLedgerSettleAndCleanup(t *testing.T) {
	a := newTestApp(t)
	a.ledger = newLedgerStore(filepath.Join(t.TempDir(), "ledger.json"))
	merged := map[string]string{} // branch -> the merged PR's head
	a.prs = newPRCache(a.now, func(ctx context.Context, dir, branch string) (*forge.PR, error) {
		if h, ok := merged[branch]; ok {
			return &forge.PR{Kind: "PR", Number: 7, State: "merged", Head: h}, nil
		}
		return nil, nil
	})

	repo, _ := filepath.EvalSymlinks(t.TempDir())
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "a")
	wt := func(name string) string {
		root := filepath.Join(repo, worktree.Dir, name)
		gitT(t, repo, "worktree", "add", "-q", "-b", "worktree-"+name, root)
		return root
	}
	done, orphan, squash := wt("loom-aaaaaaaa"), wt("loom-bbbbbbbb"), wt("loom-cccccccc")
	a.st.Upsert(store.SessionRow{Name: "loom-a", Cwd: done, CreatedAt: 1, EndedAt: 2, LastStatus: "done", Title: "fix it"})
	// orphan: no session row, a commit on no other branch, no PR
	os.WriteFile(filepath.Join(orphan, "f"), []byte("x"), 0o644)
	gitT(t, orphan, "add", "f")
	gitT(t, orphan, "commit", "-q", "-m", "b")
	// squash: the same, but its PR has merged
	os.WriteFile(filepath.Join(squash, "g"), []byte("x"), 0o644)
	gitT(t, squash, "add", "g")
	gitT(t, squash, "commit", "-q", "-m", "c")
	merged["worktree-loom-cccccccc"] = gitT(t, squash, "rev-parse", "HEAD")
	// late: its PR merged, then a commit was made that the PR never took
	late := wt("loom-eeeeeeee")
	gitT(t, late, "commit", "-q", "--allow-empty", "-m", "in the PR")
	merged["worktree-loom-eeeeeeee"] = gitT(t, late, "rev-parse", "HEAD")
	gitT(t, late, "commit", "-q", "--allow-empty", "-m", "after the merge")

	rows, err := a.Ledger()
	if err != nil || len(rows) != 4 {
		t.Fatalf("Ledger = %+v, %v", rows, err)
	}
	if rows[0].Root != orphan || !rows[0].Orphan || rows[0].Unique != 1 || rows[0].Ahead != 1 || rows[0].Base != "main" {
		t.Fatalf("orphan row first: %+v", rows[0])
	}
	byRoot := func() map[string]LedgerRowDTO {
		rows, _ := a.Ledger()
		m := map[string]LedgerRowDTO{}
		for _, r := range rows {
			m[r.Root] = r
		}
		return m
	}
	if r := byRoot()[done]; r.Session != "loom-a" || r.Title != "fix it" || r.Orphan || r.Blocker != "" || r.Settled {
		t.Fatalf("done row: %+v", r)
	}

	// settle is refused while work could be lost
	if err := a.Settle(orphan); err == nil || !strings.Contains(err.Error(), "1 commit on no other branch") {
		t.Fatalf("orphan settle: %v", err)
	}
	os.WriteFile(filepath.Join(done, "dirty"), []byte("x"), 0o644)
	if err := a.Settle(done); err == nil || !strings.Contains(err.Error(), "1 changed") {
		t.Fatalf("dirty settle: %v", err)
	}
	os.Remove(filepath.Join(done, "dirty"))
	if err := a.Settle(done); err != nil {
		t.Fatal(err)
	}

	// the merged PR settles its row by itself, once the lookup is back
	a.prs.wg.Wait()
	m := byRoot()
	if r := m[squash]; !r.Settled || !r.Auto || r.Blocker != "" || !r.Covered {
		t.Fatalf("merged row not auto-settled: %+v", r)
	}
	if r := m[late]; r.Settled || r.Covered || !strings.Contains(r.Blocker, "2 commits on no other branch") {
		t.Fatalf("a commit after the merge must block: %+v", r)
	}
	// undo: kept, and not settled again by the merged PR
	if err := a.Unsettle(squash); err != nil {
		t.Fatal(err)
	}
	if r := byRoot()[squash]; r.Settled {
		t.Fatalf("undone row settled again: %+v", r)
	}
	if err := a.Settle(squash); err != nil {
		t.Fatal(err)
	}

	res, err := a.CleanupSettled()
	if err != nil || len(res) != 2 {
		t.Fatalf("cleanup = %+v, %v", res, err)
	}
	for _, r := range res {
		if r.Err != "" {
			t.Fatalf("cleanup %s: %s", r.Name, r.Err)
		}
		if _, err := os.Stat(r.Root); !os.IsNotExist(err) {
			t.Fatalf("%s still on disk", r.Root)
		}
	}
	if m := byRoot(); len(m) != 2 || !m[orphan].Orphan {
		t.Fatalf("after cleanup: %+v", m)
	}
	// branches are kept
	gitT(t, repo, "rev-parse", "--verify", "worktree-loom-cccccccc")
}

func TestLedgerUnsettlesWhenWorkReturns(t *testing.T) {
	a := newTestApp(t)
	a.ledger = newLedgerStore("")
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "a")
	root := filepath.Join(repo, worktree.Dir, "loom-dddddddd")
	gitT(t, repo, "worktree", "add", "-q", "-b", "worktree-loom-dddddddd", root)
	a.st.Upsert(store.SessionRow{Name: "loom-d", Cwd: root, CreatedAt: 1, EndedAt: 2, LastStatus: "done"})
	if err := a.Settle(root); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "new"), []byte("x"), 0o644)
	rows, _ := a.Ledger()
	if len(rows) != 1 || rows[0].Settled || rows[0].Blocker == "" {
		t.Fatalf("settled row with new work: %+v", rows)
	}
	if _, ok := a.ledger.get(root); ok {
		t.Fatal("settle mark kept")
	}
}

// A repo reached through a symlink: git lists the worktree by its resolved
// path, the row keeps the symlinked one. The row must still join its
// session, and a live session must still block settling.
func TestLedgerMatchesThroughSymlinks(t *testing.T) {
	a := newTestApp(t)
	a.ledger = newLedgerStore("")
	real, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(real, "r")
	os.MkdirAll(repo, 0o755)
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "a")
	link := filepath.Join(t.TempDir(), "lnk")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(link, "r", worktree.Dir, "loom-ffffffff")
	gitT(t, filepath.Join(link, "r"), "worktree", "add", "-q", "-b", "worktree-loom-ffffffff", root)
	a.st.Upsert(store.SessionRow{Name: "loom-f", Cwd: root, CreatedAt: 1, EndedAt: -1, LastStatus: "running"})
	rows, err := a.Ledger()
	if err != nil || len(rows) != 1 {
		t.Fatalf("Ledger = %+v, %v", rows, err)
	}
	r := rows[0]
	if r.Session != "loom-f" || !r.Live || r.Blocker == "" {
		t.Fatalf("symlinked row not joined: %+v", r)
	}
	if err := a.Settle(r.Root); err == nil {
		t.Fatal("settled while its session runs")
	}
}
