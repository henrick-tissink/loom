package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func row(name string) SessionRow {
	return SessionRow{
		Name: name, ClaudeSessionID: name[5:], ProjectLabel: "parallax",
		Cwd: "/w/parallax", Model: "opus", Mode: "plan",
		CreatedAt: 1000, EndedAt: -1, ExitCode: -1, LastStatus: "unknown",
	}
}

func TestUpsertGetRoundtrip(t *testing.T) {
	s := open(t)
	r := row("loom-aaa")
	if err := s.Upsert(r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get("loom-aaa")
	if err != nil || !ok {
		t.Fatalf("Get: %v %v", ok, err)
	}
	if got != r {
		t.Fatalf("got %+v want %+v", got, r)
	}
	// upsert same name updates, no duplicate
	r.Model = "sonnet"
	if err := s.Upsert(r); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Get("loom-aaa")
	if got.Model != "sonnet" {
		t.Fatalf("update lost: %+v", got)
	}
}

func TestLiveRecentAndMarkEnded(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-aaa"))
	s.Upsert(row("loom-bbb"))
	s.SetStatus("loom-aaa", "running")

	live, err := s.Live()
	if err != nil || len(live) != 2 {
		t.Fatalf("Live = %d rows, %v", len(live), err)
	}
	if err := s.MarkEnded("loom-bbb", "error", 3, 2000); err != nil {
		t.Fatal(err)
	}
	live, _ = s.Live()
	if len(live) != 1 || live[0].Name != "loom-aaa" {
		t.Fatalf("Live after end = %+v", live)
	}
	rec, err := s.Recent(10)
	if err != nil || len(rec) != 1 || rec[0].LastStatus != "error" || rec[0].ExitCode != 3 {
		t.Fatalf("Recent = %+v, %v", rec, err)
	}
}

func TestMarkLiveOrphansEnded(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-aaa")) // still in tmux
	s.Upsert(row("loom-bbb")) // vanished from tmux
	s.MarkEnded("loom-ccc-precreate", "done", 0, 500)
	s.Upsert(SessionRow{Name: "loom-ccc", ClaudeSessionID: "ccc", ProjectLabel: "x",
		Cwd: "/x", CreatedAt: 1, EndedAt: 400, ExitCode: 0, LastStatus: "done"}) // history

	// Mutate orphan exit_code away from -1 to test that UPDATE enforces the contract
	orphan := row("loom-bbb")
	orphan.ExitCode = 7
	s.Upsert(orphan)

	// graceUnix=9999 is permissive here (after every row's created_at=1000),
	// preserving this test's original "retire everything not tmux-alive" intent.
	if err := s.MarkLiveOrphansEnded([]string{"loom-aaa"}, 9999, 3000); err != nil {
		t.Fatal(err)
	}
	live, _ := s.Live()
	if len(live) != 1 || live[0].Name != "loom-aaa" {
		t.Fatalf("Live = %+v (want only loom-aaa)", live)
	}
	// history row untouched (never pruned/re-ended)
	ccc, _, _ := s.Get("loom-ccc")
	if ccc.EndedAt != 400 {
		t.Fatalf("history row mutated: %+v", ccc)
	}
	bbb, _, _ := s.Get("loom-bbb")
	if bbb.LastStatus != "done" || bbb.ExitCode != -1 || bbb.EndedAt != 3000 {
		t.Fatalf("orphan not retired: %+v", bbb)
	}
}

// TestMarkLiveOrphansEndedRespectsGraceWindow guards finding 2a: a session
// just launched can be observed by a poll that races the tmux session's own
// creation (store row written, tmux session not yet visible to ListSessions,
// or vice versa). Such a row must NOT be retired as an orphan while it's
// still within the grace window, even though it isn't in liveTmuxNames.
func TestMarkLiveOrphansEndedRespectsGraceWindow(t *testing.T) {
	s := open(t)
	young := row("loom-young")
	young.CreatedAt = 995 // "now" (1000) minus a 5s grace window
	s.Upsert(young)

	// graceUnix=990 < created_at=995: too young to retire yet, protected
	if err := s.MarkLiveOrphansEnded(nil, 990, 2000); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get("loom-young")
	if got.LastStatus != "unknown" || got.EndedAt != -1 {
		t.Fatalf("young row retired despite grace window: %+v", got)
	}

	// once it ages past the grace cutoff (graceUnix=1000 > created_at=995), it
	// IS eligible for retirement
	if err := s.MarkLiveOrphansEnded(nil, 1000, 3000); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Get("loom-young")
	if got.LastStatus != "done" || got.EndedAt != 3000 {
		t.Fatalf("aged row not retired: %+v", got)
	}
}

func TestMigrationV3TitleOnExistingDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loom.db")
	s, err := Open(p) // creates at latest version
	if err != nil {
		t.Fatal(err)
	}
	r := row("loom-t")
	r.Title = "hedge the vega"
	if err := s.Upsert(r); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(p) // reopen: migrations must be no-op idempotent
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, ok, _ := s2.Get("loom-t")
	if !ok || got.Title != "hedge the vega" {
		t.Fatalf("title roundtrip: %+v %v", got, ok)
	}
}

func TestSetTitle(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-a"))
	if err := s.SetTitle("loom-a", "new title"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get("loom-a")
	if got.Title != "new title" {
		t.Fatalf("Title = %q", got.Title)
	}
}

// TestMigrationsAreTransactional guards the migration-runner fix (spec §3):
// each migration's DDL + user_version bump must execute in ONE transaction,
// so a DB where v4's objects already exist but user_version is stale (as if
// a crash happened between the two under the old two-Exec-calls runner)
// still opens cleanly — re-entrancy via IF NOT EXISTS on every v4 object,
// belt-and-braces with the per-migration transaction.
func TestMigrationsAreTransactional(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loom.db")
	s, err := Open(p) // creates at the latest migration version, including v4 objects
	if err != nil {
		t.Fatal(err)
	}
	var want int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&want); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a pre-fix partial apply: v4 objects exist (created above) but
	// user_version is rolled back to 3. Re-opening must re-apply v4 (a
	// no-op via IF NOT EXISTS) AND continue on through v5, since neither was
	// recorded as applied.
	raw, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(p)
	if err != nil {
		t.Fatalf("re-entrant Open failed: %v", err)
	}
	defer s2.Close()

	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != want {
		t.Fatalf("user_version = %d, want %d", v, want)
	}

	// The re-applied v4 objects must still be usable (IF NOT EXISTS re-run
	// didn't clobber them into some broken state).
	if _, err := s2.TranscriptCount(); err != nil {
		t.Fatalf("transcripts table unusable after re-entrant migration: %v", err)
	}
}

// TestMigrationV6IndexCreatedAndReentrant guards the recall index migration
// (spec §6): idx_transcripts_project must exist after Open, and re-opening
// an already-migrated DB must be a clean no-op (IF NOT EXISTS, the same
// convention as v4/v5).
func TestMigrationV6IndexCreatedAndReentrant(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loom.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := s.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_transcripts_project'",
	).Scan(&name); err != nil {
		t.Fatalf("idx_transcripts_project missing: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(p) // reopen: migrations must be no-op idempotent
	if err != nil {
		t.Fatalf("re-entrant Open failed: %v", err)
	}
	defer s2.Close()
	if err := s2.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_transcripts_project'",
	).Scan(&name); err != nil {
		t.Fatalf("idx_transcripts_project missing after re-open: %v", err)
	}
}

// TestAddDirsRoundtrip pins migration v8's column through the whole session
// path (§5): without persistence a resumed multi-repo session silently comes
// back seeing one repo, and the failure only surfaces when a sibling write
// fails mid-turn.
func TestAddDirsRoundtrip(t *testing.T) {
	s := open(t)
	r := row("loom-multi")
	if err := s.Upsert(r); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get("loom-multi")
	if got.AddDirs != "" {
		t.Fatalf("default AddDirs = %q, want empty (migration v8 default)", got.AddDirs)
	}

	r.AddDirs = `["/w/ballista","/w/bankenstein"]`
	if err := s.Upsert(r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get("loom-multi")
	if err != nil || !ok || got.AddDirs != r.AddDirs {
		t.Fatalf("AddDirs = %q, %v %v; want %q", got.AddDirs, ok, err, r.AddDirs)
	}
	live, _ := s.Live()
	if len(live) != 1 || live[0].AddDirs != r.AddDirs {
		t.Fatalf("AddDirs lost in query(): %+v", live)
	}
	// resume writes a second row from the first: the dirs must survive, since
	// a second resume otherwise drops them again
	resumed := got
	resumed.Name = "loom-multi-2"
	if err := s.Upsert(resumed); err != nil {
		t.Fatal(err)
	}
	got2, _, _ := s.Get("loom-multi-2")
	if got2.AddDirs != r.AddDirs {
		t.Fatalf("resumed AddDirs = %q", got2.AddDirs)
	}
}

// GetByClaudeSessionID is what stops a search-resume from coming back
// single-repo (§5): search hits carry a conversation id, not a loom row name.
func TestGetByClaudeSessionID(t *testing.T) {
	s := open(t)
	first := row("loom-aaa")
	first.ClaudeSessionID = "conv-1"
	first.AddDirs = `["/w/ballista"]`
	if err := s.Upsert(first); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		id          string
		wantOK      bool
		wantName    string
		wantAddDirs string
	}{
		{name: "found", id: "conv-1", wantOK: true, wantName: "loom-aaa", wantAddDirs: `["/w/ballista"]`},
		{name: "unknown id", id: "conv-nope"},
		// '' must never match: rows are correlated to a conversation id
		// asynchronously from the transcript, so every uncorrelated row holds ''.
		{name: "empty id", id: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := s.GetByClaudeSessionID(tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got.Name != tc.wantName || got.AddDirs != tc.wantAddDirs {
				t.Fatalf("got %q/%q, want %q/%q", got.Name, got.AddDirs, tc.wantName, tc.wantAddDirs)
			}
		})
	}

	// A resume writes a SECOND row under the same conversation id, holding the
	// add-dir set filtered to directories that still exist. The newest must win,
	// or a repo that vanished gets resurrected from the original launch row.
	second := row("loom-bbb")
	second.ClaudeSessionID = "conv-1"
	second.AddDirs = `[]`
	second.CreatedAt = first.CreatedAt + 1
	if err := s.Upsert(second); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetByClaudeSessionID("conv-1")
	if err != nil || !ok || got.Name != "loom-bbb" || got.AddDirs != `[]` {
		t.Fatalf("newest row should win: %+v %v %v", got, ok, err)
	}
}

// TestMigrationV7V8FromStaleUserVersion is §9's re-entrancy test, run against
// a real DB rather than a fresh one: v7's objects are left in place and only
// user_version is rewound, which is exactly the shape that catches a v7 object
// missing IF NOT EXISTS. v8's ALTER is dropped alongside the rewind because
// ALTER TABLE has no IF NOT EXISTS — that is precisely why it holds its own
// migration slot, and this test would fail if it were folded into v7.
func TestMigrationV7V8FromStaleUserVersion(t *testing.T) {
	cases := []struct {
		name string
		// unapply rewinds a fully-migrated DB to look like a v6-era one, to
		// varying degrees of thoroughness.
		unapply []string
		// keepsProjects: the project rows survived the rewind, so the replay
		// must not have clobbered them.
		keepsProjects bool
	}{
		{
			name:          "version only", // both slots replayed against live objects
			unapply:       []string{"PRAGMA user_version = 6"},
			keepsProjects: true,
		},
		{
			name: "v7 objects still present", // IF NOT EXISTS carries the replay
			unapply: []string{
				"ALTER TABLE sessions DROP COLUMN add_dirs",
				"PRAGMA user_version = 6",
			},
			keepsProjects: true,
		},
		{
			// Both standalone ALTERs (v8 sessions.add_dirs, v9
			// projects.collapsed) rewound: each must re-apply cleanly, and the
			// project row's user edit must survive the v7 replay that runs
			// between them.
			name: "both ALTER columns rewound",
			unapply: []string{
				"ALTER TABLE sessions DROP COLUMN add_dirs",
				"ALTER TABLE projects DROP COLUMN collapsed",
				"PRAGMA user_version = 6",
			},
			keepsProjects: true,
		},
		{
			name: "genuine pre-v7 database", // nothing to collide with
			unapply: []string{
				"DROP TABLE project_repos",
				"DROP TABLE projects",
				"ALTER TABLE sessions DROP COLUMN add_dirs",
				"PRAGMA user_version = 6",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "loom.db")
			s, err := Open(p)
			if err != nil {
				t.Fatal(err)
			}
			var want int
			if err := s.db.QueryRow("PRAGMA user_version").Scan(&want); err != nil {
				t.Fatal(err)
			}
			// real history the replay must not disturb
			if err := s.Upsert(row("loom-old")); err != nil {
				t.Fatal(err)
			}
			if err := s.SetProjectHidden(UngroupedRoot, true, 1500); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			raw, err := sql.Open("sqlite", "file:"+p)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range c.unapply {
				if _, err := raw.Exec(stmt); err != nil {
					t.Fatalf("unapply %q: %v", stmt, err)
				}
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			s2, err := Open(p)
			if err != nil {
				t.Fatalf("re-entrant Open failed: %v", err)
			}
			defer s2.Close()

			var v int
			if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
				t.Fatal(err)
			}
			if v != want {
				t.Fatalf("user_version = %d, want %d", v, want)
			}
			if _, ok, _ := s2.Get("loom-old"); !ok {
				t.Fatal("session history lost across the replay")
			}
			// the re-applied v7 objects are usable and the seed was not
			// duplicated (INSERT OR IGNORE), nor its user edit clobbered when
			// the row survived the rewind
			ung, ok, err := s2.GetProject(UngroupedRoot)
			if err != nil || !ok {
				t.Fatalf("Ungrouped seed missing after replay: %v %v", ok, err)
			}
			all, err := s2.ListProjects()
			if err != nil || len(all) != 1 {
				t.Fatalf("ListProjects = %+v, %v; seed duplicated", all, err)
			}
			if c.keepsProjects && !ung.Hidden {
				t.Fatal("surviving project row was clobbered by the v7 replay")
			}
			if err := s2.Upsert(row("loom-new")); err != nil {
				t.Fatalf("sessions unusable after v8 replay: %v", err)
			}
			if err := s2.SetProjectCollapsed(UngroupedRoot, true); err != nil {
				t.Fatalf("projects.collapsed unusable after v9 replay: %v", err)
			}
		})
	}
}

// TestMigrationV8IsStandaloneALTER pins the house rule the test above depends
// on: an ALTER must never share a slot with re-entrant DDL, or a replay from a
// stale user_version fails on the ALTER even though its neighbours are
// idempotent (v2/v3 precedent).
func TestMigrationV8IsStandaloneALTER(t *testing.T) {
	s := open(t)
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v < 8 {
		t.Fatalf("user_version = %d, want >= 8", v)
	}
	for _, obj := range []string{"projects", "project_repos"} {
		var name string
		if err := s.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", obj).Scan(&name); err != nil {
			t.Fatalf("%s missing: %v", obj, err)
		}
	}
	for _, idx := range []string{"idx_projects_solo", "idx_project_repos_label", "idx_project_repos_project"} {
		var name string
		if err := s.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='index' AND name=?", idx).Scan(&name); err != nil {
			t.Fatalf("%s missing: %v", idx, err)
		}
	}
}

func TestSetSeedStatus(t *testing.T) {
	s := open(t)
	r := row("loom-aaa")
	s.Upsert(r)
	got, _, _ := s.Get("loom-aaa")
	if got.SeedStatus != "" {
		t.Fatalf("default SeedStatus = %q, want empty (migration v2 default)", got.SeedStatus)
	}
	if err := s.SetSeedStatus("loom-aaa", "sent"); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Get("loom-aaa")
	if got.SeedStatus != "sent" {
		t.Fatalf("SeedStatus = %q, want sent", got.SeedStatus)
	}
	if err := s.SetSeedStatus("loom-aaa", "failed"); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Get("loom-aaa")
	if got.SeedStatus != "failed" {
		t.Fatalf("SeedStatus = %q, want failed", got.SeedStatus)
	}
}

func TestDeleteSessionRemovesFinishedRowOnly(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-live")) // last_status "unknown" == live
	s.Upsert(row("loom-done"))
	s.MarkEnded("loom-done", "done", 0, 2000)

	// deleting a live row is a no-op (status guard)
	if err := s.DeleteSession("loom-live"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("loom-live"); !ok {
		t.Fatal("live row was deleted — status guard failed")
	}
	// deleting a finished row removes it
	if err := s.DeleteSession("loom-done"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("loom-done"); ok {
		t.Fatal("finished row was not deleted")
	}
	// unknown name is a harmless no-op
	if err := s.DeleteSession("loom-nope"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteEndedAndCountEnded(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-live"))
	s.SetStatus("loom-live", "running")
	s.Upsert(row("loom-d1"))
	s.MarkEnded("loom-d1", "done", 0, 2000)
	s.Upsert(row("loom-d2"))
	s.MarkEnded("loom-d2", "error", 1, 2001)

	n, err := s.CountEnded()
	if err != nil || n != 2 {
		t.Fatalf("CountEnded = %d, %v; want 2", n, err)
	}
	deleted, err := s.DeleteEnded()
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteEnded = %d, %v; want 2", deleted, err)
	}
	if live, _ := s.Live(); len(live) != 1 || live[0].Name != "loom-live" {
		t.Fatalf("live row lost after DeleteEnded: %+v", live)
	}
	if n, _ := s.CountEnded(); n != 0 {
		t.Fatalf("CountEnded after clear = %d; want 0", n)
	}
}

func TestEndedNames(t *testing.T) {
	s := open(t)
	s.Upsert(row("loom-live"))
	s.SetStatus("loom-live", "running")
	s.Upsert(row("loom-d1"))
	s.MarkEnded("loom-d1", "done", 0, 2000)
	s.Upsert(row("loom-d2"))
	s.MarkEnded("loom-d2", "error", 1, 2001)

	names, err := s.EndedNames()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if len(names) != 2 || !got["loom-d1"] || !got["loom-d2"] || got["loom-live"] {
		t.Fatalf("EndedNames = %v; want exactly loom-d1, loom-d2", names)
	}
}

// TestDuplicateColumnSwallowIsScopedToStandaloneALTER pins the exemption
// applyMigration grants for re-entrancy. It exists so a replay from a stale
// user_version does not brick Open() on an already-applied ALTER — and it must
// not be a general "ignore migration failures" escape hatch: in a
// multi-statement slot the same swallow would commit a half-applied migration
// and bump user_version over the damage, which is precisely what the
// single-transaction runner exists to prevent.
func TestDuplicateColumnSwallowIsScopedToStandaloneALTER(t *testing.T) {
	for _, tc := range []struct {
		name string
		ddl  string
		ok   bool
	}{
		{"standalone add column", `ALTER TABLE sessions ADD COLUMN add_dirs TEXT NOT NULL DEFAULT ''`, true},
		{"standalone, no COLUMN keyword", `ALTER TABLE sessions ADD add_dirs TEXT`, true},
		{"trailing semicolon", `ALTER TABLE sessions ADD COLUMN add_dirs TEXT;`, true},
		{"two statements", "ALTER TABLE sessions ADD COLUMN add_dirs TEXT; CREATE TABLE IF NOT EXISTS z(a)", false},
		{"multi-line slot", "CREATE TABLE IF NOT EXISTS z(a);\nALTER TABLE sessions ADD COLUMN add_dirs TEXT", false},
		{"rename, not add", `ALTER TABLE sessions RENAME COLUMN cwd TO dir`, false},
		{"a create", `CREATE TABLE IF NOT EXISTS z(a)`, false},
	} {
		if got := isStandaloneAddColumn(tc.ddl); got != tc.ok {
			t.Errorf("%s: isStandaloneAddColumn = %v, want %v", tc.name, got, tc.ok)
		}
	}

	s := open(t)
	// The live shape: re-running v8's exact DDL on a DB that already has the
	// column must succeed and bump the version, not error.
	if err := s.applyMigration(8, `ALTER TABLE sessions ADD COLUMN add_dirs TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("replaying a standalone ALTER must be a no-op, got %v", err)
	}
	// The same duplicate-column error inside a slot that is NOT a standalone
	// ALTER must still be fatal.
	err := s.applyMigration(99,
		"ALTER TABLE sessions ADD COLUMN add_dirs TEXT NOT NULL DEFAULT ''; CREATE TABLE IF NOT EXISTS z(a)")
	if err == nil {
		t.Fatal("a duplicate-column failure in a multi-statement slot must be fatal")
	}
	var n int
	if err := s.db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='z'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the failed slot committed anyway: half-applied migration")
	}
}

// TestMigrationV10ToV13FromStaleUserVersion is the orchestration arc's
// re-entrancy test, run against a REAL DB rather than a fresh one: the new
// objects are left in place (or selectively removed) and only user_version is
// rewound, which is the shape that catches a CREATE missing IF NOT EXISTS and
// an ALTER folded in beside one.
//
// Four slots land together — v10 orchestrators, v11 projects.notes_dir, v12 the
// delegation tables, v13 sessions.delegation — so the replay crosses two
// standalone ALTERs with re-entrant CREATE slots on both sides of each. That is
// exactly the arrangement that breaks if either ALTER is ever folded into a
// neighbouring slot: `duplicate column name` is only swallowed for a slot that
// is nothing BUT an ALTER (isStandaloneAddColumn), and a multi-statement slot
// would strand every statement after the failing one while bumping the version
// over the damage.
func TestMigrationV10ToV13FromStaleUserVersion(t *testing.T) {
	cases := []struct {
		name string
		// unapply rewinds a fully-migrated DB to look like a v9-era one, to
		// varying degrees of thoroughness.
		unapply []string
		// keepsEdits: the pre-existing rows survived the rewind, so the replay
		// must not have clobbered them.
		keepsEdits bool
	}{
		{
			name:       "version only", // all four slots replayed against live objects
			unapply:    []string{"PRAGMA user_version = 9"},
			keepsEdits: true,
		},
		{
			name: "both new ALTER columns rewound",
			unapply: []string{
				"ALTER TABLE projects DROP COLUMN notes_dir",
				"ALTER TABLE sessions DROP COLUMN delegation",
				"PRAGMA user_version = 9",
			},
		},
		{
			// One CREATE slot's objects gone, the other's intact: v10 must
			// rebuild and v12 must no-op through IF NOT EXISTS in the same
			// replay. keepsEdits is false because the dropped table took its
			// row with it — that is the rewind, not a replay defect.
			name: "orchestrators dropped, delegation tables kept",
			unapply: []string{
				"DROP TABLE orchestrators",
				"PRAGMA user_version = 9",
			},
		},
		{
			name: "genuine pre-v10 database", // nothing to collide with
			unapply: []string{
				"DROP TABLE orchestrators",
				"DROP TABLE delegation_artifacts",
				"DROP TABLE delegation_tasks",
				"DROP TABLE delegation_runs",
				"ALTER TABLE projects DROP COLUMN notes_dir",
				"ALTER TABLE sessions DROP COLUMN delegation",
				"PRAGMA user_version = 9",
			},
		},
		{
			name: "all the way back to v6", // the slice-1 replay still works underneath
			unapply: []string{
				"DROP TABLE orchestrators",
				"DROP TABLE delegation_artifacts",
				"DROP TABLE delegation_tasks",
				"DROP TABLE delegation_runs",
				"ALTER TABLE projects DROP COLUMN notes_dir",
				"ALTER TABLE projects DROP COLUMN collapsed",
				"ALTER TABLE sessions DROP COLUMN delegation",
				"ALTER TABLE sessions DROP COLUMN add_dirs",
				"PRAGMA user_version = 6",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "loom.db")
			s, err := Open(p)
			if err != nil {
				t.Fatal(err)
			}
			var want int
			if err := s.db.QueryRow("PRAGMA user_version").Scan(&want); err != nil {
				t.Fatal(err)
			}
			// real state the replay must not disturb, one row per new object
			if err := s.Upsert(row("loom-old")); err != nil {
				t.Fatal(err)
			}
			if err := s.SetProjectNotesDir(UngroupedRoot, "/h/.loom/projects/x/notes", 1500); err != nil {
				t.Fatal(err)
			}
			if ok, _, err := s.ClaimOrchestrator("/w/a", 1000); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			run, err := s.InsertDelegationRun("atlas", "/w/a", "{}", "{}", 1000)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			raw, err := sql.Open("sqlite", "file:"+p)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range c.unapply {
				if _, err := raw.Exec(stmt); err != nil {
					t.Fatalf("unapply %q: %v", stmt, err)
				}
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			s2, err := Open(p)
			if err != nil {
				t.Fatalf("re-entrant Open failed: %v", err)
			}
			defer s2.Close()

			var v int
			if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
				t.Fatal(err)
			}
			if v != want {
				t.Fatalf("user_version = %d, want %d", v, want)
			}
			if _, ok, _ := s2.Get("loom-old"); !ok {
				t.Fatal("session history lost across the replay")
			}
			if c.keepsEdits {
				ung, ok, err := s2.GetProject(UngroupedRoot)
				if err != nil || !ok || ung.NotesDir != "/h/.loom/projects/x/notes" {
					t.Fatalf("notes_dir clobbered by the replay: %+v %v %v", ung, ok, err)
				}
				o, ok, err := s2.GetOrchestrator("/w/a")
				if err != nil || !ok || o.SpawnedAt != 1000 {
					t.Fatalf("orchestrator claim clobbered by the replay: %+v %v %v", o, ok, err)
				}
				got, ok, err := s2.GetDelegationRun(run.ID)
				if err != nil || !ok || got.Slug != run.Slug {
					t.Fatalf("delegation run clobbered by the replay: %+v %v %v", got, ok, err)
				}
			}

			// every new object is usable after the replay, not merely present
			if err := s2.Upsert(row("loom-new")); err != nil {
				t.Fatalf("sessions unusable after the v13 replay: %v", err)
			}
			if err := s2.SetSessionDelegation("loom-new", "1:schema"); err != nil {
				t.Fatalf("sessions.delegation unusable after the v13 replay: %v", err)
			}
			if err := s2.SetProjectNotesDir(UngroupedRoot, "/h/notes2", 1600); err != nil {
				t.Fatalf("projects.notes_dir unusable after the v11 replay: %v", err)
			}
			if _, _, err := s2.ClaimOrchestrator("/w/b", 2000); err != nil {
				t.Fatalf("orchestrators unusable after the v10 replay: %v", err)
			}
			r2, err := s2.InsertDelegationRun("atlas2", "/w/a", "{}", "{}", 2000)
			if err != nil {
				t.Fatalf("delegation_runs unusable after the v12 replay: %v", err)
			}
			if err := s2.InsertDelegationTask(DelegationTask{RunID: r2.ID, TaskID: "t",
				State: "pending", RepoLabel: "r", UpdatedAt: 2000}); err != nil {
				t.Fatalf("delegation_tasks unusable after the v12 replay: %v", err)
			}
			if err := s2.UpsertDelegationArtifact(DelegationArtifact{RunID: r2.ID,
				ArtifactID: "a", TaskID: "t", Path: "p"}); err != nil {
				t.Fatalf("delegation_artifacts unusable after the v12 replay: %v", err)
			}
		})
	}
}

// TestUserVersionIsThirteen asserts the ABSOLUTE migration head, which the
// orchestrator spec §13 asks for by name and for a specific reason: revision 1
// of that spec allocated an already-occupied slot, and migrate() loops
// `for i := v; i < len(migrations)`, so a DB sitting at the colliding version
// would skip the new slot entirely and open cleanly WITHOUT it — green on every
// fresh-DB test, broken on every real one. A relative assertion cannot catch
// that. When a later slice adds a slot, this number moves with it deliberately.
func TestUserVersionIsThirteen(t *testing.T) {
	s := open(t)
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 13 {
		t.Fatalf("user_version = %d, want 13", v)
	}
	for _, obj := range []string{"orchestrators", "delegation_runs", "delegation_tasks",
		"delegation_artifacts"} {
		var name string
		if err := s.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", obj).Scan(&name); err != nil {
			t.Fatalf("%s missing: %v", obj, err)
		}
	}
	for _, idx := range []string{"idx_dtasks_state", "idx_druns_project"} {
		var name string
		if err := s.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='index' AND name=?", idx).Scan(&name); err != nil {
			t.Fatalf("%s missing: %v", idx, err)
		}
	}
	// delegation_amendments belongs to the deferred §§9-12 block. An empty
	// table is an invitation to write to it, so its absence is deliberate and
	// pinned here — unparking that work must add a slot, not discover one.
	var name string
	if err := s.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='delegation_amendments'",
	).Scan(&name); err != sql.ErrNoRows {
		t.Fatalf("delegation_amendments exists (%q, %v); slice 3a does not ship it", name, err)
	}
}

// TestSessionDelegationRoundtrip pins migration v13 through the session path.
// Without it a delegation child's cwd (a worktree under ~/.loom) matches no
// project target, the resolver fails closed, and every child vanishes from the
// rail the moment anything is hidden — including when the user soloed the run's
// own project.
func TestSessionDelegationRoundtrip(t *testing.T) {
	s := open(t)
	r := row("loom-child")
	if err := s.Upsert(r); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get("loom-child")
	if got.Delegation != "" {
		t.Fatalf("default Delegation = %q, want empty (migration v13 default)", got.Delegation)
	}
	if err := s.SetSessionDelegation("loom-child", "7:auth-api"); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Get("loom-child")
	if got.Delegation != "7:auth-api" {
		t.Fatalf("Delegation = %q", got.Delegation)
	}
	live, _ := s.Live()
	if len(live) != 1 || live[0].Delegation != "7:auth-api" {
		t.Fatalf("Delegation lost in query(): %+v", live)
	}
	// a re-spawn writes a second row from the first (the Resume shape): the
	// linkage must survive or the new child is unattributable again
	resumed := got
	resumed.Name = "loom-child-2"
	if err := s.Upsert(resumed); err != nil {
		t.Fatal(err)
	}
	if got2, _, _ := s.Get("loom-child-2"); got2.Delegation != "7:auth-api" {
		t.Fatalf("resumed Delegation = %q", got2.Delegation)
	}
}

// TestProjectNotesDirSurvivesRepoint is the derived-default regression the
// orchestrator spec §3 names explicitly: notes_dir is MATERIALIZED into the row
// rather than derived from root, so renaming the project's directory does not
// silently relocate its whole brain.
func TestProjectNotesDirSurvivesRepoint(t *testing.T) {
	s := open(t)
	if err := s.UpsertProject(Project{Root: "/w/old", Name: "P", Origin: "created",
		CreatedAt: 1000, UpdatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetProject("/w/old"); got.NotesDir != "" {
		t.Fatalf("default NotesDir = %q, want empty (migration v11 default)", got.NotesDir)
	}
	const notes = "/h/.loom/projects/P-9f2c1ab4/notes"
	if err := s.SetProjectNotesDir("/w/old", notes, 1100); err != nil {
		t.Fatal(err)
	}
	if err := s.RepointProject("/w/old", "/w/new", 1200); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetProject("/w/new")
	if err != nil || !ok {
		t.Fatalf("GetProject after repoint: %v %v", ok, err)
	}
	if got.NotesDir != notes {
		t.Fatalf("NotesDir = %q after repoint, want %q", got.NotesDir, notes)
	}
	list, _ := s.ListProjects()
	for _, p := range list {
		if p.Root == "/w/new" && p.NotesDir != notes {
			t.Fatalf("NotesDir lost in ListProjects: %+v", p)
		}
	}
}

// TestMigrationHeadIsPinned pins the schema head. The orchestrator spec §9 and
// the delegation spec §13.1 were both written against a v9 head and both
// allocated v10/v11 for different DDL, so the tree renumbered delegation to
// v12/v13. That renumbering is exactly the kind of decision that a later slice
// re-breaks by allocating "the next free slot" from a stale spec, and the
// failure is silent in the worst way: migrate() loops
// `for i := v; i < len(migrations)`, so a DB already at a colliding version
// SKIPS the new slot entirely and opens cleanly WITHOUT the tables — green on
// a fresh DB, broken on every real one.
//
// This test failing is not a bug to route around by editing the constant. It
// means a slot was added, and the question to answer is whether every
// already-migrated DB in existence will still receive it.
func TestMigrationHeadIsPinned(t *testing.T) {
	const wantHead = 13

	s := open(t)
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != wantHead {
		t.Fatalf("PRAGMA user_version = %d, want %d — a migration slot was added or renumbered; "+
			"confirm a DB already at %d still receives it before updating this constant", v, wantHead, v)
	}
}

// TestMigrationsReplayFromEveryStaleVersion is the generalized form of
// TestMigrationsAreTransactional, and it is what catches a NON-RE-ENTRANT
// ALTER. Every slot must be replayable against a DB whose objects already
// exist but whose user_version was rolled back — the pre-fix partial-apply
// shape, and also what a two-instance install looks like mid-upgrade.
//
// CREATE ... IF NOT EXISTS is re-entrant by construction; ALTER TABLE ADD
// COLUMN is NOT, which is why the house rule gives every ALTER its own slot
// and applyMigration exempts exactly the standalone-ADD-COLUMN shape. v8, v9
// and v11 are all bare ALTERs, so replaying from 7, 8 or 10 is the case that
// actually exercises that exemption. Replaying from EVERY version rather than
// one hand-picked version means a future ALTER cannot be added into a slot
// this test does not cover.
func TestMigrationsReplayFromEveryStaleVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loom.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	var head int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&head); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// From v1, not v0. v1 predates the IF-NOT-EXISTS house rule and creates
	// `sessions` bare, so replaying it against a populated DB fails on "table
	// sessions already exists". That is not a live defect and the fix is not
	// available: a DB at user_version 0 is by definition a brand-new empty
	// file, so the replay-from-0 case cannot occur on real data, and the house
	// rule forbids editing a shipped migration to make a test happy. Every
	// slot that a real DB can actually be stale at IS covered.
	for stale := 1; stale <= head; stale++ {
		t.Run(fmt.Sprintf("from_v%d", stale), func(t *testing.T) {
			// A real DB copy, not a fresh file: the objects are already there,
			// so every statement in every replayed slot runs against existing
			// state — which is the only way a non-re-entrant statement shows up.
			dir := t.TempDir()
			dst := filepath.Join(dir, "loom.db")
			blob, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, blob, 0o600); err != nil {
				t.Fatal(err)
			}

			raw, err := sql.Open("sqlite", "file:"+dst)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version = %d", stale)); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			s2, err := Open(dst)
			if err != nil {
				t.Fatalf("replay from stale user_version %d failed: %v", stale, err)
			}
			defer s2.Close()

			var got int
			if err := s2.db.QueryRow("PRAGMA user_version").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != head {
				t.Fatalf("after replay from %d: user_version = %d, want %d", stale, got, head)
			}

			// The replayed objects must still be USABLE, not merely present —
			// a re-run that silently emptied or reshaped a table would leave
			// the version correct and the DB wrong. One touch per slice's
			// headline table, orchestrator (v10/v11) and delegation (v12/v13)
			// included, since those are the slots this suite is newest at.
			if _, err := s2.TranscriptCount(); err != nil {
				t.Fatalf("transcripts unusable after replay from %d: %v", stale, err)
			}
			if _, err := s2.ListOrchestrators(); err != nil {
				t.Fatalf("orchestrators unusable after replay from %d: %v", stale, err)
			}
			if _, err := s2.ListProjects(); err != nil {
				t.Fatalf("projects unusable after replay from %d: %v", stale, err)
			}
		})
	}
}
