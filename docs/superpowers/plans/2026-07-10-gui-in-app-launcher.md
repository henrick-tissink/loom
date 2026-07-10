# GUI In-App Launcher Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Let `loom-gui` create sessions itself — a `+ New` button opens a form (project · model · mode · seed) that launches a real claude session and drops you into it — so the GUI is self-contained.

**Architecture:** `cmd/loom-gui/main.go` constructs `registry.Discover(...)` and a `session.Launcher{...}` (exactly as the TUI does) and passes both to the `App` bridge. The bridge exposes `ListProjects()` and `LaunchSession(...)`, backed by a pure, validated `buildRecipe`. The frontend adds a modal form and auto-attaches the new session.

**Tech Stack:** Go 1.26.4, Wails v2, existing engine packages (`registry`, `session`, `status`, `tmux`, `store`, `config`), xterm.js frontend.

## Global Constraints

- Do NOT modify any `internal/` package — consume only.
- Models allowed: `"", "opus", "sonnet", "fable"`. Modes allowed: `"", "plan", "acceptEdits", "auto", "bypassPermissions"`. (`""` = default.) Mirror verbatim.
- `Launch` is non-blocking; call it synchronously from the bridge.
- `LaunchSession` passes `w=120, h=32`; the terminal attach resizes to real dims.
- Launcher constructed exactly as the TUI: `ReadyMarker: session.DefaultReadyMarker`, `TrustMarker: session.DefaultTrustMarker`, `ReadyTimeout: 60*time.Second`, `PollEvery: 500*time.Millisecond`, `ClaudeJSONPath: cfg.ClaudeJSONPath()`.
- The `+ New` button must carry `--wails-draggable: no-drag` (it lives in the draggable titlebar).
- All new colors/spacing use existing CSS tokens; palette stays in `theme.js`.

---

### Task 1: Backend — projects + launcher bridge

**Files:**
- Create: `cmd/loom-gui/launch.go`
- Create: `cmd/loom-gui/launch_test.go`
- Modify: `cmd/loom-gui/app.go` (add fields, `newApp` params, `ListProjects`, `LaunchSession`)
- Modify: `cmd/loom-gui/app_test.go` (update `newApp` call sites to new arity; add bridge tests)
- Modify: `cmd/loom-gui/main.go` (construct projects + launcher, pass to `newApp`)

**Interfaces:**
- Consumes: `registry.Project`, `registry.Discover`, `session.Recipe`, `session.Launcher`, `session.DefaultReadyMarker`, `session.DefaultTrustMarker`, `status.Engine`, `tmux.Client`, `config`.
- Produces:
  - `type ProjectDTO struct { Label, Path string }`
  - `func projectsToDTOs(ps []registry.Project) []ProjectDTO`
  - `func buildRecipe(projects []registry.Project, projectPath, model, mode, seed string) (session.Recipe, error)`
  - `func newApp(engine *status.Engine, tm *tmux.Client, launcher *session.Launcher, projects []registry.Project, now func() time.Time) *App`
  - `(*App) ListProjects() []ProjectDTO`
  - `(*App) LaunchSession(projectPath, model, mode, seed string) (string, error)`

- [ ] **Step 1: Write the failing test (launch.go pure logic)**

Create `cmd/loom-gui/launch_test.go`:

```go
package main

import (
	"testing"

	"github.com/henricktissink/loom/internal/registry"
)

var testProjects = []registry.Project{
	{Label: "loom", Path: "/ws/loom"},
	{Label: "group/api", Path: "/ws/group/api"},
}

func TestProjectsToDTOs(t *testing.T) {
	got := projectsToDTOs(testProjects)
	if len(got) != 2 || got[0] != (ProjectDTO{Label: "loom", Path: "/ws/loom"}) {
		t.Fatalf("mapping mismatch: %+v", got)
	}
	if projectsToDTOs(nil) == nil {
		t.Fatal("must return non-nil empty slice")
	}
}

func TestBuildRecipe_valid(t *testing.T) {
	r, err := buildRecipe(testProjects, "/ws/group/api", "sonnet", "acceptEdits", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.ProjectLabel != "group/api" || r.Cwd != "/ws/group/api" ||
		r.Model != "sonnet" || r.Mode != "acceptEdits" || r.Seed != "hello" {
		t.Fatalf("recipe mismatch: %+v", r)
	}
}

func TestBuildRecipe_defaultsAllowed(t *testing.T) {
	// empty model/mode/seed are all valid (== claude defaults, no seed)
	if _, err := buildRecipe(testProjects, "/ws/loom", "", "", ""); err != nil {
		t.Fatalf("empty model/mode/seed should be valid: %v", err)
	}
}

func TestBuildRecipe_rejectsUnknown(t *testing.T) {
	if _, err := buildRecipe(testProjects, "/ws/nope", "", "", ""); err == nil {
		t.Error("unknown project should error")
	}
	if _, err := buildRecipe(testProjects, "/ws/loom", "gpt", "", ""); err == nil {
		t.Error("unknown model should error")
	}
	if _, err := buildRecipe(testProjects, "/ws/loom", "", "yolo", ""); err == nil {
		t.Error("unknown mode should error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/loom-gui/ -run 'TestProjectsToDTOs|TestBuildRecipe' -v`
Expected: FAIL — `undefined: projectsToDTOs` / `undefined: buildRecipe` / `undefined: ProjectDTO`.

- [ ] **Step 3: Write launch.go**

Create `cmd/loom-gui/launch.go`:

```go
package main

import (
	"fmt"

	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
)

// ProjectDTO is the flat view of a discovered workspace project for the form.
type ProjectDTO struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

func projectsToDTOs(ps []registry.Project) []ProjectDTO {
	out := make([]ProjectDTO, 0, len(ps))
	for _, p := range ps {
		out = append(out, ProjectDTO{Label: p.Label, Path: p.Path})
	}
	return out
}

// Allowed values mirror the TUI launcher's option sets verbatim ("" = default).
var allowedModels = map[string]bool{"": true, "opus": true, "sonnet": true, "fable": true}
var allowedModes = map[string]bool{"": true, "plan": true, "acceptEdits": true, "auto": true, "bypassPermissions": true}

// buildRecipe validates the form inputs and resolves the project path to a
// discovered project, returning a launch Recipe. It errors on an unknown
// project, model, or mode so a malformed request never reaches the launcher.
func buildRecipe(projects []registry.Project, projectPath, model, mode, seed string) (session.Recipe, error) {
	if !allowedModels[model] {
		return session.Recipe{}, fmt.Errorf("unknown model %q", model)
	}
	if !allowedModes[mode] {
		return session.Recipe{}, fmt.Errorf("unknown mode %q", mode)
	}
	for _, p := range projects {
		if p.Path == projectPath {
			return session.Recipe{
				ProjectLabel: p.Label,
				Cwd:          p.Path,
				Model:        model,
				Mode:         mode,
				Seed:         seed,
			}, nil
		}
	}
	return session.Recipe{}, fmt.Errorf("unknown project %q", projectPath)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/loom-gui/ -run 'TestProjectsToDTOs|TestBuildRecipe' -v`
Expected: PASS.

- [ ] **Step 5: Extend the App bridge (app.go)**

In `cmd/loom-gui/app.go`, update the imports to add `fmt`, `registry`, and `session`:

```go
import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/tmux"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)
```

Add two fields to the `App` struct (after `engine`/`tm`):

```go
	launcher *session.Launcher
	projects []registry.Project
```

Change `newApp` to accept and store them:

```go
func newApp(engine *status.Engine, tm *tmux.Client, launcher *session.Launcher, projects []registry.Project, now func() time.Time) *App {
	a := &App{engine: engine, tm: tm, launcher: launcher, projects: projects, now: now}
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
```

Add the two new bridge methods (place them after `ListSessions`):

```go
// ListProjects returns the workspace projects discovered at startup.
func (a *App) ListProjects() []ProjectDTO { return projectsToDTOs(a.projects) }

// LaunchSession starts a new claude session from the form inputs and returns
// its tmux session name. The session then appears in the next poll; the
// frontend auto-attaches it.
func (a *App) LaunchSession(projectPath, model, mode, seed string) (string, error) {
	if a.launcher == nil {
		return "", fmt.Errorf("launcher unavailable")
	}
	r, err := buildRecipe(a.projects, projectPath, model, mode, seed)
	if err != nil {
		return "", err
	}
	return a.launcher.Launch(r, 120, 32, a.now())
}
```

- [ ] **Step 6: Update app_test.go call sites and add bridge tests**

In `cmd/loom-gui/app_test.go`, update the two existing `newApp(...)` calls to the new 5-arg signature (pass `nil, nil` for launcher and projects), and add bridge tests. The full file becomes:

```go
package main

import (
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/tmux"
)

func TestApp_ListSessions_pollErrorReturnsEmpty(t *testing.T) {
	eng := status.NewEngine(tmux.New(), nil, t.TempDir())
	app := newApp(eng, tmux.New(), nil, nil, func() time.Time { return time.Unix(0, 0) })

	got := app.ListSessions()
	if got == nil {
		t.Fatal("ListSessions must never return nil (marshals to [])")
	}
	if len(got) != 0 {
		t.Fatalf("want 0 on poll error, got %d", len(got))
	}
}

func TestApp_CloseUnknownIsNoop(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, time.Now)
	app.CloseSession("does-not-exist") // must not panic
}

func TestApp_ListProjects_nonNil(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, time.Now)
	if app.ListProjects() == nil {
		t.Fatal("ListProjects must return non-nil (marshals to [])")
	}
}

func TestApp_LaunchSession_nilLauncherErrors(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, time.Now)
	if _, err := app.LaunchSession("/ws/loom", "", "", ""); err == nil {
		t.Fatal("LaunchSession with nil launcher must error")
	}
}
```

- [ ] **Step 7: Wire main.go**

In `cmd/loom-gui/main.go`, add `registry` and `session` to the imports:

```go
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
```

Then, in `run()`, replace the engine/app construction block:

```go
	engine := status.NewEngine(tm, st, cfg.ClaudeConfigDir)
	app := newApp(engine, tm, time.Now)
```

with:

```go
	projects, err := registry.Discover(cfg.WorkspaceRoot, cfg.ClaudeConfigDir)
	if err != nil {
		return fmt.Errorf("discover projects in %s: %w", cfg.WorkspaceRoot, err)
	}
	launcher := &session.Launcher{
		Tmux: tm, Store: st,
		ClaudeConfigDir: cfg.ClaudeConfigDir,
		ClaudeJSONPath:  cfg.ClaudeJSONPath(),
		ReadyMarker:     session.DefaultReadyMarker,
		TrustMarker:     session.DefaultTrustMarker,
		ReadyTimeout:    60 * time.Second,
		PollEvery:       500 * time.Millisecond,
	}
	engine := status.NewEngine(tm, st, cfg.ClaudeConfigDir)
	app := newApp(engine, tm, launcher, projects, time.Now)
```

- [ ] **Step 8: Run the full package test + vet + build**

Run:
```
go test ./cmd/loom-gui/
go vet ./cmd/loom-gui/
go build -o /dev/null ./cmd/loom-gui/
```
Expected: all pass (the pre-committed `frontend/dist/.gitkeep` keeps the `//go:embed` target present so the package compiles).

- [ ] **Step 9: Commit**

```bash
git add cmd/loom-gui/launch.go cmd/loom-gui/launch_test.go cmd/loom-gui/app.go cmd/loom-gui/app_test.go cmd/loom-gui/main.go
git commit -m "feat(gui): project list + launch-session bridge (in-app launcher backend)"
```

---

### Task 2: Frontend — New-session modal + auto-attach

**Files:**
- Modify: `cmd/loom-gui/frontend/index.html` (titlebar `+ New` button)
- Modify: `cmd/loom-gui/frontend/tokens.css` (titlebar layout, button, modal styles)
- Modify: `cmd/loom-gui/frontend/main.js` (open modal, populate, launch, auto-attach)

**Interfaces:**
- Consumes: `window.go.main.App.ListProjects()`, `window.go.main.App.LaunchSession(projectPath, model, mode, seed)`, and the existing `selectSession(name)`.
- Produces: no Go interface — frontend integration only.

- [ ] **Step 1: Titlebar button (index.html)**

In `cmd/loom-gui/frontend/index.html`, replace the `<header id="titlebar">…</header>` line with:

```html
    <header id="titlebar">
      <span class="wordmark">loom</span>
      <button id="new-session" title="New session">+ New</button>
    </header>
```

- [ ] **Step 2: Titlebar + modal styles (tokens.css)**

In `cmd/loom-gui/frontend/tokens.css`, replace the existing `#titlebar` rule with a flex layout, and append the button + modal styles at the end of the file.

Replace:

```css
#titlebar {
  height: 44px; flex: 0 0 44px; display: flex; align-items: center;
  padding-left: 78px; border-bottom: 1px solid var(--hairline);
  --wails-draggable: drag;
}
```

with:

```css
#titlebar {
  height: 44px; flex: 0 0 44px; display: flex; align-items: center;
  justify-content: space-between; padding: 0 14px 0 78px;
  border-bottom: 1px solid var(--hairline);
  --wails-draggable: drag;
}
#new-session {
  --wails-draggable: no-drag;
  font-family: var(--font-ui); font-size: 12px; color: var(--text-dim);
  background: transparent; border: 1px solid var(--hairline);
  border-radius: 6px; padding: 4px 11px; cursor: pointer;
}
#new-session:hover { color: var(--needs); border-color: var(--needs); }
```

Append at the end of the file:

```css
.modal-backdrop {
  position: fixed; inset: 0; background: rgba(0,0,0,0.55);
  display: grid; place-items: center; z-index: 50;
}
.modal {
  width: 420px; max-width: calc(100vw - 48px);
  background: var(--surface); border: 1px solid var(--hairline);
  border-radius: 12px; padding: 22px;
  display: flex; flex-direction: column; gap: 16px;
  box-shadow: 0 30px 90px -30px rgba(0,0,0,0.8);
}
.modal h2 {
  margin: 0; font-size: 13px; letter-spacing: 0.16em; text-transform: uppercase;
  color: var(--text-faint); font-weight: 600;
}
.field { display: flex; flex-direction: column; gap: 6px; }
.field label {
  font-size: 10.5px; letter-spacing: 0.12em; text-transform: uppercase; color: var(--text-faint);
}
.field select, .field textarea {
  font-family: var(--font-mono); font-size: 12.5px; color: var(--text);
  background: var(--bg); border: 1px solid var(--hairline); border-radius: 6px;
  padding: 8px 10px; width: 100%;
}
.field textarea { resize: vertical; min-height: 62px; font-family: var(--font-ui); }
.modal-error { color: var(--error); font-size: 12px; min-height: 0; }
.modal-actions { display: flex; justify-content: flex-end; gap: 10px; }
.modal-actions button {
  font-family: var(--font-ui); font-size: 12.5px; padding: 7px 14px;
  border-radius: 6px; cursor: pointer; border: 1px solid var(--hairline);
}
.btn-ghost { background: transparent; color: var(--text-dim); }
.btn-ghost:hover { color: var(--text); border-color: var(--text-faint); }
.btn-launch { background: var(--needs); color: #1a1200; border-color: var(--needs); font-weight: 600; }
.btn-launch:disabled { opacity: 0.5; cursor: default; }
```

- [ ] **Step 3: Modal logic (main.js)**

In `cmd/loom-gui/frontend/main.js`, add the launcher wiring. After the `applyTokens();` line, add the button hookup, and append the modal functions. Add near the top (after `applyTokens();`):

```js
document.getElementById("new-session").addEventListener("click", openLauncher);
```

Append these functions at the end of `main.js`:

```js
const MODELS = [
  ["", "Default"], ["opus", "opus"], ["sonnet", "sonnet"], ["fable", "fable"],
];
const MODES = [
  ["", "Default"], ["plan", "plan"], ["acceptEdits", "acceptEdits"],
  ["auto", "auto"], ["bypassPermissions", "bypassPermissions"],
];

function optionsHtml(pairs) {
  return pairs.map(([v, t]) => `<option value="${v}">${t}</option>`).join("");
}

async function openLauncher() {
  let projects = [];
  try {
    projects = await window.go.main.App.ListProjects();
  } catch (e) {
    console.error("ListProjects failed", e);
  }

  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <div class="modal" role="dialog" aria-label="New session">
      <h2>New session</h2>
      <div class="field">
        <label for="f-project">Project</label>
        <select id="f-project">${projects.map((p) => `<option value="${p.path}">${p.label}</option>`).join("")}</select>
      </div>
      <div class="field">
        <label for="f-model">Model</label>
        <select id="f-model">${optionsHtml(MODELS)}</select>
      </div>
      <div class="field">
        <label for="f-mode">Permission mode</label>
        <select id="f-mode">${optionsHtml(MODES)}</select>
      </div>
      <div class="field">
        <label for="f-seed">Seed prompt (optional)</label>
        <textarea id="f-seed" placeholder="Initial prompt or /slash-command"></textarea>
      </div>
      <div class="modal-error" id="f-error"></div>
      <div class="modal-actions">
        <button class="btn-ghost" id="f-cancel">Cancel</button>
        <button class="btn-launch" id="f-launch">Launch</button>
      </div>
    </div>`;
  document.body.appendChild(backdrop);

  const close = () => backdrop.remove();
  backdrop.addEventListener("click", (e) => { if (e.target === backdrop) close(); });
  backdrop.querySelector("#f-cancel").addEventListener("click", close);

  const launchBtn = backdrop.querySelector("#f-launch");
  launchBtn.addEventListener("click", async () => {
    const path = backdrop.querySelector("#f-project").value;
    const model = backdrop.querySelector("#f-model").value;
    const mode = backdrop.querySelector("#f-mode").value;
    const seed = backdrop.querySelector("#f-seed").value;
    const errEl = backdrop.querySelector("#f-error");
    if (!path) { errEl.textContent = "Pick a project to launch."; return; }
    errEl.textContent = "";
    launchBtn.disabled = true;
    try {
      const name = await window.go.main.App.LaunchSession(path, model, mode, seed);
      close();
      selectSession(name);
      poll();
    } catch (e) {
      errEl.textContent = "Launch failed: " + e;
      launchBtn.disabled = false;
    }
  });
}
```

- [ ] **Step 4: Build and verify (manual — controller-driven)**

Run:
```
cd cmd/loom-gui
wails build
open build/bin/loom-gui.app   # or the installed /Applications/loom-gui.app
```
Manual acceptance:
1. `+ New` (top-right) opens the modal; the Project dropdown lists your real workspace projects.
2. Pick a project, leave model/mode default, Launch → the modal closes, a claude session appears in the rail and the terminal auto-attaches showing claude booting.
3. Launching with a seed sends the seed once claude is ready (watch it arrive).
4. An error (e.g. transient tmux failure) shows inline in the modal without crashing; Cancel / backdrop-click dismisses.

- [ ] **Step 5: Commit**

```bash
git add cmd/loom-gui/frontend/index.html cmd/loom-gui/frontend/tokens.css cmd/loom-gui/frontend/main.js
git commit -m "feat(gui): New-session modal launcher with auto-attach"
```

---

## Self-Review

**Spec coverage:** `+ New` button (T2/1), modal form project/model/mode/seed (T2/3), `ListProjects` (T1/5), `LaunchSession` + `buildRecipe` validation (T1/3,5), main.go launcher+projects construction mirroring the TUI (T1/7), auto-attach after launch (T2/3), non-blocking launch (uses `Launch` as-is), inline error handling (T1 returns errors; T2 shows them), `--wails-draggable: no-drag` on the button (T2/2).

**Placeholder scan:** none — full code in every step.

**Type consistency:** `ProjectDTO{Label,Path}`, `projectsToDTOs`, `buildRecipe(projects, projectPath, model, mode, seed)`, `newApp(engine, tm, launcher, projects, now)`, `ListProjects`, `LaunchSession(projectPath, model, mode, seed)` are consistent across Task 1's files and Task 2's calls. Frontend option values (`"", opus, sonnet, fable` / `"", plan, acceptEdits, auto, bypassPermissions`) match `allowedModels`/`allowedModes`. The `selectSession(name)` reused in T2/3 is the function defined in the terminal task (Task 4 of the skeleton).
