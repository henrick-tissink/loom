package delegate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/transcript"
)

// scratchRepo builds a throwaway git repo under t.TempDir with one commit.
//
// Every git-touching test in this file builds its own tree here. Nothing in
// this package may run a mutating git command against the Loom repository
// itself — a `git worktree add` or a stray commit in the developer's own
// checkout is not a test failure, it is damage.
func scratchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=loom", "GIT_AUTHOR_EMAIL=loom@example.invalid",
			"GIT_COMMITTER_NAME=loom", "GIT_COMMITTER_EMAIL=loom@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	write(t, filepath.Join(dir, "README.md"), "scratch\n")
	run("add", "README.md")
	run("commit", "-qm", "base")
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=loom", "GIT_AUTHOR_EMAIL=loom@example.invalid",
		"GIT_COMMITTER_NAME=loom", "GIT_COMMITTER_EMAIL=loom@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// §17: exit 0 = pass, exit 1 = fail, timeout = FAIL (not unknown), cmd[0]
// missing = infra-error, EADDRINUSE ⇒ env-suspect and still a failure.
//
// The timeout row is the one that matters most: an "unknown" is a state a human
// has to resolve, and a check that cannot finish inside its own declared budget
// has already answered.
func TestCheckExitCodesAndTimeout(t *testing.T) {
	repo := scratchRepo(t)

	tests := []struct {
		name        string
		cmd         []string
		timeout     time.Duration
		wantStatus  CheckStatus
		wantExit    int
		wantSuspect bool
		wantInOut   string
	}{
		{name: "exit 0 is a pass", cmd: []string{"sh", "-c", "exit 0"}, wantStatus: CheckPass},
		{name: "exit 1 is a failure", cmd: []string{"sh", "-c", "exit 1"}, wantStatus: CheckFail, wantExit: 1},
		{name: "any non-zero exit is a failure", cmd: []string{"sh", "-c", "exit 7"}, wantStatus: CheckFail, wantExit: 7},
		{
			name: "a timeout is a failure, not an unknown",
			cmd:  []string{"sleep", "30"}, timeout: 150 * time.Millisecond,
			wantStatus: CheckFail, wantExit: -1, wantInOut: "timed out",
		},
		{
			name:       "cmd[0] not found is an infrastructure error, not a failure",
			cmd:        []string{"loom-no-such-binary-exists-here"},
			wantStatus: CheckInfraError, wantExit: -1,
		},
		{
			name:       "a port collision is flagged env-suspect and stays a failure",
			cmd:        []string{"sh", "-c", "echo 'listen tcp :3000: bind: address already in use'; exit 1"},
			wantStatus: CheckFail, wantExit: 1, wantSuspect: true,
		},
		{
			name:       "stderr is captured, interleaved with stdout",
			cmd:        []string{"sh", "-c", "echo out; echo err 1>&2; exit 2"},
			wantStatus: CheckFail, wantExit: 2, wantInOut: "err",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Checker{}
			res := c.Run(context.Background(), CheckRequest{
				RunID: 1, TaskID: "t", Worktree: repo,
				Check: Check{Cmd: tc.cmd, ResolvedTimeout: tc.timeout},
			})
			if res.Status != tc.wantStatus {
				t.Fatalf("Status = %q, want %q (output: %q)", res.Status, tc.wantStatus, res.Output)
			}
			if res.Exit != tc.wantExit {
				t.Errorf("Exit = %d, want %d", res.Exit, tc.wantExit)
			}
			if res.EnvSuspect != tc.wantSuspect {
				t.Errorf("EnvSuspect = %v, want %v", res.EnvSuspect, tc.wantSuspect)
			}
			if tc.wantInOut != "" && !strings.Contains(res.Output, tc.wantInOut) {
				t.Errorf("output %q does not contain %q", res.Output, tc.wantInOut)
			}
		})
	}
}

// §8.1's output cap, exercised by a check that produces far more than it.
//
// The assertion is on the SHAPE — head kept, tail kept, a visible marker
// between them — because a silently truncated tail is the one part of a failing
// check's output a human actually needs, and a cap that dropped the tail would
// pass a naive "output is bounded" test.
func TestCheckCapsHugeOutput(t *testing.T) {
	repo := scratchRepo(t)
	// ~700KB, comfortably past the 512KB the cap keeps, with recognisable ends.
	script := "printf HEADSTART; yes AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA | head -c 700000; printf TAILEND"

	c := &Checker{}
	res := c.Run(context.Background(), CheckRequest{
		RunID: 1, TaskID: "t", Worktree: repo,
		Check: Check{Cmd: []string{"sh", "-c", script}},
	})

	if res.Status != CheckPass {
		t.Fatalf("Status = %q, want pass (output head: %.120q)", res.Status, res.Output)
	}
	want := CheckOutputHead + len(CheckOutputMarker) + CheckOutputTail
	if len(res.Output) != want {
		t.Errorf("len(Output) = %d, want exactly %d (head+marker+tail)", len(res.Output), want)
	}
	if !strings.HasPrefix(res.Output, "HEADSTART") {
		t.Error("the head of the output was not kept")
	}
	if !strings.HasSuffix(res.Output, "TAILEND") {
		t.Error("the tail of the output was not kept — a failing suite's verdict is at the END")
	}
	if !strings.Contains(res.Output, CheckOutputMarker) {
		t.Error("the elision is invisible; a human cannot tell output was dropped")
	}
}

// §17: an unpublished artifact SHORT-CIRCUITS before the command runs. The
// assertion that the command never executed is the point — a check that ran
// against a tree missing its own deliverable produces a verdict about nothing.
func TestCheckUnpublishedArtifactShortCircuits(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, repo string)
		want    string
	}{
		{
			name:    "the artifact does not exist at all",
			prepare: func(t *testing.T, repo string) {},
			want:    "api/auth.yaml",
		},
		{
			name: "the artifact exists but is untracked",
			prepare: func(t *testing.T, repo string) {
				write(t, filepath.Join(repo, "api/auth.yaml"), "openapi: 3\n")
			},
			want: "api/auth.yaml",
		},
		{
			name: "the artifact is STAGED but not committed",
			prepare: func(t *testing.T, repo string) {
				write(t, filepath.Join(repo, "api/auth.yaml"), "openapi: 3\n")
				gitIn(t, repo, "add", "api/auth.yaml")
			},
			want: "api/auth.yaml",
		},
		{
			name: "the artifact is committed but has since been modified",
			prepare: func(t *testing.T, repo string) {
				write(t, filepath.Join(repo, "api/auth.yaml"), "openapi: 3\n")
				gitIn(t, repo, "add", "api/auth.yaml")
				gitIn(t, repo, "commit", "-qm", "publish")
				write(t, filepath.Join(repo, "api/auth.yaml"), "openapi: 3 # local edit\n")
			},
			want: "api/auth.yaml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := scratchRepo(t)
			tc.prepare(t, repo)
			marker := filepath.Join(repo, "the-check-ran")

			c := &Checker{}
			res := c.Run(context.Background(), CheckRequest{
				RunID: 1, TaskID: "auth-api", Worktree: repo,
				Check:     Check{Cmd: []string{"touch", marker}},
				Artifacts: []Artifact{{ID: "auth-openapi", Path: "api/auth.yaml"}},
			})

			if res.Status != CheckUnpublished {
				t.Fatalf("Status = %q, want %q", res.Status, CheckUnpublished)
			}
			if len(res.Unpublished) != 1 || res.Unpublished[0] != tc.want {
				t.Errorf("Unpublished = %v, want [%s] — the refusal must name the path", res.Unpublished, tc.want)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the check command RAN despite an unpublished artifact")
			}
		})
	}
}

// The positive half: a committed artifact publishes, and the command runs.
func TestCheckRunsOnceArtifactsArePublished(t *testing.T) {
	repo := scratchRepo(t)
	write(t, filepath.Join(repo, "api/auth.yaml"), "openapi: 3\n")
	gitIn(t, repo, "add", "api/auth.yaml")
	gitIn(t, repo, "commit", "-qm", "publish")

	c := &Checker{}
	res := c.Run(context.Background(), CheckRequest{
		RunID: 1, TaskID: "auth-api", Worktree: repo,
		Check:     Check{Cmd: []string{"sh", "-c", "exit 0"}},
		Artifacts: []Artifact{{ID: "auth-openapi", Path: "api/auth.yaml"}},
	})
	if res.Status != CheckPass {
		t.Fatalf("Status = %q, want pass (unpublished: %v, output: %q)", res.Status, res.Unpublished, res.Output)
	}
}

// A repository with no commits at all: everything declared is unpublished, and
// that is a normal answer rather than an infrastructure error. Reading git's
// "fatal: bad revision HEAD" as a broken Loom would send a human debugging
// their install when the truth is "the child has not committed yet" — which is
// the state EVERY task is in for its first minutes.
func TestCheckUnbornBranchIsUnpublishedNotInfraError(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(repo, "a.txt"), "x\n")
	gitIn(t, repo, "add", "a.txt")

	missing, err := Published(repo, []Artifact{{ID: "a", Path: "a.txt"}})
	if err != nil {
		t.Fatalf("Published returned an error for an unborn branch: %v", err)
	}
	if len(missing) != 1 || missing[0] != "a.txt" {
		t.Fatalf("missing = %v, want [a.txt]", missing)
	}
}

// A directory that is not a git repository at all IS an infrastructure error,
// and must not be laundered into "unpublished". The two have opposite remedies:
// one is "commit your file", the other is "your worktree is gone".
func TestCheckPublishedRefusesANonRepository(t *testing.T) {
	if _, err := Published(t.TempDir(), []Artifact{{ID: "a", Path: "a.txt"}}); err == nil {
		t.Fatal("Published accepted a directory that is not a git repository")
	}
}

// §8.1's environment contract: CLAUDECODE and CLAUDE_CODE_* scrubbed (a check
// that believes it is inside a Claude session changes its own behaviour, and a
// check whose result depends on who launched it is not a definition of done),
// LOOM_* present, and a manifest's own env unable to overwrite the LOOM_* facts.
func TestCheckEnvironment(t *testing.T) {
	repo := scratchRepo(t)
	c := &Checker{Environ: []string{
		"CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CONFIG_DIR=/keep/me",
		"KEEP_ME=yes",
	}}

	res := c.Run(context.Background(), CheckRequest{
		RunID: 42, TaskID: "auth-api", Worktree: repo,
		// /usr/bin/env as cmd[0], not `sh -c env`: with a scrubbed, injected
		// environment there is no PATH for the shell to find `env` with.
		Check: Check{
			Cmd: []string{"/usr/bin/env"},
			Env: map[string]string{"FROM_MANIFEST": "1", "LOOM_TASK_ID": "spoofed"},
		},
		RepoDirs: map[string]string{"v-atlas": "/w/v-atlas"},
	})
	if res.Status != CheckPass {
		t.Fatalf("Status = %q, want pass (output %q)", res.Status, res.Output)
	}

	for _, banned := range []string{"CLAUDECODE=", "CLAUDE_CODE_ENTRYPOINT="} {
		if strings.Contains(res.Output, banned) {
			t.Errorf("%s survived the scrub", banned)
		}
	}
	for _, want := range []string{
		"CLAUDE_CONFIG_DIR=/keep/me", // scrubbing is narrow, not a purge
		"KEEP_ME=yes",
		"FROM_MANIFEST=1",
		"LOOM_RUN_ID=42",
		"LOOM_WORKTREE=" + repo,
		"LOOM_REPO_V_ATLAS=/w/v-atlas", // '-' is not legal in an env var name
	} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("environment is missing %q\ngot:\n%s", want, res.Output)
		}
	}
	// The manifest tried to say it was a different task. LOOM_* is written last
	// and therefore wins: a check must not be able to lie to itself about which
	// task it is certifying.
	if !strings.Contains(res.Output, "LOOM_TASK_ID=auth-api") {
		t.Errorf("LOOM_TASK_ID was overridden by the manifest's env\ngot:\n%s", res.Output)
	}
}

// §8.1: the check's cwd is re-validated INSIDE the worktree at execution time,
// even though §4.4 rule 7 already checked it at load — a load-time pass is a
// statement about the file as it was parsed, and the tree it runs in is a
// second fact. The refusal must happen before the command runs.
func TestCheckCwdEscapingTheWorktreeIsRefused(t *testing.T) {
	repo := scratchRepo(t)
	marker := filepath.Join(repo, "the-check-ran")

	for _, cwd := range []string{"..", "../..", "/etc"} {
		t.Run(cwd, func(t *testing.T) {
			c := &Checker{}
			res := c.Run(context.Background(), CheckRequest{
				RunID: 1, TaskID: "t", Worktree: repo,
				Check: Check{Cmd: []string{"touch", marker}, Cwd: cwd},
			})
			if res.Status != CheckInfraError {
				t.Fatalf("Status = %q, want %q — an escaping cwd is a refusal, not a verdict on the child",
					res.Status, CheckInfraError)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the check command RAN with a cwd outside the worktree")
			}
		})
	}
}

// §8.2's auto-run predicate: BOTH the branch head moved AND the transcript is
// idle or needs-you. Running a suite against a tree the child is halfway
// through writing produces noise, not signal.
func TestCheckShouldRun(t *testing.T) {
	tests := []struct {
		name       string
		head, last string
		state      transcript.State
		want       bool
	}{
		{name: "moved and idle", head: "aaa", last: "bbb", state: transcript.StateIdle, want: true},
		{name: "moved and needs-you", head: "aaa", last: "bbb", state: transcript.StateNeedsYou, want: true},
		{name: "moved but still generating", head: "aaa", last: "bbb", state: transcript.StateRunning},
		{name: "moved but state unknown", head: "aaa", last: "bbb", state: transcript.StateUnknown},
		{name: "idle but unmoved", head: "aaa", last: "aaa", state: transcript.StateIdle},
		{name: "no commits at all", head: "", last: "", state: transcript.StateIdle},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldRun(tc.head, tc.last, tc.state); got != tc.want {
				t.Fatalf("ShouldRun(%q, %q, %v) = %v, want %v", tc.head, tc.last, tc.state, got, tc.want)
			}
		})
	}
}

// The capping writer, unit-tested at its boundaries because the check runner
// feeds it from two goroutines and a boundary bug there would show up as a
// silently mangled failure tail rather than as a crash.
func TestCheckCapWriter(t *testing.T) {
	tests := []struct {
		name   string
		chunks []int // byte counts written, in order
	}{
		{name: "under the cap", chunks: []int{10, 20}},
		{name: "exactly at the cap", chunks: []int{CheckOutputHead, CheckOutputTail}},
		{name: "one byte over", chunks: []int{CheckOutputHead, CheckOutputTail, 1}},
		{name: "one huge write", chunks: []int{4 * (CheckOutputHead + CheckOutputTail)}},
		{name: "many small writes wrapping the ring", chunks: manyChunks(4096, 300)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var total int
			w := &capWriter{}
			for _, n := range tc.chunks {
				if _, err := w.Write(fill(byte('a'+total%26), n)); err != nil {
					t.Fatal(err)
				}
				total += n
			}
			got := w.String()
			if total <= CheckOutputHead+CheckOutputTail {
				if len(got) != total {
					t.Fatalf("len = %d, want %d (nothing should have been elided)", len(got), total)
				}
				return
			}
			want := CheckOutputHead + len(CheckOutputMarker) + CheckOutputTail
			if len(got) != want {
				t.Fatalf("len = %d, want %d", len(got), want)
			}
			if !strings.Contains(got, CheckOutputMarker) {
				t.Fatal("elision marker missing")
			}
		})
	}
}

func manyChunks(size, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = size
	}
	return out
}

func fill(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// capOutput and capWriter must agree: one applies the rule to bytes in hand,
// the other while they stream past, and a disagreement would render the same
// check two ways depending on which path produced the string.
func TestCheckCapOutputMatchesCapWriter(t *testing.T) {
	s := string(fill('z', 3*(CheckOutputHead+CheckOutputTail)))
	w := &capWriter{}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if capOutput(s) != w.String() {
		t.Fatal("capOutput and capWriter disagree about the same input")
	}
}

// §8.3's fingerprint, taken at the moment publication is verified so the record
// is tied to the producing commit. Capped, because a fingerprint is a digest
// and a script that prints a megabyte is a script misbehaving.
func TestCheckFingerprint(t *testing.T) {
	repo := scratchRepo(t)
	a := Artifact{ID: "auth-openapi", Kind: "interface", Path: "README.md",
		Fingerprint: []string{"sh", "-c", "printf '  deadbeef  '"}}

	fp, err := Fingerprint(repo, a)
	if err != nil {
		t.Fatal(err)
	}
	if fp != "deadbeef" {
		t.Fatalf("fingerprint = %q, want %q (trimmed)", fp, "deadbeef")
	}

	big := Artifact{ID: "big", Kind: "interface", Path: "README.md",
		Fingerprint: []string{"sh", "-c", "yes x | head -c 100000"}}
	fp, err = Fingerprint(repo, big)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != FingerprintCap {
		t.Fatalf("len(fingerprint) = %d, want the %d-byte cap", len(fp), FingerprintCap)
	}
}

// §8.2's debounce is only implementable if Run reports the sha it actually ran
// against. Before this, BranchHead was documented and never assigned, so every
// caller had to re-derive it with its own `git rev-parse` — which races a commit
// landing during the check and records a head the check never saw.
//
// The table covers every exit path of Run that reaches a real worktree, because
// the debounce has to work for a task that is failing or not yet published just
// as much as for one that passes: a check that re-runs on every tick because its
// result carried no head is the failure this field exists to prevent.
func TestCheckReportsTheBranchHeadItRanAgainst(t *testing.T) {
	tests := []struct {
		name       string
		cmd        []string
		artifacts  []Artifact
		wantStatus CheckStatus
	}{
		{name: "pass", cmd: []string{"sh", "-c", "exit 0"}, wantStatus: CheckPass},
		{name: "fail", cmd: []string{"sh", "-c", "exit 1"}, wantStatus: CheckFail},
		{
			name: "infra-error", cmd: []string{"loom-no-such-binary-ever"},
			wantStatus: CheckInfraError,
		},
		{
			name: "unpublished short-circuit", cmd: []string{"sh", "-c", "exit 0"},
			artifacts:  []Artifact{{ID: "never", Path: "never-committed.yaml"}},
			wantStatus: CheckUnpublished,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := scratchRepo(t)
			want := strings.TrimSpace(mustGitOut(t, repo, "rev-parse", "HEAD"))

			c := &Checker{}
			res := c.Run(context.Background(), CheckRequest{
				RunID: 1, TaskID: "t", Worktree: repo,
				Check: Check{Cmd: tc.cmd}, Artifacts: tc.artifacts,
			})
			if res.Status != tc.wantStatus {
				t.Fatalf("Status = %q, want %q (output %q)", res.Status, tc.wantStatus, res.Output)
			}
			if res.BranchHead != want {
				t.Errorf("BranchHead = %q, want %q — §8.2's debounce cannot work without it",
					res.BranchHead, want)
			}
		})
	}
}

// A commit that lands DURING the check must not be attributed to it. The head is
// captured up front, so the reported sha is the tree the command actually saw;
// recording the later one would let the debounce skip a check for work that was
// never checked.
func TestCheckBranchHeadIsThePreCommitHead(t *testing.T) {
	repo := scratchRepo(t)
	before := strings.TrimSpace(mustGitOut(t, repo, "rev-parse", "HEAD"))

	c := &Checker{}
	res := c.Run(context.Background(), CheckRequest{
		RunID: 1, TaskID: "t", Worktree: repo,
		// The check itself commits, standing in for a child that pushed while
		// the suite was running.
		Check: Check{
			Cmd: []string{"sh", "-c",
				"echo x > mid.txt && git add mid.txt && git commit -qm mid"},
			// The identity is pinned here rather than inherited, so the test does
			// not depend on the developer's global git config existing.
			Env: map[string]string{
				"GIT_AUTHOR_NAME": "loom", "GIT_AUTHOR_EMAIL": "loom@example.invalid",
				"GIT_COMMITTER_NAME": "loom", "GIT_COMMITTER_EMAIL": "loom@example.invalid",
				"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
			},
		},
	})
	if res.Status != CheckPass {
		t.Fatalf("Status = %q, want pass (output %q)", res.Status, res.Output)
	}
	after := strings.TrimSpace(mustGitOut(t, repo, "rev-parse", "HEAD"))
	if after == before {
		t.Fatal("the test's own precondition failed: no commit landed during the check")
	}
	if res.BranchHead != before {
		t.Errorf("BranchHead = %q, want the pre-check head %q, not the mid-check %q",
			res.BranchHead, before, after)
	}
}

// An unreadable head is "" — never an infra-error. A repo with no commits is the
// state EVERY task is in for its first minutes, and turning that into a broken
// Loom would send a human debugging their install.
func TestCheckBranchHeadEmptyOnUnbornBranch(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main", ".")

	c := &Checker{}
	res := c.Run(context.Background(), CheckRequest{
		RunID: 1, TaskID: "t", Worktree: repo,
		Check: Check{Cmd: []string{"sh", "-c", "exit 0"}},
	})
	if res.BranchHead != "" {
		t.Errorf("BranchHead = %q, want \"\" for an unborn branch", res.BranchHead)
	}
	if res.Status != CheckPass {
		t.Errorf("Status = %q, want pass — an unreadable head is not a check result", res.Status)
	}
	// And "" is exactly what ShouldRun already refuses to auto-run on, so the
	// two halves of §8.2 agree without a special case anywhere.
	if ShouldRun(res.BranchHead, "", transcript.StateIdle) {
		t.Error("ShouldRun fired on an empty head")
	}
}

func mustGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// --- failure-mode probes (slice 3a) ---------------------------------------

// §8.1 is BINDING: "Exit 0 = pass. Anything else = fail." and §8.4 allows
// exactly one retry, for an INFRASTRUCTURE error only, because "retrying a
// failing check until it passes is how a system launders a flake into a false
// certification".
//
// A check that exits 0 while leaving a background process holding the stdout
// pipe — `npm test` that leaves a watcher, a suite that starts a dev server,
// anything with a trailing `&` — trips cmd.WaitDelay. Wait then returns
// exec.ErrWaitDelay, which is not an *exec.ExitError, so isInfraError says yes
// and the runner both (a) records a check that exited 0 as `infra-error` and
// (b) RE-EXECUTES it. Re-executing an agent-authored argv nobody approved a
// second time is the more serious half: a check is arbitrary code, and §4.3's
// whole safety argument is that the human approved the one argv at the gate.
func TestCheckLingeringBackgroundProcessIsNotAnInfraErrorAndRunsOnce(t *testing.T) {
	repo := scratchRepo(t)
	tally := filepath.Join(t.TempDir(), "runs")
	script := filepath.Join(t.TempDir(), "check.sh")
	write(t, script, "#!/bin/sh\necho ran >> "+tally+"\nsleep 30 &\nexit 0\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	res := (&Checker{}).Run(context.Background(), CheckRequest{
		Worktree: repo, Check: Check{Cmd: []string{script}},
	})

	b, _ := os.ReadFile(tally)
	if n := strings.Count(string(b), "ran"); n != 1 {
		t.Errorf("the check argv executed %d times; §8.4 permits one execution and one "+
			"retry on an INFRASTRUCTURE error only, and this command exited 0", n)
	}
	if res.Status != CheckPass {
		t.Errorf("Status = %q (exit %d), want pass — the command exited 0 and a lingering "+
			"grandchild is not a failure to run", res.Status, res.Exit)
	}
}

// A cancelled PARENT context is Loom shutting down, not a verdict. §8.1's "exit
// 0 = pass, anything else = fail" is a statement about a command that RAN; a
// command Loom killed made no statement at all, and recording it as `fail`
// moves the task to a state Terminal() reports true for — a red certification
// nobody earned and no human can distinguish afterwards from a genuine one.
//
// The timeout case is deliberately NOT this: §8.1 binds a timeout to be a
// failure, and it is, because the check declared its own budget and exceeded
// it. Cancellation has no such declaration behind it.
func TestCheckParentCancellationIsNotAVerdict(t *testing.T) {
	repo := scratchRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	res := (&Checker{}).Run(ctx, CheckRequest{
		Worktree: repo, Check: Check{Cmd: []string{"sleep", "30"}},
	})

	if res.Status == CheckFail {
		t.Errorf("Status = fail after the parent context was cancelled; a check Loom "+
			"killed made no statement about done, and `fail` is terminal (exit=%d)", res.Exit)
	}
}

// §8.3's precondition is two git commands per artifact, and an artifact path is
// handed to both of them as a PATHSPEC. Git pathspecs carry magic — `:(exclude)`,
// `:(glob)`, `:!` — and a magic pathspec matches whatever it likes, so a
// manifest whose artifact path begins with `:` satisfies "published" while
// publishing nothing.
//
// The manifest is agent-authored and therefore untrusted input (§4.3), and §4.4
// rule 7 only checks that the path does not ESCAPE the repo after
// filepath.Clean, which `:(exclude)db` does not. The remedy is
// --literal-pathspecs on both commands, or a load-time refusal of a leading
// ':'; the point of the test is that "did you finish?" must not have a spelling
// that always answers yes.
func TestCheckArtifactPathspecMagicIsNotPublication(t *testing.T) {
	repo := scratchRepo(t)
	write(t, filepath.Join(repo, "db", "x.sql"), "create table t;\n")
	gitIn(t, repo, "add", "db/x.sql")
	gitIn(t, repo, "commit", "-qm", "publish something else")

	for _, path := range []string{":(exclude)db/nothing.sql", ":!db/nothing.sql"} {
		t.Run(path, func(t *testing.T) {
			missing, err := Published(repo, []Artifact{{ID: "a", Path: path}})
			if err != nil {
				t.Fatalf("Published: %v", err)
			}
			if len(missing) == 0 {
				t.Errorf("artifact %q reported PUBLISHED; nothing at that path is committed "+
					"— the path was interpreted as pathspec magic", path)
			}
		})
	}
}

// The half-written-artifact shape §8.3 exists for, in its least obvious
// spelling: the child committed the artifact and then deleted it from the
// worktree. `ls-files --error-unmatch` still says tracked; only the second
// command (`diff --quiet HEAD`) catches it. The check must not run at all —
// asserted on the filesystem, because a mock would only prove the mock was not
// called.
func TestCheckArtifactDeletedAfterCommitIsUnpublished(t *testing.T) {
	repo := scratchRepo(t)
	write(t, filepath.Join(repo, "art.sql"), "v1\n")
	gitIn(t, repo, "add", "art.sql")
	gitIn(t, repo, "commit", "-qm", "publish")
	if err := os.Remove(filepath.Join(repo, "art.sql")); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "the-check-ran")
	res := (&Checker{}).Run(context.Background(), CheckRequest{
		Worktree:  repo,
		Check:     Check{Cmd: []string{"touch", marker}},
		Artifacts: []Artifact{{ID: "a", Path: "art.sql"}},
	})

	if res.Status != CheckUnpublished {
		t.Errorf("Status = %q, want unpublished", res.Status)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the check command ran despite an unpublished artifact — §8.3 short-circuits BEFORE execution")
	}
	if len(res.Unpublished) != 1 || res.Unpublished[0] != "art.sql" {
		t.Errorf("Unpublished = %v, want the specific path", res.Unpublished)
	}
}

// §8.1: the check's cwd is "re-validated to be inside the worktree AT EXECUTION
// TIME". The load-time pass is lexical by documented choice — at load the
// worktree does not exist yet — so a child that creates a symlink out of its
// tree after it was briefed would have had Loom start its check outside the
// directory the human approved.
//
// A boundary, not a sandbox: the check is arbitrary agent-authored code and can
// `cd` anywhere once it runs. What this holds is that LOOM never starts it
// outside the tree — the same property Worktrees.Create's occupancy refusal
// holds, and for the same reason.
func TestCheckCwdIsRevalidatedThroughSymlinksAtExecutionTime(t *testing.T) {
	repo := scratchRepo(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	write(t, filepath.Join(outside, "deeper", "keep.txt"), "x\n")
	write(t, filepath.Join(repo, "inside", "keep.txt"), "x\n")

	cases := []struct {
		name      string
		cwd       string
		wantRefus bool
	}{
		{name: "a symlink out of the worktree is refused", cwd: "escape", wantRefus: true},
		{name: "a path beneath that symlink is refused too",
			cwd: "escape/deeper", wantRefus: true},
		{name: "an ordinary subdirectory still runs", cwd: "inside"},
		{name: "a cwd that does not exist yet is left to exec, not refused here",
			// A bootstrap that creates its own build directory is legitimate,
			// and turning "I cannot resolve it" into a refusal would break it.
			cwd: "not-created-yet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			res := (&Checker{}).Run(context.Background(), CheckRequest{
				Worktree: repo,
				Check:    Check{Cmd: []string{"touch", marker}, Cwd: tc.cwd},
			})
			if !tc.wantRefus {
				return
			}
			if res.Status != CheckInfraError {
				t.Errorf("Status = %q, want infra-error — a refused cwd means the command "+
					"never ran, and calling it `fail` would blame the child", res.Status)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("the check ran from a directory outside the worktree")
			}
			if !strings.Contains(res.Output, "outside the worktree") {
				t.Errorf("the refusal must say why: %q", res.Output)
			}
		})
	}
}
