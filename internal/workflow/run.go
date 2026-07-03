package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
)

// Runner executes workflow runs: Start (step 1), Advance (step N→N+1),
// RetryPendingSeed, and Abandon — all CAS-guarded per spec §2.6.
type Runner struct {
	Store           *store.Store
	Launcher        *session.Launcher
	ClaudeConfigDir string
}

// StepPreview is what a confirm dialog shows before an advance actually
// fires (spec §2.11): substitution already applied, computed at
// confirm-OPEN time (a deterministic local file read, same precedent as
// openDetail elsewhere in the codebase).
type StepPreview struct {
	Label, Relation, Seed string
	Unavailable           bool // any {{prev.*}} token resolved to "(unavailable)"
	Finish                bool // run.StepIdx is already the last step: this is a finish, not an advance
}

// ErrContinueDead is returned by Advance when the next step's relation is
// "continue" (or was forced there) and the current step's session cannot
// be resolved to a live row (spec §2.8). The UI recovery path is a
// one-shot demotion to fork: call Advance again with forceFork=true.
var ErrContinueDead = errors.New("workflow: continue target session is dead")

// ErrRunAdvancedElsewhere is returned when AdvanceRunCAS's compare-and-swap
// is rejected: the caller acted on a stale snapshot — a double-press, a
// second Loom instance, or a run that moved to done/abandoned concurrently
// (spec §2.6). The caller must not retry blindly; it should re-read the run.
var ErrRunAdvancedElsewhere = errors.New("workflow: run advanced elsewhere")

const (
	perValueCap = 8 * 1024
	totalCap    = 15 * 1024
	truncMarker = "…[truncated]"
	unavailable = "(unavailable)"
)

// extractionLike carries just the fields substitute() needs from a
// resolved step's transcript — decoupled from memory.Extraction so it's
// independently constructible in tests without a real transcript file.
type extractionLike struct {
	Outcome   string
	Title     string
	Ask       string
	AskUsable bool // whether Ask passed the real ask filters (memory.AskUsable)
}

// templateSubRe matches only the three whitelisted tokens — anything else
// would already have been rejected at LoadAll time (def.go), so this never
// needs to be the general "any {{...}}" matcher templateTokenRe (def.go) is.
var templateSubRe = regexp.MustCompile(`\{\{prev\.(?:outcome|title|ask)\}\}`)

// substitute fills in {{prev.outcome}}/{{prev.title}}/{{prev.ask}} in seed
// from prev (spec §2.3). Missing/empty values (or an Ask that failed the
// ask-filter rule) render literally as "(unavailable)" — substitution
// never blocks an advance. Each substituted value is capped at 8KB with a
// visible truncation marker; the fully-assembled seed is then hard-capped
// at 15KB the same way. Returns the assembled seed and whether any token
// resolved to unavailable.
//
// Values pulled from prev are already single-line by construction
// (memory.CleanText collapses \n\r\t upstream) — substitute does NOT
// re-collapse them, but defensively strips any stray \n\r anyway before
// they're assembled into the seed: tmux send-keys treats a literal newline
// as pressing Enter, which would submit the seed early/incomplete.
func substitute(seed string, prev extractionLike) (string, bool) {
	hadUnavailable := false
	out := templateSubRe.ReplaceAllStringFunc(seed, func(tok string) string {
		var val string
		var ok bool
		switch tok {
		case "{{prev.outcome}}":
			val, ok = prev.Outcome, prev.Outcome != ""
		case "{{prev.title}}":
			val, ok = prev.Title, prev.Title != ""
		case "{{prev.ask}}":
			val, ok = prev.Ask, prev.Ask != "" && prev.AskUsable
		default:
			return tok // not a recognized token: load-time whitelisting should prevent this; leave as-is defensively
		}
		if !ok {
			hadUnavailable = true
			return unavailable
		}
		val = stripCRLF(val)
		val = truncateBytes(val, perValueCap, truncMarker)
		return val
	})
	out = truncateBytes(out, totalCap, truncMarker)
	return out, hadUnavailable
}

func stripCRLF(s string) string {
	if !containsCRLF(s) {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			continue
		}
		b = append(b, s[i])
	}
	return string(b)
}

func containsCRLF(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return true
		}
	}
	return false
}

// truncateBytes trims s to at most max bytes, appending marker (and
// trimming further to make room for it) when it does. Trims back to a
// valid UTF-8 rune boundary so a truncation never splits a multi-byte
// character.
func truncateBytes(s string, max int, marker string) string {
	if len(s) <= max {
		return s
	}
	cut := max - len(marker)
	if cut < 0 {
		cut = 0
	}
	if cut > len(s) {
		cut = len(s)
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

// isLive mirrors internal/ui/app.go's isLiveRow (spec §2.8: liveness is
// read from the STORE row status, never a tmux query — tmux reaps dead
// panes within a poll).
func isLive(r store.SessionRow) bool {
	return r.LastStatus != "done" && r.LastStatus != "error"
}

// ResolveStepSession resolves run's CURRENT step to a live-or-latest
// SessionRow BY IDENTITY (spec §2.5), never trusting the pinned tmux name
// alone: the pinned name in run.SessionNames[run.StepIdx] is looked up,
// then its ClaudeSessionID is used to find the newest row sharing that
// claude session id via GetLatestByClaudeSessionID. This is what makes a
// dashboard `r`-resume of a dead step session (new tmux name, same claude
// id) transparent to the run — every caller (attach, continue-liveness,
// fork extraction, project inheritance) goes through this, never the
// pinned name directly.
//
// ok=false only when the pinned name has no store row at all (e.g. Launch
// failed after a CAS already recorded the name — the documented accepted
// failure mode, see Start/Advance).
func (r *Runner) ResolveStepSession(run store.RunRow) (store.SessionRow, bool) {
	if run.StepIdx < 0 || int(run.StepIdx) >= len(run.SessionNames) {
		return store.SessionRow{}, false
	}
	pinned := run.SessionNames[run.StepIdx]
	row, ok, err := r.Store.Get(pinned)
	if err != nil || !ok {
		return store.SessionRow{}, false
	}
	if row.ClaudeSessionID == "" {
		return row, true
	}
	if latest, ok2, err2 := r.Store.GetLatestByClaudeSessionID(row.ClaudeSessionID); err2 == nil && ok2 {
		return latest, true
	}
	return row, true
}

// extractPrev builds the substitution source from row's transcript (spec
// §2.3/§2.5): the fork-extraction machinery, reused unchanged for
// "prev" on every relation, including continue ("{{prev.*}} on continue =
// same session's extraction — well-defined", spec §2.2). Never errors
// outward: a missing/unreadable transcript just yields an all-empty
// extractionLike, which substitute() renders as "(unavailable)" everywhere
// — extraction failures never block an advance.
func (r *Runner) extractPrev(row store.SessionRow) extractionLike {
	path := transcript.Path(r.ClaudeConfigDir, row.Cwd, row.ClaudeSessionID)
	ex, err := memory.ExtractFile(path, "")
	if err != nil {
		return extractionLike{}
	}
	return extractionLike{Outcome: ex.Outcome, Title: ex.Title, Ask: ex.Ask, AskUsable: memory.AskUsable(ex.Ask)}
}

func parseDef(defJSON string) (Definition, error) {
	var d Definition
	if err := json.Unmarshal([]byte(defJSON), &d); err != nil {
		return Definition{}, fmt.Errorf("corrupt run definition snapshot: %w", err)
	}
	return d, nil
}

// Preview computes what Advance would do next, for a confirm dialog (spec
// §2.11): substitution runs NOW (confirm-open time), not at press time. If
// run is already at its last step, Finish=true and no substitution is
// attempted (the UI's finish confirm just needs the run's own Name, from
// RunRow, not a StepPreview).
func (r *Runner) Preview(run store.RunRow) (StepPreview, error) {
	def, err := parseDef(run.DefJSON)
	if err != nil {
		return StepPreview{}, err
	}
	if int(run.StepIdx) >= len(def.Steps)-1 {
		return StepPreview{Finish: true}, nil
	}
	next := def.Steps[run.StepIdx+1]

	var prev extractionLike
	if curRow, found := r.ResolveStepSession(run); found {
		prev = r.extractPrev(curRow)
	}
	seed, hadUnavailable := substitute(next.Seed, prev)
	return StepPreview{Label: next.Label, Relation: next.Relation, Seed: seed, Unavailable: hadUnavailable}, nil
}

func appendName(names []string, name string) []string {
	out := make([]string, len(names)+1)
	copy(out, names)
	out[len(names)] = name
	return out
}

// Start launches step 1 of def as a brand-new run (spec §2.10). Order:
// InsertRun (the run id must exist before the step-1 tag can be built) →
// Launch → SetTags → AdvanceRunCAS(id,0,0,[name],""). If the CAS is
// rejected here (claimed=false) — which should never happen for a
// brand-new run's very first write, but is surfaced rather than assumed
// impossible — the session exists (and is tagged) but the run row never
// records it; documented accepted failure mode, matching the one Advance
// accepts for fork/fresh (see Advance's doc comment).
func (r *Runner) Start(def Definition, w, h int, now time.Time) (int64, error) {
	if len(def.Steps) == 0 {
		return 0, errors.New("workflow: definition has no steps")
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		return 0, err
	}
	runID, err := r.Store.InsertRun(def.Name, string(defJSON), now.Unix())
	if err != nil {
		return 0, err
	}

	step1 := def.Steps[0]
	// Step 1's relation is always ignored (always fresh, spec §2.2); there
	// is no previous step to substitute from.
	seed, _ := substitute(step1.Seed, extractionLike{})
	recipe := session.Recipe{
		ProjectLabel: filepath.Base(step1.Project),
		Cwd:          step1.Project,
		Model:        step1.Model,
		Mode:         step1.Mode,
		Seed:         seed,
	}
	name, err := r.Launcher.Launch(recipe, w, h, now)
	if err != nil {
		return runID, fmt.Errorf("workflow: run %d: step 1 launch failed: %w", runID, err)
	}
	tag := fmt.Sprintf("wf:%s#%d:step1", def.Name, runID)
	if err := r.Store.SetTags(name, tag); err != nil {
		return runID, fmt.Errorf("workflow: run %d: step 1 session %s launched but tagging failed: %w", runID, name, err)
	}
	claimed, err := r.Store.AdvanceRunCAS(runID, 0, 0, []string{name}, "", now.Unix())
	if err != nil {
		return runID, err
	}
	if !claimed {
		return runID, fmt.Errorf("workflow: run %d: step 1 session %s launched but the run row was not updated (CAS rejected) — accepted failure mode, see Start's doc comment", runID, name)
	}
	return runID, nil
}

// resolveCwdLabel resolves the cwd/label for a fork/fresh step. A
// non-empty step.Project already holds a resolved absolute path (baked in
// by LoadAll — see Step's doc comment); an empty Project means "inherit
// the resolved previous step's cwd/label" (spec §2.12), read from the
// previous step's resolved SessionRow, not re-looked-up in any registry
// (Runner has none).
func resolveCwdLabel(step Step, curRow store.SessionRow, found bool) (cwd, label string, err error) {
	if step.Project != "" {
		return step.Project, filepath.Base(step.Project), nil
	}
	if !found {
		return "", "", errors.New("workflow: cannot resolve project: step names none and the previous step's session is unresolved")
	}
	return curRow.Cwd, curRow.ProjectLabel, nil
}

// Advance moves run from its current step to the next one (spec §2.2/
// §2.6/§2.8/§2.9). forceFork is the §2.8 demotion: when the UI is
// recovering from a just-returned ErrContinueDead, it calls Advance again
// with forceFork=true to force THIS ONE advance to "fork" regardless of
// the definition's natural relation for that step.
//
// Ordering:
//   - continue: CAS FIRST (claiming the step with pending_seed set to the
//     substituted seed), THEN an async goroutine delivers it once the
//     session's transcript state is NeedsYou/Idle (spec §2.9) — no launch
//     is involved, so "CAS before any launch" is trivially satisfied.
//   - fork/fresh: Launch (its own gated, pane-marker-based seeding is
//     already safe pre-mount, spec §2.9) happens before the CAS, because
//     Launcher.Launch mints its own session id internally — there is no
//     API to hand it a pre-chosen one. This mirrors Start's own
//     Launch-then-CAS order and accepts the SAME failure mode: if the CAS
//     is rejected after a successful Launch (only possible via a genuine
//     double-press/two-instance race that the UI's in-flight guard and
//     fresh-snapshot re-check are meant to make rare), the new session is
//     real but unrecorded on this run — ResolveStepSession for that step
//     then reports not-found and the run shows its dead-step hint. This is
//     a disclosed deviation from a stricter "mint name via
//     session.NewSessionID/TmuxName before Launch" reading of the plan;
//     see the Task 2 report for the full rationale.
func (r *Runner) Advance(run store.RunRow, forceFork bool, w, h int, now time.Time) error {
	def, err := parseDef(run.DefJSON)
	if err != nil {
		return err
	}
	if int(run.StepIdx) >= len(def.Steps)-1 {
		return errors.New("workflow: run is already at its last step (finish, don't advance)")
	}
	nextIdx := run.StepIdx + 1
	next := def.Steps[nextIdx]
	relation := next.Relation
	if forceFork {
		relation = "fork"
	}

	curRow, found := r.ResolveStepSession(run)
	var prev extractionLike
	if found {
		prev = r.extractPrev(curRow)
	}

	switch relation {
	case "continue":
		if !found || !isLive(curRow) {
			return ErrContinueDead
		}
		seed, _ := substitute(next.Seed, prev)
		names := appendName(run.SessionNames, curRow.Name)
		claimed, err := r.Store.AdvanceRunCAS(run.ID, run.StepIdx, nextIdx, names, seed, now.Unix())
		if err != nil {
			return err
		}
		if !claimed {
			return ErrRunAdvancedElsewhere
		}
		runID := run.ID
		go func() {
			updated, ok, err := r.Store.GetRun(runID)
			if err != nil || !ok {
				return
			}
			_ = r.sendPendingSeed(updated)
		}()
		return nil

	case "fork", "fresh":
		cwd, label, err := resolveCwdLabel(next, curRow, found)
		if err != nil {
			return err
		}
		seed, _ := substitute(next.Seed, prev) // templates substituted for ALL relations, spec §2.2
		recipe := session.Recipe{ProjectLabel: label, Cwd: cwd, Model: next.Model, Mode: next.Mode, Seed: seed}
		name, err := r.Launcher.Launch(recipe, w, h, now)
		if err != nil {
			return fmt.Errorf("workflow: run %d: step %d launch failed: %w", run.ID, nextIdx+1, err)
		}
		tag := fmt.Sprintf("wf:%s#%d:step%d", run.Name, run.ID, nextIdx+1)
		if err := r.Store.SetTags(name, tag); err != nil {
			return fmt.Errorf("workflow: run %d: step %d session %s launched but tagging failed: %w", run.ID, nextIdx+1, name, err)
		}
		names := appendName(run.SessionNames, name)
		claimed, err := r.Store.AdvanceRunCAS(run.ID, run.StepIdx, nextIdx, names, "", now.Unix())
		if err != nil {
			return err
		}
		if !claimed {
			return fmt.Errorf("%w: run %d step %d session %s launched but the run row was not updated (accepted failure mode, see Advance's doc comment)",
				ErrRunAdvancedElsewhere, run.ID, nextIdx+1, name)
		}
		return nil

	default:
		return fmt.Errorf("workflow: unknown relation %q", relation)
	}
}

// waitForContinueGate polls path (the current step's transcript) until its
// classifier state reaches NeedsYou or Idle, or a bounded timeout elapses
// (spec §2.9: "the ❯ glyph is meaningless mid-generation" — gate on the
// engine's transcript-derived state, not a raw pane read). Reuses
// Launcher's PollEvery/ReadyTimeout fields (defaulting the same way
// Launcher itself does) rather than adding Runner-local knobs, since
// Runner's field set is fixed by the Produces contract.
func (r *Runner) waitForContinueGate(row store.SessionRow) bool {
	path := transcript.Path(r.ClaudeConfigDir, row.Cwd, row.ClaudeSessionID)
	rdr := transcript.NewReader(path)
	poll := r.Launcher.PollEvery
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	timeout := r.Launcher.ReadyTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	waited := time.Duration(0)
	for {
		snap, err := rdr.Poll()
		if err == nil && (snap.State == transcript.StateNeedsYou || snap.State == transcript.StateIdle) {
			return true
		}
		if waited >= timeout {
			return false
		}
		time.Sleep(poll)
		waited += poll
	}
}

// sendPendingSeed delivers run's PendingSeed into its current step's
// session, gated on transcript state, and clears PendingSeed on success
// (spec §2.9). Shared by Advance's continue path (best-effort, run
// asynchronously — its error is intentionally dropped, the pending_seed
// column is the durable record of "still owed") and RetryPendingSeed
// (synchronous, error returned to the caller).
func (r *Runner) sendPendingSeed(run store.RunRow) error {
	if run.PendingSeed == "" {
		return nil
	}
	curRow, found := r.ResolveStepSession(run)
	if !found || !isLive(curRow) {
		return ErrContinueDead
	}
	if !r.waitForContinueGate(curRow) {
		return fmt.Errorf("workflow: run %d: session not yet idle/needs-you — seed still pending", run.ID)
	}
	if err := r.Launcher.Tmux.SendLiteral(curRow.Name, run.PendingSeed); err != nil {
		return err
	}
	if err := r.Launcher.Tmux.SendEnter(curRow.Name); err != nil {
		return err
	}
	return r.Store.ClearPendingSeed(run.ID, time.Now().Unix())
}

// RetryPendingSeed re-attempts delivery of run.PendingSeed (spec §2.9: "a
// Loom restart with a non-empty pending_seed renders the run as `seed
// pending` and `n` retries delivery instead of advancing"). A dead current
// session is reported as ErrContinueDead, same as a fresh continue would.
func (r *Runner) RetryPendingSeed(run store.RunRow) error {
	return r.sendPendingSeed(run)
}

// Finish marks run done via the CAS gate (spec §2.7): the UI's finishCmd
// already re-reads the run fresh before calling this, but that pre-read and
// the actual write are two separate moments — the write itself must be
// conditioned on the SAME snapshot (run.StepIdx, status='running'), not
// applied unconditionally after the fact, or an advance/second-finish
// landing in between would be silently clobbered. claimed=false surfaces
// ErrRunAdvancedElsewhere, the same signal a rejected AdvanceRunCAS gives:
// the caller acted on a stale snapshot and must not retry blindly.
func (r *Runner) Finish(run store.RunRow, now time.Time) error {
	claimed, err := r.Store.FinishRunCAS(run.ID, run.StepIdx, now.Unix())
	if err != nil {
		return err
	}
	if !claimed {
		return ErrRunAdvancedElsewhere
	}
	return nil
}

// Abandon marks run abandoned (spec §2.12: "Abandon ≠ kill" — the step's
// session is left running untouched; only the run's bookkeeping changes).
// The write is conditioned on status='running' (AbandonRunCAS) rather than
// the old unconditional SetRunStatus: abandon-vs-finish is a real race (the
// confirm was opened against a running snapshot, but the run finished
// before 'y' landed) and must not silently overwrite 'done' back to
// 'abandoned'. abandon-vs-ABANDON is harmless — re-abandoning an
// already-abandoned run stays idempotent (nil error) — so on
// claimed=false this re-reads the row to tell the two apart: already
// abandoned is a no-op, already done is a mild error, never a silent
// overwrite.
func (r *Runner) Abandon(run store.RunRow, now time.Time) error {
	claimed, err := r.Store.AbandonRunCAS(run.ID, now.Unix())
	if err != nil {
		return err
	}
	if claimed {
		return nil
	}
	fresh, ok, err := r.Store.GetRun(run.ID)
	if err != nil {
		return err
	}
	if !ok || fresh.Status == "abandoned" {
		return nil // already abandoned elsewhere — idempotent, not an error
	}
	return fmt.Errorf("workflow: run %d: cannot abandon — already %s", run.ID, fresh.Status)
}
