package delegate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// §9.2's execution half. Every test here builds its own repo under t.TempDir()
// for the reason worktree_test.go's header gives: `git worktree add` writes into
// whatever repo it is pointed at, and that must never be the developer's
// checkout.

// producerBranch commits one file on its own branch off main and returns the ref
// the way PlanBase would record it — branch AND sha, because MergeProducers is
// specified to merge the sha.
func producerBranch(t *testing.T, repo, name, file, body string) ProducerRef {
	t.Helper()
	mustGit(t, repo, "checkout", "-q", "-b", name, "main")
	writeFile(t, filepath.Join(repo, file), body)
	mustGit(t, repo, "add", file)
	mustGit(t, repo, "commit", "-qm", "producer "+name)
	sha := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD"))
	mustGit(t, repo, "checkout", "-q", "main")
	return ProducerRef{Task: name, Branch: name, SHA: sha}
}

// consumerWorktree is the tree the producers get merged into.
func consumerWorktree(t *testing.T, repo string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "consumer")
	mustGit(t, repo, "worktree", "add", "-q", "-b", "consumer", dir, "main")
	return dir
}

// The normal shape: `needs` is a list, so multiple producers is not the exotic
// case. All of them must be in the tree.
func TestMergeProducersMergesEveryProducer(t *testing.T) {
	repo := newScratchRepo(t)
	schema := producerBranch(t, repo, "schema", "schema.sql", "create table t;\n")
	config := producerBranch(t, repo, "config", "config.yaml", "key: value\n")
	dir := consumerWorktree(t, repo)

	if err := MergeProducers(dir, []ProducerRef{schema, config}); err != nil {
		t.Fatalf("MergeProducers: %v", err)
	}
	for _, f := range []string{"schema.sql", "config.yaml", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s missing from the consumer tree: %v", f, err)
		}
	}
}

// The sha is merged, not the branch. This is the property that makes a re-spawn
// reproduce the same tree: a producer whose branch has since advanced (§10.3
// sends it back to work) must contribute the sha that was PLANNED.
func TestMergeProducersMergesTheShaNotTheBranchHead(t *testing.T) {
	repo := newScratchRepo(t)
	ref := producerBranch(t, repo, "schema", "schema.sql", "v1\n")

	// The producer goes back to work and its branch head advances.
	mustGit(t, repo, "checkout", "-q", "schema")
	writeFile(t, filepath.Join(repo, "later.txt"), "after the plan was made\n")
	mustGit(t, repo, "add", "later.txt")
	mustGit(t, repo, "commit", "-qm", "producer keeps working")
	mustGit(t, repo, "checkout", "-q", "main")

	dir := consumerWorktree(t, repo)
	if err := MergeProducers(dir, []ProducerRef{ref}); err != nil {
		t.Fatalf("MergeProducers: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "schema.sql")); err != nil {
		t.Errorf("the planned commit is not in the tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "later.txt")); err == nil {
		t.Error("merged the branch head, not the recorded sha: a re-spawn would not reproduce this tree")
	}
}

// §9.2's HARD STOP. Two producers disagreeing about the same lines is
// information for a human, not something a child absorbs.
func TestMergeProducersConflictIsAHardStop(t *testing.T) {
	repo := newScratchRepo(t)
	a := producerBranch(t, repo, "alpha", "shared.txt", "alpha wins\n")
	b := producerBranch(t, repo, "beta", "shared.txt", "beta wins\n")
	dir := consumerWorktree(t, repo)

	err := MergeProducers(dir, []ProducerRef{a, b})
	if err == nil {
		t.Fatal("conflicting producers merged cleanly")
	}
	var conflict *ProducerConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("got %T (%v), want *ProducerConflict", err, err)
	}
	if got := conflict.Files; len(got) != 1 || got[0] != "shared.txt" {
		t.Errorf("Files = %v, want [shared.txt]", got)
	}
	// Between names both, in merge order, so the human is told which one was
	// already in the tree.
	if len(conflict.Between) != 2 ||
		conflict.Between[0].Task != "alpha" || conflict.Between[1].Task != "beta" {
		t.Errorf("Between = %+v, want alpha then beta", conflict.Between)
	}
	// And the tree is left clean rather than half-merged: handing a child a
	// worktree with conflict markers in it is worse than not spawning.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	status := mustGit(t, dir, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("worktree left dirty after an aborted merge:\n%s", status)
	}
}

// Create is re-runnable from (run, task) alone, so the merges inside it must be
// too — the re-spawn path reuses a branch that already carries them.
func TestMergeProducersIsIdempotent(t *testing.T) {
	repo := newScratchRepo(t)
	ref := producerBranch(t, repo, "schema", "schema.sql", "create table t;\n")
	dir := consumerWorktree(t, repo)

	if err := MergeProducers(dir, []ProducerRef{ref}); err != nil {
		t.Fatalf("first merge: %v", err)
	}
	before := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD"))
	if err := MergeProducers(dir, []ProducerRef{ref}); err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if after := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD")); after != before {
		t.Errorf("re-merging moved HEAD %s -> %s; the re-spawn path is not idempotent", before, after)
	}
}

// A failure that is not a disagreement between producers must not be dressed up
// as one: ProducerConflict routes a human to a planning decision they would not
// actually have.
func TestMergeProducersNonConflictFailuresAreNotProducerConflicts(t *testing.T) {
	repo := newScratchRepo(t)
	dir := consumerWorktree(t, repo)

	tests := []struct {
		name string
		ref  ProducerRef
	}{
		{"no recorded sha", ProducerRef{Task: "schema", Branch: "schema"}},
		{"sha not in repo", ProducerRef{Task: "schema", Branch: "schema", SHA: strings.Repeat("0", 40)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := MergeProducers(dir, []ProducerRef{tt.ref})
			if err == nil {
				t.Fatal("want an error")
			}
			var conflict *ProducerConflict
			if errors.As(err, &conflict) {
				t.Fatalf("reported as a producer conflict: %v", err)
			}
			if !strings.Contains(err.Error(), "schema") {
				t.Errorf("error does not name the producer: %v", err)
			}
		})
	}
}

// The empty plan is the common case and is not a special path.
func TestMergeProducersEmptyPlanIsANoOp(t *testing.T) {
	repo := newScratchRepo(t)
	dir := consumerWorktree(t, repo)
	before := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD"))
	if err := MergeProducers(dir, nil); err != nil {
		t.Fatalf("MergeProducers(nil): %v", err)
	}
	if after := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD")); after != before {
		t.Errorf("HEAD moved on an empty plan: %s -> %s", before, after)
	}
}

// The whole point of merging INSIDE Create: a worktree is never handed to a
// child between `worktree add` and the producer merges, and bootstrap sees the
// merged tree because producers are exactly what changes a lockfile.
func TestCreateMergesProducersBeforeBootstrap(t *testing.T) {
	repo := newScratchRepo(t)
	ref := producerBranch(t, repo, "schema", "schema.sql", "create table t;\n")
	w := newTestWorktrees(t)

	// Bootstrap records what the tree looked like when it ran.
	c, err := w.Create(Request{
		RunSlug: "run", TaskID: "api", RepoLabel: "app",
		RepoPath: repo,
		Base:     strings.TrimSpace(mustGit(t, repo, "rev-parse", "main")),
		Merge:    []ProducerRef{ref},
		Setup: RepoSetup{Bootstrap: []string{"sh", "-c",
			"test -f schema.sql && echo present > bootstrap-saw.txt"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "schema.sql")); err != nil {
		t.Errorf("the producer's file is not in the child's worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "bootstrap-saw.txt")); err != nil {
		t.Errorf("bootstrap ran before the producer merges: %v", err)
	}
}

// The conflict is reported against the CONSUMER — the task that will not spawn.
func TestCreateReportsProducerConflictAgainstTheConsumer(t *testing.T) {
	repo := newScratchRepo(t)
	a := producerBranch(t, repo, "alpha", "shared.txt", "alpha wins\n")
	b := producerBranch(t, repo, "beta", "shared.txt", "beta wins\n")
	w := newTestWorktrees(t)

	_, err := w.Create(Request{
		RunSlug: "run", TaskID: "api", RepoLabel: "app",
		RepoPath: repo,
		Base:     strings.TrimSpace(mustGit(t, repo, "rev-parse", "main")),
		Merge:    []ProducerRef{a, b},
	})
	if err == nil {
		t.Fatal("Create succeeded with conflicting producers")
	}
	var conflict *ProducerConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("got %T (%v), want *ProducerConflict", err, err)
	}
	if conflict.Task != "api" {
		t.Errorf("Task = %q, want the consumer %q", conflict.Task, "api")
	}
}
