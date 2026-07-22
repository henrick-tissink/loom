package ui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/memory"
	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/registry"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
)

// --- fixtures -----------------------------------------------------------

// fakeProjects is the visibility authority driven from literal rows: the TUI
// consumes only Resolver(), so a test can pin every §6.1 shape (hidden, solo,
// solo-but-missing, nothing hidden, read failure) without a DB or a service.
type fakeProjects struct {
	ps    []store.Project
	repos []store.ProjectRepo
	err   error
}

func (f *fakeProjects) Resolver() (*projects.Resolver, error) {
	if f.err != nil {
		return nil, f.err
	}
	return projects.NewResolver(f.ps, f.repos), nil
}

const (
	hiddenRoot  = "/w/HappyPay"
	visibleRoot = "/w/Innostream"
	hiddenCwd   = hiddenRoot + "/HappyPayCoreApi"
	visibleCwd  = visibleRoot + "/ballista"
)

// hiddenHappyPay is the standing fixture: one hidden project, one visible
// one, plus the reserved Ungrouped row that migration v7 seeds (its empty
// root prefixes every path, so its presence is part of what the filter has to
// survive).
func hiddenHappyPay() *fakeProjects {
	return &fakeProjects{
		ps: []store.Project{
			{Root: store.UngroupedRoot, Name: "Ungrouped", Origin: "reserved"},
			{Root: hiddenRoot, Name: "HappyPay", Origin: "discovered", Hidden: true},
			{Root: visibleRoot, Name: "Innostream", Origin: "discovered"},
		},
		repos: []store.ProjectRepo{
			{Path: hiddenCwd, ProjectRoot: hiddenRoot, Label: "HappyPay/HappyPayCoreApi"},
			{Path: visibleCwd, ProjectRoot: visibleRoot, Label: "Innostream/ballista"},
		},
	}
}

func liveRow(name, cwd string, st status.Status) status.Row {
	return status.Row{
		SessionRow: store.SessionRow{Name: name, ProjectLabel: filepath.Base(cwd), Cwd: cwd},
		Status:     st,
	}
}

func names(rows []uiRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.name
	}
	return out
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// --- §6.3 surface 1: rail -----------------------------------------------

// TestHidingFiltersRail pins the rail half of §6.3. The unattributable row is
// in the table deliberately: while anything is hidden, a session Loom cannot
// place is treated as hidden (§4, fail-closed) — the alternative is that the
// one live transcript with cwd=” becomes the leak.
func TestHidingFiltersRail(t *testing.T) {
	a := NewApp(Deps{Projects: hiddenHappyPay()})
	a.width, a.height = 100, 30
	a.Update(snapMsg(status.Snapshot{Live: []status.Row{
		liveRow("loom-hidden", hiddenCwd, status.NeedsYou),
		liveRow("loom-visible", visibleCwd, status.Running),
		liveRow("loom-nowhere", "/tmp/scratch", status.Running),
		liveRow("loom-nocwd", "", status.Running),
	}}))

	if got, want := names(a.rows), []string{"loom-visible"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("rail rows = %v, want %v", got, want)
	}
	if out := a.View(); strings.Contains(out, "HappyPayCoreApi") {
		t.Fatalf("hidden project's label rendered in the rail:\n%s", out)
	}
}

// --- §6.3 surface 2: Finished / RECENT ----------------------------------

// TestHidingFiltersFinishedOverFetches is the over-fetch case §6.3 calls out
// and §9 pins: store.Recent applies its LIMIT in SQL, so filtering the
// engine's already-capped 10 rows would return one entry here instead of ten.
// The list length must be unchanged by hiding whenever enough visible rows
// exist at all.
func TestHidingFiltersFinishedOverFetches(t *testing.T) {
	st := testStore(t)
	// The 30 most recently ended sessions are all hidden; the visible ones
	// sit below the engine's own Recent(10) window entirely.
	for i := range 30 {
		insertEnded(t, st, fmt.Sprintf("loom-h%02d", i), hiddenCwd, int64(1000+i))
	}
	for i := range 12 {
		insertEnded(t, st, fmt.Sprintf("loom-v%02d", i), visibleCwd, int64(500+i))
	}

	a := NewApp(Deps{Store: st, Projects: hiddenHappyPay()})
	a.width, a.height = 100, 30
	engineRecent, err := st.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	a.Update(snapMsg(status.Snapshot{Recent: engineRecent}))

	if len(a.snap.Recent) != recentDisplayLimit {
		t.Fatalf("Finished list = %d rows, want %d (over-fetch trimmed after filtering)",
			len(a.snap.Recent), recentDisplayLimit)
	}
	for _, r := range a.snap.Recent {
		if r.Cwd != visibleCwd {
			t.Fatalf("hidden row %q in the Finished list", r.Name)
		}
	}
}

func insertEnded(t *testing.T, st *store.Store, name, cwd string, endedAt int64) {
	t.Helper()
	err := st.Upsert(store.SessionRow{
		Name: name, ClaudeSessionID: name, ProjectLabel: filepath.Base(cwd), Cwd: cwd,
		CreatedAt: endedAt - 10, EndedAt: endedAt, ExitCode: 0, LastStatus: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- §6.3 surface 3: wall -----------------------------------------------

// TestHidingFiltersWall: the wall renders a.snap.Live via applyWallOrder, so
// it inherits the snapMsg filter — this test exists to keep it that way, since
// the wall is the one TUI surface that puts pane CONTENT on screen.
func TestHidingFiltersWall(t *testing.T) {
	a := NewApp(Deps{Projects: hiddenHappyPay()})
	a.width, a.height = 100, 30
	a.Update(snapMsg(status.Snapshot{Live: []status.Row{
		liveRow("loom-hidden", hiddenCwd, status.Running),
		liveRow("loom-visible", visibleCwd, status.Running),
	}}))
	if len(a.wallOrder) != 1 || a.wallOrder[0].Name != "loom-visible" {
		t.Fatalf("wallOrder = %+v, want only loom-visible", a.wallOrder)
	}
	if a.wallSelected == "loom-hidden" {
		t.Fatal("wall selection landed on a hidden session")
	}
}

// --- §6.3 surface 4: recall / RELATED panel -----------------------------

// TestHidingFiltersRelatedPanel drives the panel's real query path. Recall
// crosses projects by design — that is its value — so a hidden project's
// history is exactly what it would otherwise surface next to a visible seed.
func TestHidingFiltersRelatedPanel(t *testing.T) {
	st := testStore(t)
	seedTranscript(t, st, "sess-hidden", hiddenCwd, "widget calibration overhaul")
	seedTranscript(t, st, "sess-visible", visibleCwd, "widget calibration rewrite")

	a := NewApp(Deps{Store: st, Projects: hiddenHappyPay()})
	cmd := a.panelQueryCmd("widget calibration", transcript.ProjectDirName(visibleCwd))
	if cmd == nil {
		t.Fatal("panelQueryCmd returned nil with a store and a project dir")
	}
	msg, ok := cmd().(panelResultsMsg)
	if !ok {
		t.Fatalf("panelQueryCmd produced %#v", msg)
	}
	if len(msg.hits) == 0 {
		t.Fatal("no hits at all — the query itself failed, so this proves nothing")
	}
	for _, h := range msg.hits {
		if h.T.SessionID == "sess-hidden" {
			t.Fatalf("hidden project's transcript in the RELATED panel: %+v", h.T)
		}
	}
}

func seedTranscript(t *testing.T, st *store.Store, id, cwd, text string) {
	t.Helper()
	if err := st.UpsertTranscript(store.Transcript{
		SessionID: id, ProjectDir: transcript.ProjectDirName(cwd), Cwd: cwd,
		Title: text, Ask: text, FirstTS: 1, LastTS: 1, MsgCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceFileDocs(
		store.IndexedFile{Path: "/f/" + id, SessionID: id, Size: 1, Mtime: 1},
		[]store.Doc{{Content: text, Role: "user", TS: 1}}); err != nil {
		t.Fatal(err)
	}
}

// --- §6.3 surface 5: search ---------------------------------------------

// TestHidingFiltersSearchOverFetches pins both halves for search: hidden
// transcripts never reach the result list, and — because SearchSessions caps
// in SQL — the visible matches still arrive even when 35 hidden ones would
// have filled the entire display window first.
func TestHidingFiltersSearchOverFetches(t *testing.T) {
	st := testStore(t)
	for i := range 35 {
		seedTranscript(t, st, fmt.Sprintf("h%02d", i), hiddenCwd, "widget calibration overhaul")
	}
	for i := range 5 {
		seedTranscript(t, st, fmt.Sprintf("v%02d", i), visibleCwd, "widget calibration overhaul")
	}

	a := NewApp(Deps{Store: st, Projects: hiddenHappyPay()})
	msg, ok := a.searchQueryCmd("widget")().(searchResultsMsg)
	if !ok {
		t.Fatalf("searchQueryCmd produced %#v", msg)
	}
	if len(msg.hits) != 5 {
		t.Fatalf("search returned %d hits, want the 5 visible ones (over-fetch)", len(msg.hits))
	}
	for _, h := range msg.hits {
		if h.Cwd != visibleCwd {
			t.Fatalf("hidden project's transcript in search results: %+v", h)
		}
	}
}

// --- §6.3 surface 6: notifications --------------------------------------

// TestHidingFiltersNotifications is the surface this whole TUI slice exists
// for: notifications live in BOTH binaries and ARCHITECTURE.md declares two
// instances against one DB supported, so an unfiltered banner from a terminal
// nobody is looking at names the hidden client anyway.
//
// A hidden transition still RAISES — §6.4 degrades the body to a label-free
// form rather than swallowing it, matching cmd/loom-gui/notify.go. What must
// never survive is the identity: the name is dropped from NewlyNeedsYou, so
// nothing downstream can render it.
func TestHidingFiltersNotifications(t *testing.T) {
	cases := []struct {
		name       string
		newly      []string
		live       []status.Row
		wantNotify bool
		wantNewly  []string
	}{
		{
			name:       "hidden transition escalates label-free",
			newly:      []string{"loom-hidden"},
			live:       []status.Row{liveRow("loom-hidden", hiddenCwd, status.NeedsYou)},
			wantNotify: true,
		},
		{
			// A name with no live row cannot come from a real poll; it is not
			// escalated, or a hand-built snapshot would raise a phantom banner.
			name:  "unmatched name raises nothing",
			newly: []string{"loom-ghost"},
			live:  []status.Row{liveRow("loom-visible", visibleCwd, status.NeedsYou)},
		},
		{
			name:       "visible transition still notifies",
			newly:      []string{"loom-visible"},
			live:       []status.Row{liveRow("loom-visible", visibleCwd, status.NeedsYou)},
			wantNotify: true,
			wantNewly:  []string{"loom-visible"},
		},
		{
			name:  "mixed: only the visible name survives",
			newly: []string{"loom-hidden", "loom-visible"},
			live: []status.Row{
				liveRow("loom-hidden", hiddenCwd, status.NeedsYou),
				liveRow("loom-visible", visibleCwd, status.NeedsYou),
			},
			wantNotify: true,
			wantNewly:  []string{"loom-visible"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := NewApp(Deps{Projects: hiddenHappyPay()})
			a.width, a.height = 100, 30
			_, cmd := a.Update(snapMsg(status.Snapshot{Live: c.live, NewlyNeedsYou: c.newly}))
			if (cmd != nil) != c.wantNotify {
				t.Fatalf("notify cmd = %v, want %v", cmd != nil, c.wantNotify)
			}
			if len(a.snap.NewlyNeedsYou) != len(c.wantNewly) {
				t.Fatalf("NewlyNeedsYou = %v, want %v", a.snap.NewlyNeedsYou, c.wantNewly)
			}
			for i, n := range c.wantNewly {
				if a.snap.NewlyNeedsYou[i] != n {
					t.Fatalf("NewlyNeedsYou = %v, want %v", a.snap.NewlyNeedsYou, c.wantNewly)
				}
			}
		})
	}
}

// --- project scoping: launcher and fan-out ------------------------------

// TestLaunchSurfacesAreProjectScoped: offering a hidden client's repo in a
// picker is the same leak as showing its session, and it is the fastest way
// to launch straight back into what was just put out of view.
func TestLaunchSurfacesAreProjectScoped(t *testing.T) {
	repos := []registry.Repo{
		{Label: "HappyPay/HappyPayCoreApi", Path: hiddenCwd},
		{Label: "Innostream/ballista", Path: visibleCwd},
		{Label: "scratch", Path: "/tmp/scratch"}, // unattributable → fail-closed
	}
	a := NewApp(Deps{Repos: repos, Projects: hiddenHappyPay()})
	a.width, a.height = 100, 30

	a.openLauncher()
	if len(a.form.repos) != 1 || a.form.repos[0].Path != visibleCwd {
		t.Fatalf("launcher repos = %+v, want only %s", a.form.repos, visibleCwd)
	}
	a.openFanout()
	if len(a.fanForm.repos) != 1 || a.fanForm.repos[0].Path != visibleCwd {
		t.Fatalf("fan-out repos = %+v, want only %s", a.fanForm.repos, visibleCwd)
	}

	// With nothing hidden the surfaces are untouched — hiding is opt-in, and
	// the fail-closed rule must stay dormant for a user who hid nothing.
	open := NewApp(Deps{Repos: repos, Projects: &fakeProjects{}})
	open.openLauncher()
	if len(open.form.repos) != len(repos) {
		t.Fatalf("launcher repos = %d, want all %d when nothing is hidden",
			len(open.form.repos), len(repos))
	}
}

// --- the §6.1 predicate as the TUI applies it ---------------------------

// TestSnapshotVisibilityRules walks §6.1's predicate over a session row: the
// solo/hidden split, the whole-directory-set rule (cwd ∪ add_dirs), the
// fail-closed direction, and solo-root-missing degrading to nothing hidden.
func TestSnapshotVisibilityRules(t *testing.T) {
	soloed := func(root string, missing bool) *fakeProjects {
		f := hiddenHappyPay()
		for i := range f.ps {
			if f.ps[i].Root == root {
				f.ps[i].Solo, f.ps[i].Missing = true, missing
			}
		}
		return f
	}
	cases := []struct {
		name    string
		auth    *fakeProjects
		row     store.SessionRow
		visible bool
	}{
		{"nothing hidden shows everything", &fakeProjects{},
			store.SessionRow{Name: "s", Cwd: "/tmp/anywhere"}, true},
		{"hidden project's repo", hiddenHappyPay(),
			store.SessionRow{Name: "s", Cwd: hiddenCwd}, false},
		{"visible project's repo", hiddenHappyPay(),
			store.SessionRow{Name: "s", Cwd: visibleCwd}, true},
		// Segment-wise, never a raw string prefix (§4): `/w/InnostreamX` is a
		// string prefix of the visible root, and matching it would show a
		// session that belongs to no project at all.
		{"sibling prefix is not a match", hiddenHappyPay(),
			store.SessionRow{Name: "s", Cwd: visibleRoot + "X/repo"}, false},
		{"add-dir into a hidden project takes the whole row", hiddenHappyPay(),
			store.SessionRow{Name: "s", Cwd: visibleCwd,
				AddDirs: `["` + hiddenCwd + `"]`}, false},
		{"add-dirs all visible", hiddenHappyPay(),
			store.SessionRow{Name: "s", Cwd: visibleCwd,
				AddDirs: `["` + visibleRoot + `"]`}, true},
		{"unattributable cwd fails closed", hiddenHappyPay(),
			store.SessionRow{Name: "s", Cwd: "/tmp/scratch"}, false},
		{"empty cwd fails closed", hiddenHappyPay(),
			store.SessionRow{Name: "s"}, false},
		{"solo shows only the soloed project", soloed(visibleRoot, false),
			store.SessionRow{Name: "s", Cwd: visibleCwd}, true},
		{"solo hides an otherwise-visible project", soloed(hiddenRoot, false),
			store.SessionRow{Name: "s", Cwd: visibleCwd}, false},
		{"solo suppresses Ungrouped too", soloed(visibleRoot, false),
			store.SessionRow{Name: "s", Cwd: "/tmp/scratch"}, false},
		// A solo root that vanished degrades to "solo hides nothing" — the
		// same row is hidden when that solo IS in force (case above), so this
		// pins the degrade direction: an empty rail mid-demo reads as Loom
		// being broken. `hidden` is untouched by solo either way, so the
		// still-hidden project stays hidden (next case).
		{"solo root missing degrades to nothing hidden", soloed(hiddenRoot, true),
			store.SessionRow{Name: "s", Cwd: visibleCwd}, true},
		{"solo root missing leaves hidden intact", soloed(visibleRoot, true),
			store.SessionRow{Name: "s", Cwd: hiddenCwd}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := NewApp(Deps{Projects: c.auth})
			a.width, a.height = 100, 30
			a.Update(snapMsg(status.Snapshot{Live: []status.Row{{
				SessionRow: c.row, Status: status.Running,
			}}}))
			if got := len(a.rows) == 1; got != c.visible {
				t.Fatalf("visible = %v, want %v (rows %v)", got, c.visible, names(a.rows))
			}
		})
	}
}

// TestSoloHiddenRoundTrip: `hidden` is never written by solo, so leaving solo
// restores the prior state exactly — the round trip §9 asks for.
func TestSoloHiddenRoundTrip(t *testing.T) {
	auth := hiddenHappyPay()
	a := NewApp(Deps{Projects: auth})
	a.width, a.height = 100, 30
	snap := status.Snapshot{Live: []status.Row{
		liveRow("loom-hidden", hiddenCwd, status.Running),
		liveRow("loom-visible", visibleCwd, status.Running),
	}}

	a.Update(snapMsg(snap))
	before := names(a.rows)

	// solo HappyPay — the hidden project becomes the only visible one.
	auth.ps[1].Solo = true
	a.Update(snapMsg(snap))
	if got := names(a.rows); len(got) != 1 || got[0] != "loom-hidden" {
		t.Fatalf("under solo rows = %v, want [loom-hidden]", got)
	}

	auth.ps[1].Solo = false
	a.Update(snapMsg(snap))
	if got := names(a.rows); len(got) != len(before) || got[0] != before[0] {
		t.Fatalf("after solo rows = %v, want the pre-solo %v", got, before)
	}
}

// TestAuthorityErrorKeepsLastGoodResolver: a transient store error must not
// un-hide a project mid-screen-share. Degrading to "nothing hidden" is the
// one failure direction this feature cannot take.
func TestAuthorityErrorKeepsLastGoodResolver(t *testing.T) {
	auth := hiddenHappyPay()
	a := NewApp(Deps{Projects: auth})
	a.width, a.height = 100, 30
	snap := status.Snapshot{Live: []status.Row{liveRow("loom-hidden", hiddenCwd, status.Running)}}

	a.Update(snapMsg(snap))
	if len(a.rows) != 0 {
		t.Fatalf("hidden row rendered before the error: %v", names(a.rows))
	}
	auth.err = errors.New("database is locked")
	a.Update(snapMsg(snap))
	if len(a.rows) != 0 {
		t.Fatalf("store error un-hid the project: %v", names(a.rows))
	}
}

// TestNilAuthorityFiltersNothing: hiding is opt-in. Deps without a Projects
// authority — every caller predating this slice — must behave exactly as
// before, including for rows no project could ever claim.
func TestNilAuthorityFiltersNothing(t *testing.T) {
	a := NewApp(Deps{})
	a.width, a.height = 100, 30
	a.Update(snapMsg(status.Snapshot{Live: []status.Row{
		liveRow("loom-a", hiddenCwd, status.Running),
		liveRow("loom-b", "", status.Running),
	}}))
	if len(a.rows) != 2 {
		t.Fatalf("rows = %v, want both with no authority configured", names(a.rows))
	}
}

// --- §6.2a: hiding is presentation, never behaviour ---------------------

// TestHiddenSessionsStillPollAndTransition is the layering invariant. The
// engine never learns about projects: it polls a hidden project's session and
// persists its status transition exactly as before, and only the rendering
// drops it. If this ever fails, hiding has started costing the user work
// rather than screen real estate.
func TestHiddenSessionsStillPollAndTransition(t *testing.T) {
	st := testStore(t)
	old := time.Now().Add(-time.Hour).Unix()
	if err := st.Upsert(store.SessionRow{
		Name: "loom-hidden", ClaudeSessionID: "hidden-id", ProjectLabel: "HappyPayCoreApi",
		Cwd: hiddenCwd, CreatedAt: old, EndedAt: -1, ExitCode: -1, LastStatus: "running",
	}); err != nil {
		t.Fatal(err)
	}

	// A tmux socket with no server: ListSessions reports zero sessions, so
	// Poll retires the orphaned live row — a real, observable transition.
	tm := &tmux.Client{Socket: fmt.Sprintf("loomvis%d", os.Getpid())}
	eng := status.NewEngine(tm, st, t.TempDir())
	a := NewApp(Deps{Engine: eng, Store: st, Projects: hiddenHappyPay()})
	a.width, a.height = 100, 30

	msg := a.pollCmd()()
	raw, ok := msg.(snapMsg)
	if !ok {
		t.Fatalf("poll produced %#v", msg)
	}
	// (a) the engine saw it: unfiltered, it is in the snapshot it returned.
	found := false
	for _, r := range raw.Recent {
		if r.Name == "loom-hidden" {
			found = true
		}
	}
	if !found {
		t.Fatal("engine's own snapshot dropped the hidden session — the engine learned about projects")
	}
	// (b) the transition was persisted, hidden or not.
	row, ok, err := st.Get("loom-hidden")
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if row.LastStatus != "done" {
		t.Fatalf("hidden session's status = %q, want the transition to have happened", row.LastStatus)
	}
	// (c) and only then is it filtered out of what is drawn.
	a.Update(msg)
	if len(a.rows) != 0 {
		t.Fatalf("hidden session rendered: %v", names(a.rows))
	}
}

// --- trim helpers -------------------------------------------------------

// TestVisibleHitsTrimsWithoutAnAuthority: the display cap must still apply
// when nothing is hidden, or the over-fetched width would leak into the
// result list as extra rows the surface never asked for.
func TestVisibleHitsTrimsWithoutAnAuthority(t *testing.T) {
	hits := make([]store.SearchHit, 40)
	for i := range hits {
		hits[i] = store.SearchHit{SessionID: fmt.Sprint(i), Cwd: visibleCwd}
	}
	if got := visibleHits(nil, hits, searchDisplayLimit); len(got) != searchDisplayLimit {
		t.Fatalf("visibleHits(nil) = %d rows, want %d", len(got), searchDisplayLimit)
	}
	rel := make([]memory.RelatedHit, 12)
	for i := range rel {
		rel[i] = memory.RelatedHit{T: store.Transcript{SessionID: fmt.Sprint(i), Cwd: visibleCwd}}
	}
	if got := visibleRelated(nil, rel, panelDisplayLimit); len(got) != panelDisplayLimit {
		t.Fatalf("visibleRelated(nil) = %d rows, want %d", len(got), panelDisplayLimit)
	}
}

// §6.4: the banner escalates a hidden project's attention without ever naming
// it. Anything a viewer could trace back to the client — a label, a title —
// must exist only for visible sessions.
func TestNotifyBodyNeverNamesASuppressedSession(t *testing.T) {
	cases := []struct {
		name       string
		items      []string
		suppressed int
		want       string
	}{
		{name: "visible only", items: []string{"loom · fix race"}, want: "loom · fix race"},
		{name: "two visible", items: []string{"a", "b"}, want: "a, b"},
		{name: "one hidden", suppressed: 1, want: "1 session needs you"},
		{name: "several hidden", suppressed: 3, want: "3 sessions need you"},
		{name: "mixed", items: []string{"loom · fix race"}, suppressed: 2,
			want: "loom · fix race (+2 more)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := notifyBody(c.items, c.suppressed); got != c.want {
				t.Fatalf("notifyBody(%v, %d) = %q, want %q", c.items, c.suppressed, got, c.want)
			}
		})
	}
}
