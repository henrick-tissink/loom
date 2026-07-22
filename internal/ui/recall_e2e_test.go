// Phase 2.5 Task 3 e2e (spec docs/superpowers/specs/2026-07-04-recall-design.md
// §7, binding): drives the real launcher RELATED panel end-to-end against a
// scratch HOME/CLAUDE_CONFIG_DIR archive seeded with fixture transcripts
// across two projects, a real compiled-in `session.Launcher` + `tmux.Client`,
// and a PATH-injected fake `claude` — then proves the echo-chamber guard by
// writing the newly-launched session's own (real, captured) transcript and
// running it through the real indexer + extractor.
//
// Isolation note (Phase-3 Task-4 precedent, internal/ui/app_test.go's
// wfE2EDeps / internal/workflow/run_test.go's testRunner): internal/tmux.New()
// hardcodes Socket:"loom" — the same socket a real, already-running Loom
// instance uses (confirmed live on this machine while this test was
// written: `tmux -L loom list-sessions` showed one genuine session). A tmux
// pane inherits the env the SERVER captured at start-server time, never a
// later client's os.Environ() — so making the fake `claude` resolve via PATH
// requires either (a) a global `set-environment -g PATH` mutation against
// that already-running real server (previously attempted for the Phase-3
// e2e and refused by the sandbox as a shared-infrastructure change — the
// same risk applies here: a concurrent real launch during the mutation
// window would get the fake binary), or (b) a brand-new throwaway socket,
// whose server captures ITS OWN start-server env from this test process.
// (b) is what's used below — real `session.Launcher`/`tmux.Client` code,
// just talking to a throwaway `-L loomrecalle2e<pid>` socket instead of the
// shared `-L loom` one. The real `-L loom` session list and `~/.loom/loom.db`
// mtime are asserted unchanged at the end.
package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/henricktissink/loom/internal/config"
	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
)

// writeE2EFixture writes a minimal real-shaped transcript (ai-title + user +
// assistant records, spec-compliant JSON) for one fixture session under the
// scratch claude-config-dir/projects/<encoded-cwd>/ tree, mirroring
// internal/workflow/run_test.go's writeTranscript/stdFixtureLines convention.
func writeE2EFixture(t *testing.T, ccd, cwd, sessionID, title, userText, assistantText string) {
	t.Helper()
	dir := filepath.Join(ccd, "projects", transcript.ProjectDirName(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		fmt.Sprintf(`{"type":"ai-title","aiTitle":%s}`, mustJSON(t, title)),
		fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%s},"cwd":%s,"timestamp":"2026-07-04T09:00:00Z"}`,
			mustJSON(t, userText), mustJSON(t, cwd)),
		fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%s}]},"cwd":%s,"timestamp":"2026-07-04T09:00:05Z"}`,
			mustJSON(t, assistantText), mustJSON(t, cwd)),
	}
	p := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// captureView renders a's current state at width w (height fixed at 30) and
// returns it, restoring a.width/a.height afterward isn't needed since the
// caller sets them again before driving further input.
func captureView(a *App, w int) string {
	a.width, a.height = w, 30
	return a.View()
}

func TestRecallE2E(t *testing.T) {
	// --- real-environment isolation: snapshot before -----------------------
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve real home before scratch env: %v", err)
	}
	realDBPath := filepath.Join(realHome, ".loom", "loom.db")
	var realDBBefore os.FileInfo
	if fi, statErr := os.Stat(realDBPath); statErr == nil {
		realDBBefore = fi
	}
	realTm := tmux.New() // socket "loom" — the real, shared server; never touched below
	realSessionsBefore, _ := realTm.ListSessions()

	// --- scratch HOME / CLAUDE_CONFIG_DIR -----------------------------------
	scratchHome := t.TempDir()
	t.Setenv("HOME", scratchHome)
	scratchCCD := filepath.Join(scratchHome, "claude-config")
	t.Setenv("CLAUDE_CONFIG_DIR", scratchCCD)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.ClaudeConfigDir != scratchCCD || cfg.WorkspaceRoot != filepath.Join(scratchHome, "Sauce") {
		t.Fatalf("config.Load did not pick up scratch env: %+v", cfg)
	}

	// --- two scratch projects under $HOME/Sauce, each with .git -------------
	projARoot := filepath.Join(cfg.WorkspaceRoot, "proja")
	projBRoot := filepath.Join(cfg.WorkspaceRoot, "projb")
	for _, p := range []string{projARoot, projBRoot} {
		if err := os.MkdirAll(filepath.Join(p, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// --- fixture transcripts (2 sessions, cross-project) --------------------
	// Seed terms after buildRecallQuery("fix the card monitoring alert
	// thresholds"): card/monitoring/alert/thresholds (fix<4 chars, the is a
	// stopword — both dropped). Both fixture docs co-locate all 4 survivors
	// within a single FTS snippet window, so both clear the ≥2-matched-term
	// gate (spec §2) — one same-project (proja), one cross-project (projb),
	// exercising the same-project-boost blend (spec §2/§6).
	const sessA = "aaaaaaaa-1111-1111-1111-111111111111"
	const sessB = "bbbbbbbb-2222-2222-2222-222222222222"
	writeE2EFixture(t, cfg.ClaudeConfigDir, projARoot, sessA,
		"Card monitoring alert thresholds fix",
		"We need to fix the card monitoring alert thresholds, they keep paging at night.",
		"Bumped the card monitoring alert thresholds from 50 to 80 failures per minute; alerts quieted down.")
	writeE2EFixture(t, cfg.ClaudeConfigDir, projBRoot, sessB,
		"Projb card monitoring pipeline",
		"Set up a card monitoring alert pipeline for projb's servers, watching thresholds closely.",
		"Card monitoring alert thresholds pipeline deployed for projb; dashboards are live now.")

	// --- store + real indexer sweep -----------------------------------------
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ix := memory.NewIndexer(st, cfg.ClaudeConfigDir)
	if err := ix.Sweep(); err != nil {
		t.Fatalf("initial Sweep: %v", err)
	}
	if n, _ := st.TranscriptCount(); n != 2 {
		t.Fatalf("TranscriptCount = %d, want 2 (both fixtures indexed)", n)
	}

	// --- real registry discovery ---------------------------------------------
	projects, err := registry.Discover(cfg.WorkspaceRoot, cfg.ClaudeConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	projs := registry.Repos(projects)
	if len(projs) != 2 || projs[0].Label != "proja" || projs[1].Label != "projb" {
		t.Fatalf("registry.Repos(Discover) = %+v, want [proja projb] (alphabetical)", projs)
	}

	// --- isolated throwaway tmux socket + PATH-injected fake claude ---------
	binDir := t.TempDir()
	sink := filepath.Join(t.TempDir(), "sink.txt")
	scriptPath := filepath.Join(binDir, "claude")
	// Bare ready marker then sink stdin verbatim — sink path baked into the
	// script's own content (tmux server-env gotcha: an env var set on this
	// test process does not reach the spawned pane's process environment,
	// only what the server captured at its own start-server time does; a
	// literal path in the script's bytes sidesteps that entirely).
	script := fmt.Sprintf("#!/bin/sh\necho \"\xe2\x9d\xaf\"\nexec cat > %q\n", sink)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tm := &tmux.Client{Socket: fmt.Sprintf("loomrecalle2e%d", os.Getpid())}
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.EnsureServer(); err != nil {
		t.Fatal(err)
	}

	launcher := &session.Launcher{
		Tmux: tm, Store: st, ClaudeConfigDir: cfg.ClaudeConfigDir,
		ClaudeJSONPath: filepath.Join(t.TempDir(), ".claude.json"),
		ReadyMarker:    session.DefaultReadyMarker,
		TrustMarker:    session.DefaultTrustMarker,
		ReadyTimeout:   5 * time.Second,
		PollEvery:      50 * time.Millisecond,
	}

	deps := Deps{
		Launcher: launcher, Repos: projs, Tmux: tm, Store: st,
		IndexerStatus: func() memory.Status { return ix.Status() },
	}
	a := NewApp(deps)
	a.width, a.height = 100, 30

	// --- drive: n -> project (default proja) --------------------------------
	_, cmd := a.Update(key("n"))
	if cmd == nil {
		t.Fatal("n did not open the launcher")
	}
	a.Update(cmd())
	if a.view != viewLauncher {
		t.Fatalf("view = %v, want viewLauncher", a.view)
	}
	if a.form.repos[a.form.repoIdx].Label != "proja" {
		t.Fatalf("default project = %q, want proja", a.form.repos[a.form.repoIdx].Label)
	}

	// --- type seed ------------------------------------------------------------
	for i := 0; i < 3; i++ {
		a.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if a.form.focus != 3 {
		t.Fatalf("focus = %d, want 3 (seed) after 3 tabs", a.form.focus)
	}
	const seedText = "fix the card monitoring alert thresholds"
	a.Update(key(seedText))
	if a.form.seed.Value() != seedText {
		t.Fatalf("seed = %q, want %q", a.form.seed.Value(), seedText)
	}

	// Bypass the 200ms debounce tick (same convention as
	// TestLauncherPanelBothM4Shapes): fire the real query directly and feed
	// its real result back through Update.
	rm, ok := a.panelQueryCmd(a.form.seed.Value(), a.currentProjectDir())().(panelResultsMsg)
	if !ok {
		t.Fatal("panelQueryCmd did not return panelResultsMsg")
	}
	a.Update(rm)

	rows := a.panelRows()
	if len(rows) < 2 {
		t.Fatalf("panelRows = %+v, want >=2 (both fixtures clear the 2-term gate)", rows)
	}
	var sawA, sawB bool
	for _, r := range rows {
		if r.snippet == "" {
			t.Fatalf("recall hit %s has an empty snippet (M4 shape violated for a real FTS hit)", r.t.SessionID)
		}
		switch r.t.SessionID {
		case sessA:
			sawA = true
			if !r.sameProject {
				t.Fatal("sessA (proja, the selected project) should be SameProject")
			}
		case sessB:
			sawB = true
			if r.sameProject {
				t.Fatal("sessB (projb) should NOT be SameProject")
			}
		}
	}
	if !sawA || !sawB {
		t.Fatalf("panelRows = %+v, want both sessA and sessB present", rows)
	}

	// --- capture: panel populated, widths 100 and 46 ------------------------
	capturePopulated100 := captureView(a, 100)
	capturePopulated46 := captureView(a, 46)
	a.width, a.height = 100, 30

	// --- space-include 2 -----------------------------------------------------
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // seed(3) -> panel[0]
	if !a.panelFocused {
		t.Fatal("down from seed did not enter the panel")
	}
	a.Update(key(" ")) // include row 0
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(key(" ")) // include row 1
	if len(a.includes) != 2 {
		t.Fatalf("includes = %d, want 2", len(a.includes))
	}
	includedIDs := map[string]bool{}
	for id := range a.includes {
		includedIDs[id] = true
	}
	if !includedIDs[sessA] || !includedIDs[sessB] {
		t.Fatalf("includes = %v, want {sessA, sessB}", a.includeOrder)
	}

	// --- capture: 2 includes pinned ("2/3 included"), widths 100 and 46 -----
	captureIncludes100 := captureView(a, 100)
	captureIncludes46 := captureView(a, 46)
	a.width, a.height = 100, 30

	// --- up back to the form (out of the panel) -----------------------------
	for a.panelFocused {
		a.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	if a.form.focus != 3 {
		t.Fatalf("focus = %d, want 3 (seed) after backing out of the panel", a.form.focus)
	}

	// Compute the expected assembled seed independently (pure function,
	// same inputs launch() itself uses) BEFORE firing the launch, so timing
	// of the async Launch call can't affect what we compare against.
	wantSeed, warned := buildSeedWithRecall(a.form.seed.Value(), a.includeSnapshot(), a.deps.Repos)
	if warned {
		t.Fatal("buildSeedWithRecall warned on a non-slash seed")
	}
	if !strings.HasPrefix(wantSeed, seedText) {
		t.Fatalf("assembled seed %q does not start with the typed seed %q", wantSeed, seedText)
	}
	if n := strings.Count(wantSeed, memory.RecallMarker); n != 2 {
		t.Fatalf("assembled seed has %d Related-prior-work blocks, want 2:\n%s", n, wantSeed)
	}

	// --- enter: launch ---------------------------------------------------------
	_, launchCmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.view != viewDash {
		t.Fatalf("view = %v, want viewDash after launch", a.view)
	}
	if launchCmd == nil {
		t.Fatal("enter on the form did not return a launch command")
	}
	if _, ok := launchCmd().(errMsg); ok {
		t.Fatalf("launch failed: %v", launchCmd())
	}

	// --- sink shows the user seed THEN the two blocks (byte assert) --------
	var sinkBytes []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		sinkBytes, _ = os.ReadFile(sink)
		if len(sinkBytes) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink never received the seed (timed out); sink=%q", sinkBytes)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if string(sinkBytes) != wantSeed+"\n" {
		t.Fatalf("sink = %q,\nwant  %q", sinkBytes, wantSeed+"\n")
	}

	// --- find the launched session's row (real Store row) -------------------
	live, err := st.Live()
	if err != nil {
		t.Fatal(err)
	}
	var launchedName, launchedSessID string
	for _, row := range live {
		if row.Cwd == projARoot {
			launchedName, launchedSessID = row.Name, row.ClaudeSessionID
		}
	}
	if launchedSessID == "" {
		t.Fatalf("no live session found with Cwd=%s; live=%+v", projARoot, live)
	}
	t.Cleanup(func() { _ = tm.KillSession(launchedName) })

	// --- echo-chamber guard e2e: write the REAL captured delivery as the ---
	// new session's own transcript turn (exactly what the real `claude`
	// binary would have appended: the sink IS the literal bytes that
	// reached the pty's stdin), then run the REAL indexer + extractor over
	// it, and confirm its own Ask excludes the pulled-in blocks.
	deliveredText := strings.TrimSuffix(string(sinkBytes), "\n")
	writeE2EFixture(t, cfg.ClaudeConfigDir, projARoot, launchedSessID,
		"", deliveredText, "Working on the alert thresholds now.")
	if err := ix.Sweep(); err != nil {
		t.Fatalf("post-launch Sweep: %v", err)
	}

	tr, ok, err := st.GetTranscript(launchedSessID)
	if err != nil || !ok {
		t.Fatalf("GetTranscript(%s) ok=%v err=%v", launchedSessID, ok, err)
	}
	if tr.Ask != seedText {
		t.Fatalf("new session Ask = %q, want exactly the typed seed %q (echo-guard should strip the rest)", tr.Ask, seedText)
	}
	if strings.Contains(tr.Ask, "Related prior work") {
		t.Fatalf("new session Ask leaked the recall marker: %q", tr.Ask)
	}
	if strings.Contains(tr.Ask, "quieted down") || strings.Contains(tr.Ask, "dashboards are live") {
		t.Fatalf("new session Ask leaked pulled-in outcome text: %q", tr.Ask)
	}

	// --- UI-level echo-guard: "/" search finds the new session, its detail --
	// shows the ask WITHOUT the pulled blocks.
	_, searchCmd := a.Update(key("/"))
	_ = searchCmd // openSearch's returned cmd is only the status refresh; ignored here
	if a.view != viewSearch {
		t.Fatalf("view = %v, want viewSearch", a.view)
	}
	a.Update(key("thresholds"))
	sm, ok := a.searchQueryCmd(a.searchInput.Value())().(searchResultsMsg)
	if !ok {
		t.Fatal("searchQueryCmd did not return searchResultsMsg")
	}
	a.Update(sm)

	var newHitIdx = -1
	for i, h := range a.searchHits {
		if h.SessionID == launchedSessID {
			newHitIdx = i
		}
	}
	if newHitIdx < 0 {
		t.Fatalf("search for %q did not surface the new session %s; hits=%+v", "thresholds", launchedSessID, a.searchHits)
	}
	a.searchCursor = newHitIdx
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.view != viewDetail {
		t.Fatalf("view = %v, want viewDetail", a.view)
	}
	if a.detailTranscript.Ask != seedText {
		t.Fatalf("detail Ask = %q, want %q", a.detailTranscript.Ask, seedText)
	}
	captureDetail100 := captureView(a, 100)
	captureDetail46 := captureView(a, 46)
	if strings.Contains(captureDetail100, "Related prior work") {
		t.Fatalf("rendered detail leaked the recall marker at width 100:\n%s", captureDetail100)
	}

	// --- real-environment isolation: verify unchanged after ------------------
	realSessionsAfter, _ := realTm.ListSessions()
	if len(realSessionsBefore) != len(realSessionsAfter) {
		t.Fatalf("real -L loom session count changed: before=%v after=%v", realSessionsBefore, realSessionsAfter)
	}
	for i := range realSessionsBefore {
		if realSessionsBefore[i].Name != realSessionsAfter[i].Name {
			t.Fatalf("real -L loom sessions changed: before=%v after=%v", realSessionsBefore, realSessionsAfter)
		}
	}
	if realDBBefore != nil {
		if fi, statErr := os.Stat(realDBPath); statErr != nil || !fi.ModTime().Equal(realDBBefore.ModTime()) || fi.Size() != realDBBefore.Size() {
			t.Fatalf("real ~/.loom/loom.db changed: before mtime=%v size=%d, after stat=%v err=%v",
				realDBBefore.ModTime(), realDBBefore.Size(), fi, statErr)
		}
	}

	t.Logf("CAPTURE populated panel (w=100):\n%s", capturePopulated100)
	t.Logf("CAPTURE populated panel (w=46):\n%s", capturePopulated46)
	t.Logf("CAPTURE 2 includes pinned (w=100):\n%s", captureIncludes100)
	t.Logf("CAPTURE 2 includes pinned (w=46):\n%s", captureIncludes46)
	t.Logf("CAPTURE detail post-echo-guard (w=100):\n%s", captureDetail100)
	t.Logf("CAPTURE detail post-echo-guard (w=46):\n%s", captureDetail46)
}
