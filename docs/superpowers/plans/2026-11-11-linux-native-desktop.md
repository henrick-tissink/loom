# Linux desktop support plan

Status: core desktop implementation landed in the working branch; release
qualification and optional distribution/desktop enhancements remain. See the
implementation record below. Branch: `plan/linux-native-desktop`, based on `e8131d2`
on `fix/linux-window-flicker`.

## Outcome and scope

Loom should behave like an installed Linux desktop application: launch from
the application menu, display reliably, use native notifications and default
applications, respect Linux input conventions, and recover gracefully when a
desktop service is absent. Preserve macOS behavior and the existing invariant
that tmux owns session lifetime. Keep the Go/Wails v2 architecture initially.

Primary acceptance platform: CachyOS/Arch with Hyprland/Wayland, the user's
actual environment. Broader targets: GNOME Wayland, KDE Plasma Wayland, and
an X11 session. Validate specific distro/library versions before claiming
support; a single passing build is not a compatibility guarantee.

Codex integration is separate. This plan changes the desktop host, not the
agent protocol. No automatic migration of Claude configuration, no automatic
startup on login, and no new agent permissions are necessary.

## Baseline already delivered

- Frontend embedded in a production Linux executable built with `webkit2_41`.
- Linux-only opaque backing surface. User confirmed that this stopped violent
  border flicker; macOS transparency remains unchanged.
- User-local installer, icon and desktop entry; launcher uses `/home/exy/dev`
  as the locally selected workspace.
- `make gui-linux` and `make gui-install-linux`; GUI Go tests passed locally.

This is an installed executable, not a dependency-free static binary. GTK,
WebKit, tmux and the agent CLI remain runtime requirements.

## Design rules

Introduce a small platform boundary for notifications, open/reveal, appearance,
and desktop identity. Prefer build-tagged Go implementations where practical;
expose an explicit platform/capability DTO to the frontend rather than infer
OS from user-agent strings. Do not build a generic desktop framework.

Use argument arrays, never shell interpolation, for paths and URLs. Bound
desktop service calls with timeouts and reap subprocesses. Missing optional
services must not stop polling or agent work. Surface actionable errors and
rate-limit repeated diagnostics. Retain URL validation in `url.go`.

The initial Linux release supports one GUI owner per user/state directory;
launcher invocations activate that owner. Shared SQLite access remains supported
for CLI readers/writers, but does not establish safe multi-GUI operation.
Explicit additional GUI windows are deferred until notification and preference
coordination is implemented and tested. This is a Linux product policy, not a
change to existing macOS multi-instance behavior.

## Phase 1 — Restore everyday desktop functionality

### 1. Native notifications

Evidence: `cmd/loom-gui/notify.go` always invokes `osascript`.

- Retain its existing attention-transition and privacy filtering logic.
- Add a Linux backend using the freedesktop notification service over D-Bus;
  inspect the existing dependency tree before adding a new library. A bounded
  `notify-send` backend is an acceptable initial fallback, with dependency
  diagnostics rather than silent failure.
- Identify the app with the installed desktop ID/icon. Escape text appropriately
  for notification markup; avoid markup injection from session titles.
- Establish the desktop identity and GUI ownership contract before this backend
  ships, including the notification desktop-entry hint. Per-process transition
  tracking is not sufficient to deduplicate notifications between GUI owners.
- Honor the existing notification preference. Later add an optional action
  to select the relevant session when the notification server supports actions.
- Do not bypass the desktop's do-not-disturb policy or assume sound support.

Acceptance: one notification per qualifying transition, no repeats on each
poll, no hidden-project disclosure, graceful missing-bus/server behavior,
and no subprocess leaks. Verify on Hyprland's notification daemon and GNOME/KDE.

### 2. Open URLs/files, reveal paths and editor focus

Evidence: `editor.go` uses `open`, `open -a`, `open -R`; `brief.go` also invokes
`open`. Frontend labels say Finder.

`url.go` returns success after Wails' void `BrowserOpenURL` call. In pinned
Wails v2.13, Linux opening ultimately waits on an unbounded command and logs
errors without returning them to Loom. It must also enter the platform boundary.

- Keep supported editor CLIs and line-number arguments. Make default-app
  fallback platform-specific; never run macOS activation commands on Linux.
- For reveal, try `org.freedesktop.FileManager1.ShowItems` with properly
  encoded file URIs. Fall back to opening the containing directory through
  `xdg-open`/`gio open`; opening a directory should open that directory itself.
- Open generated brief HTML through the desktop default application.
- Route every Linux HTTP(S) launch (terminal links, PR links, local development
  servers and other callers) through the bounded opener; retain scheme validation.
  Return missing-opener, nonzero-exit and timeout errors to the frontend and
  display them visibly rather than only logging to the developer console.
  Distinguish successful dispatch from proof that a browser window appeared.
  Do not kill an independently launched browser when timing out its opener.
- Rename visible labels to “Show in file manager”; keep old bridge method
  compatibility until all frontend callers are migrated together.
- Honor compositor focus restrictions. Report a launched editor accurately;
  do not claim it was focused when Wayland activation was denied.

Acceptance: files, directories, spaces, Unicode, quotes and leading hyphens;
missing paths; no installed editor; no file manager; brief export and reveal.
Also test HTTP(S) success, rejected schemes, missing/failing/hanging openers,
and visible error recovery. Use fake runners for argument/error tests, then
real desktop checks.

### 3. Keyboard, pointer and clipboard conventions

Evidence: `frontend/main.js` hard-codes `metaKey`, Command-click link opening,
Command-K hints, and a 92px macOS traffic-light inset.

- Keep Command shortcuts on macOS. Proposed Linux palette shortcut:
  Ctrl+Shift+P, plus a clickable palette control. Check terminal bindings before
  finalizing; preserve Ctrl+C, Ctrl+D, Ctrl+K and shell/application shortcuts.
- Use Ctrl-click for Linux terminal links and files; update all hints together.
  Plain clicks must still support terminal selection.
- Support Ctrl+Shift+C/V for terminal copy/paste with clear focus ownership;
  preserve standard Ctrl+C/V in text fields. Verify bracketed multiline paste.
- Audit primary-selection/middle-click behavior as an optional Linux convention,
  not a prerequisite that breaks ordinary clipboard handling.
- Add readable focus states, keyboard navigation and Escape behavior for
  dialogs; do not swallow agent-terminal input outside intentional shortcuts.

Acceptance: shortcut table tested with terminal, text field, modal and dashboard
focus; link modifiers and tooltips agree; no raw terminal control regressions.

## Phase 2 — Appearance, windowing and startup

### 4. Native layout and appearance

- Keep Linux backing opaque. Make the CSS ground opaque on Linux too, so the
  light theme is not blended with the dark backing color. Preserve macOS tint.
- Apply traffic-light spacing only on macOS. Audit drag regions, decorations,
  maximize/fullscreen transitions, minimum size and compositor-managed tiling.
- `settings.go` runs `defaults read` for system appearance. Replace Linux lookup
  with a supported desktop/portal setting where available, and an explicit
  fallback. Keep frontend `prefers-color-scheme` and backend launch theme in sync;
  avoid transiently launching a light agent theme before dark settings resolve.
- Test live theme changes, text scaling, 100/125/150/200% display scale,
  mixed-DPI monitor movement and long localized labels.
- Evaluate reduced-motion preferences, contrast and WebKit accessibility with
  keyboard/screen-reader testing. Record unsupported behavior rather than claim
  blanket accessibility compliance.

Acceptance: no border flicker over sustained polling, typing, resize, theme and
fullscreen transitions; no clipped controls or unused macOS titlebar gap.

### 5. Terminal rendering and resource use

- Audit xterm WebGL initialization/context-loss fallback and resize observation.
  Wails v2 currently disables webview GPU acceleration by default when Linux
  options are nil; do not inadvertently change this when adding Linux options.
- Measure typing/scroll latency, idle CPU, resize behavior and memory with
  representative session counts before any desktop changes, and retain that
  baseline for subsequent comparisons. Avoid unconditional GPU flags.
- Coalesce redundant resize/title updates only where measurements justify it.
- Add a documented renderer fallback if reproducible GPU issues require one;
  verify integrated/discrete GPU configurations when available.

### 6. Robust desktop launch and diagnostics

- `path.go`: validate login-shell PATH hydration with bash, zsh and fish, bounded
  execution, missing shell and a minimal launcher environment. Use an installed
  UTF-8 locale on Linux rather than assume bare `UTF-8` is valid.
- Include `internal/tmux/tmux.go:ensureLocale` in the same audit. Test absent
  locale variables separately from explicit `LANG=C` and `LC_ALL=C`; document
  which explicit settings are preserved and how UTF-8 terminal operation is
  achieved, rather than silently overriding the user's locale globally.
- Missing `tmux`/`claude` currently causes early stderr exit. Provide a visible
  startup error with exact missing dependencies and a persistent diagnostic log.
- Make workspace selection discoverable and persistent, with documented
  precedence: explicit environment override, saved preference, existing default.
- Align executable, desktop ID, GTK program name and Wayland/X11 app identity
  so icons, grouping and notifications match. Validate actual runtime app ID.
- Resolve GUI ownership and attempt bounded activation forwarding before
  dependency checks, tmux startup, DB writes, reconciliation or indexer startup.
  A second invocation must be able to reach a healthy owner even with a reduced
  PATH or newly missing agent binary. Forward activation only; an environment
  override on a second launch must not silently replace the owner's workspace.
- Pinned Wails v2 provides `SingleInstanceLock`, but registers it during frontend
  construction, after Loom's current initialization. Its Linux D-Bus failure
  path can allow another instance. Simply enabling it does not meet this plan.
  Spike early activation/ownership using supported primitives before choosing
  the implementation; avoid PID-file-only locks and handle crashes/restarts.
- Degraded policy: do not silently start a second GUI if forwarding fails or
  ownership cannot be established. Show a bounded, actionable startup error
  and preserve the existing process/sessions. Without a session bus, a first
  GUI may start only if an independent ownership mechanism safely establishes
  exclusivity; otherwise report the missing service and exit. Do not bypass
  compositor focus restrictions or report activation success without delivery.
- Define close behavior clearly: close the GUI, leave sessions/jobs running.
  Never terminate tmux as a side effect of desktop activation or shutdown.
- Exercise suspend/resume, monitor removal, stale tmux sockets and session
  reconnect. Logs should avoid storing prompts or sensitive terminal contents.

Acceptance: launch from menu after logout/login without an interactive shell;
clear errors when dependencies are absent; restart adopts existing sessions;
window grouping works; no unexpected duplicate windows or killed agents.
Exercise two concurrent launches, a second launch with minimal/changed PATH,
missing agent binary, absent/disconnected session bus, unreachable owner,
owner crash and restart. Assert only one GUI starts polling or writes preferences;
the second launch must not duplicate notifications or alter settings.

Before any later multi-window feature, explicitly solve two confirmed hazards:
external/background attention transitions are per-engine (owned-session status
read/update is also not an atomic notification claim), and `settings.go` uses a
shared `settings.json.tmp` plus independently cached preferences. Define one
notification owner or atomic delivery claims, and serialized preference updates
with reload/conflict handling and unique temp files. Unique temp names alone do
not prevent stale overwrites. Acceptance must include simultaneous preference
edits and a single external/background attention transition observed by two
processes. Audit other JSON-backed stores under the same ownership policy.

## Phase 3 — Installation, distribution and maintainability

### 7. Reproducible local install and uninstall

- Harden `tools/install-linux.py`: validate inputs before writes, atomic desktop
  file replacement, cleanup on failure, concurrent update handling, correct Exec
  escaping (including spaces, percent signs and backslashes), and XDG paths.
- Add uninstall that removes only owned application artifacts; retain history,
  settings, agent state and sessions. Offer data removal only as a separate action.
- Separate install-existing-artifact from rebuild-and-install; add a version/
  commit identifier visible in About/diagnostics.
- Keep Linux and macOS build commands explicit or route by host OS; pin build
  tooling rather than install an unbounded Wails `@latest` during releases.
- Resolve dependency audit findings through a dedicated compatibility review,
  not `npm audit fix --force` (npm reported five findings during the initial build).

Acceptance: clean install, reinstall while running, upgrade and uninstall;
desktop-file validation; no checkout/build tools needed at runtime; unrelated
files and user data preserved. Test installer with isolated directories.

### 8. Distribution options, in order

1. Support the tested Arch-family environment with documented packages and a
   reproducible artifact/installer; consider a PKGBUILD after dependency mapping.
2. Build on a declared compatible Linux baseline; publish checksums and runtime
   requirements. Add Debian/Ubuntu packaging and ARM64 only when tested.
3. Evaluate AppImage for portable delivery. Prototype GTK/WebKit bundling and
   compatibility before promising “one file runs everywhere”.
4. Evaluate Flatpak only after host integration design: Loom needs host repo,
   tmux, agent CLI, credentials and process access. Document the required portals/
   host execution model and tradeoffs. Do not disguise broad host access as
   effective sandbox isolation. Snap is likewise a separate feasibility task.

No framework migration is required for the first supported release. Consider
Wails upgrades separately if a measured issue cannot be resolved in v2.

### 9. CI, documentation and release gates

- Existing CI explicitly excludes `cmd/loom-gui`. Add pinned GTK/WebKit build
  dependencies, locked frontend install/build, Go vet and GUI tests with the
  correct ABI tag, plus a production GUI build.
- Add headless launch smoke coverage where feasible; label it accurately:
  Xvfb cannot validate Hyprland/Wayland rendering or notification behavior.
- Retain core and discipline tests; add macOS build/test coverage so platform
  extraction does not regress the existing app.
- Replace retired-TUI guidance in `docs/MANUAL.md` with current GUI workflows,
  Linux/macOS shortcut tables, notification prerequisites and lifecycle behavior.
- Update `docs/ARCHITECTURE.md` for platform boundaries and any new persisted
  settings. Keep README requirements aligned with go.mod and supported distros.

Release matrix: Hyprland Wayland required; GNOME/KDE Wayland and X11 smoke
checks before claiming those desktops supported; macOS build and behavior
checks for unchanged transparency, shortcuts, notifications and open/reveal.

## Optional enhancements after the core release

- Tray/StatusNotifierItem with explicit opt-in close-to-tray behavior; provide
  a usable fallback on desktops without tray support.
- Desktop launcher actions (new session, search) with validated activation IPC.
- Attention badges/urgency hints where supported; no flashing-border behavior.
- Opt-in autostart via XDG with an equally visible disable control.
- Remember window geometry on floating desktops, without fighting tiling WMs.
- Configurable shortcuts, notification actions and richer desktop integration
  only after the defaults work reliably.

## Delivery sequence and definition of done

Start with rendering/resource baseline capture and an activation/ownership spike.
Then keep commits/PRs reviewable: (1) platform boundary, desktop identity and
early GUI ownership/activation, (2) bounded URL/file opening and reveal,
(3) native notifications, (4) input/layout/theme and remaining startup diagnostics,
(5) installer/CI/docs, then measured rendering fixes and optional enhancements.
The numbered feature sections above group scope; this sequence defines their
implementation dependencies. Multi-window support is a separate later feature.
Each change carries focused tests and real desktop verification where needed.

Before calling Linux supported: launch installed artifact from menu; start a
session, type/paste/resize, receive a notification, open a link/file/brief,
switch themes, close/reopen and resume the same live session; update/uninstall
without data loss; confirm macOS behavior remains intact. Record environment,
results, unresolved limitations and evidence with the release. Human observation
is required for flicker, focus and notification behavior; automated tests alone
do not establish those outcomes.


## Implementation record (2026-11-11)

Delivered: early Linux flock/socket ownership and bounded activation; explicit
GPU policy and identity; Linux notification dispatch with rate-limited errors;
bounded URL/file/editor opening and file-manager reveal fallback; Linux modifier,
clipboard, palette, opaque layout, focus and reduced-motion behavior; portal
startup appearance and UTF-8 defaults; workspace preference; safer settings
persistence; visible startup diagnostics and error record; atomic artifact
installation, receipt-checked uninstall, tarball/checksum build target; GUI CI
for Linux/macOS; refreshed user/architecture documentation. Compatible dependency
updates reduce npm audit findings from five to two.

Validation: production frontend/Go build; full Go test suite (including GUI),
vet, GUI/config/tmux race tests; installer upgrade/uninstall and modified-file
protection tests; desktop-file validation; Hyprland mapped window and immediate
second-launch forwarding observed. Full tests require a normal temporary root:
the execution sandbox exposes a synthetic /tmp/.git that invalidates an existing
outside-repository test, so the suite was rerun with TMPDIR=/var/tmp.

Not yet release-qualified: sustained compositor/GPU performance and mixed-DPI
checks, suspend/resume, clipboard/IME/screen-reader and notification visuals,
GNOME/KDE/X11 and physical macOS validation. CI jobs are configured, not yet run
remotely. A pre-change performance baseline was not captured, so no performance
improvement is claimed. Some interactive launches exited cleanly before later
inspection; an immediate launch/activation test passed, but user confirmation
of whether those windows were closed is outstanding.

Remaining work: AppImage/Flatpak feasibility prototypes and portable distro/ABI
qualification; major Vite/esbuild audit remediation (two remaining build-tool
findings); optional tray, autostart, notification actions, geometry and desktop
actions. The tarball targets the build host's GTK/WebKit/glibc ABI and is not a
universal Linux binary. Multi-window coordination remains explicitly deferred.
