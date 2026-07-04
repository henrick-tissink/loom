package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
)

// sinkCatScript is used for hand-built tmux sessions (continue-relation
// tests, CAS-race tests) where the test controls NewSession's shellCmd
// directly and so CAN pass a positional sink path — no PATH injection
// needed for these.
const sinkCatScript = "#!/bin/sh\nexec cat > \"$1\"\n"

// testRunner builds a Runner backed by a throwaway tmux socket (killed at
// t.Cleanup) and a PATH-injected fake `claude` binary standing in for the
// real one: Runner.Start and Advance's fork/fresh path shell out to
// `claude --session-id ...` via session.Recipe/Launcher.Launch, and there
// is no seam to substitute a different binary name or hand it a
// positional sink argument, so the fake is found via PATH instead — the
// same technique Task 4's e2e plan uses.
//
// The fake's sink path is baked into the SCRIPT'S OWN CONTENT at creation
// time here, rather than passed via an environment variable set right
// before the launch that needs it: tmux new-session sessions inherit the
// SERVER's global environment table (fixed at server start / via an
// explicit `set-environment -g`, verified empirically against tmux
// 3.7b) — NOT the live os.Environ() of whatever client process happens to
// invoke `new-session` — so a t.Setenv done between EnsureServer and a
// later Launch call is invisible to the pane. Baking the path into the
// script sidesteps that entirely. The fake prints the bare ready marker
// immediately (no trust dialog: that machinery is session package's own
// concern, already tested there) then captures whatever is sent into it
// (the seed) to sink.
func testRunner(t *testing.T) (r *Runner, ccd, sink string) {
	t.Helper()
	binDir := t.TempDir()
	sink = filepath.Join(t.TempDir(), "sink.txt")
	script := filepath.Join(binDir, "claude")
	content := fmt.Sprintf("#!/bin/sh\necho \"❯\"\nexec cat > %q\n", sink)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tm := &tmux.Client{Socket: fmt.Sprintf("loomwf%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ccd = t.TempDir()
	l := &session.Launcher{
		Tmux: tm, Store: st, ClaudeConfigDir: ccd,
		ClaudeJSONPath: filepath.Join(t.TempDir(), ".claude.json"),
		ReadyMarker:    session.DefaultReadyMarker,
		TrustMarker:    session.DefaultTrustMarker,
		ReadyTimeout:   5 * time.Second,
		PollEvery:      50 * time.Millisecond,
	}
	return &Runner{Store: st, Launcher: l, ClaudeConfigDir: ccd}, ccd, sink
}

func writeTranscript(t *testing.T, ccd, cwd, sessionID string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(ccd, "projects", projectDirNameForTest(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, sessionID+".jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// projectDirNameForTest delegates to the real transcript.ProjectDirName so
// this test fixture can never drift from the encoding it's meant to mirror.
func projectDirNameForTest(cwd string) string {
	return transcript.ProjectDirName(cwd)
}

// stdFixtureLines is a minimal transcript: a user prompt, a final
// assistant text reply (no tool_use — classifies as NeedsYou, extracts as
// Outcome="Plan complete: build X then Y."), and an ai-title sidecar
// (Title="Plan Step"). Ask extracts to "do the plan".
func stdFixtureLines(cwd string) []string {
	return []string{
		fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"do the plan"},"cwd":%q,"timestamp":"2026-07-03T00:00:00Z"}`, cwd),
		fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Plan complete: build X then Y."}]},"cwd":%q,"timestamp":"2026-07-03T00:00:05Z"}`, cwd),
		`{"type":"ai-title","aiTitle":"Plan Step"}`,
	}
}

// --- substitute() (spec §2.3) ---------------------------------------

func TestSubstituteWhitelistedTokensReplaced(t *testing.T) {
	prev := extractionLike{Outcome: "did the thing", Title: "My Title", Ask: "please do X", AskUsable: true}
	got, had := substitute("Prior: {{prev.outcome}} / {{prev.title}} / {{prev.ask}}", prev)
	want := "Prior: did the thing / My Title / please do X"
	if got != want || had {
		t.Fatalf("got %q had=%v, want %q false", got, had, want)
	}
}

func TestSubstituteMissingValuesRenderUnavailable(t *testing.T) {
	got, had := substitute("{{prev.outcome}}", extractionLike{})
	if got != unavailable || !had {
		t.Fatalf("got %q had=%v, want %q true", got, had, unavailable)
	}
}

func TestSubstituteAskFilteredWhenNotUsableEvenIfNonEmpty(t *testing.T) {
	prev := extractionLike{Ask: "<command-name>/foo</command-name>", AskUsable: false}
	got, had := substitute("{{prev.ask}}", prev)
	if got != unavailable || !had {
		t.Fatalf("got %q had=%v, want %q true (ask-filter rule)", got, had, unavailable)
	}
}

func TestSubstitutePerValueCapTruncatesWithMarker(t *testing.T) {
	long := strings.Repeat("a", 9000)
	prev := extractionLike{Outcome: long}
	got, had := substitute("{{prev.outcome}}", prev)
	if had {
		t.Fatal("a present (if over-cap) value must not count as unavailable")
	}
	if !strings.HasSuffix(got, truncMarker) {
		t.Fatalf("missing truncation marker, suffix = %q", got[len(got)-30:])
	}
	if len(got) != perValueCap {
		t.Fatalf("len(got) = %d, want exactly perValueCap=%d", len(got), perValueCap)
	}
}

func TestSubstituteTotalCapTruncatesAssembledSeed(t *testing.T) {
	long := strings.Repeat("b", 8000) // under the 8KB per-value cap individually
	seed := "{{prev.outcome}} {{prev.title}} {{prev.ask}}"
	prev := extractionLike{Outcome: long, Title: long, Ask: long, AskUsable: true}
	got, _ := substitute(seed, prev)
	if len(got) != totalCap {
		t.Fatalf("len(got) = %d, want exactly totalCap=%d", len(got), totalCap)
	}
	if !strings.HasSuffix(got, truncMarker) {
		t.Fatal("missing total-cap truncation marker")
	}
}

func TestSubstituteStripsCRLFDefensively(t *testing.T) {
	prev := extractionLike{Outcome: "line one\nline two\r\n"}
	got, _ := substitute("{{prev.outcome}}", prev)
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("got %q, want CR/LF stripped defensively before send", got)
	}
}

// --- ResolveStepSession (spec §2.5 identity resolution) --------------

func TestResolveStepSessionNotFoundWhenPinnedNameMissing(t *testing.T) {
	r, _, _ := testRunner(t)
	run := store.RunRow{ID: 1, StepIdx: 0, SessionNames: []string{"loom-ghost"}}
	if _, ok := r.ResolveStepSession(run); ok {
		t.Fatal("expected not found for a pinned name with no store row")
	}
}

func TestResolveStepSessionIdentityAfterSimulatedResume(t *testing.T) {
	r, _, _ := testRunner(t)
	claudeID := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	old := store.SessionRow{Name: "loom-old", ClaudeSessionID: claudeID, ProjectLabel: "p", Cwd: "/w/p",
		CreatedAt: 1, EndedAt: 5, ExitCode: 0, LastStatus: "done"}
	if err := r.Store.Upsert(old); err != nil {
		t.Fatal(err)
	}
	newer := store.SessionRow{Name: "loom-new", ClaudeSessionID: claudeID, ProjectLabel: "p", Cwd: "/w/p",
		CreatedAt: 2, EndedAt: -1, ExitCode: -1, LastStatus: "idle"}
	if err := r.Store.Upsert(newer); err != nil {
		t.Fatal(err)
	}

	run := store.RunRow{ID: 1, StepIdx: 0, SessionNames: []string{"loom-old"}}
	got, ok := r.ResolveStepSession(run)
	if !ok {
		t.Fatal("expected found")
	}
	if got.Name != "loom-new" {
		t.Fatalf("resolved Name = %q, want loom-new (the resumed row, by identity)", got.Name)
	}
	if !isLive(got) {
		t.Fatal("resolved row must be the live resumed one, not the dead pinned one")
	}
}

// --- Preview (spec §2.11) --------------------------------------------

func TestPreviewFinishAtLastStep(t *testing.T) {
	r, _, _ := testRunner(t)
	def := Definition{Name: "wf", Steps: []Step{{Label: "a", Project: "/w/p", Relation: "fresh", Seed: "x"}}}
	defJSON, _ := json.Marshal(def)
	run := store.RunRow{ID: 1, Name: "wf", DefJSON: string(defJSON), StepIdx: 0, SessionNames: []string{"loom-a"}}
	pv, err := r.Preview(run)
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Finish {
		t.Fatalf("Preview = %+v, want Finish=true (single-step def already at last step)", pv)
	}
}

func TestPreviewSubstitutesFromResolvedPrevSessionAndFlagsAvailable(t *testing.T) {
	r, ccd, _ := testRunner(t)
	claudeID := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	cwd := t.TempDir()
	writeTranscript(t, ccd, cwd, claudeID, stdFixtureLines(cwd)...)
	if err := r.Store.Upsert(store.SessionRow{Name: "loom-step1", ClaudeSessionID: claudeID, ProjectLabel: "p", Cwd: cwd,
		CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "idle"}); err != nil {
		t.Fatal(err)
	}

	def := Definition{Name: "wf", Steps: []Step{
		{Label: "a", Project: cwd, Relation: "fresh", Seed: "x"},
		{Label: "b", Relation: "fork", Seed: "Prior: {{prev.outcome}} asked {{prev.ask}}"},
	}}
	defJSON, _ := json.Marshal(def)
	run := store.RunRow{ID: 1, Name: "wf", DefJSON: string(defJSON), StepIdx: 0, SessionNames: []string{"loom-step1"}}

	pv, err := r.Preview(run)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Finish {
		t.Fatal("must not be Finish: two steps, currently at step 1")
	}
	if pv.Label != "b" || pv.Relation != "fork" {
		t.Fatalf("pv = %+v", pv)
	}
	if pv.Unavailable {
		t.Fatalf("pv.Unavailable = true, want false: both outcome and ask should resolve")
	}
	if !strings.Contains(pv.Seed, "Plan complete: build X then Y.") || !strings.Contains(pv.Seed, "do the plan") {
		t.Fatalf("Seed = %q, want both tokens substituted", pv.Seed)
	}
}

// --- Start (spec §2.10) ------------------------------------------------

func TestStartLaunchesStep1TagsAndRecordsCAS(t *testing.T) {
	r, _, sink := testRunner(t)
	cwd := t.TempDir()
	def := Definition{Name: "wf1", Steps: []Step{
		{Label: "plan", Project: cwd, Model: "opus", Mode: "plan", Relation: "fresh", Seed: "Plan it."},
	}}

	runID, err := r.Start(def, 80, 24, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	run, ok, err := r.Store.GetRun(runID)
	if err != nil || !ok {
		t.Fatalf("GetRun: %v %v", ok, err)
	}
	if run.StepIdx != 0 || len(run.SessionNames) != 1 {
		t.Fatalf("run = %+v, want StepIdx=0 len(SessionNames)=1 (invariant)", run)
	}
	if run.Status != "running" {
		t.Fatalf("Status = %q, want running", run.Status)
	}

	row, ok, err := r.Store.Get(run.SessionNames[0])
	if err != nil || !ok {
		t.Fatalf("session row: %v %v", ok, err)
	}
	wantTag := fmt.Sprintf("wf:wf1#%d:step1", runID)
	if row.Tags != wantTag {
		t.Fatalf("Tags = %q, want %q", row.Tags, wantTag)
	}
	if row.Model != "opus" || row.Mode != "plan" {
		t.Fatalf("row = %+v", row)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(sink)
		if string(b) == "Plan it.\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink = %q, want seed delivered", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- Advance: fork/fresh (spec §2.2/§2.6/§2.9) --------------------------

func TestAdvanceForkLaunchesNewSessionWithSubstitutedSeedAndInvariantHolds(t *testing.T) {
	r, ccd, sink := testRunner(t)
	prevCwd := t.TempDir()
	claudeID := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	writeTranscript(t, ccd, prevCwd, claudeID, stdFixtureLines(prevCwd)...)
	if err := r.Store.Upsert(store.SessionRow{Name: "loom-step1", ClaudeSessionID: claudeID, ProjectLabel: "p", Cwd: prevCwd,
		CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "idle"}); err != nil {
		t.Fatal(err)
	}

	def := Definition{Name: "wf2", Steps: []Step{
		{Label: "plan", Project: prevCwd, Relation: "fresh", Seed: "x"},
		{Label: "execute", Relation: "fork", Seed: "Go: {{prev.outcome}}"},
	}}
	defJSON, _ := json.Marshal(def)
	runID, err := r.Store.InsertRun("wf2", string(defJSON), 100)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := r.Store.AdvanceRunCAS(runID, 0, 0, []string{"loom-step1"}, "", 100); err != nil || !claimed {
		t.Fatalf("seed CAS: claimed=%v err=%v", claimed, err)
	}
	run, _, _ := r.Store.GetRun(runID)

	if err := r.Advance(run, false, 80, 24, time.Now()); err != nil {
		t.Fatal(err)
	}

	updated, _, _ := r.Store.GetRun(runID)
	if updated.StepIdx != 1 || len(updated.SessionNames) != 2 {
		t.Fatalf("updated = %+v, want StepIdx=1 len(SessionNames)=2 (invariant)", updated)
	}
	if updated.PendingSeed != "" {
		t.Fatalf("PendingSeed = %q, want empty for fork/fresh", updated.PendingSeed)
	}
	row, ok, _ := r.Store.Get(updated.SessionNames[1])
	if !ok {
		t.Fatal("new step-2 session row missing")
	}
	wantTag := fmt.Sprintf("wf:wf2#%d:step2", runID)
	if row.Tags != wantTag {
		t.Fatalf("Tags = %q, want %q", row.Tags, wantTag)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(sink)
		if strings.Contains(string(b), "Plan complete: build X then Y.") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink = %q, want substituted outcome", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAdvanceForkWithNoProjectAndUnresolvedPrevReturnsError(t *testing.T) {
	r, _, _ := testRunner(t)
	def := Definition{Name: "wf11", Steps: []Step{
		{Label: "a", Project: "/w/p", Relation: "fresh", Seed: "x"},
		{Label: "b", Relation: "fork", Seed: "y"},
	}}
	defJSON, _ := json.Marshal(def)
	runID, _ := r.Store.InsertRun("wf11", string(defJSON), 100)
	// Pin a name with NO backing store row: ResolveStepSession returns not-found.
	if _, err := r.Store.AdvanceRunCAS(runID, 0, 0, []string{"loom-neverexisted"}, "", 100); err != nil {
		t.Fatal(err)
	}
	run, _, _ := r.Store.GetRun(runID)

	if err := r.Advance(run, false, 80, 24, time.Now()); err == nil {
		t.Fatal("expected error: step 2 names no project and the previous step's session is unresolved")
	}
}

func TestAdvanceAtLastStepReturnsError(t *testing.T) {
	r, _, _ := testRunner(t)
	def := Definition{Name: "wf10", Steps: []Step{{Label: "a", Project: "/w/p", Relation: "fresh", Seed: "x"}}}
	defJSON, _ := json.Marshal(def)
	runID, _ := r.Store.InsertRun("wf10", string(defJSON), 100)
	if _, err := r.Store.AdvanceRunCAS(runID, 0, 0, []string{"loom-a"}, "", 100); err != nil {
		t.Fatal(err)
	}
	run, _, _ := r.Store.GetRun(runID)
	if err := r.Advance(run, false, 80, 24, time.Now()); err == nil {
		t.Fatal("expected an error advancing past the last step")
	}
}

// --- Advance: continue (spec §2.8/§2.9) ---------------------------------

func TestAdvanceContinueDeliversPendingSeedGatedOnTranscriptStateThenClears(t *testing.T) {
	r, ccd, _ := testRunner(t)
	cwd := t.TempDir()
	claudeID := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	writeTranscript(t, ccd, cwd, claudeID, stdFixtureLines(cwd)...) // tail = assistant end_turn -> NeedsYou

	name := "loom-continue1"
	scriptPath := filepath.Join(t.TempDir(), "sink-cat.sh")
	if err := os.WriteFile(scriptPath, []byte(sinkCatScript), 0o755); err != nil {
		t.Fatal(err)
	}
	sink := filepath.Join(t.TempDir(), "sink.txt")
	if err := r.Launcher.Tmux.NewSession(name, cwd, "'"+scriptPath+"' '"+sink+"'", 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Upsert(store.SessionRow{Name: name, ClaudeSessionID: claudeID, ProjectLabel: "p", Cwd: cwd,
		CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "idle"}); err != nil {
		t.Fatal(err)
	}

	def := Definition{Name: "wf3", Steps: []Step{
		{Label: "a", Project: cwd, Relation: "fresh", Seed: "x"},
		{Label: "b", Relation: "continue", Seed: "continue please"},
	}}
	defJSON, _ := json.Marshal(def)
	runID, _ := r.Store.InsertRun("wf3", string(defJSON), 100)
	if _, err := r.Store.AdvanceRunCAS(runID, 0, 0, []string{name}, "", 100); err != nil {
		t.Fatal(err)
	}
	run, _, _ := r.Store.GetRun(runID)

	if err := r.Advance(run, false, 80, 24, time.Now()); err != nil {
		t.Fatal(err)
	}

	// The CAS claim (with pending_seed set) must already be visible
	// immediately after Advance returns — persisted, not fire-and-forget
	// (spec §2.9).
	justAfter, _, _ := r.Store.GetRun(runID)
	if justAfter.StepIdx != 1 || len(justAfter.SessionNames) != 2 || justAfter.SessionNames[1] != name {
		t.Fatalf("justAfter = %+v, want StepIdx=1 SessionNames=[.. %s] (continue reuses the same session name, invariant holds)", justAfter, name)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		updated, _, _ := r.Store.GetRun(runID)
		if updated.PendingSeed == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("PendingSeed never cleared: %+v", updated)
		}
		time.Sleep(50 * time.Millisecond)
	}
	deadline2 := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(sink)
		if strings.Contains(string(b), "continue please") {
			break
		}
		if time.Now().After(deadline2) {
			t.Fatalf("sink = %q, want delivered seed", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAdvanceContinueOnDeadSessionReturnsErrContinueDead(t *testing.T) {
	r, _, _ := testRunner(t)
	name := "loom-dead1"
	if err := r.Store.Upsert(store.SessionRow{Name: name, ClaudeSessionID: "aaaa", ProjectLabel: "p", Cwd: "/w/p",
		CreatedAt: 1, EndedAt: 5, ExitCode: 0, LastStatus: "done"}); err != nil {
		t.Fatal(err)
	}

	def := Definition{Name: "wf4", Steps: []Step{
		{Label: "a", Project: "/w/p", Relation: "fresh", Seed: "x"},
		{Label: "b", Relation: "continue", Seed: "y"},
	}}
	defJSON, _ := json.Marshal(def)
	runID, _ := r.Store.InsertRun("wf4", string(defJSON), 100)
	if _, err := r.Store.AdvanceRunCAS(runID, 0, 0, []string{name}, "", 100); err != nil {
		t.Fatal(err)
	}
	run, _, _ := r.Store.GetRun(runID)

	err := r.Advance(run, false, 80, 24, time.Now())
	if !errors.Is(err, ErrContinueDead) {
		t.Fatalf("err = %v, want ErrContinueDead", err)
	}
	after, _, _ := r.Store.GetRun(runID)
	if after.StepIdx != 0 {
		t.Fatalf("StepIdx = %d, want unchanged 0 (no advance on a dead-continue refusal)", after.StepIdx)
	}
}

func TestAdvanceForceForkDemotesDeadContinueToFork(t *testing.T) {
	r, ccd, sink := testRunner(t)
	cwd := t.TempDir()
	claudeID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	writeTranscript(t, ccd, cwd, claudeID, stdFixtureLines(cwd)...)
	name := "loom-deadfork1"
	if err := r.Store.Upsert(store.SessionRow{Name: name, ClaudeSessionID: claudeID, ProjectLabel: "p", Cwd: cwd,
		CreatedAt: 1, EndedAt: 5, ExitCode: 0, LastStatus: "done"}); err != nil {
		t.Fatal(err)
	}

	def := Definition{Name: "wf5", Steps: []Step{
		{Label: "a", Project: cwd, Relation: "fresh", Seed: "x"},
		{Label: "b", Relation: "continue", Seed: "Go: {{prev.outcome}}"},
	}}
	defJSON, _ := json.Marshal(def)
	runID, _ := r.Store.InsertRun("wf5", string(defJSON), 100)
	if _, err := r.Store.AdvanceRunCAS(runID, 0, 0, []string{name}, "", 100); err != nil {
		t.Fatal(err)
	}
	run, _, _ := r.Store.GetRun(runID)

	if err := r.Advance(run, true, 80, 24, time.Now()); err != nil {
		t.Fatalf("forceFork Advance: %v", err)
	}
	updated, _, _ := r.Store.GetRun(runID)
	if updated.StepIdx != 1 || len(updated.SessionNames) != 2 {
		t.Fatalf("updated = %+v, want StepIdx=1 len(SessionNames)=2", updated)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(sink)
		if strings.Contains(string(b), "Plan complete: build X then Y.") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink = %q, want substituted fork seed (demoted from continue)", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestAdvanceCASRaceOnlyOneCallerClaimsSameSnapshot is THE CAS proof for
// the Runner (spec §2.6/§5): two Advance calls fire from the identical
// snapshot; exactly one must claim (nil error), the other must be
// rejected (ErrRunAdvancedElsewhere), and the run must end up advanced
// exactly once (invariant intact).
func TestAdvanceCASRaceOnlyOneCallerClaimsSameSnapshot(t *testing.T) {
	r, ccd, _ := testRunner(t)
	cwd := t.TempDir()
	claudeID := "11111111-1111-1111-1111-111111111111"
	writeTranscript(t, ccd, cwd, claudeID, stdFixtureLines(cwd)...)
	name := "loom-race1"
	scriptPath := filepath.Join(t.TempDir(), "sink-cat.sh")
	if err := os.WriteFile(scriptPath, []byte(sinkCatScript), 0o755); err != nil {
		t.Fatal(err)
	}
	sink := filepath.Join(t.TempDir(), "sink.txt")
	if err := r.Launcher.Tmux.NewSession(name, cwd, "'"+scriptPath+"' '"+sink+"'", 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Upsert(store.SessionRow{Name: name, ClaudeSessionID: claudeID, ProjectLabel: "p", Cwd: cwd,
		CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "idle"}); err != nil {
		t.Fatal(err)
	}

	def := Definition{Name: "wf6", Steps: []Step{
		{Label: "a", Project: cwd, Relation: "fresh", Seed: "x"},
		{Label: "b", Relation: "continue", Seed: "y"},
	}}
	defJSON, _ := json.Marshal(def)
	runID, _ := r.Store.InsertRun("wf6", string(defJSON), 100)
	if _, err := r.Store.AdvanceRunCAS(runID, 0, 0, []string{name}, "", 100); err != nil {
		t.Fatal(err)
	}
	run, _, _ := r.Store.GetRun(runID)

	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- r.Advance(run, false, 80, 24, time.Now()) }()
	}
	e1, e2 := <-results, <-results

	nilCount, rejectedCount := 0, 0
	for _, e := range []error{e1, e2} {
		switch {
		case e == nil:
			nilCount++
		case errors.Is(e, ErrRunAdvancedElsewhere):
			rejectedCount++
		default:
			t.Fatalf("unexpected error: %v", e)
		}
	}
	if nilCount != 1 || rejectedCount != 1 {
		t.Fatalf("nil=%d rejected=%d (e1=%v e2=%v), want exactly one claim", nilCount, rejectedCount, e1, e2)
	}

	final, _, _ := r.Store.GetRun(runID)
	if final.StepIdx != 1 || len(final.SessionNames) != 2 {
		t.Fatalf("final = %+v, want StepIdx=1 len(SessionNames)=2 (invariant, exactly one advance applied)", final)
	}
}

// --- RetryPendingSeed (spec §2.9) ---------------------------------------

func TestRetryPendingSeedDeliversWhenTranscriptAlreadyIdleThenClears(t *testing.T) {
	r, ccd, _ := testRunner(t)
	cwd := t.TempDir()
	claudeID := "22222222-2222-2222-2222-222222222222"
	writeTranscript(t, ccd, cwd, claudeID, stdFixtureLines(cwd)...)
	name := "loom-retry1"
	scriptPath := filepath.Join(t.TempDir(), "sink-cat.sh")
	if err := os.WriteFile(scriptPath, []byte(sinkCatScript), 0o755); err != nil {
		t.Fatal(err)
	}
	sink := filepath.Join(t.TempDir(), "sink.txt")
	if err := r.Launcher.Tmux.NewSession(name, cwd, "'"+scriptPath+"' '"+sink+"'", 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Upsert(store.SessionRow{Name: name, ClaudeSessionID: claudeID, ProjectLabel: "p", Cwd: cwd,
		CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "idle"}); err != nil {
		t.Fatal(err)
	}

	runID, _ := r.Store.InsertRun("wf7", `{"name":"wf7","steps":[]}`, 100)
	// Simulate a restart-recovered run: pending_seed already set from a prior CAS.
	if claimed, err := r.Store.AdvanceRunCAS(runID, 0, 0, []string{name}, "resend me", 100); err != nil || !claimed {
		t.Fatalf("seed CAS: claimed=%v err=%v", claimed, err)
	}
	run, _, _ := r.Store.GetRun(runID)

	if err := r.RetryPendingSeed(run); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(sink)
		if strings.Contains(string(b), "resend me") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink = %q, want delivered retry seed", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	after, _, _ := r.Store.GetRun(runID)
	if after.PendingSeed != "" {
		t.Fatalf("PendingSeed = %q, want cleared after retry", after.PendingSeed)
	}
}

func TestRetryPendingSeedOnDeadSessionReturnsErrContinueDead(t *testing.T) {
	r, _, _ := testRunner(t)
	name := "loom-retrydead1"
	if err := r.Store.Upsert(store.SessionRow{Name: name, ClaudeSessionID: "x", ProjectLabel: "p", Cwd: "/w/p",
		CreatedAt: 1, EndedAt: 5, ExitCode: 0, LastStatus: "done"}); err != nil {
		t.Fatal(err)
	}
	runID, _ := r.Store.InsertRun("wf8", `{"name":"wf8","steps":[]}`, 100)
	if _, err := r.Store.AdvanceRunCAS(runID, 0, 0, []string{name}, "stuck seed", 100); err != nil {
		t.Fatal(err)
	}
	run, _, _ := r.Store.GetRun(runID)

	err := r.RetryPendingSeed(run)
	if !errors.Is(err, ErrContinueDead) {
		t.Fatalf("err = %v, want ErrContinueDead", err)
	}
}

// runningTailLines is a transcript whose tail classifies as StateRunning (an
// assistant tool_use block pending, no matching tool_result/text yet) — used
// to hold sendPendingSeed's waitForContinueGate open while a test drives a
// concurrent delivery in behind it.
func runningTailLines(cwd string) []string {
	return []string{
		fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"do the plan"},"cwd":%q,"timestamp":"2026-07-03T00:00:00Z"}`, cwd),
		fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]},"cwd":%q,"timestamp":"2026-07-03T00:00:05Z"}`, cwd),
	}
}

// appendNeedsYouTail appends lines to path flipping the classifier's tail
// state to StateNeedsYou (a tool_result consuming the pending tool_use,
// then a final text-only assistant reply).
func appendNeedsYouTail(t *testing.T, path, cwd string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := []string{
		fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","name":"Bash"}]},"cwd":%q,"timestamp":"2026-07-03T00:00:10Z"}`, cwd),
		fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]},"cwd":%q,"timestamp":"2026-07-03T00:00:15Z"}`, cwd),
	}
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

// TestSendPendingSeedNoOpsIfPendingSeedClearedWhileWaitingAtGate guards the
// debt-sweep fix (spec §2.9): sendPendingSeed must re-read PendingSeed AFTER
// waitForContinueGate, not send blind on the snapshot it started with.
// Without the fix, an async Advance goroutine's best-effort delivery and a
// user's 'n' RetryPendingSeed can both pass the gate and double-send — here
// we simulate the "already delivered by someone else" half of that race:
// the transcript starts StateRunning (gate blocked), pending_seed is
// cleared out from under the caller while it waits, and the transcript THEN
// flips to NeedsYou (gate opens) — the fixed code must see the cleared seed
// and no-op, so the tmux sink receives ZERO bytes.
func TestSendPendingSeedNoOpsIfPendingSeedClearedWhileWaitingAtGate(t *testing.T) {
	r, ccd, _ := testRunner(t)
	cwd := t.TempDir()
	claudeID := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	transcriptPath := writeTranscript(t, ccd, cwd, claudeID, runningTailLines(cwd)...) // tail = StateRunning: gate blocked

	name := "loom-racegate1"
	scriptPath := filepath.Join(t.TempDir(), "sink-cat.sh")
	if err := os.WriteFile(scriptPath, []byte(sinkCatScript), 0o755); err != nil {
		t.Fatal(err)
	}
	sink := filepath.Join(t.TempDir(), "sink.txt")
	if err := r.Launcher.Tmux.NewSession(name, cwd, "'"+scriptPath+"' '"+sink+"'", 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Upsert(store.SessionRow{Name: name, ClaudeSessionID: claudeID, ProjectLabel: "p", Cwd: cwd,
		CreatedAt: 1, EndedAt: -1, ExitCode: -1, LastStatus: "idle"}); err != nil {
		t.Fatal(err)
	}

	runID, _ := r.Store.InsertRun("wfrace", `{"name":"wfrace","steps":[]}`, 100)
	if claimed, err := r.Store.AdvanceRunCAS(runID, 0, 0, []string{name}, "duplicate me", 100); err != nil || !claimed {
		t.Fatalf("seed CAS: claimed=%v err=%v", claimed, err)
	}
	run, _, _ := r.Store.GetRun(runID)

	done := make(chan error, 1)
	go func() { done <- r.sendPendingSeed(run) }()

	// Give sendPendingSeed a moment to reach and block on
	// waitForContinueGate (StateRunning), then simulate the other
	// deliverer having already won the race and cleared pending_seed,
	// THEN let the gate open.
	time.Sleep(150 * time.Millisecond)
	if err := r.Store.ClearPendingSeed(runID, 200); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	appendNeedsYouTail(t, transcriptPath, cwd)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sendPendingSeed = %v, want nil (no-op, seed already cleared)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sendPendingSeed never returned")
	}

	b, err := os.ReadFile(sink)
	if err == nil && len(b) != 0 {
		t.Fatalf("sink = %q, want ZERO sends (duplicate delivery must be suppressed)", b)
	}
}

// --- Abandon (spec §2.12) -----------------------------------------------

func TestAbandonSetsStatusAbandoned(t *testing.T) {
	r, _, _ := testRunner(t)
	runID, _ := r.Store.InsertRun("wf9", "{}", 100)
	if err := r.Abandon(store.RunRow{ID: runID}, time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := r.Store.GetRun(runID)
	if got.Status != "abandoned" || got.UpdatedAt != 200 {
		t.Fatalf("got = %+v, want Status=abandoned UpdatedAt=200", got)
	}
}

// TestAbandonOnAlreadyAbandonedRunIsIdempotent: abandon-vs-abandon (e.g. a
// double-press that got past the UI's in-flight guard, or a second Loom
// instance) must stay a harmless no-op, not an error.
func TestAbandonOnAlreadyAbandonedRunIsIdempotent(t *testing.T) {
	r, _, _ := testRunner(t)
	runID, _ := r.Store.InsertRun("wf9b", "{}", 100)
	if err := r.Abandon(store.RunRow{ID: runID}, time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	if err := r.Abandon(store.RunRow{ID: runID}, time.Unix(300, 0)); err != nil {
		t.Fatalf("second Abandon on an already-abandoned run returned an error: %v, want nil (idempotent)", err)
	}
	got, _, _ := r.Store.GetRun(runID)
	if got.Status != "abandoned" || got.UpdatedAt != 200 {
		t.Fatalf("got = %+v, want unchanged since the FIRST abandon (UpdatedAt=200) — the second must be a no-op", got)
	}
}

// TestAbandonOnAlreadyFinishedRunSurfacesMildErrorNotOverwrite is the
// narrowing proof (spec §2.12): a run that finished between the abandon
// confirm opening and 'y' firing must not be silently clobbered back to
// 'abandoned' — Abandon must return an error, and the row must stay 'done'.
func TestAbandonOnAlreadyFinishedRunSurfacesMildErrorNotOverwrite(t *testing.T) {
	r, _, _ := testRunner(t)
	runID, _ := r.Store.InsertRun("wf9c", "{}", 100)
	if err := r.Finish(store.RunRow{ID: runID, StepIdx: 0}, time.Unix(150, 0)); err != nil {
		t.Fatal(err)
	}

	err := r.Abandon(store.RunRow{ID: runID}, time.Unix(200, 0))
	if err == nil {
		t.Fatal("Abandon on an already-done run returned nil, want a mild error")
	}
	got, _, _ := r.Store.GetRun(runID)
	if got.Status != "done" || got.UpdatedAt != 150 {
		t.Fatalf("got = %+v, want unchanged status=done (not silently overwritten to abandoned)", got)
	}
}

// --- Finish (spec §2.7 CAS gate) --------------------------------------------

func TestFinishMarksRunDoneViaCAS(t *testing.T) {
	r, _, _ := testRunner(t)
	runID, _ := r.Store.InsertRun("wf10", "{}", 100)

	if err := r.Finish(store.RunRow{ID: runID, StepIdx: 0}, time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := r.Store.GetRun(runID)
	if got.Status != "done" || got.UpdatedAt != 200 {
		t.Fatalf("got = %+v, want Status=done UpdatedAt=200", got)
	}
}

// TestFinishRejectedOnStaleStepIdxReturnsErrRunAdvancedElsewhere: an advance
// landed between the caller's pre-read and the Finish call (the same TOCTOU
// window AdvanceRunCAS closes for advances) — Finish's CAS must reject it and
// report ErrRunAdvancedElsewhere, not silently mark the run done at the wrong
// step.
func TestFinishRejectedOnStaleStepIdxReturnsErrRunAdvancedElsewhere(t *testing.T) {
	r, _, _ := testRunner(t)
	runID, _ := r.Store.InsertRun("wf11", "{}", 100)
	if _, err := r.Store.AdvanceRunCAS(runID, 0, 1, []string{"loom-a", "loom-b"}, "", 150); err != nil {
		t.Fatal(err)
	}

	err := r.Finish(store.RunRow{ID: runID, StepIdx: 0}, time.Unix(200, 0)) // stale: pre-advance snapshot
	if !errors.Is(err, ErrRunAdvancedElsewhere) {
		t.Fatalf("err = %v, want ErrRunAdvancedElsewhere", err)
	}
	got, _, _ := r.Store.GetRun(runID)
	if got.Status != "running" || got.StepIdx != 1 {
		t.Fatalf("got = %+v, want untouched (status=running step_idx=1)", got)
	}
}
