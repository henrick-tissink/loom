package delegate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
)

// §11.4 — resume. Verbatim reuse of internal/workflow's delivery machinery,
// restated here rather than imported: the seed is written to a durable
// `pending_seed` column, delivered by a send gated on transcript state, cleared
// on send, retried after a restart, and rendered as `seed pending` / `seed
// FAILED` when it is not. The re-read-before-send guard against double delivery
// applies here too — and is STRENGTHENED into a claim, because the delegation
// store has a CAS where workflow has a plain clear. See deliver for the argument
// and the cost; the difference is deliberate and is the one place this file does
// not copy workflow verbatim.
//
// Why restated and not imported: delegate sits BESIDE workflow in the dependency
// graph and deliberately does not import it (§15). The shared rule is small
// enough that copying it with a citation beats a dependency, and this paragraph
// exists so the duplication reads as a decision. The citation is
// internal/workflow/run.go's waitForContinueGate and sendPendingSeed — if the
// double-delivery guard there is ever changed, it must be changed here.
//
// THE DURABLE PLUMBING ALREADY EXISTS AND MUST NOT BE DUPLICATED:
// store.SetTaskPendingSeed writes the column and store.ClearTaskPendingSeedCAS
// clears it conditionally on the exact seed text still being there. That CAS is
// the double-delivery guard in its strongest available form — stronger than
// workflow's re-read, because two Looms cannot both win it. Wire those; do not
// write new ones.

// ONE STEP IS ADDED TO WORKFLOW'S SEQUENCE AND ITS ORDER IS LOAD-BEARING:
// MATERIALIZE, THEN SEED (§11.4).
//
//  1. The unblock condition is satisfied (producer verified, artifact published).
//  2. Loom MATERIALIZES it in the child's worktree: `git merge <producer-branch>`
//     for a same-repo dependency, or a RE-LAUNCH with the new --add-dir for a
//     cross-repo one. An add-dir cannot be added to a live session — this is the
//     one unblock that costs a restart, and §6.3's resume-by-claude-id keeps the
//     context.
//  3. ONLY THEN is the seed delivered.
//
// Seeding before materializing sends a child a statement that is FALSE when it
// reads it, and it will burn its next several turns discovering that. If step 2's
// merge conflicts, the seed says so instead and the task stays `blocked` with a
// conflict flag — it does not become a silent failure and it does not become a
// lie.

// Rendezvous owns the park-and-resume path: §10.3's integration send-back and
// §11's unforeseen dependencies both travel through it, deliberately, so there
// is ONE delivery mechanism rather than two that drift.
type Rendezvous struct {
	Store    *store.Store
	Layout   Layout
	Detector *Detector
	// Tmux is the send seam. An interface rather than *tmux.Client so the
	// ORDER — materialize, seed, clear — is testable without a terminal, the
	// same shape spawn.go's Launcher takes.
	Tmux Sender
	// Resumer is needed only for the cross-repo case, which is a RE-LAUNCH
	// (add-dirs cannot be added to a live session — spike-verified,
	// docs/spikes/2026-07-22-add-dir-spike.md).
	//
	// It is session.Launcher's RESUME and not its Launch, and the distinction is
	// the whole justification for accepting a restart here. §11.4 says the
	// restart is acceptable because §6.3's resume-by-claude-id keeps the
	// context; session.Recipe carries no claude session id at all (recipe.go),
	// so a Recipe-shaped relaunch is a FRESH claude with an empty head — the
	// child would lose exactly the thing parking was chosen to preserve.
	// Launcher.Resume re-passes the add-dirs from the row and appends to the
	// same <uuid>.jsonl, which is what this needs and already spike-verified.
	Resumer Resumer
	// ClaudeConfigDir locates the transcript for the continue gate.
	ClaudeConfigDir string
	// PollEvery / Timeout mirror session.Launcher's defaults rather than adding
	// package-local knobs; 0 means the same defaults workflow uses (500ms / 60s).
	PollEvery time.Duration
	Timeout   time.Duration
	// Width/Height are the relaunch geometry, same defaults and same reason as
	// Spawner's: a 0×0 tmux window is not a smaller window, it is an unusable
	// one.
	Width, Height int
	Now           func() time.Time
}

// Sender is the tmux surface this file needs.
//
// SendLiteral and not a keystroke API: a seed is text a human would have typed,
// and anything that interprets it is a way for an artifact path to become a key
// chord. SendEnter is separate and is REQUIRED — a literal send leaves the text
// sitting unsubmitted in the child's composer, which reads as "delivered" to
// every row in the database and is a park nobody is ever told about. workflow's
// sendPendingSeed sends both, in this order, for the same reason.
//
// KillSession is here because the cross-repo unblock is a restart: the old child
// must be gone before the resumed one exists, or §6.2's two-claudes-one-worktree
// is reached through the resume path instead of the spawn path it was made
// structurally impossible in.
type Sender interface {
	SendLiteral(session, s string) error
	SendEnter(session string) error
	KillSession(session string) error
}

// Resumer is session.Launcher's Resume, narrowed. Same shape and same reason as
// spawn.go's Launcher: the ORDER around a relaunch is testable without tmux.
type Resumer interface {
	Resume(old store.SessionRow, w, h int, now time.Time) (string, error)
}

// ErrChildGone is a park whose child is no longer there — no session row, or a
// row that has ended. The seed is written and STAYS written; what this reports is
// that nobody can be told about it right now.
//
// It is an error and not a silent retry because §12.2's rule is flag-never-kill
// and always-visible: a blocked task with a dead session and an owed seed is a
// human decision (re-spawn by claude id, or abandon), and a delivery loop that
// kept trying forever would render as "seed pending" indefinitely with no one
// told which of the two situations it is.
var ErrChildGone = errors.New("delegate: the parked task has no live child session")

// ErrSeedUndelivered is the continue gate timing out. NOT a failure of the seed:
// the column stays set, the `seed-pending` flag stays on, and §12.2's retry
// re-attempts delivery. The seed is owed until it is delivered.
var ErrSeedUndelivered = errors.New("delegate: the child was not ready to receive the seed — it is still owed")

// Unblocked reports whether a blocked task's condition is satisfied — the
// machine-checkable one derived from the block's Need and the graph, NEVER the
// child's prose `resume_when`. Treating a sentence an agent wrote as an
// executable predicate is how a park becomes permanent.
//
// For BlockNeedsArtifact: the producer is verified AND the artifact is
// published, which is §9.1's ready condition applied to one edge. For the other
// three kinds it is always false — a human clears those, and the clearing is an
// explicit act with a row behind it.
func (r *Rendezvous) Unblocked(e EffectiveGraph, states map[string]TaskState, published map[string]bool, taskID string) bool {
	b := e.Blocks[taskID]
	if b.Kind != BlockNeedsArtifact {
		// Includes the no-block case. Nothing to unblock is not the same as
		// unblocked, and returning true here would offer a resume for a task
		// that never parked.
		return false
	}
	art := strings.TrimSpace(b.Need.Artifact)
	if art == "" {
		return false
	}
	if !published[art] {
		return false
	}
	producer := producerOf(e, art)
	if producer == "" {
		// §11.3's re-plan case: nobody produces it, so no amount of waiting
		// satisfies it. The remedy is a human amending the plan, not a resume.
		return false
	}
	// The producer states are enumerated, and the enumeration is COPIED FROM
	// graph.go's needsMet rather than shared with it — the same rule and the same
	// reason: "which states mean the producer is done" is a safety property, and a
	// consumer's park must not start resuming on a state that would not have
	// been allowed to spawn it in the first place.
	//
	// It must be the SAME FOUR states needsMet names, not the {verified, merged}
	// pair this originally carried. A resume decision and a ready decision that
	// disagree about one edge is the withdrawn-offer hazard needsMet documents,
	// seen from the other side: with only {verified, merged}, a consumer parked on
	// an artifact whose producer is `integrating` would answer false, §12.2's
	// block-stale watchdog would never fire (the condition is not satisfied), and
	// the park would sit through the whole of §10.2's integration and §5.2's human
	// gate before resuming — a rendezvous made LATE by the producer making
	// progress. Every one of the four means the check went green and §8.3
	// published the artifact from a commit, so nothing is weakened by naming them.
	switch states[producer] {
	case StateVerified, StateIntegrating, StateMergeable, StateMerged:
		return true
	}
	return false
}

// Materialization records what step 2 actually did, because the seed text is
// derived from it: "account-schema is now present at db/migrations/0007_account.sql
// in your worktree" is only true if the merge happened, and a seed that names a
// path is worth several turns of a child's context compared with "continue".
type Materialization struct {
	TaskID string
	// Merged is the producer branches merged into the child's worktree.
	Merged []ProducerRef
	// AddedDirs is the cross-repo add-dirs the relaunch granted.
	AddedDirs []string
	// Relaunched is true when step 2 had to restart the child. Rendered,
	// because a restart is the one unblock that costs the human something
	// visible and they should not have to infer it from a new tmux window.
	Relaunched bool
	// Paths maps artifact id → its path in the child's worktree, for the seed.
	Paths map[string]string
	// Conflict is set when the merge conflicted. The task STAYS blocked, the
	// seed becomes a conflict description instead of a continue, and the flag is
	// visible — the failure is not silent and is not laundered into a success.
	Conflict *ProducerConflict
}

// Materialize is step 2. It runs BEFORE any seed is written, and its failure is
// a seed of a different kind rather than an absence of one.
func (r *Rendezvous) Materialize(run store.DelegationRun, m Manifest, t Task, b Block) (Materialization, error) {
	mat := Materialization{TaskID: t.ID, Paths: map[string]string{}}
	if b.Kind != BlockNeedsArtifact {
		// The other three kinds are cleared by a human, and there is nothing on
		// disk to make true. Returning an empty materialization rather than an
		// error keeps ONE resume path: the seed for a human-cleared block is a
		// plain continue, produced by the same SeedText.
		return mat, nil
	}
	producer, art, ok := artifactOwner(m, b.Need.Artifact)
	if !ok {
		return mat, fmt.Errorf("%w: %q", ErrNoSuchArtifact, b.Need.Artifact)
	}
	consumerRow, found, err := r.Store.GetDelegationTask(run.ID, t.ID)
	if err != nil {
		return mat, err
	}
	if !found {
		return mat, fmt.Errorf("delegate: run %d has no row for task %q", run.ID, t.ID)
	}
	producerRow, found, err := r.Store.GetDelegationTask(run.ID, producer.ID)
	if err != nil {
		return mat, err
	}
	if !found {
		return mat, fmt.Errorf("delegate: run %d has no row for producer %q", run.ID, producer.ID)
	}

	if producer.Repo == t.Repo {
		// Same repo: merge the producer's branch into the child's worktree, BY
		// SHA, for the reason MergeProducers gives — the branch is a moving
		// target and the sha is what §12.3's divergence computes against.
		ref := ProducerRef{Task: producer.ID, Branch: producerRow.Branch, SHA: producerRow.BranchHead}
		dir := consumerRow.Worktree
		if dir == "" {
			dir = physicalPath(r.Layout.Dir(run.Slug, t.Repo, t.ID))
		}
		if err := MergeProducers(dir, []ProducerRef{ref}); err != nil {
			var conflict *ProducerConflict
			if errors.As(err, &conflict) {
				// A conflict is a FINDING, not a failure of this function: the
				// caller turns it into a seed of a different kind and the task
				// stays blocked. Returning it as an error too would make the
				// caller choose between reporting it and acting on it.
				conflict.Task = t.ID
				mat.Conflict = conflict
				return mat, nil
			}
			return mat, err
		}
		mat.Merged = []ProducerRef{ref}
		mat.Paths[art.ID] = art.Path
		return mat, nil
	}

	// Cross repo: the artifact arrives as an --add-dir on the producer's repo
	// integration worktree, and an add-dir cannot be added to a live session
	// (spike). This is the one unblock that costs a restart.
	dir := physicalPath(r.Layout.IntegrationDir(run.Slug, producer.Repo))
	mat.AddedDirs = []string{dir}
	mat.Paths[art.ID] = filepath.Join(dir, art.Path)
	if err := r.relaunch(run, t, consumerRow, mat.AddedDirs); err != nil {
		return mat, err
	}
	mat.Relaunched = true
	return mat, nil
}

// artifactOwner finds the task that declares an artifact, and the artifact.
// Manifest order, first declaration wins — the same rule BuildGraph applies to
// Producer, so the two can never disagree about who owns an id.
func artifactOwner(m Manifest, artifactID string) (Task, Artifact, bool) {
	artifactID = strings.TrimSpace(artifactID)
	for _, t := range m.Tasks {
		for _, a := range t.Produces {
			if a.ID == artifactID {
				return t, a, true
			}
		}
	}
	return Task{}, Artifact{}, false
}

// relaunch is the cross-repo restart: end the old child, resume it BY CLAUDE
// SESSION ID with the new add-dir, and re-bind the task row to the new session.
//
// The order is kill-then-resume and not the reverse. Two claudes in one worktree
// on one branch is the worst outcome in this design (§6.2 step 3), and the resume
// path must not be the hole through which it is reached — so the old session is
// killed and its row ended BEFORE the new one exists, even though that leaves a
// window in which the task has no child at all. A window with no child is
// recoverable; two children writing the same branch is not.
//
// The task is walked blocked → spawning → running through the SAME CAS pair the
// spawn path uses, rather than through a bespoke "rebind" write: `spawning` is
// exactly what this is, BindTaskSessionCAS is the only writer of session_name,
// and inventing a second one would leave two ways for a task row to acquire a
// child.
func (r *Rendezvous) relaunch(run store.DelegationRun, t Task, row store.DelegationTask, addDirs []string) error {
	if r.Resumer == nil {
		return fmt.Errorf("delegate: task %q needs a cross-repo relaunch but no resumer is configured", t.ID)
	}
	if row.SessionName == "" {
		return fmt.Errorf("%w: task %q has no session to resume", ErrChildGone, t.ID)
	}
	old, ok, err := r.Store.Get(row.SessionName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: task %q names session %q, which has no row", ErrChildGone, t.ID, row.SessionName)
	}
	now := r.now()
	if old.EndedAt == -1 && r.Tmux != nil {
		// Best effort, and deliberately so: a tmux session that is already gone
		// is the outcome we wanted. What must NOT be best effort is the row —
		// an un-ended row keeps counting against §6.6's cap forever.
		_ = r.Tmux.KillSession(old.Name)
	}
	if old.EndedAt == -1 {
		if err := r.Store.MarkEnded(old.Name, "ended", 0, now.Unix()); err != nil {
			return err
		}
	}

	claimed, _, err := r.Store.AdvanceTaskFromAnyCAS(run.ID, t.ID, []string{string(StateBlocked)}, string(StateSpawning), now.Unix())
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("%w: task %q was not blocked when the relaunch started", ErrTaskMovedElsewhere, t.ID)
	}

	dirs := mergeAddDirs(session.DecodeAddDirs(old.AddDirs), addDirs)
	w, h := r.geometry()
	name, err := r.Resumer.Resume(relaunchRow(old, dirs), w, h, now)
	if err != nil {
		// Nothing was created, so the claim is released back to `blocked` — the
		// state the human will see, with the error beside it. The release is
		// itself a CAS, so a concurrent abandon wins.
		_, _ = r.Store.AdvanceTaskCAS(run.ID, t.ID, string(StateSpawning), string(StateBlocked), r.now().Unix())
		return fmt.Errorf("delegate: task %q: relaunch: %w", t.ID, err)
	}
	// Same two links spawn.go writes, and surfaced for the same reason: a child
	// without its delegation link renders as Ungrouped and vanishes the moment
	// anything is hidden (§14.1).
	tagErr := r.Store.SetTags(name, DelegationTag(run.Slug, t.ID))
	delErr := r.Store.SetSessionDelegation(name, FormatDelegation(run.ID, t.ID))
	bound, bindErr := r.Store.BindTaskSessionCAS(run.ID, t.ID, name, r.now().Unix())
	switch {
	case delErr != nil:
		return fmt.Errorf("delegate: task %q: session %s resumed but not linked to its run: %w", t.ID, name, delErr)
	case tagErr != nil:
		return fmt.Errorf("delegate: task %q: session %s resumed but not tagged: %w", t.ID, name, tagErr)
	case bindErr != nil:
		return fmt.Errorf("delegate: task %q: session %s resumed but the task row was not bound: %w", t.ID, name, bindErr)
	case !bound:
		return fmt.Errorf("delegate: task %q: session %s resumed but the task left `spawning` first — the child is live", t.ID, name)
	}
	return nil
}

// mergeAddDirs unions the row's existing add-dirs with the new ones, preserving
// the original order and appending what is new. Order is preserved rather than
// sorted because the row is written back and a re-ordered list would make every
// "did the grant change" comparison a false positive.
func mergeAddDirs(existing, added []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(existing)+len(added))
	for _, d := range append(append([]string(nil), existing...), added...) {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// Resume is the whole of §11.4 in order: Unblocked → Materialize → Seed →
// Detector.Clear → CAS blocked → running. The CAS is LAST among the state
// writes: a task marked running whose seed never arrived is a child sitting at a
// prompt that Loom believes is working, and §12.2's `block-stale` watchdog would
// have nothing left to point at.
func (r *Rendezvous) Resume(run store.DelegationRun, m Manifest, t Task, b Block) error {
	row, ok, err := r.Store.GetDelegationTask(run.ID, t.ID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("delegate: run %d has no row for task %q", run.ID, t.ID)
	}
	if TaskState(row.State) != StateBlocked {
		// Not a retryable condition: the premise this resume was computed from
		// is gone (runs.go's rule — a CAS is never retried in a loop).
		return fmt.Errorf("%w: task %q is %s, not blocked", ErrTaskMovedElsewhere, t.ID, row.State)
	}

	mat, err := r.Materialize(run, m, t, b)
	if err != nil {
		return err
	}
	if mat.Conflict != nil {
		// Step 2 conflicted. The task STAYS blocked and the seed says so —
		// telling the child to continue would be a statement that is false when
		// it reads it. The seed error, if any, is joined rather than replacing
		// the conflict: the conflict is the finding the human needs.
		// The badge goes on FIRST, before the seed. §11.4 says the task stays
		// blocked "with a conflict flag", and the seed write can itself fail (the
		// continue gate times out routinely); flagging afterwards would leave the
		// one case where the human most needs to find this row as the one case
		// where the row is unbadged.
		r.flagConflict(run.ID, t.ID)
		if serr := r.Seed(run, t.ID, ConflictSeedText(b, mat.Conflict)); serr != nil {
			return errors.Join(mat.Conflict, serr)
		}
		return mat.Conflict
	}

	if err := r.Seed(run, t.ID, SeedText(m, t, b, mat)); err != nil {
		// Undelivered or child gone: the seed is durably owed, the block stays
		// on disk, the task stays blocked. Everything about this state is
		// visible and nothing about it is lost.
		return err
	}
	return r.finishResume(run, t)
}

// finishResume is the tail both Resume and ApplyScope share: the declaration is
// removed, the durable copy is cleared, and only then does the task go back to
// `running`.
//
// The CAS is LAST, and a rejected one is tolerated ONLY when the row is already
// `running` — which is the ordinary outcome of a cross-repo relaunch, since
// BindTaskSessionCAS moved it there while materializing. Any other state means
// something else moved the task and this resume must say so rather than paper
// over it.
func (r *Rendezvous) finishResume(run store.DelegationRun, t Task) error {
	if r.Detector != nil {
		if err := r.Detector.Clear(run.Slug, t.Repo, t.ID); err != nil {
			return err
		}
	}
	now := r.now().Unix()
	// A resume that got this far MATERIALIZED cleanly, so an earlier conflict
	// badge is stale and must come off — a flag that only ever goes on is a flag
	// the human learns to ignore. Cleared before the state moves, so the row is
	// never `running` and badged `conflict` at the same time.
	_ = setTaskFlag(r.Store, run.ID, t.ID, FlagConflict, false, now)
	if err := r.Store.SetTaskBlock(run.ID, t.ID, "", now); err != nil {
		return err
	}
	claimed, err := r.Store.AdvanceTaskCAS(run.ID, t.ID, string(StateBlocked), string(StateRunning), now)
	if err != nil {
		return err
	}
	if claimed {
		return nil
	}
	fresh, ok, err := r.Store.GetDelegationTask(run.ID, t.ID)
	if err != nil {
		return err
	}
	if ok && TaskState(fresh.State) == StateRunning {
		return nil
	}
	return fmt.Errorf("%w: task %q left `blocked` during its resume", ErrTaskMovedElsewhere, t.ID)
}

// ApplyScope is §11.3's needs-scope branch, and it is the one place in this file
// that grants something rather than materializing something.
//
// NEVER AUTO-GRANTED: an unapproved amendment is refused before the brief is
// touched, so the file on disk cannot record an authorization no human agreed to.
// The refusal is the first statement in the function for exactly that reason.
//
// The brief is rewritten IN PLACE. A second file would leave two answers to "what
// am I allowed to touch" and the child will find the wrong one after a
// compaction; an amendment section appended to the one brief keeps the original
// authorization visible beside what was added to it, which is what a human
// auditing an overreach needs to read.
func (r *Rendezvous) ApplyScope(run store.DelegationRun, t Task, a Amendment) error {
	if a.Kind != AmendScope {
		return fmt.Errorf("delegate: ApplyScope needs a %s amendment, got %q", AmendScope, a.Kind)
	}
	if a.ApprovedAt.IsZero() {
		return fmt.Errorf("%w: scope widening for task %q", ErrAmendmentNotApproved, t.ID)
	}
	if len(a.Paths) == 0 {
		// A widening that names nothing is not a grant, and writing it into the
		// brief would tell the child its boundary moved without saying where.
		return fmt.Errorf("delegate: scope amendment for task %q grants no paths", t.ID)
	}
	if err := r.rewriteBrief(run, t, a); err != nil {
		return err
	}
	if err := r.Seed(run, t.ID, ScopeSeedText(t, a)); err != nil {
		return err
	}
	return r.finishResume(run, t)
}

// rewriteBrief appends the approved amendment to brief.md, atomically. The file
// must already exist: a missing brief means the meta dir is gone, and writing a
// fresh file containing ONLY an amendment would hand the child a document whose
// every original constraint has silently disappeared.
func (r *Rendezvous) rewriteBrief(run store.DelegationRun, t Task, a Amendment) error {
	path := r.Layout.BriefPath(run.Slug, t.Repo, t.ID)
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("delegate: task %q: cannot amend brief: %w", t.ID, err)
	}
	var b strings.Builder
	b.Write(existing)
	if !strings.HasSuffix(string(existing), "\n") {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n## Authorization amendment — approved %s\n\n", a.ApprovedAt.UTC().Format(time.RFC3339))
	b.WriteString("A human has widened this task's authorization. Section 2 above still applies;\nthese paths are added to it, and nothing has been taken away.\n\n")
	for _, p := range a.Paths {
		fmt.Fprintf(&b, "- `%s`\n", p)
	}
	if a.Reason != "" {
		fmt.Fprintf(&b, "\nRequested because: %s\n", a.Reason)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Seed writes the durable pending_seed and attempts delivery. Split from Resume
// so §12.2's `block-stale` retry and a post-restart sweep can re-attempt
// delivery without redoing materialization — re-merging an already-merged
// producer branch is harmless but re-LAUNCHING a child is not.
//
// Order, and it is the durability argument: write the column FIRST, then send.
// A crash between the two costs a duplicate delivery attempt, which the clearing
// CAS absorbs; a crash the other way round costs a seed nobody remembers is
// owed.
func (r *Rendezvous) Seed(run store.DelegationRun, taskID, seed string) error {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return fmt.Errorf("delegate: task %q: refusing to owe an empty seed", taskID)
	}
	now := r.now().Unix()
	if err := r.Store.SetTaskPendingSeed(run.ID, taskID, seed, now); err != nil {
		return err
	}
	// The flag is set BEFORE the attempt, not after a failure. A seed that is
	// owed is owed from the moment the column is written, and setting the badge
	// only on the failure path means a crash mid-delivery leaves a debt nothing
	// names.
	if err := setTaskFlag(r.Store, run.ID, taskID, FlagSeedPending, true, now); err != nil {
		return err
	}

	row, ok, err := r.Store.GetDelegationTask(run.ID, taskID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("delegate: run %d has no row for task %q", run.ID, taskID)
	}
	if row.SessionName == "" {
		return fmt.Errorf("%w: task %q has no session", ErrChildGone, taskID)
	}
	sess, ok, err := r.Store.Get(row.SessionName)
	if err != nil {
		return err
	}
	if !ok || sess.EndedAt != -1 {
		// REPORTED, not retried forever. The seed stays in the column and the
		// flag stays on; what the human is told is that the child this seed is
		// owed to is gone, which is a different decision (re-spawn, or abandon)
		// from "the child was busy".
		return fmt.Errorf("%w: task %q session %q", ErrChildGone, taskID, row.SessionName)
	}
	return r.deliver(sess, row)
}

// flagConflict badges §11.4's materialization conflict. Its error is DROPPED,
// and that is the same rule the rest of this file follows for badges: the
// conflict is already durable in pending_seed and already returned to the
// caller, so a lost badge costs a row's colour, while a returned error here
// would replace the conflict — the finding the human actually needs — with a
// complaint about writing a flag.
func (r *Rendezvous) flagConflict(runID int64, taskID string) {
	_ = setTaskFlag(r.Store, runID, taskID, FlagConflict, true, r.now().Unix())
}

// setTaskFlag is the flag read-modify-write as a CAS on the exact column text,
// shared with the detector so there is one flag writer discipline in this
// package. See Detector.setFlag's comment for why a rejected CAS is dropped
// rather than retried.
func setTaskFlag(st *store.Store, runID int64, taskID string, flag Flag, on bool, now int64) error {
	row, ok, err := st.GetDelegationTask(runID, taskID)
	if err != nil || !ok {
		return err
	}
	flags := DecodeFlags(row.Flags)
	if flags[flag] == on {
		return nil
	}
	if on {
		flags = flags.With(flag)
	} else {
		flags = flags.Without(flag)
	}
	_, err = st.SetTaskFlagsCAS(runID, taskID, row.Flags, EncodeFlags(flags), now)
	return err
}

// deliver is the gated send: wait for the transcript to reach a state that will
// actually consume typed text, RE-READ the row, send only if the pending seed is
// still exactly what we hold, then clear via CAS.
//
// The re-read is not paranoia — it is workflow's disclosed race: a concurrent
// retry (or the other Loom instance) may already have delivered and cleared
// between this call being scheduled and running. Sending unconditionally on a
// stale snapshot double-delivers into a live agent's prompt.
// It differs from workflow's send-then-clear in ONE respect, and the difference
// is the point: the clearing CAS runs FIRST and IS the claim. workflow's re-read
// narrows the double-delivery window but cannot close it — two deliverers can
// both re-read the same non-empty seed and both send, which is a duplicated
// instruction injected into a live agent's prompt. store.ClearTaskPendingSeedCAS
// is conditioned on the exact seed text, so exactly one caller can win it, across
// processes as well as goroutines; the loser no-ops. workflow could not do this
// because its ClearPendingSeed is not a CAS.
//
// The cost is a crash between the claim and the send, which would lose a seed
// that the column no longer names. That is bounded by re-arming the column on any
// send failure, and it is the cheaper of the two: a lost seed renders as a task
// sitting `blocked` with no `seed-pending` badge, which a human sees, while a
// double delivery is two contradictory instructions inside a context nobody can
// inspect.
func (r *Rendezvous) deliver(row store.SessionRow, task store.DelegationTask) error {
	if task.PendingSeed == "" {
		return nil
	}
	if !r.waitForContinueGate(row) {
		return fmt.Errorf("%w: task %q session %q", ErrSeedUndelivered, task.TaskID, row.Name)
	}
	// Re-read fresh. The gate can block for the whole timeout, and a concurrent
	// deliverer — the other Loom instance, or a §12.2 retry — may have replaced
	// the seed with a newer one in the meantime. Sending the snapshot this call
	// started with would deliver a superseded instruction.
	fresh, ok, err := r.Store.GetDelegationTask(task.RunID, task.TaskID)
	if err != nil {
		return err
	}
	if !ok || fresh.PendingSeed == "" || fresh.PendingSeed != task.PendingSeed {
		return nil
	}
	claimed, err := r.Store.ClearTaskPendingSeedCAS(task.RunID, task.TaskID, task.PendingSeed, r.now().Unix())
	if err != nil {
		return err
	}
	if !claimed {
		return nil // somebody else owns this delivery
	}
	if err := r.sendSeed(row.Name, task); err != nil {
		return err
	}
	// Only now is the debt discharged. Clearing the badge before the send would
	// make an un-sent seed invisible, which is the one thing §11.4 says must not
	// happen.
	return setTaskFlag(r.Store, task.RunID, task.TaskID, FlagSeedPending, false, r.now().Unix())
}

// sendSeed is the two-part send, plus the re-arm that makes the claim-first
// ordering safe. The literal and the Enter are separate tmux calls, so a failure
// between them leaves text in the composer that nobody submitted — re-arming the
// column is what makes that state recoverable instead of invisible.
func (r *Rendezvous) sendSeed(sessionName string, task store.DelegationTask) error {
	sendErr := r.Tmux.SendLiteral(sessionName, task.PendingSeed)
	if sendErr == nil {
		sendErr = r.Tmux.SendEnter(sessionName)
	}
	if sendErr == nil {
		return nil
	}
	// Re-arm ONLY if the column is still empty. A newer seed written between the
	// claim and this point supersedes the one that failed, and restoring the old
	// text over it would deliver the stale instruction later.
	if fresh, ok, err := r.Store.GetDelegationTask(task.RunID, task.TaskID); err == nil && ok && fresh.PendingSeed == "" {
		_ = r.Store.SetTaskPendingSeed(task.RunID, task.TaskID, task.PendingSeed, r.now().Unix())
	}
	return fmt.Errorf("delegate: task %q: delivering seed to %s: %w", task.TaskID, sessionName, sendErr)
}

// waitForContinueGate blocks until the child's transcript reports a state that
// will consume typed text, or the timeout expires.
//
// Gating on the ENGINE'S TRANSCRIPT-DERIVED STATE and not a raw pane read is the
// rule, copied from workflow: a pane read is a picture of a UI, and this
// codebase matches claude's rendered UI by SHAPE, never by string. StateNeedsYou
// and StateIdle are the two that accept input; anything else means the child is
// mid-turn and the text would be swallowed or, worse, interleaved.
//
// A timeout is NOT a failure that clears the seed. The column stays set, the run
// renders `seed pending`, and §12.2's `block-stale` offers the retry — the seed
// is owed until it is delivered.
func (r *Rendezvous) waitForContinueGate(row store.SessionRow) bool {
	// The two states that accept typed text. Named here rather than in the body,
	// because "which states are safe to send into" is the one decision in this
	// function and it is copied from workflow, not derived.
	accepts := func(s transcript.State) bool {
		return s == transcript.StateNeedsYou || s == transcript.StateIdle
	}
	rdr := transcript.NewReader(transcript.Path(r.ClaudeConfigDir, row.Cwd, row.ClaudeSessionID))
	poll := r.PollEvery
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	waited := time.Duration(0)
	for {
		// A read error is treated exactly like "not ready": a transcript that
		// does not exist yet is the normal state of a session that has only just
		// been resumed, and failing the gate on it would turn every relaunch
		// into an undelivered seed.
		if snap, err := rdr.Poll(); err == nil && accepts(snap.State) {
			return true
		}
		if waited >= timeout {
			return false
		}
		time.Sleep(poll)
		waited += poll
	}
}

func (r *Rendezvous) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Rendezvous) geometry() (int, int) {
	w, h := r.Width, r.Height
	if w <= 0 {
		w = 120
	}
	if h <= 0 {
		h = 32
	}
	return w, h
}

// SeedText renders the resume message. Derived from the materialization, so it
// states facts that are true at the moment the child reads them.
//
// Capped at the same 8KB+15KB budget the rest of Loom's seeds use, and the CAP
// MATTERS here: tmux send-keys has a measured argv ceiling around 16.3KB, which
// is why §7's brief is a POINTER to a file rather than the text. A resume seed
// that exceeds it is truncated, never split — a half-delivered instruction is
// worse than a short one.
func SeedText(m Manifest, t Task, b Block, mat Materialization) string {
	var s strings.Builder
	// The artifact and its PATH first: that sentence is the whole value of this
	// seed, and it is the one a child can act on without re-reading anything.
	if art := strings.TrimSpace(b.Need.Artifact); art != "" {
		if p := mat.Paths[art]; p != "" {
			fmt.Fprintf(&s, "`%s` is now present at `%s` in your worktree.", art, p)
		} else {
			fmt.Fprintf(&s, "`%s` is now available.", art)
		}
	} else {
		s.WriteString("Your block has been cleared by a human.")
	}
	for _, ref := range mat.Merged {
		fmt.Fprintf(&s, "\nProducer task `%s` was merged into your branch at `%s`.", ref.Task, shortSHA(ref.SHA))
	}
	if len(mat.AddedDirs) > 0 {
		fmt.Fprintf(&s, "\nThese directories are now readable and MUST NOT be written: %s.", backquoteList(mat.AddedDirs))
	}
	if mat.Relaunched {
		// Said out loud, because the child can otherwise only infer it, and a
		// child that does not know it was restarted will re-derive state it
		// still has.
		s.WriteString("\nYour session was restarted to grant that directory; your conversation was resumed, not reset.")
	}
	s.WriteString("\nYour authorization is unchanged. Continue.")
	return capSeed(s.String())
}

// shortSHA is the rendering length git itself defaults to. A full sha in a seed
// is 40 characters of a child's context spent on something it will never type.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// capSeed bounds a seed at the budget the rest of Loom uses. TRUNCATED, never
// split: tmux send-keys has a measured argv ceiling around 16.3KB, and half an
// instruction is worse than a short one.
func capSeed(s string) string {
	const cap = 8 << 10
	if len(s) <= cap {
		return s
	}
	return s[:cap] + "\n… (truncated)"
}

// ConflictSeedText renders the step-2-conflicted variant: the task stays blocked
// and the child is told exactly which files conflicted and between which
// branches, which is the same payload §10.3's integration send-back carries.
func ConflictSeedText(b Block, c *ProducerConflict) string {
	var s strings.Builder
	s.WriteString("Loom could not materialize what you are blocked on: merging the producer's work\ninto your worktree CONFLICTED. Nothing was changed — the merge was aborted and\nyour tree is clean.\n")
	if art := strings.TrimSpace(b.Need.Artifact); art != "" {
		fmt.Fprintf(&s, "\nBlocked on: `%s`.", art)
	}
	if c != nil {
		var names []string
		for _, ref := range c.Between {
			names = append(names, "`"+ref.Task+"`")
		}
		if len(names) > 0 {
			fmt.Fprintf(&s, "\nProducers that disagree: %s.", strings.Join(names, " then "))
		}
		if len(c.Files) > 0 {
			fmt.Fprintf(&s, "\nConflicting files:\n")
			for _, f := range c.Files {
				fmt.Fprintf(&s, "- `%s`\n", f)
			}
		}
	}
	// The instruction is explicitly NOT "resolve it". Two producers disagreeing
	// about the same lines is information about the PLAN, and asking a child to
	// settle it is asking it to make a design decision it was not authorized for
	// (§9.2). It stays parked.
	s.WriteString("\nDo NOT resolve this yourself and do not modify those files: two tasks disagree about\nthem, which is a planning decision that belongs to a human. Stay at your prompt.\n")
	return capSeed(s.String())
}

// ScopeSeedText renders the §11.3 needs-scope resume: an accepted authorization
// amendment REWRITES brief.md in place and re-seeds the child. In place, because
// the brief is the child's durable record of what it was told — a second brief
// file leaves two answers to "what am I allowed to touch", and the child will
// find the wrong one after a compaction.
func ScopeSeedText(t Task, a Amendment) string {
	var s strings.Builder
	s.WriteString("A human has APPROVED a widening of your authorization. Your brief has been rewritten\nin place — re-read it before continuing.\n\nYou may now also modify:\n")
	for _, p := range a.Paths {
		fmt.Fprintf(&s, "- `%s`\n", p)
	}
	// Everything else still holds, said explicitly: a child told its boundary
	// moved will otherwise reason about which of the original constraints the
	// amendment replaced, and the answer is none of them.
	s.WriteString("\nNothing else changed. Every other constraint in your brief still applies, including\nwriting only inside your own worktree. Continue.\n")
	return capSeed(s.String())
}

// relaunchRow is the cross-repo re-launch input: the child's existing session row
// with the new add-dir folded in.
//
// A ROW and not a session.Recipe, and this is a correction to the sketched shape
// rather than a preference. session.Recipe carries no claude session id
// (recipe.go), so Launcher.Launch over one starts a FRESH conversation — the
// context this whole park-instead-of-restart design exists to preserve would be
// gone. Launcher.Resume takes the old row, resumes by ClaudeSessionID (spike:
// --resume appends to the SAME <uuid>.jsonl) and re-passes AddDirs from the row,
// which is exactly the "one unblock that costs a restart" §11.4 describes.
//
// Only AddDirs is changed. Cwd, model, mode and tags are carried untouched so
// the resumed child is the same child in the same worktree — Resume does its own
// physical-path resolution, so no second copy of that rule appears here.
func relaunchRow(old store.SessionRow, addDirs []string) store.SessionRow {
	old.AddDirs = session.EncodeAddDirs(addDirs)
	return old
}
