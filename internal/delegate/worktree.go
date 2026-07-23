package delegate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/gitdiff"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
)

// Layout is §6.2's deterministic naming, in one place. Determinism from
// (run, task) is what makes crash recovery exact: a stranded worktree on disk
// names its run and its task and nothing else, and recovery has to be able to
// get back from the path to the row.
//
//	branch:   loom/<run-slug>/<task-id>
//	worktree: <loom-dir>/worktrees/<run-slug>/<repo-label>/<task-id>
//	meta dir: <loom-dir>/worktrees/<run-slug>/<repo-label>/<task-id>.meta/
//
// Under ~/.loom and NOT in the workspace, deliberately. A worktree inside the
// repo pollutes git status, .gitignore, editor indexers and every glob in every
// check. A worktree beside the repo in $LOOM_WORKSPACE gets picked up by
// registry.Discover as N new single-repo projects.
type Layout struct {
	// Root is <loom-dir>/worktrees. Injected rather than derived from
	// os.UserHomeDir so every test builds its scratch trees under t.TempDir()
	// and nothing in this package can touch a real repo by accident.
	Root string
}

// NewLayout builds the layout under a config.Config.LoomDir.
func NewLayout(loomDir string) Layout { return Layout{Root: filepath.Join(loomDir, "worktrees")} }

// RunRoot is every worktree of one run, and it exists because §6.6's cap is a
// per-run count: the run's live children are exactly the live sessions whose
// cwd lies under this directory. See LiveChildren.
func (l Layout) RunRoot(runSlug string) string { return filepath.Join(l.Root, runSlug) }

// Dir is the worktree path for one task. The <run-slug> is <manifest-name>-<id>
// (store.InsertDelegationRun owns it), so two runs of the same manifest never
// collide.
func (l Layout) Dir(runSlug, repoLabel, taskID string) string {
	return filepath.Join(l.Root, runSlug, repoLabel, taskID)
}

// MetaDir is the `<task-id>.meta/` directory BESIDE the worktree — never inside
// it. It holds brief.md and block.json.
//
// Revision 1 put these in <worktree>/.loom/ and excluded them via
// <worktree>/.git/info/exclude. That is impossible: in a linked worktree
// <worktree>/.git is a FILE containing "gitdir: …", so the path is not a
// directory and the append fails outright. Verified experimentally, along with
// the reason the obvious repair is worse — info/ is a COMMON-DIR path, so
// writing .git/worktrees/<id>/info/exclude is not honoured while writing the
// main repo's .git/info/exclude is, i.e. the only working form writes into the
// user's own repository, affects every other worktree of it, and accumulates a
// line per task per run forever.
//
// Keeping Loom's files outside the tree removes the exclusion problem entirely,
// removes any chance of a child committing its own brief into the artifact set,
// and needs no git feature at all.
//
// The name is the worktree path plus a suffix rather than a differently-named
// sibling, so the two are derivable from each other by a string operation in
// either direction: recovery walking the disk finds "<x>" and "<x>.meta" and
// needs no third rule to pair them.
func (l Layout) MetaDir(runSlug, repoLabel, taskID string) string {
	return l.Dir(runSlug, repoLabel, taskID) + ".meta"
}

// BriefPath and BlockPath are the two files that live in the meta dir. §7's seed
// is a POINTER to BriefPath, not the brief itself: send-keys has a measured argv
// ceiling of ~16.3KB and a real brief will exceed it, and a file also survives
// context compaction and is re-readable by the child on demand.
func (l Layout) BriefPath(runSlug, repoLabel, taskID string) string {
	return filepath.Join(l.MetaDir(runSlug, repoLabel, taskID), briefFile)
}

func (l Layout) BlockPath(runSlug, repoLabel, taskID string) string {
	return filepath.Join(l.MetaDir(runSlug, repoLabel, taskID), blockFile)
}

const (
	briefFile = "brief.md"
	blockFile = "block.json"
)

// BranchName is loom/<run-slug>/<task-id>. Task ids are [a-z0-9-] (§4.4 rule 3)
// precisely so they are safe as both a path and a git ref component.
func BranchName(runSlug, taskID string) string { return "loom/" + runSlug + "/" + taskID }

// Worktrees is the git plumbing: create, verify, seed, bootstrap, remove.
//
// Store is here for exactly one reason — §6.2 step 3's hard precondition, which
// is a query against `sessions`, not against git. Everything else in this type
// is pure filesystem and git.
//
// # What a worktree does NOT isolate (§6.4, BINDING disclosure)
//
// This list is not caveats; it is the failure mode that will actually bite, and
// 3a's M4 exists to measure it. Nothing in this file solves any of it, and the
// honest statement is that a green check in a worktree is a claim about the
// tree, never about the machine:
//
//   - ports — two children running a dev server on 3000 collide, and the second
//     one's check fails for a reason unrelated to its work;
//   - databases — a shared local Postgres/Redis is shared, and migrations run by
//     one child are seen by all of them, in whatever order they happen to run;
//   - caches and build state — ~/.cache, ~/.npm, the Go build cache, Docker;
//     mostly benign, occasionally catastrophic, since one poisoned entry fails
//     every child at once;
//   - gitignored files do not come along — a fresh worktree has no .env, no
//     node_modules, no venv. This is the most common cause of a check that fails
//     for non-reasons, and it is the entire reason §6.5's bootstrap and
//     seed_files exist below;
//   - the git object store is shared — a `git gc` in one worktree affects all of
//     them, and branch deletion is global, which is a second reason nothing here
//     ever deletes a branch;
//   - global tool state and test state that is not in the repo — daemons,
//     containers, ~/.claude itself, fixtures on absolute paths.
//
// The port class is DETECTED, not solved, and the detection lives in check.go's
// envSuspectShapes as a triage label on a failure. It is a heuristic, never a
// diagnosis, and it never turns a failure into a pass.
type Worktrees struct {
	Layout Layout
	Store  *store.Store
	// Cap is §6.6's concurrency cap for this run. Zero means unset and yields
	// Concurrency3a — NOT zero-means-no-children, which would turn a forgotten
	// field into a run that silently never starts.
	Cap int
	// BootstrapTimeout bounds §6.5's bootstrap subprocess. Zero yields
	// CheckTimeoutDefault: bootstrap is a check-shaped subprocess and the
	// manifest gives it no timeout of its own, so it inherits the check's.
	BootstrapTimeout time.Duration
	// Environ defaults to os.Environ(). Injected for the same reason Checker
	// injects it — a test asserts the scrubbing instead of mutating the process
	// environment.
	Environ []string
}

// Request is everything Create needs, gathered by the caller so this type never
// reaches back into a manifest or a store row.
type Request struct {
	RunSlug   string
	TaskID    string
	RepoLabel string
	// RepoPath is the repo's PRIMARY work tree — the directory `git worktree
	// add` is run against, and the source of seed-file copies.
	RepoPath string
	// Base is the run's pinned base sha (§6.2 step 1), recorded per repo on the
	// RUN at run-creation time and not per task. Every child of a run branches
	// from the same commit.
	Base string
	// Setup is the repo's bootstrap argv and seed-file list (§6.5).
	Setup RepoSetup
	// Brief is the fully assembled §7 text, written to the meta dir. Assembly
	// lives in spawn.go: this type writes bytes and does not know what a brief
	// says.
	Brief string
}

// Created is the result of a successful Create, and it is what the spawn gate
// renders and what the store row records.
type Created struct {
	// Dir is PHYSICALLY RESOLVED (filepath.EvalSymlinks). This is load-bearing,
	// not cosmetic: a process's getcwd() returns the physical path, so claude
	// records and derives its transcript directory from the resolved form. A
	// stored unresolved cwd makes transcript lookup miss entirely and the
	// session sits at `unknown` forever. See session.physicalDir and
	// docs/spikes/2026-07-22-add-dir-spike.md — this is also the exact string
	// §6.2 step 3's occupancy check and §13.3's orphan recovery compare on.
	Dir     string
	MetaDir string
	Branch  string
	// Base is the sha the worktree was actually created at, resolved to a full
	// object name so what is recorded is a commit and not the abbreviation or
	// branch name the caller happened to hold.
	Base string
	// Seeded lists the seed files copied, so §5.1's gate can show the human that
	// .env is about to be handed to an agent.
	Seeded []string
	// SeedRefused lists the seed entries that were REFUSED, and why (§6.5).
	//
	// It is a field rather than an error return because the two halves of §6.5
	// have opposite severities and one error value cannot carry both: a failed
	// bootstrap BLOCKS the spawn, a refused seed file does not — "refusals are
	// per-file and are rendered; one bad entry must not silently drop the rest".
	// Returning an error would block the spawn; dropping it would make the
	// refusal invisible, which is the failure mode this codebase forbids. So it
	// travels with the value the gate already renders.
	SeedRefused []SeedFileError
	// Reused is true when the branch or the path already existed and matched
	// (a re-spawn). Idempotence is a requirement, not a nicety: crash recovery
	// re-derives everything from (run, task) and must be able to re-run this.
	Reused bool
}

// Create materializes the worktree, meta dir, brief, seed files and bootstrap,
// in §6.2's order, idempotently:
//
//  1. verify the pinned base exists in the repo;
//  2. HARD PRECONDITION FIRST — refuse with ErrWorktreeOccupied if any live
//     sessions row already has cwd == physicalDir(<worktree>). Two claudes in
//     one worktree on one branch is the worst outcome in this design and is made
//     structurally impossible here rather than argued about in recovery;
//  3. `git -C <repo> worktree add -b <branch> <path> <base>`; if the branch
//     already exists, `worktree add <path> <branch>`; if the path exists and its
//     branch matches, reuse. Any other collision is a hard, loud refusal —
//     never a silent overwrite;
//  4. mkdir the .meta dir BESIDE the worktree and write brief.md into it.
//     Nothing Loom owns is written inside the worktree, so `git status
//     --porcelain` in a fresh worktree is empty;
//  5. copy seed files, run bootstrap (§6.5).
//
// §9.2's multi-producer base merge is NOT here: same-repo dependency
// materialization is part of the deferred scheduler. In 3a a worktree is created
// at the run's pinned base, full stop.
//
// A dirty primary work tree is FINE at spawn and is allowed; dirtiness matters
// at merge. The gate warns when an in-scope repo is dirty, naming it, because
// children branch from committed HEAD and uncommitted work in the user's own
// tree is invisible to every child.
//
// On a bootstrap failure the Created value is returned ALONGSIDE the error: the
// worktree exists, is on disk, and is the caller's to keep or discard, and a
// zero value would strand it with nothing naming it.
func (w *Worktrees) Create(req Request) (Created, error) {
	if w.Store == nil {
		// Refusing beats proceeding. Without a store there is no way to run step
		// 2's occupancy query, and a Create that skips it is exactly the
		// double-spawn this design calls its worst outcome.
		return Created{}, errors.New("delegate: Worktrees.Create needs a Store to check worktree occupancy")
	}
	if req.RunSlug == "" || req.TaskID == "" || req.RepoLabel == "" || req.RepoPath == "" {
		return Created{}, fmt.Errorf("delegate: incomplete worktree request (run=%q task=%q repo=%q path=%q)",
			req.RunSlug, req.TaskID, req.RepoLabel, req.RepoPath)
	}

	dir := w.Layout.Dir(req.RunSlug, req.RepoLabel, req.TaskID)
	meta := w.Layout.MetaDir(req.RunSlug, req.RepoLabel, req.TaskID)
	branch := BranchName(req.RunSlug, req.TaskID)

	// 1. The pinned base must exist in THIS repo.
	base := strings.TrimSpace(req.Base)
	if base == "" {
		return Created{}, fmt.Errorf("delegate: task %s has no pinned base", req.TaskID)
	}
	full, err := gitOut(req.RepoPath, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return Created{}, fmt.Errorf("delegate: pinned base %s missing from %s: %w", base, req.RepoPath, err)
	}
	base = strings.TrimSpace(full)

	// 2. Occupancy, and the cap, before any side effect.
	if _, occupied, err := w.Occupant(dir); err != nil {
		return Created{}, err
	} else if occupied {
		return Created{}, fmt.Errorf("%w: %s", ErrWorktreeOccupied, dir)
	}
	live, err := w.LiveChildren(req.RunSlug)
	if err != nil {
		return Created{}, err
	}
	if limit := w.cap(); live >= limit {
		return Created{}, fmt.Errorf("%w (%d/%d)", ErrCapReached, live, limit)
	}

	// 3. The worktree itself. Prune first, unconditionally: an administrative
	// entry whose directory the user deleted by hand makes `worktree add` refuse
	// this path AND refuse the branch as "already checked out", and the remedy is
	// a command the user should never have to learn. Prune only drops entries
	// whose directory is already gone, so it cannot take live work.
	_ = gitRun(req.RepoPath, "worktree", "prune")

	reused := false
	switch info, statErr := os.Stat(dir); {
	case statErr == nil && info.IsDir():
		// The path is there. Reuse is allowed only when it is genuinely THIS
		// task's worktree of THIS repo on THIS branch; anything else is a
		// collision and is refused loudly rather than overwritten.
		if err := verifyWorktree(dir, req.RepoPath, branch); err != nil {
			return Created{}, err
		}
		reused = true
	case statErr == nil:
		return Created{}, fmt.Errorf("delegate: %s exists and is not a directory", dir)
	case !errors.Is(statErr, fs.ErrNotExist):
		return Created{}, statErr
	default:
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return Created{}, err
		}
		if branchExists(req.RepoPath, branch) {
			// A re-spawn. The branch carries the previous attempt's commits and is
			// never recreated from base, because that would silently discard them.
			reused = true
			err = gitRun(req.RepoPath, "worktree", "add", dir, branch)
		} else {
			err = gitRun(req.RepoPath, "worktree", "add", "-b", branch, dir, base)
		}
		if err != nil {
			return Created{}, fmt.Errorf("delegate: worktree add %s: %w", dir, err)
		}
	}

	// 4. The meta dir BESIDE the worktree. 0o700/0o600 because the brief carries
	// the authorization text and block.json carries whatever the child put in it.
	// This is not a security boundary — slice 1 §6.5 states Loom's
	// confidentiality claims honestly — but there is no reason to widen it.
	if err := os.MkdirAll(meta, 0o700); err != nil {
		return Created{}, err
	}
	if err := os.WriteFile(filepath.Join(meta, briefFile), []byte(req.Brief), 0o600); err != nil {
		return Created{}, err
	}

	c := Created{
		Dir:     physicalPath(dir),
		MetaDir: physicalPath(meta),
		Branch:  branch,
		Base:    base,
		Reused:  reused,
	}

	// 5. Seed files, then bootstrap.
	c.Seeded, c.SeedRefused = w.seed(req.RepoPath, dir, req.Setup.SeedFiles)
	if err := w.bootstrap(dir, req.Setup.Bootstrap); err != nil {
		return c, err
	}
	return c, nil
}

// Remove takes the worktree directory away and KEEPS the branch and the meta
// dir (§6.3):
//
//	merged            → worktree removed, branch kept, .meta kept
//	discarded         → worktree removed (--force), branch kept, .meta kept
//	child died        → worktree KEPT UNTOUCHED, branch kept
//	run abandoned     → kept until an explicit sweep
//	Loom restarted    → kept; re-derived from (run, task)
//
// There is deliberately no DeleteBranch anywhere in this package. A branch is a
// few bytes and is the only durable record of a discarded attempt; deleting one
// is the single irreversible act available in this design, the object store is
// shared across every worktree of the repo, and there is no reason to take it. A
// prune-and-delete sweep is offered as an explicit, listing-first human action
// elsewhere.
//
// .meta survives a merge because block.json and the final brief are the only
// durable record of what the child was told and why it stopped. It is a few
// kilobytes.
//
// The "child died" row is the CALLER's decision — a dead child's worktree is not
// garbage and Remove is simply not called for it. What Remove enforces is the
// converse: it refuses while a LIVE session occupies the directory, because
// pulling a tree out from under a running claude yields a session that cannot
// write, cannot say why, and leaves the repo's worktree list disagreeing with
// the disk. force overrides, because the discard path is a human saying so.
//
// Every row of the table above has to leave the repo in a state the user never
// has to repair by hand, which is why the prune brackets the removal and why a
// forced removal git itself refuses falls back to taking the directory.
//
// repoPath is a parameter and not derived from the worktree. That is a deviation
// from the scaffold's signature and it is load-bearing: in the case that matters
// most — the user deleted the directory by hand — there is nothing left to ask,
// and pruning the stale administrative entry is exactly what must not be skipped.
func (w *Worktrees) Remove(repoPath, runSlug, repoLabel, taskID string, force bool) error {
	if repoPath == "" {
		return errors.New("delegate: Worktrees.Remove needs the repo path to prune the worktree list")
	}
	dir := w.Layout.Dir(runSlug, repoLabel, taskID)

	// Prune BEFORE looking at the directory: the stale-entry case (directory
	// gone, `git worktree list` still advertising it) is invisible from the
	// filesystem and is the one that breaks the next spawn.
	_ = gitRun(repoPath, "worktree", "prune")

	switch _, err := os.Stat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		// Idempotent, and deliberately not an error: a user deleting a worktree
		// by hand is a supported thing to do to a window, and the prune above has
		// already repaired the only durable consequence.
		return nil
	case err != nil:
		return err
	}

	if !force {
		if row, occupied, err := w.occupantOrSkip(dir); err != nil {
			return err
		} else if occupied {
			return fmt.Errorf("%w: %s (session %s)", ErrWorktreeOccupied, dir, row.Name)
		}
	}

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	if err := gitRun(repoPath, append(args, dir)...); err != nil {
		if !force {
			// Typically an uncommitted change in the worktree. Loud, and the
			// remedy — discard, which is --force — is the human's to choose.
			return fmt.Errorf("delegate: worktree remove %s: %w", dir, err)
		}
		// force, and git still refused: the administrative entry is corrupt or
		// the directory is no longer recognisable as a worktree. Take the
		// directory and prune, because the alternative is handing back a repo the
		// user has to repair themselves, which this package treats as a failure.
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return errors.Join(fmt.Errorf("delegate: worktree remove %s: %w", dir, err), rmErr)
		}
	}
	_ = gitRun(repoPath, "worktree", "prune")
	return nil
}

// Prune is `git worktree prune` as an explicit action: it drops administrative
// entries whose directories are gone and touches nothing else. It never deletes
// a branch — see Remove.
func Prune(repoPath string) error { return gitRun(repoPath, "worktree", "prune") }

// PinBase reads the repo's current HEAD — §6.2 step 1's pinned base, taken once
// per repo at run creation. Every child of a run branches from the same commit.
func PinBase(repoPath string) (string, error) {
	out, err := gitOut(repoPath, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("delegate: pin base in %s: %w", repoPath, err)
	}
	return strings.TrimSpace(out), nil
}

// Dirty reports whether the repo's primary work tree has uncommitted changes,
// for §6.2's spawn-gate warning. It intentionally uses internal/gitdiff's
// capture rather than a second `git status` spelling.
//
// A capture ERROR is not dirtiness: gitdiff reports "not a git repository" the
// same way it reports any failure, and warning "this repo is dirty" about a path
// that is not a repo at all points the human at the wrong problem. Repo
// existence is the manifest loader's check, not this one's.
func Dirty(repoPath string) bool {
	d := gitdiff.WorkingTree(repoPath)
	return d.Error == "" && d.Dirty
}

// PinBases resolves every repo in a manifest to the commit its children will
// branch from — §6.2 step 1's "pinned per REPO ON THE RUN, not per task, so
// every child of a run branches from the same commit".
//
// Pinned ONCE, at run creation, and stored. The rejected alternative — each
// spawn reading HEAD when it happens to run — makes two children of the same
// run branch from different commits whenever the human commits between
// approvals, and §10's integration stops being deterministic before it is even
// built. It also makes a re-spawn unreproducible.
//
// A repo whose HEAD cannot be read is an ERROR and stops the run being created,
// rather than a missing entry: `git worktree add` would refuse with an empty
// base anyway, and refusing at creation puts the failure next to the gesture
// that caused it instead of on the first approve.
func PinBases(m Manifest) (map[string]string, error) {
	out := make(map[string]string, len(m.RepoPaths))
	for label, dir := range m.RepoPaths {
		sha, err := gitOut(dir, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return nil, fmt.Errorf("delegate: repo %q (%s): cannot pin a base commit: %w", label, dir, err)
		}
		out[label] = strings.TrimSpace(sha)
	}
	return out, nil
}

// Occupant returns the live sessions row whose cwd equals physicalDir(dir), if
// any. It is both §6.2 step 3's precondition and §13.3's orphan-recovery lookup,
// and it deliberately keys on CWD rather than on a tag.
//
// Tags are NOT visible in tmux. store.SetTags writes the sessions.tags COLUMN;
// tmux carries only loom-<uuid>, because ARCHITECTURE §4.1 forbids embedding a
// label in the tmux name ('.' and ':' break `tmux -t` targeting). Revision 1
// built orphan recovery on interrogating tmux for a tag; there is nothing there
// to interrogate. Cwd is the one identity Launcher.Launch writes in the same
// call that creates the tmux session, so it is the one that exists the instant
// the child does.
func (w *Worktrees) Occupant(dir string) (store.SessionRow, bool, error) {
	if w.Store == nil {
		return store.SessionRow{}, false, errors.New("delegate: Worktrees.Occupant needs a Store")
	}
	rows, err := w.Store.Live()
	if err != nil {
		return store.SessionRow{}, false, fmt.Errorf("delegate: occupancy check for %s: %w", dir, err)
	}
	want := physicalPath(dir)
	for _, r := range rows {
		if r.Cwd == want {
			return r, true, nil
		}
	}
	return store.SessionRow{}, false, nil
}

// LiveChildren counts one run's live children — §6.6's cap, counted from the
// sessions table rather than from delegation_tasks.state.
//
// Two counts of the same number, with a reason. spawn.go's ActiveChildren counts
// task STATES, which is the right thing to render at a gate that already holds
// the run's rows. This one counts live SESSIONS whose cwd is under the run's
// worktree root, and it is the backstop at the site that actually consumes the
// slot. It is better evidence in exactly the cases the cap protects against: two
// Loom instances against one DB is supported (§13), and both can read "2
// running" and both approve; a state column left behind by a crash can claim a
// child that is not there. A live tmux session with a cwd can do neither.
//
// It counts running AND blocked children without distinguishing them, which is
// what §6.6 asks for — a blocked child holds its worktree and its context, and
// that is the entire reason Loom parks children instead of killing them.
func (w *Worktrees) LiveChildren(runSlug string) (int, error) {
	if w.Store == nil {
		return 0, errors.New("delegate: Worktrees.LiveChildren needs a Store")
	}
	rows, err := w.Store.Live()
	if err != nil {
		return 0, fmt.Errorf("delegate: cap count for run %s: %w", runSlug, err)
	}
	root := physicalPath(w.Layout.RunRoot(runSlug)) + string(os.PathSeparator)
	n := 0
	for _, r := range rows {
		if strings.HasPrefix(r.Cwd, root) {
			n++
		}
	}
	return n, nil
}

// cap is §6.6's effective cap. Zero means unset and yields 3a's 3, not the
// shipped default of 4: this file belongs to 3a, and a Worktrees constructed
// without a cap must not quietly run a wider fan-out than the slice it is part
// of. The hard maximum is enforced here as well, because a cap read from config
// is user input and must not be able to express "unlimited".
func (w *Worktrees) cap() int {
	switch {
	case w.Cap <= 0:
		return Concurrency3a
	case w.Cap > ConcurrencyMax:
		return ConcurrencyMax
	case w.Cap < ConcurrencyMin:
		return ConcurrencyMin
	}
	return w.Cap
}

// occupantOrSkip is Occupant with a nil Store treated as "cannot tell", and it
// is used by Remove only. Create refuses without a store because it is about to
// put a second claude into a directory; Remove is also reachable from a cleanup
// sweep, where refusing to tidy up because no DB was passed helps nobody.
func (w *Worktrees) occupantOrSkip(dir string) (store.SessionRow, bool, error) {
	if w.Store == nil {
		return store.SessionRow{}, false, nil
	}
	return w.Occupant(dir)
}

// seed implements §6.5 step 1. Every entry is judged independently and a refusal
// never aborts the rest: one bad line in an agent-authored manifest must not
// silently cost the child its .env.
func (w *Worktrees) seed(repoPath, dir string, files []string) (seeded []string, refused []SeedFileError) {
	for _, entry := range files {
		clean, mode, bad := judgeSeedFile(repoPath, entry)
		switch {
		case bad != nil:
			refused = append(refused, *bad)
			continue
		case clean == "":
			continue // blank line in the manifest, not a refusal
		}
		if err := copySeedFile(filepath.Join(repoPath, clean), filepath.Join(dir, clean), mode); err != nil {
			refused = append(refused, SeedFileError{File: entry, Why: err.Error()})
			continue
		}
		seeded = append(seeded, clean)
	}
	return seeded, refused
}

// SeedPlan answers §6.5's question about a set of entries WITHOUT copying
// anything: which will be handed to the child, and which are refused and why.
//
// It exists so the spawn gate can render both halves. §6.5 binds "each copy is
// listed at the spawn gate — the human is being shown that .env is about to be
// handed to an agent", and the corollary is that a REFUSED .env must be shown
// too: the child then runs a check that fails for a reason that has nothing to
// do with its work (§6.4's most common failure, and 3a's M4). Learning that
// after the spawn, from a return value nothing renders, is the invisible-failure
// mode this codebase forbids.
//
// Every judgement is made by the same judgeSeedFile the copier uses, so the gate
// cannot promise a copy that Create will refuse. It is a judgement about a repo
// that may change between the gate and the spawn, which is exactly why Create
// judges again rather than trusting this — the same "load, then re-validate at
// execution" shape as §8.1's check cwd.
func SeedPlan(repoPath string, files []string) (planned []string, refused []SeedFileError) {
	for _, entry := range files {
		clean, _, bad := judgeSeedFile(repoPath, entry)
		switch {
		case bad != nil:
			refused = append(refused, *bad)
		case clean != "":
			planned = append(planned, clean)
		}
	}
	return planned, refused
}

// judgeSeedFile applies §6.5's refusal rules to one entry and returns the
// cleaned relative path and the source file's permission bits. A nil error with
// an empty path means the entry was blank — skipped, not refused.
//
// The rules fail CLOSED throughout: anything this function cannot positively
// justify is refused, because a seed copy is Loom handing a secret to an agent.
func judgeSeedFile(repoPath, entry string) (clean string, mode fs.FileMode, refusal *SeedFileError) {
	rel := strings.TrimSpace(entry)
	if rel == "" {
		return "", 0, nil
	}
	clean = filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", 0, &SeedFileError{File: entry, Why: "escapes the repo"}
	}
	src := filepath.Join(repoPath, clean)
	fi, err := os.Lstat(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", 0, &SeedFileError{File: entry, Why: "not present in the primary work tree"}
	case err != nil:
		return "", 0, &SeedFileError{File: entry, Why: err.Error()}
	case fi.Mode()&os.ModeSymlink != 0:
		// Refused rather than followed: the target is chosen by whatever wrote
		// the repo, and following it is how "copy .env" becomes "copy
		// ~/.ssh/id_rsa".
		return "", 0, &SeedFileError{File: entry, Why: "is a symlink"}
	case !fi.Mode().IsRegular():
		return "", 0, &SeedFileError{File: entry, Why: "is not a regular file"}
	}
	// The lexical test above catches "../x". This one catches the same escape
	// achieved through a symlinked PARENT directory, which no amount of
	// filepath.Clean will reveal.
	if !insideDir(repoPath, src) {
		return "", 0, &SeedFileError{File: entry, Why: "escapes the repo"}
	}
	if gitTracked(repoPath, clean) {
		// §6.5: a tracked file is either already in the worktree, or copying it
		// hides a modification from every diff in the system.
		return "", 0, &SeedFileError{File: entry, Why: "is tracked by git"}
	}
	if !gitIgnored(repoPath, clean) {
		return "", 0, &SeedFileError{File: entry, Why: "is not gitignored"}
	}
	// The mode is carried from the source because a .env copied as 0644 is a
	// different file from the one the user has.
	return clean, fi.Mode().Perm(), nil
}

// bootstrap implements §6.5 step 2: a check-shaped subprocess with the worktree
// as cwd, whose failure BLOCKS the spawn. It is strictly cheaper to fail here
// than to spend a child's whole context discovering that node_modules is
// missing.
func (w *Worktrees) bootstrap(dir string, argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	timeout := w.BootstrapTimeout
	if timeout <= 0 {
		timeout = CheckTimeoutDefault
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// ARGV ARRAY, NO SHELL (§4.3): the manifest is agent-authored, and a shell
	// string turns every quoting mistake into an execution surface.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	environ := w.Environ
	if environ == nil {
		environ = os.Environ()
	}
	// The summarizer's precedent: a subprocess that believes it is inside a
	// Claude session changes its own behaviour via hooks and output modes, and a
	// bootstrap whose result depends on who launched it is not a precondition.
	cmd.Env = append(scrubEnv(environ), "LOOM_WORKTREE="+dir)
	// Set for the reason the summarizer sets it: an orphaned grandchild holding
	// the pipe otherwise wedges Wait() forever, and a wedged bootstrap holds a
	// slot against the cap with nothing on screen.
	cmd.WaitDelay = 5 * time.Second

	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	be := &BootstrapError{Argv: argv, Exit: -1, Output: capOutput(string(out))}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		be.Exit = ee.ExitCode()
	} else {
		// Not an exit status: cmd[0] not found, or a fork failure. That message is
		// the only description of the failure that exists, so it is kept.
		be.Output = strings.TrimRight(be.Output, "\n") + "\n" + err.Error()
	}
	if ctx.Err() != nil {
		be.Output = strings.TrimRight(be.Output, "\n") + fmt.Sprintf("\n[bootstrap timed out after %s]", timeout)
	}
	return be
}

// SeedFileError names a refused seed file and why (§6.5): the entry is not
// gitignored (copying a tracked file means it is either already there or you are
// hiding a modification), it escapes the repo, or it is a symlink. Refusals are
// per-file and are rendered; one bad entry must not silently drop the rest.
type SeedFileError struct {
	File string
	Why  string
}

// Error names the file and the reason. A bare "seed file refused" would send the
// human to the logs to find out which of five entries was dropped, and this
// string is what the spawn gate shows.
func (e *SeedFileError) Error() string {
	return fmt.Sprintf("delegate: seed file %q refused: %s", e.File, e.Why)
}

// BootstrapError carries a failed bootstrap's captured output. §6.5: a failed
// bootstrap BLOCKS the spawn, loudly, with output — it is strictly cheaper to
// fail here than to spend a child's whole context discovering the dependencies
// were never installed.
type BootstrapError struct {
	Argv   []string
	Exit   int
	Output string
}

// Error names the argv and the exit code. The output stays a field because it is
// rendered as a block, not folded into a one-line error.
func (e *BootstrapError) Error() string {
	return fmt.Sprintf("delegate: bootstrap %q failed (exit %d)", strings.Join(e.Argv, " "), e.Exit)
}

// verifyWorktree is the reuse predicate for an existing path: it must be a work
// tree, of THIS repo, with THIS branch checked out. Each failure gets its own
// message because the remedies differ — a foreign directory at the path is a
// naming collision, a detached HEAD is a half-finished recovery, and a different
// branch is two runs colliding.
func verifyWorktree(dir, repoPath, branch string) error {
	inWorkTree, err := gitOut(dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inWorkTree) != "true" {
		return fmt.Errorf("delegate: %s exists but is not a git work tree — refusing to overwrite it", dir)
	}
	mine, err1 := gitOut(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	theirs, err2 := gitOut(repoPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err1 != nil || err2 != nil {
		return fmt.Errorf("delegate: cannot verify that %s belongs to %s", dir, repoPath)
	}
	if physicalPath(strings.TrimSpace(mine)) != physicalPath(strings.TrimSpace(theirs)) {
		return fmt.Errorf("delegate: %s is a work tree of another repository — refusing to overwrite it", dir)
	}
	head, err := gitOut(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("delegate: %s has a detached HEAD, expected branch %s", dir, branch)
	}
	if got := strings.TrimSpace(head); got != branch {
		return fmt.Errorf("delegate: %s is on branch %s, expected %s", dir, got, branch)
	}
	return nil
}

func branchExists(repoPath, branch string) bool {
	return gitRun(repoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch) == nil
}

// tracked and ignored are the two questions §6.5 asks git about a seed entry.
// Both fail CLOSED — an error from either reads as "tracked" / "not ignored" and
// the copy does not happen, because a seed copy that cannot be justified is one
// that should not be made.
func gitTracked(repoPath, rel string) bool {
	return gitRun(repoPath, "ls-files", "--error-unmatch", "--", rel) == nil
}

func gitIgnored(repoPath, rel string) bool {
	return gitRun(repoPath, "check-ignore", "-q", "--", rel) == nil
}

// inside reports whether path lies within base once both are physically
// resolved. Resolving both sides is the point: the caller has already done the
// lexical check, and what is left is the symlinked-parent escape that only shows
// up after EvalSymlinks.
func insideDir(base, path string) bool {
	rel, err := filepath.Rel(physicalPath(base), physicalPath(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// copyFile copies a seed file, creating parent directories. The mode is carried
// from the source because a .env copied as 0644 is a different file from the one
// the user has.
func copySeedFile(src, dst string, mode fs.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}

// gitOut runs one git command and returns stdout, folding stderr into the error
// so a failure says why rather than just "exit status 128" — the same shape
// internal/gitdiff uses, restated rather than imported because gitdiff's helper
// is unexported and this package needs exit statuses, not diffs.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("%s: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

func gitRun(dir string, args ...string) error {
	_, err := gitOut(dir, args...)
	return err
}

// physicalDir is session.PhysicalDir, aliased rather than reimplemented.
//
// The rule it enforces is that every path this package stores or compares —
// worktree cwd, §6.2 step 3's occupancy refusal, §13.3's orphan recovery — must
// be BYTE-IDENTICAL to what Launcher.Launch wrote into sessions.cwd, which is
// the symlink-resolved form. A second copy here would compile, pass its own
// tests, and drift the day either side changed; then the double-spawn guard
// would silently stop guarding, which is the one failure this design calls its
// worst outcome. One function, one behaviour, no drift.
var physicalDir = session.PhysicalDir

// physicalPath is physicalDir extended to a path that does not exist YET.
//
// Not a stylistic wrapper. EvalSymlinks fails on a missing path and
// session.PhysicalDir then returns its argument unchanged — correct for its own
// callers, which only ever resolve a directory that is already there, and wrong
// for the occupancy check, which runs BEFORE `worktree add` creates anything. On
// macOS the difference is routine rather than exotic: /tmp is a symlink to
// /private/tmp and a per-user temp dir hangs off /var → /private/var, so the
// unresolved "<root>/<run>/<repo>/<task>" and the resolved string
// Launcher.Launch wrote into sessions.cwd do not compare equal, and the guard
// against two claudes in one worktree silently stops guarding — the exact
// failure the alias above exists to prevent.
//
// So: resolve the deepest ancestor that DOES exist and re-append the rest. For a
// path that exists this is physicalDir and nothing else, which is what keeps the
// one-behaviour promise intact.
func physicalPath(p string) string {
	if p == "" {
		return p
	}
	if _, err := os.Lstat(p); err == nil {
		return physicalDir(p)
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(physicalPath(parent), filepath.Base(p))
}
