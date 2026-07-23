package orchestrator

import (
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/store"
)

func tr(id, dir, title, outcome string, ts int64) store.Transcript {
	return store.Transcript{SessionID: id, ProjectDir: dir, Cwd: dir,
		Title: title, Outcome: outcome, LastTS: ts}
}

// TestHasOrchTag pins the token test. Tags is a free-form field, and a
// substring test would swallow a user's own "orchid" tag — silently dropping
// their real sessions out of every brief.
func TestHasOrchTag(t *testing.T) {
	tests := []struct {
		tags string
		want bool
	}{
		{"orch", true},
		{"orch,fan:x", true},
		{"fan:x orch", true},
		{"fan:x, orch", true},
		{"", false},
		{"orchid", false},
		{"reorch", false},
		{"fan:orchestra", false},
	}
	for _, tc := range tests {
		if got := hasOrchTag(tc.tags); got != tc.want {
			t.Errorf("hasOrchTag(%q) = %v, want %v", tc.tags, got, tc.want)
		}
	}
}

// TestFilterRecentDrops covers §5.2's drops and §5.4's guard at the unit level:
// the tag exclusion, dedupe, and the fail-closed visibility filter.
func TestFilterRecentDrops(t *testing.T) {
	const root, repo = "/w/proj", "/w/proj/repo"
	visible := projects.NewResolver(
		[]store.Project{
			{Root: "", Name: "Ungrouped"},
			{Root: root, Name: "proj"},
			{Root: "/w/secret", Name: "secret", Hidden: true},
		},
		[]store.ProjectRepo{{Path: repo, ProjectRoot: root, Label: "repo"}})

	tests := []struct {
		name    string
		trs     []store.Transcript
		orchIDs map[string]bool
		wantIDs []string
	}{
		{
			name:    "orch-tagged session is excluded",
			trs:     []store.Transcript{tr("a", root, "orch", "I mapped it", 5), tr("b", repo, "work", "shipped", 4)},
			orchIDs: map[string]bool{"a": true},
			wantIDs: []string{"b"},
		},
		{
			name:    "a non-orch session in the same dir survives",
			trs:     []store.Transcript{tr("a", root, "orch", "I mapped it", 5), tr("c", root, "work", "shipped", 4)},
			orchIDs: map[string]bool{"a": true},
			wantIDs: []string{"c"},
		},
		{
			name:    "same session indexed under two dirs is deduped",
			trs:     []store.Transcript{tr("d", root, "t", "o", 9), tr("d", repo, "t", "o", 9)},
			wantIDs: []string{"d"},
		},
		{
			name:    "hidden project rows never appear",
			trs:     []store.Transcript{tr("e", "/w/secret", "t", "o", 9), tr("f", repo, "t", "o", 8)},
			wantIDs: []string{"f"},
		},
		{
			name:    "unattributable row is dropped, fail-closed",
			trs:     []store.Transcript{tr("g", "/nowhere/at/all", "t", "o", 9), tr("h", repo, "t", "o", 8)},
			wantIDs: []string{"h"},
		},
		{
			name:    "descending recency, session id breaks ties",
			trs:     []store.Transcript{tr("z", repo, "t", "o", 3), tr("a", repo, "t", "o", 3), tr("m", repo, "t", "o", 7)},
			wantIDs: []string{"m", "a", "z"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := filterRecent(tc.trs, recentWorkInput{
				dirs: []string{root, repo}, resolver: visible, orchIDs: tc.orchIDs,
			})
			var got []string
			for _, r := range rows {
				got = append(got, r.SessionID)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantIDs, ",") {
				t.Fatalf("got %v, want %v", got, tc.wantIDs)
			}
		})
	}
}

// TestFilterRecentSoloSuppresses pins the other half of slice 1 §6: under solo
// nothing outside the soloed project may be quoted, including rows Loom would
// otherwise show.
func TestFilterRecentSoloSuppresses(t *testing.T) {
	const root = "/w/proj"
	r := projects.NewResolver(
		[]store.Project{
			{Root: "", Name: "Ungrouped"},
			{Root: root, Name: "proj"},
			{Root: "/w/other", Name: "other", Solo: true},
		}, nil)
	rows := filterRecent([]store.Transcript{
		tr("a", root, "t", "o", 5), tr("b", "/w/other", "t", "o", 4),
	}, recentWorkInput{dirs: []string{root}, resolver: r})
	if len(rows) != 1 || rows[0].SessionID != "b" {
		t.Fatalf("solo did not suppress the non-solo row: %+v", rows)
	}
}

// TestRecentRowCap pins the 40-row half of §5.2's double cap.
func TestRecentRowCap(t *testing.T) {
	var trs []store.Transcript
	for i := 0; i < maxRecentRows*3; i++ {
		trs = append(trs, tr(string(rune('a'+i%26))+string(rune('a'+i/26)), "/w/p", "t", "o", int64(i)))
	}
	rows := filterRecent(trs, recentWorkInput{dirs: []string{"/w/p"}})
	if len(rows) != maxRecentRows {
		t.Fatalf("got %d rows, want %d", len(rows), maxRecentRows)
	}
}

// TestRecentOutcomeFallback pins §5.2 step 5: outcome, else ask ONLY when
// AskUsable, else the explicit "(no outcome recorded)". A `<command-…>` wrapper
// quoted as "what that session was about" is confident noise.
func TestRecentOutcomeFallback(t *testing.T) {
	tests := []struct {
		name    string
		t       store.Transcript
		want    string
		wantSub bool
	}{
		{"outcome wins", store.Transcript{Outcome: "shipped it", Ask: "asked"}, "shipped it", false},
		{"usable ask fills in", store.Transcript{Ask: "add a retry"}, "add a retry", false},
		{"command wrapper is not usable", store.Transcript{Ask: "<command-name>/foo</command-name>"},
			"(no outcome recorded)", false},
		{"caveat preamble is not usable", store.Transcript{Ask: "Caveat: the messages below"},
			"(no outcome recorded)", false},
		{"neither", store.Transcript{}, "(no outcome recorded)", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := recentOutcome(tc.t); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRecentRowRenderIsUTC pins §5.3 for the one field that would otherwise
// depend on where the machine is standing.
func TestRecentRowRender(t *testing.T) {
	r := RecentRow{LastTS: 1752000000, Label: "bankenstein", Title: "fix auth", Outcome: "done"}
	if got, want := r.Render(), "2025-07-08 · bankenstein · fix auth — done"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	r.Title = ""
	if !strings.Contains(r.Render(), "(untitled)") {
		t.Fatalf("empty title not rendered: %q", r.Render())
	}
}

// TestEchoChamberGuard is §13's named must-fail-if-removed test, run end to end
// against a real store: an `orch`-tagged session, indexed, with a NON-EMPTY
// outcome and a cwd equal to the project root, must not appear in that
// project's next brief — while a non-orch session in the same directory does.
//
// This is the test that pins the real path. Revision 1 of the spec aimed the
// guard at the brief's own text, which was already dropped by
// memory.extractUserText's tool_result rule; the path that actually compounds
// is the orchestrator's own Outcome, which no user-side filter touches.
func TestEchoChamberGuard(t *testing.T) {
	for _, intent := range []string{"", "payment seam mapping across repositories"} {
		name := "recency path"
		if intent != "" {
			name = "ranked path"
		}
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			root := t.TempDir()
			mustProject(t, st, root, "Innostream")
			mustRepo(t, st, root, root, "innostream")

			mustIndexedSession(t, st, "orch-gen-1", root,
				"orchestrator", "ORCH_OUTCOME payment seam mapping across repositories done",
				"orch", 2000)
			mustIndexedSession(t, st, "human-1", root,
				"human work", "CONTROL_OUTCOME payment seam mapping across repositories shipped",
				"", 1000)

			s, _ := svc(t, st, nil, fakeGit{
				branch: map[string]string{root: "main"},
				head:   map[string]string{root: "abc123def456"},
			}.run)
			br, err := s.Reassemble(root, intent)
			if err != nil {
				t.Fatalf("Reassemble: %v", err)
			}
			if strings.Contains(br.Text, "ORCH_OUTCOME") {
				t.Fatal("the orchestrator's own outcome reached the next brief — " +
					"the echo-chamber guard is not holding")
			}
			if !strings.Contains(br.Text, "CONTROL_OUTCOME") {
				t.Fatal("the non-orch control was dropped — the exclusion is a blanket drop")
			}
		})
	}
}

// TestLoomBriefMentionIsNotFiltered pins §5.4's decision NOT to add a
// LOOM-BRIEF sentinel to memory's excludedPrefixes: a session whose text merely
// mentions the label is an ordinary session and must still appear. Widening
// excludedPrefixes would widen the exported AskUsable that internal/workflow
// depends on, which is a worse trade than a boilerplate row.
func TestLoomBriefMentionIsNotFiltered(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	mustProject(t, st, root, "Innostream")
	mustRepo(t, st, root, root, "innostream")
	mustIndexedSession(t, st, "mentions", root, "talked about the brief",
		"I read the LOOM-BRIEF label and moved on", "", 1000)

	s, _ := svc(t, st, nil, fakeGit{
		branch: map[string]string{root: "main"}, head: map[string]string{root: "abc"},
	}.run)
	br, err := s.Reassemble(root, "")
	if err != nil {
		t.Fatalf("Reassemble: %v", err)
	}
	if !strings.Contains(br.Text, "I read the LOOM-BRIEF label") {
		t.Fatal("a session merely mentioning LOOM-BRIEF was filtered — there is no sentinel filter by decision")
	}
}

// TestOrchSessionIDsCoversEveryGeneration pins why the exclusion set is built
// from the sessions table rather than from the orchestrators table: that table
// holds one row per project, so generation N−2 would be invisible to it and its
// transcript would leak straight back into the brief.
func TestOrchSessionIDsCoversEveryGeneration(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	mustIndexedSession(t, st, "gen1", root, "t", "o", "orch", 1000)
	mustIndexedSession(t, st, "gen2", root, "t", "o", "orch", 2000)
	mustIndexedSession(t, st, "human", root, "t", "o", "", 3000)
	// A live one too. The guard must cover live and terminal rows alike: this
	// used to be two different queries stitched together, and the live row is
	// what proved the stitch. store.TaggedSessions is one query with no limit,
	// but the coverage requirement is unchanged and so is the assertion.
	if err := st.Upsert(store.SessionRow{
		Name: "loom-gen3", ClaudeSessionID: "gen3", Cwd: root, Tags: "orch",
		CreatedAt: 4000, EndedAt: -1, ExitCode: -1, LastStatus: "running",
	}); err != nil {
		t.Fatal(err)
	}

	ids, err := orchSessionIDs(st)
	if err != nil {
		t.Fatalf("orchSessionIDs: %v", err)
	}
	for _, want := range []string{"gen1", "gen2", "gen3"} {
		if !ids[want] {
			t.Fatalf("%s missing from the exclusion set: %v", want, ids)
		}
	}
	if ids["human"] {
		t.Fatal("a non-orch session landed in the exclusion set")
	}
}

// TestRankedVsRecencyPath pins §5.2 step 2: a typed intent runs recall's
// existing ranking (so an unrelated session is not a candidate at all), and no
// intent falls back to recency over the dir set (so both appear). The ranking
// selects the candidate set; step 4's last_ts ordering still decides the order.
func TestRankedVsRecencyPath(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	mustProject(t, st, root, "Innostream")
	mustRepo(t, st, root, root, "innostream")

	mustIndexedSession(t, st, "match", root, "payment seam refactor",
		"MATCHING_OUTCOME split the payment seam", "", 1000)
	mustIndexDocs(t, st, "match", "we refactored the payment seam across repositories", 1000)
	mustIndexedSession(t, st, "unrelated", root, "css tweaks",
		"UNRELATED_OUTCOME nudged a margin", "", 2000)
	mustIndexDocs(t, st, "unrelated", "moved a margin two pixels left", 2000)

	s, _ := svc(t, st, nil, fakeGit{
		branch: map[string]string{root: "main"}, head: map[string]string{root: "abc"},
	}.run)

	ranked, err := s.Reassemble(root, "payment seam refactor across repositories")
	if err != nil {
		t.Fatalf("Reassemble(intent): %v", err)
	}
	if !strings.Contains(ranked.Text, "MATCHING_OUTCOME") {
		t.Fatal("ranked path dropped the matching session")
	}
	if strings.Contains(ranked.Text, "UNRELATED_OUTCOME") {
		t.Fatal("ranked path included a session the recall gate should have excluded")
	}

	recency, err := s.Reassemble(root, "")
	if err != nil {
		t.Fatalf("Reassemble(no intent): %v", err)
	}
	for _, want := range []string{"MATCHING_OUTCOME", "UNRELATED_OUTCOME"} {
		if !strings.Contains(recency.Text, want) {
			t.Fatalf("recency path dropped %s", want)
		}
	}
}

// TestRankedPathExcludesOtherProjects guards the collapse onto
// memory.RelatedIn. RelatedIn ranks over the WHOLE index and returns
// cross-project hits with SameProject=false — the dir set is a boost, not a
// WHERE clause. Dropping the membership filter when the ranked branch became
// one call would leak another project's work into this project's brief, which
// is both a containment break (§5.2 scopes the section to the project) and a
// hiding break. The recency branch cannot regress this way because its query
// is already set-scoped, so only the ranked branch is asserted.
func TestRankedPathExcludesOtherProjects(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	other := t.TempDir()
	mustProject(t, st, root, "Innostream")
	mustRepo(t, st, root, root, "innostream")
	mustProject(t, st, other, "Elsewhere")
	mustRepo(t, st, other, other, "elsewhere")

	mustIndexedSession(t, st, "mine", root, "payment seam refactor",
		"MINE_OUTCOME split the payment seam", "", 1000)
	mustIndexDocs(t, st, "mine", "we refactored the payment seam across repositories", 1000)
	// Denser in the query terms, so raw bm25 ranks it ABOVE the in-project hit
	// and a missing membership filter shows up rather than hiding behind order.
	mustIndexedSession(t, st, "theirs", other, "their payment seam work",
		"THEIRS_OUTCOME touched a payment seam", "", 2000)
	mustIndexDocs(t, st, "theirs",
		"payment seam refactor payment seam refactor payment seam across repositories", 2000)

	s, _ := svc(t, st, nil, fakeGit{
		branch: map[string]string{root: "main"}, head: map[string]string{root: "abc"},
	}.run)

	got, err := s.Reassemble(root, "payment seam refactor across repositories")
	if err != nil {
		t.Fatalf("Reassemble(intent): %v", err)
	}
	if !strings.Contains(got.Text, "MINE_OUTCOME") {
		t.Fatal("ranked path dropped this project's own matching session")
	}
	if strings.Contains(got.Text, "THEIRS_OUTCOME") {
		t.Fatal("ranked path leaked another project's session into the brief")
	}
}
