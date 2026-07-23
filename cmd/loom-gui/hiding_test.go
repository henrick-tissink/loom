package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/henricktissink/loom/internal/delegate"
	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/workflow"
)

// §6.3 binds one test per GUI leak surface: rail · Finished · search ·
// workflow runs · workflow definitions · ListProjects · fan-out checklist ·
// needs-you count · window title · dock badge · notifications. Each test below
// names its surface; the ones that share a code path (title and dock badge
// both read needsYouCount) say so explicitly rather than being silently folded
// together, because §9's "one per surface" exists to catch the surface someone
// forgets.

func liveIn(name, cwd string, addDirs ...string) status.Row {
	return status.Row{
		SessionRow: store.SessionRow{
			Name: name, ProjectLabel: "api", Cwd: cwd,
			AddDirs: session.EncodeAddDirs(addDirs),
		},
		Status: status.NeedsYou, Title: "work",
	}
}

// Surface 1: the rail.
func TestLeak_rail(t *testing.T) {
	snap := status.Snapshot{Live: []status.Row{
		liveIn("vis", visRepo),
		liveIn("hid", hidRepo),
		// §6.1: visibility is evaluated over cwd ∪ add_dirs, so a session
		// sitting in a visible repo while it edits a hidden one is hidden.
		liveIn("cross", visRepo, hidRepo),
	}}

	all := snapshotToDTOs(snap, testAttributor())
	if len(all) != 3 {
		t.Fatalf("nothing hidden: want 3 rows, got %d", len(all))
	}
	if all[0].ProjectRoot != visRoot || all[0].ProjectName != "Visible" {
		t.Errorf("server-computed attribution missing: %+v", all[0])
	}
	if all[0].Project != "api" {
		t.Errorf("ProjectLabel must stay a display-only passthrough: %+v", all[0])
	}

	got := snapshotToDTOs(snap, testAttributor(hidRoot))
	if len(got) != 1 || got[0].Name != "vis" {
		t.Fatalf("rail leaked: %+v", got)
	}
}

// Surface 2: the Finished list.
func TestLeak_finished(t *testing.T) {
	rows := []store.SessionRow{
		{Name: "vis", Cwd: visRepo, EndedAt: 10},
		{Name: "hid", Cwd: hidRepo, EndedAt: 20},
	}
	if got := recentToDTOs(rows, nil, testAttributor()); len(got) != 2 {
		t.Fatalf("nothing hidden: want 2, got %d", len(got))
	}
	got := recentToDTOs(rows, nil, testAttributor(hidRoot))
	if len(got) != 1 || got[0].Name != "vis" {
		t.Fatalf("Finished leaked: %+v", got)
	}
	if got[0].ProjectRoot != visRoot || got[0].ProjectName != "Visible" {
		t.Errorf("server-computed attribution missing: %+v", got[0])
	}
}

// Surface 3: search results.
func TestLeak_search(t *testing.T) {
	hits := []store.SearchHit{
		{SessionID: "a", Cwd: visRepo},
		{SessionID: "b", Cwd: hidRepo},
		// Fail-closed: while anything is hidden, a row we cannot place is
		// treated as hidden (§4). The live DB has exactly one such transcript.
		{SessionID: "c", Cwd: ""},
	}
	if got := searchHitsToDTOs(hits, testResolver()); len(got) != 3 {
		t.Fatalf("nothing hidden: want 3, got %d", len(got))
	}
	got := searchHitsToDTOs(hits, testResolver(hidRoot))
	if len(got) != 1 || got[0].SessionID != "a" {
		t.Fatalf("search leaked: %+v", got)
	}
}

// Surface 4: the workflow DEFINITIONS list. Attribution is the union of every
// step's resolved path, not step 1: a chain that starts visible and moves into
// a hidden repo would otherwise sit in the list naming the hidden work.
func TestLeak_workflowDefs(t *testing.T) {
	defs := []workflow.Definition{
		{Name: "visible-only", Path: "/w/a.json", Steps: []workflow.Step{
			{Label: "s1", Project: visRepo}, {Label: "s2", Project: visRoot}}},
		{Name: "crosses-into-hidden", Path: "/w/b.json", Steps: []workflow.Step{
			{Label: "s1", Project: visRepo}, {Label: "s2", Project: hidRepo}}},
	}
	if got := defsToDTOs(defs, testResolver()); len(got) != 2 {
		t.Fatalf("nothing hidden: want 2, got %d", len(got))
	}
	got := defsToDTOs(defs, testResolver(hidRoot))
	if len(got) != 1 || got[0].Name != "visible-only" {
		t.Fatalf("definitions list leaked: %+v", got)
	}
}

// Surface 5: the workflow RUNS list, over the same union — including the cwds
// of the sessions the run has actually launched, which is the arm that catches
// a run whose def_json says "visible" but whose live session is not.
func TestLeak_workflowRuns(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)
	a.runner = &workflow.Runner{Store: a.st}

	visDef := mustDefJSON(t, visRepo, visRoot)
	crossDef := mustDefJSON(t, visRepo, hidRepo)

	visID := insertRun(t, a, "vis-run", visDef, nil)
	crossID := insertRun(t, a, "cross-run", crossDef, nil)

	// A run whose definition is entirely visible but whose step-1 session was
	// launched into the hidden project.
	if err := a.st.Upsert(store.SessionRow{
		Name: "hidden-sess", Cwd: hidRepo, EndedAt: -1, ExitCode: -1,
	}); err != nil {
		t.Fatal(err)
	}
	sessID := insertRun(t, a, "sess-run", visDef, []string{"hidden-sess"})

	if got := a.ListRuns(); len(got) != 3 {
		t.Fatalf("nothing hidden: want 3 runs, got %d", len(got))
	}
	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	got := a.ListRuns()
	if len(got) != 1 || got[0].ID != visID {
		t.Fatalf("runs list leaked (cross=%d sess=%d): %+v", crossID, sessID, got)
	}
}

func mustDefJSON(t *testing.T, stepProjects ...string) string {
	t.Helper()
	var d workflow.Definition
	d.Name = "d"
	for i, p := range stepProjects {
		d.Steps = append(d.Steps, workflow.Step{Label: fmt.Sprintf("s%d", i+1), Project: p})
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func insertRun(t *testing.T, a *App, name, defJSON string, names []string) int64 {
	t.Helper()
	id, err := a.st.InsertRun(name, defJSON, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) > 0 {
		if _, err := a.st.AdvanceRunCAS(id, 0, 0, names, "", 2); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// Surface 6: ListProjects, the launcher's target picker.
func TestLeak_listProjects(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)

	if got := a.ListProjects(); len(got) != 4 {
		t.Fatalf("nothing hidden: want 4 targets (2 roots + 2 repos), got %+v", got)
	}
	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range a.ListProjects() {
		if p.ProjectRoot == hidRoot {
			t.Fatalf("picker leaked a hidden project's target: %+v", p)
		}
	}
}

// Surface 7: the fan-out checklist. The checklist is rendered from
// ListProjects, and Fanout re-validates against the same set — so a stale
// frontend list cannot launch into a project the user just hid.
func TestLeak_fanoutChecklist(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)
	a.launcher = &session.Launcher{}
	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}

	got := a.Fanout([]string{hidRepo}, "", "", "hi")
	if got.Launched != 0 || got.Failed != 1 || got.Error == "" {
		t.Fatalf("fan-out into a hidden project must fail before launching: %+v", got)
	}
}

// Surfaces 8–10: the needs-you count, the window title and the dock badge all
// read the same number, computed over the already-filtered DTOs.
func TestLeak_needsYouCountTitleAndBadge(t *testing.T) {
	snap := status.Snapshot{Live: []status.Row{
		liveIn("vis", visRepo),
		liveIn("hid", hidRepo),
	}}
	if n := needsYouCount(snapshotToDTOs(snap, testAttributor())); n != 2 {
		t.Fatalf("nothing hidden: count = %d, want 2", n)
	}
	n := needsYouCount(snapshotToDTOs(snap, testAttributor(hidRoot)))
	if n != 1 {
		t.Fatalf("count = %d, want 1 — the title reads \"loom — %d need you\" and the dock badge the same number", n, n)
	}
}

// Surface 11: notifications. A flipped session in a hidden project still
// escalates (§6.4 — silently swallowing it would mean the user never learns an
// agent is blocked) but loses its label, which is the part that names the client.
func TestLeak_notifications(t *testing.T) {
	snap := status.Snapshot{
		NewlyNeedsYou: []string{"vis", "hid"},
		Live:          []status.Row{liveIn("vis", visRepo), liveIn("hid", hidRepo)},
	}
	labels, suppressed := needsYouLabels(snap, testResolver(hidRoot))
	if len(labels) != 1 || labels[0] != "api · work" || suppressed != 1 {
		t.Fatalf("labels = %q, suppressed = %d", labels, suppressed)
	}

	tests := []struct {
		name       string
		labels     []string
		suppressed int
		want       string
	}{
		{"one visible", []string{"api · work"}, 0, "api · work needs you"},
		{"one hidden only", nil, 1, "1 session needs you"},
		{"one of each", []string{"api · work"}, 1, "2 sessions need you"},
		{"nothing", nil, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body string
			n := &notifier{run: func(_, b string) { body = b }}
			n.needsYou(tt.labels, tt.suppressed)
			if body != tt.want {
				t.Fatalf("body = %q, want %q", body, tt.want)
			}
		})
	}
}

// §9: Finished and search lengths are unchanged when a project is hidden. Both
// apply their LIMIT in SQL, so a filter applied after the cap silently
// truncates the page — the over-fetch is what keeps a full list on screen.
func TestOverFetch_finishedAndSearchLengthsUnchanged(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)

	// Interleave so that a naive "LIMIT then filter" would lose roughly half.
	for i := 0; i < recentLimit+10; i++ {
		for _, cwd := range []string{visRepo, hidRepo} {
			name := fmt.Sprintf("s-%s-%d", cwd, i)
			if err := a.st.Upsert(store.SessionRow{
				Name: name, ClaudeSessionID: name, Cwd: cwd,
				EndedAt: int64(i), ExitCode: 0, LastStatus: "done",
			}); err != nil {
				t.Fatal(err)
			}
			if err := a.st.UpsertTranscript(store.Transcript{
				SessionID: name, ProjectDir: "d", Cwd: cwd, Title: "t", LastTS: int64(i),
			}); err != nil {
				t.Fatal(err)
			}
			if err := a.st.ReplaceFileDocs(
				store.IndexedFile{Path: "/" + name, SessionID: name, Size: 1, Mtime: 1},
				[]store.Doc{{Content: "widget widget", Role: "user", TS: 1}}); err != nil {
				t.Fatal(err)
			}
		}
	}

	before, beforeHits := len(a.ListRecent()), len(a.SearchSessions("widget"))
	if before != recentLimit {
		t.Fatalf("Finished = %d, want a full page of %d", before, recentLimit)
	}
	if beforeHits != searchLimit {
		t.Fatalf("search = %d, want a full page of %d", beforeHits, searchLimit)
	}

	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	if after := len(a.ListRecent()); after != before {
		t.Fatalf("Finished = %d after hiding, want %d unchanged", after, before)
	}
	if after := len(a.SearchSessions("widget")); after != beforeHits {
		t.Fatalf("search = %d after hiding, want %d unchanged", after, beforeHits)
	}
}

// §6.2b: hiding suppresses new Loom-initiated background work. The skip must
// NOT mark sumTried — that map is per-process, so poisoning it would mean
// unhiding never re-enables the summary.
func TestAutoSummarize_suppressedWhileHiddenAndReEnabledOnUnhide(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)
	a.settings = &settingsStore{cur: Settings{AutoSummarize: true}}
	// A summarizer that cannot possibly succeed: the assertion is about which
	// row gets picked, never about the summary itself.
	a.summarizer = &memory.Summarizer{Store: a.st, Binary: "/nonexistent", WorkDir: t.TempDir()}

	rows := []store.SessionRow{{Name: "hid", ClaudeSessionID: "cs-hid", Cwd: hidRepo, EndedAt: 10}}

	a.maybeAutoSummarize(rows, testResolver(hidRoot))
	a.sumMu.Lock()
	tried, busy := a.sumTried["cs-hid"], a.sumBusy
	a.sumMu.Unlock()
	if tried {
		t.Fatal("hidden row must be skipped WITHOUT poisoning sumTried")
	}
	if busy {
		t.Fatal("no summarize should have started for a hidden project")
	}

	// Unhide: the same row is now eligible, with nothing left over from the
	// suppressed pass.
	a.maybeAutoSummarize(rows, testResolver())
	a.sumMu.Lock()
	tried = a.sumTried["cs-hid"]
	a.sumMu.Unlock()
	if !tried {
		t.Fatal("unhiding must re-enable auto-summarize for the row")
	}
}

// Orchestrator spec §7: an orchestrator session is never auto-summarized. Its
// transcript is a byproduct, never a channel — the notes are the deliberate
// handoff — so the summary costs quota to produce something §5.4's
// echo-chamber guard then refuses to show anyone.
//
// The skip must NOT mark sumTried, for the same per-process reason as the
// hidden case: tags are mutable, so an id poisoned while tagged could never
// recover its summary without restarting Loom. Both halves are asserted, and
// the untagged control row proves the skip is keyed on the tag rather than on
// something incidental to the fixture.
func TestAutoSummarize_skipsOrchestratorWithoutPoisoningSumTried(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)
	a.settings = &settingsStore{cur: Settings{AutoSummarize: true}}
	a.summarizer = &memory.Summarizer{Store: a.st, Binary: "/nonexistent", WorkDir: t.TempDir()}

	orch := []store.SessionRow{{
		Name: "o", ClaudeSessionID: "cs-orch", Cwd: visRepo, Tags: "orch", EndedAt: 10,
	}}

	a.maybeAutoSummarize(orch, testResolver())
	a.sumMu.Lock()
	tried, busy := a.sumTried["cs-orch"], a.sumBusy
	a.sumMu.Unlock()
	if tried {
		t.Fatal("orchestrator row must be skipped WITHOUT poisoning sumTried")
	}
	if busy {
		t.Fatal("no summarize should have started for an orchestrator session")
	}

	// The tag is the whole reason: the identical row without it is eligible.
	untagged := []store.SessionRow{{
		Name: "o", ClaudeSessionID: "cs-orch", Cwd: visRepo, Tags: "", EndedAt: 10,
	}}
	a.maybeAutoSummarize(untagged, testResolver())
	a.sumMu.Lock()
	tried = a.sumTried["cs-orch"]
	a.sumMu.Unlock()
	if !tried {
		t.Fatal("an untagged row must still be eligible — the skip is keyed on the orch tag")
	}
}

// The token test must not be a substring test. A user's own "orchid" tag is a
// real tag on a real session, and swallowing it would drop their work out of
// the Finished summaries with nothing anywhere saying why.
func TestAutoSummarize_orchTagIsATokenNotASubstring(t *testing.T) {
	a := newTestApp(t)
	seedProjects(t, a)
	a.settings = &settingsStore{cur: Settings{AutoSummarize: true}}
	a.summarizer = &memory.Summarizer{Store: a.st, Binary: "/nonexistent", WorkDir: t.TempDir()}

	rows := []store.SessionRow{{
		Name: "x", ClaudeSessionID: "cs-orchid", Cwd: visRepo, Tags: "orchid", EndedAt: 10,
	}}
	a.maybeAutoSummarize(rows, testResolver())
	a.sumMu.Lock()
	tried := a.sumTried["cs-orchid"]
	a.sumMu.Unlock()
	if !tried {
		t.Fatal(`a session tagged "orchid" was treated as an orchestrator (substring match)`)
	}
}

// --- delegation §14.1: child attribution through the REAL DTO path -------

// These are the three cases §17 says revision 1 would have passed while
// broken, asserted at the DTO layer rather than inside internal/delegate.
// That placement is the point: the override only fixes anything if the DTO
// layer actually CALLS it, and a wrapper that exists but is not wired is
// indistinguishable from no wrapper at all from the user's seat.
//
// The fixture uses a real store so the run row, the delegation column and the
// resolver are joined the way production joins them.
func delegationFixture(t *testing.T, hidden ...string) (*App, store.SessionRow) {
	t.Helper()
	a := newTestApp(t)
	seedProjects(t, a)

	run, err := a.st.InsertDelegationRun("rearch", visRoot, "{}", "{}", 1000)
	if err != nil {
		t.Fatal(err)
	}
	// The child's cwd is a worktree under ~/.loom — outside every project
	// target, which is the whole reason the prefix scan cannot place it.
	child := store.SessionRow{
		Name:            "loom-child",
		ProjectLabel:    "api",
		Cwd:             "/home/u/.loom/worktrees/rearch-1/api/account-schema",
		ClaudeSessionID: "cs-child",
		Delegation:      delegate.FormatDelegation(run.ID, "account-schema"),
		EndedAt:         -1,
		ExitCode:        -1,
	}
	if err := a.st.Upsert(child); err != nil {
		t.Fatal(err)
	}
	return a, child
}

func TestDelegationChildVisibilityThroughDTO(t *testing.T) {
	tests := []struct {
		name    string
		hidden  []string
		soloed  string
		wantVis bool
		why     string
	}{
		{
			name: "solo the run's own project", soloed: visRoot, wantVis: true,
			why: "the one situation where you most want to watch a run must not blank it",
		},
		{
			name: "solo a different project", soloed: hidRoot, wantVis: false,
			why: "solo means only that project's work is on screen",
		},
		{
			name: "nothing hidden", wantVis: true, why: "no filtering at all",
		},
		{
			name: "a different project hidden", hidden: []string{hidRoot}, wantVis: true,
			why: "filtering is on but not for this project — the fail-closed trap",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, child := delegationFixture(t)
			for _, root := range tc.hidden {
				if err := a.SetProjectHidden(root, true); err != nil {
					t.Fatal(err)
				}
			}
			if tc.soloed != "" {
				if err := a.SetProjectSolo(tc.soloed, true); err != nil {
					t.Fatal(err)
				}
			}

			snap := status.Snapshot{Live: []status.Row{{
				SessionRow: child, Status: status.NeedsYou, Title: "work",
			}}}
			got := snapshotToDTOs(snap, a.attributor(a.resolver()))

			if tc.wantVis && len(got) != 1 {
				t.Fatalf("child hidden from the rail: %s", tc.why)
			}
			if !tc.wantVis && len(got) != 0 {
				t.Fatalf("child leaked into the rail: %s", tc.why)
			}
		})
	}
}

// Nothing hidden ⇒ the child groups under its run's PROJECT, not Ungrouped.
// The rail sections on projectRoot, so an Ungrouped answer scatters a run's
// children out of their own project even with no filtering on at all.
func TestDelegationChildGroupsUnderItsProject(t *testing.T) {
	a, child := delegationFixture(t)

	snap := status.Snapshot{Live: []status.Row{{
		SessionRow: child, Status: status.NeedsYou, Title: "work",
	}}}
	got := snapshotToDTOs(snap, a.attributor(a.resolver()))
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].ProjectRoot != visRoot {
		t.Fatalf("projectRoot = %q, want %q (Ungrouped is %q)",
			got[0].ProjectRoot, visRoot, store.UngroupedRoot)
	}
	if got[0].ProjectName != "Visible" {
		t.Fatalf("projectName = %q, want the run's project", got[0].ProjectName)
	}
}

// The Finished list is a second DTO path with its own visibility call, so it
// gets its own assertion — §6.3's "one per surface" rule. A fix applied only
// to the rail is exactly the kind of half-wiring this catches.
func TestDelegationChildVisibleInFinished(t *testing.T) {
	a, child := delegationFixture(t)
	if err := a.SetProjectSolo(visRoot, true); err != nil {
		t.Fatal(err)
	}
	child.EndedAt, child.ExitCode = 2000, 0

	got := recentToDTOs([]store.SessionRow{child}, nil, a.attributor(a.resolver()))
	if len(got) != 1 {
		t.Fatal("child vanished from Finished while its own project was soloed")
	}
	if got[0].ProjectRoot != visRoot {
		t.Fatalf("projectRoot = %q, want %q", got[0].ProjectRoot, visRoot)
	}
}

// A child whose delegation names a DELETED run falls through to the prefix
// scan and thus to fail-closed.
func TestDelegationChildWithDeletedRunFailsClosed(t *testing.T) {
	a, child := delegationFixture(t)
	if err := a.SetProjectHidden(hidRoot, true); err != nil { // ⇒ Filtering()
		t.Fatal(err)
	}
	child.Delegation = delegate.FormatDelegation(99999, "account-schema")

	snap := status.Snapshot{Live: []status.Row{{
		SessionRow: child, Status: status.NeedsYou, Title: "work",
	}}}
	if got := snapshotToDTOs(snap, a.attributor(a.resolver())); len(got) != 0 {
		t.Fatalf("a child naming a deleted run must fail closed, got %+v", got)
	}
}
