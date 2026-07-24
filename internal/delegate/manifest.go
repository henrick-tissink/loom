package delegate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/projects"
)

// ManifestVersion is the only `manifest` value this Loom loads. Unknown
// versions are a LoadError and never a best-effort parse: a manifest is the
// thing that decides what real agents are told to do, and half-understanding one
// is worse than refusing it.
const ManifestVersion = 1

// Manifest is one loaded, validated plan file.
//
// It lives at <project_root>/.loom/manifests/<slug>.json — inside the user's
// workspace, authored by an agent, read by Loom READ-ONLY. It is deliberately
// NOT under ~/.loom the way workflow definitions are: that would require
// granting the authoring agent --add-dir ~/.loom, i.e. write access to loom.db
// and to every other run's state, to an agent whose output is untrusted by
// construction. The workflows precedent is right for hand-edited user files and
// wrong for agent-authored ones.
//
// Loaded on view open, like workflow.LoadAll — no watcher — and SNAPSHOTTED into
// delegation_runs.manifest_json at run creation, exactly as workflow_runs.
// def_json is: a run replays its snapshot even if the file changes underneath.
//
// The on-disk `integration` block (§10) is intentionally absent from this
// struct. Unknown JSON keys are ignored, so a full manifest still loads; 3a
// simply does not run integration and must not pretend to validate it.
type Manifest struct {
	Version  int                  `json:"manifest"`
	Name     string               `json:"name"` // == filename stem (workflows precedent)
	Project  string               `json:"project"`
	Defaults Defaults             `json:"defaults"`
	Repos    map[string]RepoSetup `json:"repos"`
	Tasks    []Task               `json:"tasks"`
	// Isolation reserves §6.1's word. Validated to "worktree" (or empty) today;
	// containers are endorsed by the evidence and rejected for revision 1
	// because a claude inside a container is a materially different product
	// (auth, PATH hydration, editor ⌘-click and tmux attach all change). The
	// field exists so adding them later is not a schema break.
	Isolation string `json:"isolation"`

	// Integration is §10's block: the per-repo gate run in each integration
	// worktree after a merge, and the cross-repo checks that are the only
	// mechanism in this design able to see an interface break across repos.
	//
	// It lives HERE, on the loaded manifest, rather than being fished out of
	// the raw file by whoever needs it. The alternative — which shipped
	// briefly — was for run creation to re-read the file for this one key and
	// splice it into the snapshot, and it was wrong in a way that is worth
	// keeping written down: `json.Marshal(m)` of a Manifest without this field
	// stores a snapshot with NO integration block, so every §10.2 pass over
	// that run skips step 3 and reports green. §5.2's precondition ("the
	// per-repo integration check is green on the merged result") was then
	// satisfiable by evidence nobody produced, on the one gate this slice calls
	// load-bearing. The field is unexported-in-effect at the format level —
	// absent is legal and common — but its ABSENCE must be a fact about the
	// manifest, never an artefact of how Loom happened to serialize it.
	//
	// Validated at load by ValidateIntegration, the same function IntegrationOf
	// runs over the snapshot, so a block that loads is a block that will run.
	Integration IntegrationSpec `json:"integration"`

	// Resolved-at-load fields, not part of the on-disk format and not persisted
	// into the snapshot. The workflow.Step.Project precedent: label→path
	// resolution happens ONCE, here, while a resolver is in hand, because
	// nothing downstream carries one.
	Path        string            `json:"-"` // the manifest FILE's path
	ProjectRoot string            `json:"-"` // §3 containment: exactly one project
	RepoPaths   map[string]string `json:"-"` // repo label → absolute primary work tree
	Warnings    []Warning         `json:"-"` // §4.4 rule 10, non-fatal, rendered on the run
}

// Defaults are the manifest-level fallbacks a task may override.
type Defaults struct {
	Model string `json:"model"`
	Mode  string `json:"mode"`
	// CheckTimeout is a Go duration string ("10m"). Parsed, defaulted and capped
	// at load into each task's Check.ResolvedTimeout, so no execution-time code
	// ever parses a string it was handed by an agent.
	CheckTimeout string `json:"check_timeout"`
}

// RepoSetup is one entry of the manifest's `repos` map. Both fields exist
// because §6.4 is real and is the failure that will actually bite: a fresh
// worktree has no node_modules, no .env, no venv, no local config, and a check
// that fails for that reason has nothing to do with the child's work.
type RepoSetup struct {
	// Bootstrap is an argv (never a shell string, §4.3) run in the worktree
	// after creation and BEFORE the child launches. A failed bootstrap blocks
	// the spawn, loudly, with output — it is strictly cheaper to fail here than
	// to spend a child's whole context discovering node_modules is missing.
	Bootstrap []string `json:"bootstrap"`
	// SeedFiles are gitignored files copied from the repo's primary work tree
	// into the worktree at the same relative path. Every copy is listed at the
	// spawn gate: the human is being shown that .env is about to be handed to an
	// agent.
	SeedFiles []string `json:"seed_files"`
}

// Task is one unit of delegated work: one repo, one worktree, one branch, one
// child session, one check, zero or more produced artifacts.
type Task struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Repo  string `json:"repo"` // a repo LABEL belonging to the manifest's project
	// Paths is a DETECTOR, not the isolation mechanism (§4.2). It feeds the
	// static overlap warning at load and the divergence report at check and
	// pre-merge time. Reading it as declared-ownership-as-isolation is the exact
	// misreading slice 1 §11's ablation forbids.
	Paths []string `json:"paths"`
	Brief string   `json:"brief"`
	// Authorization is REQUIRED and must be non-empty (§4.4 rule 5). Removing
	// explicit authorization-scope text measurably raises scope overreach, so
	// its absence is a load ERROR, not a defaulted field. Loom appends its own
	// invariants (§7) and will not invent the task-specific half.
	Authorization string `json:"authorization"`
	// Needs names ARTIFACT ids, never task ids. The task graph is DERIVED
	// (producer(artifact) → consumer), so the ready condition is a statement
	// about a thing that exists on disk and passed a check rather than about a
	// peer's self-declared status. A dependency you cannot name an artifact for
	// is a dependency you have not specified.
	Needs    []string   `json:"needs"`
	Produces []Artifact `json:"produces"`
	// Check is REQUIRED. A task without an executable check has no definition of
	// done and cannot be part of a run.
	Check Check  `json:"check"`
	Model string `json:"model"`
	Mode  string `json:"mode"`
}

// Artifact is a named, path-addressed, COMMITTED file a task publishes.
// Artifacts are the only currency between tasks.
type Artifact struct {
	ID string `json:"id"`
	// Kind is "file" or "interface". An interface artifact additionally carries
	// a Fingerprint argv, run at publish time (§8.3) and recorded against the
	// producing commit. §10.5's stale-contract alarm consumes that record and is
	// deferred; recording it is cheap and is what makes unparking it free.
	Kind        string   `json:"kind"`
	Path        string   `json:"path"`
	Fingerprint []string `json:"fingerprint"`
}

// Check is a task's executable definition of done (§4.3).
//
// Cmd is an ARGV ARRAY, never a shell string. No shell, no word splitting, no
// interpolation. A manifest is agent-authored and therefore untrusted input, and
// `sh -c` over it is a remote-code-execution channel with extra steps. It is
// already arbitrary code execution — but as an explicit, reviewable, rendered
// argv that the human approves at §5.1's gate, which is the whole point of the
// gate.
type Check struct {
	Cmd     []string          `json:"cmd"`
	Cwd     string            `json:"cwd"` // relative to the worktree; "" == "."
	Timeout string            `json:"timeout"`
	Env     map[string]string `json:"env"`

	// ResolvedTimeout is Timeout parsed, defaulted from Defaults.CheckTimeout,
	// and capped at CheckTimeoutMax. Filled in at load; never re-derived.
	ResolvedTimeout time.Duration `json:"-"`
}

// LoadError is a malformed or invalid manifest file, reported instead of
// panicking — the workflow.LoadError contract, for the same reason: a bad file
// is listed with its reason and never costs the user the other files.
type LoadError struct {
	Path, Err string
}

// Warning is a §4.4 rule 10 finding: non-fatal, rendered on the run, never a
// refusal. Two tasks in the same repo with overlapping paths; a leaf nothing
// consumes; a check whose cmd[0] is not on PATH. Each is legal and each is worth
// a human glance, which is exactly the class of thing that must not be an error.
type Warning struct {
	Task string // "" for a manifest-level warning
	Text string
}

// Scope is the one project a manifest is confined to (§3's containment rule),
// with its repos resolved to absolute paths.
type Scope struct {
	Root  string            // project root; delegation_runs.project_root
	Name  string            // display name, for error messages
	Repos map[string]string // repo label → absolute primary work tree
}

// Resolver is the narrow slice of internal/projects that loading needs. It is an
// interface so the loader is testable without a store and so this package never
// grows a second attribution derivation — a second derivation is how a hidden
// client leaks.
type Resolver interface {
	// ResolveProject maps a manifest's `project` field to exactly one project.
	// It returns ErrProjectNotFound or ErrProjectAmbiguous; both are turned into
	// a LoadError naming the project, never a panic.
	ResolveProject(name string) (Scope, error)
}

// repoOwner is an OPTIONAL upgrade of Resolver, used for one thing: §4.4 rule
// 4's error must name BOTH projects when a task's repo belongs to a different
// one, and "unknown label" is not that message. It is an unexported optional
// interface rather than a second method on Resolver because Resolver is the seam
// every other file and every test fake codes against, and widening a seam to
// improve one error string is how fakes rot. A resolver that does not implement
// it degrades to the unknown-label message, which is still an error and still
// visible.
type repoOwner interface {
	OwnerOfRepo(label string) (Scope, bool)
}

// NewResolver builds a Resolver over the launch-target set internal/projects
// already computes. Targets carry label, path, project root and project name,
// which is the whole of what §4.4 rules 2 and 4 need — so containment is checked
// against the same set the launcher launches from, not a parallel one.
func NewResolver(targets []projects.Target) Resolver {
	r := targetResolver{
		byName: map[string][]string{},
		byRoot: map[string]*Scope{},
		owner:  map[string]string{},
	}
	for _, t := range targets {
		root := t.ProjectRoot
		if root == "" {
			// A target with no project root is Ungrouped — it belongs to no
			// project and therefore can never satisfy §3's containment rule.
			continue
		}
		sc, ok := r.byRoot[root]
		if !ok {
			sc = &Scope{Root: root, Name: t.ProjectName, Repos: map[string]string{}}
			r.byRoot[root] = sc
			r.byName[t.ProjectName] = append(r.byName[t.ProjectName], root)
		}
		// Both TargetRoot and TargetRepo rows become addressable repo labels. A
		// project whose root IS its only work tree (the common single-repo
		// shape) has no TargetRepo row at all, and excluding roots would make
		// that project unusable in a manifest for no gain.
		sc.Repos[t.Label] = t.Path
		if _, dup := r.owner[t.Label]; !dup {
			r.owner[t.Label] = root
		}
	}
	return r
}

type targetResolver struct {
	byName map[string][]string // project display name → project roots sharing it
	byRoot map[string]*Scope
	owner  map[string]string // repo label → project root (first writer wins)
}

func (r targetResolver) ResolveProject(name string) (Scope, error) {
	roots := r.byName[name]
	// A manifest may also name the project by its absolute root path. Display
	// names are user-editable and collide; a root does not, so it is the escape
	// hatch out of ErrProjectAmbiguous rather than a second spelling of the
	// field.
	if len(roots) == 0 {
		if _, ok := r.byRoot[name]; ok {
			roots = []string{name}
		}
	}
	switch len(roots) {
	case 0:
		return Scope{}, fmt.Errorf("%w: %q", ErrProjectNotFound, name)
	case 1:
		return *r.byRoot[roots[0]], nil
	default:
		sorted := append([]string(nil), roots...)
		sort.Strings(sorted)
		return Scope{}, fmt.Errorf("%w: %q resolves to %s", ErrProjectAmbiguous, name, strings.Join(sorted, ", "))
	}
}

func (r targetResolver) OwnerOfRepo(label string) (Scope, bool) {
	root, ok := r.owner[label]
	if !ok {
		return Scope{}, false
	}
	return *r.byRoot[root], true
}

// ManifestDir is the directory LoadAll reads for a project. Split out because
// two surfaces need it (the loader and whatever offers "validate this manifest")
// and a second spelling of the path is a bug waiting for a rename.
func ManifestDir(projectRoot string) string {
	return filepath.Join(projectRoot, ".loom", "manifests")
}

// LoadAll loads every *.json regular file at the top level of dir. A missing dir
// is not an error (empty result). Invalid files produce a LoadError and are
// excluded from the returned manifests; both slices are sorted (manifests by
// Name, errors by Path) so callers, tests and the UI get a deterministic order.
//
// Modeled directly on workflow.LoadAll, including the symlink/irregular-file
// exclusion, because the same property matters: one bad file must never cost the
// user the others.
func LoadAll(dir string, r Resolver) ([]Manifest, []LoadError) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []LoadError{{Path: dir, Err: err.Error()}}
	}

	var ms []Manifest
	var errs []LoadError
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			continue // symlinks/devices/etc. excluded, workflow.LoadAll precedent
		}
		path := filepath.Join(dir, e.Name())
		m, merr := loadOne(path, r)
		if merr != nil {
			errs = append(errs, LoadError{Path: path, Err: merr.Error()})
			continue
		}
		ms = append(ms, m)
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	return ms, errs
}

// loadOne parses and validates one manifest, applying §4.4's ordered checks:
//
//  1. `manifest` version known; `name` == filename stem.
//  2. `project` resolves to exactly one project root.
//  3. ≥1 task; task ids unique, non-empty, and matching taskIDRe — they become
//     branch and directory components (§6.2), which is the whole reason the
//     charset is narrow.
//  4. every `repo` is a known label BELONGING TO the named project. A repo from
//     another project is an error naming both projects: under §3's containment
//     rule a task set spanning two projects means the project boundary is
//     wrong, and the fix is to fix the projects.
//  5. model/mode in the same known sets workflow/def.go uses. bypassPermissions
//     is legal but flagged — §5.1 renders it in red with the task id.
//  6. artifact ids globally unique; every `needs` names a declared artifact; a
//     task may not need an artifact it produces itself.
//  7. artifact paths and check cwds resolve INSIDE the task's repo after
//     filepath.Clean — no `..`, no absolute paths.
//  8. cycle detection (§4.5).
//  9. warnings (rule 10) collected, never fatal.
//
// Rule 9 of the spec — `integration.per_repo` coverage — is deliberately not
// implemented: integration is §10 and is deferred. Validating a block nothing
// executes would assert a guarantee 3a does not provide.
//
// Only ever returns an error for LoadAll to wrap; never panics on malformed
// input, including a manifest whose `tasks` is a JSON object or whose `needs` is
// a string.
func loadOne(path string, r Resolver) (Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	// Unknown keys are IGNORED, not rejected — the on-disk `integration` block
	// (§10) is real, authored, and unimplemented in 3a, so DisallowUnknownFields
	// here would refuse every complete manifest the format documents.
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("invalid JSON: %w", err)
	}

	// Rule 1.
	if m.Version != ManifestVersion {
		return Manifest{}, fmt.Errorf("unknown manifest version %d (this Loom loads %d)", m.Version, ManifestVersion)
	}
	stem := strings.TrimSuffix(filepath.Base(path), ".json")
	if m.Name != stem {
		return Manifest{}, fmt.Errorf("name %q does not match filename %q", m.Name, stem)
	}
	if !validIsolation[m.Isolation] {
		return Manifest{}, fmt.Errorf("unknown isolation %q (only %q)", m.Isolation, "worktree")
	}

	// Rule 2. Containment: exactly one project, resolved once, here.
	if m.Project == "" {
		return Manifest{}, errors.New("project required")
	}
	scope, err := r.ResolveProject(m.Project)
	if err != nil {
		return Manifest{}, err
	}
	m.ProjectRoot = scope.Root
	m.RepoPaths = map[string]string{}

	// Rule 5, manifest level. Defaults are validated before tasks so a typo in
	// `defaults` is not reported as three separate task errors.
	if !validModels[m.Defaults.Model] {
		return Manifest{}, fmt.Errorf("defaults: unknown model %q", m.Defaults.Model)
	}
	if !validModes[m.Defaults.Mode] {
		return Manifest{}, fmt.Errorf("defaults: unknown mode %q", m.Defaults.Mode)
	}
	defTimeout, err := parseTimeout(m.Defaults.CheckTimeout, CheckTimeoutDefault)
	if err != nil {
		return Manifest{}, fmt.Errorf("defaults: check_timeout: %w", err)
	}

	// §10's block, validated HERE and not at first use. A malformed integration
	// check is an error and not a warning for the reason IntegrationOf gives:
	// a check the author wrote and Loom silently skipped is the worst outcome
	// available, because the gate then reports green on evidence nobody
	// produced. Its repo LABELS are a warning rather than an error — see
	// warnings() — since an unknown label costs a check that cannot run, which
	// is loud at integration time, not a gate that silently passes.
	integ, err := ValidateIntegration(m.Integration, defTimeout)
	if err != nil {
		return Manifest{}, err
	}
	m.Integration = integ

	// Rule 3.
	if len(m.Tasks) == 0 {
		return Manifest{}, errors.New("must have at least 1 task")
	}

	var warns []Warning
	seenTask := map[string]bool{}
	for i := range m.Tasks {
		t := &m.Tasks[i]
		where := fmt.Sprintf("task %d", i+1)
		if t.ID != "" {
			where = fmt.Sprintf("task %q", t.ID)
		}
		if t.ID == "" {
			return Manifest{}, fmt.Errorf("%s: id required", where)
		}
		if !taskIDRe.MatchString(t.ID) {
			return Manifest{}, fmt.Errorf("%s: id must match %s (it becomes a branch and directory component)", where, taskIDRe)
		}
		if seenTask[t.ID] {
			return Manifest{}, fmt.Errorf("%s: duplicate task id", where)
		}
		seenTask[t.ID] = true

		// Rule 4.
		if t.Repo == "" {
			return Manifest{}, fmt.Errorf("%s: repo required", where)
		}
		repoPath, ok := scope.Repos[t.Repo]
		if !ok {
			if o, has := r.(repoOwner); has {
				if other, found := o.OwnerOfRepo(t.Repo); found {
					return Manifest{}, fmt.Errorf("%s: repo %q belongs to project %q, not %q — a run is scoped to exactly one project (§3); if these repos really are one initiative, the project boundary is what needs fixing",
						where, t.Repo, other.Name, scope.Name)
				}
			}
			return Manifest{}, fmt.Errorf("%s: unknown repo %q for project %q (known: %s)", where, t.Repo, scope.Name, strings.Join(sortedKeys(scope.Repos), ", "))
		}
		m.RepoPaths[t.Repo] = repoPath

		// Rule 5.
		if !validModels[t.Model] {
			return Manifest{}, fmt.Errorf("%s: unknown model %q", where, t.Model)
		}
		if !validModes[t.Mode] {
			return Manifest{}, fmt.Errorf("%s: unknown mode %q", where, t.Mode)
		}
		if effective(t.Mode, m.Defaults.Mode) == "bypassPermissions" {
			warns = append(warns, Warning{Task: t.ID, Text: "runs with mode bypassPermissions — no permission prompts in this child"})
		}

		if strings.TrimSpace(t.Authorization) == "" {
			return Manifest{}, fmt.Errorf("%s: authorization required and non-empty — Loom appends its own invariants but will not invent the task-specific half", where)
		}

		// Rule 7, check half. `check` is required: an empty argv is no check.
		if len(t.Check.Cmd) == 0 {
			return Manifest{}, fmt.Errorf("%s: check.cmd required — a task without an executable check has no definition of done", where)
		}
		if t.Check.Cmd[0] == "" {
			return Manifest{}, fmt.Errorf("%s: check.cmd[0] is empty", where)
		}
		if _, err := ResolveInside(repoPath, t.Check.Cwd); err != nil {
			return Manifest{}, fmt.Errorf("%s: check.cwd %q: %w", where, t.Check.Cwd, err)
		}
		to, err := parseTimeout(t.Check.Timeout, defTimeout)
		if err != nil {
			return Manifest{}, fmt.Errorf("%s: check.timeout: %w", where, err)
		}
		if to > CheckTimeoutMax {
			warns = append(warns, Warning{Task: t.ID, Text: fmt.Sprintf("check.timeout %s capped at %s", to, CheckTimeoutMax)})
			to = CheckTimeoutMax
		}
		t.Check.ResolvedTimeout = to

		for j := range t.Produces {
			a := &t.Produces[j]
			if a.ID == "" {
				return Manifest{}, fmt.Errorf("%s: produces[%d]: artifact id required", where, j+1)
			}
			if !validArtifactKinds[a.Kind] {
				return Manifest{}, fmt.Errorf("%s: artifact %q: unknown kind %q (file or interface)", where, a.ID, a.Kind)
			}
			if a.Path == "" {
				return Manifest{}, fmt.Errorf("%s: artifact %q: path required — an artifact is path-addressed and committed, or it is not a handoff", where, a.ID)
			}
			if _, err := ResolveInside(repoPath, a.Path); err != nil {
				return Manifest{}, fmt.Errorf("%s: artifact %q: path %q: %w", where, a.ID, a.Path, err)
			}
		}
	}

	// Rule 6 runs over the WHOLE manifest, after every task has been seen:
	// artifact ids are global, so a duplicate cannot be detected task-locally
	// and a forward `needs` is legal.
	produced := map[string]string{} // artifact id → producing task id
	for _, t := range m.Tasks {
		for _, a := range t.Produces {
			if prev, dup := produced[a.ID]; dup {
				return Manifest{}, fmt.Errorf("duplicate artifact id %q (tasks %q and %q) — artifact ids are the only currency between tasks and must be global", a.ID, prev, t.ID)
			}
			produced[a.ID] = t.ID
		}
	}
	for _, t := range m.Tasks {
		for _, n := range t.Needs {
			if n == "" {
				return Manifest{}, fmt.Errorf("task %q: empty entry in needs", t.ID)
			}
			prod, ok := produced[n]
			if !ok {
				return Manifest{}, fmt.Errorf("task %q: needs %q, which no task produces — a dependency you cannot name an artifact for is a dependency you have not specified", t.ID, n)
			}
			if prod == t.ID {
				return Manifest{}, fmt.Errorf("task %q: needs %q, which it produces itself", t.ID, n)
			}
		}
	}

	// Rule 8.
	if ce := DetectCycle(BuildGraph(m), m.Name); ce != nil {
		return Manifest{}, ce
	}

	// Rule 10. Everything below is non-fatal by construction: it is appended to
	// warns and can never return.
	warns = append(warns, m.warnings(scope)...)
	m.Warnings = warns
	m.Path = path
	return m, nil
}

// warnings is §4.4 rule 10, kept apart from loadOne so that the "this can never
// fail the load" property is visible in the signature rather than promised in a
// comment.
func (m Manifest) warnings(scope Scope) []Warning {
	var out []Warning

	// Overlapping declared paths between two tasks in the SAME repo. Two tasks
	// in different repos cannot collide: the worktree is what keeps them apart.
	for i := range m.Tasks {
		for j := i + 1; j < len(m.Tasks); j++ {
			a, b := m.Tasks[i], m.Tasks[j]
			if a.Repo != b.Repo {
				continue
			}
			for _, pa := range a.Paths {
				for _, pb := range b.Paths {
					if globsOverlap(pa, pb) {
						out = append(out, Warning{
							Task: a.ID,
							Text: fmt.Sprintf("declared path %q overlaps task %q's %q in repo %q — paths are a detector, not isolation, so this is a merge-conflict forecast", pa, b.ID, pb, a.Repo),
						})
					}
				}
			}
		}
	}

	// A leaf whose output no one integrates. The spec's literal reading —
	// `produces: []` — is the one implemented: warning on every terminal task
	// instead would fire on the last link of every chain and train the reader to
	// ignore the list, which is the failure mode a warning channel dies of.
	for _, t := range m.Tasks {
		if len(t.Produces) == 0 {
			out = append(out, Warning{Task: t.ID, Text: "produces no artifacts — nothing downstream can depend on it and nothing integrates its output"})
		}
	}

	// A check whose cmd[0] is not on PATH. Only bare command names are looked
	// up: `./scripts/contract-test.sh` resolves relative to the WORKTREE, which
	// does not exist yet at load, and LookPath would resolve it against Loom's
	// own cwd and produce a confident wrong answer.
	for _, t := range m.Tasks {
		cmd := t.Check.Cmd[0]
		if strings.ContainsRune(cmd, filepath.Separator) || strings.Contains(cmd, "/") {
			continue
		}
		if _, err := exec.LookPath(cmd); err != nil {
			out = append(out, Warning{Task: t.ID, Text: fmt.Sprintf("check command %q is not on PATH — the check will be an infra-error, not a failure", cmd)})
		}
	}

	// A `repos` entry for a label no task uses, or for a label the project does
	// not have, is dead configuration: bootstrap and seed_files that will never
	// run. Silent dead config in the file that decides what agents are told to
	// do is exactly the thing to surface.
	used := map[string]bool{}
	for _, t := range m.Tasks {
		used[t.Repo] = true
	}
	for _, label := range sortedKeys(m.Repos) {
		if _, known := scope.Repos[label]; !known {
			out = append(out, Warning{Text: fmt.Sprintf("repos[%q] is not a repo of project %q — its bootstrap and seed_files will never run", label, scope.Name)})
			continue
		}
		if !used[label] {
			out = append(out, Warning{Text: fmt.Sprintf("repos[%q] is declared but no task uses it", label)})
		}
	}

	// §10's integration block, by LABEL. A per-repo gate for a repo no task
	// touches never runs (§10.2 integrates a task into ITS repo's worktree),
	// and a gate for a label the project does not have has no worktree at all.
	//
	// Warnings and not errors, unlike everything ValidateIntegration refuses:
	// those are blocks that would run wrong, these are blocks that will not run.
	// A gate that cannot run is loud at integration time — RunIntegration
	// reports the missing worktree — whereas a malformed gate that is skipped
	// is silent, and silence is what §10 cannot afford. The distinction is the
	// same one `repos[]` above draws, for the same reason.
	for _, label := range sortedKeys(m.Integration.PerRepo) {
		switch {
		case scope.Repos[label] == "":
			out = append(out, Warning{Text: fmt.Sprintf("integration.per_repo[%q] is not a repo of project %q — that gate will never run", label, scope.Name)})
		case !used[label]:
			out = append(out, Warning{Text: fmt.Sprintf("integration.per_repo[%q] is declared but no task uses that repo — that gate will never run", label)})
		}
	}
	for _, c := range m.Integration.Cross {
		if scope.Repos[c.Repo] == "" {
			out = append(out, Warning{Text: fmt.Sprintf("integration.cross[%q] runs in repo %q, which is not a repo of project %q — it will never run", c.ID, c.Repo, scope.Name)})
		}
		for _, n := range c.Needs {
			// needs_repos gates WHEN the cross check runs (§10.2 step 4 waits
			// for each named repo to be green). A name nothing can satisfy is a
			// check that waits forever, which reads on the run exactly like a
			// check that is merely slow — the one confusion worth a line here.
			if scope.Repos[n] == "" {
				out = append(out, Warning{Text: fmt.Sprintf("integration.cross[%q] needs_repos %q, which is not a repo of project %q — that check can never become eligible", c.ID, n, scope.Name)})
			}
		}
	}

	// seed_files are copied into the worktree at the SAME relative path (§6.5),
	// so an absolute or escaping entry cannot be honoured. It is a warning and
	// not an error because worktree.Create refuses it at copy time with the real
	// file in hand — a lexical guess here must not be the thing that rejects a
	// whole manifest.
	for _, label := range sortedKeys(m.Repos) {
		for _, sf := range m.Repos[label].SeedFiles {
			if _, err := ResolveInside(scope.Repos[label], sf); err != nil {
				out = append(out, Warning{Text: fmt.Sprintf("repos[%q].seed_files %q escapes the repo and will be refused at spawn", label, sf)})
			}
		}
	}
	return out
}

// globsOverlap is a deliberately CONSERVATIVE static overlap test: two globs are
// treated as overlapping when the literal prefix of one is a path-prefix of the
// other's. Full glob intersection is undecidable in the general case and
// pointless here — the output is a warning a human reads, and the failure mode
// that matters is missing a real overlap, not naming a possible one. Compared on
// path-component boundaries so internal/account does not "overlap"
// internal/accounts.
func globsOverlap(a, b string) bool {
	pa, pb := literalPrefix(a), literalPrefix(b)
	if pa == "" || pb == "" {
		return true // a leading wildcard matches anywhere in the repo
	}
	return pa == pb || strings.HasPrefix(pa, pb+"/") || strings.HasPrefix(pb, pa+"/")
}

// literalPrefix is the portion of a glob before its first metacharacter,
// truncated to whole path components.
func literalPrefix(g string) string {
	g = strings.TrimPrefix(filepath.ToSlash(g), "./")
	i := strings.IndexAny(g, "*?[")
	if i >= 0 {
		g = g[:i]
		if j := strings.LastIndex(g, "/"); j >= 0 {
			g = g[:j]
		} else {
			g = ""
		}
	}
	return strings.Trim(g, "/")
}

// parseTimeout resolves a duration string against a fallback. An unparseable or
// non-positive value is an ERROR and never a silent fallback: a check that runs
// for the default when the author wrote "10min" is a check whose timeout the
// author does not know.
func parseTimeout(s string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
}

func effective(task, def string) string {
	if task != "" {
		return task
	}
	return def
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// taskIDRe is §4.4 rule 3's charset. Task ids become BOTH a path component
// (~/.loom/worktrees/<slug>/<repo>/<task-id>) and a git ref component
// (loom/<slug>/<task-id>), so the intersection of "safe in a path" and "safe in
// a ref" is what is allowed, and it is checked once at load rather than escaped
// at every use site.
var taskIDRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// Known sets, mirroring internal/workflow/def.go exactly — the same sets the
// interactive launcher offers. A third spelling of "which models exist" is how
// a manifest validates and then fails at launch.
var (
	validModels = map[string]bool{"": true, "opus": true, "sonnet": true, "fable": true}
	validModes  = map[string]bool{"": true, "plan": true, "acceptEdits": true, "auto": true, "bypassPermissions": true}
	// validIsolation is §6.1's reserved word. "container" is REJECTED at load
	// today and is listed nowhere: accepting the value and ignoring it would
	// silently give a user unisolated children they believe are contained.
	validIsolation = map[string]bool{"": true, "worktree": true}
	// validArtifactKinds is the same argument one level down. A kind of "iface"
	// would parse, produce no fingerprint, and silently opt the artifact out of
	// §10.5's stale-contract alarm — a typo that removes a safety net without
	// removing anything visible. "" is accepted and means "file".
	validArtifactKinds = map[string]bool{"": true, "file": true, "interface": true}
)

// ResolveInside is §4.4 rule 7 and §8.1's execution-time re-check, in one place.
// It joins rel under base, cleans it, and returns ErrEscapesRepo if the result
// leaves base — absolute rel included.
//
// It is exported because the check runner calls it again at execution time
// against the materialized worktree. A load-time pass is a statement about the
// file as it was parsed; the tree it will run in is a second fact.
//
// Purely LEXICAL: no EvalSymlinks, no Stat. At load the worktree does not exist
// yet, and a rule that can only be enforced on a materialized tree is a rule the
// loader cannot enforce. The runtime caller is the one holding a real tree and
// is where a symlink-aware check belongs if one is ever added.
func ResolveInside(base, rel string) (string, error) {
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q is absolute", ErrEscapesRepo, rel)
	}
	// A leading ':' is git PATHSPEC MAGIC, not a path. §8.3's precondition
	// hands an artifact path straight to `git ls-files` and `git diff` as a
	// pathspec, and `:(exclude)db/nothing.sql` matches whatever else is tracked
	// — so both commands succeed and the artifact reports published with
	// nothing committed. A probe demonstrated it. The escape test above cannot
	// catch it: filepath.Clean leaves the magic alone and the result is
	// lexically inside the repo.
	//
	// Refused at LOAD, where the remedy is a one-character manifest fix, and
	// again defended at the git call sites with --literal-pathspecs, because a
	// manifest snapshot written by another Loom never passes through here.
	if strings.HasPrefix(rel, ":") {
		return "", fmt.Errorf("%w: %q begins with ':', which git reads as pathspec magic", ErrEscapesRepo, rel)
	}
	cleanBase := filepath.Clean(base)
	full := filepath.Clean(filepath.Join(cleanBase, rel))
	if full != cleanBase && !strings.HasPrefix(full, cleanBase+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrEscapesRepo, rel)
	}
	return full, nil
}

// Edge is one derived producer→consumer dependency. The manifest declares
// artifacts; the edges are computed, which is why an author cannot express a
// dependency without naming the thing that satisfies it.
type Edge struct {
	From     string // producing task id
	To       string // consuming task id
	Artifact string // the artifact id that produced the edge
}

// Graph is the derived task graph. Pure data: no DB, no manifest pointer, no
// side effects — it is what cycle detection and Ready operate on, and both must
// be testable from a literal.
type Graph struct {
	// TaskIDs in manifest order, so error messages and renderings follow the
	// order the author wrote rather than Go's map iteration.
	TaskIDs []string
	// Needs maps a task id to the artifact ids it consumes.
	Needs map[string][]string
	// Producer maps an artifact id to the task that publishes it.
	Producer map[string]string
	// Edges sorted by (To, Artifact) for determinism.
	Edges []Edge
}

// BuildGraph derives the graph from a validated manifest. Pure.
//
// It is TOTAL, not partial: a `needs` naming an artifact nobody produces is a
// load error, but BuildGraph is also called on hand-built literals and on the
// snapshot, and a graph builder that panics on an unproduced artifact would turn
// a validation error into a crash at the one place validation has not run yet.
// Such a need contributes no edge — an unsatisfiable dependency is not a cycle.
func BuildGraph(m Manifest) Graph {
	g := Graph{
		Needs:    map[string][]string{},
		Producer: map[string]string{},
	}
	for _, t := range m.Tasks {
		g.TaskIDs = append(g.TaskIDs, t.ID)
		if len(t.Needs) > 0 {
			g.Needs[t.ID] = append([]string(nil), t.Needs...)
		}
		for _, a := range t.Produces {
			if _, dup := g.Producer[a.ID]; !dup {
				g.Producer[a.ID] = t.ID
			}
		}
	}
	seen := map[Edge]bool{}
	for _, t := range m.Tasks {
		for _, n := range t.Needs {
			from, ok := g.Producer[n]
			if !ok {
				continue
			}
			e := Edge{From: from, To: t.ID, Artifact: n}
			if seen[e] {
				continue // a repeated `needs` entry is one dependency, not two
			}
			seen[e] = true
			g.Edges = append(g.Edges, e)
		}
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].To != g.Edges[j].To {
			return g.Edges[i].To < g.Edges[j].To
		}
		return g.Edges[i].Artifact < g.Edges[j].Artifact
	})
	return g
}

// CycleError is a dependency cycle, carrying the actual back-edge path so the
// message can name it (§4.5):
//
//	manifest "atlas-rearchitecture": dependency cycle:
//	  auth-api → (auth-openapi) → ballista-client → (ballista-types) → schema → (account-schema) → auth-api
type CycleError struct {
	Manifest string
	Path     []Edge // the cycle, in order, closing back on Path[0].From
}

func (e *CycleError) Error() string {
	if len(e.Path) == 0 {
		return fmt.Sprintf("manifest %q: dependency cycle", e.Manifest)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "manifest %q: dependency cycle:\n  %s", e.Manifest, e.Path[0].From)
	for _, ed := range e.Path {
		fmt.Fprintf(&b, " → (%s) → %s", ed.Artifact, ed.To)
	}
	return b.String()
}

// DetectCycle reports a dependency cycle, or nil.
//
// BINDING (§4.5): a cycle is a SILENT deadlock that presents as healthy
// progress — every task sits pending, no error is raised, and the run looks like
// it is waiting on work that is being done. It must be impossible to start such
// a run.
//
// An ITERATIVE THREE-COLOUR DFS, not Kahn's algorithm. Kahn tells you a cycle
// exists; the three-colour DFS lets you recover the actual back-edge path, and
// the message must name it. Iterative rather than recursive so a pathological
// manifest cannot blow the stack. O(V+E) over a graph that will never exceed a
// few dozen nodes; there is no reason to be clever.
func DetectCycle(g Graph, manifestName string) *CycleError {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current DFS stack — a back edge into grey IS the cycle
		black = 2 // finished; everything below it is known acyclic
	)

	out := map[string][]Edge{}
	for _, e := range g.Edges {
		out[e.From] = append(out[e.From], e)
	}

	// Roots are walked in manifest order, and each node's out-edges are already
	// sorted by (To, Artifact) from BuildGraph, so the cycle REPORTED for a
	// graph containing several is the same one on every run. A validation error
	// that names a different cycle each time is a validation error nobody
	// believes.
	colour := make(map[string]int, len(g.TaskIDs))
	type frame struct {
		node string
		i    int
	}
	for _, root := range g.TaskIDs {
		if colour[root] != white {
			continue
		}
		colour[root] = grey
		stack := []frame{{node: root}}
		// path[k] is the edge from stack[k].node to stack[k+1].node, so
		// len(path) == len(stack)-1 always, and the cycle is a suffix of path.
		var path []Edge
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			adj := out[f.node]
			if f.i >= len(adj) {
				colour[f.node] = black
				stack = stack[:len(stack)-1]
				if len(path) > 0 {
					path = path[:len(path)-1]
				}
				continue
			}
			e := adj[f.i]
			f.i++
			switch colour[e.To] {
			case white:
				colour[e.To] = grey
				path = append(path, e)
				stack = append(stack, frame{node: e.To})
			case grey:
				// Back edge. The cycle starts where e.To sits on the stack; a
				// self-loop lands on the top frame and yields the single edge.
				pos := 0
				for k := range stack {
					if stack[k].node == e.To {
						pos = k
						break
					}
				}
				cycle := make([]Edge, 0, len(path)-pos+1)
				cycle = append(cycle, path[pos:]...)
				cycle = append(cycle, e)
				return &CycleError{Manifest: manifestName, Path: cycle}
			}
			// black: already fully explored and known acyclic. Re-walking it
			// would make a diamond quadratic and, worse, is how a shared
			// sub-DAG gets misreported as a cycle.
		}
	}
	return nil
}

// Ready is §9.1's ready predicate, and it is a PURE FUNCTION: no DB, no side
// effects, no spawning. It proposes; §5.1's human gate disposes.
//
// A task is ready when, for EVERY artifact in needs, both: the producing task is
// verified (its check went green), AND the artifact exists at its declared path,
// committed, on the producer's branch. Both, not either — producer-verified
// without the artifact means the check did not actually cover the handoff;
// artifact-present without verification means an untested artifact. A task with
// no needs is ready as soon as the run is created.
//
// `published` is the set of artifact ids §8.3 has verified as committed, which
// the caller reads from delegation_artifacts. Passing it in rather than querying
// keeps this function pure and keeps one scheduler tick to one read.
//
// This is the whole of 3a's scheduler — "which tasks have no unmet edges". The
// deadlock detector, the watchdogs and the rendezvous path are §§9.3/11/12 and
// are deferred.
func Ready(g Graph, states map[string]TaskState, published map[string]bool) []string {
	var out []string
	for _, id := range g.TaskIDs {
		// Only a task that has not been acted on yet can be PROPOSED. The
		// candidate states are enumerated here rather than routed through
		// TaskState.Terminal so that adding a state to state.go cannot silently
		// make a running task re-offerable — the compiler will not warn, but a
		// reader of this switch will.
		switch states[id] {
		case "", StatePending, StateReady:
		default:
			continue
		}
		unmet := false
		for _, art := range g.Needs[id] {
			producer, known := g.Producer[art]
			if !known || !published[art] {
				unmet = true
				break
			}
			// merged is downstream of verified: a producer whose work already
			// landed is not less green than one waiting at the gate.
			switch states[producer] {
			case StateVerified, StateMerged:
			default:
				unmet = true
			}
			if unmet {
				break
			}
		}
		if !unmet {
			out = append(out, id)
		}
	}
	return out
}
