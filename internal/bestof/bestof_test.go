package bestof

import (
	"path/filepath"
	"testing"
)

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bestof.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Group{}); err == nil {
		t.Fatal("a group with no id was stored")
	}
	g := Group{ID: NewID(), Repo: "/r", Base: "abc", Seed: "do it", Created: 10, Sessions: []string{"loom-a", "loom-b", "loom-c"}}
	if err := s.Put(g); err != nil {
		t.Fatal(err)
	}
	s.Put(Group{ID: "other", Created: 11, Sessions: []string{"loom-x"}})

	s2, _ := Open(path)
	if got, ok := s2.Get(g.ID); !ok || got.Seed != "do it" || len(got.Sessions) != 3 {
		t.Fatalf("not persisted: %+v", got)
	}
	if p := s2.Of()["loom-b"]; p.Group != g.ID || p.Index != 2 || p.Of != 3 {
		t.Fatalf("Of = %+v", p)
	}
	g.Kept, g.Discarded = "loom-b", []string{"loom-a", "loom-c"}
	s2.Put(g)
	if s2.Of()["loom-a"].Kept != "loom-b" {
		t.Fatal("kept not reported")
	}
	// Only loom-c's row is left: the group stays. loom-x is gone: its group goes.
	s2.Prune(func(n string) bool { return n == "loom-c" })
	s3, _ := Open(path)
	if _, ok := s3.Get(g.ID); !ok {
		t.Fatal("a group with a remaining session was pruned")
	}
	if _, ok := s3.Get("other"); ok {
		t.Fatal("a group with no sessions left was kept")
	}
}
