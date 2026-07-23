package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
)

// Launcher is the slice of session.Launcher this package uses. An interface so
// spawn's ORDER — claim, assemble, launch, bind — can be tested without tmux;
// *session.Launcher satisfies it, and nothing here reimplements launching.
type Launcher interface {
	Launch(r session.Recipe, w, h int, now time.Time) (string, error)
}

var (
	// ErrOrchestratorExists is §7.3's refusal: the claim was rejected because
	// this project already has a live orchestrator. It names the winner so the
	// user can attach to it instead of wondering why nothing happened.
	ErrOrchestratorExists = errors.New("orchestrator: project already has a live orchestrator")
	// ErrUngrouped refuses the reserved root='' row (§2). There is no directory
	// to launch at and no coherent repo set, and the BACKEND rejects it — a
	// disabled button is a courtesy, not an enforcement.
	ErrUngrouped = errors.New("orchestrator: Ungrouped cannot have an orchestrator")
	// ErrProjectMissing is the guard for a root that is gone. tmux
	// new-session -c <nonexistent> exits 0 and silently starts in $HOME, so a
	// missing root must be refused here, loudly.
	ErrProjectMissing = errors.New("orchestrator: project root is missing or unusable")
	// ErrNotesDirMissing is §7.2's named error: notes_dir points into the
	// user's workspace and does not exist. Loom does not create directories in
	// the user's workspace, full stop — not even one it is about to write its
	// own pointer to.
	ErrNotesDirMissing = errors.New("orchestrator: notes directory does not exist")
	// ErrNoProject is a root with no project row.
	ErrNoProject = errors.New("orchestrator: no such project")
)

// ClaimGrace is how long a claim with no session name is left alone before the
// sweep deletes it (§7). The asymmetry is deliberate and stated there: a
// stranded claim row is cheap and recoverable, a double orchestrator is not.
const ClaimGrace = 60 * time.Second

// Service assembles briefs and spawns orchestrators. One per process; it holds
// no per-project state, because every durable fact lives in the store or on
// disk (§2, the orchestrator is disposable and so is this).
type Service struct {
	st       *store.Store
	launcher Launcher
	loomDir  string

	// now and git are seams for tests, not configuration. Production passes
	// neither.
	now func() time.Time
	git gitRunner
}

func New(st *store.Store, l Launcher, loomDir string) *Service {
	return &Service{st: st, launcher: l, loomDir: loomDir, now: time.Now, git: runGit}
}

// Result is what a successful spawn produced.
type Result struct {
	SessionName string
	BriefPath   string
	Brief       Brief
}

// IsOrchestratorSession reports whether a session row's tags mark it as an
// orchestrator. Exported so the callers that must special-case one —
// maybeAutoSummarize, which §7 says skips it — can ask rather than re-derive
// the token test. A second derivation is how one surface quietly stops
// agreeing with the others.
func IsOrchestratorSession(tags string) bool { return hasOrchTag(tags) }

// State returns the project's drift/brief ledger (§8's state.json), for the
// callers that RENDER it — the project-overview DTO wants spawn count, last
// session, assembled-at and the truncated-section list.
//
// It exists so the GUI never parses state.json itself. That file is a cache
// with a schema number whose corrupt-means-absent rule (§8, binding) is
// load-bearing: a second reader that treated a truncated write as an error,
// or that read a field this package later renames, would resurrect exactly the
// failure loadState was written to prevent. ok=false means "no usable state
// yet" — a project never briefed, or a file being rewritten right now — and is
// a normal answer to render as blanks, never an error to raise.
func (s *Service) State(root string) (State, bool) {
	return loadState(PathsFor(s.loomDir, root).State)
}

// Reassemble rewrites brief.md and state.json for a project WITHOUT spawning
// anything (§10's ReassembleBrief). It is how a human refreshes the drift
// report before deciding whether a new orchestrator is worth starting, and it
// costs nothing: assembly is local reads plus two SQLite queries (§11).
//
// It does not touch spawn_count or last_session — those are the spawn ledger,
// and a refresh is not a spawn.
func (s *Service) Reassemble(root, intent string) (Brief, error) {
	_, br, err := s.assembleAndWrite(root, intent)
	return br, err
}

// Spawn is §7's binding order. Every step's placement is load-bearing:
//
//	1 guard          — a missing root must never reach tmux (see ErrProjectMissing)
//	2 materialize    — but only ever under ~/.loom (§3)
//	3 CLAIM          — before any side effect, so two Loom instances produce one launch
//	4 assemble+write — the brief must exist before the seed points at it
//	5 launch         — Launcher.Launch mints the session id internally
//	6 tag + bind     — which is why 3 and 6 straddle it
//
// The disclosed failure mode: a launch that fails after a successful claim
// leaves a claim row with no session name. Sweep deletes it after ClaimGrace.
func (s *Service) Spawn(root, intent string, w, h int) (Result, error) {
	p, repos, err := s.project(root)
	if err != nil {
		return Result{}, err
	}

	notesDir, err := s.ensureNotesDir(p)
	if err != nil {
		return Result{}, err
	}

	now := s.now()
	claimed, existing, err := s.st.ClaimOrchestrator(p.Root, now.Unix())
	if err != nil {
		return Result{}, err
	}
	if !claimed {
		who := existing.SessionName
		if who == "" {
			who = "a spawn in flight"
		}
		return Result{}, fmt.Errorf("%w: %s", ErrOrchestratorExists, who)
	}

	// Re-reads the project row on purpose: ensureNotesDir may have just
	// materialized notes_dir, and the brief's Authorization scope names that
	// directory. Assembling from the pre-materialization row would tell the
	// orchestrator it may write nowhere.
	paths, br, err := s.assembleAndWrite(p.Root, intent)
	if err != nil {
		// The claim is left standing rather than rolled back: rolling it back
		// here would need a second CAS that could race a legitimate second
		// spawn, and the sweep already has the answer.
		return Result{}, err
	}

	name, err := s.launcher.Launch(session.Recipe{
		ProjectLabel: p.Name,
		Cwd:          p.Root,
		AddDirs:      addDirsFor(p.Root, notesDir, repos),
		Model:        "opus",
		// EXPLICIT, never inherited (§7, binding). Recipe.Argv appends
		// --permission-mode only when Mode != "", so "" means the ACCOUNT
		// default — and this account's default is auto mode, under which the
		// add-dir spike recorded a Write to an --add-dir'd sibling succeeding
		// with no prompt at all ("Allowed by auto mode classifier"). An
		// orchestrator launched that way has silent write access to every
		// member repo, which is instruction-level ownership reintroduced by
		// omission. acceptEdits is rejected for the same reason; plan is
		// rejected because it cannot write the notes and leaving it is a
		// keystroke Loom must not make on the user's behalf.
		Mode: "default",
		Seed: Seed(paths.Brief, intent),
	}, w, h, now)
	if err != nil {
		return Result{}, err
	}

	// Tag before bind. A SetTags failure is not fatal to the launch — the
	// fanout precedent (ui/app.go: "a SetTags failure is COUNTED as launched,
	// untagged") — but it IS visible: an untagged orchestrator is one the
	// echo-chamber guard will not exclude, so the failure is returned alongside
	// the session rather than swallowed.
	tagErr := s.st.SetTags(name, orchTag)
	bound, bindErr := s.st.BindOrchestratorSession(p.Root, name, "")
	res := Result{SessionName: name, BriefPath: paths.Brief, Brief: br}
	switch {
	case tagErr != nil:
		return res, fmt.Errorf("orchestrator: session %s launched but not tagged: %w", name, tagErr)
	case bindErr != nil:
		return res, fmt.Errorf("orchestrator: session %s launched but claim not bound: %w", name, bindErr)
	case !bound:
		// The claim was swept or taken over while the launch was in flight. The
		// session is real and tagged, so the next sweep adopts it; saying so is
		// better than a silently mislabelled singleton.
		return res, nil
	}

	// The spawn ledger is stamped after the launch succeeds, so spawn_count
	// counts sessions that exist rather than attempts that did not.
	if st, ok := loadState(paths.State); ok {
		st.SpawnCount++
		st.LastSession = name
		_ = saveState(paths.State, st)
	}
	return res, nil
}

// project loads and guards the project row (§7.1).
func (s *Service) project(root string) (store.Project, []store.ProjectRepo, error) {
	if root == store.UngroupedRoot {
		return store.Project{}, nil, ErrUngrouped
	}
	p, ok, err := s.st.GetProject(root)
	if err != nil {
		return store.Project{}, nil, err
	}
	if !ok {
		return store.Project{}, nil, fmt.Errorf("%w: %s", ErrNoProject, root)
	}
	if p.Missing || !dirUsable(p.Root) {
		return store.Project{}, nil, fmt.Errorf("%w: %s", ErrProjectMissing, p.Root)
	}
	repos, err := s.st.ListProjectRepos(p.Root)
	if err != nil {
		return store.Project{}, nil, err
	}
	return p, repos, nil
}

// ensureNotesDir is §7.2. Two branches, and the asymmetry is the whole point:
//
//   - empty ⇒ MATERIALIZE under ~/.loom and write the literal path back to the
//     row. Materializing rather than deriving-on-read is load-bearing:
//     RepointProject changes `root`, and a derived default would silently
//     relocate a project's whole brain on a directory rename.
//   - set but absent ⇒ create it only if it is inside ~/.loom. Anywhere else is
//     the user's workspace, and slice 1 deleted Loom's own "Loom creates the
//     directory" branch to keep "Loom writes nothing into the user's workspace
//     except on explicit request" absolute. So: a named error, and nothing
//     created.
func (s *Service) ensureNotesDir(p store.Project) (string, error) {
	paths := PathsFor(s.loomDir, p.Root)
	if p.NotesDir == "" {
		if err := os.MkdirAll(paths.NotesDir, 0o755); err != nil {
			return "", err
		}
		if err := s.st.SetProjectNotesDir(p.Root, paths.NotesDir, s.now().Unix()); err != nil {
			return "", err
		}
		return paths.NotesDir, nil
	}
	if dirUsable(p.NotesDir) {
		return p.NotesDir, nil
	}
	if under(s.loomDir, p.NotesDir) {
		if err := os.MkdirAll(p.NotesDir, 0o755); err != nil {
			return "", err
		}
		return p.NotesDir, nil
	}
	return "", fmt.Errorf("%w: %s (Loom does not create directories in your workspace — "+
		"create it, or pick another with Move notes…)", ErrNotesDirMissing, p.NotesDir)
}

// MoveNotes copies the three notes to dir and repoints the row (§3's *Move
// notes…* gesture). The source is LEFT IN PLACE, never deleted: Loom retires,
// never deletes (ARCHITECTURE §1), and the one gesture that could destroy an
// orchestrator's whole brain is not one to make irreversible.
//
// dir must already exist. That is the same rule as ensureNotesDir and for the
// same reason: the destination is chosen through OpenDirectoryDialog, which
// only returns directories that do.
func (s *Service) MoveNotes(root, dir string) error {
	p, _, err := s.project(root)
	if err != nil {
		return err
	}
	dir = projects.Canonical(dir)
	if !dirUsable(dir) {
		return fmt.Errorf("%w: %s", ErrNotesDirMissing, dir)
	}
	if p.NotesDir != "" && p.NotesDir != dir {
		for _, name := range NoteFiles {
			b, err := os.ReadFile(filepath.Join(p.NotesDir, name))
			if err != nil {
				continue // absent notes are the normal case, not a failure
			}
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
				return err
			}
		}
	}
	return s.st.SetProjectNotesDir(p.Root, dir, s.now().Unix())
}

// assembleAndWrite gathers every input, renders the brief, and writes brief.md
// and state.json. Shared by Spawn and Reassemble so there is exactly one
// assembly path — §8's "bootstrap and steady state are one code path, which is
// the only way the bootstrap path stays tested" applies to this too.
func (s *Service) assembleAndWrite(root, intent string) (Paths, Brief, error) {
	p, repos, err := s.project(root)
	if err != nil {
		return Paths{}, Brief{}, err
	}
	paths := PathsFor(s.loomDir, p.Root)
	prev, prevOK := loadState(paths.State)

	states := make([]RepoState, 0, len(repos))
	labels := make(map[string]string, len(repos)+1)
	dirs := []string{p.Root}
	for _, r := range repos {
		states = append(states, repoState(s.git, r.Label, r.Path, r.Missing))
		labels[r.Path] = r.Label
		if r.Path != p.Root {
			dirs = append(dirs, r.Path)
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Label < states[j].Label })

	notes := readNotes(p.NotesDir, prev, prevOK)

	var drift []string
	if prevOK {
		drift = driftLines(s.git, prev, states, notes)
	}

	resolver, err := s.resolver()
	if err != nil {
		return Paths{}, Brief{}, err
	}
	orchIDs, err := orchSessionIDs(s.st)
	if err != nil {
		return Paths{}, Brief{}, err
	}
	recent, err := recentWork(s.st, recentWorkInput{
		dirs: dirs, labels: labels, intent: cleanIntent(intent),
		resolver: resolver, orchIDs: orchIDs,
	})
	if err != nil {
		return Paths{}, Brief{}, err
	}

	now := s.now()
	br := Assemble(Input{
		ProjectName: p.Name, Root: p.Root, NotesDir: p.NotesDir,
		Repos: states, Notes: notes, Drift: drift, Recent: recent,
		Intent: intent, Now: now,
	})

	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		return Paths{}, Brief{}, err
	}
	if err := writeAtomic(paths.Brief, []byte(br.Text)); err != nil {
		return Paths{}, Brief{}, err
	}

	next := State{
		Schema: stateSchema, ProjectRoot: p.Root, NotesDir: p.NotesDir,
		AssembledAt: now.Unix(), SpawnCount: prev.SpawnCount, LastSession: prev.LastSession,
		BriefBytes: br.Bytes, BriefSHA256: br.SHA256, TruncatedSections: br.Truncated,
		Repos: map[string]RepoStamp{}, Notes: map[string]NoteStamp{},
	}
	for _, r := range states {
		if r.Missing || r.Err != "" {
			// Do NOT stamp a repo we could not read: stamping an empty head
			// would make the next brief report "history rewritten" for a repo
			// that never moved.
			if old, ok := prev.Repos[r.Path]; ok {
				next.Repos[r.Path] = old
			}
			continue
		}
		next.Repos[r.Path] = RepoStamp{Branch: r.Branch, Head: r.Head}
	}
	for _, n := range notes {
		if n.Exists {
			next.Notes[n.Name] = NoteStamp{SHA256: n.SHA256, Bytes: n.Bytes}
		}
	}
	if err := saveState(paths.State, next); err != nil {
		return Paths{}, Brief{}, err
	}
	return paths, br, nil
}

func (s *Service) resolver() (*projects.Resolver, error) {
	ps, err := s.st.ListProjects()
	if err != nil {
		return nil, err
	}
	repos, err := s.st.ListAllProjectRepos()
	if err != nil {
		return nil, err
	}
	return projects.NewResolver(ps, repos), nil
}

// Sweep is the recovery half of §7 and §9. Called from the same place
// projects.Sweep already runs. Three jobs, in this order:
//
//  1. adopt   — a live `orch`-tagged session with no row gets one. Adopt BEFORE
//     reap, the engine's own idiom: a row rebuilt from a live session
//     must exist before anything decides the project has no
//     orchestrator.
//  2. retire  — a claim whose session has gone terminal gets ended_at stamped
//     here, NOT by status.Engine: the engine stays project-unaware.
//  3. reap    — a claim older than ClaimGrace that never became a session.
//
// Returns the number of claims reaped, for the caller to surface. Errors are
// returned, never swallowed; a sweep that quietly stops working is how the
// singleton silently becomes a duplicate.
func (s *Service) Sweep() (reaped int64, err error) {
	now := s.now()

	resolver, err := s.resolver()
	if err != nil {
		return 0, err
	}
	live, err := s.st.Live()
	if err != nil {
		return 0, err
	}
	adopted := map[string]bool{}
	for _, row := range live {
		if !hasOrchTag(row.Tags) {
			continue
		}
		a, ok := resolver.Attribute(row.Cwd)
		if !ok || a.Root == store.UngroupedRoot {
			continue // no project owns it; nothing to be the singleton of
		}
		if _, err := s.st.AdoptOrchestrator(store.Orchestrator{
			ProjectRoot: a.Root, SessionName: row.Name,
			ClaudeSessionID: row.ClaudeSessionID,
			SpawnedAt:       row.CreatedAt, EndedAt: -1,
		}); err != nil {
			return 0, err
		}
		adopted[row.Name] = true
	}

	rows, err := s.st.ListOrchestrators()
	if err != nil {
		return 0, err
	}
	for _, o := range rows {
		if o.EndedAt != -1 || o.SessionName == "" || adopted[o.SessionName] {
			continue
		}
		sess, ok, err := s.st.Get(o.SessionName)
		if err != nil {
			return 0, err
		}
		if !ok {
			// The session row is gone (a bulk clear). The claim outlived what
			// it pointed at; retire it at now rather than leaving a live claim
			// pointing at nothing.
			if _, err := s.st.EndOrchestrator(o.ProjectRoot, now.Unix()); err != nil {
				return 0, err
			}
			continue
		}
		if sess.EndedAt == -1 {
			continue
		}
		if _, err := s.st.EndOrchestrator(o.ProjectRoot, sess.EndedAt); err != nil {
			return 0, err
		}
	}

	return s.st.SweepStaleOrchestratorClaims(now.Add(-ClaimGrace).Unix())
}

// addDirsFor is §7.5's add-dir set: every member repo that is NOT already
// covered by the cwd, plus notes_dir when it lies outside the root.
//
// A repo under the root needs no --add-dir — cwd already grants it — and adding
// one anyway costs an argv entry that says nothing. A `missing` repo is
// excluded because session.validateDirs rejects a nonexistent add-dir and would
// fail the whole launch: losing one absent sibling is recoverable, losing the
// orchestrator is not.
//
// The add-dir spike is what makes this cheap: trust prompts ONCE, for the
// primary cwd only, and the siblings are granted silently. A five-repo
// orchestrator costs the user exactly one dialog.
func addDirsFor(root, notesDir string, repos []store.ProjectRepo) []string {
	seen := map[string]bool{root: true}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] || under(root, p) {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, r := range repos {
		if r.Missing || !dirUsable(r.Path) {
			continue
		}
		add(r.Path)
	}
	sort.Strings(out)
	add(notesDir)
	return out
}

// under reports whether p lies at or below root, SEGMENT-WISE. A raw string
// prefix would call `/a/bc` a child of `/a/b` — the same bug slice 1's resolver
// documents for five real sibling repos.
func under(root, p string) bool {
	if root == "" || p == "" {
		return false
	}
	root, p = filepath.Clean(root), filepath.Clean(p)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// dirUsable mirrors session.dirUsable (unexported there). `tmux new-session -c
// <nonexistent>` exits 0 and SILENTLY falls back to $HOME, so a stale path
// would start a real agent in the wrong directory with no error anywhere.
func dirUsable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// writeAtomic is the settings.go idiom: temp + rename, so a crash mid-write
// leaves the previous brief intact rather than a half one the agent would read
// as complete.
func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
