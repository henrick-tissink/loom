package store

import (
	"database/sql"
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
	s, err := Open(p) // creates at latest version (v5), including v4 objects
	if err != nil {
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
	if v != 5 {
		t.Fatalf("user_version = %d, want 5", v)
	}

	// The re-applied v4 objects must still be usable (IF NOT EXISTS re-run
	// didn't clobber them into some broken state).
	if _, err := s2.TranscriptCount(); err != nil {
		t.Fatalf("transcripts table unusable after re-entrant migration: %v", err)
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
