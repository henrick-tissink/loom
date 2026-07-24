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
	"github.com/henricktissink/loom/internal/gitdiff"
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

// §12.2's and §12.3's flag sentences travel with the status, composed in Go.
//
// The frontend must not own this wording: two of the sentences are BINDING —
// §12.3.3's drift says "changed since spawn" and never "the child wrote this",
// because the walk cannot tell the human's own edits from a child's — and a
// frontend dictionary is the second place that hedge gets dropped. A hidden
// placeholder carries an EMPTY list, never a null: the painter indexes it
// without a guard.
func TestArchSnapshot_flagDetailsTravelAndAreEmptyWhenHidden(t *testing.T) {
	a := archApp(t)
	seedArchRun(t, a, visRoot, secretManifest(visRepoLbl))
	runs, err := a.st.ListDelegationRuns(visRoot)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListDelegationRuns: %v %v", len(runs), err)
	}
	if err := a.st.SetTaskFlags(runs[0].ID, secretTaskID,
		delegate.EncodeFlags(delegate.Flags{delegate.FlagOutsideWrites: true}), 20); err != nil {
		t.Fatal(err)
	}

	got := a.OrchestrationSnapshot(visRoot, 0, 0)
	var seen bool
	for _, st := range got.Statuses {
		if st.FlagDetails == nil {
			t.Fatalf("status %q carries a null flagDetails", st.ID)
		}
		for _, d := range st.FlagDetails {
			if d.Name != string(delegate.FlagOutsideWrites) {
				continue
			}
			seen = true
			// The hedge, both halves: the claim is about CHANGE, and the
			// sentence says out loud that Loom cannot attribute it.
			for _, want := range []string{"CHANGED SINCE SPAWN", "cannot tell the human's own edits"} {
				if !strings.Contains(d.Note, want) {
					t.Errorf("outside-writes note = %q, want it to contain %q", d.Note, want)
				}
			}
			if !d.Loud {
				t.Errorf("outside-writes is not loud: %+v", d)
			}
		}
	}
	if !seen {
		t.Fatalf("the flag's sentence never reached the payload: %+v", got.Statuses)
	}
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

// Stage 4d's diagram is a SECOND copy of the document's identifying text, in a
// different shape: internal/arch lifts node ids and labels out of a mermaid
// fence and hangs them off the block as structured data. If the document body is
// suppressed for a hidden project and the Diagram is not, every node label
// crosses to the frontend anyway — and a struct walk would not notice, because
// nobody walking DocumentDTO thinks to look inside Block.Diagram.Nodes.
//
// Substring search over the MARSHALLED BYTES, both doors, with a fixture that is
// asserted load-bearing first: a test whose fixture has no fence proves nothing
// about the fence.
func TestArchDocument_hiddenSuppressesTheDiagramWithTheBody(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(root, "docs", "ARCHITECTURE.md")
	// The secret appears ONLY inside the fence, as a node label. A copy in the
	// prose would let a leak from the body pass as a leak from the diagram.
	body := "# Overview\n\n```mermaid\nflowchart LR\n  api[" + secretTitle + "]\n  api --> db[" + secretRepoLbl + "]\n```\n"
	if err := os.WriteFile(doc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t)
	if err := a.st.UpsertProject(store.Project{Root: root, Name: "Client", Origin: "discovered",
		CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	// Load-bearing check: the visible payload really does carry the labels, and
	// really did parse the fence — otherwise this asserts the absence of
	// something that was never there.
	visible := a.ProjectDocument(doc)
	if visible.Doc == nil {
		t.Fatalf("visible document refused: %+v", visible)
	}
	diagrams := 0
	for _, b := range visible.Doc.Blocks {
		if b.Diagram != nil {
			diagrams++
		}
	}
	if diagrams != 1 {
		t.Fatalf("fixture is not load-bearing: %d diagrams parsed out of the fence", diagrams)
	}
	for _, s := range []string{secretTitle, secretRepoLbl} {
		if !strings.Contains(mustJSON(t, visible), s) {
			t.Fatalf("fixture is not load-bearing: %q absent from the visible document", s)
		}
	}
	if !strings.Contains(mustJSON(t, a.ProjectDocuments(root)), secretTitle) {
		t.Fatal("fixture is not load-bearing: the document SET does not carry the label either")
	}

	if err := a.SetProjectHidden(root, true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"ProjectDocument", mustJSON(t, a.ProjectDocument(doc))},
		{"ProjectDocuments", mustJSON(t, a.ProjectDocuments(root))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, s := range []string{secretTitle, secretRepoLbl, root, "flowchart"} {
				if strings.Contains(tc.raw, s) {
					t.Errorf("hidden document leaked %q through the diagram: %s", s, tc.raw)
				}
			}
		})
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

// --- §5.1's edge kinds, and §12.1 on the poll -------------------------------

// parkManifest is three tasks with ONE declared edge (a → b over `art-a`). `c`
// declares nothing, so any edge reaching it can only have been discovered
// mid-task — which is what makes it a usable park fixture.
func parkManifest(repoLabel string) delegate.Manifest {
	return delegate.Manifest{
		Version: delegate.ManifestVersion,
		Name:    "park-fixture",
		Project: "Visible",
		Tasks: []delegate.Task{
			{ID: "a", Title: "A", Repo: repoLabel,
				Produces: []delegate.Artifact{{ID: "art-a", Path: "a.txt"}},
				Check:    delegate.Check{Cmd: []string{"true"}}},
			{ID: "b", Title: "B", Repo: repoLabel, Needs: []string{"art-a"},
				Check: delegate.Check{Cmd: []string{"true"}}},
			{ID: "c", Title: "C", Repo: repoLabel,
				Check: delegate.Check{Cmd: []string{"true"}}},
		},
	}
}

// seedPlainRun writes a run and one `pending` row per task, with none of
// seedArchRun's marker-string decoration.
func seedPlainRun(t *testing.T, a *App, root string, man delegate.Manifest) store.DelegationRun {
	t.Helper()
	b, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	run, err := a.st.InsertDelegationRun(man.Name, root, string(b), "{}", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range man.Tasks {
		if err := a.st.InsertDelegationTask(store.DelegationTask{
			RunID: run.ID, TaskID: task.ID, State: string(delegate.StatePending),
			RepoLabel: task.Repo, UpdatedAt: 100, CheckExit: -1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return run
}

// setBlock parks a task on the durable column, which is what the view reads.
func setBlock(t *testing.T, a *App, runID int64, taskID string, b delegate.Block) {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.st.SetTaskBlock(runID, taskID, string(raw), 200); err != nil {
		t.Fatal(err)
	}
}

func edgeByPair(edges []GraphEdgeDTO, from, to string) (GraphEdgeDTO, bool) {
	for _, e := range edges {
		if e.From == from && e.To == to {
			return e, true
		}
	}
	return GraphEdgeDTO{}, false
}

// §5.1's table, on the wire. A declared dependency is `plan`; one that only
// exists because a child stopped and said so, or because a human accepted an
// amendment, is `park`. Both must be DRAWN — the picture has to show what the
// run is actually waiting on — and the two must be distinguishable, because
// §2.6 counts one of them as evidence the plan was wrong.
func TestArchSnapshot_edgeKinds(t *testing.T) {
	cases := []struct {
		name string
		// seed installs whatever makes the extra edge exist.
		seed     func(t *testing.T, a *App, run store.DelegationRun)
		from, to string
		wantKind string
	}{
		{name: "a declared dependency is plan",
			seed: func(*testing.T, *App, store.DelegationRun) {}, from: "a", to: "b", wantKind: EdgePlan},
		{name: "a live needs-artifact block is a park edge",
			seed: func(t *testing.T, a *App, run store.DelegationRun) {
				setBlock(t, a, run.ID, "c", delegate.Block{
					Version: delegate.BlockVersion, Task: "c", Kind: delegate.BlockNeedsArtifact,
					Need:    delegate.BlockNeed{Artifact: "art-a", From: "a"},
					Summary: "needs a's artifact",
				})
			},
			from: "a", to: "c", wantKind: EdgePark},
		{name: "an ACCEPTED amendment is a park edge too — it was discovered, not planned",
			seed: func(t *testing.T, a *App, run store.DelegationRun) {
				seq, err := a.st.AppendDelegationAmendment(run.ID, string(delegate.AmendEdge),
					delegate.EncodeAmendmentBody(delegate.Amendment{
						Kind: delegate.AmendEdge, Task: "c", From: "a", Artifact: "art-a"}), 210)
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := a.st.ApproveDelegationAmendmentCAS(run.ID, seq, 220); err != nil || !ok {
					t.Fatalf("approve amendment: ok=%v err=%v", ok, err)
				}
			},
			from: "a", to: "c", wantKind: EdgePark},
		{name: "an UNAPPROVED amendment contributes no edge at all",
			seed: func(t *testing.T, a *App, run store.DelegationRun) {
				if _, err := a.st.AppendDelegationAmendment(run.ID, string(delegate.AmendEdge),
					delegate.EncodeAmendmentBody(delegate.Amendment{
						Kind: delegate.AmendEdge, Task: "c", From: "a", Artifact: "art-a"}), 210); err != nil {
					t.Fatal(err)
				}
			},
			from: "a", to: "c", wantKind: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := archApp(t)
			run := seedPlainRun(t, a, visRoot, parkManifest(visRepoLbl))
			tc.seed(t, a, run)

			snap := a.OrchestrationSnapshot(visRoot, 0, 0)
			got, ok := edgeByPair(snap.Edges, tc.from, tc.to)
			if tc.wantKind == "" {
				if ok {
					t.Fatalf("an unapproved amendment put an edge on the graph: %+v — an "+
						"amendment is INERT until a human grants it", got)
				}
				return
			}
			if !ok {
				t.Fatalf("no %s→%s edge in %+v", tc.from, tc.to, snap.Edges)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("edge %s→%s kind = %q, want %q", tc.from, tc.to, got.Kind, tc.wantKind)
			}
		})
	}
}

// §5.1: a park edge is "drawn in the back-edge band ABOVE the ranks". It must
// therefore not push a rank — a graph whose columns shifted the moment a child
// parked, and shifted back when the park cleared, is the re-layout §7.3 forbids
// arriving through the data rather than through the render.
func TestArchSnapshot_parkEdgeDoesNotPushRank(t *testing.T) {
	a := archApp(t)
	run := seedPlainRun(t, a, visRoot, parkManifest(visRepoLbl))
	before := a.OrchestrationSnapshot(visRoot, 0, 0)
	rankOf := func(s OrchestrationDTO, id string) int {
		t.Helper()
		for _, n := range s.Nodes {
			if n.ID == id {
				return n.Rank
			}
		}
		t.Fatalf("no node %q", id)
		return -1
	}
	if rankOf(before, "c") != 0 {
		t.Fatalf("fixture: c should start at rank 0, got %d", rankOf(before, "c"))
	}

	setBlock(t, a, run.ID, "c", delegate.Block{
		Version: delegate.BlockVersion, Task: "c", Kind: delegate.BlockNeedsArtifact,
		Need: delegate.BlockNeed{Artifact: "art-a", From: "a"}, Summary: "parked",
	})
	after := a.OrchestrationSnapshot(visRoot, 0, 0)
	if _, ok := edgeByPair(after.Edges, "a", "c"); !ok {
		t.Fatalf("fixture: the park edge was not drawn: %+v", after.Edges)
	}
	if got := rankOf(after, "c"); got != 0 {
		t.Errorf("the park edge pushed c to rank %d — park edges are banded, not ranked", got)
	}
	// It IS a topology change, so the rev must move: a banded edge changes the
	// ranking input, and a client holding the old rev would keep stale columns.
	if after.Rev == before.Rev {
		t.Error("a park edge appearing did not move the topology rev")
	}
}

// A task with a live block is NOT ready, whatever its edges say. The view's
// ready set is the scheduler's (EffectiveGraph.Ready) — a second implementation
// over the declared graph would light up a node the runner will not promote and
// the spawn gate will refuse.
func TestArchSnapshot_readySetIsTheEffectiveScheduler(t *testing.T) {
	a := archApp(t)
	run := seedPlainRun(t, a, visRoot, parkManifest(visRepoLbl))
	readyOf := func(s OrchestrationDTO, id string) bool {
		t.Helper()
		for _, st := range s.Statuses {
			if st.ID == id {
				return st.Ready
			}
		}
		t.Fatalf("no status for %q", id)
		return false
	}
	if !readyOf(a.OrchestrationSnapshot(visRoot, 0, 0), "c") {
		t.Fatal("fixture: c has no dependencies and should start ready")
	}

	setBlock(t, a, run.ID, "c", delegate.Block{
		Version: delegate.BlockVersion, Task: "c", Kind: delegate.BlockNeedsDecision,
		Summary: "a human must choose",
	})
	if readyOf(a.OrchestrationSnapshot(visRoot, 0, 0), "c") {
		t.Error("a task with a live block was offered as ready — the declared graph cannot " +
			"see the block, and the spawn gate would refuse the press")
	}
}

// §10.2's baseline fault and §12.1's deadlock share the status `deadlocked` and
// read completely differently. The discriminator rides on the run head, so the
// seam never has to make a second call on the one tick a run goes red.
func TestArchSnapshot_runHeadCarriesRedKind(t *testing.T) {
	cases := []struct {
		name        string
		integration string
		wantKind    string
		wantFaults  int
	}{
		{name: "no baseline recorded reads as a §12.1 deadlock",
			integration: "{}", wantKind: RedDeadlock},
		{name: "a red baseline reads as a §10.2 fault, with the repo named",
			integration: `{"` + visRepoLbl + `":{"status":"fail","head":"abc123","at":9,"out":"the repo's own tests fail"}}`,
			wantKind:    RedBaselineFault, wantFaults: 1},
		{name: "a passing baseline is not a fault",
			integration: `{"` + visRepoLbl + `":{"status":"pass","head":"abc123","at":9}}`,
			wantKind:    RedDeadlock},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := archApp(t)
			run := seedPlainRun(t, a, visRoot, parkManifest(visRepoLbl))
			if ok, err := a.st.SetDelegationRunIntegrationCAS(run.ID, "", tc.integration, 300); err != nil || !ok {
				t.Fatalf("set integration: ok=%v err=%v", ok, err)
			}
			if ok, err := a.st.AdvanceDelegationRunCAS(run.ID, "planning", "deadlocked", 310); err != nil || !ok {
				t.Fatalf("advance run: ok=%v err=%v", ok, err)
			}
			snap := a.OrchestrationSnapshot(visRoot, 0, 0)
			if snap.Run == nil {
				t.Fatal("no run head")
			}
			if snap.Run.RedKind != tc.wantKind {
				t.Errorf("redKind = %q, want %q", snap.Run.RedKind, tc.wantKind)
			}
			if len(snap.Run.BaselineFaults) != tc.wantFaults {
				t.Errorf("baselineFaults = %+v, want %d", snap.Run.BaselineFaults, tc.wantFaults)
			}
			if snap.Run.BaselineFaults == nil {
				t.Error("baselineFaults is null; the frontend indexes it without a guard")
			}
		})
	}
}

// §12.1 on the POLL. RunDeadlock reports the wait-for cycle from persisted
// state and WRITES NOTHING — the reason it exists beside TickDelegationRun,
// which runs checks, integrations and seed deliveries on its way to the same
// verdict.
func TestRunDeadlock_reportsTheCycleWithoutWriting(t *testing.T) {
	a := archApp(t)
	a.loomDir = t.TempDir()
	man := parkManifest(visRepoLbl)
	run := seedPlainRun(t, a, visRoot, man)
	// A mutual wait made ENTIRELY of live blocks: a waits on c's unforeseen
	// artifact, c waits on a's. Zero declared edges are involved, which is
	// §12.1(a)'s point — over the declared graph alone this run looks fine.
	setBlock(t, a, run.ID, "a", delegate.Block{
		Version: delegate.BlockVersion, Task: "a", Kind: delegate.BlockNeedsArtifact,
		Need: delegate.BlockNeed{Artifact: "art-c", From: "c"}, Summary: "a waits on c",
	})
	setBlock(t, a, run.ID, "c", delegate.Block{
		Version: delegate.BlockVersion, Task: "c", Kind: delegate.BlockNeedsArtifact,
		Need: delegate.BlockNeed{Artifact: "art-a", From: "a"}, Summary: "c waits on a",
	})
	if ok, err := a.st.AdvanceTaskCAS(run.ID, "b", string(delegate.StatePending),
		string(delegate.StateAbandoned), 320); err != nil || !ok {
		t.Fatalf("park b out of the way: ok=%v err=%v", ok, err)
	}

	before, _, err := a.st.GetDelegationRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeTasks, err := a.st.ListDelegationTasks(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	got := a.RunDeadlock(run.ID)
	if got.Error != "" {
		t.Fatalf("RunDeadlock: %s", got.Error)
	}
	if got.Deadlock == nil {
		t.Fatal("no deadlock reported for a run whose two remaining tasks wait on each other")
	}
	if got.Deadlock.Shape != string(delegate.ShapeMutualWait) {
		t.Errorf("shape = %q, want %q — a cycle is fatal to the plan however many "+
			"decisions are also owed", got.Deadlock.Shape, delegate.ShapeMutualWait)
	}
	if len(got.Deadlock.Cycle) == 0 {
		t.Error("the wait-for cycle is empty; a boolean would be useless here — the remedy " +
			"is a re-plan and a re-plan needs the loop")
	}

	after, _, err := a.st.GetDelegationRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("RunDeadlock moved the run row:\n before %+v\n after  %+v — the poll must "+
			"not advance the run in order to render it", before, after)
	}
	afterTasks, err := a.st.ListDelegationTasks(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mustJSON(t, beforeTasks) != mustJSON(t, afterTasks) {
		t.Errorf("RunDeadlock moved a task row:\n before %s\n after  %s",
			mustJSON(t, beforeTasks), mustJSON(t, afterTasks))
	}
}

// §3.1's gate is one rule across every door, and RunDeadlock is a new door. A
// deadlock names tasks and artifacts, which are agent-authored and routinely
// name the client; there is no label-free version of this payload.
func TestRunDeadlock_hiddenProjectGetsTheBareMarker(t *testing.T) {
	a := archApp(t)
	a.loomDir = t.TempDir()
	run := seedArchRun(t, a, hidRoot, secretManifest(secretRepoLbl))
	setBlock(t, a, run.ID, secretTaskID, delegate.Block{
		Version: delegate.BlockVersion, Task: secretTaskID, Kind: delegate.BlockNeedsArtifact,
		Need: delegate.BlockNeed{Artifact: secretArtifact, From: "acme-schema"}, Summary: secretTitle,
	})
	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	got := a.RunDeadlock(run.ID)
	if !got.Hidden {
		t.Fatalf("a hidden project's deadlock was served: %+v", got)
	}
	raw := mustJSON(t, got)
	for _, secret := range []string{secretTaskID, secretTitle, secretArtifact, secretRepoLbl} {
		if strings.Contains(raw, secret) {
			t.Errorf("the marker %q crossed on a hidden project's deadlock payload: %s", secret, raw)
		}
	}
}

// §12.3.1 is written as "`diverged` flag WITH THE FILE LIST". The list is a
// decode of a column the check already wrote, so it costs the poll nothing —
// and a payload that carried the flag alone would say a task diverged and make
// the human open the merge gate to find out from what, putting the evidence
// behind the gate it exists to inform.
//
// A hidden node carries the EMPTY report: file paths are the most identifying
// thing on the struct.
func TestArchSnapshot_divergenceFileListsTravelAndStopAtTheGate(t *testing.T) {
	const outsideFile = "internal/acme/ledger/secret_client_thing.go"
	seed := func(t *testing.T, a *App, man delegate.Manifest) OrchestrationDTO {
		t.Helper()
		run := seedArchRun(t, a, visRoot, man)
		if err := a.st.SetTaskDivergence(run.ID, secretTaskID,
			delegate.EncodeDivergence(gitdiff.Divergence{
				Outside:  []string{outsideFile},
				Siblings: map[string][]string{"acme-schema": {outsideFile}},
			}), 400); err != nil {
			t.Fatal(err)
		}
		return a.OrchestrationSnapshot(visRoot, 0, 0)
	}

	a := archApp(t)
	visible := seed(t, a, secretManifest(visRepoLbl))
	var found bool
	for _, st := range visible.Statuses {
		if st.ID != secretTaskID {
			continue
		}
		found = true
		if st.Divergence.Empty {
			t.Fatalf("the recorded divergence did not reach the poll: %+v", st.Divergence)
		}
		if len(st.Divergence.Outside) != 1 || st.Divergence.Outside[0] != outsideFile {
			t.Errorf("outside = %v, want the one recorded file", st.Divergence.Outside)
		}
		if len(st.Divergence.Siblings["acme-schema"]) != 1 {
			t.Errorf("siblings = %v — §12.3.2's prediction is the stronger half and must "+
				"not be dropped", st.Divergence.Siblings)
		}
	}
	if !found {
		t.Fatal("no status for the diverged task")
	}

	// Now the same run in the VISIBLE project, with the diverged task's repo
	// owned by the hidden one: §3.1.4's per-node placeholder, which is the case
	// a door-level gate does not cover.
	b := archApp(t)
	man := secretManifest(visRepoLbl)
	man.Tasks[1].Repo = secretRepoLbl
	if err := b.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	hiddenSnap := seed(t, b, man)
	if hiddenSnap.Strip.HiddenNodes != 1 {
		t.Fatalf("fixture: expected one hidden node, got %d", hiddenSnap.Strip.HiddenNodes)
	}
	if raw := mustJSON(t, hiddenSnap); strings.Contains(raw, outsideFile) {
		t.Errorf("a file path crossed on a hidden project's payload: %s", raw)
	}
	for _, st := range hiddenSnap.Statuses {
		if st.Divergence.Outside == nil || st.Divergence.Siblings == nil {
			t.Errorf("hidden status %q carries a null divergence; the frontend indexes it "+
				"without a guard", st.ID)
		}
	}
}

// §5.5's "blocked on you" row and §5.2's strip figures, after §10 shipped.
//
// The merge row is `mergeable`, not `verified`: §10.2 is triggered when a task
// becomes verified and Loom enters integration without being asked, so
// `verified` is a stage Loom advances and `mergeable` is the one waiting on a
// human. A row saying "awaiting your merge" over a task Loom is actively
// integrating comes with a button that refuses.
func TestArchSnapshot_mergeGateRowIsMergeableNotVerified(t *testing.T) {
	cases := []struct {
		state       delegate.TaskState
		wantBlocked bool
		wantFigure  func(StripDTO) int
		figureName  string
	}{
		{state: delegate.StateVerified, wantBlocked: false,
			wantFigure: func(s StripDTO) int { return s.Verified }, figureName: "verified"},
		{state: delegate.StateIntegrating, wantBlocked: false,
			wantFigure: func(s StripDTO) int { return s.Running }, figureName: "running"},
		{state: delegate.StateMergeable, wantBlocked: true,
			wantFigure: func(s StripDTO) int { return s.Mergeable }, figureName: "mergeable"},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			a := archApp(t)
			run := seedPlainRun(t, a, visRoot, parkManifest(visRepoLbl))
			if ok, err := a.st.AdvanceTaskCAS(run.ID, "c", string(delegate.StatePending),
				string(tc.state), 500); err != nil || !ok {
				t.Fatalf("advance c to %s: ok=%v err=%v", tc.state, ok, err)
			}
			snap := a.OrchestrationSnapshot(visRoot, 0, 0)

			var blocked bool
			for _, b := range snap.Blocked {
				if b.ID == "c" {
					blocked = true
					if b.Action != "merge" {
						t.Errorf("action = %q, want merge", b.Action)
					}
				}
			}
			if blocked != tc.wantBlocked {
				t.Errorf("blocked-on-you row for a %s task = %v, want %v (rows: %+v)",
					tc.state, blocked, tc.wantBlocked, snap.Blocked)
			}
			if got := tc.wantFigure(snap.Strip); got != 1 {
				t.Errorf("a %s task counted %d in the %q figure, want 1 — a task counted "+
					"in no figure is present in the node count and absent from every number "+
					"that explains it: %+v", tc.state, got, tc.figureName, snap.Strip)
			}
		})
	}
}
