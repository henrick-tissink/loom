package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMustReadInRepo(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write("internal/auth/token.go", "package auth\n")
	write("README.md", "x\n")
	run("add", ".")
	run("commit", "-qm", "base")
	write("internal/auth/token.go", "package auth\n// changed\n")
	write("README.md", "y\n")
	write(".github/workflows/ci.yml", "on: push\n") // new; untracked outside the session dir: git lists none
	write("internal/migrations/001.sql", "x\n")     // new, in the session's directory

	// defaults; the session runs in a subdirectory: rules load from the root
	sub := filepath.Join(dir, "internal")
	a := auditDir(sub, "x", "", 0)
	var got []string
	for _, m := range a.must {
		got = append(got, m.File+"="+m.Why+"@"+m.Source)
	}
	if strings.Join(got, " ") != "internal/auth/token.go=auth@defaults migrations/001.sql=migrations@defaults" {
		t.Fatalf("must = %v", got)
	}
	r := buildReview(reviewIn{dto: SessionDTO{Name: "loom-1"}, audits: []repoAudit{a}})
	if len(r.MustRead) != 2 || !hasRisk(r, "bad", "2 files a person must read changed: internal/auth/token.go (auth), migrations/001.sql (migrations). Passing tests don't prove them") {
		t.Fatalf("review = %+v %+v", r.MustRead, r.Risks)
	}
	for _, m := range a.must { // opened from the repo root (tracked) or the session dir (new)
		if want := map[string]string{"internal/auth/token.go": filepath.Join(realPath(dir), "internal/auth/token.go"), "migrations/001.sql": filepath.Join(sub, "migrations/001.sql")}[m.File]; m.Path != want {
			t.Errorf("%s: path %s, want %s", m.File, m.Path, want)
		}
	}
	if r.Risks[0].Text[:7] != "2 files" {
		t.Fatalf("must-read is not the first risk: %+v", r.Risks)
	}

	// from the root, the new CI file is listed and matched too
	if a := auditDir(dir, "x", "", 0); len(a.must) != 3 {
		t.Fatalf("root must = %+v", a.must)
	}

	// the repo's own file replaces the defaults
	write(".loom/must-read", "README.md   docs the team signs off\n")
	ms := mustReadIn(dir, "x", []string{"README.md", "internal/auth/token.go"}, nil)
	if len(ms) != 1 || ms[0].File != "README.md" || ms[0].Why != "docs the team signs off" || ms[0].Source != ".loom/must-read" {
		t.Fatalf("own rules = %+v", ms)
	}
	if m := mustReadMap(ms); m[dir]["README.md"] != "docs the team signs off" {
		t.Fatalf("map = %+v", m)
	}
	if r := buildReview(reviewIn{dto: SessionDTO{Name: "loom-1"}}); r.MustRead == nil || len(r.MustRead) != 0 || hasRisk(r, "bad", "must read") {
		t.Fatalf("no audits: %+v", r)
	}
}
