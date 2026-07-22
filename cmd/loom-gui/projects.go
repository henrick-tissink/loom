package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// This file is the GUI's edge onto internal/projects (spec §4/§6/§7). Two
// jobs: the read-through helpers every leak surface in this package calls, and
// the bound methods behind the project overview and "+ New project".
//
// Everything here is read-through rather than cached. A startup snapshot of
// the project set is exactly the bug §7 names: registry.Discover ran once into
// App.repos, which buildRecipe, Fanout and workflow.LoadAll all validated
// against, so a project created in-app was listed but not launchable until the
// next restart, and hide/rename/reassign never reached the launcher at all.

// resolver returns the current attribution/visibility authority, or nil when
// there is no project service (tests, and the degraded path where the store
// failed to open). Every consumer goes through the nil-tolerant helpers below
// so a missing service degrades to "nothing is hidden", never to an empty rail.
//
// On a READ error the last good resolver is reused instead, matching
// internal/ui's visibility(): a transient DB error must not un-hide a client
// mid-share. The two frontends run against one loom.db and ARCHITECTURE.md
// declares both instances live at once, so opposite failure directions would
// mean the same blip hides a project in the TUI and reveals it in the GUI.
// "No authority at all" (nil service, or an error before any successful read)
// still means no filtering — hiding is opt-in and an empty rail blamed on Loom
// is the worse failure — but once we have seen the flags, we keep honouring
// them.
func (a *App) resolver() *projects.Resolver {
	if a.projects == nil {
		return nil
	}
	r, err := a.projects.Resolver()
	a.resMu.Lock()
	defer a.resMu.Unlock()
	if err != nil {
		return a.lastRes
	}
	a.lastRes = r
	return r
}

// visible applies §6.1's predicate over a session's directory set. A nil
// resolver means we cannot know what is hidden; showing the row is the safe
// failure here, because the alternative is a blank UI blamed on Loom.
func visible(r *projects.Resolver, dirs ...string) bool {
	if r == nil {
		return true
	}
	return r.Visible(dirs...)
}

func projectVisible(r *projects.Resolver, root string) bool {
	if r == nil {
		return true
	}
	return r.ProjectVisible(root)
}

// attribute answers "which project owns this directory" for the DTO fields.
// An unattributable path comes back as the reserved Ungrouped row, which is a
// real project in §2's model — the DTO carries it as-is and the rail groups by
// it, so no surface needs a second branch for "no project".
func attribute(r *projects.Resolver, cwd string) projects.Attribution {
	if r == nil {
		return projects.Attribution{}
	}
	at, _ := r.Attribute(cwd)
	return at
}

// sessionDirs is a session's WHOLE directory set — cwd ∪ add_dirs — which is
// what §6.1 evaluates visibility over: a session whose cwd sits in a visible
// project while it edits a hidden project's repo is hidden. Returning an empty
// slice for a row with neither is deliberate; Visible treats that as
// unattributable and fails closed.
func sessionDirs(r store.SessionRow) []string {
	dirs := make([]string, 0, 4)
	if r.Cwd != "" {
		dirs = append(dirs, r.Cwd)
	}
	return append(dirs, session.DecodeAddDirs(r.AddDirs)...)
}

// targets is the full launch surface from loom.db — project roots ∪ repo
// paths, unfiltered. Errors degrade to an empty set rather than propagating:
// the launcher showing nothing is recoverable, a modal error on every poll is
// not.
func (a *App) targets() []projects.Target {
	if a.projects == nil {
		return nil
	}
	ts, err := a.projects.LaunchTargets()
	if err != nil {
		return nil
	}
	return ts
}

// visibleTargets drops the targets belonging to a hidden (or non-soloed)
// project. `missing` targets are KEPT and flagged: §7 wants them dimmed and
// non-launchable but still present, so the user can re-point them.
func (a *App) visibleTargets() []projects.Target {
	r := a.resolver()
	all := a.targets()
	out := make([]projects.Target, 0, len(all))
	for _, t := range all {
		if projectVisible(r, t.ProjectRoot) {
			out = append(out, t)
		}
	}
	return out
}

// launchableTargets is what a launch request is validated against: visible and
// present on disk. Missing is refused here rather than in the launcher so the
// failure names the project instead of surfacing as tmux silently starting in
// $HOME (§12).
func (a *App) launchableTargets() []projects.Target {
	ts := a.visibleTargets()
	out := make([]projects.Target, 0, len(ts))
	for _, t := range ts {
		if !t.Missing {
			out = append(out, t)
		}
	}
	return out
}

// workflowRepos is the set workflow.LoadAll validates step labels against:
// repos ∪ project roots (§2), so a definition naming a promoted group dir
// ("Innostream") still resolves.
//
// Target.Label is already the stable, directory-derived identifier §2 binds
// workflow labels to — a root's label is its BASENAME, never the project's
// user-editable name, so a rename cannot invalidate a saved workflow on disk —
// and is therefore used verbatim. Hidden projects are deliberately NOT
// filtered out: hiding is a presentation concern (§6.2a) and a definition that
// stops resolving would be a behaviour change; the definitions LIST is
// filtered instead.
func (a *App) workflowRepos() []registry.Repo {
	ts := a.targets()
	out := make([]registry.Repo, 0, len(ts))
	for _, t := range ts {
		out = append(out, registry.Repo{Label: t.Label, Path: t.Path})
	}
	return out
}

// ProjectRepoDTO is one member repo of a project, for the overview.
type ProjectRepoDTO struct {
	Path    string `json:"path"`
	Label   string `json:"label"`
	Missing bool   `json:"missing"`
}

// ProjectDetailDTO is a project row plus its repos — the shape behind the rail
// sections and the project overview. Ungrouped is included: §2 makes it a real
// reserved row rather than a computed bucket, so the frontend needs no second
// branch for it.
type ProjectDetailDTO struct {
	Root      string           `json:"root"`
	Name      string           `json:"name"`
	Origin    string           `json:"origin"`
	Hidden    bool             `json:"hidden"`
	Solo      bool             `json:"solo"`
	Missing   bool             `json:"missing"`
	Collapsed bool             `json:"collapsed"`
	Ungrouped bool             `json:"ungrouped"`
	Repos     []ProjectRepoDTO `json:"repos"`
}

// ListProjectDetails returns every project with its repos. It is NOT filtered
// by §6: this is the surface the hide/solo toggles live on, and a project that
// vanished from its own settings screen the moment it was hidden could never
// be unhidden.
func (a *App) ListProjectDetails() []ProjectDetailDTO {
	out := []ProjectDetailDTO{}
	defer func() { _ = recover() }()
	if a.st == nil {
		return out
	}
	ps, err := a.st.ListProjects()
	if err != nil {
		return out
	}
	all, err := a.st.ListAllProjectRepos()
	if err != nil {
		return out
	}
	byRoot := map[string][]ProjectRepoDTO{}
	for _, m := range all {
		byRoot[m.ProjectRoot] = append(byRoot[m.ProjectRoot],
			ProjectRepoDTO{Path: m.Path, Label: m.Label, Missing: m.Missing})
	}
	for _, p := range ps {
		repos := byRoot[p.Root]
		if repos == nil {
			repos = []ProjectRepoDTO{}
		}
		out = append(out, ProjectDetailDTO{
			Root: p.Root, Name: p.Name, Origin: p.Origin,
			Hidden: p.Hidden, Solo: p.Solo, Missing: p.Missing,
			Collapsed: p.Collapsed,
			Ungrouped: p.Root == store.UngroupedRoot,
			Repos:     repos,
		})
	}
	return out
}

// ProjectWarnings surfaces the non-fatal problems from the last reconcile (a
// skipped label collision, §2). Discovery is never fatal, so this is the only
// place a skipped repo becomes visible — without it the repo is simply absent
// from the launcher with no explanation anywhere.
func (a *App) ProjectWarnings() []string {
	if a.projects == nil {
		return []string{}
	}
	w := a.projects.Warnings()
	if w == nil {
		return []string{}
	}
	return w
}

// CreateProject registers a project at root with the given member repos.
//
// The root MUST already exist (§7). The folder picker cannot return a
// nonexistent path anyway, so the check only ever fires for a hand-built call
// — and refusing keeps "loom never writes into your workspace" absolute.
func (a *App) CreateProject(root, name string, repoPaths []string) error {
	if a.st == nil {
		return fmt.Errorf("store unavailable")
	}
	root = projects.Canonical(root)
	if root == "" {
		return fmt.Errorf("project root required")
	}
	if err := mustBeDir(root); err != nil {
		return err
	}
	if name == "" {
		name = filepath.Base(root)
	}
	if err := a.checkNewProject(root, name); err != nil {
		return err
	}

	now := a.now().Unix()
	if err := a.st.UpsertProject(store.Project{
		Root: root, Name: name, Origin: "created", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return err
	}
	// Members are best-effort per row for the same reason discovery is: one
	// repo already owned by another project (§2 exclusivity) must not cost the
	// user the whole project they just created.
	var firstErr error
	for _, p := range repoPaths {
		if err := a.addRepo(root, p, len(repoPaths) > 1, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// checkNewProject enforces the two write-time rejections §4 and §2 name: a
// root that is already a project (or an existing project's member repo — the
// path would then be both a root and a repo), and a duplicate name, which
// would make labels ambiguous for the project segment.
func (a *App) checkNewProject(root, name string) error {
	if _, ok, err := a.st.GetProject(root); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("a project already exists at %s", root)
	}
	if _, ok, err := a.st.GetProjectRepo(root); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s is already a repo of another project", root)
	}
	ps, err := a.st.ListProjects()
	if err != nil {
		return err
	}
	for _, p := range ps {
		if p.Name == name {
			return fmt.Errorf("a project named %q already exists", name)
		}
	}
	return nil
}

// addRepo writes one membership row, applying §2's label rule: "project/repo",
// except a repo that IS its project's root, which keeps the bare basename
// ("loom", not "loom/loom"). multi is false for a project of one, which is the
// other bare-basename case.
func (a *App) addRepo(root, path string, multi bool, now int64) error {
	path = projects.Canonical(path)
	if err := a.checkNewMember(root, path); err != nil {
		return err
	}
	label := filepath.Base(path)
	if multi && path != root {
		label = filepath.Base(root) + "/" + label
	}
	return a.st.UpsertProjectRepo(store.ProjectRepo{
		Path: path, ProjectRoot: root, Label: label, AddedAt: now,
	})
}

// checkNewMember is the write-time gate on EVERY membership row, whichever
// gesture produced it. It lives in one function because it used to live in
// AddProjectRepo only: "+ New project" fed its checklist straight into
// addRepo, so a member that was another project's root inserted silently — the
// path then sits in §4's target set twice, and the root-wins rule drops the
// membership without a word. The directory check is here for the same reason
// mustBeDir guards the root (§7/§12): a path that is not a directory reaches
// `tmux new-session -c`, which exits 0 and starts the agent in $HOME.
//
// A path that is ALREADY this project's own root is fine and is how a
// single-repo project of one is spelled (§2).
func (a *App) checkNewMember(root, path string) error {
	if path == "" {
		return fmt.Errorf("repo path required")
	}
	if err := mustBeDir(path); err != nil {
		return err
	}
	if p, ok, err := a.st.GetProject(path); err != nil {
		return err
	} else if ok && p.Root != root {
		return fmt.Errorf("%s is already a project root", path)
	}
	return nil
}

// AddProjectRepo adds one repo to an existing project.
func (a *App) AddProjectRepo(root, path string) error {
	if a.st == nil {
		return fmt.Errorf("store unavailable")
	}
	root, path = projects.Canonical(root), projects.Canonical(path)
	if _, ok, err := a.st.GetProject(root); err != nil {
		return err
	} else if !ok {
		return store.ErrNoProject
	}
	// Checked here as well as inside addRepo, and deliberately BEFORE the
	// relabel below: a rejected add must leave the existing member's label
	// exactly as it found it.
	if err := a.checkNewMember(root, path); err != nil {
		return err
	}
	existing, err := a.st.ListProjectRepos(root)
	if err != nil {
		return err
	}
	// Adding a second member promotes the project to multi-repo, so the
	// existing member's bare label is now wrong. Relabel it in the same
	// gesture; leaving it would break §2's uniqueness-by-construction the
	// moment another project owns a same-basename repo.
	if len(existing) == 1 && existing[0].Path != path {
		if m := existing[0]; m.Path != root {
			relabelled := filepath.Base(root) + "/" + filepath.Base(m.Path)
			if m.Label != relabelled {
				if err := a.st.ReassignRepo(m.Path, root, relabelled); err != nil {
					return err
				}
			}
		}
	}
	return a.addRepo(root, path, len(existing) > 0, a.now().Unix())
}

// RemoveProjectRepo is defined as REASSIGNMENT to a single-repo project at the
// repo's own path, never deletion (§7). The row must persist or discovery
// re-absorbs the repo into its old project on the next launch — and because §4
// keys on repo paths, the repo's whole history re-attributes to the new
// project for free.
func (a *App) RemoveProjectRepo(path string) error {
	if a.st == nil {
		return fmt.Errorf("store unavailable")
	}
	path = projects.Canonical(path)
	m, ok, err := a.st.GetProjectRepo(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is not a repo of any project", path)
	}
	if m.ProjectRoot == path {
		return fmt.Errorf("%s is its own project's root", path)
	}
	now := a.now().Unix()
	if _, exists, err := a.st.GetProject(path); err != nil {
		return err
	} else if !exists {
		if err := a.st.UpsertProject(store.Project{
			Root: path, Name: filepath.Base(path), Origin: "created",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
	}
	return a.st.ReassignRepo(path, path, filepath.Base(path))
}

// RenameProject sets the display name. Labels are unaffected by construction
// (§2 derives them from the root basename), so a rename can never invalidate a
// workflow definition on disk.
func (a *App) RenameProject(root, name string) error {
	if a.st == nil {
		return fmt.Errorf("store unavailable")
	}
	if name == "" {
		return fmt.Errorf("project name required")
	}
	ps, err := a.st.ListProjects()
	if err != nil {
		return err
	}
	for _, p := range ps {
		if p.Name == name && p.Root != root {
			return fmt.Errorf("a project named %q already exists", name)
		}
	}
	return a.st.SetProjectName(root, name, a.now().Unix())
}

// SetProjectHidden persists §6's hide flag. Persisted, not in-memory: a
// restart mid-demo silently revealing a client is the worse failure (§6.4).
func (a *App) SetProjectHidden(root string, hidden bool) error {
	if a.st == nil {
		return fmt.Errorf("store unavailable")
	}
	return a.st.SetProjectHidden(root, hidden, a.now().Unix())
}

// SetProjectSolo makes root the single soloed project (or clears solo). The
// store does the clear-then-set in one transaction; `hidden` is never touched,
// so leaving solo restores the prior state exactly.
func (a *App) SetProjectSolo(root string, solo bool) error {
	if a.st == nil {
		return fmt.Errorf("store unavailable")
	}
	return a.st.SetProjectSolo(root, solo, a.now().Unix())
}

// SetProjectCollapsed persists the rail section's collapse state (§8: beside
// the other project flags in loom.db, not in a third store). The frontend
// mirrors it to localStorage so the state survives a reload even when this
// binding is absent, but the server value wins as soon as the DTO carries one —
// which is why this must not fail silently into a divergent local mirror.
func (a *App) SetProjectCollapsed(root string, collapsed bool) error {
	if a.st == nil {
		return fmt.Errorf("store unavailable")
	}
	return a.st.SetProjectCollapsed(root, collapsed)
}

// SuggestRepos prefills "+ New project"'s repo checklist with the immediate
// child repos of the picked root — §3's rule-2 arm applied to one directory.
//
// It returns a value, not an error: an unreadable or repo-less root yields an
// empty list and the modal still offers the root itself, so the gesture is
// never a dead end (a project of one is a legitimate shape, §2). Non-nil so the
// JS side can iterate without a null check.
func (a *App) SuggestRepos(root string) []string {
	root = projects.Canonical(root)
	if root == "" {
		return []string{}
	}
	kids := registry.ChildRepos(root)
	if kids == nil {
		return []string{}
	}
	return kids
}

// OpenDirectoryDialog is the native folder picker behind create-project,
// re-point and add-repo (§8). The Go side owns it because Wails' runtime dialog
// needs the app context; the frontend renders its "Choose…" button only when
// this binding is present and degrades to a typed absolute path otherwise.
//
// A cancelled dialog returns ("", nil) — the empty string, not an error. The
// callers treat empty as "keep what was typed", so cancelling must not surface
// as a modal error the user has to dismiss.
func (a *App) OpenDirectoryDialog(title string) (string, error) {
	if a.ctx == nil {
		return "", fmt.Errorf("window unavailable")
	}
	if title == "" {
		title = "Choose a directory"
	}
	dir, err := wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: title,
		// The picker is the write-side canonicalization point (§4): a path that
		// arrives with a trailing slash or a relative segment would both break
		// segment-wise matching and mint a second PK row for the same directory.
		CanCreateDirectories: false,
	})
	if err != nil {
		return "", err
	}
	if dir == "" {
		return "", nil
	}
	return projects.Canonical(dir), nil
}

// RepointProject moves a `missing` project to a new root. root is the primary
// key, so a directory that was simply renamed on disk cannot be repaired by
// discovery: it would mint a second row and strand the first with its curated
// membership.
func (a *App) RepointProject(oldRoot, newRoot string) error {
	if a.st == nil {
		return fmt.Errorf("store unavailable")
	}
	newRoot = projects.Canonical(newRoot)
	if err := mustBeDir(newRoot); err != nil {
		return err
	}
	if err := a.st.RepointProject(oldRoot, newRoot, a.now().Unix()); err != nil {
		return err
	}
	// The sweep is what clears `missing`; running it now means the project is
	// launchable immediately rather than after the next startup reconcile.
	if a.projects != nil {
		_ = a.projects.Sweep()
	}
	return nil
}

// mustBeDir is the §7 precondition for every path the user hands us. A file
// masquerading as a root would pass a bare existence check and then fail
// invisibly at `tmux new-session -c`, which exits 0 and starts in $HOME (§12).
func mustBeDir(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s does not exist", path)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}
