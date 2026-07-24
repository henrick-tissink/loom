package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/gitdiff"
	"github.com/henricktissink/loom/internal/store"
)

// §10 — test-gated integration. Slice 1 §11 calls this "the load-bearing
// component in every system that measured a win", and §10.5 says just as plainly
// that cross-repo it has the weakest foundation in the design. Both are true and
// both must survive contact with the implementation.
//
// The one sentence to keep in mind while reading this file: the integration
// worktree is a TEST BED and is NEVER the thing that ships. It answers exactly
// one question — "is this task green combined with its already-verified
// siblings?" — and §10.4 merges the TASK'S OWN BRANCH into the user's branch.
// Revision 1 merged the integration branch, which is cumulative, so the first
// task through the gate would have dragged in every sibling that verified before
// it: the human approves diff(B) and Loom lands diff(A)+diff(B), with A's own
// §5.2 gate never shown and A's divergence acknowledgement never given. That is
// the exact shape §5.2 exists to forbid, on the one mechanism the evidence says
// works.

// IntegrationSpec is the manifest's `integration` block (§4.2).
//
// It is decoded from the run's manifest SNAPSHOT rather than living on Manifest,
// and that is a seam, not an oversight: manifest.go is 3a's proven, frozen
// loader, and this half of the slice needs the block that loader deliberately
// ignored. Folding it into Manifest with its own validation is the right end
// state and is a handoff note in run.go — until then IntegrationOf reads the
// same JSON delegation_runs.manifest_json already stores, so nothing in the run
// path depends on the on-disk file (which may have been edited or deleted since
// the run started — the workflow_runs.def_json precedent).
type IntegrationSpec struct {
	// PerRepo is the check run in a repo's integration worktree after a merge
	// (§10.2 step 3). A repo with no entry has no per-repo gate: the task's own
	// check is then the only evidence, which is a REAL degradation and is
	// rendered as one, not defaulted to something that passes.
	PerRepo map[string]Check `json:"per_repo"`
	// Cross is §10.5's honest part. It only exists if the initiative writes it.
	Cross []CrossCheck `json:"cross"`
}

// CrossCheck is one `integration.cross` entry: a real executable check spanning
// repos, and the ONLY mechanism in this design that can surface a cross-repo
// interface break. No VCS operation can — `git merge` in bankenstein cannot know
// that ballista calls a function whose signature just changed, there is no
// analogue of a merge conflict across repository boundaries, and this design
// does not invent one.
//
// If an initiative has no runnable cross-repo test, slice 3 CANNOT provide
// test-gated cross-repo integration. That degradation is disclosed (§16), it is
// rendered, and the stale-contract alarm below is a narrow partial substitute
// that is never to be presented as integration testing.
type CrossCheck struct {
	ID    string   `json:"id"`
	Cmd   []string `json:"cmd"`
	Cwd   string   `json:"cwd"`
	Repo  string   `json:"repo"` // whose integration worktree is the cwd
	Needs []string `json:"needs_repos"`
	// Timeout parses with §4.3's rules; ResolvedTimeout is filled by
	// IntegrationOf so nothing downstream re-parses a duration string.
	Timeout         string            `json:"timeout"`
	Env             map[string]string `json:"env"`
	ResolvedTimeout time.Duration     `json:"-"`
}

// integrationEnvelope is the shape IntegrationOf pulls out of the snapshot: the
// `integration` block plus the `defaults` its check timeouts fall back to.
//
// A private mirror of two Manifest fields rather than a re-decode into Manifest:
// Manifest carries resolution state the loader fills in (RepoPaths, ProjectRoot)
// that a raw json.Unmarshal cannot populate, and a half-populated Manifest
// floating around this file is exactly the value some later hand would pass to
// something that needs the real one.
type integrationEnvelope struct {
	Defaults    Defaults `json:"defaults"`
	Integration *struct {
		PerRepo map[string]Check `json:"per_repo"`
		Cross   []CrossCheck     `json:"cross"`
	} `json:"integration"`
}

// IntegrationOf decodes the `integration` block out of a manifest snapshot.
//
// Degrades: an absent block yields a zero spec and no error, because a manifest
// without integration checks is legal (and is what every 3a manifest is). A
// MALFORMED block is an error — a check the author wrote and Loom silently
// skipped is the worst outcome available here, since the gate would then be
// green on evidence nobody produced.
//
// Since Manifest carries `Integration` and the snapshot is `json.Marshal(m)`,
// the block this decodes is normally one loadOne already validated. It is
// re-validated anyway, through the SAME validator: the snapshot is a column
// that older Looms wrote and that a human can edit in sqlite, so trusting it
// because a loader once looked at it would be trusting a different process's
// memory. Cost is a map walk and a duration parse per run load.
func IntegrationOf(manifestJSON string) (IntegrationSpec, error) {
	var spec IntegrationSpec
	if strings.TrimSpace(manifestJSON) == "" {
		return spec, nil
	}
	var env integrationEnvelope
	if err := json.Unmarshal([]byte(manifestJSON), &env); err != nil {
		return spec, fmt.Errorf("delegate: manifest snapshot will not parse: %w", err)
	}
	if env.Integration == nil {
		return spec, nil
	}

	// The default the manifest declares for TASK checks is the default for
	// integration checks too. Two independent defaults would mean an initiative
	// that raised its timeout once still had a 10-minute integration gate, and
	// discovering that costs a red run and a human's afternoon.
	fallback, err := parseTimeout(env.Defaults.CheckTimeout, CheckTimeoutDefault)
	if err != nil {
		return spec, fmt.Errorf("delegate: integration: defaults.check_timeout: %w", err)
	}
	return ValidateIntegration(IntegrationSpec{
		PerRepo: env.Integration.PerRepo,
		Cross:   env.Integration.Cross,
	}, fallback)
}

// ValidateIntegration is the ONE validator for the `integration` block, shared
// by IntegrationOf (snapshot) and manifest.loadOne (file).
//
// One function rather than two because the two inputs are the same bytes at
// different moments, and a loader that accepted what the run path later refused
// would produce a manifest that validates in the GUI's "validate" affordance and
// then fails at the first integration — the worst possible place to learn it.
//
// It returns a NEW spec rather than mutating: ResolvedTimeout is filled in here
// and nothing downstream may re-parse a duration string an agent wrote.
func ValidateIntegration(in IntegrationSpec, fallback time.Duration) (IntegrationSpec, error) {
	var spec IntegrationSpec
	if len(in.PerRepo) > 0 {
		spec.PerRepo = make(map[string]Check, len(in.PerRepo))
	}
	for _, label := range sortedKeys(in.PerRepo) {
		c := in.PerRepo[label]
		if len(c.Cmd) == 0 {
			// An empty argv cannot be a gate. Refused rather than dropped: a
			// dropped per-repo check is a repo that silently has no gate at all,
			// and §5.2's precondition would then be satisfied by nothing.
			return IntegrationSpec{}, fmt.Errorf("delegate: integration.per_repo[%q] has no command", label)
		}
		d, err := parseTimeout(c.Timeout, fallback)
		if err != nil {
			return IntegrationSpec{}, fmt.Errorf("delegate: integration.per_repo[%q]: %w", label, err)
		}
		c.ResolvedTimeout = d
		spec.PerRepo[label] = c
	}

	seen := make(map[string]bool, len(in.Cross))
	for n, c := range in.Cross {
		switch {
		case strings.TrimSpace(c.ID) == "":
			return IntegrationSpec{}, fmt.Errorf("delegate: integration.cross[%d] has no id", n)
		case len(c.Cmd) == 0:
			return IntegrationSpec{}, fmt.Errorf("delegate: integration.cross[%q] has no command", c.ID)
		case strings.TrimSpace(c.Repo) == "":
			// Without a repo there is no cwd, and guessing one would run an
			// agent-authored argv in a directory nobody named.
			return IntegrationSpec{}, fmt.Errorf("delegate: integration.cross[%q] names no repo", c.ID)
		case seen[c.ID]:
			// The id is the check's IDENTITY on the wire: IntegrationResult
			// reports which cross check failed by id, and §10.5's rendering
			// keys on it. Two entries sharing one id make a red result name a
			// check the human cannot locate, and make a green one indexable to
			// the wrong argv. Refused rather than de-duplicated: which of the
			// two survives is not a decision this function can make.
			return IntegrationSpec{}, fmt.Errorf("delegate: integration.cross[%q] is declared twice — a cross-check id is how a result names itself", c.ID)
		}
		seen[c.ID] = true
		d, err := parseTimeout(c.Timeout, fallback)
		if err != nil {
			return IntegrationSpec{}, fmt.Errorf("delegate: integration.cross[%q]: %w", c.ID, err)
		}
		c.ResolvedTimeout = d
		spec.Cross = append(spec.Cross, c)
	}
	return spec, nil
}

// IntegrationDir is §10.1's per-repo integration worktree, one per repo per run,
// created at RUN creation and branched from the same pinned base as the
// children:
//
//	~/.loom/worktrees/<run-slug>/<repo-label>/__integration
//
// The method lives here rather than in worktree.go so that §10's surface can be
// filled in without editing 3a's frozen file; Go does not care which file a
// method is declared in, and the layout rules are all in worktree.go's Layout
// comment.
//
// The `__integration` leaf cannot collide with a task's directory: task ids are
// `[a-z0-9-]{1,64}` (§4.4 rule 3), so no task id can begin with an underscore.
// That is not a coincidence — the id charset was chosen to make path and ref
// components safe, and this is one of the things it buys.
func (l Layout) IntegrationDir(runSlug, repoLabel string) string {
	return filepath.Join(l.Root, runSlug, repoLabel, integrationLeaf)
}

const integrationLeaf = "__integration"

// IntegrationBranch is loom/<run-slug>/integration/<repo-label>. Distinct from
// BranchName's namespace by the `integration/` segment, so a task can never
// collide with one and a human reading `git branch` can tell them apart.
func IntegrationBranch(runSlug, repoLabel string) string {
	return "loom/" + runSlug + "/integration/" + repoLabel
}

// IntegrationStage names WHERE in §10.2's sequence a pass stopped. It is
// rendered, because "the integration is red" is ambiguous and the remedies are
// completely different: a conflict is a human decision, a bootstrap failure is
// usually §6.4's environment, a per-repo failure is the task, and a cross
// failure may be either task in the seam.
type IntegrationStage string

const (
	StageMerge     IntegrationStage = "merge"
	StageBootstrap IntegrationStage = "bootstrap"
	StagePerRepo   IntegrationStage = "per-repo"
	StageCross     IntegrationStage = "cross"
	StageDone      IntegrationStage = "done"
)

// Blame is §10.2's two attributions, distinguished EXPLICITLY because the
// remedies are opposite and the failure of not distinguishing them is silent.
type Blame string

const (
	// BlameTask — red with the task merged, GREEN at `pre` without it. The task
	// is parked via §10.3.
	BlameTask Blame = "task"
	// BlameBaseline — red with the task merged AND red at `pre`. A RUN-LEVEL
	// fault: the run row goes red, NO task is blamed, spawning stops,
	// needs-you-grade escalation. Cheap to evaluate, since the previous pass's
	// result at `pre` is already recorded in delegation_runs.integration.
	//
	// Without this row, a baseline that is red for environmental reasons (§6.4)
	// silently blames every task in sequence — and each of those tasks is a
	// child that gets parked and re-seeded for a fault that has nothing to do
	// with it.
	BlameBaseline Blame = "baseline"
)

// Baseline is one repo's entry in `delegation_runs.integration`: the integration
// worktree's head and the last result observed there. It is what §10.2's
// attribution table compares against, which is why it is persisted rather than
// recomputed — recomputing means re-running the check at `pre`, i.e. paying for
// the baseline twice on every single integration.
type Baseline struct {
	Head   string      `json:"head"`
	Status CheckStatus `json:"status"`
	At     int64       `json:"at"`
	Out    string      `json:"out"` // capped, capOutput's rules
}

// Known reports whether this baseline is a VERDICT about `head` and not merely a
// recorded position.
//
// The empty status is a third value and it is load-bearing: Ensure records a
// worktree's head with no verdict, and a repo with no declared per-repo check
// never earns one. Treating "" as green would blame every task on a run whose
// baseline was never evaluated; treating it as red would blame the baseline for
// every first integration. Neither — unknown means the check is actually re-run
// at `pre` before anyone is blamed (see attribute).
func (b Baseline) Known(head string) bool { return b.Head == head && b.Status != "" }

// Red reports a recorded FAILING verdict. `unpublished` and `infra-error` count:
// none of them is evidence the tree is good, and a gate that reads "not pass" as
// "fine" certifies on the absence of a result.
func (b Baseline) Red() bool { return b.Status != "" && b.Status != CheckPass }

// EncodeBaselines / DecodeBaselines are the `integration` column's codec. Empty
// map encodes as the EMPTY STRING to match the column default (EncodeFlags'
// rule), and Decode degrades to an empty map rather than erroring — a corrupt
// column costs the attribution optimization, never the run.
func EncodeBaselines(m map[string]Baseline) string {
	if len(m) == 0 {
		return ""
	}
	// json.Marshal sorts map keys, so the encoding is a function of the SET and
	// not of insertion order. Two Looms writing the same map must produce the
	// same bytes, or SetDelegationRunIntegrationCAS ping-pongs forever.
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func DecodeBaselines(s string) map[string]Baseline {
	out := map[string]Baseline{}
	if strings.TrimSpace(s) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		// Degrade, never block: a corrupt column costs the next attribution one
		// extra check execution at `pre`. Erroring here would cost the run its
		// ability to integrate at all.
		return map[string]Baseline{}
	}
	return out
}

// IntegrationResult is one pass of §10.2, and it is what the run view renders
// and what §5.2's gate reads.
type IntegrationResult struct {
	RunID     int64
	TaskID    string
	RepoLabel string

	// Pre is step 0's recorded head. It is in the result even on success,
	// because it is the only way to say what the task was integrated ON TOP OF.
	Pre string
	// Head is the integration head after a green pass. Empty on failure — a
	// failed pass is RESET to Pre and leaves nothing behind.
	Head string
	// Certified is the TASK BRANCH's commit as it stood when this pass staged
	// it. It is not Head — Head is a commit on the integration branch, which is
	// not the thing §10.4 merges — and it is the value §5.2's gate compares the
	// branch against so that the diff shown is the diff applied.
	Certified string

	Stage  IntegrationStage
	Status CheckStatus
	Blame  Blame
	// CrossCheck names the failing `integration.cross` entry when Stage is
	// StageCross.
	CrossCheck string
	// Conflicts is the conflicting file list when Stage is StageMerge. It is the
	// payload of §10.3's seed: the child gets told exactly which files, not
	// "there was a conflict".
	Conflicts []string
	Output    string
	RanAt     time.Time

	// Warnings is everything that happened which a human must see and which did
	// not change the verdict: a repo with no per-repo gate (a REAL degradation —
	// the task's own check is then the only evidence), a cross check skipped
	// because a repo it needs is not green, a worktree that could not be removed
	// after a merge.
	//
	// A field and not a log line, for this codebase's standing reason: a failure
	// nobody renders is a failure that gets debugged by reading source. It is
	// deliberately NOT folded into Output, which is a subprocess's bytes and is
	// rendered as such.
	Warnings []string

	// baseline is the verdict at `pre` that attribute had to pay for, carried so
	// record can persist it in the same place every other baseline goes.
	// Unexported: the rendered form is the run's `integration` column, and a
	// renderer reading this field would be reading the attribution's scratch
	// space.
	baseline *Baseline
}

// Green reports whether the whole sequence passed and the task may become
// `mergeable`.
func (r IntegrationResult) Green() bool { return r.Stage == StageDone && r.Status == CheckPass }

// Integrator runs §10.2. One per Loom process.
type Integrator struct {
	Store   *store.Store
	Layout  Layout
	Checker *Checker
	// Repos maps repo label → the PRIMARY work tree, for `git worktree add` and
	// for §10.4's merge target. The integration worktree is a linked worktree of
	// this repo, exactly like a child's.
	//
	// It falls back to Manifest.RepoPaths, which is the same map the loader
	// resolved. Injectable so a test — and a future multi-checkout setup — can
	// point the integrator somewhere other than where the loader looked.
	Repos map[string]string
	// Environ is the scrubbed base environment for check subprocesses; nil means
	// Checker's own.
	Environ []string
	// Worktrees is §10.4 step 3's remover (worktree removed, branch KEPT, .meta
	// KEPT). Optional: a nil one degrades to leaving the worktree on disk with a
	// WARNING on the result, because a cleanup failure must never look like a
	// merge failure — the merge has already landed in the user's branch, and
	// telling them otherwise sends them to re-run it.
	Worktrees *Worktrees
	// Blocks writes §10.3's Loom-authored park declaration at the same path a
	// child would use. An interface and not *Detector, so the ORDER — durable
	// row first, file second — is assertable without a filesystem detector, and
	// so a nil one degrades to the durable half alone rather than to no park.
	Blocks BlockWriter
	Now    func() time.Time

	// mu is §10.2's "serialized per run — one integration at a time, RUN-WIDE".
	// Run-wide and not per-repo: a cross check reads several repos' integration
	// worktrees at once and must not see one mid-merge, and per-repo locking
	// makes that race invisible in tests and reliable in production.
	//
	// Keyed by run id, because two runs against two different projects share
	// nothing and serializing them would make a slow initiative block a fast
	// one. TryLock semantics — a busy run returns ErrIntegrationBusy and the
	// caller re-offers next tick; a blocking acquire inside a poll tick is how
	// one stuck merge takes the whole poll loop with it.
	//
	// This is the IN-PROCESS half only. The enforced half is
	// store.ClaimTaskIntegrationCAS, whose exclusion predicate lives inside the
	// UPDATE: two Loom instances against one DB is supported and a mutex is
	// per-process, so a claim enforced out here would be advisory.
	mu      sync.Mutex
	running map[int64]bool
}

// BlockWriter is the one Detector operation §10.3 needs, named as an interface
// at the consumer: this file must not acquire a dependency on the detector's
// polling state in order to write one file.
type BlockWriter interface {
	Write(runSlug, repoLabel, taskID string, b Block) error
}

func (i *Integrator) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *Integrator) checker() *Checker {
	if i.Checker != nil {
		return i.Checker
	}
	return &Checker{Environ: i.Environ}
}

func (i *Integrator) repoPath(m Manifest, label string) string {
	if p := i.Repos[label]; p != "" {
		return p
	}
	return m.RepoPaths[label]
}

// Busy reports whether THIS process is running a pass for this run. It is the
// `work-stale` watchdog's exemption (Observation.Busy) and nothing else: a pass
// that is genuinely in flight here must not have its row released out from under
// it, and a pass in another process is — correctly — invisible to this, which is
// why the watchdog's recovery is written to be safe rather than exclusive.
func (i *Integrator) Busy(runID int64) bool {
	if i == nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.running[runID]
}

// acquire is the TryLock: a release func, or false when this run already has a
// pass in flight in THIS process.
func (i *Integrator) acquire(runID int64) (func(), bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.running[runID] {
		return nil, false
	}
	if i.running == nil {
		i.running = map[int64]bool{}
	}
	i.running[runID] = true
	return func() {
		i.mu.Lock()
		delete(i.running, runID)
		i.mu.Unlock()
	}, true
}

// Integrate runs §10.2's sequence for one verified task. The step numbering
// below is the spec's, verbatim, and every failure path ends at step R.
//
//  0. pre = git -C <integration-wt> rev-parse HEAD.
//  1. git merge --no-ff <task-branch>. Conflict → abort → R → §10.3.
//  2. Re-run repos[].bootstrap in the integration worktree IF the merge touched
//     a dependency manifest (package.json, go.mod, …). A coarse test,
//     DELIBERATELY over-eager: a needless `npm ci` costs seconds, a skipped one
//     costs a red check that looks like the task's fault. Failure → R.
//  3. Run integration.per_repo[repo] there. Red → R.
//  4. Run every `cross` check whose needs_repos are all currently at a GREEN
//     per-repo integration. Red → R, with the cross check named.
//  5. Green throughout → the task becomes `mergeable` and §5.2's gate appears.
//     The integration branch KEEPS the merge.
//     R. reset --hard <pre> + clean -fd, and attribute per the Blame table.
//
// BINDING — THE INTEGRATION BRANCH ONLY EVER CONTAINS WORK THAT WAS GREEN END TO
// END. Revision 1 aborted only on CONFLICT, so a clean merge whose check was red
// left the failing commits in the branch permanently. Every later task then
// integrated on top of known-broken code; its check was red for reasons that had
// nothing to do with it; step 4 attributed that to "the task that triggered this
// pass", systematically the wrong one; and §5.2's precondition became
// UNREACHABLE for the rest of the run until a human hand-repaired a branch Loom
// owns. Reset-on-red is not a nicety — without it the mechanism the evidence
// calls load-bearing degrades to noise after the first red.
//
// A task reset out of the branch is re-attempted from step 0 against the
// then-current head whenever the child pushes new commits, exactly as a first
// attempt. That is not a retry-until-green (§16 rejects those); it is a new
// attempt against new evidence.
func (i *Integrator) Integrate(ctx context.Context, run store.DelegationRun, m Manifest, t Task) (IntegrationResult, error) {
	res := IntegrationResult{RunID: run.ID, TaskID: t.ID, RepoLabel: t.Repo, RanAt: i.now()}
	if i.Store == nil {
		return res, errors.New("delegate: Integrator needs a Store")
	}

	release, ok := i.acquire(run.ID)
	if !ok {
		return res, ErrIntegrationBusy
	}
	defer release()

	// The ENFORCED claim. §10.2's serialization is run-wide and the predicate
	// lives inside the UPDATE (store.ClaimTaskIntegrationCAS), because a caller
	// that counts `integrating` tasks and then claims has two moments and both
	// racers count zero.
	//
	// The expected set is `verified` alone. Passing `integrating` would be
	// pointless rather than clever — the exclusion predicate counts the task's
	// own row and refuses it.
	claimed, _, err := i.Store.ClaimTaskIntegrationCAS(run.ID, t.ID, []string{string(StateVerified)}, i.now().Unix())
	if err != nil {
		return res, err
	}
	if !claimed {
		// A read AFTER the refusal, purely to say WHICH refusal it was: the two
		// have opposite meanings for the caller (re-offer next tick versus drop
		// the step entirely) and one rejected UPDATE cannot report them apart. It
		// performs no side effect, so this is not a second decision moment.
		if row, found, rerr := i.Store.GetDelegationTask(run.ID, t.ID); rerr == nil && found &&
			TaskState(row.State) != StateVerified {
			return res, fmt.Errorf("%w: %s is %s", ErrTaskMovedElsewhere, t.ID, row.State)
		}
		return res, ErrIntegrationBusy
	}

	res, err = i.sequence(ctx, run, m, t)
	if err != nil {
		// The sequence could not be EVALUATED at all (git is broken, the
		// worktree is gone). The task is released back to `verified` so the next
		// tick re-attempts it, rather than being left wedged in `integrating` —
		// which TaskState.Terminal deliberately does not report as terminal and
		// which would therefore read as in-flight work forever.
		_, _ = i.Store.AdvanceTaskCAS(run.ID, t.ID, string(StateIntegrating), string(StateVerified), i.now().Unix())
		return res, err
	}
	return res, i.record(run, m, t, res)
}

// sequence is §10.2 steps 0–4 plus R. It performs NO state transitions — record
// does — and the split is what makes every failure path's tree state assertable
// without a store in the picture.
func (i *Integrator) sequence(ctx context.Context, run store.DelegationRun, m Manifest, t Task) (IntegrationResult, error) {
	res := IntegrationResult{RunID: run.ID, TaskID: t.ID, RepoLabel: t.Repo, RanAt: i.now()}
	dir := i.Layout.IntegrationDir(run.Slug, t.Repo)
	branch := BranchName(run.Slug, t.ID)

	spec, err := IntegrationOf(run.ManifestJSON)
	if err != nil {
		return res, err
	}

	// 0. `pre` FIRST, before any side effect. Every failure path below ends in
	// `git reset --hard <pre>`, and a `pre` captured after the merge would reset
	// to the merge — i.e. would leave exactly the red commits reset-on-red
	// exists to remove.
	pre, err := gitOut(dir, "rev-parse", "HEAD")
	if err != nil {
		return res, fmt.Errorf("delegate: integration worktree %s: %w", dir, err)
	}
	res.Pre = strings.TrimSpace(pre)

	// The sha this pass is about to certify, read BEFORE the merge so it names
	// the commit that was actually staged. Read from the integration worktree,
	// which shares the repository's refs with the primary tree, so no second
	// path resolution is needed. A branch that will not resolve is not an error
	// here — the merge below is about to say so far better than this could.
	if head, herr := gitOut(dir, "rev-parse", "--verify", branch+"^{commit}"); herr == nil {
		res.Certified = strings.TrimSpace(head)
	}

	// 1. The merge. --no-ff so the integration branch records WHICH task was
	// staged and when; a fast-forward would make the staging history
	// indistinguishable from the task's own.
	msg := fmt.Sprintf("loom: stage %s (%s) for integration", t.ID, run.Slug)
	if err := gitRun(dir, "merge", "--no-ff", "--no-edit", "-m", msg, branch); err != nil {
		files := conflictedFiles(dir)
		_ = gitRun(dir, "merge", "--abort")
		i.reset(dir, res.Pre)
		res.Stage, res.Status, res.Blame = StageMerge, CheckFail, BlameTask
		res.Conflicts = files
		if len(files) == 0 {
			// No conflicted paths means the merge failed for a reason that is
			// not a disagreement — a missing branch, a broken index. Reported as
			// git said it, because sending a human to resolve a conflict that
			// does not exist is worse than quoting the tool.
			res.Output = fmt.Sprintf("merging %s into the integration worktree failed: %v", branch, err)
			return res, nil
		}
		// A conflict is by construction ABOUT THE TASK: the baseline cannot
		// conflict with itself, so the table's second row is unreachable here
		// and the attribution needs no evaluation at `pre`.
		res.Output = "conflicting files:\n  " + strings.Join(files, "\n  ")
		return res, nil
	}

	// 2. Bootstrap, if the merge touched a dependency manifest. The file list is
	// pre..HEAD rather than the merge commit's own diff, so a merge that brings
	// in several commits is judged on everything it brought.
	touched := changedFiles(dir, res.Pre, "HEAD")
	if boot := m.Repos[t.Repo].Bootstrap; len(boot) > 0 && touchedDependencyManifest(touched) {
		// A bare Worktrees purely for its bootstrap runner: the argv, the
		// timeout, the CLAUDECODE scrubbing and the WaitDelay are already
		// decided there, and a second spelling of "run the repo's bootstrap"
		// would drift from the one the spawn path uses on exactly the day it
		// mattered.
		w := &Worktrees{Environ: i.Environ}
		if err := w.bootstrap(dir, boot); err != nil {
			i.reset(dir, res.Pre)
			res.Stage, res.Status, res.Blame = StageBootstrap, CheckFail, BlameTask
			// §10.2 step 2 routes a bootstrap failure to `integration_blocked`,
			// which IS the task's park — so the blame is the task's even though
			// a bootstrap failure is most often §6.4's environment. Not
			// second-guessed here: the alternative is re-running bootstrap at
			// `pre` on every failure, and bootstrap is the one subprocess in
			// this sequence whose whole cost model is "expensive, run rarely".
			// The env-suspect rendering already exists to say "this smells like
			// the machine".
			res.Output = err.Error()
			var be *BootstrapError
			if errors.As(err, &be) {
				res.Output = be.Error() + "\n" + be.Output
			}
			return res, nil
		}
	}

	// 3. The per-repo gate.
	check, declared := spec.PerRepo[t.Repo]
	if !declared {
		// A REAL degradation, rendered as one. Defaulting to a pass would make
		// §5.2's "the per-repo integration check is green on the merged result"
		// satisfiable by a manifest that declares no gate at all.
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"repo %q declares no integration.per_repo check: the task's own check is the only evidence for this merge", t.Repo))
	} else if out := i.runPerRepo(ctx, run, m, check, t.Repo, t.ID); out.Status != CheckPass {
		res.Stage, res.Status, res.Output = StagePerRepo, out.Status, out.Output
		res.Blame = i.attribute(run, dir, res.Pre, t.Repo, func() Result {
			return i.runPerRepo(ctx, run, m, check, t.Repo, t.ID)
		}, &res)
		return res, nil
	}

	// 4. Cross checks. Only those whose needs_repos are all at a green per-repo
	// integration: a cross check run against a repo whose own gate is red
	// produces a failure nobody can attribute, which is the exact ambiguity the
	// Blame table exists to remove.
	for _, c := range spec.Cross {
		if !i.crossReady(run, spec, c) {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"cross check %q skipped: not every repo it needs (%s) is at a green per-repo integration",
				c.ID, strings.Join(c.Needs, ", ")))
			continue
		}
		out := i.runCross(ctx, run, c, t.ID)
		if out.Status == CheckPass {
			continue
		}
		res.Stage, res.Status, res.Output, res.CrossCheck = StageCross, out.Status, out.Output, c.ID
		res.Blame = i.attribute(run, dir, res.Pre, t.Repo, func() Result {
			return i.runCross(ctx, run, c, t.ID)
		}, &res)
		return res, nil
	}

	// 5. Green throughout. The integration branch KEEPS the merge — this is the
	// one path that does not reset.
	head, err := gitOut(dir, "rev-parse", "HEAD")
	if err != nil {
		// Reset, then report. Returning here without one leaves a merge commit
		// in the integration branch that no result was ever recorded against —
		// the state Ensure's own comment calls out as the thing never to create,
		// because the NEXT pass's `pre` is then a commit with no verdict and the
		// attribution table has nothing to compare. The staged work was green
		// through step 4, so nothing red is being discarded: the task is released
		// back to `verified` by Integrate and re-attempted from step 0.
		i.reset(dir, res.Pre)
		return res, err
	}
	res.Head = strings.TrimSpace(head)
	res.Stage, res.Status = StageDone, CheckPass
	return res, nil
}

// attribute is §10.2's two-row table, and the one place the difference between
// "your code is wrong" and "the room is on fire" is decided.
//
//	red with the task merged, GREEN at `pre`  → the TASK     (§10.3 parks the child)
//	red with the task merged AND red at `pre` → the BASELINE (a run-level fault)
//
// It performs step R first — `reset --hard <pre>` plus `clean -fd` — because any
// evaluation at `pre` has to happen in a tree that IS `pre`, and because every
// failure path resets whether or not the second row is reached.
//
// The recorded baseline is used when it is a verdict about this exact `pre` (the
// previous pass's result, which is the whole reason it is persisted). When it is
// not — the first integration of a run, a restart, a corrupt column — the check
// is RE-RUN at `pre`. That re-run is not in §10.2's text, which assumes the
// recorded result is always there, and it is necessary: the first red of a run
// has no recorded baseline, and both available defaults fail. Defaulting to "the
// task" blames a child for an environmental fault, which is precisely what this
// row exists to prevent; defaulting to "the baseline" stops the run's spawning
// on the first ordinary red test. The cost is one extra check execution, paid
// only on a red and only when the baseline is unknown, and the verdict is then
// recorded so the next pass reads it for free.
func (i *Integrator) attribute(run store.DelegationRun, dir, pre, repo string, rerun func() Result, res *IntegrationResult) Blame {
	i.reset(dir, pre)

	if b := DecodeBaselines(run.Integration)[repo]; b.Known(pre) {
		if b.Red() {
			return BlameBaseline
		}
		return BlameTask
	}

	at := rerun()
	res.Warnings = append(res.Warnings, fmt.Sprintf(
		"no recorded baseline at %s: the check was re-run WITHOUT %s to decide who to blame", short(pre), res.TaskID))
	res.baseline = &Baseline{Head: pre, Status: at.Status, At: i.now().Unix(), Out: capOutput(at.Output)}
	if at.Status != CheckPass {
		return BlameBaseline
	}
	return BlameTask
}

// runPerRepo executes one repo's integration check IN ITS INTEGRATION WORKTREE.
//
// No artifacts are passed: §8.3's publication precondition is a statement about
// a TASK's declared artifacts on the task's own branch, and applying it here
// would make an integration check refuse to run because a file the integration
// worktree never declared is not committed.
//
// RepoDirs points at INTEGRATION worktrees, not primary ones — check.go's
// CheckRequest comment calls this out as "a change of value and not of
// contract". A check that shells out to a sibling repo must see the sibling's
// STAGED code, or the two halves of one run are certified against different
// trees.
func (i *Integrator) runPerRepo(ctx context.Context, run store.DelegationRun, m Manifest, c Check, repo, taskID string) Result {
	return i.checker().Run(ctx, CheckRequest{
		RunID:    run.ID,
		TaskID:   taskID,
		Worktree: i.Layout.IntegrationDir(run.Slug, repo),
		Check:    c,
		RepoDirs: i.allIntegrationDirs(run, m),
	})
}

// runCross executes one cross check in §10.2's execution environment: cwd is the
// integration worktree of c.Repo, and every repo in needs_repos is exported as
// LOOM_REPO_<LABEL>=<its integration worktree path>.
//
// That environment is the whole reason a cross check is worth writing: a
// consumer's contract test builds and runs against the producer's STAGED code
// rather than its released code, which is the only thing that makes a cross-repo
// test meaningful mid-run. Loom supplies the environment; the initiative
// supplies the test.
func (i *Integrator) runCross(ctx context.Context, run store.DelegationRun, c CrossCheck, taskID string) Result {
	return i.checker().Run(ctx, CheckRequest{
		RunID:    run.ID,
		TaskID:   taskID,
		Worktree: i.Layout.IntegrationDir(run.Slug, c.Repo),
		Check:    Check{Cmd: c.Cmd, Cwd: c.Cwd, Env: c.Env, ResolvedTimeout: c.ResolvedTimeout},
		RepoDirs: i.crossEnv(run, c),
	})
}

// allIntegrationDirs is every in-scope repo's integration worktree, for a
// per-repo check's LOOM_REPO_* set.
func (i *Integrator) allIntegrationDirs(run store.DelegationRun, m Manifest) map[string]string {
	out := make(map[string]string, len(m.Repos))
	for label := range m.Repos {
		out[label] = i.Layout.IntegrationDir(run.Slug, label)
	}
	return out
}

// Ensure creates or verifies one repo's integration worktree (§10.1), branched
// from the run's pinned base. Idempotent, like Worktrees.Create and for the same
// reason: recovery re-derives everything from (run, repo) and must be able to
// re-run this.
//
// It is called at RUN CREATION, not lazily at the first integration, so that a
// repo whose worktree cannot be created fails while the human is still looking
// at the run rather than an hour later behind a green check.
func (i *Integrator) Ensure(run store.DelegationRun, m Manifest, repoLabel string) (Created, error) {
	repo := i.repoPath(m, repoLabel)
	if repo == "" {
		return Created{}, fmt.Errorf("delegate: repo %q has no path in this run", repoLabel)
	}
	var bases map[string]string
	if err := json.Unmarshal([]byte(run.BaseSHAs), &bases); err != nil {
		return Created{}, fmt.Errorf("delegate: run %s has an unreadable base_shas: %w", run.Slug, err)
	}
	base := strings.TrimSpace(bases[repoLabel])
	if base == "" {
		return Created{}, fmt.Errorf("delegate: run %s pinned no base for repo %q", run.Slug, repoLabel)
	}
	full, err := gitOut(repo, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return Created{}, fmt.Errorf("delegate: pinned base %s missing from %s: %w", base, repo, err)
	}
	base = strings.TrimSpace(full)

	dir := i.Layout.IntegrationDir(run.Slug, repoLabel)
	branch := IntegrationBranch(run.Slug, repoLabel)
	// Dir is PHYSICALLY RESOLVED for Created.Dir's stated reason: this path is
	// handed to a cross-repo consumer as an --add-dir, and every path this
	// package stores or compares must be byte-identical to what the launcher
	// wrote.
	c := Created{Dir: physicalPath(dir), Branch: branch, Base: base}

	// Prune first, unconditionally, for Create's reason: an administrative entry
	// whose directory a human deleted makes `worktree add` refuse both the path
	// and the branch, and the remedy is a command nobody should have to learn.
	_ = gitRun(repo, "worktree", "prune")

	switch info, statErr := os.Stat(dir); {
	case statErr == nil && info.IsDir():
		if err := verifyWorktree(dir, repo, branch); err != nil {
			return Created{}, err
		}
		c.Reused = true
		return c, nil
	case statErr == nil:
		return Created{}, fmt.Errorf("delegate: %s exists and is not a directory", dir)
	case !os.IsNotExist(statErr):
		return Created{}, statErr
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return Created{}, err
	}
	if branchExists(repo, branch) {
		// A restart, or a worktree a human removed. The branch carries every
		// green staging merge so far and is NEVER recreated from base — doing so
		// would silently discard the staging area §10.2 spends the whole run
		// building, and the next pass's `pre` would be a commit no result was
		// ever recorded against.
		c.Reused = true
		err = gitRun(repo, "worktree", "add", dir, branch)
	} else {
		err = gitRun(repo, "worktree", "add", "-b", branch, dir, base)
	}
	if err != nil {
		return Created{}, fmt.Errorf("delegate: integration worktree add %s: %w", dir, err)
	}
	return c, nil
}

// reset is step R: `git reset --hard <pre>` plus `git clean -fd` for merge
// debris. Both, always — a reset alone leaves untracked files a conflicted merge
// wrote, and those files are precisely the ones that make the NEXT pass fail for
// no reason.
//
// It returns nothing, on purpose. Its only caller is already reporting a
// failure, and the one thing worse than a reset that did not work is losing the
// failure that caused it to a second error. A reset that genuinely could not run
// leaves the worktree visibly wrong and the next pass's `pre` says so.
func (i *Integrator) reset(dir, pre string) {
	if pre == "" {
		return
	}
	_ = gitRun(dir, "reset", "--hard", pre)
	_ = gitRun(dir, "clean", "-fd")
}

// inProgressGitOp names the operation a repository is in the middle of, or "".
//
// It exists because `git status --porcelain` does not answer this question. A
// merge whose result matches HEAD leaves an empty status and a live MERGE_HEAD,
// and gitdiff.WorkingTree — the clean-tree predicate §10.3 names — reports
// clean. The marker files are git's own record of the operation, they are what
// git itself consults before refusing, and they are the only evidence available
// without parsing a localized human-readable status.
//
// The git dir is ASKED FOR rather than assumed to be `<dir>/.git`: in a linked
// worktree `.git` is a FILE pointing at `…/.git/worktrees/<name>`, and every
// marker for that worktree's in-progress operation lives in the pointed-at
// directory. Hard-coding the path would make this function answer "nothing in
// progress" for every worktree in the system, which is the one place Loom
// itself starts merges.
//
// A failure to resolve the git dir answers "" — this is a guard on top of a
// guard, and a git that cannot answer is already going to fail the merge it is
// guarding, loudly and without destroying anything.
func inProgressGitOp(dir string) string {
	out, err := gitOut(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return ""
	}
	gitDir := strings.TrimSpace(out)
	for _, m := range []struct{ path, name string }{
		{"MERGE_HEAD", "merge"},
		{"CHERRY_PICK_HEAD", "cherry-pick"},
		{"REVERT_HEAD", "revert"},
		{"rebase-merge", "rebase"},
		{"rebase-apply", "rebase or am"},
		{"BISECT_LOG", "bisect"},
	} {
		if _, err := os.Stat(filepath.Join(gitDir, m.path)); err == nil {
			return m.name
		}
	}
	return ""
}

// changedFiles is the file list between two commits, for step 2's coarse test. A
// failure degrades to nil, which makes step 2 skip bootstrap — the same answer
// "nothing dependency-shaped changed" gives, and the check that follows is what
// notices if that was wrong.
func changedFiles(dir, from, to string) []string {
	out, err := gitOut(dir, "diff", "--name-only", from, to)
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.TrimSpace(line); f != "" {
			files = append(files, f)
		}
	}
	return files
}

// touchedDependencyManifest is step 2's coarse test. Over-eager by design; the
// list is small, literal, and matched on basename anywhere in the merge's file
// list.
func touchedDependencyManifest(files []string) bool {
	for _, f := range files {
		base := filepath.Base(f)
		for _, dep := range dependencyManifests {
			if base == dep {
				return true
			}
		}
	}
	return false
}

// dependencyManifests is that list. Not configurable: an initiative that needs a
// different trigger is better served by a bootstrap that is cheap to re-run,
// which is the property step 2 actually depends on.
var dependencyManifests = []string{
	"package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock",
	"go.mod", "go.sum", "Cargo.toml", "Cargo.lock",
	"requirements.txt", "pyproject.toml", "poetry.lock",
	"Gemfile", "Gemfile.lock", "composer.json", "composer.lock",
}

// crossReady reports whether every repo in c.Needs is currently at a green
// per-repo integration (step 4's precondition). A cross check run against a repo
// whose own gate is red produces a failure nobody can attribute, which is the
// exact ambiguity the Blame table exists to remove.
//
// A repo that declares NO per-repo check is not treated as red: it has no gate
// to be red at (that degradation is warned about at step 3), and refusing on it
// would mean an initiative that writes a cross check but no per-repo check gets
// a cross check that silently never runs — the worst outcome available here,
// since §10.5 says the cross check is the ONLY real cross-repo mechanism there
// is.
func (i *Integrator) crossReady(run store.DelegationRun, spec IntegrationSpec, c CrossCheck) bool {
	baselines := DecodeBaselines(run.Integration)
	for _, repo := range c.Needs {
		if _, declared := spec.PerRepo[repo]; !declared {
			continue
		}
		if baselines[repo].Red() {
			return false
		}
	}
	return true
}

// crossEnv is §10.2's cross-check execution environment, and it is the whole
// reason a cross check is worth writing: cwd is the integration worktree of
// c.Repo, and every repo in needs_repos is exported as
// LOOM_REPO_<LABEL>=<its integration worktree path>. A consumer's contract test
// therefore builds and runs against the producer's STAGED code rather than its
// released code — the only thing that makes a cross-repo test meaningful
// mid-run. Loom supplies the environment; the initiative supplies the test.
//
// It returns the label→dir MAP rather than assembled env lines, and that is a
// deliberate departure from the scaffold's signature: Checker.env already owns
// the label mangling (check.go's envLabel, which exists because a repo label may
// contain '-'), the sorting, and the rule that LOOM_* wins last. Returning
// strings here would be a second spelling of the cross-check environment, and
// the day the two disagreed the check that ran would not be the check anyone
// read.
//
// c.Repo is always in the map even when needs_repos omits it: a test whose cwd
// is a repo it cannot name by variable is a test that has to hard-code a path.
func (i *Integrator) crossEnv(run store.DelegationRun, c CrossCheck) map[string]string {
	out := make(map[string]string, len(c.Needs)+1)
	out[c.Repo] = i.Layout.IntegrationDir(run.Slug, c.Repo)
	for _, repo := range c.Needs {
		out[repo] = i.Layout.IntegrationDir(run.Slug, repo)
	}
	return out
}

// record persists one pass: the run's baseline blob, the task's state, and — on
// a task-blamed failure — §10.3's park.
//
// Every transition is a CAS and none is retried in a loop: a rejected CAS means
// the row moved, which means the premise the step was computed from is gone, so
// the step is abandoned and recomputed next tick.
//
//	green            integrating → mergeable
//	blamed the task  integrating → blocked, with a Loom-authored block + seed
//	blamed the base  integrating → verified, and the RUN goes red
func (i *Integrator) record(run store.DelegationRun, m Manifest, t Task, res IntegrationResult) error {
	now := i.now().Unix()

	// The baseline first, because it is the evidence the NEXT pass attributes
	// against, and losing it costs a re-run rather than correctness.
	switch {
	case res.baseline != nil:
		if err := i.setBaselineFor(run.ID, t.Repo, *res.baseline); err != nil {
			return err
		}
	case res.Green():
		if err := i.setBaselineFor(run.ID, t.Repo, Baseline{
			Head: res.Head, Status: CheckPass, At: now, Out: capOutput(res.Output),
		}); err != nil {
			return err
		}
	}

	if res.Green() {
		// The certified sha BEFORE the state moves. §5.2's gate refuses a task
		// whose branch has moved past it, so a crash between the two must leave
		// the gate closed rather than open on an uncertified branch — writing it
		// afterwards inverts that.
		if res.Certified != "" {
			if err := i.Store.SetTaskCertifiedSHA(run.ID, t.ID, res.Certified, now); err != nil {
				return err
			}
		}
		claimed, err := i.Store.AdvanceTaskCAS(run.ID, t.ID, string(StateIntegrating), string(StateMergeable), now)
		if err != nil {
			return err
		}
		if !claimed {
			return fmt.Errorf("%w: %s left `integrating` during its own pass", ErrTaskMovedElsewhere, t.ID)
		}
		i.rearm(run.ID)
		return nil
	}

	if res.Blame == BlameBaseline {
		// A RUN-LEVEL fault. NO task is blamed: this task goes back to
		// `verified` and is re-attempted once the baseline is repaired, and the
		// RUN goes red so spawning stops.
		//
		// The red status is `deadlocked` — the one red, permanent, still-active
		// run status the schema has (store.ActiveDelegationRuns keeps it in the
		// active set precisely because it is waiting on a human). Inventing a
		// `baseline-red` status string was the alternative and is rejected:
		// every existing consumer of delegation_runs.status would treat an
		// unknown value as not-red, which is the invisible failure this codebase
		// forbids. The REASON is not lost — it is in the `integration` column,
		// which is exactly where §10.2 says the baseline result lives.
		if _, err := i.Store.AdvanceTaskCAS(run.ID, t.ID, string(StateIntegrating), string(StateVerified), now); err != nil {
			return err
		}
		// NO SECOND BASELINE WRITE HERE. It used to record res.Output — the
		// output of the run WITH THE TASK STAGED — as the baseline's evidence,
		// which is the evidence for the opposite verdict: the run row says no
		// task is to blame and then showed the failure captured from the tree
		// containing that task. `delegation_runs.integration` is the only place
		// redRunReason and the run view can learn why the run is red, so that
		// entry is the whole explanation a human gets.
		//
		// What belongs there is already written by the switch at the top of this
		// function: attribute paid for a real check run AT `pre` and stashed its
		// output in res.baseline. When res.baseline is nil the recorded baseline
		// was already a known-red verdict about this exact `pre`, so the entry in
		// the column is correct as it stands and overwriting it would replace a
		// measurement with a differently-measured one.
		//
		// Both source statuses are tried because a run may still be `planning`
		// when its first integration lands. Not a retry of a lost CAS — two
		// disjoint premises, one attempt each.
		for _, from := range []string{"running", "planning"} {
			claimed, err := i.Store.AdvanceDelegationRunCAS(run.ID, from, "deadlocked", now)
			if err != nil {
				return err
			}
			if claimed {
				break
			}
		}
		return nil
	}

	// §10.3 — sent BACK TO THE CHILD, never fixed by Loom.
	return i.park(run, t, res, now)
}

// park is §10.3: Loom does not resolve conflicts and does not fix failing tests.
//
// The task is parked using the SAME MECHANISM as a rendezvous (§11) — a
// `pending_seed` describing the conflict (file list) or the failure (check name
// + captured tail, capped), delivered through the existing gated send once the
// child is idle. The child still has full context on the work, which is the
// entire argument for not killing children.
//
// ORDER, and it is the durability argument, copied from Rendezvous.Seed: the
// durable columns are written BEFORE the file and before any delivery. A crash
// after the row costs a duplicate delivery attempt, which the clearing CAS
// absorbs; a crash the other way round costs a park nobody remembers is owed,
// and the child then sits at a prompt forever while the run renders as healthy.
//
// There is no `integration_blocked` state (see state.go): §10.3's own text makes
// this the same park, the same resume path and the same rendering as a
// child-authored block, and a separate state would fork §11.4's delivery
// machinery on a distinction the child cannot observe.
func (i *Integrator) park(run store.DelegationRun, t Task, res IntegrationResult, now int64) error {
	seed := IntegrationSeedText(run, res)
	b := Block{
		Version:    BlockVersion,
		Run:        run.Slug,
		Task:       t.ID,
		At:         i.now(),
		Kind:       BlockNeedsDecision,
		Author:     AuthorLoom,
		Summary:    integrationSummary(res),
		Detail:     seed,
		ResumeWhen: "the failure above is fixed and committed on this branch; Loom re-runs the integration when the branch head moves",
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if err := i.Store.SetTaskBlock(run.ID, t.ID, string(raw), now); err != nil {
		return err
	}
	if err := i.Store.SetTaskPendingSeed(run.ID, t.ID, seed, now); err != nil {
		return err
	}
	// The flag goes on WITH the column, exactly as Rendezvous.Seed sets it, and
	// for the same reason: a seed is owed from the moment the column is written.
	//
	// This park writes the column and does NOT attempt delivery — the child is
	// mid-task and the seed is delivered when it next reaches a prompt — so
	// without this the one park that owes a seed longest was the one park that
	// never said so. The alternative considered and rejected was redefining the
	// flag as "a delivery was attempted": that makes the badge a log line rather
	// than a claim, and §12.2's `block-stale` retry keys on the debt, not on the
	// history of attempts.
	if err := setTaskFlag(i.Store, run.ID, t.ID, FlagSeedPending, true, now); err != nil {
		return err
	}
	if _, err := i.Store.AdvanceTaskCAS(run.ID, t.ID, string(StateIntegrating), string(StateBlocked), now); err != nil {
		return err
	}
	// The file LAST, and its failure is loud but not fatal: the durable half is
	// already written and the task is already parked, so losing the run over a
	// meta dir that went away would convert a recoverable park into a lost one.
	if i.Blocks != nil {
		if err := i.Blocks.Write(run.Slug, t.Repo, t.ID, b); err != nil {
			return fmt.Errorf("delegate: %s is parked, but its block declaration could not be written: %w", t.ID, err)
		}
	}
	return nil
}

// rearm takes a run back out of `deadlocked` when the baseline fault that put it
// there has been repaired, and it is the missing half of §10.2's "spawning stops
// until IT IS FIXED".
//
// Without it there was no fix. §10.2's baseline fault and §12.1's deadlock both
// land on `deadlocked`, and every AdvanceDelegationRunCAS in the tree named that
// status as the DESTINATION — nothing moved a run out. One environment-shaped
// red at `pre` (§6.4's disclosed non-isolation, a busy port, one flake) therefore
// bricked the run permanently: Approve refused with ErrRunRed, Preview blocked
// every merge, and the only exit was Abandon, which strands every child's
// committed work behind a gate that cannot be reopened.
//
// The discriminator is the same one redRunReason and the run view already use: a
// run is baseline-red iff some repo's recorded Baseline is Red. So the re-arm
// fires only when a green pass has just repaired the LAST red baseline, and a
// §12.1 deadlock — which has no red baseline and, having stopped, has no green
// integration pass to trigger this either — is untouched. That permanence is
// deliberate (§12.1: "red and permanent"); a transient environment fault's is
// not, and the two were indistinguishable because they share a column.
//
// A green pass is the evidence, not a button. The alternative considered was a
// human-pressed re-arm, and it is strictly worse here: the human would be
// asserting the environment is fixed, whereas a green integration at `pre` has
// just MEASURED it. Errors are swallowed for the reason every badge write in this
// package swallows them — the pass itself succeeded, and failing it over a status
// column would turn a repair into a failure. A lost re-arm costs one more pass.
func (i *Integrator) rearm(runID int64) {
	run, found, err := i.Store.GetDelegationRun(runID)
	if err != nil || !found || run.Status != "deadlocked" {
		return
	}
	// The run row is RE-READ rather than reasoned about: this pass's green
	// baseline has already been written by record's first switch, so the map read
	// here is the whole current picture, including a second repo another Loom
	// instance may have just repaired or just broken.
	for _, b := range DecodeBaselines(run.Integration) {
		if b.Red() {
			// Some repo is still red. Un-redding now would re-open spawning
			// against a baseline nobody has repaired.
			return
		}
	}
	_, _ = i.Store.AdvanceDelegationRunCAS(runID, "deadlocked", "running", i.now().Unix())
}

// integrationSummary is the one-line form for the run view. It names the STAGE,
// because "the integration is red" is ambiguous and the remedies are completely
// different.
func integrationSummary(res IntegrationResult) string {
	switch res.Stage {
	case StageMerge:
		return fmt.Sprintf("integration merge conflicted (%d file(s))", len(res.Conflicts))
	case StageBootstrap:
		return "the repo's bootstrap failed in the integration worktree after the merge"
	case StageCross:
		return "cross check " + res.CrossCheck + " failed with this task staged"
	default:
		return "the per-repo integration check failed with this task staged"
	}
}

// IntegrationSeedText is what the child is told (§10.3): the file list for a
// conflict, the check name plus the captured tail for a failure, capped — the
// same payload ConflictSeedText carries for a §9.2 producer conflict, so the two
// parks read alike to the child.
//
// It does not re-explain the task. The child has the context — that is the whole
// argument for parking rather than killing — and a seed that restates the brief
// spends turns the child already paid for.
func IntegrationSeedText(run store.DelegationRun, res IntegrationResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Loom staged your branch for integration in run %s and it did not pass.\n\n", run.Slug)
	switch res.Stage {
	case StageMerge:
		b.WriteString("Merging your branch into the integration worktree conflicted.\n")
		if len(res.Conflicts) > 0 {
			b.WriteString("Conflicting files:\n")
			for _, f := range res.Conflicts {
				b.WriteString("  " + f + "\n")
			}
		}
		b.WriteString("\nResolve this on your own branch — Loom does not resolve conflicts for you.\n")
	case StageBootstrap:
		b.WriteString("The repo's bootstrap failed in the integration worktree after your branch was merged.\n")
	case StageCross:
		fmt.Fprintf(&b, "The cross-repo check %q failed with your branch staged.\n", res.CrossCheck)
	default:
		b.WriteString("The per-repo integration check failed with your branch staged.\n")
	}
	if out := strings.TrimSpace(res.Output); out != "" && res.Stage != StageMerge {
		b.WriteString("\nOutput:\n")
		b.WriteString(out)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nYour work was reset out of the integration branch (back to %s). Nothing of yours was lost:\n"+
		"your own branch is untouched. Commit a fix and Loom re-runs the integration.\n", short(res.Pre))
	return capSeed(b.String())
}

// short and capSeed are rendezvous.go's shortSHA and capSeed, reused rather than
// restated. §10.3 parks a task through the SAME mechanism as §11's rendezvous,
// and the whole point of that sentence is that the child cannot tell the two
// apart — a second seed cap or a second sha length here would make Loom-authored
// parks render differently from child-authored ones for no reason anyone could
// state.
var short = shortSHA

// setBaselineFor merges one repo's entry into the run's `integration` blob under
// CAS, re-reading and re-applying on a lost race.
//
// A read-modify-write and not a setter, because the blob is a MAP over repos:
// two passes for two different repos can complete concurrently (§10.2 serializes
// per RUN, and two Loom instances against one DB serialize nothing), and a plain
// setter's later write erases the other repo's freshly recorded baseline. The
// attribution table then reads a missing baseline as "no previous result", which
// flips an attribution from `baseline` to `task` and blames a child for a red the
// environment caused.
//
// The retry re-applies the SAME value and never re-runs a check — that is the
// distinction store.SetDelegationRunIntegrationCAS's comment draws, and it is
// what makes retrying here legitimate where retrying a state CAS is not. The
// bound is small and its exhaustion is an ERROR rather than a silent give-up: an
// unrecorded baseline costs the next pass a re-run, and the human should be told
// which repo it was.
func (i *Integrator) setBaselineFor(runID int64, repo string, b Baseline) error {
	for attempt := 0; attempt < 5; attempt++ {
		run, found, err := i.Store.GetDelegationRun(runID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("delegate: run %d disappeared while recording its integration baseline", runID)
		}
		m := DecodeBaselines(run.Integration)
		m[repo] = b
		claimed, err := i.Store.SetDelegationRunIntegrationCAS(runID, run.Integration, EncodeBaselines(m), i.now().Unix())
		if err != nil {
			return err
		}
		if claimed {
			return nil
		}
	}
	return fmt.Errorf("delegate: could not record the integration baseline for repo %q on run %d (contended)", repo, runID)
}

// Merge is §5.2's action and §10.4's sequence. BINDING: it merges
// `loom/<run-slug>/<task-id>` — THE TASK'S OWN BRANCH — into the user's checked
// out branch with --no-ff and a message naming run and task. Never the
// integration branch (see this file's header).
//
//  1. Merge task branch → user's branch. Task → merged.
//  2. Re-derive the integration worktree from the USER'S BRANCH HEAD
//     (reset --hard <user-branch-head>, clean -fd) and re-run
//     integration.per_repo[repo]. The staging area now stages on top of what
//     actually shipped, so every subsequent task's evidence is about the real
//     tree rather than a parallel history. Red here is a BASELINE fault by the
//     Blame table — the user's own branch is red — and no task is blamed.
//  3. Worktree removed, branch KEPT, .meta/ KEPT (§6.3).
//
// PRECONDITION: the target repo's working tree must be clean, or this refuses
// with ErrDirtyTarget and the offending files named. Merging into a dirty tree
// is how a human loses work to a machine. Never stash, never force.
//
// DISCLOSED, and it must stay in the rendering: AFTER STEP 1 THE USER'S BRANCH IS
// NOT THE TREE THE CHECK CERTIFIED. The check certified `<integration branch> +
// this task`; the user's branch is `<whatever the user had> + this task`, and the
// two differ by every commit the user made since the run's base and by every
// sibling that was staged but not merged. That is the price of §10.4's binding
// rule — merging the cumulative integration branch would make certified and
// shipped identical AND would land siblings the human never approved, which is
// the failure §5.2 exists to forbid, on the one mechanism the evidence says is
// load-bearing. Step 2 is the repair, not a formality: it re-points the staging
// area at what actually shipped, so the gap between certified and shipped is
// bounded to one task and is measured immediately rather than discovered by the
// next task's integration.
//
// Siblings in the same repo now have a base that is behind and are NOT rebased:
// they meet the merged code at their own integration step, which is exactly
// where a conflict should surface, with a check to run against the result. A
// verified sibling that was in the integration branch before step 2 is not lost
// — it is still verified, its own branch is untouched, and it has to re-earn its
// green against reality, which is the correct cost.
func (i *Integrator) Merge(ctx context.Context, run store.DelegationRun, m Manifest, t Task, force bool) (IntegrationResult, error) {
	res := IntegrationResult{RunID: run.ID, TaskID: t.ID, RepoLabel: t.Repo, RanAt: i.now()}
	if i.Store == nil {
		return res, errors.New("delegate: Integrator needs a Store")
	}
	repo := i.repoPath(m, t.Repo)
	if repo == "" {
		return res, fmt.Errorf("delegate: repo %q has no path in this run", t.Repo)
	}

	row, found, err := i.Store.GetDelegationTask(run.ID, t.ID)
	if err != nil {
		return res, err
	}
	if !found {
		return res, fmt.Errorf("delegate: task %s is not part of run %s", t.ID, run.Slug)
	}
	if TaskState(row.State) != StateMergeable && !force {
		return res, fmt.Errorf("%w: %s is %s, not mergeable", ErrTaskMovedElsewhere, t.ID, row.State)
	}

	// §10.2's serialization applies to THIS TOO, and its absence was a hazard of
	// exactly the class the claim exists to prevent. Step 2 below re-derives the
	// integration worktree — `reset --hard <user head>` plus `clean -fd` — in the
	// same directory a concurrent Integrate pass is standing in, so a merge
	// pressed mid-pass deletes that pass's staging merge from under it. The pass
	// then runs its check against a tree that does not contain the task it is
	// certifying, records a GREEN baseline for it, and promotes it to
	// `mergeable`: the gate the whole slice rests on goes green for a combination
	// nobody ran.
	//
	// Both halves, for the reason the mutex comment gives: the mutex is the
	// in-process half and the CAS's predicate — inside the UPDATE — is the
	// enforced one, because two Loom instances against one DB is supported.
	release, ok := i.acquire(run.ID)
	if !ok {
		return res, ErrIntegrationBusy
	}
	defer release()

	// The claim is taken FROM the state the row is actually in. Normally
	// `mergeable`; under §5.2's force it is whatever the human is overriding, and
	// naming it here rather than hard-coding `mergeable` is what keeps the
	// release below symmetrical.
	claimed, from, err := i.Store.ClaimTaskIntegrationCAS(run.ID, t.ID, []string{row.State}, i.now().Unix())
	if err != nil {
		return res, err
	}
	if !claimed {
		if fresh, ok2, rerr := i.Store.GetDelegationTask(run.ID, t.ID); rerr == nil && ok2 && fresh.State == row.State {
			// The row did not move, so the refusal came from the run-wide
			// exclusion: another task of this run is integrating. Next tick.
			return res, ErrIntegrationBusy
		}
		return res, fmt.Errorf("%w: %s moved during its own merge", ErrTaskMovedElsewhere, t.ID)
	}
	// Every path that is not a landed merge puts the row back where it was. A row
	// left in `integrating` by a refusal would hold the run-wide claim forever and
	// stop every other task in the run from integrating — which is the wedge
	// §10.2's watchdog row exists to recover from and which nothing should be
	// creating on a predictable path.
	landed := false
	defer func() {
		if !landed {
			_, _ = i.Store.AdvanceTaskCAS(run.ID, t.ID, string(StateIntegrating), from, i.now().Unix())
		}
	}()

	// The target is resolved HERE and not taken from a preview: a human who
	// switches branches between reading the gate and pressing it must not
	// silently land the work somewhere else.
	target, err := gitOut(repo, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return res, fmt.Errorf("delegate: %s has a detached HEAD; check out a branch before merging", repo)
	}
	target = strings.TrimSpace(target)

	// Clean tree, always, force or not. `force` is §5.2's "merge past an
	// unacknowledged divergence"; it is NOT permission to merge onto
	// uncommitted work, which is how a human loses work to a machine.
	if d := gitdiff.WorkingTree(repo); d.Error == "" && d.Dirty {
		files := append(append([]string{}, d.Files...), d.Untracked...)
		sort.Strings(files)
		return res, fmt.Errorf("%w: %s has uncommitted changes: %s", ErrDirtyTarget, repo, strings.Join(files, ", "))
	}

	// The human's OWN in-progress operation, which the dirty guard above cannot
	// see (see ErrRepoBusy). Checked before any side effect and never forced:
	// `force` is permission to merge past an unacknowledged divergence, and has
	// never been permission to throw away a resolution somebody is standing in
	// the middle of.
	if op := inProgressGitOp(repo); op != "" {
		return res, fmt.Errorf("%w: %s has an in-progress %s; finish it or abort it yourself before merging — "+
			"Loom will not touch an operation it did not start", ErrRepoBusy, repo, op)
	}

	branch := BranchName(run.Slug, t.ID)

	// §5.2's "THE DIFF SHOWN IS THE DIFF APPLIED", enforced against the sha the
	// green pass actually certified rather than against a branch NAME.
	//
	// A child is not stopped when its task reaches `mergeable` — nothing in the
	// design stops it, and §12.2's whole temperament is that Loom does not kill
	// children — so the branch head can move after the gate appears. Resolving
	// the branch at merge time then lands commits no check, no integration pass
	// and no preview ever saw, and inside the task's declared paths there is not
	// even a divergence finding to notice it.
	//
	// The refusal is unconditional in `force`, unlike everything else here.
	// Force covers a divergence the human has looked at; there is by
	// construction nothing to look at here, because the whole finding is that
	// the work was never evaluated. Runner.Tick returns the task to `running` so
	// the ordinary check → verify → integrate cycle re-certifies the new head —
	// the same re-attempt edge §10.2 gives a parked task.
	if row.CertifiedSHA != "" {
		head, herr := gitOut(repo, "rev-parse", "--verify", branch+"^{commit}")
		switch {
		case herr != nil:
			return res, fmt.Errorf("delegate: task branch %s cannot be resolved in %s: %w", branch, repo, herr)
		case strings.TrimSpace(head) != row.CertifiedSHA:
			return res, fmt.Errorf("%w: %s certified %s, the branch is now at %s — "+
				"the new commits are re-checked and re-integrated before this gate re-opens",
				ErrBranchMoved, branch, short(row.CertifiedSHA), short(strings.TrimSpace(head)))
		}
	}

	msg := fmt.Sprintf("loom: merge %s from delegation run %s\n\nBranch: %s", t.ID, run.Slug, branch)

	// 1. THE TASK'S OWN BRANCH. Never the integration branch.
	//
	// The git merge runs BEFORE the CAS, which inverts this codebase's usual
	// claim-then-act order, deliberately: the two failure modes are not
	// symmetric. A second Loom re-running this merge is a no-op ("Already up to
	// date"), while a row that says `merged` with nothing in the user's branch
	// is a lie about the one thing the human is trusting. The cheap duplicate is
	// preferred to the expensive lie, and the CAS below records what actually
	// happened rather than what was intended.
	if err := gitRun(repo, "merge", "--no-ff", "--no-edit", "-m", msg, branch); err != nil {
		files := conflictedFiles(repo)
		// The abort is SCOPED to a merge Loom started. The pre-flight above
		// proved no operation was in progress, so a MERGE_HEAD here is ours and
		// aborting it is cleaning up after ourselves; its ABSENCE means git
		// refused before starting one, and running `merge --abort` then is at
		// best a no-op and at worst — if the pre-flight is ever weakened — a
		// human's resolution thrown away with a message blaming the task branch.
		if inProgressGitOp(repo) != "" {
			_ = gitRun(repo, "merge", "--abort")
		}
		res.Stage, res.Status, res.Blame, res.Conflicts = StageMerge, CheckFail, BlameTask, files
		res.Output = fmt.Sprintf("merging %s into %s conflicted", branch, target)
		return res, fmt.Errorf("delegate: merging %s into %s: %w", branch, target, err)
	}
	landed = true
	now := i.now().Unix()
	if claimed, err := i.Store.AdvanceTaskCAS(run.ID, t.ID, string(StateIntegrating), string(StateMerged), now); err != nil {
		return res, err
	} else if !claimed {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"the merge landed but %s had already left `integrating`, so the row was not re-written", t.ID))
	}
	if force {
		// Written for the RECORD, never read as permission by anything — and
		// written through the CAS form, not SetTaskFlags. `row.Flags` was read
		// before the merge ran, and a blind write of that stale set erases any
		// flag a tick set in between (`outside-writes` is the likely one, and it
		// is the flag a human most wants beside a forced merge).
		if err := setTaskFlag(i.Store, run.ID, t.ID, FlagForced, true, now); err != nil {
			res.Warnings = append(res.Warnings, "the `forced` flag could not be recorded: "+err.Error())
		}
	}

	// 2. Re-derive the integration worktree from the USER'S BRANCH HEAD and
	// re-run the per-repo check there.
	res = i.rederive(ctx, run, m, t, repo, res)

	// 3. Worktree removed, branch KEPT, .meta/ KEPT.
	if i.Worktrees != nil {
		if err := i.Worktrees.Remove(repo, run.Slug, t.Repo, t.ID, false); err != nil {
			res.Warnings = append(res.Warnings, "the child's worktree was not removed: "+err.Error())
		}
	} else {
		res.Warnings = append(res.Warnings, "no worktree manager is configured: the child's worktree was left on disk")
	}
	return res, nil
}

// rederive is §10.4 step 2, and it is what keeps the staging area honest: after a
// merge the integration worktree is reset to the USER'S BRANCH HEAD, so every
// subsequent task's integration evidence is about the tree that actually shipped
// rather than about a parallel history.
//
// A red result here is a BASELINE fault by §10.2's table — the user's own branch
// is red — and NO TASK IS BLAMED. In particular the task that was just merged is
// not sent back: it was certified green, the human approved it, and blaming it
// for the state of a branch it has just joined is the exact mis-attribution the
// second row exists to prevent.
//
// Nothing here is fatal. The merge has already landed; a re-derivation that
// cannot run leaves the staging area stale, which is a WARNING the human must
// see and not a reason to report the merge as failed.
func (i *Integrator) rederive(ctx context.Context, run store.DelegationRun, m Manifest, t Task, repo string, res IntegrationResult) IntegrationResult {
	dir := i.Layout.IntegrationDir(run.Slug, t.Repo)
	head, err := gitOut(repo, "rev-parse", "HEAD")
	if err != nil {
		res.Warnings = append(res.Warnings, "could not read the user's branch head; the integration worktree was NOT re-derived: "+err.Error())
		return res
	}
	res.Pre = strings.TrimSpace(head)

	if err := gitRun(dir, "reset", "--hard", res.Pre); err != nil {
		res.Warnings = append(res.Warnings, "the integration worktree was NOT re-derived from the merged branch: "+err.Error())
		return res
	}
	_ = gitRun(dir, "clean", "-fd")
	res.Head = res.Pre

	spec, err := IntegrationOf(run.ManifestJSON)
	if err != nil {
		res.Warnings = append(res.Warnings, "the integration block will not parse: "+err.Error())
		return res
	}
	check, declared := spec.PerRepo[t.Repo]
	if !declared {
		res.Stage, res.Status = StageDone, CheckPass
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"repo %q declares no integration.per_repo check: the re-derived baseline carries no verdict", t.Repo))
		if err := i.setBaselineFor(run.ID, t.Repo, Baseline{Head: res.Head, At: i.now().Unix()}); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
		return res
	}

	out := i.runPerRepo(ctx, run, m, check, t.Repo, t.ID)
	res.Stage, res.Status, res.Output = StagePerRepo, out.Status, out.Output
	if out.Status == CheckPass {
		res.Stage = StageDone
	} else {
		res.Blame = BlameBaseline
		res.Warnings = append(res.Warnings,
			"the user's own branch is red after this merge: that is a BASELINE fault and no task is blamed")
	}
	if err := i.setBaselineFor(run.ID, t.Repo, Baseline{
		Head: res.Head, Status: out.Status, At: i.now().Unix(), Out: capOutput(out.Output),
	}); err != nil {
		res.Warnings = append(res.Warnings, err.Error())
	}
	return res
}

// MergePreview is what §5.2's gate renders BEFORE the human commits to anything:
// the diff that will actually be applied, the divergence that must be
// acknowledged, and the reasons the button is disabled.
//
// It exists as a struct for the same reason SpawnPreview does — the TUI and the
// GUI must not show different things before the same decision — and because §5.2
// is the load-bearing gate: it is the one place a human reads a diff a machine
// wrote into a tree they own.
type MergePreview struct {
	TaskID string
	Repo   string
	Branch string
	// Target is the user's currently checked-out branch in the primary work
	// tree, resolved at preview time and RE-CHECKED at merge time. A human who
	// switches branches between reading and pressing must not silently land the
	// work somewhere else.
	Target string
	// Dirty and DirtyFiles carry the ErrDirtyTarget precondition, named.
	Dirty      bool
	DirtyFiles []string
	// Divergence is §12.3's three comparisons. A non-empty Outside or Siblings
	// requires an EXPLICIT SECOND ACKNOWLEDGEMENT (§5.2) — it does not block.
	Divergence DivergenceReport
	// Integration is the evidence: which stage passed, against which baseline.
	Integration IntegrationResult
	// Blockers is every reason the merge is refused, rendered in full rather
	// than as a disabled button with no explanation.
	Blockers []string
	// Warnings is everything the human should read but that does not refuse:
	// a `stale-contract` flag, a `forced` history, an outside-writes finding.
	Warnings []string
}

// DivergenceReport is the pre-merge divergence bundle: §12.3.1/2's git-diff
// comparisons plus §12.3.3's snapshot drift, which is a DIFFERENT MECHANISM over
// different evidence and is kept as a separate field so the UI cannot conflate
// "the child committed outside its paths" with "a file outside the worktree
// changed". snapshot.go owns the second half.
type DivergenceReport struct {
	Outside  []string
	Siblings map[string][]string
	Drift    SnapshotDrift
}

// Preview computes MergePreview. Divergence is recomputed here and not read from
// the column: §12.3 requires it be computed on every check run AND AGAIN
// IMMEDIATELY BEFORE EVERY MERGE — before, because a divergence discovered after
// a merge is a fact, not a gate.
//
// The Integration field is reconstructed from what is DURABLE — the run's
// per-repo baseline and the task's state — because no per-task integration row
// exists (§13.1 gives the RUN an `integration` column and the task none). The
// task's state is itself the evidence and is not a weaker one: `mergeable` is
// reachable only through a green pass of §10.2, so a task at this gate has
// already earned it. What is honestly missing is the stage-by-stage detail of
// that pass, so the preview says which baseline the task was staged on rather
// than implying it has more.
func (i *Integrator) Preview(run store.DelegationRun, m Manifest, t Task) (MergePreview, error) {
	p := MergePreview{TaskID: t.ID, Repo: t.Repo, Branch: BranchName(run.Slug, t.ID)}
	if i.Store == nil {
		return p, errors.New("delegate: Integrator needs a Store")
	}
	repo := i.repoPath(m, t.Repo)
	if repo == "" {
		p.Blockers = append(p.Blockers, fmt.Sprintf("repo %q has no path in this run", t.Repo))
		return p, nil
	}

	row, found, err := i.Store.GetDelegationTask(run.ID, t.ID)
	if err != nil {
		return p, err
	}
	if !found {
		p.Blockers = append(p.Blockers, "this task is not part of the run")
		return p, nil
	}
	if TaskState(row.State) != StateMergeable {
		p.Blockers = append(p.Blockers, fmt.Sprintf("the task is %s, not mergeable", row.State))
	}
	if run.Status == "deadlocked" {
		p.Blockers = append(p.Blockers, "the run is red; see the integration baseline")
	}

	if target, terr := gitOut(repo, "symbolic-ref", "--quiet", "--short", "HEAD"); terr != nil {
		p.Blockers = append(p.Blockers, "the target repo has a detached HEAD; check out a branch first")
	} else {
		p.Target = strings.TrimSpace(target)
	}
	if d := gitdiff.WorkingTree(repo); d.Error == "" && d.Dirty {
		p.Dirty = true
		p.DirtyFiles = append(append([]string{}, d.Files...), d.Untracked...)
		sort.Strings(p.DirtyFiles)
		p.Blockers = append(p.Blockers, "the target repo has uncommitted changes; merging into a dirty tree is refused")
	}

	// §12.3, recomputed. A failure to compute it is a BLOCKER and never a
	// silently empty report: empty is the answer a human acts on, and a broken
	// capture that renders as empty is a detector that reports clean.
	if div, derr := TaskDivergence(row.Worktree, row.BaseSHA, m, t); derr != nil {
		p.Blockers = append(p.Blockers, "divergence could not be computed: "+derr.Error())
	} else {
		p.Divergence.Outside, p.Divergence.Siblings = div.Outside, div.Siblings
	}
	p.Divergence.Drift = DecodeSnapshot(row.SpawnSnapshot).Check()

	for flag := range DecodeFlags(row.Flags) {
		switch flag {
		case FlagStaleContract:
			p.Blockers = append(p.Blockers, "an interface artifact this task needs changed after it was spawned (stale-contract)")
		default:
			p.Warnings = append(p.Warnings, "flag: "+string(flag))
		}
	}
	// §10.5 evaluated LIVE rather than read from the flag: the flag is what some
	// earlier pass found, and this gate is about now.
	if drifts, derr := i.StaleContract(run, m, t); derr != nil {
		p.Warnings = append(p.Warnings, "the stale-contract alarm could not be evaluated: "+derr.Error())
	} else {
		for _, d := range drifts {
			p.Blockers = append(p.Blockers, fmt.Sprintf(
				"interface artifact %q (from %s) changed after this task was spawned: %s → %s",
				d.Artifact, d.Producer, short(d.WasCommit), short(d.NowCommit)))
		}
	}
	// An absence of evidence, rendered as one and never as evidence of absence.
	for _, id := range needsWithoutBaseline(m, t, row) {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"no contract baseline was recorded for artifact %q: the stale-contract alarm cannot speak about it", id))
	}
	// Warnings are sorted so two renderers of the same row show the same list:
	// the flag loop above ranges a map.
	sort.Strings(p.Warnings)

	base := DecodeBaselines(run.Integration)[t.Repo]
	p.Integration = IntegrationResult{
		RunID: run.ID, TaskID: t.ID, RepoLabel: t.Repo,
		Pre: base.Head, Status: base.Status, RanAt: time.Unix(base.At, 0),
	}
	if TaskState(row.State) == StateMergeable {
		p.Integration.Stage, p.Integration.Status = StageDone, CheckPass
		p.Integration.Head = base.Head
	}
	return p, nil
}

// NeedsBaseline is one entry of `delegation_tasks.needs_snapshot`: an interface
// artifact's fingerprint and commit AS THEY STOOD WHEN THE CONSUMER WAS SPAWNED.
//
// A copy, not a reference into delegation_artifacts, and that copy IS the
// mechanism: that table holds only the LATEST publication, so a producer sent
// back to work by §10.3 overwrites the fingerprint the consumer was built
// against — and without this the alarm would compare the current value with
// itself and never fire.
type NeedsBaseline struct {
	Fingerprint string `json:"fingerprint"`
	Commit      string `json:"commit"`
}

// EncodeNeedsBaselines / DecodeNeedsBaselines are that column's codec, with
// EncodeFlags' empty-string rule and DecodeFlags' degrade-never-block rule.
func EncodeNeedsBaselines(m map[string]NeedsBaseline) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func DecodeNeedsBaselines(s string) map[string]NeedsBaseline {
	out := map[string]NeedsBaseline{}
	if strings.TrimSpace(s) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return map[string]NeedsBaseline{}
	}
	return out
}

// StaleContract is §10.5's second mechanism, and its narrowness is the point.
//
// For every `kind: "interface"` artifact, §8.3 records the fingerprint and
// commit at publish time. When a CONSUMER becomes verified, Loom re-fingerprints
// every artifact that consumer `needs`. If a fingerprint changed after the
// consumer was SPAWNED, the consumer is flagged `stale-contract` naming the
// artifact and both commits, its mergeability is WITHDRAWN, and it is re-parked
// via §11's path with a seed describing the change.
//
// This catches the single most common cross-repo break — the provider revised
// the interface after the consumer built against it — with no cross-repo test at
// all. It catches NOTHING ELSE. It is not integration testing, must not be
// rendered as if it were, and is not a substitute for the missing thing (§10.5:
// no VCS operation can surface a cross-repo interface break, and this design
// does not invent one).
//
// An artifact with no recorded baseline yields NO finding, deliberately: the
// alarm compares strings, and treating "we never wrote it down" as a change
// would fire on every task spawned by an older Loom. That absence is surfaced by
// Preview as a warning, because an absence of evidence must be rendered and must
// never be presented as evidence of absence.
func (i *Integrator) StaleContract(run store.DelegationRun, m Manifest, t Task) ([]ContractDrift, error) {
	if len(t.Needs) == 0 {
		return nil, nil
	}
	row, found, err := i.Store.GetDelegationTask(run.ID, t.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("delegate: task %s is not part of run %s", t.ID, run.Slug)
	}
	was := DecodeNeedsBaselines(row.NeedsSnapshot)
	if len(was) == 0 {
		return nil, nil
	}
	arts, err := i.Store.ListDelegationArtifacts(run.ID)
	if err != nil {
		return nil, err
	}
	now := make(map[string]store.DelegationArtifact, len(arts))
	for _, a := range arts {
		now[a.ArtifactID] = a
	}
	kinds := interfaceArtifacts(m)

	var out []ContractDrift
	for _, id := range t.Needs {
		// Only `interface` artifacts carry a fingerprint contract. A data
		// artifact changing is the normal course of a run and firing on it would
		// make the alarm the thing everyone clicks through.
		if !kinds[id] {
			continue
		}
		before, ok := was[id]
		if !ok || before.Fingerprint == "" {
			continue
		}
		cur, ok := now[id]
		if !ok || cur.Fingerprint == before.Fingerprint {
			continue
		}
		out = append(out, ContractDrift{
			Artifact: id, Producer: cur.TaskID,
			WasCommit: before.Commit, WasPrint: before.Fingerprint,
			NowCommit: cur.CommitSHA, NowPrint: cur.Fingerprint,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Artifact < out[b].Artifact })
	return out, nil
}

// needsWithoutBaseline is the "we cannot tell" set: interface artifacts this
// task needs for which no fingerprint was recorded at spawn.
func needsWithoutBaseline(m Manifest, t Task, row store.DelegationTask) []string {
	kinds := interfaceArtifacts(m)
	was := DecodeNeedsBaselines(row.NeedsSnapshot)
	var out []string
	for _, id := range t.Needs {
		if kinds[id] && was[id].Fingerprint == "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// interfaceArtifacts is the set of artifact ids the manifest declares as
// `kind: "interface"` — the only ones §10.5's alarm speaks about.
func interfaceArtifacts(m Manifest) map[string]bool {
	out := map[string]bool{}
	for _, t := range m.Tasks {
		for _, a := range t.Produces {
			if a.Kind == "interface" {
				out[a.ID] = true
			}
		}
	}
	return out
}

// ContractDrift is one changed interface fingerprint.
type ContractDrift struct {
	Artifact string
	Producer string
	// WasCommit/WasPrint are what the consumer was spawned against;
	// NowCommit/NowPrint are current. Both commits are carried because the
	// remedy starts with `git log WasCommit..NowCommit -- <path>` and the human
	// should not have to reconstruct that.
	WasCommit, WasPrint string
	NowCommit, NowPrint string
}

// ─────────────────────────────────────────────────────────────────────────────
// HANDOFF NOTES — files this file does not own.
//
// internal/delegate/run.go:
//
//   - Tick step 5 calls Integrate for `verified` tasks. ErrIntegrationBusy is NOT
//     an error condition — it is next tick.
//   - Approve must refuse to spawn while the run is red ('deadlocked'), which is
//     how §10.2's "spawning stops" is actually enforced. The refusal belongs in
//     TickReport.Suppressed with the reason, not as a dropped action.
//   - A task parked by §10.3 sits in `blocked` with a needs-decision block whose
//     Author is `loom`. Rendezvous.Unblocked answers false for that kind BY
//     DESIGN (a human clears those), so the re-attempt path is the BRANCH HEAD
//     MOVING: blocked + head moved ⇒ re-check ⇒ verified ⇒ Integrate. Without
//     that edge a §10.3 park is permanent, which §10.2 explicitly is not.
//   - Merge must call Preview, compare MergeAck against the freshly computed
//     Divergence/Drift, and only then call Integrator.Merge.
//   - Spawn must write delegation_tasks.needs_snapshot (EncodeNeedsBaselines over
//     the artifacts the task `needs`, as they stand at spawn) or §10.5's alarm
//     has no baseline and stays silent. store.SetTaskNeedsSnapshot exists.
//   - Create must call Integrator.Ensure once per in-scope repo (§10.1: "created
//     at RUN creation").
//
// the run view (cmd/loom-gui, TUI):
//
//   - A run at status 'deadlocked' is EITHER §12.1's deadlock OR §10.2's baseline
//     fault, and the two read completely differently to a human. The
//     discriminator is delegation_runs.integration: a repo whose Baseline.Status
//     is not `pass` is a baseline fault and the reason is in Baseline.Out. A view
//     that renders 'deadlocked' as a wait-for cycle unconditionally shows an
//     empty cycle for every baseline fault.
//   - IntegrationResult.Warnings must be rendered. "This repo declares no
//     per-repo integration check" is the single most consequential thing this
//     file can say about a green result.
// ─────────────────────────────────────────────────────────────────────────────
