package delegate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

// §11.1–11.3 — the STOP protocol: what a child writes when it cannot proceed,
// how Loom notices, and how an unforeseen dependency becomes an amendment.
//
// BINDING framing, and it is not decoration: rendezvous is the FALLBACK.
// Dependency-gated scheduling (§9) is the mechanism. If most tasks rendezvous
// the manifest was wrong, and that count IS M3 in §2 — the measurement that
// decides whether this half of the slice should exist at all. Nothing in this
// file should ever be made smoother at the cost of making that count less
// visible.
//
// Why a FILE and not a message, a tag or an exit: the child does not exit, is
// not killed, and is not asked to summarize. An idle `claude` at a prompt costs
// nothing and retains its ENTIRE context, which is the whole argument for a park
// rather than a restart, and the whole reason §12.2's watchdogs flag instead of
// killing. The file lives in `<task-id>.meta/` BESIDE the worktree, so a block
// declaration can never be committed as an artifact and never appears in the
// child's diff (§6.2 step 4) — and, per worktree.go's comment, an in-worktree
// exclusion is not merely ugly but impossible, since <worktree>/.git is a FILE.

// BlockKind is §11.1's `kind`. The four are distinguished because the REMEDIES
// are disjoint, which is also why §12.1 splits its two deadlock shapes along
// this line: `needs-artifact` resolves inside the run, the other three resolve
// outside it.
type BlockKind string

const (
	// BlockNeedsArtifact — a peer must publish something. The only kind Loom can
	// resolve on its own: when the producer verifies and the artifact publishes,
	// §11.4 materializes and re-seeds.
	BlockNeedsArtifact BlockKind = "needs-artifact"
	// BlockNeedsDecision — a human must choose. Also what LOOM writes for a
	// §9.2 producer conflict and, in a form it authors itself, for §10.3's
	// integration failures.
	BlockNeedsDecision BlockKind = "needs-decision"
	// BlockNeedsScope — the work requires touching paths outside this task's
	// authorization. This is the CORRECT and encouraged response to discovering
	// the task boundary was drawn wrong, and it is the signal that makes scope
	// overreach visible instead of silent. A child that quietly widens its own
	// scope is the failure this kind exists to convert into a rendered event.
	BlockNeedsScope BlockKind = "needs-scope"
	// BlockExternal — a credential, a service, an outage. Nothing in the run can
	// clear it; §12.1(b) lists it as an owed decision.
	BlockExternal BlockKind = "blocked-external"
)

// BlockVersion is `block.json`'s `"block"` field. Versioned from the first
// release because the brief states this format VERBATIM to an agent, and a
// format an agent was told in prose is one Loom will be reading unversioned
// copies of for a long time.
const BlockVersion = 1

// Block is `<task-id>.meta/block.json` (§11.1).
//
// Field names are the wire format and are not to be prettified: the brief prints
// this shape to the child verbatim, so a rename here silently invalidates every
// brief already written into a live worktree.
type Block struct {
	Version int       `json:"block"`
	Run     string    `json:"run"`  // run SLUG, which is what the child was told
	Task    string    `json:"task"` // task id
	At      time.Time `json:"at"`
	Kind    BlockKind `json:"kind"`
	// Need is populated for needs-artifact. `need.artifact` MAY name an artifact
	// that appears nowhere in the manifest — that is the common case for an
	// unforeseen dependency, and §11.3's re-plan branch exists for it. A parser
	// that rejected an unknown artifact id would reject exactly the blocks this
	// mechanism was built to carry.
	Need BlockNeed `json:"need"`
	// Summary is one line, for the run view. Detail is the child's full
	// explanation, capped on read.
	Summary string `json:"summary"`
	Detail  string `json:"detail"`
	// ResumeWhen is the child's own statement of its unblock condition, in
	// prose. It is RENDERED and never parsed: the machine-checkable condition is
	// derived from Need and the graph, and treating a sentence an agent wrote as
	// an executable predicate is how a park becomes permanent.
	ResumeWhen string `json:"resume_when"`

	// Paths is the concrete authorization a needs-scope block is asking for,
	// when the child names one. Advisory input to §11.3's proposal; never
	// auto-granted.
	Paths []string `json:"paths,omitempty"`

	// Author records who wrote this declaration. Loom authors blocks too — a
	// §9.2 producer conflict and a §10.3 integration failure are both parked
	// through this exact path, deliberately, so there is one park mechanism and
	// one resume mechanism rather than two that drift. The field exists so the
	// UI can say "Loom stopped this" rather than implying the child did.
	Author BlockAuthor `json:"author,omitempty"`
}

// BlockAuthor distinguishes a child's own declaration from one Loom wrote on its
// behalf.
type BlockAuthor string

const (
	AuthorChild BlockAuthor = ""     // the default: absent means the child wrote it
	AuthorLoom  BlockAuthor = "loom" // §9.2 conflict, §10.3 integration failure
)

// BlockNeed is §11.1's `need` object.
type BlockNeed struct {
	Artifact string `json:"artifact"`
	From     string `json:"from"` // producing task id, as the CHILD believes it
}

// Empty reports whether a block value carries no declaration, so callers can
// hold a zero Block rather than a *Block and avoid the nil check that would
// otherwise appear at every one of §9's ready sites.
func (b Block) Empty() bool { return b.Kind == "" }

// ParseBlock decodes and validates a block declaration.
//
// Validation is deliberately THIN: version known, kind one of the four, task
// non-empty. Everything else is prose written by an agent under duress, and a
// parser that rejects a block for a missing `resume_when` has converted a
// recoverable park into a silent one. Unknown fields are ignored, not rejected,
// for the same reason.
func ParseBlock(raw []byte) (Block, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return Block{}, errors.New("delegate: block declaration is empty")
	}
	var w blockWire
	if err := json.Unmarshal(trimmed, &w); err != nil {
		return Block{}, fmt.Errorf("delegate: block declaration is not JSON: %w", err)
	}
	b := Block{
		Version:    w.Version,
		Run:        strings.TrimSpace(w.Run),
		Task:       strings.TrimSpace(w.Task),
		Kind:       BlockKind(strings.TrimSpace(string(w.Kind))),
		Need:       BlockNeed{Artifact: strings.TrimSpace(w.Need.Artifact), From: strings.TrimSpace(w.Need.From)},
		Summary:    strings.TrimSpace(w.Summary),
		Detail:     w.Detail,
		ResumeWhen: strings.TrimSpace(w.ResumeWhen),
		Paths:      w.Paths,
		Author:     w.Author,
	}
	// A MISSING version is the current one; any OTHER value is refused. The two
	// halves are not symmetric on purpose. An agent transcribing the brief's
	// example may drop the field, and losing a real park to that is the exact
	// silent outcome §11.2 forbids — whereas a `"block": 2` was written by
	// something that knows a format this build does not, and guessing at it is
	// how a future field with meaning gets read as decoration.
	if b.Version == 0 {
		b.Version = BlockVersion
	}
	if b.Version != BlockVersion {
		return Block{}, fmt.Errorf("delegate: block declaration version %d, want %d", b.Version, BlockVersion)
	}
	switch b.Kind {
	case BlockNeedsArtifact, BlockNeedsDecision, BlockNeedsScope, BlockExternal:
	case "":
		return Block{}, errors.New("delegate: block declaration names no kind")
	default:
		return Block{}, fmt.Errorf("delegate: block declaration kind %q is none of %s, %s, %s, %s",
			b.Kind, BlockNeedsArtifact, BlockNeedsDecision, BlockNeedsScope, BlockExternal)
	}
	if b.Task == "" {
		return Block{}, errors.New("delegate: block declaration names no task")
	}
	b.At = parseBlockTime(w.At)
	return b, nil
}

// blockWire is the decode-side shape, and it exists for exactly one field: At.
//
// Decoding straight into Block would make `time.Time`'s strict RFC3339
// unmarshaller a VALIDATOR — a child that wrote "2026-07-22 14:03:11" would lose
// its entire declaration to a timestamp nobody schedules on, and the park would
// go silent for a cosmetic reason. Everything else round-trips by name, so the
// wire tags stay the ones the brief printed.
type blockWire struct {
	Version    int         `json:"block"`
	Run        string      `json:"run"`
	Task       string      `json:"task"`
	At         string      `json:"at"`
	Kind       BlockKind   `json:"kind"`
	Need       BlockNeed   `json:"need"`
	Summary    string      `json:"summary"`
	Detail     string      `json:"detail"`
	ResumeWhen string      `json:"resume_when"`
	Paths      []string    `json:"paths"`
	Author     BlockAuthor `json:"author"`
}

// parseBlockTime accepts the formats an agent actually writes and degrades to the
// zero time rather than to an error. The timestamp is RENDERED and never compared
// against anything that decides a transition; Loom's own clock is what §12.2's
// watchdogs use, because a child's `at` is a self-report like every other string
// it produces (§3).
func parseBlockTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// MalformedBlock is §11.2's loud failure: `block.json` exists and will not
// parse. It carries the RAW CONTENT, capped, because the whole remedy is a human
// reading what the child actually wrote.
//
// This is never swallowed. A swallowed block is a child parked forever with
// nobody told — it holds a worktree, a slot against §6.6's cap and an hour of
// context, and the run renders as healthy. Degrading to a `block-malformed` flag
// with the text on screen is the "failures degrade rather than crash, and are
// always VISIBLE" rule in its sharpest form.
type MalformedBlock struct {
	TaskID string
	Path   string
	Raw    string
	Err    error
}

func (e *MalformedBlock) Error() string { return "delegate: malformed block declaration" }
func (e *MalformedBlock) Unwrap() error { return e.Err }

// RawBlockCap bounds what MalformedBlock carries into the DB and the UI. The
// same 8KB the seed path uses; a block declaration that is bigger than this is
// not a block declaration.
const RawBlockCap = 8 << 10

// BlockPoll is §11.2's cadence: the run view's own 2s. Stated as a constant here
// rather than inherited from the caller so the polling interval is a decision
// with a citation instead of whatever the UI happened to be doing.
const BlockPoll = 2 * time.Second

// Detector watches every live task's block.json.
//
// THE FILE IS THE TRIGGER; the transcript going idle is CORROBORATION ONLY
// (§11.2, binding). The inverse — treating an idle transcript as a block — would
// make every thinking pause a park, and treating a block as needing transcript
// confirmation would delay a real one behind a 20-minute idle threshold.
//
// Fingerprinting is mtime+size, the `indexed_files` idiom already used across
// this codebase, rather than a content hash: it is one Stat per live task per
// tick where a hash is a read, and a child rewriting its block declaration with
// identical bytes is not an event.
type Detector struct {
	Layout Layout
	Store  *store.Store
	Now    func() time.Time

	// seen is the last fingerprint per (run, task), so an unchanged file is not
	// re-parsed and — more importantly — a block already recorded does not
	// re-fire its state transition on every 2s tick.
	seen map[blockKey]blockStamp
}

// blockKey is keyed by the run SLUG and not the run id because Clear, Read and
// Write are handed a slug (they are path operations, and the path is built from
// the slug) while Poll holds the row. Keying on the id would leave Clear unable
// to forget a fingerprint it just deleted the file for, and the next tick would
// re-fire the stale stamp as a change.
type blockKey struct {
	runSlug string
	taskID  string
}

type blockStamp struct {
	modUnix int64
	size    int64
}

// BlockEvent is one detection. Malformed and Block are mutually exclusive; both
// are rendered.
type BlockEvent struct {
	TaskID    string
	Block     Block
	Malformed *MalformedBlock
	// Cleared is true when a previously-seen block.json disappeared — the child
	// removed it, or a re-spawn wiped the meta dir. It is an event because a
	// task stuck in `blocked` with no declaration on disk is otherwise
	// indistinguishable from one that is legitimately parked.
	Cleared bool
}

// Poll stats every live task's block path once and returns what changed.
//
// It performs the `running → blocked` CAS itself (§11.2) because the CAS and the
// detection must not be separated by a caller's tick boundary: two Loom
// instances polling the same run would otherwise both observe the same new file
// and both act on it. A rejected CAS is not an error — the other instance won,
// the state is right, and the event is still returned so this instance's UI
// updates.
func (d *Detector) Poll(run store.DelegationRun, m Manifest) ([]BlockEvent, error) {
	if d.seen == nil {
		d.seen = map[blockKey]blockStamp{}
	}
	rows := map[string]store.DelegationTask{}
	if d.Store != nil {
		list, err := d.Store.ListDelegationTasks(run.ID)
		if err != nil {
			return nil, err
		}
		for _, row := range list {
			rows[row.TaskID] = row
		}
	}

	var events []BlockEvent
	// firstErr accumulates rather than aborts: one unreadable meta dir must not
	// stop the OTHER tasks of the run being polled. A detector that gives up on
	// the first bad task is a detector that stops noticing parks the moment one
	// worktree is removed under it.
	var firstErr error
	for _, t := range m.Tasks {
		key := blockKey{runSlug: run.Slug, taskID: t.ID}
		if row, ok := rows[t.ID]; ok && !TaskState(row.State).HoldsAChild() {
			// No child, no park. Forgetting the stamp matters: a task that is
			// re-spawned later gets a fresh meta dir, and a remembered stamp
			// from the previous life would make the new declaration look
			// unchanged.
			delete(d.seen, key)
			continue
		}

		path := d.Layout.BlockPath(run.Slug, t.Repo, t.ID)
		st, err := os.Stat(path)
		if err != nil {
			if _, had := d.seen[key]; had {
				delete(d.seen, key)
				if d.Store != nil {
					if cerr := d.Store.SetTaskBlock(run.ID, t.ID, "", d.now().Unix()); cerr != nil && firstErr == nil {
						firstErr = cerr
					}
				}
				events = append(events, BlockEvent{TaskID: t.ID, Cleared: true})
			}
			continue
		}
		stamp := blockStamp{modUnix: st.ModTime().UnixNano(), size: st.Size()}
		if prev, had := d.seen[key]; had && prev == stamp {
			continue
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			// Deliberately NOT stamped: an unreadable file is a transient the
			// next tick should retry, and stamping it would convert a permission
			// blip into a park nobody ever sees.
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		d.seen[key] = stamp

		b, perr := ParseBlock(raw)
		if perr != nil {
			mb := &MalformedBlock{TaskID: t.ID, Path: path, Raw: capRawBlock(string(raw)), Err: perr}
			if err := d.recordMalformed(run, t.ID, mb); err != nil && firstErr == nil {
				firstErr = err
			}
			events = append(events, BlockEvent{TaskID: t.ID, Malformed: mb})
			continue
		}
		if err := d.record(run, t.ID, raw); err != nil && firstErr == nil {
			firstErr = err
		}
		events = append(events, BlockEvent{TaskID: t.ID, Block: b})
	}
	return events, firstErr
}

// record persists a well-formed declaration and performs §11.2's transition.
//
// The BYTES are stored, not a re-encoding of the parsed value: the human's whole
// remedy is reading what the child actually wrote, and a normalized round-trip
// is a different document the moment the parser starts ignoring a field.
func (d *Detector) record(run store.DelegationRun, taskID string, raw []byte) error {
	if d.Store == nil {
		return nil
	}
	now := d.now().Unix()
	if err := d.Store.SetTaskBlock(run.ID, taskID, capRawBlock(string(raw)), now); err != nil {
		return err
	}
	if err := d.setFlag(run.ID, taskID, FlagBlockMalformed, false); err != nil {
		return err
	}
	// A rejected CAS is NOT an error (§11.2): the other Loom instance won the
	// same detection, the row is already `blocked`, and the event still goes back
	// to this instance's UI. Only a task that is not `running` at all — one
	// already blocked, or checking, or abandoned under us — declines, and none of
	// those is a failure this function can do anything about.
	_, err := d.Store.AdvanceTaskCAS(run.ID, taskID, string(StateRunning), string(StateBlocked), now)
	return err
}

// recordMalformed is §11.2's loud degrade: the raw text is stored, the flag is
// set, and the task is STILL parked. Parking on an unparseable declaration is
// deliberate — the child has stopped either way, and leaving the row `running`
// would make Loom's own view of the run disagree with the only observable fact
// (a child sitting at a prompt with a block file beside it).
func (d *Detector) recordMalformed(run store.DelegationRun, taskID string, mb *MalformedBlock) error {
	if d.Store == nil {
		return nil
	}
	now := d.now().Unix()
	if err := d.Store.SetTaskBlock(run.ID, taskID, mb.Raw, now); err != nil {
		return err
	}
	if err := d.setFlag(run.ID, taskID, FlagBlockMalformed, true); err != nil {
		return err
	}
	_, err := d.Store.AdvanceTaskCAS(run.ID, taskID, string(StateRunning), string(StateBlocked), now)
	return err
}

// setFlag is the read-modify-write every flag write in this package needs, done
// as a CAS on the exact column text. An unconditional write loses a flag another
// writer set between the read and the write, and §12.2's watchdogs write the same
// column from a different loop. A rejected CAS is dropped rather than retried in
// a loop: the premise the new value was computed from is gone, and the next tick
// recomputes it from a row it actually read (runs.go's rule).
func (d *Detector) setFlag(runID int64, taskID string, flag Flag, on bool) error {
	return setTaskFlag(d.Store, runID, taskID, flag, on, d.now().Unix())
}

func (d *Detector) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// capRawBlock bounds stored and rendered raw content. Head-only, not head+tail:
// a declaration is a JSON object read from the top, and the interesting part of a
// broken one is where it stopped being JSON.
func capRawBlock(s string) string {
	if len(s) <= RawBlockCap {
		return s
	}
	return s[:RawBlockCap] + "\n… (truncated)"
}

// Read loads one task's block declaration from disk, without touching the
// detector's fingerprint state. The recovery and rendering path: a Loom restart
// has an empty `seen` map and must be able to read what is already there.
func (d *Detector) Read(runSlug, repoLabel, taskID string) (Block, error) {
	path := d.Layout.BlockPath(runSlug, repoLabel, taskID)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// ABSENT IS NOT AN ERROR. Every caller asks this question of tasks
			// that are mostly not blocked, and an error return for the normal
			// case is how a renderer ends up ignoring the error return entirely.
			return Block{}, nil
		}
		return Block{}, err
	}
	b, perr := ParseBlock(raw)
	if perr != nil {
		return Block{}, &MalformedBlock{TaskID: taskID, Path: path, Raw: capRawBlock(string(raw)), Err: perr}
	}
	return b, nil
}

// Write records a Loom-authored block (§9.2's producer conflict, §10.3's
// integration failure) at the same path a child would use, with Author=loom.
//
// Same path, same parser, same resume: the alternative — a second "Loom parked
// this" channel — forks §11.4's delivery machinery on a distinction that changes
// nothing about what has to happen next.
func (d *Detector) Write(runSlug, repoLabel, taskID string, b Block) error {
	if b.Version == 0 {
		b.Version = BlockVersion
	}
	if b.At.IsZero() {
		b.At = d.now().UTC()
	}
	if b.Task == "" {
		b.Task = taskID
	}
	b.Author = AuthorLoom
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	path := d.Layout.BlockPath(runSlug, repoLabel, taskID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Write-then-rename, in the same directory. The detector's own trigger is a
	// stat on this path, and a partially written file is exactly the input that
	// would raise a `block-malformed` flag against a declaration LOOM wrote —
	// the one malformed block that could never be explained by the child.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Forget the stamp so the next Poll reports this as the event it is. Loom
	// writing a block and Loom failing to notice it is the §9.2/§10.3 park going
	// silent, which is the same failure a swallowed child block would be.
	delete(d.seen, blockKey{runSlug: runSlug, taskID: taskID})
	return nil
}

// Clear removes a block declaration after a successful resume. It runs AFTER the
// seed is delivered, never before: a removed declaration with an undelivered
// seed is a task that looks unblocked and is not, and §12.2's `block-stale`
// watchdog would have nothing left to point at.
//
// It is a FILE operation only. The `block_json` column is cleared by the caller
// that holds the run id (Rendezvous.Resume), because clearing the durable record
// and clearing the trigger are two different decisions: a Loom that removed the
// file but crashed before the CAS must still render the task as parked, and the
// column is what it renders from.
func (d *Detector) Clear(runSlug, repoLabel, taskID string) error {
	path := d.Layout.BlockPath(runSlug, repoLabel, taskID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(d.seen, blockKey{runSlug: runSlug, taskID: taskID})
	return nil
}

// Propose turns a block into the amendment §11.3 says it becomes. PURE — it
// reads the graph and returns a proposal; nothing is written and nothing is
// granted. Loom proposes the SHAPE of an amendment and never invents a task
// (§16).
//
//   - needs-artifact naming an artifact some task produces → AmendEdge.
//   - needs-artifact naming an artifact nobody produces → AmendReplan, with
//     From set to Loom's suggestion of the producing task, and ErrNoSuchArtifact
//     returned alongside it so the caller cannot mistake a request for an edge.
//     The value AND the error, because the proposal is exactly what the human
//     needs to see and dropping it to return only the error would make Loom's
//     "here is the task/artifact to add" offer impossible.
//   - needs-scope → AmendScope over the child's requested paths.
//   - needs-decision / blocked-external → no amendment; there is nothing to
//     amend, only somebody to tell (§12.1(b)).
func Propose(e EffectiveGraph, m Manifest, b Block) (Amendment, error) {
	switch b.Kind {
	case BlockNeedsArtifact:
		art := strings.TrimSpace(b.Need.Artifact)
		if art == "" {
			return Amendment{}, fmt.Errorf("delegate: task %q: needs-artifact block names no artifact", b.Task)
		}
		if from := producerOf(e, art); from != "" {
			return Amendment{
				Kind: AmendEdge, Task: b.Task, Artifact: art, From: from, Reason: b.Summary,
			}, nil
		}
		// The value AND the error. The proposal IS the offer §11.3 makes to the
		// human ("add artifact X to task Y"), and returning only the error would
		// leave the one screen that can resolve a re-plan with nothing to show.
		//
		// From is the child's own guess, kept only when it names a real task:
		// the child believes some peer owns this, and a suggestion that names a
		// task nobody has heard of is worse than no suggestion at all. It is
		// rendered as a suggestion and never applied — Loom does not invent
		// tasks (§16).
		suggestion := ""
		if knownTask(e, m, b.Need.From) {
			suggestion = strings.TrimSpace(b.Need.From)
		}
		return Amendment{
			Kind: AmendReplan, Task: b.Task, Artifact: art, From: suggestion, Reason: b.Summary,
		}, fmt.Errorf("%w: %q (task %q blocked on it)", ErrNoSuchArtifact, art, b.Task)

	case BlockNeedsScope:
		return Amendment{
			Kind: AmendScope, Task: b.Task, Paths: normalizePaths(b.Paths), Reason: b.Summary,
		}, nil

	case BlockNeedsDecision, BlockExternal:
		// Nothing to amend. §12.1(b) lists these as owed decisions; there is no
		// edge, no authorization and no re-plan implied by either, and
		// manufacturing an amendment row for them would put entries in §2's M3
		// count that are not unforeseen dependencies at all.
		return Amendment{}, nil
	}
	return Amendment{}, fmt.Errorf("delegate: cannot propose an amendment for block kind %q", b.Kind)
}

// producerOf answers "who publishes this artifact" over the EFFECTIVE graph, so
// an artifact that only exists because of an earlier accepted amendment is found
// too.
func producerOf(e EffectiveGraph, artifact string) string {
	if g := e.Merged(); g.Producer != nil {
		if from := g.Producer[artifact]; from != "" {
			return from
		}
	}
	return e.Declared().Producer[artifact]
}

// knownTask reports whether an id names a task of this run. The manifest is
// consulted as well as the graph because the graph is built from the SNAPSHOT and
// a caller may hold the freshly loaded file; a suggestion is rendering, and the
// wider of the two answers is the useful one.
func knownTask(e EffectiveGraph, m Manifest, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	for _, t := range e.Merged().TaskIDs {
		if t == id {
			return true
		}
	}
	for _, t := range m.Tasks {
		if t.ID == id {
			return true
		}
	}
	return false
}

// normalizePaths sorts and de-duplicates the child's requested authorization so
// two proposals of the same widening are the same row. The globs are NOT
// validated here: an unusable glob is a human-visible part of the proposal they
// are about to approve, and silently dropping it would grant something other than
// what was shown.
func normalizePaths(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// Accept validates an amendment against the effective graph and returns the
// graph it produces.
//
// BINDING (§11.3, last rule): EVERY amendment re-runs cycle detection over the
// AMENDED graph, and one that closes a loop is REJECTED with ErrAmendmentCycle
// and escalated. This is the specific case where a loud block silently becomes a
// deadlock — the child stopped visibly and correctly, the human accepted the
// obvious-looking edge, and the run is now unsatisfiable with no error anywhere.
// §4.5 makes this impossible at load; without this check it is entirely possible
// at runtime, which is worse, because by then there are children holding
// context.
//
// The returned error wraps a *CycleError, so the caller can render the actual
// wait-for path exactly as the loader does.
//
// The walk itself is graph.go's AmendmentCycle and the fold is its With: one
// definition of "the amended graph", used by the check and by the value that is
// returned. Re-deriving either here is how the graph a cycle was checked against
// stops being the graph the scheduler then runs on.
func Accept(e EffectiveGraph, a Amendment) (EffectiveGraph, error) {
	// NOT AUTO-GRANTED, and this is the enforcement point for every kind, not
	// only `scope`. §11.3 makes the human the grantor of an amendment and
	// Amendment.ApprovedAt is the record of that act; a caller that has not got
	// one is asking Loom to widen a child's world on an agent's say-so.
	//
	// The timestamp is read HERE rather than through a helper on purpose: this
	// is the gate the whole never-auto-granted rule rests on, and it must be
	// legible at the place that refuses.
	if a.ApprovedAt.IsZero() {
		return e, fmt.Errorf("%w: %s amendment for task %q", ErrAmendmentNotApproved, a.Kind, a.Task)
	}

	if a.Kind == AmendEdge {
		if a.Task == "" || a.From == "" || a.Artifact == "" {
			return e, fmt.Errorf("delegate: edge amendment is incomplete (task %q, from %q, artifact %q)", a.Task, a.From, a.Artifact)
		}
		// An edge between tasks this run does not have is not a dependency, it
		// is a typo that would gate a consumer on something that can never
		// become verified — a park converted into a permanent one, which is the
		// failure this whole section exists to avoid. AmendmentCycle SKIPS such
		// an edge (correctly: it cannot close a loop), so it is refused here or
		// nowhere.
		if !taskInRun(e, a.Task) || !taskInRun(e, a.From) {
			return e, fmt.Errorf("delegate: edge amendment names a task that is not in this run: %q → %q", a.From, a.Task)
		}
	}

	// EVERY amendment re-runs it, including the kinds that contribute no edge —
	// §11.3 read literally, and see AmendmentCycle for why an amendment refused by
	// a PRE-EXISTING cycle is the correct answer rather than collateral damage.
	if ce := AmendmentCycle(e, a); ce != nil {
		return e, fmt.Errorf("%w: %w", ErrAmendmentCycle, ce)
	}
	// With, and not a local fold: the graph the cycle was checked against and the
	// graph the scheduler runs on next tick must be one definition, or they are
	// two schedulers.
	return e.With(a), nil
}

// taskInRun reports whether an id is a task of the run's own graph. The DECLARED
// set, not the merged one: an amendment cannot introduce a task, so the question
// is always about the manifest the run was started from.
func taskInRun(e EffectiveGraph, id string) bool {
	for _, t := range e.Declared().TaskIDs {
		if t == id {
			return true
		}
	}
	return false
}

// ErrAmendmentNotApproved is §11.3's "NEVER auto-granted" as a refusal rather
// than a convention. It lives here and not with state.go's sentinels because it
// is meaningful only at the acceptance gate, and because the gate and the reason
// it exists should be readable in one place.
var ErrAmendmentNotApproved = errors.New("delegate: amendment has not been approved by a human")
