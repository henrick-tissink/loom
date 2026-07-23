package delegate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/transcript"
)

// CheckStatus is the value of `delegation_tasks.check_status`.
//
// §13.1's column comment also lists "env-suspect", and that is a spec slip this
// package does not follow: §6.4 and §17 both bind env-suspect as a FLAG on a
// failure, never a status of its own ("EADDRINUSE in output ⇒ env-suspect flag
// and the result is still a failure"). Making it a status would let a caller
// switch on it and forget the failure, which is precisely the laundering §8.4
// forbids. Result.EnvSuspect carries it instead.
type CheckStatus string

const (
	// CheckPass is exit 0, and nothing else. No parsing of output, no "looks
	// like the tests passed", no heuristic.
	CheckPass CheckStatus = "pass"
	// CheckFail is any non-zero exit, including a timeout. A timeout is a
	// FAILURE, not an unknown: an unknown is a state a human has to resolve, and
	// a check that cannot finish inside its own declared budget has answered.
	CheckFail CheckStatus = "fail"
	// CheckUnpublished is §8.3's short-circuit: a declared artifact is missing
	// or uncommitted, so the command never ran.
	CheckUnpublished CheckStatus = "unpublished"
	// CheckInfraError is cmd[0] not found or a fork failure — NOT a non-zero
	// exit, and recorded distinctly. It is the one condition that gets a retry
	// (exactly one, §8.4); retrying a failing check until it passes is how a
	// system launders a flake into a false certification, and this system's
	// entire claim to trustworthiness is that the check result means something.
	CheckInfraError CheckStatus = "infra-error"
)

// Check timeouts (§8.1). The default is generous because a real test suite is
// slow; the hard maximum exists because a manifest is agent-authored and
// "timeout": "90m" would park a slot against §6.6's cap for an afternoon.
const (
	CheckTimeoutDefault = 10 * time.Minute
	CheckTimeoutMax     = 30 * time.Minute
)

// Output caps (§8.1): 256KB head + 256KB tail with a visible elision marker.
// Visible because a silently truncated failure tail is the one part of the
// output a human actually needs.
const (
	CheckOutputHead   = 256 << 10
	CheckOutputTail   = 256 << 10
	CheckOutputMarker = "\n… [output elided] …\n"
)

// Result is one check execution.
type Result struct {
	Status CheckStatus
	Exit   int
	Output string // stdout+stderr interleaved, capped head+tail
	// BranchHead is the sha the check actually ran against. Without it §8.2's
	// debounce either re-runs forever against an unchanged tree or never re-runs
	// after a commit that landed mid-check.
	BranchHead string
	RanAt      time.Time
	// Unpublished names the specific artifact paths that were missing or
	// uncommitted, when Status is CheckUnpublished. The paths, not a count: the
	// remedy is "commit this file".
	Unpublished []string
	// Published is true once §8.3's precondition has PASSED — every declared
	// artifact tracked and committed on the task's branch.
	//
	// It is its own field rather than something a caller infers from Status,
	// because the inference is wrong in the case that matters: a check that
	// fails after its artifacts were verified is a `fail` whose artifacts ARE
	// published, and §9.1's second condition ("the artifact exists at its
	// declared path, committed, on the producer's branch") is about the
	// artifacts, not about the check. A caller recording the publication from
	// `Status == CheckPass` would leave delegation_artifacts empty for every
	// red task and silently strand its consumers as unready forever.
	Published bool
	// EnvSuspect is §6.4's triage label — the output matched one of a small set
	// of environment-failure shapes. It NEVER turns a failure into a pass; it
	// changes how the failure is rendered so a human triaging ten red checks can
	// tell "your code is wrong" from "your neighbour took port 3000" in one
	// glance, and it excludes the task from 3a's M2 measurement. A heuristic,
	// explicitly, not a diagnosis.
	EnvSuspect bool
	// Fingerprints maps an interface artifact's id to its fingerprint output at
	// this publish (§8.3), trimmed and capped. Recorded now, consumed by §10.5's
	// stale-contract alarm, which is deferred.
	Fingerprints map[string]string
}

// Checker runs a task's check as a subprocess, out-of-band, in the child's
// worktree — never inside the child's session, never as a slash command, never
// as anything the child could influence the reported result of. §8 exists
// because ~22.6% of validated misalignment episodes involved inaccurate
// self-reporting: a child's "done" is an executable check on a published
// artifact, never a message.
type Checker struct {
	// Environ defaults to os.Environ(). Injected so a test can assert the
	// scrubbing rather than mutate the process environment.
	Environ []string
}

// CheckRequest is one execution's inputs, gathered by the caller.
type CheckRequest struct {
	RunID    int64
	TaskID   string
	Worktree string
	Check    Check
	// Artifacts are the task's declared artifacts, verified by §8.3 BEFORE the
	// command runs.
	Artifacts []Artifact
	// RepoDirs is repo label → directory, exported as LOOM_REPO_<LABEL>. In 3a
	// these are the repos' primary work trees; §10.2 will point them at
	// integration worktrees instead, which is a change of value and not of
	// contract.
	RepoDirs map[string]string
}

// Run executes the check and returns its Result. It never returns a Go error: a
// failure to run IS a result (CheckInfraError), and every caller's job is to
// record and render it rather than to abort a run over it.
//
// Execution contract (§8.1):
//
//   - exec.CommandContext with cmd[0] looked up on the hydrated PATH; ARGV
//     ARRAY, NO SHELL (§4.3);
//   - Dir = <worktree>/<check.cwd>, RE-VALIDATED inside the worktree at
//     execution time via ResolveInside even though load already checked it;
//   - env = the parent environment with CLAUDECODE and CLAUDE_CODE_* scrubbed
//     (the memory.Summarizer precedent — a check must not think it is inside a
//     Claude session), plus check.env, LOOM_TASK_ID, LOOM_RUN_ID, LOOM_WORKTREE
//     and one LOOM_REPO_<LABEL> per repo in scope;
//   - cmd.WaitDelay is set, for the reason the summarizer sets it: an orphaned
//     grandchild otherwise wedges Wait() forever;
//   - exit 0 = pass, anything else = fail. No output parsing.
func (c *Checker) Run(ctx context.Context, req CheckRequest) Result {
	res := Result{RanAt: time.Now()}

	// §8.2's debounce ("the task's branch head has moved since the last check")
	// is only implementable if the sha is captured HERE, by the code that
	// already has the worktree in hand. The alternative — letting each caller
	// re-derive it with its own `git rev-parse` — races: a commit landing
	// between this Run and the caller's read would record a head the check never
	// saw, and the next check would be skipped against a tree it never ran on.
	//
	// Captured BEFORE the publication short-circuit, so an `unpublished` or
	// `infra-error` result is debounced too. Otherwise a task whose artifacts
	// are not yet committed re-runs its check on every poll tick forever.
	//
	// A failure to read it (a worktree with no commits yet, or one whose
	// directory went away) leaves it EMPTY, which is the same "no head" ShouldRun
	// already refuses to auto-run on. Empty is therefore never written over a
	// previously recorded head by a caller: doing so would read back as "never
	// checked" and re-run the check on every poll tick.
	res.BranchHead = worktreeHead(req.Worktree)

	// An empty argv cannot be a definition of done. §4.4 makes it a load error,
	// so reaching here means the snapshot was written by another Loom or edited
	// by hand — which is exactly when a silent "pass" would be worst.
	if len(req.Check.Cmd) == 0 {
		res.Status, res.Exit = CheckInfraError, -1
		res.Output = "check has no command (cmd is empty)"
		return res
	}

	// §8.3 FIRST, before anything is executed. The precondition is the reason
	// the artifact list is in the manifest at all: it converts "did you finish?"
	// into two git commands, and a task whose artifacts are not committed has
	// nothing for a check to be a statement about.
	missing, err := Published(req.Worktree, req.Artifacts)
	if err != nil {
		res.Status, res.Exit = CheckInfraError, -1
		res.Output = "publication check failed: " + err.Error()
		return res
	}
	if len(missing) > 0 {
		res.Status, res.Exit = CheckUnpublished, -1
		res.Unpublished = missing
		res.Output = "unpublished artifacts (missing or uncommitted): " + strings.Join(missing, ", ")
		return res
	}
	res.Published = true

	// Fingerprints are taken at the moment publication is verified, so the
	// record is tied to the producing commit rather than to whenever §10.5
	// happens to look. A fingerprint that cannot be taken is recorded as its own
	// error string rather than dropped: §10.5's alarm compares strings, and a
	// silently absent one reads as "unchanged".
	for _, a := range req.Artifacts {
		if a.Kind != "interface" || len(a.Fingerprint) == 0 {
			continue
		}
		fp, ferr := Fingerprint(req.Worktree, a)
		if ferr != nil {
			fp = "ERROR: " + ferr.Error()
		}
		if res.Fingerprints == nil {
			res.Fingerprints = map[string]string{}
		}
		res.Fingerprints[a.ID] = fp
	}

	dir, err := checkDir(req.Worktree, req.Check.Cwd)
	if err != nil {
		// A refusal, not a failure: the command never ran, so calling it `fail`
		// would blame the child for a manifest defect. §4.4 rule 7 checked this
		// at load; this is the second check, against the tree that materialized.
		res.Status, res.Exit = CheckInfraError, -1
		res.Output = fmt.Sprintf("check cwd %q refused: %v", req.Check.Cwd, err)
		return res
	}

	// §8.4: exactly one retry, and only for an INFRASTRUCTURE error (cmd[0] not
	// found, fork failure) — never for a non-zero exit. Retrying a failing check
	// until it passes is how a system launders a flake into a false
	// certification, and this system's entire claim to trustworthiness is that
	// the check result means something.
	var out string
	var exit int
	var runErr error
	for attempt := 0; attempt < 2; attempt++ {
		out, exit, runErr = c.runOnce(ctx, dir, req)
		// A cancelled parent breaks the loop explicitly. exec would refuse the
		// second Start anyway (CommandContext checks ctx before forking), but
		// relying on that would make "the argv runs at most twice" a property
		// of os/exec rather than of this loop.
		if runErr == nil || errors.Is(runErr, errCheckCancelled) || !isInfraError(runErr) {
			break
		}
	}

	res.Output, res.Exit, res.RanAt = out, exit, time.Now()
	switch {
	case errors.Is(runErr, errCheckCancelled):
		// Recorded as infra-error and NOT as a failure: §13.2 has no state for
		// "no verdict", and infra-error is already the status every caller maps
		// back to the state it claimed from. Inventing a fifth status would
		// widen the schema for a condition that says exactly what infra-error
		// already says — the check made no statement about done. The output
		// says which, because "infra-error" alone would send a human looking
		// for a missing binary.
		res.Status, res.Exit = CheckInfraError, -1
		res.Output = strings.TrimSpace(out + "\n\n… check was cancelled before it finished (Loom shutting down); no verdict was recorded …")
	case runErr != nil && isInfraError(runErr):
		res.Status = CheckInfraError
		res.Output = strings.TrimSpace(out + "\n" + runErr.Error())
	case exit == 0:
		res.Status = CheckPass
	default:
		res.Status = CheckFail
		res.EnvSuspect = envSuspect(out)
	}
	return res
}

// runOnce is one execution attempt. It returns the captured output, the exit
// code, and an error ONLY for a failure to run — a non-zero exit is a result,
// not an error, and conflating the two is what would let an infra retry fire on
// a genuine failure.
func (c *Checker) runOnce(ctx context.Context, dir string, req CheckRequest) (string, int, error) {
	timeout := clampCheckTimeout(req.Check.ResolvedTimeout)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, req.Check.Cmd[0], req.Check.Cmd[1:]...)
	cmd.Dir = dir
	cmd.Env = c.env(req)
	// Stdin is closed, not inherited: a check that blocks on a prompt would hold
	// its slot against §6.6's cap until the timeout, and there is no human on
	// the other end of a subprocess Loom started out-of-band.
	cmd.Stdin = strings.NewReader("")
	// One writer for both streams, so the capture is INTERLEAVED in the order
	// the process produced it. Two buffers concatenated would reorder a failing
	// test's stderr away from the stdout line that explains it, which is the
	// only part of the output a human reads.
	buf := &capWriter{}
	cmd.Stdout, cmd.Stderr = buf, buf
	// WaitDelay for the reason internal/memory/summarize.go sets it: without it
	// a child that leaves an orphaned grandchild holding the stdout pipe open
	// wedges Wait() indefinitely even though cmd.Process is already dead. That
	// is what actually guarantees the process is reaped on timeout — the Kill
	// alone does not.
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	out := buf.String()

	// A cancelled PARENT is Loom shutting down, and it is NOT a verdict. §8.1
	// binds "exit 0 = pass, anything else = fail" as a statement about a
	// command that RAN to its own conclusion; a command Loom killed made no
	// statement at all. Recording it as `fail` moved the task to a state
	// TaskState.Terminal reports true for, and afterwards a red certification
	// nobody earned is indistinguishable from a real one — which is precisely
	// the laundering §8.4 forbids, running in the other direction.
	//
	// Checked BEFORE the deadline branch and keyed on the PARENT's Err, not
	// cctx's: cctx is derived, so a parent cancel shows up there as
	// context.Canceled while our own budget shows up as DeadlineExceeded. The
	// timeout deliberately stays a failure — the check declared that budget and
	// exceeded it. Cancellation has no such declaration behind it.
	if ctx.Err() != nil && !errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return out, -1, errCheckCancelled
	}

	// A timeout is a FAILURE, not an unknown (§8.1). It is labelled in the
	// output because "exit 1" with no explanation sends a human looking for a
	// test that never got to run.
	if cctx.Err() != nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		out = strings.TrimRight(out, "\n") + fmt.Sprintf("\n\n… check timed out after %s and was killed …\n", timeout)
		return out, exitCodeOf(err, -1), nil
	}

	// exec.ErrWaitDelay means the process REACHED AN EXIT STATUS and then
	// WaitDelay elapsed while something still held the output pipe — a check
	// that leaves a watcher, a dev server, anything with a trailing `&`. That
	// is §6.4's population, not an infrastructure error, and it is not an
	// *exec.ExitError either, so isInfraError used to answer yes: a check that
	// exited 0 was recorded `infra-error` AND §8.4's retry re-executed an
	// agent-authored argv the human approved exactly once at the §5.1 gate.
	// Found by a probe. ProcessState is populated in this case and is the
	// authority on the exit code; Go only substitutes ErrWaitDelay for a nil
	// error, so a non-zero exit still arrives as an *exec.ExitError above.
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil {
		return out, cmd.ProcessState.ExitCode(), nil
	}
	if err != nil {
		if isInfraError(err) {
			return out, -1, err
		}
		return out, exitCodeOf(err, -1), nil
	}
	return out, 0, nil
}

// errCheckCancelled is "Loom stopped this check", as distinct from every
// condition that is about the check itself. It is not exported: no caller
// should branch on it, because the only correct handling — record no verdict —
// is what Run already does with it.
var errCheckCancelled = errors.New("delegate: check cancelled before it could finish")

// isInfraError distinguishes "the command never ran" from "the command ran and
// said no". Only the first is retried (§8.4) and only the first is recorded as
// CheckInfraError; an *exec.ExitError means a real process reached a real exit
// code, which is a verdict.
func isInfraError(err error) bool {
	if err == nil {
		return false
	}
	// exec.ErrWaitDelay is defence in depth: runOnce already converts it to a
	// verdict using ProcessState, and this makes sure a future path that misses
	// that conversion cannot turn a green check into a retried infra-error.
	if errors.Is(err, exec.ErrWaitDelay) {
		return false
	}
	var exitErr *exec.ExitError
	return !errors.As(err, &exitErr)
}

func exitCodeOf(err error, fallback int) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if err == nil {
		return 0
	}
	return fallback
}

// clampCheckTimeout defends at EXECUTION time against a timeout the loader was
// supposed to have bounded. The snapshot may have been written by another Loom,
// and a 90-minute check parks a slot against §6.6's cap for an afternoon.
func clampCheckTimeout(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return CheckTimeoutDefault
	case d > CheckTimeoutMax:
		return CheckTimeoutMax
	default:
		return d
	}
}

// checkDir resolves the check's working directory inside the worktree.
//
// "" and "." are the worktree itself and are used directly: there is nothing to
// re-validate about a path that IS the base. Everything else goes through
// ResolveInside — the same function §4.4 rule 7 uses at load — because a
// load-time pass is a statement about the file as it was parsed, and the tree
// it will run in is a second fact.
func checkDir(worktree, rel string) (string, error) {
	if rel == "" || rel == "." {
		return worktree, nil
	}
	dir, err := ResolveInside(worktree, rel)
	if err != nil {
		return "", err
	}
	// And again through SYMLINKS, which the lexical pass cannot see. §8.1 says
	// the cwd is "re-validated to be inside the worktree AT EXECUTION TIME",
	// and a probe showed that sentence was not literally true: a child that
	// creates `escape -> /somewhere` gets a check whose cwd is outside its
	// worktree, with the manifest's cwd looking perfectly innocent.
	//
	// This is a boundary, not a sandbox, and the comment says so because the
	// difference matters: the check is arbitrary agent-authored code that can
	// `cd` wherever it likes once it starts. What this buys is that LOOM never
	// starts it outside the tree the human approved — the same reason
	// Worktrees.Create refuses an occupied directory rather than trusting the
	// child not to write there.
	//
	// A path that cannot be resolved (it does not exist yet, or a lookup fails)
	// keeps the lexical answer rather than becoming a refusal: exec will report
	// a missing directory far more usefully than "refused" would, and turning
	// "I could not tell" into a hard no here would break every check whose cwd
	// its own bootstrap creates.
	real, rerr := filepath.EvalSymlinks(dir)
	if rerr != nil {
		return dir, nil
	}
	base, berr := filepath.EvalSymlinks(worktree)
	if berr != nil {
		return dir, nil
	}
	if real != base && !strings.HasPrefix(real, base+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q resolves to %q, outside the worktree", ErrEscapesRepo, rel, real)
	}
	return dir, nil
}

// env builds the check's environment (§8.1).
//
// Order is load-bearing: the parent environment, scrubbed; then the manifest's
// check.env; then Loom's own LOOM_* variables LAST, because later entries win
// in exec and a manifest must not be able to tell its own check that it is a
// different task. Keys are sorted so two runs of the same check get a
// byte-identical environment — a check whose result depends on map iteration
// order is not a definition of done.
func (c *Checker) env(req CheckRequest) []string {
	base := c.Environ
	if base == nil {
		base = os.Environ()
	}
	env := scrubEnv(base)

	for _, k := range sortedKeys(req.Check.Env) {
		env = append(env, k+"="+req.Check.Env[k])
	}
	for _, label := range sortedKeys(req.RepoDirs) {
		env = append(env, "LOOM_REPO_"+envLabel(label)+"="+req.RepoDirs[label])
	}
	return append(env,
		"LOOM_TASK_ID="+req.TaskID,
		"LOOM_RUN_ID="+strconv.FormatInt(req.RunID, 10),
		"LOOM_WORKTREE="+req.Worktree,
	)
}

// envLabel makes a repo label safe as an environment variable name. Repo labels
// are directory names — they may contain '-' or '.' — and are NOT constrained
// the way task ids are (§4.4 rule 3 constrains task ids precisely because they
// become path and ref components; labels become neither). A label that produced
// `LOOM_REPO_V-ATLAS=` would be dropped by the shell that reads it, silently.
func envLabel(label string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(label) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// envSuspect is §6.4's triage heuristic, applied to a FAILING check's output
// only. It never turns a failure into a pass.
func envSuspect(out string) bool {
	lower := strings.ToLower(out)
	for _, shape := range envSuspectShapes {
		if strings.Contains(lower, strings.ToLower(shape)) {
			return true
		}
	}
	return false
}

// worktreeHead is the child's current commit — Result.BranchHead's source.
//
// It returns "" rather than an error on purpose: every condition that can make
// it fail (a branch with no commits, a worktree whose directory was removed
// under us) is one where the CHECK's own result is the thing worth reporting,
// and a `rev-parse` failure must not be able to turn a genuine pass or fail
// into an infra-error. "" is the caller-visible "unknown" — see Run.
func worktreeHead(worktree string) string {
	out, err := gitOut(worktree, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Published is §8.3's precondition, and it is the whole reason the artifact list
// is in the manifest: it converts "did you finish?" into two git commands per
// artifact.
//
//	git -C <worktree> ls-files --error-unmatch -- <path>   # tracked
//	git -C <worktree> diff --quiet HEAD -- <path>          # committed, not just staged
//
// It returns the paths that failed, so a missing or merely-STAGED artifact
// short-circuits the check as CheckUnpublished with the specific paths named and
// the command never executed. Uncommitted work does not exist.
func Published(worktree string, artifacts []Artifact) ([]string, error) {
	if len(artifacts) == 0 {
		return nil, nil
	}

	// HEAD is resolved ONCE, up front, because a branch with no commits makes
	// every per-artifact `git diff HEAD` fail with the same fatal — and reading
	// that fatal as an infrastructure error would report a broken Loom when the
	// truth is the plainest possible form of "nothing is committed yet".
	//
	// --verify --quiet is what separates the two: an unresolvable ref exits 1,
	// while "not a git repository" exits 128 (verified experimentally). Without
	// --quiet both are 128 and the two conditions are indistinguishable without
	// string-matching git's stderr, which is exactly the kind of brittleness
	// this codebase has already paid for once.
	if ok, err := gitOK(worktree, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return nil, err
	} else if !ok {
		paths := make([]string, 0, len(artifacts))
		for _, a := range artifacts {
			paths = append(paths, artifactPath(a))
		}
		return paths, nil
	}

	var missing []string
	for _, a := range artifacts {
		path := artifactPath(a)
		if a.Path == "" {
			// An artifact with no path cannot be verified, and unverifiable is
			// unpublished. §4.4 rejects this at load; a snapshot from another
			// Loom is the way it gets here, and guessing is not an option.
			missing = append(missing, path)
			continue
		}
		// --literal-pathspecs on BOTH commands. An artifact path is
		// agent-authored and reaches git as a PATHSPEC, and pathspecs carry
		// magic: `:(exclude)db/nothing.sql` and `:!db/nothing.sql` match
		// whatever else is tracked, so both commands succeed and the artifact
		// is "published" with nothing committed. A probe demonstrated it.
		// §4.4 rule 7 only tested that a path does not ESCAPE the repo after
		// filepath.Clean, which magic does not, so a bad manifest reached here.
		// Rule 7 now also refuses a leading ':' at load; this is the second
		// line, at the site that consumes the value, because a snapshot written
		// by another Loom never passes through this loader.
		tracked, err := gitOK(worktree, "--literal-pathspecs", "ls-files", "--error-unmatch", "--", a.Path)
		if err != nil {
			return nil, err
		}
		if !tracked {
			missing = append(missing, path)
			continue
		}
		// Tracked is not committed. A staged-but-uncommitted artifact is
		// UNPUBLISHED — that is the whole content of "uncommitted work does not
		// exist", and it is the case a `ls-files`-only check would wave through.
		clean, err := gitOK(worktree, "--literal-pathspecs", "diff", "--quiet", "HEAD", "--", a.Path)
		if err != nil {
			return nil, err
		}
		if !clean {
			missing = append(missing, path)
		}
	}
	return missing, nil
}

// artifactPath is what a refusal names. The path, because the remedy is "commit
// this file"; the id as a fallback so a pathless artifact is still nameable.
func artifactPath(a Artifact) string {
	if a.Path == "" {
		return a.ID + " (no path declared)"
	}
	return a.Path
}

// git runs a git plumbing command in dir and returns its stdout.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Scrubbed for the same reason the check itself is: git hooks and
	// credential helpers read the environment, and a query whose answer depends
	// on who launched it is not evidence.
	cmd.Env = scrubEnv(os.Environ())
	out, err := cmd.Output()
	return string(out), err
}

// gitOK runs a git predicate: ok=false for exit 1 (the answer is "no"), an
// error for anything else (git missing, not a repository, a corrupt object
// store). The distinction is the whole point — "your file is not committed" and
// "your repository is broken" have opposite remedies, and §8.3 must never
// report the second as the first.
func gitOK(dir string, args ...string) (bool, error) {
	if _, err := git(dir, args...); err != nil {
		if code, isExit := exitStatus(err); isExit && code == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return true, nil
}

func exitStatus(err error) (int, bool) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

// Fingerprint runs an interface artifact's fingerprint argv in the worktree and
// returns its stdout, trimmed and capped at 4KB. Run at the moment §8.3 verifies
// publication, so the record is tied to the producing commit.
func Fingerprint(worktree string, a Artifact) (string, error) {
	if len(a.Fingerprint) == 0 {
		return "", nil
	}
	// Bounded like every other subprocess this package starts. The spec gives no
	// timeout for a fingerprint because it is "a hash"; an unbounded one would
	// nonetheless wedge the run's clock behind a misbehaving script, and a
	// minute is generous for `sha256sum`.
	ctx, cancel := context.WithTimeout(context.Background(), fingerprintTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, a.Fingerprint[0], a.Fingerprint[1:]...)
	cmd.Dir = worktree
	cmd.Env = scrubEnv(os.Environ())
	cmd.Stdin = strings.NewReader("")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("fingerprint %s: %w", a.ID, err)
	}
	fp := strings.TrimSpace(string(out))
	if len(fp) > FingerprintCap {
		fp = fp[:FingerprintCap]
	}
	return fp, nil
}

const fingerprintTimeout = time.Minute

// FingerprintCap bounds a recorded fingerprint. A fingerprint is a hash or a
// short digest; anything longer is a script misbehaving, and storing megabytes
// of it per artifact per publish is not a thing this table should absorb.
const FingerprintCap = 4 << 10

// ShouldRun is §8.2's auto-run predicate: the check runs automatically when BOTH
// the task's branch head has moved since the last check AND the child's
// transcript state is Idle or NeedsYou.
//
// The second condition is waitForContinueGate's existing rule, reused verbatim
// rather than imported (this package does not depend on internal/workflow):
// `❯` is meaningless mid-generation, and running a test suite against a tree the
// child is halfway through writing produces noise, not signal. A manual "run
// check" action is always available and does not consult this.
func ShouldRun(branchHead, lastCheckedHead string, st transcript.State) bool {
	// No head at all means no commits yet: there is nothing published to check,
	// and §8.3 would short-circuit anyway. Refusing here saves the git calls.
	if branchHead == "" || branchHead == lastCheckedHead {
		return false
	}
	return st == transcript.StateIdle || st == transcript.StateNeedsYou
}

// CheckDebounce is §8.2's debounce. At most one check is in flight per task; the
// caller enforces that, because "in flight" is a property of the runner's own
// bookkeeping and not of any row.
const CheckDebounce = 10 * time.Second

// envSuspectShapes is §6.4's small, deliberately short list of environment
// failure signatures. Short on purpose: every entry is a claim that a string in
// arbitrary test output means something about the environment, and a long list
// is a long tail of wrong claims. Matched case-insensitively against the
// captured output of a FAILING check only.
var envSuspectShapes = []string{
	"EADDRINUSE",
	"address already in use",
	"connection refused",
	"database is locked",
}

// scrubEnv is memory.ScrubEnv, aliased rather than restated.
//
// The rule (§8.1): a check that believes it is running inside a Claude session
// changes its own behaviour via hooks and output modes, and a check whose
// result depends on who launched it is not a definition of done. The list of
// variables that carry that belief is claude's, not ours, so it will grow —
// and two copies of it would drift into a check that passes for the summarizer
// and fails for the runner. One copy.
var scrubEnv = memory.ScrubEnv

// capOutput applies the head+tail cap with a visible elision marker to output
// that is already in hand.
func capOutput(s string) string {
	if len(s) <= CheckOutputHead+CheckOutputTail {
		return s
	}
	return s[:CheckOutputHead] + CheckOutputMarker + s[len(s)-CheckOutputTail:]
}

// capWriter applies the same cap WHILE capturing, so a check that prints a
// gigabyte costs a fixed 512KB of memory rather than a gigabyte plus a
// truncation. That distinction is the whole reason this is a writer and not a
// bytes.Buffer passed through capOutput: the runaway-output case is the one
// where the naive form takes the machine down, and it is a case a test suite in
// a loop produces routinely.
//
// The tail is a fixed ring, not a re-sliced append: re-slicing keeps the whole
// underlying array alive, which is the same leak wearing a smaller len().
//
// Both streams share one writer (interleaving is the point), and exec copies
// each on its own goroutine, so the mutex is required — not defensive.
type capWriter struct {
	mu     sync.Mutex
	head   []byte
	ring   []byte
	pos    int
	filled bool
	total  int64
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.total += int64(n)

	if room := CheckOutputHead - len(w.head); room > 0 {
		take := min(room, len(p))
		w.head = append(w.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if w.ring == nil {
		w.ring = make([]byte, CheckOutputTail)
	}
	if len(p) >= CheckOutputTail {
		copy(w.ring, p[len(p)-CheckOutputTail:])
		w.pos, w.filled = 0, true
		return n, nil
	}
	for len(p) > 0 {
		k := copy(w.ring[w.pos:], p)
		p = p[k:]
		w.pos += k
		if w.pos == CheckOutputTail {
			w.pos, w.filled = 0, true
		}
	}
	return n, nil
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	tail := w.ring[:w.pos]
	if w.filled {
		tail = append(append([]byte{}, w.ring[w.pos:]...), w.ring[:w.pos]...)
	}
	if w.total <= CheckOutputHead+CheckOutputTail {
		return string(w.head) + string(tail)
	}
	return string(w.head) + CheckOutputMarker + string(tail)
}
