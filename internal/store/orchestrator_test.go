package store

import "testing"

// TestClaimOrchestratorSingleton pins orchestrator spec §2's guarantee — "at
// most one orchestrator per project at a time" — at the only layer that can
// hold it. A UI guard is per-process and two Loom instances against one DB is a
// supported state, so if this table does not fail, nothing does.
func TestClaimOrchestratorSingleton(t *testing.T) {
	const root = "/w/Innostream"

	t.Run("second claim loses and names the winner", func(t *testing.T) {
		s := open(t)
		ok, _, err := s.ClaimOrchestrator(root, 1000)
		if err != nil || !ok {
			t.Fatalf("first claim: %v %v", ok, err)
		}
		if _, err := s.BindOrchestratorSession(root, "loom-winner", "conv-1"); err != nil {
			t.Fatal(err)
		}
		ok, existing, err := s.ClaimOrchestrator(root, 2000)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("second claim succeeded: two orchestrators for one project")
		}
		// The refusal has to be explainable — §7.3's error names the existing
		// session, and it can only do that if the claim hands the row back.
		if existing.SessionName != "loom-winner" || existing.EndedAt != -1 {
			t.Fatalf("loser got %+v, want the live winner", existing)
		}
		got, _, _ := s.GetOrchestrator(root)
		if got.SessionName != "loom-winner" || got.SpawnedAt != 1000 {
			t.Fatalf("losing claim mutated the row: %+v", got)
		}
	})

	t.Run("claim in flight also blocks", func(t *testing.T) {
		// The dangerous window: a claim exists, the launch has not returned, so
		// session_name is still ''. If occupancy were "has a session name" the
		// two-instance double-press would launch twice.
		s := open(t)
		if ok, _, _ := s.ClaimOrchestrator(root, 1000); !ok {
			t.Fatal("first claim rejected")
		}
		if ok, _, _ := s.ClaimOrchestrator(root, 1001); ok {
			t.Fatal("in-flight claim did not block a second spawn")
		}
	})

	t.Run("ended row is overwritten, not blocking", func(t *testing.T) {
		// §9 keeps a terminated orchestrator's row so the overview can say
		// "last orchestrator ran Tuesday". If existence meant occupancy, a
		// project could be spawned into exactly once, ever.
		s := open(t)
		s.ClaimOrchestrator(root, 1000)
		s.BindOrchestratorSession(root, "loom-old", "conv-1")
		if ok, err := s.EndOrchestrator(root, 1500); err != nil || !ok {
			t.Fatalf("EndOrchestrator: %v %v", ok, err)
		}
		ok, _, err := s.ClaimOrchestrator(root, 2000)
		if err != nil || !ok {
			t.Fatalf("claim after end: %v %v", ok, err)
		}
		got, _, _ := s.GetOrchestrator(root)
		if got.SessionName != "" || got.ClaudeSessionID != "" || got.SpawnedAt != 2000 || got.EndedAt != -1 {
			t.Fatalf("re-claim left stale identity: %+v", got)
		}
	})

	t.Run("claims are per project", func(t *testing.T) {
		s := open(t)
		if ok, _, _ := s.ClaimOrchestrator("/w/a", 1000); !ok {
			t.Fatal("claim on /w/a rejected")
		}
		if ok, _, _ := s.ClaimOrchestrator("/w/b", 1000); !ok {
			t.Fatal("claim on /w/b rejected: the singleton is per project, not global")
		}
	})
}

// TestOrchestratorRoundtrip covers the plain row shape and the list query the
// GUI's single-query join (§10, no N+1) depends on.
func TestOrchestratorRoundtrip(t *testing.T) {
	s := open(t)
	if _, ok, err := s.GetOrchestrator("/w/none"); ok || err != nil {
		t.Fatalf("GetOrchestrator on unknown root = %v %v, want false nil", ok, err)
	}
	if list, err := s.ListOrchestrators(); err != nil || len(list) != 0 {
		t.Fatalf("ListOrchestrators empty = %+v %v", list, err)
	}

	s.ClaimOrchestrator("/w/b", 1000)
	s.BindOrchestratorSession("/w/b", "loom-b", "conv-b")
	s.ClaimOrchestrator("/w/a", 900)
	s.BindOrchestratorSession("/w/a", "loom-a", "")
	// the conversation id arrives later, off the transcript, not at launch
	if err := s.SetOrchestratorClaudeSessionID("/w/a", "loom-a", "conv-a"); err != nil {
		t.Fatal(err)
	}
	s.EndOrchestrator("/w/a", 1200)

	list, err := s.ListOrchestrators()
	if err != nil || len(list) != 2 {
		t.Fatalf("ListOrchestrators = %+v %v", list, err)
	}
	want := []Orchestrator{
		{ProjectRoot: "/w/a", SessionName: "loom-a", ClaudeSessionID: "conv-a", SpawnedAt: 900, EndedAt: 1200},
		{ProjectRoot: "/w/b", SessionName: "loom-b", ClaudeSessionID: "conv-b", SpawnedAt: 1000, EndedAt: -1},
	}
	for i := range want {
		if list[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, list[i], want[i])
		}
	}
	// a late correction keyed on the wrong session name must not land
	if err := s.SetOrchestratorClaudeSessionID("/w/b", "loom-stale", "conv-x"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetOrchestrator("/w/b"); got.ClaudeSessionID != "conv-b" {
		t.Fatalf("stale session name overwrote the live claude id: %+v", got)
	}
}

// TestEndOrchestratorCAS: two Loom instances sweep the same dead session on
// their own poll loops. The second must not overwrite the first's timestamp —
// "last orchestrator ran Tuesday" has to mean Tuesday.
func TestEndOrchestratorCAS(t *testing.T) {
	s := open(t)
	s.ClaimOrchestrator("/w/a", 1000)
	s.BindOrchestratorSession("/w/a", "loom-a", "conv-a")

	if ok, _ := s.EndOrchestrator("/w/a", 1500); !ok {
		t.Fatal("first end rejected")
	}
	if ok, _ := s.EndOrchestrator("/w/a", 9999); ok {
		t.Fatal("second end claimed: a stale snapshot won the race")
	}
	if got, _, _ := s.GetOrchestrator("/w/a"); got.EndedAt != 1500 {
		t.Fatalf("EndedAt = %d, want 1500 (the first sweep's stamp)", got.EndedAt)
	}
	if ok, _ := s.EndOrchestrator("/w/never", 1500); ok {
		t.Fatal("ended a claim that does not exist")
	}
}

// TestBindOrchestratorSessionCAS pins the guard that keeps a late bind from a
// failed-then-swept spawn out of a NEWER claim's row.
func TestBindOrchestratorSessionCAS(t *testing.T) {
	s := open(t)
	s.ClaimOrchestrator("/w/a", 1000)
	// spawn A's launch hangs; the claim is swept, spawn B wins the root
	if n, err := s.SweepStaleOrchestratorClaims(1001); err != nil || n != 1 {
		t.Fatalf("sweep = %d %v, want 1", n, err)
	}
	s.ClaimOrchestrator("/w/a", 2000)
	if ok, _ := s.BindOrchestratorSession("/w/a", "loom-b", "conv-b"); !ok {
		t.Fatal("B's bind rejected")
	}
	// A's launch finally returns
	ok, err := s.BindOrchestratorSession("/w/a", "loom-a", "conv-a")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("late bind claimed a row it no longer owns")
	}
	if got, _, _ := s.GetOrchestrator("/w/a"); got.SessionName != "loom-b" {
		t.Fatalf("late bind stamped a dead session over the live one: %+v", got)
	}
}

// TestSweepStaleOrchestratorClaims is §7's recovery for the disclosed
// claim-then-launch failure: without it, one failed launch locks a project out
// of ever spawning again.
func TestSweepStaleOrchestratorClaims(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*Store)
		cutoff  int64
		want    int64
		survive string // root that must still have a row afterwards
	}{
		{
			name:    "stranded claim older than the grace is swept",
			setup:   func(s *Store) { s.ClaimOrchestrator("/w/a", 1000) },
			cutoff:  1060,
			want:    1,
			survive: "",
		},
		{
			name:    "claim younger than the grace is left alone",
			setup:   func(s *Store) { s.ClaimOrchestrator("/w/a", 1000) },
			cutoff:  1000,
			want:    0,
			survive: "/w/a",
		},
		{
			name: "a claim that became a session is never swept",
			setup: func(s *Store) {
				s.ClaimOrchestrator("/w/a", 1000)
				s.BindOrchestratorSession("/w/a", "loom-a", "conv-a")
			},
			cutoff:  9999,
			want:    0,
			survive: "/w/a",
		},
		{
			name: "a retired orchestrator is history, not a stranded claim",
			setup: func(s *Store) {
				s.ClaimOrchestrator("/w/a", 1000)
				s.BindOrchestratorSession("/w/a", "loom-a", "conv-a")
				s.EndOrchestrator("/w/a", 1100)
			},
			cutoff:  9999,
			want:    0,
			survive: "/w/a",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := open(t)
			c.setup(s)
			n, err := s.SweepStaleOrchestratorClaims(c.cutoff)
			if err != nil || n != c.want {
				t.Fatalf("sweep = %d %v, want %d", n, err, c.want)
			}
			_, ok, _ := s.GetOrchestrator("/w/a")
			if ok != (c.survive == "/w/a") {
				t.Fatalf("row present = %v, want %v", ok, c.survive == "/w/a")
			}
		})
	}
}

// TestAdoptOrchestrator covers §9's adopt-before-reap: a live `orch`-tagged
// session with no row is adopted rather than treated as an orphan, and an
// adoption never clobbers a live claim.
func TestAdoptOrchestrator(t *testing.T) {
	s := open(t)
	found := Orchestrator{ProjectRoot: "/w/a", SessionName: "loom-found",
		ClaudeSessionID: "conv-f", SpawnedAt: 500, EndedAt: -1}

	ok, err := s.AdoptOrchestrator(found)
	if err != nil || !ok {
		t.Fatalf("adopt with no row: %v %v", ok, err)
	}
	if got, _, _ := s.GetOrchestrator("/w/a"); got != found {
		t.Fatalf("adopted row = %+v, want %+v", got, found)
	}

	// A live claim outranks a discovery: adopting over it would silently
	// rebadge the winner's row with a second orchestrator's identity.
	s2 := open(t)
	s2.ClaimOrchestrator("/w/a", 1000)
	s2.BindOrchestratorSession("/w/a", "loom-live", "conv-l")
	if ok, _ := s2.AdoptOrchestrator(found); ok {
		t.Fatal("adoption clobbered a live claim")
	}
	if got, _, _ := s2.GetOrchestrator("/w/a"); got.SessionName != "loom-live" {
		t.Fatalf("live row was rebadged: %+v", got)
	}
}
