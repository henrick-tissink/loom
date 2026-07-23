package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/delegate"
	"github.com/henricktissink/loom/internal/store"
)

// Tests for the orchestration view's three gated entry points:
// OrchestrationSnapshot (arch.go), ProjectDocuments and ProjectDocument
// (orchestration.go). They live together because §3.1 is ONE rule across THREE
// doors and the route is deliberately open — a suite that covered two of them
// would go green on the door that leaks.
//
// The identifying strings below are deliberately distinctive so the
// marshalled-JSON assertions cannot pass by accident.

const (
	// Everything a hidden project's payload must never contain. Each is the
	// exact shape §3.1.4 enumerates: title, repo label, repo path, worktree,
	// brief path, authorization text, artifact id, check command, check tail.
	secretTaskID   = "acme-ledger"
	secretTitle    = "Ledger v2 for AcmeBank"
	secretRepoLbl  = "acmebank-api"
	secretBrief    = "/ws/hidden/.loom/briefs/acme-ledger.md"
	secretScope    = "May edit internal/acme/ledger; may NOT touch billing"
	secretArtifact = "acme-ledger-openapi"
	secretCheckCmd = "go test ./internal/acmeledger/..."
	secretCheckOut = "FAIL github.com/acme/bank/internal/acmeledger"
	secretWorktree = "/Users/h/.loom/wt/acme-ledger-1"

	// visRepoLbl is the visible project's repo label — the one a manifest may
	// legitimately address without crossing a project boundary.
	visRepoLbl = "visible-api"
)

// --- fixtures ---------------------------------------------------------------

// archApp is newTestApp plus the visible/hidden project pair, each with one
// member repo carrying a manifest-addressable label.
//
// It seeds its own rows rather than reusing seedProjects: UpsertProjectRepo is
// insert-only (ON CONFLICT DO NOTHING, the "never update" discipline), so a
// second upsert over the same path would silently keep the other fixture's
// label and every manifest here would address a repo that does not exist.
func archApp(t *testing.T) *App {
	t.Helper()
	a := newTestApp(t)
	for _, p := range []store.Project{
		{Root: visRoot, Name: "Visible", Origin: "discovered", CreatedAt: 1, UpdatedAt: 1},
		{Root: hidRoot, Name: "Client", Origin: "discovered", CreatedAt: 1, UpdatedAt: 1},
	} {
		if err := a.st.UpsertProject(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []store.ProjectRepo{
		{Path: visRepo, ProjectRoot: visRoot, Label: visRepoLbl, AddedAt: 1},
		{Path: hidRepo, ProjectRoot: hidRoot, Label: secretRepoLbl, AddedAt: 1},
	} {
		if err := a.st.UpsertProjectRepo(m); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// spawnAndBind walks a task through the real CAS path to a bound child session,
// because BindTaskSessionCAS only claims from `spawning` — writing the column
// directly would test a state the runner cannot produce.
func spawnAndBind(t *testing.T, a *App, runID int64, taskID, sessionName string) {
	t.Helper()
	if ok, err := a.st.AdvanceTaskCAS(runID, taskID, string(delegate.StatePending),
		string(delegate.StateSpawning), 140); err != nil || !ok {
		t.Fatalf("advance to spawning: ok=%v err=%v", ok, err)
	}
	if ok, err := a.st.BindTaskSessionCAS(runID, taskID, sessionName, 150); err != nil || !ok {
		t.Fatalf("bind session: ok=%v err=%v", ok, err)
	}
}

// secretManifest is a two-task run whose every renderable field is a marker
// string. `schema` names the artifact `acme-ledger` publishes; `ledger` consumes
// it, so there is one real edge.
func secretManifest(repoLabel string) delegate.Manifest {
	return delegate.Manifest{
		Version: delegate.ManifestVersion,
		Name:    "acme-rearchitecture",
		Project: "Client",
		Tasks: []delegate.Task{
			{
				ID: "acme-schema", Title: "Schema", Repo: repoLabel,
				Brief: secretBrief, Authorization: secretScope,
				Produces: []delegate.Artifact{{ID: secretArtifact, Path: "openapi.yaml"}},
				Check:    delegate.Check{Cmd: []string{"go", "build", "./..."}},
			},
			{
				ID: secretTaskID, Title: secretTitle, Repo: repoLabel,
				Brief: secretBrief, Authorization: secretScope,
				Needs: []string{secretArtifact},
				Check: delegate.Check{Cmd: strings.Fields(secretCheckCmd)},
			},
		},
	}
}

// seedRun writes a run and its task rows straight into the store, which is what
// OrchestrationSnapshot reads. The manifest is stored as a snapshot exactly as
// InsertDelegationRun does in production.
func seedArchRun(t *testing.T, a *App, root string, man delegate.Manifest) store.DelegationRun {
	t.Helper()
	b, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	return seedArchRawRun(t, a, root, man, string(b))
}

func seedArchRawRun(t *testing.T, a *App, root string, man delegate.Manifest, raw string) store.DelegationRun {
	t.Helper()
	run, err := a.st.InsertDelegationRun(man.Name, root, raw, "{}", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range man.Tasks {
		row := store.DelegationTask{
			RunID: run.ID, TaskID: task.ID, State: string(delegate.StatePending),
			RepoLabel: task.Repo, UpdatedAt: 100, CheckExit: -1,
		}
		if task.ID == secretTaskID {
			row.State = string(delegate.StateFailed)
			row.Worktree = secretWorktree
			row.Branch = "loom/" + secretTaskID
			row.CheckStatus = "fail"
			row.CheckExit = 1
			row.CheckOut = secretCheckOut
			row.SessionName = "loom-acme"
		}
		if err := a.st.InsertDelegationTask(row); err != nil {
			t.Fatal(err)
		}
	}
	return run
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- §3.1: the payload is the gate -----------------------------------------

// The strongest available form of the hiding test (§12): substring search over
// the MARSHALLED BYTES, not a struct walk. A struct walk asserts on the fields
// someone remembered to walk; the bytes are what actually crosses to the
// frontend.
func TestArchHidden_marshalledSnapshotCarriesNoIdentifyingString(t *testing.T) {
	a := archApp(t)
	seedArchRun(t, a, hidRoot, secretManifest(secretRepoLbl))

	// Visible first: the fixture must actually contain what we then assert is
	// absent, or the test proves nothing.
	before := mustJSON(t, a.OrchestrationSnapshot(hidRoot, 0, 0))
	for _, s := range []string{secretTaskID, secretTitle, secretRepoLbl, secretBrief,
		secretScope, secretArtifact, secretCheckOut, secretWorktree, hidRepo} {
		if !strings.Contains(before, s) {
			t.Fatalf("fixture is not load-bearing: %q absent from the visible payload", s)
		}
	}

	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	got := a.OrchestrationSnapshot(hidRoot, 0, 0)
	if !got.Hidden {
		t.Fatalf("hidden project returned a populated snapshot: %+v", got)
	}
	after := mustJSON(t, got)
	for _, s := range []string{secretTaskID, secretTitle, secretRepoLbl, secretBrief,
		secretScope, secretArtifact, secretCheckCmd, secretCheckOut, secretWorktree,
		hidRepo, hidRoot, "acme-rearchitecture", "Client"} {
		if strings.Contains(after, s) {
			t.Errorf("hidden snapshot leaked %q: %s", s, after)
		}
	}
	// §3.1.1 field by field: no rev, no run, no counts, no error text.
	if got.Rev != 0 || got.Run != nil || len(got.Runs) != 0 || len(got.Nodes) != 0 ||
		len(got.Edges) != 0 || len(got.Statuses) != 0 || len(got.Blocked) != 0 ||
		len(got.Warnings) != 0 || got.Error != "" || got.Strip != (StripDTO{}) {
		t.Errorf("hidden payload carries more than the marker: %+v", got)
	}
}

// §3.1.3: the hidden render is CONSTANT. A project with a run and one without
// must produce byte-identical payloads, or the seam leaks one bit.
func TestArchHidden_payloadIdenticalWithAndWithoutARun(t *testing.T) {
	withRun := archApp(t)
	seedArchRun(t, withRun, hidRoot, secretManifest(secretRepoLbl))
	if err := withRun.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	without := archApp(t)
	if err := without.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}

	got, want := mustJSON(t, withRun.OrchestrationSnapshot(hidRoot, 0, 0)),
		mustJSON(t, without.OrchestrationSnapshot(hidRoot, 0, 0))
	if got != want {
		t.Errorf("hidden payload varies with whether a run exists:\n with: %s\n without: %s", got, want)
	}

	// Same rule for the document set: a hidden project with documents on disk
	// must be indistinguishable from one without.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "ARCHITECTURE.md"),
		[]byte("# "+secretTitle+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	docsApp := newTestApp(t)
	if err := docsApp.st.UpsertProject(store.Project{Root: dir, Name: "Client", Origin: "discovered",
		CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := docsApp.SetProjectHidden(dir, true); err != nil {
		t.Fatal(err)
	}
	if got, want := mustJSON(t, docsApp.ProjectDocuments(dir)), mustJSON(t, without.ProjectDocuments(hidRoot)); got != want {
		t.Errorf("hidden document payload varies with whether documents exist:\n %s\n %s", got, want)
	}
}

// §3.1.1's fail-closed clause, across all three doors. An unattributable or
// unknown root is HIDDEN, not empty — "empty because nothing matched" is an
// accident of the data, and an accident is not a gate.
func TestArchHidden_failsClosedOnUnattributableRoot(t *testing.T) {
	a := archApp(t)
	seedArchRun(t, a, hidRoot, secretManifest(secretRepoLbl))

	for _, root := range []string{"", "/ws/nobody-knows-this", store.UngroupedRoot, hidRepo} {
		t.Run("root="+root, func(t *testing.T) {
			if got := a.OrchestrationSnapshot(root, 0, 0); !got.Hidden {
				t.Errorf("unattributable root %q did not fail closed: %+v", root, got)
			}
			if got := a.ProjectDocuments(root); !got.Hidden {
				t.Errorf("ProjectDocuments(%q) did not fail closed: %+v", root, got)
			}
		})
	}
}

// §12's round-trip: unhiding restores a payload byte-identical to the visible
// case. Slice 1's solo↔hidden restore discipline.
func TestArchHidden_unhideRestoresIdenticalPayload(t *testing.T) {
	a := archApp(t)
	seedArchRun(t, a, hidRoot, secretManifest(secretRepoLbl))
	before := mustJSON(t, a.OrchestrationSnapshot(hidRoot, 0, 0))

	for _, step := range []func() error{
		func() error { return a.SetProjectHidden(hidRoot, true) },
		func() error { return a.SetProjectHidden(hidRoot, false) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if after := mustJSON(t, a.OrchestrationSnapshot(hidRoot, 0, 0)); after != before {
		t.Errorf("unhide did not restore the payload:\n before %s\n after  %s", before, after)
	}
}

// Solo is the other half of §6's predicate: another project's solo suppresses
// this one; solo on THIS project must not.
func TestArchHidden_soloSuppressesOthersNotSelf(t *testing.T) {
	a := archApp(t)
	seedArchRun(t, a, hidRoot, secretManifest(secretRepoLbl))

	if err := a.SetProjectSolo(visRoot, true); err != nil {
		t.Fatal(err)
	}
	if got := a.OrchestrationSnapshot(hidRoot, 0, 0); !got.Hidden {
		t.Errorf("another project's solo did not suppress this one: %+v", got)
	}
	if err := a.SetProjectSolo(visRoot, false); err != nil {
		t.Fatal(err)
	}
	if err := a.SetProjectSolo(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	got := a.OrchestrationSnapshot(hidRoot, 0, 0)
	if got.Hidden {
		t.Fatalf("solo on this very project suppressed its own overview: %+v", got)
	}
	if len(got.Nodes) != 2 {
		t.Errorf("soloed project lost its own nodes: %+v", got.Nodes)
	}
}

// The regression on slice 1's deliberate exception. The overview is the SETTINGS
// screen: it must stay reachable while hidden, and Hide/Show plus Solo must keep
// working from it. A future "fix" toward route-level unreachability fails here,
// loudly, rather than quietly stranding the user on a screen with no way back.
func TestArchHidden_overviewStaysReachableAndTogglesKeepWorking(t *testing.T) {
	a := archApp(t)
	seedArchRun(t, a, hidRoot, secretManifest(secretRepoLbl))
	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}

	var row *ProjectDetailDTO
	for _, d := range a.ListProjectDetails() {
		if d.Root == hidRoot {
			row = &d
			break
		}
	}
	if row == nil {
		t.Fatal("ListProjectDetails dropped the hidden project: the overview it hosts is the only way to unhide")
	}
	if !row.Hidden {
		t.Errorf("the row must report its own hidden state so the button reads Show: %+v", row)
	}
	// Both toggles still function from the page.
	if err := a.SetProjectSolo(hidRoot, true); err != nil {
		t.Fatalf("Solo stopped working from a hidden project's overview: %v", err)
	}
	if err := a.SetProjectSolo(hidRoot, false); err != nil {
		t.Fatal(err)
	}
	if err := a.SetProjectHidden(hidRoot, false); err != nil {
		t.Fatalf("Show stopped working from a hidden project's overview: %v", err)
	}
	if got := a.OrchestrationSnapshot(hidRoot, 0, 0); got.Hidden {
		t.Error("unhiding from the page did not restore the view on the next poll")
	}
}

// §3.1.4, and the correction this file makes to it.
//
// Two assertions, and the second is the one that matters. A node naming a repo
// this project does not own — here, the hidden client's — becomes an opaque
// placeholder; that is the fail-closed direction, and it is the shape §3.1.4's
// cross-project case takes in 3a, where a run is contained to exactly one
// project (delegation §3) so a node cannot legitimately sit in another one's
// tree. A node whose repo IS this project's stays visible even though its
// worktree lives under ~/.loom, where
// no project claims it — §3.1.4 as written says `res.Visible(repo, worktree)`,
// and Resolver.Visible fails closed on an unattributable directory, so the
// literal rule would blank every node of every run the instant anything is
// hidden. This test fails under the literal reading.
func TestArchNodeFiltering_repoDecidesAndWorktreeDoesNot(t *testing.T) {
	a := archApp(t)
	man := secretManifest(secretRepoLbl)
	// One task in the VISIBLE project's repo, holding a worktree under ~/.loom.
	man.Tasks = append(man.Tasks, delegate.Task{
		ID: "vis-task", Title: "Visible work", Repo: visRepoLbl,
		Authorization: "may edit", Needs: []string{secretArtifact},
		Check: delegate.Check{Cmd: []string{"go", "test", "./..."}},
	})
	run, err := a.st.InsertDelegationRun(man.Name, visRoot, mustJSON(t, man), "{}", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range man.Tasks {
		row := store.DelegationTask{RunID: run.ID, TaskID: task.ID, RepoLabel: task.Repo,
			State: string(delegate.StatePending), CheckExit: -1, UpdatedAt: 100}
		if task.ID == "vis-task" {
			row.State = string(delegate.StateRunning)
			row.SessionName = "loom-vis"
			row.Worktree = filepath.Join(os.Getenv("HOME"), ".loom", "wt", "vis-task")
		}
		if err := a.st.InsertDelegationTask(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}

	got := a.OrchestrationSnapshot(visRoot, 0, 0)
	if got.Hidden {
		t.Fatal("the run's own project is visible; its snapshot must not be gated")
	}
	byID := map[string]GraphNodeDTO{}
	for _, n := range got.Nodes {
		byID[n.ID] = n
	}
	vis, ok := byID["vis-task"]
	if !ok || vis.Hidden {
		t.Fatalf("a node in a VISIBLE repo was blanked because its worktree is under ~/.loom: %+v", got.Nodes)
	}
	if vis.Title != "Visible work" {
		t.Errorf("visible node lost its identity: %+v", vis)
	}
	if got.Strip.HiddenNodes != 2 {
		t.Errorf("bare hidden count wrong: got %d want 2", got.Strip.HiddenNodes)
	}
	// Structure preserved: three nodes and the same edge count as unhidden.
	if len(got.Nodes) != 3 || len(got.Edges) != 2 {
		t.Errorf("hiding changed the topology: %d nodes, %d edges", len(got.Nodes), len(got.Edges))
	}
	// And the placeholders carry nothing (the marshalled form again).
	raw := mustJSON(t, got)
	for _, s := range []string{secretTaskID, secretTitle, secretRepoLbl, secretBrief,
		secretScope, secretArtifact, secretCheckOut, secretWorktree} {
		if strings.Contains(raw, s) {
			t.Errorf("cross-project placeholder leaked %q", s)
		}
	}
	// A hidden node can never be human-blocking, even though its check failed.
	for _, b := range got.Blocked {
		if strings.HasPrefix(b.ID, "hidden-") || b.Title == "" {
			t.Errorf("blocked-on-you named a hidden node: %+v", b)
		}
	}
}

// --- §4.2: containment ------------------------------------------------------

// ProjectDocument attributes FIRST and admits second, with containment as an
// ADDITIONAL check. Traversal out of the tree is refused; a refusal is visible;
// and a refusal from a hidden project echoes nothing at all.
func TestArchDocument_containmentAndVisibilityOrder(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "docs", "ARCHITECTURE.md")
	if err := os.WriteFile(inside, []byte("# In tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(outside, "secrets.md")
	if err := os.WriteFile(escape, []byte("# "+secretTitle+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notMD := filepath.Join(root, "docs", "id_rsa")
	if err := os.WriteFile(notMD, []byte("PRIVATE KEY"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t)
	if err := a.st.UpsertProject(store.Project{Root: root, Name: "Doc", Origin: "discovered",
		CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	if got := a.ProjectDocument(inside); got.Doc == nil {
		t.Fatalf("in-tree document refused: %+v", got)
	}
	for _, tc := range []struct{ name, path string }{
		{"relative escape", filepath.Join(root, "docs", "..", "..", filepath.Base(outside), "secrets.md")},
		{"absolute escape", escape},
		{"not markdown", notMD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := a.ProjectDocument(tc.path)
			if got.Doc != nil {
				t.Fatalf("containment let %s through: %+v", tc.name, got)
			}
			if got.Error == "" && got.Refusal == nil {
				t.Errorf("refusal was silent; §4.2 binds it to be visible: %+v", got)
			}
		})
	}

	// Visibility runs FIRST, so a refusal never echoes a path from a hidden
	// project — including the path the caller passed in.
	if err := a.SetProjectHidden(root, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{inside, notMD} {
		got := a.ProjectDocument(p)
		if !got.Hidden {
			t.Fatalf("hidden project served %q: %+v", p, got)
		}
		if raw := mustJSON(t, got); strings.Contains(raw, root) || strings.Contains(raw, secretTitle) {
			t.Errorf("hidden document refusal echoed a path: %s", raw)
		}
	}
}

// --- §9 degradation ---------------------------------------------------------

// Every row here must render as NAMED, VISIBLE text. No blank panels, no
// silence: the assertions are on content, not on the absence of a panic.
func TestArchSnapshot_degradation(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T, a *App)
		verify func(t *testing.T, got OrchestrationDTO)
	}{
		{
			name:  "no run at all is not an error",
			setup: func(t *testing.T, a *App) {},
			verify: func(t *testing.T, got OrchestrationDTO) {
				if got.Error != "" || got.Run != nil || len(got.Runs) != 0 {
					t.Errorf("absence of orchestration rendered as a state: %+v", got)
				}
				if got.Hidden {
					t.Error("a visible project with no run must not read as hidden")
				}
			},
		},
		{
			name: "manifest is not JSON",
			setup: func(t *testing.T, a *App) {
				seedArchRawRun(t, a, visRoot, delegate.Manifest{Name: "broken"}, "{\n  \"manifest\": 1,\n  oops\n}")
			},
			verify: func(t *testing.T, got OrchestrationDTO) {
				if !strings.Contains(got.Error, "line 3") {
					t.Errorf("parse error must name line and column, got %q", got.Error)
				}
			},
		},
		{
			name: "schema newer than this build",
			setup: func(t *testing.T, a *App) {
				m := delegate.Manifest{Version: 2, Name: "future"}
				seedArchRun(t, a, visRoot, m)
			},
			verify: func(t *testing.T, got OrchestrationDTO) {
				if !strings.Contains(got.Error, "schema 2") || !strings.Contains(got.Error, "update Loom") {
					t.Errorf("newer schema must be named, not guessed at: %q", got.Error)
				}
				if len(got.Nodes) != 0 {
					t.Error("no partial draw for an unknown schema")
				}
			},
		},
		{
			name: "needs names an artifact nobody publishes",
			setup: func(t *testing.T, a *App) {
				seedArchRun(t, a, visRoot, delegate.Manifest{
					Version: delegate.ManifestVersion, Name: "dangling",
					Tasks: []delegate.Task{{ID: "solo", Title: "Solo", Repo: visRepoLbl,
						Authorization: "x", Needs: []string{"nobody-makes-this"},
						Check: delegate.Check{Cmd: []string{"true"}}}},
				})
			},
			verify: func(t *testing.T, got OrchestrationDTO) {
				var ghost *GraphNodeDTO
				for i, n := range got.Nodes {
					if n.Kind == "ghost" {
						ghost = &got.Nodes[i]
					}
				}
				if ghost == nil || !strings.Contains(ghost.Title, "nobody-makes-this") {
					t.Fatalf("dangling need did not become a named ghost: %+v", got.Nodes)
				}
				if len(got.Warnings) == 0 {
					t.Error("ghost node without a warning is a silent degradation")
				}
				if len(got.Edges) != 1 {
					t.Errorf("ghost must be wired to its consumer: %+v", got.Edges)
				}
			},
		},
		{
			name: "dependency cycle",
			setup: func(t *testing.T, a *App) {
				seedArchRun(t, a, visRoot, delegate.Manifest{
					Version: delegate.ManifestVersion, Name: "cyclic",
					Tasks: []delegate.Task{
						{ID: "a", Repo: visRepoLbl, Authorization: "x", Needs: []string{"b-art"},
							Produces: []delegate.Artifact{{ID: "a-art"}}, Check: delegate.Check{Cmd: []string{"true"}}},
						{ID: "b", Repo: visRepoLbl, Authorization: "x", Needs: []string{"a-art"},
							Produces: []delegate.Artifact{{ID: "b-art"}}, Check: delegate.Check{Cmd: []string{"true"}}},
					},
				})
			},
			verify: func(t *testing.T, got OrchestrationDTO) {
				if len(got.Nodes) != 2 {
					t.Fatalf("a cycle must still draw: %+v", got)
				}
				flagged := 0
				for _, e := range got.Edges {
					if e.Cycle {
						flagged++
					}
				}
				if flagged == 0 {
					t.Error("cycle edges not flagged")
				}
				if len(got.Warnings) == 0 || !strings.Contains(got.Warnings[0], "cycle") {
					t.Errorf("cycle members not named: %+v", got.Warnings)
				}
				if got.Strip.LongestChain == 0 {
					t.Error("analysis must return a number over a cyclic graph, not hang or zero out")
				}
			},
		},
		{
			name: "child with no check, no scope and no worktree",
			setup: func(t *testing.T, a *App) {
				man := delegate.Manifest{Version: delegate.ManifestVersion, Name: "bare",
					Tasks: []delegate.Task{{ID: "bare", Title: "Bare", Repo: visRepoLbl}}}
				run, err := a.st.InsertDelegationRun(man.Name, visRoot, mustJSON(t, man), "{}", 100)
				if err != nil {
					t.Fatal(err)
				}
				if err := a.st.InsertDelegationTask(store.DelegationTask{RunID: run.ID, TaskID: "bare",
					RepoLabel: visRepoLbl, State: string(delegate.StateRunning),
					SessionName: "loom-bare", CheckExit: -1, UpdatedAt: 100}); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, got OrchestrationDTO) {
				if len(got.Nodes) != 1 {
					t.Fatalf("want one node, got %+v", got.Nodes)
				}
				want := []string{"no check declared", "brief declares no authorization scope", "not isolated"}
				for _, w := range want {
					if !containsStr(got.Nodes[0].Warnings, w) {
						t.Errorf("missing chip %q: %+v", w, got.Nodes[0].Warnings)
					}
				}
			},
		},
		{
			name: "run id from another project is refused, not looked up",
			setup: func(t *testing.T, a *App) {
				seedArchRun(t, a, hidRoot, secretManifest(secretRepoLbl))
			},
			verify: func(t *testing.T, got OrchestrationDTO) {},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := archApp(t)
			tc.setup(t, a)
			tc.verify(t, a.OrchestrationSnapshot(visRoot, 0, 0))
		})
	}
}

// The cross-project run-id arm of the row above, spelled out: asking the
// VISIBLE project's seam for a run that belongs to the hidden project must
// return that project's nothing, not its graph.
func TestArchSnapshot_runIDFromAnotherProjectIsRefused(t *testing.T) {
	a := archApp(t)
	run := seedArchRun(t, a, hidRoot, secretManifest(secretRepoLbl))

	got := a.OrchestrationSnapshot(visRoot, run.ID, 0)
	if got.Run != nil || len(got.Nodes) != 0 {
		t.Fatalf("a run id crossed a project boundary: %+v", got)
	}
	if raw := mustJSON(t, got); strings.Contains(raw, secretTitle) {
		t.Errorf("cross-project run leaked: %s", raw)
	}
}

// --- §7: rev-gated full payload --------------------------------------------

func TestArchSnapshot_revGatesTheLayoutHalfOnly(t *testing.T) {
	a := archApp(t)
	run := seedArchRun(t, a, visRoot, secretManifest(visRepoLbl))

	full := a.OrchestrationSnapshot(visRoot, 0, 0)
	if full.Unchanged || len(full.Nodes) == 0 || full.Rev == 0 {
		t.Fatalf("first call must carry the full payload and a non-zero rev: %+v", full)
	}

	same := a.OrchestrationSnapshot(visRoot, 0, full.Rev)
	if !same.Unchanged || len(same.Nodes) != 0 || len(same.Edges) != 0 {
		t.Errorf("unchanged rev still re-sent the layout half: %+v", same)
	}
	if len(same.Statuses) == 0 || same.Strip.Nodes == 0 {
		t.Error("the per-tick half must always be sent, rev or not")
	}

	// A CHECK RESULT is the patch-in-place half: it must not move the rev, or
	// the graph re-lays out under the user's cursor on every check.
	if ok, err := a.st.RecordTaskCheckCAS(run.ID, "acme-schema", string(delegate.StatePending),
		string(delegate.StateVerified), "pass", 0, "ok", "abc", 200); err != nil || !ok {
		t.Fatalf("check CAS: ok=%v err=%v", ok, err)
	}
	afterCheck := a.OrchestrationSnapshot(visRoot, 0, full.Rev)
	if afterCheck.Rev != full.Rev {
		t.Errorf("a check result moved the topology rev: %d → %d", full.Rev, afterCheck.Rev)
	}
	if afterCheck.Statuses[0].State != string(delegate.StateVerified) {
		t.Errorf("the status half did not carry the new state: %+v", afterCheck.Statuses)
	}

	// Hiding IS a topology change: the placeholder must not sit on screen until
	// something unrelated moves.
	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	man := secretManifest(visRepoLbl)
	man.Tasks[0].Repo = secretRepoLbl
	if _, err := a.st.InsertDelegationRun("second", visRoot, mustJSON(t, man), "{}", 300); err != nil {
		t.Fatal(err)
	}
	if got := a.OrchestrationSnapshot(visRoot, 0, full.Rev); got.Rev == full.Rev {
		t.Error("a changed node set did not move the rev")
	}
}

// --- §2 conformance ---------------------------------------------------------

// These encode the evidence constraints slice 1 §11 paid for. Deleting one
// re-opens the failure it was written against.
func TestArchConformance_evidenceConstraints(t *testing.T) {
	a := archApp(t)
	man := secretManifest(visRepoLbl)
	run := seedArchRun(t, a, visRoot, man)

	// A child that is talking to the human and whose check has NOT run.
	if err := a.st.SetTaskPendingSeed(run.ID, "acme-schema", "", 150); err != nil {
		t.Fatal(err)
	}
	spawnAndBind(t, a, run.ID, "acme-schema", "loom-talky")
	if err := a.st.Upsert(store.SessionRow{Name: "loom-talky", ClaudeSessionID: "cs-1",
		Cwd: visRepo, EndedAt: -1, ExitCode: -1, LastStatus: "needs_you"}); err != nil {
		t.Fatal(err)
	}

	got := a.OrchestrationSnapshot(visRoot, 0, 0)
	st := statusByID(got, "acme-schema")
	if st.SessionStatus != "needs_you" {
		t.Fatalf("session status not fused: %+v", st)
	}
	// §2.1: self-report is not a state this UI can express. The lifecycle stays
	// where the CAS column put it and the check result is empty — there is no
	// field a talkative child can move.
	if st.State != string(delegate.StateRunning) || st.CheckStatus != "" {
		t.Errorf("a needs_you child promoted its own completion: %+v", st)
	}
	if !isBlocked(got, "acme-schema") {
		t.Error("a needs_you child must be human-blocking")
	}
	// §2.1 again on the other node: check.last.ok == false is human-blocking.
	if !isBlocked(got, secretTaskID) {
		t.Errorf("a failed check is not on the blocked-on-you strip: %+v", got.Blocked)
	}

	// §2.2: no orchestrator prose about a child exists anywhere in the DTO. The
	// strongest available form is a field-name assertion over the marshalled
	// payload — a field that does not exist cannot be rendered.
	raw := mustJSON(t, got)
	for _, banned := range []string{"review", "opinion", "assessment", "critique", "feedback"} {
		if strings.Contains(strings.ToLower(raw), "\""+banned) {
			t.Errorf("the DTO carries an orchestrator-opinion field %q: %s", banned, raw)
		}
	}
	// §2.3/§2.5: isolation and authorization scope are carried verbatim so their
	// presence is auditable.
	n := nodeByID(got, secretTaskID)
	if n.Worktree != secretWorktree || n.Authorization != secretScope {
		t.Errorf("isolation/authorization not displayed verbatim: %+v", n)
	}
}

// §5.3's non-negotiable field, and §14's most likely field failure: a node's
// live status must resolve THROUGH the claude session id, because a resume
// mints a new tmux name. Keying on session_name alone greys out the graph the
// moment a child is resumed.
func TestArchSnapshot_statusResolvesThroughClaudeSessionID(t *testing.T) {
	a := archApp(t)
	run := seedArchRun(t, a, visRoot, secretManifest(visRepoLbl))
	spawnAndBind(t, a, run.ID, "acme-schema", "loom-original")
	// The original row: finished, because the resume replaced it.
	if err := a.st.Upsert(store.SessionRow{Name: "loom-original", ClaudeSessionID: "cs-resume",
		Cwd: visRepo, CreatedAt: 100, EndedAt: 200, ExitCode: 0, LastStatus: "done"}); err != nil {
		t.Fatal(err)
	}
	// The resumed row: a DIFFERENT tmux name, the SAME claude session id.
	if err := a.st.Upsert(store.SessionRow{Name: "loom-resumed", ClaudeSessionID: "cs-resume",
		Cwd: visRepo, CreatedAt: 300, EndedAt: -1, ExitCode: -1, LastStatus: "running"}); err != nil {
		t.Fatal(err)
	}

	st := statusByID(a.OrchestrationSnapshot(visRoot, 0, 0), "acme-schema")
	if st.SessionStatus != "running" {
		t.Errorf("node status keyed on the tmux name, not the claude session id: %+v", st)
	}
}

// --- §5.2 analysis ----------------------------------------------------------

func TestArchAnalysis_readyBottleneckAndChain(t *testing.T) {
	// a → b → c, plus an independent d. Nothing published, so only the roots
	// are ready and `a` blocks two descendants while `d` blocks none.
	man := delegate.Manifest{Version: delegate.ManifestVersion, Name: "chain",
		Tasks: []delegate.Task{
			{ID: "a", Repo: visRepoLbl, Authorization: "x", Check: delegate.Check{Cmd: []string{"true"}},
				Produces: []delegate.Artifact{{ID: "a-art"}}},
			{ID: "b", Repo: visRepoLbl, Authorization: "x", Check: delegate.Check{Cmd: []string{"true"}},
				Needs: []string{"a-art"}, Produces: []delegate.Artifact{{ID: "b-art"}}},
			{ID: "c", Repo: visRepoLbl, Authorization: "x", Check: delegate.Check{Cmd: []string{"true"}},
				Needs: []string{"b-art"}},
			{ID: "d", Repo: visRepoLbl, Authorization: "x", Check: delegate.Check{Cmd: []string{"true"}}},
		}}

	tests := []struct {
		name       string
		merged     []string
		published  []string
		wantReady  []string
		wantBottle string
		wantChain  int
	}{
		{name: "nothing done", wantReady: []string{"a", "d"}, wantBottle: "a", wantChain: 3},
		{
			name: "a merged and published", merged: []string{"a"}, published: []string{"a-art"},
			wantReady: []string{"b", "d"}, wantBottle: "b", wantChain: 2,
		},
		{
			name: "everything but c done", merged: []string{"a", "b", "d"},
			published: []string{"a-art", "b-art"},
			wantReady: []string{"c"}, wantBottle: "", wantChain: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := archApp(t)
			run := seedArchRun(t, a, visRoot, man)
			for _, id := range tc.merged {
				if ok, err := a.st.AdvanceTaskCAS(run.ID, id, string(delegate.StatePending),
					string(delegate.StateMerged), 200); err != nil || !ok {
					t.Fatalf("advance %s: %v %v", id, ok, err)
				}
			}
			for _, art := range tc.published {
				if err := a.st.UpsertDelegationArtifact(store.DelegationArtifact{RunID: run.ID,
					ArtifactID: art, TaskID: strings.TrimSuffix(art, "-art"), Path: art,
					PublishedAt: 150}); err != nil {
					t.Fatal(err)
				}
			}
			got := a.OrchestrationSnapshot(visRoot, 0, 0)

			var ready []string
			for _, s := range got.Statuses {
				if s.Ready {
					ready = append(ready, s.ID)
				}
			}
			if strings.Join(ready, ",") != strings.Join(tc.wantReady, ",") {
				t.Errorf("ready set: got %v want %v", ready, tc.wantReady)
			}
			if got.Strip.Bottleneck != tc.wantBottle {
				t.Errorf("bottleneck: got %q want %q", got.Strip.Bottleneck, tc.wantBottle)
			}
			if got.Strip.LongestChain != tc.wantChain {
				t.Errorf("longest remaining chain: got %d want %d", got.Strip.LongestChain, tc.wantChain)
			}
			// §5.2 binding: the chain is in NODES. Nothing on the strip may be
			// rendered as a duration.
			if got.Strip.Nodes != 4 || got.Strip.Repos != 1 || got.Strip.SharedRepos != 1 {
				t.Errorf("cohesion read wrong: %+v", got.Strip)
			}
		})
	}
}

// The degraded shapes: a nil store and a project that exists with no runs must
// each return an empty DTO, never a panic (the ListSessions/ListRecent
// precedent).
func TestArchSnapshot_degradedAppReturnsEmptyNotPanic(t *testing.T) {
	bare := newApp(nil, nil, nil, nil, nil, func() time.Time { return time.Unix(1000, 0) })
	got := bare.OrchestrationSnapshot("/anything", 0, 0)
	if got.Nodes == nil || got.Runs == nil || got.Statuses == nil {
		t.Errorf("degraded payload must carry empty slices the frontend can index: %+v", got)
	}
	// §3.1.1's fail-closed clause reaches the degraded app too: with no
	// resolver there is no authority that could say a root is visible, and the
	// payload is the only gate. This is deliberately STRICTER than the rail's
	// nil-resolver rule (projects.go: "no authority at all means no
	// filtering"), because an empty rail blamed on Loom is recoverable and a
	// client's brief text on a shared screen is not.
	if !got.Hidden {
		t.Error("a degraded app must fail closed on this surface, not open")
	}
	if got := bare.ProjectDocuments("/anything"); got.Documents == nil {
		t.Errorf("degraded document payload must be indexable: %+v", got)
	}
}

// --- helpers ---------------------------------------------------------------

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func nodeByID(d OrchestrationDTO, id string) GraphNodeDTO {
	for _, n := range d.Nodes {
		if n.ID == id {
			return n
		}
	}
	return GraphNodeDTO{}
}

func statusByID(d OrchestrationDTO, id string) NodeStatusDTO {
	for _, s := range d.Statuses {
		if s.ID == id {
			return s
		}
	}
	return NodeStatusDTO{}
}

func isBlocked(d OrchestrationDTO, id string) bool {
	for _, b := range d.Blocked {
		if b.ID == id {
			return true
		}
	}
	return false
}

// §5.6 puts layout in Go and has the frontend paint returned coordinates. This
// asserts the seam: the payload carries a position for every node and a stage
// that contains them, and it carries them on the LAYOUT half only.
//
// The `unchanged` half of this matters as much as the first: coordinates that
// were re-sent on a status tick would be re-applied by the frontend, which is
// the re-layout-under-the-cursor §7.3 exists to forbid.
func TestArchSnapshot_carriesLayoutCoordinatesOnTheLayoutHalfOnly(t *testing.T) {
	a := archApp(t)
	seedArchRun(t, a, visRoot, secretManifest(visRepoLbl))

	full := a.OrchestrationSnapshot(visRoot, 0, 0)
	if len(full.Nodes) == 0 {
		t.Fatalf("no nodes: %+v", full)
	}
	if full.Layout.Width <= 0 || full.Layout.Height <= 0 {
		t.Fatalf("no stage geometry: %+v", full.Layout)
	}
	if full.Layout.NodeW <= 0 || full.Layout.NodeH <= 0 {
		t.Errorf("the card size must travel with the coordinates — the painter needs it to "+
			"aim an edge at a node's midpoint: %+v", full.Layout)
	}
	seen := map[string]bool{}
	for _, n := range full.Nodes {
		if n.X < 0 || n.Y < 0 {
			t.Errorf("node %s at (%d,%d) is off the stage", n.ID, n.X, n.Y)
		}
		if n.X+full.Layout.NodeW > full.Layout.Width || n.Y+full.Layout.NodeH > full.Layout.Height {
			t.Errorf("node %s at (%d,%d) falls outside the %dx%d stage",
				n.ID, n.X, n.Y, full.Layout.Width, full.Layout.Height)
		}
		key := fmt.Sprintf("%d,%d", n.X, n.Y)
		if seen[key] {
			t.Errorf("two nodes share the position %s", key)
		}
		seen[key] = true
	}

	same := a.OrchestrationSnapshot(visRoot, 0, full.Rev)
	if !same.Unchanged {
		t.Fatalf("expected the unchanged reply: %+v", same)
	}
	if same.Layout != (LayoutDTO{}) {
		t.Errorf("the stage geometry was re-sent on a status tick: %+v — the client already "+
			"holds it, and re-applying it is the re-layout §7.3 forbids", same.Layout)
	}
}

// A hidden node keeps its place in the picture (§3.1.4: the topology stays
// structurally true), so it gets coordinates like any other. A placeholder
// with no position would be a hole that says "something was here" far more
// loudly than the placeholder does.
func TestArchSnapshot_hiddenNodesAreStillPlaced(t *testing.T) {
	a := archApp(t)
	man := secretManifest(visRepoLbl)
	man.Tasks[0].Repo = secretRepoLbl
	seedArchRun(t, a, visRoot, man)
	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}

	snap := a.OrchestrationSnapshot(visRoot, 0, 0)
	hiddenSeen := false
	for _, n := range snap.Nodes {
		if !n.Hidden {
			continue
		}
		hiddenSeen = true
		if n.X < 0 || n.Y < 0 || snap.Layout.Width <= 0 {
			t.Errorf("hidden node %s was not placed: (%d,%d) on %+v", n.ID, n.X, n.Y, snap.Layout)
		}
	}
	if !hiddenSeen {
		t.Fatal("the fixture produced no hidden node")
	}
}
