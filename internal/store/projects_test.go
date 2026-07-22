package store

import (
	"database/sql"
	"errors"
	"testing"
)

func proj(root, name string) Project {
	return Project{Root: root, Name: name, Origin: "discovered", CreatedAt: 1000, UpdatedAt: 1000}
}

func repo(path, root, label string) ProjectRepo {
	return ProjectRepo{Path: path, ProjectRoot: root, Label: label, AddedAt: 1000}
}

func mustUpsertProject(t *testing.T, s *Store, p Project) {
	t.Helper()
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject(%s): %v", p.Root, err)
	}
}

func mustUpsertRepo(t *testing.T, s *Store, r ProjectRepo) {
	t.Helper()
	if err := s.UpsertProjectRepo(r); err != nil {
		t.Fatalf("UpsertProjectRepo(%s): %v", r.Path, err)
	}
}

// TestUngroupedSeeded pins the §7 reserved row: Ungrouped must be a real
// project row (not a computed bucket) so §6's predicate has something to key
// on, and re-opening must not mint a second one.
func TestUngroupedSeeded(t *testing.T) {
	s := open(t)
	p, ok, err := s.GetProject(UngroupedRoot)
	if err != nil || !ok {
		t.Fatalf("Ungrouped row missing: %v %v", ok, err)
	}
	if p.Name != "Ungrouped" || p.Origin != "reserved" {
		t.Fatalf("seed = %+v, want name=Ungrouped origin=reserved", p)
	}
	if p.Hidden || p.Solo || p.Missing {
		t.Fatalf("seed flags set: %+v", p)
	}
	if p.CreatedAt == 0 || p.UpdatedAt == 0 {
		t.Fatalf("seed timestamps unset: %+v", p)
	}

	all, err := s.ListProjects()
	if err != nil || len(all) != 1 {
		t.Fatalf("ListProjects = %+v, %v; want exactly the seed", all, err)
	}
}

// TestUpsertProjectDoesNotClobber is the §7 upsert-without-clobber rule:
// discovery re-runs on every launch and must never overwrite a user-set name,
// hidden, solo or origin.
func TestUpsertProjectDoesNotClobber(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(t *testing.T, s *Store)
		check func(t *testing.T, p Project)
	}{
		{
			name: "rename",
			edit: func(t *testing.T, s *Store) {
				if err := s.SetProjectName("/w/inno", "Innostream Ltd", 2000); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, p Project) {
				if p.Name != "Innostream Ltd" {
					t.Fatalf("Name = %q, rename clobbered", p.Name)
				}
			},
		},
		{
			name: "hide",
			edit: func(t *testing.T, s *Store) {
				if err := s.SetProjectHidden("/w/inno", true, 2000); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, p Project) {
				if !p.Hidden {
					t.Fatal("Hidden cleared by re-discovery")
				}
			},
		},
		{
			name: "solo",
			edit: func(t *testing.T, s *Store) {
				if err := s.SetProjectSolo("/w/inno", true, 2000); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, p Project) {
				if !p.Solo {
					t.Fatal("Solo cleared by re-discovery")
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := open(t)
			mustUpsertProject(t, s, proj("/w/inno", "inno"))
			c.edit(t, s)

			// discovery pass two: same root, everything else as freshly scanned
			redisc := proj("/w/inno", "inno")
			redisc.Origin = "discovered"
			mustUpsertProject(t, s, redisc)

			p, ok, _ := s.GetProject("/w/inno")
			if !ok {
				t.Fatal("project vanished")
			}
			c.check(t, p)
			if all, _ := s.ListProjects(); len(all) != 2 {
				t.Fatalf("ListProjects = %d rows; re-upsert duplicated", len(all))
			}
		})
	}
}

// TestUpsertProjectPreservesCreatedOrigin guards the created/discovered
// distinction origin exists for (§7 reconciliation): a scan crossing a
// user-created project must not demote it to 'discovered'.
func TestUpsertProjectPreservesCreatedOrigin(t *testing.T) {
	s := open(t)
	created := proj("/w/atlas", "Atlas")
	created.Origin = "created"
	mustUpsertProject(t, s, created)
	mustUpsertProject(t, s, proj("/w/atlas", "atlas")) // discovery finds it

	p, _, _ := s.GetProject("/w/atlas")
	if p.Origin != "created" || p.Name != "Atlas" {
		t.Fatalf("got %+v, want origin=created name=Atlas", p)
	}
}

// TestSetProjectSoloIsExclusive pins the partial unique index's contract
// (§6.1): at most one project is solo, and leaving solo restores hidden
// exactly because solo never touches it.
func TestSetProjectSoloIsExclusive(t *testing.T) {
	s := open(t)
	mustUpsertProject(t, s, proj("/w/a", "a"))
	mustUpsertProject(t, s, proj("/w/b", "b"))
	if err := s.SetProjectHidden("/w/a", true, 1500); err != nil {
		t.Fatal(err)
	}

	if err := s.SetProjectSolo("/w/a", true, 2000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectSolo("/w/b", true, 2001); err != nil {
		t.Fatalf("second solo rejected instead of superseding: %v", err)
	}
	a, _, _ := s.GetProject("/w/a")
	b, _, _ := s.GetProject("/w/b")
	if a.Solo || !b.Solo {
		t.Fatalf("solo not exclusive: a=%+v b=%+v", a, b)
	}
	if !a.Hidden {
		t.Fatal("solo clobbered hidden — exiting solo would not restore prior state")
	}

	if err := s.SetProjectSolo("/w/b", false, 2002); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"/w/a", "/w/b"} {
		p, _, _ := s.GetProject(root)
		if p.Solo {
			t.Fatalf("%s still solo after clear: %+v", root, p)
		}
	}
	a, _, _ = s.GetProject("/w/a")
	if !a.Hidden {
		t.Fatal("hidden lost across a solo round-trip")
	}
}

// TestSetProjectCollapsed pins §8's "collapse state lives in loom.db alongside
// the other project flags": it round-trips through ListProjects (the DTO's
// source), survives a re-open, and — like solo/hidden — a re-running discovery
// upsert must not reset it.
func TestSetProjectCollapsed(t *testing.T) {
	s := open(t)
	mustUpsertProject(t, s, proj("/w/a", "a"))
	mustUpsertProject(t, s, proj("/w/b", "b"))

	if p, _, _ := s.GetProject("/w/a"); p.Collapsed {
		t.Fatalf("default = collapsed: %+v", p)
	}
	if err := s.SetProjectCollapsed("/w/a", true); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if want := p.Root == "/w/a"; p.Collapsed != want {
			t.Fatalf("%s collapsed=%v, want %v", p.Root, p.Collapsed, want)
		}
	}

	// discovery re-runs on every launch; a collapsed section must not spring
	// open because the reconciler saw the project again.
	mustUpsertProject(t, s, proj("/w/a", "a"))
	if p, _, _ := s.GetProject("/w/a"); !p.Collapsed {
		t.Fatalf("re-upsert clobbered collapsed: %+v", p)
	}

	if err := s.SetProjectCollapsed("/w/a", false); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.GetProject("/w/a"); p.Collapsed {
		t.Fatalf("collapse not cleared: %+v", p)
	}
}

func TestUpsertProjectRepoDoesNotClobberMembership(t *testing.T) {
	s := open(t)
	mustUpsertProject(t, s, proj("/w/inno", "inno"))
	mustUpsertProject(t, s, proj("/w/solo-repo", "solo-repo"))
	mustUpsertRepo(t, s, repo("/w/inno/ballista", "/w/inno", "inno/ballista"))

	// user reassigns, then discovery re-runs with its original derivation
	if err := s.ReassignRepo("/w/inno/ballista", "/w/solo-repo", "ballista"); err != nil {
		t.Fatal(err)
	}
	mustUpsertRepo(t, s, repo("/w/inno/ballista", "/w/inno", "inno/ballista"))

	r, ok, _ := s.GetProjectRepo("/w/inno/ballista")
	if !ok || r.ProjectRoot != "/w/solo-repo" || r.Label != "ballista" {
		t.Fatalf("re-discovery re-absorbed the repo: %+v", r)
	}
	if rs, _ := s.ListProjectRepos("/w/inno"); len(rs) != 0 {
		t.Fatalf("old project still lists the repo: %+v", rs)
	}
}

// TestUpsertProjectRepoLabelConflict: §2 makes a label collision non-fatal —
// the reconciler must be able to skip the row and warn, which needs a
// distinguishable error rather than an opaque constraint failure.
func TestUpsertProjectRepoLabelConflict(t *testing.T) {
	s := open(t)
	mustUpsertProject(t, s, proj("/w/inno", "inno"))
	mustUpsertRepo(t, s, repo("/w/inno/ballista", "/w/inno", "ballista"))

	err := s.UpsertProjectRepo(repo("/w/other/ballista", "/w/inno", "ballista"))
	if !errors.Is(err, ErrLabelTaken) {
		t.Fatalf("err = %v, want ErrLabelTaken", err)
	}
	if _, ok, _ := s.GetProjectRepo("/w/other/ballista"); ok {
		t.Fatal("conflicting row was inserted anyway")
	}
}

func TestReassignRepo(t *testing.T) {
	s := open(t)
	mustUpsertProject(t, s, proj("/w/inno", "inno"))
	mustUpsertProject(t, s, proj("/w/hp", "hp"))
	mustUpsertRepo(t, s, repo("/w/inno/quickbit", "/w/inno", "inno/quickbit"))

	if err := s.ReassignRepo("/w/inno/quickbit", "/w/hp", "hp/quickbit"); err != nil {
		t.Fatal(err)
	}
	r, _, _ := s.GetProjectRepo("/w/inno/quickbit")
	if r.ProjectRoot != "/w/hp" || r.Label != "hp/quickbit" {
		t.Fatalf("reassign: %+v", r)
	}
	if rs, _ := s.ListProjectRepos("/w/hp"); len(rs) != 1 {
		t.Fatalf("target project = %+v", rs)
	}

	// unknown target project must not strand the row under a root nothing lists
	if err := s.ReassignRepo("/w/inno/quickbit", "/w/nope", "x"); !errors.Is(err, ErrNoProject) {
		t.Fatalf("err = %v, want ErrNoProject", err)
	}
	if r2, _, _ := s.GetProjectRepo("/w/inno/quickbit"); r2 != r {
		t.Fatalf("failed reassign mutated the row: %+v", r2)
	}

	// unknown repo path
	if err := s.ReassignRepo("/w/ghost", "/w/hp", "hp/ghost"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

// TestRemoveRepoIsReassignment pins §7's definition of "remove from project":
// reassignment to a single-repo project at the repo's own path. The row must
// persist, or discovery re-absorbs it on the next launch.
func TestRemoveRepoIsReassignment(t *testing.T) {
	s := open(t)
	mustUpsertProject(t, s, proj("/w/inno", "inno"))
	mustUpsertRepo(t, s, repo("/w/inno/quickbit", "/w/inno", "inno/quickbit"))

	mustUpsertProject(t, s, proj("/w/inno/quickbit", "quickbit"))
	if err := s.ReassignRepo("/w/inno/quickbit", "/w/inno/quickbit", "quickbit"); err != nil {
		t.Fatal(err)
	}
	// "next launch": discovery re-runs
	mustUpsertProject(t, s, proj("/w/inno", "inno"))
	mustUpsertRepo(t, s, repo("/w/inno/quickbit", "/w/inno", "inno/quickbit"))

	r, ok, _ := s.GetProjectRepo("/w/inno/quickbit")
	if !ok || r.ProjectRoot != "/w/inno/quickbit" {
		t.Fatalf("repo re-absorbed after restart: %+v", r)
	}
}

func TestSetMissingSweep(t *testing.T) {
	s := open(t)
	mustUpsertProject(t, s, proj("/w/inno", "inno"))
	mustUpsertRepo(t, s, repo("/w/inno/ballista", "/w/inno", "inno/ballista"))
	mustUpsertRepo(t, s, repo("/elsewhere/vendored", "/w/inno", "inno/vendored"))

	// the sweep stats every KNOWN row, including out-of-root members
	all, err := s.ListAllProjectRepos()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAllProjectRepos = %+v, %v", all, err)
	}

	if err := s.SetProjectMissing("/w/inno", true, 2000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectRepoMissing("/w/inno/ballista", true); err != nil {
		t.Fatal(err)
	}
	p, _, _ := s.GetProject("/w/inno")
	r, _, _ := s.GetProjectRepo("/w/inno/ballista")
	if !p.Missing || !r.Missing {
		t.Fatalf("missing not set: %+v %+v", p, r)
	}

	// self-clearing: the directory comes back
	if err := s.SetProjectMissing("/w/inno", false, 2001); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectRepoMissing("/w/inno/ballista", false); err != nil {
		t.Fatal(err)
	}
	p, _, _ = s.GetProject("/w/inno")
	r, _, _ = s.GetProjectRepo("/w/inno/ballista")
	if p.Missing || r.Missing {
		t.Fatalf("missing did not self-clear: %+v %+v", p, r)
	}
}

// TestRepointProject covers §7's re-point: a directory rename must leave ONE
// row with hidden preserved, member paths rewritten segment-wise, out-of-root
// members untouched, and any conflict at the target refused whole.
func TestRepointProject(t *testing.T) {
	setup := func(t *testing.T) *Store {
		s := open(t)
		mustUpsertProject(t, s, proj("/w/HappyPay", "HappyPay"))
		mustUpsertRepo(t, s, repo("/w/HappyPay", "/w/HappyPay", "HappyPay/HappyPay"))
		mustUpsertRepo(t, s, repo("/w/HappyPay/HappyPayCoreApi", "/w/HappyPay", "HappyPay/HappyPayCoreApi"))
		mustUpsertRepo(t, s, repo("/elsewhere/HappyCardEngine", "/w/HappyPay", "HappyPay/HappyCardEngine"))
		if err := s.SetProjectHidden("/w/HappyPay", true, 1500); err != nil {
			t.Fatal(err)
		}
		if err := s.SetProjectMissing("/w/HappyPay", true, 1500); err != nil {
			t.Fatal(err)
		}
		return s
	}

	t.Run("rename moves one row and keeps hidden", func(t *testing.T) {
		s := setup(t)
		if err := s.RepointProject("/w/HappyPay", "/w/HappyPay2", 2000); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.GetProject("/w/HappyPay"); ok {
			t.Fatal("old root still present — re-point minted a second row")
		}
		p, ok, _ := s.GetProject("/w/HappyPay2")
		if !ok || !p.Hidden {
			t.Fatalf("re-pointed project = %+v, ok=%v; hidden must survive", p, ok)
		}
		if p.UpdatedAt != 2000 {
			t.Fatalf("UpdatedAt = %d, want 2000", p.UpdatedAt)
		}
		rs, _ := s.ListProjectRepos("/w/HappyPay2")
		if len(rs) != 3 {
			t.Fatalf("members = %+v, want 3", rs)
		}
		got := map[string]bool{}
		for _, r := range rs {
			got[r.Path] = true
		}
		if !got["/w/HappyPay2"] || !got["/w/HappyPay2/HappyPayCoreApi"] {
			t.Fatalf("member paths not rewritten: %+v", rs)
		}
		if !got["/elsewhere/HappyCardEngine"] {
			t.Fatalf("out-of-root member was rewritten: %+v", rs)
		}
	})

	t.Run("sibling prefix is not rewritten", func(t *testing.T) {
		s := setup(t)
		// /w/HappyPayCLM is a raw string prefix match on /w/HappyPay but a
		// different directory (§4) — it belongs to another project and must
		// not be touched.
		mustUpsertProject(t, s, proj("/w/HappyPayCLM", "clm"))
		mustUpsertRepo(t, s, repo("/w/HappyPayCLM", "/w/HappyPayCLM", "HappyPayCLM"))

		if err := s.RepointProject("/w/HappyPay", "/w/HappyPay2", 2000); err != nil {
			t.Fatal(err)
		}
		r, ok, _ := s.GetProjectRepo("/w/HappyPayCLM")
		if !ok || r.ProjectRoot != "/w/HappyPayCLM" {
			t.Fatalf("sibling repo disturbed: %+v ok=%v", r, ok)
		}
	})

	t.Run("occupied target root is refused", func(t *testing.T) {
		s := setup(t)
		mustUpsertProject(t, s, proj("/w/Taken", "Taken"))
		if err := s.RepointProject("/w/HappyPay", "/w/Taken", 2000); !errors.Is(err, ErrRootConflict) {
			t.Fatalf("err = %v, want ErrRootConflict", err)
		}
		if _, ok, _ := s.GetProject("/w/HappyPay"); !ok {
			t.Fatal("refused re-point still moved the project")
		}
	})

	t.Run("member PK conflict at target rolls the whole move back", func(t *testing.T) {
		s := setup(t)
		// another project already owns (as an out-of-root member) the path
		// /w/HappyPay/HappyPayCoreApi would be rewritten onto, so the target
		// root is free but a member PK is not
		mustUpsertProject(t, s, proj("/w/Squatter", "squatter"))
		mustUpsertRepo(t, s, repo("/w/HappyPay2/HappyPayCoreApi", "/w/Squatter", "squatter/core"))

		if err := s.RepointProject("/w/HappyPay", "/w/HappyPay2", 2000); !errors.Is(err, ErrRootConflict) {
			t.Fatalf("err = %v, want ErrRootConflict", err)
		}
		if _, ok, _ := s.GetProject("/w/HappyPay"); !ok {
			t.Fatal("project moved despite the refused member conflict")
		}
		r, ok, _ := s.GetProjectRepo("/w/HappyPay/HappyPayCoreApi")
		if !ok || r.ProjectRoot != "/w/HappyPay" {
			t.Fatalf("members partially moved: %+v ok=%v", r, ok)
		}
	})

	t.Run("new root under old root", func(t *testing.T) {
		s := open(t)
		mustUpsertProject(t, s, proj("/w/a", "a"))
		mustUpsertRepo(t, s, repo("/w/a", "/w/a", "a"))
		mustUpsertRepo(t, s, repo("/w/a/inner", "/w/a", "a/inner"))

		if err := s.RepointProject("/w/a", "/w/a/inner", 2000); err != nil {
			t.Fatalf("re-point into own subdir: %v", err)
		}
		rs, _ := s.ListProjectRepos("/w/a/inner")
		got := map[string]bool{}
		for _, r := range rs {
			got[r.Path] = true
		}
		if len(rs) != 2 || !got["/w/a/inner"] || !got["/w/a/inner/inner"] {
			t.Fatalf("members = %+v", rs)
		}
	})

	t.Run("no-op and reserved root", func(t *testing.T) {
		s := setup(t)
		if err := s.RepointProject("/w/HappyPay", "/w/HappyPay", 2000); err != nil {
			t.Fatalf("self re-point: %v", err)
		}
		if err := s.RepointProject(UngroupedRoot, "/w/x", 2000); !errors.Is(err, ErrReservedRoot) {
			t.Fatalf("err = %v, want ErrReservedRoot", err)
		}
		if err := s.RepointProject("/w/HappyPay", UngroupedRoot, 2000); !errors.Is(err, ErrReservedRoot) {
			t.Fatalf("err = %v, want ErrReservedRoot", err)
		}
		if err := s.RepointProject("/w/ghost", "/w/x", 2000); !errors.Is(err, ErrNoProject) {
			t.Fatalf("err = %v, want ErrNoProject", err)
		}
	})
}
