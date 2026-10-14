package main

import (
	"fmt"
	"testing"

	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

// §6.3 binds one test per GUI leak surface: rail · Finished · search ·
// ListProjects · needs-you count · window title · dock badge · notifications. Each test below
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

// --- delegation §14.1: child attribution through the REAL DTO path -------
