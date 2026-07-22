package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/workflow"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Go↔JS bridge. It owns no orchestration logic beyond the PTY
// registry; session state comes from the shared engine.
type App struct {
	ctx          context.Context
	engine       *status.Engine
	tm           *tmux.Client
	st           *store.Store
	launcher     *session.Launcher
	now          func() time.Time
	reg          *ptyRegistry
	notifier     *notifier
	settings     *settingsStore
	summarizer   *memory.Summarizer
	runner       *workflow.Runner
	workflowsDir string

	// projects is the runtime source of truth for launch targets and for §6's
	// visibility predicate, queried read-through. It replaced a by-value
	// registry.Discover snapshot taken at startup, which is why a project
	// created in-app used to be listed but not launchable (§7).
	projects *projects.Service

	// lastRes is the last resolver a read actually succeeded in building. It
	// is what a failed read falls back to, so a transient DB error cannot
	// un-hide a project mid-screen-share (see resolver()). Guarded because the
	// poll loop and the bound methods both call resolver().
	resMu   sync.Mutex
	lastRes *projects.Resolver

	// auto-summarize guards: sumTried marks sessions already attempted this
	// process (so a failed/empty summary isn't retried forever); sumBusy keeps
	// at most one summarize running at a time.
	sumMu    sync.Mutex
	sumTried map[string]bool
	sumBusy  bool
}

func newApp(engine *status.Engine, tm *tmux.Client, st *store.Store, launcher *session.Launcher, svc *projects.Service, now func() time.Time) *App {
	a := &App{engine: engine, tm: tm, st: st, launcher: launcher, projects: svc, now: now, notifier: newNotifier()}
	// Until startup() wires the real emitter, events go nowhere (safe for tests).
	a.reg = newPTYRegistry(
		func(name string) *exec.Cmd { return tm.AttachCmd(name) },
		func(event string, data ...any) {
			if a.ctx != nil {
				wruntime.EventsEmit(a.ctx, event, data...)
			}
		},
	)
	return a
}

// startup is called by Wails with the app context once the window is ready.
func (a *App) startup(ctx context.Context) { a.ctx = ctx }

// ListSessions polls the engine and returns the live sessions as DTOs.
// Any error (or panic from a half-built engine) degrades to an empty list.
func (a *App) ListSessions() (out []SessionDTO) {
	out = []SessionDTO{}
	defer func() { _ = recover() }()
	if a.engine == nil {
		return out
	}
	snap, err := a.engine.Poll(a.now())
	if err != nil {
		return out
	}
	// One resolver per poll, shared by the rail rows and by the notification
	// join below, so the two can never disagree about what is hidden.
	res := a.resolver()
	out = snapshotToDTOs(snap, res)
	a.onSnapshot(snap, out, res)
	return out
}

// onSnapshot runs the attention side effects of a poll: a native notification
// for sessions that just flipped to needs-you (once-only, from the engine),
// and the window title reflecting the current needs-you count.
func (a *App) onSnapshot(snap status.Snapshot, dtos []SessionDTO, res *projects.Resolver) {
	if a.notifier != nil && (a.settings == nil || a.settings.get().Notifications) {
		labels, suppressed := needsYouLabels(snap, res)
		a.notifier.needsYou(labels, suppressed)
	}
	if a.ctx == nil {
		return
	}
	n := needsYouCount(dtos)
	title := "loom"
	if n > 0 {
		title = fmt.Sprintf("loom — %d need you", n)
	}
	wruntime.WindowSetTitle(a.ctx, title)
	// Mirror the count on the Dock icon so it's visible with the window hidden.
	setDockBadge(n)
}

// needsYouCount is the badge/title number. It counts the DTOs, which are
// already §6-filtered, so hiding a project takes its attention count with it —
// the count is a listed leak surface (§6.3): "3 need you" over a two-session
// demo names a project the user just put out of view.
func needsYouCount(dtos []SessionDTO) int {
	n := 0
	for _, d := range dtos {
		if d.Status == "needs_you" {
			n++
		}
	}
	return n
}

// ListProjects is the launcher's target picker: project roots ∪ repo paths
// from loom.db (§7), not the startup discovery snapshot. The bound method name
// is the frontend's contract and is left alone.
func (a *App) ListProjects() []ProjectDTO { return targetsToDTOs(a.visibleTargets()) }

// LaunchSession starts a new claude session from the form inputs and returns
// its tmux session name. addDirs carries §5's scoped multi-repo shape: cwd is
// the primary repo, addDirs the other selected ones.
func (a *App) LaunchSession(repoPath, model, mode, seed string, addDirs []string) (string, error) {
	if a.launcher == nil {
		return "", fmt.Errorf("launcher unavailable")
	}
	r, err := buildRecipe(a.launchableTargets(), repoPath, model, mode, seed, addDirs)
	if err != nil {
		return "", err
	}
	return a.launcher.Launch(r, launchCols, launchRows, a.now())
}

func (a *App) AttachSession(name string) error {
	if a.tm == nil {
		return fmt.Errorf("tmux unavailable")
	}
	return a.reg.attach(name)
}
func (a *App) SendInput(name, data string) { _ = a.reg.send(name, data) }

// SendReply types a line into a live session and presses Enter — triage from
// the rail without full-screen attaching. Best-effort; only meaningful for a
// live tmux session. Empty replies are rejected so a stray Enter can't fire.
func (a *App) SendReply(name, text string) error {
	if a.tm == nil {
		return fmt.Errorf("tmux unavailable")
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("empty reply")
	}
	if err := a.tm.SendLiteral(name, text); err != nil {
		return err
	}
	return a.tm.SendEnter(name)
}
func (a *App) ResizeSession(name string, cols, rows int) {
	_ = a.reg.resize(name, uint16(cols), uint16(rows))
}
func (a *App) CloseSession(name string) { a.reg.close(name) }

// recentLimit is how many finished sessions the Finished group shows;
// recentFetch is what we ask the store for so that filtering hidden projects
// out (§6.3) still leaves a full page. The multiplier is a judgement call, not
// a guarantee — a user with one visible project among many still sees a short
// list, which is correct: there is nothing else to show.
const (
	recentLimit = 30
	recentFetch = recentLimit * 4
)

// ListRecent returns the most recent finished sessions for the Finished group,
// each annotated with its stored LLM summary (when one exists).
func (a *App) ListRecent() []FinishedDTO {
	out := []FinishedDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	// Over-fetch, then filter, then trim (the memory/recall.go pattern). The
	// LIMIT is applied in SQL, so filtering after the cap would silently
	// shorten the Finished list — badly under solo, where most of a page can
	// belong to other projects. The predicate cannot be pushed into SQL: a
	// LIKE join cannot express longest-prefix, and Recent also feeds
	// Engine.Poll, which must stay project-blind (§6.2a).
	rows, err := a.st.Recent(recentFetch)
	if err != nil {
		return out
	}
	res := a.resolver()
	a.maybeAutoSummarize(rows, res)
	out = recentToDTOs(rows, a.summaryFor, res)
	if len(out) > recentLimit {
		out = out[:recentLimit]
	}
	return out
}

// summaryFor returns the stored LLM summary for a claude session id, or "".
func (a *App) summaryFor(claudeSessionID string) string {
	if a.st == nil || claudeSessionID == "" {
		return ""
	}
	if t, ok, _ := a.st.GetTranscript(claudeSessionID); ok {
		return t.LLMSummary
	}
	return ""
}

// SummarizeSession generates (or regenerates) the LLM summary for a session on
// demand and returns it. Runs the hardened haiku summarizer over the session's
// indexed docs; the result is persisted so the Finished list shows it.
func (a *App) SummarizeSession(name string) (string, error) {
	if a.summarizer == nil || a.st == nil {
		return "", fmt.Errorf("summarizer unavailable")
	}
	row, ok, err := a.st.Get(name)
	if err != nil {
		return "", err
	}
	if !ok || row.ClaudeSessionID == "" {
		return "", fmt.Errorf("session %q not found", name)
	}
	return a.summarizer.Summarize(row.ClaudeSessionID, a.now())
}

// maybeAutoSummarize, when the pref is on, summarizes one not-yet-summarized
// finished session per call in the background — at most one at a time, each
// session attempted once per process — so the Finished list fills in over
// time without a burst of claude calls.
func (a *App) maybeAutoSummarize(rows []store.SessionRow, res *projects.Resolver) {
	if a.summarizer == nil || a.settings == nil || !a.settings.get().AutoSummarize {
		return
	}
	a.sumMu.Lock()
	defer a.sumMu.Unlock()
	if a.sumBusy {
		return
	}
	if a.sumTried == nil {
		a.sumTried = map[string]bool{}
	}
	for _, r := range rows {
		id := r.ClaudeSessionID
		if id == "" || a.sumTried[id] || r.EndedAt < 0 {
			continue // no id, already attempted, or still live
		}
		if !visible(res, sessionDirs(r)...) {
			// §6.2b: hiding suppresses new Loom-initiated background work.
			// This runs on raw store rows before DTO mapping, so without the
			// check we'd spend claude quota summarizing the project the user
			// just put out of view. Skipped WITHOUT marking sumTried —
			// marking it is per-process and unhiding would never re-enable
			// the summary.
			continue
		}
		if a.summaryFor(id) != "" {
			a.sumTried[id] = true // already summarized
			continue
		}
		a.sumTried[id] = true
		a.sumBusy = true
		go func(id string) {
			_, _ = a.summarizer.Summarize(id, a.now())
			a.sumMu.Lock()
			a.sumBusy = false
			a.sumMu.Unlock()
		}(id)
		return // one per call
	}
}

// KillSession terminates a live tmux session (a running/needs-you agent).
func (a *App) KillSession(name string) error {
	if a.tm == nil {
		return fmt.Errorf("tmux unavailable")
	}
	return a.tm.KillSession(name)
}

// DismissSession removes a finished session from history (does not touch tmux).
func (a *App) DismissSession(name string) error {
	if a.st == nil {
		return fmt.Errorf("store unavailable")
	}
	return a.st.DeleteSession(name)
}

// ResumeSession relaunches a finished session via `claude --resume` and returns
// the new tmux session name.
func (a *App) ResumeSession(name string) (string, error) {
	if a.launcher == nil || a.st == nil {
		return "", fmt.Errorf("resume unavailable")
	}
	row, ok, err := a.st.Get(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("session %q not found", name)
	}
	return a.launcher.Resume(row, 120, 32, a.now())
}
