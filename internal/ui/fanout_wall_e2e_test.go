// Phase 4 Task 3 e2e (spec docs/superpowers/specs/2026-07-04-fanout-wall-design.md
// §5, binding): drives fan-out (`N`) and the wall (`W`) end-to-end against a
// scratch HOME/CLAUDE_CONFIG_DIR workspace of three real scratch projects, a
// real compiled-in `session.Launcher` + `tmux.Client` + `status.Engine`, and a
// PATH-injected fake `claude` — fan out one recipe across all three, confirm
// every launched session gets the SAME "fan:"+groupID tag (real Store query),
// confirm the dashboard renders the persistent fanHint and the `· fan`
// marker on all three rows, then open the wall and confirm it shows three
// real live tails, survives a select+attach+detach round trip, and re-renders
// cleanly afterward.
//
// Isolation note (Recall Task-3 precedent, internal/ui/recall_e2e_test.go;
// itself following Phase-3 Task-4's internal/ui/app_test.go wfE2EDeps /
// internal/workflow/run_test.go testRunner): internal/tmux.New() hardcodes
// Socket:"loom" — the same socket a real, already-running Loom instance uses.
// A tmux pane inherits the env the SERVER captured at its own start-server
// time, never a later client's os.Environ(), so making the fake `claude`
// resolve via PATH requires a brand-new throwaway socket (`-L
// loomfanwalle2e<pid>`) whose server captures ITS OWN start-server env from
// this test process, rather than mutating the real, shared `-L loom`
// server's global PATH (the same shared-infrastructure risk disclosed and
// refused in the Phase-3/Recall e2e reports). Real `session.Launcher`/
// `tmux.Client`/`status.Engine` code, just talking to the throwaway socket.
// The real `-L loom` session list and `~/.loom/loom.db` mtime are asserted
// unchanged at the end.
//
// Disclosed deviation — the F12 detach step: tmux's own `bind-key -n F12
// detach-client` IS registered (internal/tmux/tmux.go's EnsureServer, run
// against the real throwaway server this test starts) and Enter on a wall
// cell returns a real, non-nil `tea.ExecProcess(AttachCmd(...), ...)`
// command — confirmed below exactly as internal/ui/app_test.go's
// TestWFAttachOnLiveRunReturnsCommand confirms the workflows equivalent.
// Actually exec'ing a real `tmux attach-session` from this automated,
// non-interactive test process (no controlling tty) is not something either
// precedent does, for the same reason: there's nothing for it to attach TO
// here. What IS exercised for real: the attach command is built and
// non-nil, gated correctly (no capture-error cell would have blocked it),
// and the exact `attachedMsg` ExecProcess's callback delivers on return (the
// same message a real F12 detach produces) is fed back through the real
// Update — driving the real pollCmd → snapMsg → wall re-render round trip.
package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/henricktissink/loom/internal/config"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

func TestFanoutWallE2E(t *testing.T) {
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
	// tmux always runs a pane's command via the default shell (zsh on this
	// machine), which unconditionally sources ~/.zshenv before anything
	// else. Left alone, that resolves against the REAL developer's HOME
	// (macOS's default-shell invocation re-resolves it from the passwd
	// database rather than honoring this process's HOME override — verified
	// empirically: without this, wall captures below showed the real
	// ~/.zshenv's own startup error). Pointing ZDOTDIR at the scratch home
	// (empty, no .zshenv there) makes zsh skip rc sourcing entirely — pure
	// test hygiene, not a product change; it keeps a real developer's shell
	// config from leaking into a captured tmux pane in this test.
	t.Setenv("ZDOTDIR", scratchHome)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.ClaudeConfigDir != scratchCCD || cfg.WorkspaceRoot != filepath.Join(scratchHome, "Sauce") {
		t.Fatalf("config.Load did not pick up scratch env: %+v", cfg)
	}

	// --- three scratch projects under $HOME/Sauce, each with .git -----------
	var projRoots []string
	for _, label := range []string{"proja", "projb", "projc"} {
		root := filepath.Join(cfg.WorkspaceRoot, label)
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		projRoots = append(projRoots, root)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	projs, err := registry.Discover(cfg.WorkspaceRoot, cfg.ClaudeConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(projs) != 3 || projs[0].Label != "proja" || projs[1].Label != "projb" || projs[2].Label != "projc" {
		t.Fatalf("registry.Discover = %+v, want [proja projb projc] (alphabetical)", projs)
	}

	// --- isolated throwaway tmux socket + PATH-injected fake claude ---------
	// Bare ready marker then discard stdin — this test asserts wall-tail
	// content by the ready marker + real pty echo of the delivered seed, not
	// by inspecting a sink file (unlike recall_e2e_test.go's byte-assert,
	// which is out of scope here — spec §5's fan-out/wall e2e list does not
	// call for a delivered-seed byte-assert), so there's no need for a
	// per-session sink path.
	binDir := t.TempDir()
	scriptPath := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\necho \"\xe2\x9d\xaf\"\nexec cat > /dev/null\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tm := &tmux.Client{Socket: fmt.Sprintf("loomfanwalle2e%d", os.Getpid())}
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
		Launcher: launcher, Projects: projs, Tmux: tm, Store: st,
		Engine: status.NewEngine(tm, st, cfg.ClaudeConfigDir),
	}
	a := NewApp(deps)
	a.width, a.height = 100, 30

	// --- drive: N -> fan-out form, toggle all 3, seed -----------------------
	_, cmd := a.Update(key("N"))
	if cmd != nil {
		t.Fatal("N (openFanout) unexpectedly returned a non-nil command")
	}
	if a.view != viewFanout {
		t.Fatalf("view = %v, want viewFanout", a.view)
	}
	if len(a.fanForm.projects) != 3 {
		t.Fatalf("fanForm.projects = %d, want 3", len(a.fanForm.projects))
	}

	a.Update(key(" ")) // toggle proja (listCur 0)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(key(" ")) // toggle projb (listCur 1)
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(key(" ")) // toggle projc (listCur 2)
	if sel := a.fanForm.selectedProjects(); len(sel) != 3 {
		t.Fatalf("selectedProjects = %+v, want all 3 checked", sel)
	}

	for i := 0; i < 3; i++ {
		a.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if a.fanForm.focus != 3 {
		t.Fatalf("focus = %d, want 3 (seed) after 3 tabs", a.fanForm.focus)
	}
	const seedText = "run the standard smoke check"
	a.Update(key(seedText))
	if a.fanForm.seed.Value() != seedText {
		t.Fatalf("seed = %q, want %q", a.fanForm.seed.Value(), seedText)
	}

	// --- capture: the populated form, before launch, widths 100 and 46 -----
	captureForm100 := captureView(a, 100)
	captureForm46 := captureView(a, 46)
	a.width, a.height = 100, 30

	// --- enter: launch all 3 for real ---------------------------------------
	_, launchCmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !a.fanInFlight {
		t.Fatal("fanInFlight must be set the moment the launch command is fired")
	}
	if a.view != viewFanout {
		t.Fatal("view must stay on viewFanout until fanResultMsg lands (spec §2.3 I2)")
	}
	if launchCmd == nil {
		t.Fatal("enter with 3 selected projects must return a command")
	}

	msg := launchCmd()
	fanRes, ok := msg.(fanResultMsg)
	if !ok {
		t.Fatalf("cmd() returned %T, want fanResultMsg", msg)
	}
	if len(fanRes.results) != 3 {
		t.Fatalf("results = %d, want 3", len(fanRes.results))
	}
	for _, r := range fanRes.results {
		if r.Err != nil {
			t.Fatalf("project %s: unexpected launch error: %v", r.Project, r.Err)
		}
		if r.Untagged {
			t.Fatalf("project %s: unexpectedly untagged", r.Project)
		}
	}

	// --- deliver the result: view -> dash, fanHint set, poll fires ----------
	_, pollCmd := a.Update(fanRes)
	if a.view != viewDash {
		t.Fatalf("view = %v, want viewDash after fanResultMsg", a.view)
	}
	if a.fanInFlight {
		t.Fatal("fanInFlight must clear once fanResultMsg lands")
	}
	if !strings.Contains(a.fanHint, "3/3 launched") {
		t.Fatalf("fanHint = %q, want it to report 3/3 launched", a.fanHint)
	}
	if !strings.Contains(a.fanHint, fanRes.group) {
		t.Fatalf("fanHint = %q, want it to contain the group id %s", a.fanHint, fanRes.group)
	}
	if pollCmd == nil {
		t.Fatal("fanResultMsg must fire pollCmd (spec §2.3)")
	}
	snap, ok := pollCmd().(snapMsg)
	if !ok {
		t.Fatalf("pollCmd() returned %T, want snapMsg", snap)
	}
	a.Update(snap)
	if len(a.snap.Live) != 3 {
		t.Fatalf("snap.Live = %d, want 3", len(a.snap.Live))
	}

	// --- assert group tag on all 3 sessions (real Store query) -------------
	for _, r := range fanRes.results {
		row, ok, err := st.Get(r.Name)
		if err != nil || !ok {
			t.Fatalf("store row for %s missing: ok=%v err=%v", r.Name, ok, err)
		}
		if row.Tags != "fan:"+fanRes.group {
			t.Fatalf("project %s Tags = %q, want fan:%s", r.Project, row.Tags, fanRes.group)
		}
	}
	t.Cleanup(func() {
		for _, r := range fanRes.results {
			_ = tm.KillSession(r.Name)
		}
	})

	// --- wait for all 3 seeds to actually deliver (real seedWhenReady goroutines) --
	deadline := time.Now().Add(5 * time.Second)
	for {
		allSent := true
		for _, r := range fanRes.results {
			row, ok, _ := st.Get(r.Name)
			if !ok || row.SeedStatus != "sent" {
				allSent = false
				break
			}
		}
		if allSent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for all 3 seeds to be marked sent")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// --- capture: dashboard with fanHint + `· fan` markers on all 3 rows ---
	captureDash100 := captureView(a, 100)
	captureDash46 := captureView(a, 46)
	a.width, a.height = 100, 30
	if n := strings.Count(captureDash100, fanMarkerSuffix); n != 3 {
		t.Fatalf("dashboard (w=100) has %d %q markers, want 3:\n%s", n, fanMarkerSuffix, captureDash100)
	}
	if !strings.Contains(captureDash100, fanRes.group) {
		t.Fatalf("dashboard (w=100) does not show the fanHint group id %s:\n%s", fanRes.group, captureDash100)
	}

	// --- W -> wall: three live tails, captures at 100 and 46 ---------------
	_, wallCmd := a.Update(key("W"))
	if a.view != viewWall {
		t.Fatalf("view = %v, want viewWall", a.view)
	}
	if wallCmd == nil {
		t.Fatal("openWall must return the immediate capture command")
	}
	if len(a.wallOrder) != 3 {
		t.Fatalf("wallOrder = %d, want 3", len(a.wallOrder))
	}
	wm, ok := wallCmd().(wallMsg)
	if !ok {
		t.Fatalf("wallCmd() returned %T, want wallMsg", wm)
	}
	a.Update(wm)
	for _, r := range a.wallOrder {
		wc, ok := a.wallCaptures[r.Name]
		if !ok || wc.err {
			t.Fatalf("wallCaptures[%s] = %+v, ok=%v — want a real, error-free capture", r.Name, wc, ok)
		}
	}

	captureWall100 := captureView(a, 100)
	captureWall46 := captureView(a, 46)
	a.width, a.height = 100, 30
	for _, capture := range []string{captureWall100, captureWall46} {
		if strings.Contains(capture, "pane unavailable") {
			t.Fatalf("wall capture unexpectedly shows a pane-unavailable cell:\n%s", capture)
		}
		if !strings.Contains(capture, "of 3") {
			t.Fatalf("wall capture missing the '… of 3' page indicator:\n%s", capture)
		}
		for _, p := range []string{"proja", "projb", "projc"} {
			if !strings.Contains(capture, p) {
				t.Fatalf("wall capture missing project header %q:\n%s", p, capture)
			}
		}
	}

	// --- select the second cell, attach, F12 (see doc comment), wall again -
	firstSelected := a.wallSelected
	a.Update(key("j"))
	if a.wallSelected == firstSelected {
		t.Fatal("j did not move the wall selection")
	}
	secondName := a.wallOrder[1].Name
	if a.wallSelected != secondName {
		t.Fatalf("wallSelected = %q, want the second cell %q", a.wallSelected, secondName)
	}

	_, attachCmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if attachCmd == nil {
		t.Fatal("enter on a live, capture-clean wall cell must return a real attach command")
	}
	// Not exec'd here — see the file-level doc comment's disclosed deviation.

	// The F12 detach round trip: attachedMsg is exactly what ExecProcess's
	// callback delivers when a real attach-session process returns (which a
	// real F12 — tmux's own registered binding, see EnsureServer above —
	// would trigger). Feed it back through the real Update.
	_, afterAttachCmd := a.Update(attachedMsg{err: nil})
	if a.view != viewWall {
		t.Fatalf("view after attachedMsg = %v, want viewWall (attach/detach never changes the view)", a.view)
	}
	if afterAttachCmd == nil {
		t.Fatal("attachedMsg handler must return pollCmd")
	}
	snap2, ok := afterAttachCmd().(snapMsg)
	if !ok {
		t.Fatalf("post-attach cmd returned %T, want snapMsg", snap2)
	}
	a.Update(snap2)
	if len(a.wallOrder) != 3 {
		t.Fatalf("wallOrder after the attach round trip = %d, want 3 (still all live)", len(a.wallOrder))
	}

	captureWallAgain100 := captureView(a, 100)
	a.width, a.height = 100, 30
	if !strings.Contains(captureWallAgain100, "of 3") {
		t.Fatalf("wall capture (post-attach round trip) missing the page indicator:\n%s", captureWallAgain100)
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

	t.Logf("CAPTURE fan-out form, 3 checked + seed (w=100):\n%s", captureForm100)
	t.Logf("CAPTURE fan-out form, 3 checked + seed (w=46):\n%s", captureForm46)
	t.Logf("CAPTURE dashboard, fanHint + `· fan` markers (w=100):\n%s", captureDash100)
	t.Logf("CAPTURE dashboard, fanHint + `· fan` markers (w=46):\n%s", captureDash46)
	t.Logf("CAPTURE wall, three live tails (w=100):\n%s", captureWall100)
	t.Logf("CAPTURE wall, three live tails (w=46):\n%s", captureWall46)
	t.Logf("CAPTURE wall, post-attach round trip (w=100):\n%s", captureWallAgain100)
}
