package delegate

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/gitdiff"
)

// manifestFor builds the two-task, one-repo manifest every case below varies.
func manifestFor(paths, siblingPaths []string) (Manifest, Task) {
	subject := Task{ID: "schema", Repo: "api", Paths: paths}
	sibling := Task{ID: "handlers", Repo: "api", Paths: siblingPaths}
	// A third task in ANOTHER repo, present in every case, so the "same repo
	// only" rule is exercised by every row rather than by one dedicated test.
	elsewhere := Task{ID: "web", Repo: "ui", Paths: []string{"**"}}
	return Manifest{Name: "atlas", Tasks: []Task{subject, sibling, elsewhere}}, subject
}

// §12.3.1-2 is in slice 3a (§2). The detector answers two questions about a
// task's own branch relative to its pinned base: what did it touch that it did
// not declare, and what did it touch that a SIBLING declared. The second is the
// one worth the mechanism — it predicts the merge conflict before integration
// reaches it.
func TestTaskDivergence(t *testing.T) {
	cases := []struct {
		name         string
		declared     []string
		siblingPaths []string
		commit       []string // repo-relative paths the child commits
		untracked    []string // repo-relative paths the child leaves uncommitted
		wantOutside  []string
		wantSiblings map[string][]string
	}{
		{
			name:     "everything inside the declared paths is clean",
			declared: []string{"db/**"}, siblingPaths: []string{"http/**"},
			commit: []string{"db/schema.sql", "db/migrations/1.sql"},
		},
		{
			name:     "a bare directory covers everything beneath it",
			declared: []string{"db"}, siblingPaths: []string{"http/**"},
			commit: []string{"db/a/b/c.sql"},
		},
		{
			name:     "a file outside the declared paths is reported",
			declared: []string{"db/**"}, siblingPaths: []string{"http/**"},
			commit:      []string{"db/schema.sql", "README.md"},
			wantOutside: []string{"README.md"},
		},
		{
			name:     "a file inside a sibling's paths is BOTH outside and a sibling hit",
			declared: []string{"db/**"}, siblingPaths: []string{"http/**"},
			commit:       []string{"http/router.go"},
			wantOutside:  []string{"http/router.go"},
			wantSiblings: map[string][]string{"handlers": {"http/router.go"}},
		},
		{
			name: "overlapping declarations: inside BOTH is a sibling hit and not outside",
			// The manifest's own overlap check warns about this at load; the
			// detector must still report the collision, because a warning the
			// author clicked through is not evidence about what happened.
			declared: []string{"http/**"}, siblingPaths: []string{"http/**"},
			commit:       []string{"http/router.go"},
			wantSiblings: map[string][]string{"handlers": {"http/router.go"}},
		},
		{
			name:     "an untracked file counts as touched",
			declared: []string{"db/**"}, siblingPaths: []string{"http/**"},
			commit:      []string{"db/schema.sql"},
			untracked:   []string{"scratch.txt"},
			wantOutside: []string{"scratch.txt"},
		},
		{
			name: "declaring nothing makes every touched file outside",
			// The rejected alternative — "declared nothing" means "declared
			// everything" — turns the detector silently off exactly when the
			// manifest is least specific.
			declared: nil, siblingPaths: []string{"http/**"},
			commit:      []string{"db/schema.sql"},
			wantOutside: []string{"db/schema.sql"},
		},
		{
			name:     "a task that changed nothing diverges in no direction",
			declared: []string{"db/**"}, siblingPaths: []string{"http/**"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := scratchRepo(t)
			base := headSHA(t, repo)
			for _, p := range tc.commit {
				write(t, filepath.Join(repo, p), "child wrote this\n")
				gitIn(t, repo, "add", "--", p)
			}
			if len(tc.commit) > 0 {
				gitIn(t, repo, "commit", "-qm", "child work")
			}
			for _, p := range tc.untracked {
				write(t, filepath.Join(repo, p), "uncommitted\n")
			}

			m, subject := manifestFor(tc.declared, tc.siblingPaths)
			got, err := TaskDivergence(repo, base, m, subject)
			if err != nil {
				t.Fatalf("TaskDivergence: %v", err)
			}
			if !reflect.DeepEqual(nilIfEmpty(got.Outside), nilIfEmpty(tc.wantOutside)) {
				t.Errorf("Outside = %v, want %v", got.Outside, tc.wantOutside)
			}
			if !reflect.DeepEqual(got.Siblings, tc.wantSiblings) {
				t.Errorf("Siblings = %v, want %v", got.Siblings, tc.wantSiblings)
			}
			if got.Empty() != (len(tc.wantOutside) == 0 && len(tc.wantSiblings) == 0) {
				t.Errorf("Empty() = %v, which disagrees with the lists", got.Empty())
			}
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// A failure to COMPUTE must be an error, never an empty report. Empty is the
// answer a human acts on at the merge gate; a broken capture that renders as
// empty is a detector that silently reports clean.
func TestTaskDivergenceFailsLoudlyRatherThanReportingClean(t *testing.T) {
	repo := scratchRepo(t)
	m, subject := manifestFor([]string{"db/**"}, nil)

	cases := []struct {
		name      string
		worktree  string
		base      string
		wantInErr string
	}{
		{name: "no worktree", worktree: "", base: "HEAD", wantInErr: "worktree"},
		{name: "no pinned base", worktree: repo, base: "", wantInErr: "pinned base"},
		{name: "not a git repository", worktree: t.TempDir(), base: "deadbeef", wantInErr: "divergence"},
		{name: "a base that is not in this repo", worktree: repo,
			base: "0000000000000000000000000000000000000000", wantInErr: "divergence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TaskDivergence(tc.worktree, tc.base, m, subject)
			if err == nil {
				t.Fatalf("TaskDivergence returned no error and %+v; a detector that cannot "+
					"look must not report clean", got)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantInErr)
			}
		})
	}
}

// §12.3.2 is SAME REPO ONLY. Two tasks in different repos cannot collide in one
// work tree, so pairing them would report a divergence that predicts no merge
// conflict — and a detector that cries wolf is one a human learns to click
// through.
func TestSiblingPaths(t *testing.T) {
	m := Manifest{Tasks: []Task{
		{ID: "schema", Repo: "api", Paths: []string{"db/**"}},
		{ID: "handlers", Repo: "api", Paths: []string{"http/**"}},
		{ID: "pathless", Repo: "api"},
		{ID: "web", Repo: "ui", Paths: []string{"src/**"}},
	}}
	got := SiblingPaths(m, m.Tasks[0])
	want := map[string][]string{"handlers": {"http/**"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SiblingPaths = %v, want %v — same repo only, self excluded, and a task "+
			"that declared no paths contributes nothing to match against", got, want)
	}
}

// The stored encoding follows EncodeFlags' rule: the EMPTY STRING for an empty
// value, not "{}". An untouched row and a cleared row must be byte-identical or
// every "has anything changed" comparison gets a false positive the first time
// a task diverges and is then corrected.
func TestDivergenceEncoding(t *testing.T) {
	if got := EncodeDivergence(gitdiff.Divergence{}); got != "" {
		t.Errorf("EncodeDivergence(empty) = %q, want the empty string", got)
	}
	full := gitdiff.Divergence{
		Outside:  []string{"README.md"},
		Siblings: map[string][]string{"handlers": {"http/router.go"}},
	}
	round := DecodeDivergence(EncodeDivergence(full))
	if !reflect.DeepEqual(round, full) {
		t.Errorf("round trip = %+v, want %+v", round, full)
	}
	// A corrupt column degrades to empty rather than to an error — the same
	// "degrade, never block" rule DecodeFlags follows. The `diverged` FLAG lives
	// in a separate column, so a corrupt list costs the file names and never the
	// warning itself.
	if got := DecodeDivergence("{not json"); !got.Empty() {
		t.Errorf("DecodeDivergence(garbage) = %+v, want empty", got)
	}
}
