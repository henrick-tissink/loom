package store

import (
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

	if err := s.MarkLiveOrphansEnded([]string{"loom-aaa"}, 3000); err != nil {
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
