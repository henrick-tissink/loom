package delegate

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
)

// The concurrency cap (§6.6, BINDING). Default 4, configurable 1-10, hard
// maximum 10. 3a runs at 3.
//
// The reasons, in the order they bite:
//
//  1. practitioner reports put the overhead crossover at roughly 8-10 concurrent
//     worktrees — disk, index rebuilds, and above all the review bandwidth of
//     the ONE human who has to look at every diff at the merge gate. Past that
//     the queue is the human, and adding children lengthens it;
//  2. §6.4's shared resources collide superlinearly — ports and a shared DB are
//     pairwise-conflicting, so four children is six pairs and ten is forty-five;
//  3. each child is a real claude with real quota;
//  4. Loom's own poll loop is O(sessions). This is the least important reason
//     and is listed last on purpose: it is a fixable engineering cost, unlike
//     1-3.
const (
	ConcurrencyMin     = 1
	ConcurrencyDefault = 4
	ConcurrencyMax     = 10
	Concurrency3a      = 3
)

// ClampConcurrency forces a configured cap into [ConcurrencyMin, ConcurrencyMax],
// treating 0 as "unset" and yielding the default. A cap read from config is user
// input and must not be able to express "unlimited".
func ClampConcurrency(n int) int {
	switch {
	case n <= 0:
		return ConcurrencyDefault
	case n < ConcurrencyMin:
		return ConcurrencyMin
	case n > ConcurrencyMax:
		return ConcurrencyMax
	default:
		return n
	}
}

// ActiveChildren is the STATE-DERIVED view of how many tasks are holding a
// child. It is not the enforced cap.
//
// §6.6 counts "running and blocked CHILDREN, not tasks", and the enforced count
// is therefore Worktrees.LiveChildren — live `sessions` rows whose cwd is under
// the run's worktree root, i.e. real claudes that exist. This function answers
// the same question from task states, which can be stale in the one direction
// that matters (a dead child leaves a `running` row until something notices),
// so it is offered for rendering a task list and nothing here gates on it.
// SpawnPreview.Running carries the enforced number; a UI showing a different
// one to the human at the gate is showing them a number Loom will not honour.
//
// Reaching the cap does not stop the scheduler. Ready tasks still queue and
// still show their approve action, greyed with "cap reached (n/n)" — hiding the
// action would make a capped run look like a stalled one.
func ActiveChildren(states map[string]TaskState) int {
	n := 0
	for _, st := range states {
		// The states are ENUMERATED here rather than routed through
		// TaskState.HoldsAChild, for the same reason Ready enumerates its
		// candidates: a cap is a safety property, and delegating it to a
		// predicate elsewhere means a state added over there silently stops
		// being counted over here. The compiler will not warn; a reader of this
		// switch will. `spawning` counts because the launch may already have
		// produced a real claude — the row is written after the process exists
		// (§13.3), so not counting it lets the cap be exceeded by exactly the
		// number of launches in flight.
		switch st {
		case StateSpawning, StateRunning, StateBlocked, StateChecking,
			// `integrating` and `mergeable` hold a child for the reasons
			// HoldsAChild spells out: §10.2 runs against the INTEGRATION
			// worktree so the task's own child is idle at its prompt (and §10.3
			// seeds it, which needs it alive), and §10.4 does not remove the
			// worktree until the human merges. Omitting them here while
			// HoldsAChild counts them is the precise drift this switch's
			// duplication is supposed to make a reader notice.
			StateIntegrating, StateMergeable:
			n++
		}
	}
	return n
}

// SpawnPreview is exactly what §5.1's gate renders, and it exists as a struct so
// the TUI and the GUI cannot show the human different things before the same
// decision.
//
// Approve-to-spawn is a BUDGET AND CONSENT gate, not a correctness gate. At
// spawn the human is approving a plan they mostly already read; the correctness
// gate is at merge, where they approve a diff a machine wrote into a tree they
// own. Conflating the two is how you end up asking eleven questions and getting
// safety from none of them.
//
// Batch-approve IS allowed here ("approve all 3 ready tasks"), because the
// decision is cheap and reversible: a bad spawn is undone by discarding a
// worktree, and nothing is lost but tokens — whose count is on the screen. The
// merge gate is the one that is never batchable.
type SpawnPreview struct {
	TaskID   string
	Title    string
	Repo     string
	Branch   string
	Worktree string
	Base     string
	// Brief is the FULL assembled brief, scrollable, exactly as the child will
	// receive it. Not a summary: the human is consenting to what is said, and a
	// summary of a brief is a different document.
	Brief string
	// CheckArgv is rendered VERBATIM. It is arbitrary code from an
	// agent-authored file, and this rendering plus the human's approval is the
	// review — there is no sandbox and this package does not claim one.
	CheckArgv []string
	Model     string
	Mode      string
	// ModeRisky is true for bypassPermissions, which §5.1 renders in red with
	// the task id. Legal, flagged, never silent.
	ModeRisky bool
	// SeedFiles are the gitignored files about to be copied into the worktree
	// (§6.5). Listed because the human is being shown that .env is about to be
	// handed to an agent.
	SeedFiles []string
	// SeedRefused are the entries §6.5 will NOT copy, and why. Rendered beside
	// SeedFiles rather than dropped: a refused .env is a check that will fail
	// for a reason that has nothing to do with the child's work (§6.4, and 3a's
	// M4), and the human can only fix it before the spawn. Refusals are
	// per-file and never block — one bad entry must not cost the child the rest.
	SeedRefused []SeedFileError
	// RepoDirty is §6.2's disclosure: children branch from committed HEAD, so
	// uncommitted work in the user's own tree is invisible to every child and
	// will conflict later.
	RepoDirty bool
	// Running / Cap are §6.6's counter, and CapReached greys the action rather
	// than removing it.
	Running    int
	Cap        int
	CapReached bool
	// Warnings are the manifest's §4.4 rule 10 findings relevant to this task,
	// carried to the gate rather than only to the load screen — the load screen
	// is not where the human is deciding.
	Warnings []string
}

// Spawner owns §5.1's gate and §13.3's launch sequence.
//
// Launcher is session.Launcher unchanged: a child is a real claude in tmux
// launched exactly like every other Loom session. Nothing about a child is
// special to the rest of Loom — it has a store row, a transcript, a memory index
// entry and a diff. That is deliberate, and it is why the attribution override
// in attribute.go is needed rather than a parallel session type.
type Spawner struct {
	Store     *store.Store
	Launcher  Launcher
	Worktrees *Worktrees
	// §6.6's cap is deliberately NOT a field here. It lives on Worktrees, which
	// is what enforces it inside Create, and one knob that the enforcing code
	// reads beats two knobs that can disagree — a cap the gate renders and the
	// creator ignores is worse than no cap, because it is a number the human
	// trusted.
	//
	// Width/Height are the launch geometry. 0 means the GUI's launch size
	// (cmd/loom-gui/workflow.go's launchCols/launchRows), which is what every
	// other non-interactive launch in Loom uses; a 0×0 tmux window is not a
	// smaller window, it is an unusable one.
	Width, Height int
	// Now is a seam for tests, not configuration. Production passes nothing.
	Now func() time.Time
}

// Launcher is the slice of session.Launcher this package uses. An interface so
// spawn's ORDER — claim, create, launch, bind — can be tested without tmux;
// *session.Launcher satisfies it, and nothing here reimplements launching.
// (Same shape and same reason as internal/orchestrator's.)
type Launcher interface {
	Launch(r session.Recipe, w, h int, now time.Time) (string, error)
}

// ErrTaskNotApproved now lives beside state.go's other sentinels.

func (s *Spawner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Spawner) geometry() (int, int) {
	w, h := s.Width, s.Height
	if w <= 0 {
		w = 120
	}
	if h <= 0 {
		h = 32
	}
	return w, h
}

// effectiveCap and running both defer to Worktrees, which is the code that
// enforces the cap when it refuses a Create. The gate must render the same
// number the creator will apply, so it asks the same object rather than
// deriving a second one from task states — see ActiveChildren.
func (s *Spawner) effectiveCap() int { return s.Worktrees.cap() }

// taskStates reads the run's task states for ActiveChildren. Used ONLY to
// explain a refusal after the fact — the cap itself is decided inside
// ClaimTaskSpawnCAS, and re-deriving it here would put the number back in the
// racy place it was just taken out of.
func (s *Spawner) taskStates(runID int64) (map[string]TaskState, error) {
	rows, err := s.Store.ListDelegationTasks(runID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]TaskState, len(rows))
	for _, r := range rows {
		out[r.TaskID] = TaskState(r.State)
	}
	return out, nil
}

func (s *Spawner) running(runSlug string) (int, error) {
	return s.Worktrees.LiveChildren(runSlug)
}

// plan derives everything about a task's spawn that is deterministic from
// (run, task) — the worktree, the meta dir, the branch and the base.
//
// It is a Created and not a third struct on purpose: the gate renders the brief
// assembled from THIS value, and so does the file handed to the child, so the
// brief the human approved is byte-identical to the brief the child reads. A
// separate "planned" shape is how those two drift.
//
// Dir and MetaDir go through physicalPath — the same resolution Create applies
// — so the planned value equals the created one. That is not cosmetic twice
// over: the brief's "write only inside this worktree" names the child's actual
// getcwd(), and the worktree string this claim records is byte-identical to
// what Launcher.Launch writes into sessions.cwd, which is what §13.3's recovery
// and §6.2's occupancy refusal compare on. /var → /private/var on macOS makes
// the difference routine rather than exotic.
func (s *Spawner) plan(run store.DelegationRun, t Task) Created {
	l := s.Worktrees.Layout
	return Created{
		Dir:     physicalPath(l.Dir(run.Slug, t.Repo, t.ID)),
		MetaDir: physicalPath(l.MetaDir(run.Slug, t.Repo, t.ID)),
		Branch:  BranchName(run.Slug, t.ID),
		Base:    baseSHA(run, t.Repo),
	}
}

// baseSHA reads the run's pinned base for a repo (§6.2 step 1: pinned per repo
// on the RUN, not per task, so every child of a run branches from the same
// commit). A malformed column yields "", which `git worktree add` will refuse
// loudly — better than inventing HEAD, which would silently branch children
// from different commits and make §10's integration non-deterministic.
func baseSHA(run store.DelegationRun, repoLabel string) string {
	var bases map[string]string
	if err := json.Unmarshal([]byte(run.BaseSHAs), &bases); err != nil {
		return ""
	}
	return bases[repoLabel]
}

// Preview assembles the gate for one ready task. It performs no writes and
// creates no worktree: the human may look and walk away, and looking must cost
// nothing.
func (s *Spawner) Preview(run store.DelegationRun, m Manifest, t Task) (SpawnPreview, error) {
	if s.Worktrees == nil || s.Store == nil {
		return SpawnPreview{}, errors.New("delegate: Spawner needs a Store and a Worktrees")
	}
	planned := s.plan(run, t)
	running, err := s.running(run.Slug)
	if err != nil {
		return SpawnPreview{}, err
	}
	cap := s.effectiveCap()

	mode := effective(t.Mode, m.Defaults.Mode)
	// The gate shows the copies AND the refusals, judged by the same predicate
	// Create will apply — not the manifest's raw list, which promises copies
	// that will not happen.
	seeded, seedRefused := SeedPlan(m.RepoPaths[t.Repo], m.Repos[t.Repo].SeedFiles)
	return SpawnPreview{
		TaskID:   t.ID,
		Title:    t.Title,
		Repo:     t.Repo,
		Branch:   planned.Branch,
		Worktree: planned.Dir,
		Base:     planned.Base,
		Brief:    Brief(run, m, t, planned, AddDirs(planned)),
		// A copy, not the manifest's slice: this value is handed to a frontend,
		// and a renderer that sorted it in place would edit the snapshot every
		// later spawn reads from.
		CheckArgv:   append([]string(nil), t.Check.Cmd...),
		Model:       effective(t.Model, m.Defaults.Model),
		Mode:        mode,
		ModeRisky:   mode == "bypassPermissions",
		SeedFiles:   seeded,
		SeedRefused: seedRefused,
		RepoDirty:   Dirty(m.RepoPaths[t.Repo]),
		Running:     running,
		Cap:         cap,
		// Greyed, never removed: hiding the action would make a capped run look
		// like a stalled one.
		CapReached: running >= cap,
		Warnings:   warningsFor(m, t.ID),
	}, nil
}

// warningsFor carries the manifest's task-scoped findings to the gate. The load
// screen is not where the human is deciding, and a warning nobody sees at the
// moment of decision is a warning that was not raised. Manifest-level warnings
// (Task == "") are included too: an unreachable leaf or a cmd[0] that is not on
// PATH bears on this decision as much as a task-scoped one.
func warningsFor(m Manifest, taskID string) []string {
	var out []string
	for _, w := range m.Warnings {
		if w.Task == "" || w.Task == taskID {
			out = append(out, w.Text)
		}
	}
	return out
}

// Approve moves ready → approved under CAS. It is separate from Spawn because
// approval is the human's act and spawning is Loom's, and collapsing them would
// make a failed worktree creation look like a withdrawn approval.
//
// Batch approval is the caller looping over this; there is deliberately no
// ApproveAll, because a batch that half-fails needs per-task reporting and a
// single error return cannot give it.
func (s *Spawner) Approve(runID int64, taskID string) (claimed bool, err error) {
	return s.Store.AdvanceTaskCAS(runID, taskID, string(StateReady), string(StateApproved), s.now().Unix())
}

// Spawn runs §13.3's sequence for an approved task:
//
//  1. CAS approved → spawning via store.ClaimTaskSpawnCAS, which records the
//     deterministic worktree, branch and base in the SAME statement. The claim
//     precedes every side effect, so pressing Approve twice — or two Loom
//     instances pressing it once each — produces exactly one spawn;
//  2. create/verify the worktree, including §6.2 step 3's occupancy refusal,
//     write the brief, copy seed files, bootstrap;
//  3. Launch, SetTags("dlg:<slug>#<task>"), SetSessionDelegation("<runID>:<taskID>");
//  4. CAS spawning → running via store.BindTaskSessionCAS, writing session_name.
//
// The stranded-launch window is NARROWED here, not closed, and revision 1's
// claim that delegation strictly improves on workflow.Advance is retracted:
// Launcher.Launch mints its own session id and the task↔session link is written
// after the process exists, so the window has the same shape. What is genuinely
// better is the recovery EVIDENCE — Launch upserts the session row with
// Cwd = physicalDir(<deterministic worktree>) in the same call that creates the
// tmux session, which is the one identity that exists the instant the child
// does. Recovery therefore keys on cwd (Worktrees.Occupant), never on a tag.
//
// A claimed=false from step 4 is the residual hole §13.3 discloses: a concurrent
// abandon can move the task out of spawning while the launch is in flight. The
// child is real and is running, so this returns the session name AND an error
// rather than swallowing either.
func (s *Spawner) Spawn(run store.DelegationRun, m Manifest, t Task) (sessionName string, err error) {
	// The zero BasePlan is "branch from the run's pinned base with no producers
	// to merge", which is every 3a run and every leaf task. It is NOT a special
	// case inside SpawnWith — a zero plan simply contributes nothing — so there
	// is one spawn sequence rather than two that drift.
	return s.SpawnWith(run, m, t, BasePlan{})
}

// SpawnWith is Spawn with §9.2's computed base. run.go's Approve calls it with
// PlanBase's result so the worktree is branched from the run base with every
// same-repo producer already merged in, and the two snapshot columns §12.3.3 and
// §10.5 depend on are captured from that plan.
//
// A separate entry point rather than a changed signature on Spawn: Spawn's
// sequence is the one proven against §13.3's races, and the safest way to add
// the plan was to add the parameter where the caller that has one can pass it
// while every existing caller keeps the sequence it was tested with.
func (s *Spawner) SpawnWith(run store.DelegationRun, m Manifest, t Task, plan BasePlan) (sessionName string, err error) {
	if s.Store == nil || s.Worktrees == nil || s.Launcher == nil {
		return "", errors.New("delegate: Spawner needs a Store, a Worktrees and a Launcher")
	}
	now := s.now()

	capN := s.effectiveCap()

	// The cap is checked BEFORE the claim: a task refused for capacity must stay
	// `approved` and keep its (greyed) action, not be dragged into `spawning`
	// and then rolled back.
	//
	// This check is NOT the enforcement, and revision 1's claim that it was is
	// retracted. It counts live `sessions` rows, and a session row is written
	// inside Launcher.Launch — several steps after the claim below — so every
	// concurrent spawn already past this point is invisible here. A probe ran
	// five approvals against a cap of three and launched five children. The
	// enforcement is now the capacity predicate inside ClaimTaskSpawnCAS, which
	// is evaluated in the same SQLite statement as the state move and therefore
	// cannot be raced.
	//
	// It is kept because it is BETTER EVIDENCE in the opposite direction, which
	// the state count cannot see: a live child whose task row was lost or
	// rewritten still holds a worktree, real quota and §6.4's shared ports. Two
	// counts, two failure directions, both refusing.
	running, err := s.running(run.Slug)
	if err != nil {
		return "", err
	}
	if running >= capN {
		return "", fmt.Errorf("%w (%d/%d)", ErrCapReached, running, capN)
	}

	planned := s.plan(run, t)
	// §9.2's plan overrides the run's pinned base only when it HAS one. A plan
	// computed from a run whose base_shas column is malformed carries "", and
	// letting that overwrite the value the row already had would replace a loud
	// `git worktree add` refusal with a child branched from somewhere else.
	if plan.Base != "" {
		planned.Base = plan.Base
	}

	// Step 1 (§13.3): CAS approved→spawning, recording the deterministic
	// worktree, branch and base in the SAME statement. THE CLAIM PRECEDES EVERY
	// SIDE EFFECT. That ordering is what makes a double spawn structurally
	// impossible rather than merely unlikely: pressing Approve twice, or two
	// Loom instances against one DB pressing it once each, contend on one row
	// and exactly one of them wins. The loser never creates a worktree and never
	// reaches Launch.
	// capN travels INTO the claim so §6.6 is decided by the same statement that
	// moves the state. A refusal is therefore indistinguishable here from a lost
	// approval race, and the error says both rather than guessing: the row is
	// untouched either way and the remedy — look at the run — is the same.
	claimed, err := s.Store.ClaimTaskSpawnCAS(run.ID, t.ID, planned.Dir, planned.Branch, planned.Base,
		EncodeProducers(plan.Merge), capN, now.Unix())
	if err != nil {
		return "", err
	}
	if !claimed {
		// Re-read the state count to say WHICH of the two it was. This is a
		// diagnosis after the fact, not a second gate: the claim already
		// refused, and nothing downstream depends on which sentence is
		// returned — only the human reading it does.
		if states, serr := s.taskStates(run.ID); serr == nil && ActiveChildren(states) >= capN {
			return "", fmt.Errorf("%w (%d/%d)", ErrCapReached, ActiveChildren(states), capN)
		}
		return "", fmt.Errorf("%w: run %d task %q", ErrTaskNotApproved, run.ID, t.ID)
	}

	// Step 2: the worktree, the brief, the seed files, the bootstrap. The brief
	// is assembled from `planned`, which is the same value Preview rendered, so
	// the human approved the bytes the child will read.
	created, err := s.Worktrees.Create(Request{
		RunSlug:   run.Slug,
		TaskID:    t.ID,
		RepoLabel: t.Repo,
		RepoPath:  m.RepoPaths[t.Repo],
		Base:      planned.Base,
		Setup:     m.Repos[t.Repo],
		Brief:     Brief(run, m, t, planned, addDirsFor(planned, plan)),
		// Merged INSIDE Create, between `worktree add` and bootstrap — see
		// Request.Merge for why the caller must not do it afterwards.
		Merge: plan.Merge,
	})
	if err != nil {
		// Nothing was launched — Create's own precondition (§6.2 step 3) and
		// its bootstrap gate both run before any child exists — so releasing the
		// claim back to `approved` restores exactly the state the human pressed
		// the button in, with the error rendered beside it. The release is
		// itself a CAS on `spawning`, so a concurrent abandon wins and this
		// becomes a no-op rather than a resurrection.
		if _, relErr := s.Store.AdvanceTaskCAS(run.ID, t.ID, string(StateSpawning), string(StateApproved), s.now().Unix()); relErr != nil {
			return "", fmt.Errorf("delegate: task %q: worktree: %w (and releasing the spawn claim failed: %v)", t.ID, err, relErr)
		}
		return "", fmt.Errorf("delegate: task %q: worktree: %w", t.ID, err)
	}

	// Step 2b: the two baselines, taken BEFORE the launch. Before, because both
	// exist to answer "what did this look like when the child started", and one
	// taken after the child exists has already lost the thing it is.
	//
	// Both failures degrade rather than abort: the child's worktree is built and
	// refusing to launch over a missing tripwire trades a real hour of work for a
	// detector. The absence is not silent either way — §12.3.3's comparator
	// reports NoBaseline and §10.5's Preview renders "no baseline recorded".
	s.recordBaselines(run, m, t, plan, now)

	// Step 3: launch. From here the child is REAL, so every subsequent failure
	// returns the session name alongside its error — a caller that saw only an
	// error would report "no child" while a claude burns quota in a worktree.
	w, h := s.geometry()
	name, err := s.Launcher.Launch(session.Recipe{
		ProjectLabel: t.Repo,
		Cwd:          created.Dir,
		Model:        effective(t.Model, m.Defaults.Model),
		Mode:         effective(t.Mode, m.Defaults.Mode),
		Seed:         SeedPointer(filepath.Join(created.MetaDir, briefFile)),
		// §9.2's cross-repo half: the producers' integration worktrees, granted
		// read-and-write by --add-dir (spike-disclosed) and checked pre-merge by
		// §12.3.3. The child cannot see what it declares it needs without them.
		AddDirs: addDirsFor(created, plan),
	}, w, h, now)
	if err != nil {
		// The task stays in `spawning` deliberately. §13.3's recovery resolves
		// by cwd against the deterministic worktree, and a launch that failed
		// halfway (tmux made, row not) is exactly what it is there to adjudicate
		// — moving the row back to `approved` here is what would put a second
		// claude in this worktree.
		return "", fmt.Errorf("delegate: task %q: launch: %w", t.ID, err)
	}

	tagErr := s.Store.SetTags(name, DelegationTag(run.Slug, t.ID))
	// The delegation link is what §14.1's attribution override keys on. A child
	// without it is not merely untagged: it attributes to Ungrouped and vanishes
	// the moment anything is hidden. So the failure is surfaced, never swallowed.
	delErr := s.Store.SetSessionDelegation(name, FormatDelegation(run.ID, t.ID))

	// Step 4: CAS spawning→running, writing session_name.
	bound, bindErr := s.Store.BindTaskSessionCAS(run.ID, t.ID, name, s.now().Unix())
	switch {
	case delErr != nil:
		return name, fmt.Errorf("delegate: task %q: session %s launched but not linked to its run (it will render as Ungrouped and hide when anything is hidden): %w", t.ID, name, delErr)
	case tagErr != nil:
		return name, fmt.Errorf("delegate: task %q: session %s launched but not tagged: %w", t.ID, name, tagErr)
	case bindErr != nil:
		return name, fmt.Errorf("delegate: task %q: session %s launched but the task row was not bound: %w", t.ID, name, bindErr)
	case !bound:
		// §13.3's disclosed residual hole, not a closed one: a concurrent
		// abandon moved the task out of `spawning` while the launch was in
		// flight. The child is real and running, so it is named rather than
		// silently dropped; abandon's own sweep by cwd is what collects it.
		return name, fmt.Errorf("delegate: task %q: session %s launched but the task left `spawning` first (abandoned concurrently?) — the child is live", t.ID, name)
	}
	return name, nil
}

// Brief assembles §7's brief.md. Loom-rendered from the manifest, in this order:
//
//  1. Identity — run, task id, repo, branch, worktree path, base commit.
//  2. Authorization — the manifest's text VERBATIM (mandatory and non-empty,
//     §4.4 rule 5), plus Loom's appended invariants: write only inside this
//     worktree; these add-dirs are readable and must not be written; do not
//     merge/rebase/push/checkout or touch another worktree; do not modify these
//     paths, which belong to sibling tasks in this repo; do not spawn subagents
//     that write outside this worktree. Loom APPENDS, never replaces —
//     inventing the task-specific half is exactly what the loader refuses to let
//     the author skip.
//  3. The task — the manifest's brief, verbatim.
//  4. Artifacts to publish — exact paths, and the rule: an artifact is published
//     when it is COMMITTED on this branch; uncommitted work does not exist.
//  5. Done — "You do not declare done. Loom runs <check argv> against your
//     committed work. Commit when you believe it will pass; Loom will run it and
//     tell you. Do not report completion in prose." The argv is given verbatim
//     so the child can run it itself while working, which is encouraged.
//  6. The STOP protocol (§11.1). 3a has no rendezvous machinery, so the section
//     tells the child to stop and say nothing further rather than to work around
//     a block; the human attaches and types. Telling a child to work around a
//     block is how scope overreach becomes silent.
func Brief(run store.DelegationRun, m Manifest, t Task, c Created, addDirs []string) string {
	var b strings.Builder
	title := t.Title
	if title == "" {
		title = t.ID
	}
	fmt.Fprintf(&b, "# %s — %s\n\n", t.ID, title)
	b.WriteString("This file was written by Loom. It is your complete brief; nothing else was said to you.\n\n")

	b.WriteString("## 1. Identity\n\n")
	fmt.Fprintf(&b, "- run: %s (slug `%s`, id %d)\n", run.Name, run.Slug, run.ID)
	fmt.Fprintf(&b, "- task: `%s`\n", t.ID)
	fmt.Fprintf(&b, "- repo: `%s`\n", t.Repo)
	fmt.Fprintf(&b, "- branch: `%s`\n", c.Branch)
	fmt.Fprintf(&b, "- worktree: `%s`\n", c.Dir)
	fmt.Fprintf(&b, "- base commit: `%s`\n\n", c.Base)

	// §7 section 2, BINDING and mandatory. The manifest's text comes FIRST and
	// verbatim; Loom appends and never replaces. Slice 1 §11 measured that
	// removing explicit authorization-scope text raises scope overreach, which
	// is why §4.4 rule 5 makes an empty one a load error and why nothing here
	// invents a substitute.
	b.WriteString("## 2. Authorization\n\n")
	b.WriteString(strings.TrimSpace(t.Authorization))
	b.WriteString("\n\nLoom's invariants, which apply to every delegated task and are not negotiable:\n\n")
	fmt.Fprintf(&b, "- Write only inside this worktree: `%s`.\n", c.Dir)
	if len(addDirs) == 0 {
		b.WriteString("- You have been granted no additional directories.\n")
	} else {
		fmt.Fprintf(&b, "- These additional directories are granted for the files named below and MUST NOT otherwise be written: %s.\n", backquoteList(addDirs))
	}
	b.WriteString("- Do not `git merge`, `git rebase`, `git push`, `git checkout` another branch, or touch another worktree.\n")
	if siblings := siblingPaths(m, t); len(siblings) > 0 {
		fmt.Fprintf(&b, "- Do not modify these paths, which belong to sibling tasks in this repo: %s.\n", backquoteList(siblings))
	} else {
		b.WriteString("- No sibling task in this repo has declared paths of its own.\n")
	}
	b.WriteString("- Do not spawn subagents that write outside this worktree.\n\n")

	b.WriteString("## 3. The task\n\n")
	b.WriteString(strings.TrimSpace(t.Brief))
	b.WriteString("\n\n")

	b.WriteString("## 4. Artifacts to publish\n\n")
	if len(t.Produces) == 0 {
		b.WriteString("This task declares no artifacts. Your work is still judged by the check in section 5,\nagainst your COMMITTED tree.\n\n")
	} else {
		for _, a := range t.Produces {
			fmt.Fprintf(&b, "- `%s` → `%s`\n", a.ID, a.Path)
		}
		b.WriteString("\nAn artifact is published when it is COMMITTED on this branch. Uncommitted work does not\nexist: Loom will not even run the check until every path above is tracked and committed.\n\n")
	}

	b.WriteString("## 5. Done\n\n")
	fmt.Fprintf(&b, "You do not declare done. Loom runs `%s` against your committed work.\n", strings.Join(t.Check.Cmd, " "))
	b.WriteString("Commit when you believe it will pass; Loom will run it and tell you.\n")
	b.WriteString("Do not report completion in prose — there is no message you can send that means done.\n")
	b.WriteString("You are encouraged to run that same command yourself while you work.\n\n")

	// §11.1's protocol in full. 3a's version of this comment said there was no
	// rendezvous machinery and a human would attach by hand; rendezvous.go now
	// exists, so the brief states what actually happens rather than under-
	// promising. The distinction is kept per kind and not smoothed away: only
	// needs-artifact resumes on its own, and telling a child that every block
	// clears automatically is a different lie from the one just removed.
	b.WriteString("## 6. If you cannot proceed — the STOP protocol\n\n")
	b.WriteString("Do not work around a block. Do not modify paths outside your authorization to unblock\nyourself. Do not exit.\n\n")
	fmt.Fprintf(&b, "Write `%s` with this shape:\n\n", filepath.Join(c.MetaDir, blockFile))
	b.WriteString("```json\n")
	b.WriteString("{ \"block\": 1,\n")
	fmt.Fprintf(&b, "  \"run\": %q,\n  \"task\": %q,\n", run.Slug, t.ID)
	b.WriteString("  \"at\": \"<RFC3339 timestamp>\",\n")
	b.WriteString("  \"kind\": \"needs-artifact | needs-decision | needs-scope | blocked-external\",\n")
	// `need` and `paths` are printed even though they are per-kind, because the
	// template is the ONLY specification the child ever sees and both fields are
	// load-bearing rather than decorative. A needs-artifact block with no
	// `need.artifact` can never satisfy Unblocked, so the park is PERMANENT; a
	// needs-scope block with no `paths` yields a scope amendment that ApplyScope
	// refuses as "grants no paths", so the widening can never be granted. In
	// both cases the child stopped correctly and visibly and the run still cannot
	// resume — the exact silent outcome §11.2 forbids, caused by the brief and
	// not by the agent.
	b.WriteString("  \"need\": { \"artifact\": \"<artifact id>\", \"from\": \"<task id you believe owns it>\" },\n")
	b.WriteString("  \"paths\": [\"<glob you need authorization for>\"],\n")
	b.WriteString("  \"summary\": \"one line\",\n")
	b.WriteString("  \"detail\": \"what you were doing and what stopped you\",\n")
	b.WriteString("  \"resume_when\": \"the condition that would unblock you\" }\n")
	b.WriteString("```\n\n")
	b.WriteString("`need` is REQUIRED for `needs-artifact` — without `need.artifact` nothing can detect\nthat your block cleared, and you will wait forever. `paths` is REQUIRED for\n`needs-scope` — a widening that names nothing cannot be granted. Omit each for the\nother kinds. If you do not know the producing task, give `need.artifact` alone.\n\n")
	// SUPERSEDED, deliberately rewritten: the 3a text promised only that a human
	// would read the file and reply. §11.4 now materializes the artifact and
	// re-seeds the child automatically for needs-artifact, and telling the child
	// to expect a human is how it decides, after a compaction, that nobody came
	// and it should proceed on its own — which is the scope overreach the STOP
	// protocol exists to prevent.
	b.WriteString("Then STOP at your prompt and say nothing further. An idle session costs nothing and\nkeeps your context. You will be replied to here — for `needs-artifact` Loom does this\nautomatically once the artifact is published; the other kinds wait for a human. Either\nway the reply arrives in this session, so do not act until it does.\n")
	b.WriteString("A `needs-scope` block is the correct and encouraged response to discovering that this\ntask's boundary was drawn wrong.\n")
	return b.String()
}

// siblingPaths is the declared-paths union of the OTHER tasks in this task's
// repo — §7's "do not modify these paths, which belong to sibling tasks".
//
// It is authorization text, not the isolation mechanism. Slice 1 §11's ablation
// measured instruction-level file ownership BELOW the single-agent baseline
// (55.5% vs 57.2%) where worktrees scored 63.3%; what keeps two children apart
// is the worktree, and this sentence only makes an overreach legible when §12.3
// later reports one. Sorted and de-duplicated so two spawns of the same task
// produce byte-identical briefs.
func siblingPaths(m Manifest, t Task) []string {
	seen := map[string]bool{}
	for _, other := range m.Tasks {
		if other.ID == t.ID || other.Repo != t.Repo {
			continue
		}
		for _, p := range other.Paths {
			seen[p] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func backquoteList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "`" + s + "`"
	}
	return strings.Join(quoted, ", ")
}

// SeedPointer is what the child is actually seeded with: a POINTER to the brief
// file, not the brief.
//
//	Read <abs>/<task-id>.meta/brief.md — it is your complete task brief.
//	Follow it exactly, including the STOP protocol.
//
// Not a stylistic choice. send-keys has a measured argv ceiling of ~16.3KB and a
// real brief with authorization text, artifact paths and a block protocol will
// exceed it. A file also survives context compaction, is re-readable on demand,
// and — because it lives OUTSIDE the worktree — cannot be committed as an
// artifact and cannot appear in the child's own diff.
func SeedPointer(briefPath string) string {
	return "Read " + briefPath + " — it is your complete task brief. Follow it exactly, including the STOP protocol."
}

// AddDirs is what a 3a child is granted beyond its worktree: exactly its own
// .meta directory, and nothing else.
//
// The meta dir is granted because the child writes block.json there, and per the
// add-dir spike an --add-dir grants read AND write silently with no second trust
// prompt — which is what is wanted for this one directory and is why ~/.loom
// itself is never granted. loom.db is not in a child's authorization scope.
// (§9.2's cross-repo add-dirs are part of the deferred scheduler and are not
// produced here.)
func AddDirs(c Created) []string {
	if c.MetaDir == "" {
		return nil
	}
	return []string{c.MetaDir}
}

// addDirsFor is AddDirs plus §9.2's cross-repo grants: the child's own meta dir,
// then the integration worktree of every repo it needs an artifact out of.
//
// The meta dir stays FIRST and the plan's dirs are de-duplicated against it, so
// the list a brief renders and the list Launch passes are the same list in the
// same order. A brief that names a different set from the one actually granted
// is worse than no list at all — the authorization text is the only thing
// constraining a grant that cannot be technically narrowed.
func addDirsFor(c Created, plan BasePlan) []string {
	out := AddDirs(c)
	seen := map[string]bool{}
	for _, d := range out {
		seen[d] = true
	}
	for _, d := range plan.AddDirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// recordBaselines writes the two spawn-time columns §12.3.3 and §10.5 compare
// against. Errors are DEGRADED, not returned: see the call site.
func (s *Spawner) recordBaselines(run store.DelegationRun, m Manifest, t Task, plan BasePlan, now time.Time) {
	// §12.3.3's tripwire: the dirty-file fingerprint of every in-scope primary
	// work tree and every add-dir'd integration worktree. This is the ONLY thing
	// that can see a child writing outside its worktree by absolute path, because
	// --add-dir's grant covers write and cannot be revoked (spike).
	if snap := EncodeSnapshot(TakeSnapshot(SnapshotDirs(m, plan))); snap != "" {
		_ = s.Store.SetTaskSpawnSnapshot(run.ID, t.ID, snap, now.Unix())
	}

	// §10.5's stale-contract baseline: the fingerprint and commit of every
	// artifact this task NEEDS, as they stand right now. A COPY and not a
	// reference — delegation_artifacts holds only the latest publication, so
	// without this the alarm would compare the current value with itself and
	// never fire.
	if len(t.Needs) == 0 {
		return
	}
	arts, err := s.Store.ListDelegationArtifacts(run.ID)
	if err != nil {
		return
	}
	byID := make(map[string]store.DelegationArtifact, len(arts))
	for _, a := range arts {
		byID[a.ArtifactID] = a
	}
	base := map[string]NeedsBaseline{}
	for _, id := range t.Needs {
		a, ok := byID[id]
		if !ok || a.Fingerprint == "" {
			// Nothing published yet. Recording an empty fingerprint would be
			// indistinguishable from a recorded one at compare time, and
			// needsWithoutBaseline exists precisely so "we cannot tell" is a
			// rendered warning rather than a silent pass.
			continue
		}
		base[id] = NeedsBaseline{Fingerprint: a.Fingerprint, Commit: a.CommitSHA}
	}
	_ = s.Store.SetTaskNeedsSnapshot(run.ID, t.ID, EncodeNeedsBaselines(base), now.Unix())
}

// DelegationTag is the sessions.tags value, matching the `wf:` convention:
// "dlg:<run-slug>#<task-id>". It is for human-facing dashboard filtering ONLY.
// It is a DB column and is invisible in tmux, so nothing may recover state from
// it — see Worktrees.Occupant.
func DelegationTag(runSlug, taskID string) string { return "dlg:" + runSlug + "#" + taskID }
