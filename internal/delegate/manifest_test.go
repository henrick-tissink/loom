package delegate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/projects"
)

// testTargets is the launch-target set every loader test resolves against: one
// project with two repos, one unrelated project (for §4.4 rule 4's
// cross-project refusal), and a pair of same-named projects (for
// ErrProjectAmbiguous). Built from real projects.Target values rather than a
// hand-written fake so the resolver under test is the one the launcher uses.
func testTargets() []projects.Target {
	return []projects.Target{
		{Kind: projects.TargetRepo, Path: "/p/inno/bankenstein", Label: "bankenstein", ProjectRoot: "/p/inno", ProjectName: "Innostream"},
		{Kind: projects.TargetRepo, Path: "/p/inno/ballista", Label: "ballista", ProjectRoot: "/p/inno", ProjectName: "Innostream"},
		{Kind: projects.TargetRepo, Path: "/p/other/zeta", Label: "zeta", ProjectRoot: "/p/other", ProjectName: "Other"},
		{Kind: projects.TargetRoot, Path: "/p/twinA", Label: "twinA", ProjectRoot: "/p/twinA", ProjectName: "Twin"},
		{Kind: projects.TargetRoot, Path: "/p/twinB", Label: "twinB", ProjectRoot: "/p/twinB", ProjectName: "Twin"},
		{Kind: projects.TargetRepo, Path: "/p/loose", Label: "loose", ProjectRoot: "", ProjectName: ""},
	}
}

func testResolver() Resolver { return NewResolver(testTargets()) }

// writeManifests drops each name→body pair into a fresh dir and returns it.
func writeManifests(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// loadSingle loads one manifest named "<stem>.json" and returns it plus the
// error string LoadAll reported, so a case can assert either half.
func loadSingle(t *testing.T, stem, body string) (Manifest, string) {
	t.Helper()
	dir := writeManifests(t, map[string]string{stem + ".json": body})
	ms, errs := LoadAll(dir, testResolver())
	if len(errs) > 0 {
		return Manifest{}, errs[0].Err
	}
	if len(ms) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(ms))
	}
	return ms[0], ""
}

// okTask is the smallest task that passes every rule; cases mutate around it.
const okTask = `{"id":"schema","repo":"bankenstein","authorization":"edit db/ only",
	"produces":[{"id":"account-schema","kind":"interface","path":"db/0007.sql"}],
	"check":{"cmd":["go","test","./..."]}}`

func wrap(tasks string, extra ...string) string {
	return `{"manifest":1,"name":"m","project":"Innostream",` + strings.Join(extra, "") + `"tasks":[` + tasks + `]}`
}

// TestLoadOne_Validation is §4.4 rule by rule. Every case asserts a LoadError
// carrying a human-readable reason — never a panic, and never a silently
// accepted manifest.
func TestLoadOne_Validation(t *testing.T) {
	cases := []struct {
		name    string
		stem    string
		body    string
		wantErr string // substring; "" means the manifest must load
	}{
		// rule 1
		{"valid baseline", "m", wrap(okTask), ""},
		{"unknown version", "m", `{"manifest":2,"name":"m","project":"Innostream","tasks":[` + okTask + `]}`, "unknown manifest version 2"},
		{"missing version", "m", `{"name":"m","project":"Innostream","tasks":[` + okTask + `]}`, "unknown manifest version 0"},
		{"name not stem", "m", `{"manifest":1,"name":"other","project":"Innostream","tasks":[` + okTask + `]}`, `does not match filename "m"`},
		{"unknown isolation", "m", wrap(okTask, `"isolation":"container",`), `unknown isolation "container"`},
		{"isolation worktree ok", "m", wrap(okTask, `"isolation":"worktree",`), ""},

		// rule 2
		{"missing project", "m", `{"manifest":1,"name":"m","tasks":[` + okTask + `]}`, "project required"},
		{"unknown project", "m", `{"manifest":1,"name":"m","project":"Nope","tasks":[` + okTask + `]}`, "no such project"},
		{"ambiguous project", "m", `{"manifest":1,"name":"m","project":"Twin","tasks":[{"id":"t","repo":"twinA","authorization":"a","check":{"cmd":["go"]}}]}`, "more than one project"},
		{"project by root path", "m", `{"manifest":1,"name":"m","project":"/p/twinA","tasks":[{"id":"t","repo":"twinA","authorization":"a","check":{"cmd":["go"]}}]}`, ""},

		// rule 3
		{"no tasks", "m", `{"manifest":1,"name":"m","project":"Innostream","tasks":[]}`, "at least 1 task"},
		{"empty task id", "m", wrap(`{"id":"","repo":"bankenstein","authorization":"a","check":{"cmd":["go"]}}`), "id required"},
		{"illegal id charset", "m", wrap(`{"id":"Schema_1","repo":"bankenstein","authorization":"a","check":{"cmd":["go"]}}`), "id must match"},
		{"illegal id slash", "m", wrap(`{"id":"a/b","repo":"bankenstein","authorization":"a","check":{"cmd":["go"]}}`), "id must match"},
		{"duplicate task id", "m", wrap(okTask + `,{"id":"schema","repo":"ballista","authorization":"a","check":{"cmd":["go"]}}`), "duplicate task id"},

		// rule 4
		{"missing repo", "m", wrap(`{"id":"t","authorization":"a","check":{"cmd":["go"]}}`), "repo required"},
		{"unknown repo", "m", wrap(`{"id":"t","repo":"nope","authorization":"a","check":{"cmd":["go"]}}`), `unknown repo "nope"`},
		{"repo from another project", "m", wrap(`{"id":"t","repo":"zeta","authorization":"a","check":{"cmd":["go"]}}`), `belongs to project "Other", not "Innostream"`},

		// rule 5
		{"unknown model", "m", wrap(`{"id":"t","repo":"bankenstein","model":"gpt","authorization":"a","check":{"cmd":["go"]}}`), `unknown model "gpt"`},
		{"unknown mode", "m", wrap(`{"id":"t","repo":"bankenstein","mode":"yolo","authorization":"a","check":{"cmd":["go"]}}`), `unknown mode "yolo"`},
		{"unknown default model", "m", wrap(okTask, `"defaults":{"model":"gpt"},`), `defaults: unknown model "gpt"`},
		{"unknown default mode", "m", wrap(okTask, `"defaults":{"mode":"yolo"},`), `defaults: unknown mode "yolo"`},
		{"bypassPermissions is legal", "m", wrap(`{"id":"t","repo":"bankenstein","mode":"bypassPermissions","authorization":"a","check":{"cmd":["go"]}}`), ""},

		// authorization / check required (§4.2)
		{"missing authorization", "m", wrap(`{"id":"t","repo":"bankenstein","check":{"cmd":["go"]}}`), "authorization required"},
		{"blank authorization", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"   ","check":{"cmd":["go"]}}`), "authorization required"},
		{"missing check", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a"}`), "check.cmd required"},
		{"empty check argv", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","check":{"cmd":[]}}`), "check.cmd required"},

		// rule 6
		{"duplicate artifact id", "m", wrap(okTask + `,{"id":"other","repo":"ballista","authorization":"a",
			"produces":[{"id":"account-schema","path":"x.sql"}],"check":{"cmd":["go"]}}`), `duplicate artifact id "account-schema"`},
		{"artifact without id", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","produces":[{"path":"x"}],"check":{"cmd":["go"]}}`), "artifact id required"},
		{"artifact without path", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","produces":[{"id":"a"}],"check":{"cmd":["go"]}}`), "path required"},
		{"unknown artifact kind", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","produces":[{"id":"a","kind":"iface","path":"x"}],"check":{"cmd":["go"]}}`), `unknown kind "iface"`},
		{"needs an undeclared artifact", "m", wrap(okTask + `,{"id":"other","repo":"ballista","authorization":"a","needs":["ghost"],"check":{"cmd":["go"]}}`), `needs "ghost", which no task produces`},
		{"needs its own artifact", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","needs":["a"],
			"produces":[{"id":"a","path":"x"}],"check":{"cmd":["go"]}}`), `needs "a", which it produces itself`},
		{"empty needs entry", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","needs":[""],"produces":[{"id":"a","path":"x"}],"check":{"cmd":["go"]}}`), "empty entry in needs"},
		{"forward needs is legal", "m", wrap(`{"id":"a","repo":"bankenstein","authorization":"a","needs":["late"],"check":{"cmd":["go"]}},
			{"id":"b","repo":"bankenstein","authorization":"a","produces":[{"id":"late","path":"x"}],"check":{"cmd":["go"]}}`), ""},

		// rule 7
		{"artifact path escapes", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","produces":[{"id":"a","path":"../../etc/passwd"}],"check":{"cmd":["go"]}}`), "escapes its repo"},
		{"artifact path absolute", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","produces":[{"id":"a","path":"/etc/passwd"}],"check":{"cmd":["go"]}}`), "is absolute"},
		{"check cwd escapes", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","check":{"cmd":["go"],"cwd":"../ballista"}}`), "escapes its repo"},
		{"check cwd absolute", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","check":{"cmd":["go"],"cwd":"/tmp"}}`), "is absolute"},
		{"inner cwd is fine", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","check":{"cmd":["go"],"cwd":"sub/dir"}}`), ""},

		// timeouts (§4.3)
		{"bad task timeout", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","check":{"cmd":["go"],"timeout":"10min"}}`), `invalid duration "10min"`},
		{"bad default timeout", "m", wrap(okTask, `"defaults":{"check_timeout":"soon"},`), `defaults: check_timeout: invalid duration "soon"`},
		{"negative timeout", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","check":{"cmd":["go"],"timeout":"-5m"}}`), "must be positive"},

		// malformed input must never panic
		{"not JSON", "m", `{"manifest":`, "invalid JSON"},
		{"tasks is an object", "m", `{"manifest":1,"name":"m","project":"Innostream","tasks":{"a":1}}`, "invalid JSON"},
		{"needs is a string", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","needs":"a","check":{"cmd":["go"]}}`), "invalid JSON"},
		{"cmd is a string", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","check":{"cmd":"go test"}}`), "invalid JSON"},
		{"empty file", "m", ``, "invalid JSON"},

		// unknown keys are IGNORED — the on-disk `integration` block (§10) is
		// authored today and unimplemented in 3a.
		{"unknown top-level keys ignored", "m", `{"manifest":1,"name":"m","project":"Innostream","integration":{"per_repo":{"bankenstein":{"cmd":["go","test"]}}},"nonsense":7,"tasks":[` + okTask + `]}`, ""},
		{"unknown task keys ignored", "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","check":{"cmd":["go"]},"future":true}`), ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, gotErr := loadSingle(t, tc.stem, tc.body)
			switch {
			case tc.wantErr == "" && gotErr != "":
				t.Fatalf("want load to succeed, got error: %s", gotErr)
			case tc.wantErr != "" && !strings.Contains(gotErr, tc.wantErr):
				t.Fatalf("want error containing %q, got %q", tc.wantErr, gotErr)
			}
		})
	}
}

// TestLoadAll_MalformedSiblingSurvives is the workflow.LoadAll contract: one bad
// file is reported with its reason and never costs the user the others.
func TestLoadAll_MalformedSiblingSurvives(t *testing.T) {
	dir := writeManifests(t, map[string]string{
		"good.json": strings.Replace(wrap(okTask), `"name":"m"`, `"name":"good"`, 1),
		"bad.json":  `{"manifest":1,`,
		"cyclic.json": `{"manifest":1,"name":"cyclic","project":"Innostream","tasks":[
			{"id":"a","repo":"bankenstein","authorization":"z","needs":["y"],"produces":[{"id":"x","path":"x"}],"check":{"cmd":["go"]}},
			{"id":"b","repo":"bankenstein","authorization":"z","needs":["x"],"produces":[{"id":"y","path":"y"}],"check":{"cmd":["go"]}}]}`,
		"notes.txt": `not a manifest`,
		"sub":       ``, // a plain file named like a dir; still .json-less, still skipped
	})

	ms, errs := LoadAll(dir, testResolver())
	if len(ms) != 1 || ms[0].Name != "good" {
		t.Fatalf("want only the good manifest, got %+v", ms)
	}
	if len(errs) != 2 {
		t.Fatalf("want 2 load errors, got %+v", errs)
	}
	// Errors are sorted by path: bad.json before cyclic.json.
	if !strings.HasSuffix(errs[0].Path, "bad.json") || !strings.Contains(errs[0].Err, "invalid JSON") {
		t.Errorf("errs[0] = %+v", errs[0])
	}
	if !strings.HasSuffix(errs[1].Path, "cyclic.json") || !strings.Contains(errs[1].Err, "dependency cycle") {
		t.Errorf("errs[1] = %+v", errs[1])
	}
}

func TestLoadAll_MissingDirIsEmptyNotAnError(t *testing.T) {
	ms, errs := LoadAll(filepath.Join(t.TempDir(), "nope"), testResolver())
	if ms != nil || errs != nil {
		t.Fatalf("want empty, got %v / %v", ms, errs)
	}
}

func TestLoadAll_SkipsIrregularFiles(t *testing.T) {
	dir := writeManifests(t, map[string]string{"real.json": strings.Replace(wrap(okTask), `"name":"m"`, `"name":"real"`, 1)})
	if err := os.Symlink(filepath.Join(dir, "real.json"), filepath.Join(dir, "link.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	ms, errs := LoadAll(dir, testResolver())
	if len(ms) != 1 || len(errs) != 0 {
		t.Fatalf("want only real.json, got %d manifests %d errors", len(ms), len(errs))
	}
}

// TestManifest_ValidRoundTrip loads the spec §4.2 manifest, integration block
// and all, and asserts every field that survives the round trip — including the
// resolved-at-load ones nothing downstream can recompute.
func TestManifest_ValidRoundTrip(t *testing.T) {
	body := `{
	  "manifest": 1,
	  "name": "atlas-rearchitecture",
	  "project": "Innostream",
	  "defaults": { "model": "sonnet", "mode": "acceptEdits", "check_timeout": "12m" },
	  "repos": {
	    "bankenstein": { "bootstrap": ["go","mod","download"], "seed_files": [".env.test"] },
	    "ballista":    { "bootstrap": ["npm","ci"], "seed_files": [".env.local"] }
	  },
	  "tasks": [
	    { "id":"schema","title":"Extract the account schema","repo":"bankenstein",
	      "paths":["db/migrations/**"],"brief":"do the thing","authorization":"db/migrations only",
	      "needs":[], "produces":[{"id":"account-schema","kind":"interface","path":"db/migrations/0007_account.sql",
	                               "fingerprint":["sha256sum","db/migrations/0007_account.sql"]}],
	      "check":{"cmd":["go","test","./internal/account/..."],"cwd":".","timeout":"10m","env":{"CGO_ENABLED":"0"}} },
	    { "id":"auth-api","repo":"bankenstein","paths":["internal/auth/**"],"brief":"b","authorization":"auth only",
	      "needs":["account-schema"],"produces":[{"id":"auth-openapi","kind":"interface","path":"api/auth.yaml"}],
	      "check":{"cmd":["go","test","./internal/auth/..."]} },
	    { "id":"ballista-client","repo":"ballista","paths":["src/clients/auth/**"],"brief":"c","authorization":"client only",
	      "needs":["auth-openapi"],"produces":[],"check":{"cmd":["go","version"]} }
	  ],
	  "integration": { "per_repo": { "bankenstein": {"cmd":["go","test","./..."]} } }
	}`
	m, gotErr := loadSingle(t, "atlas-rearchitecture", body)
	if gotErr != "" {
		t.Fatalf("load failed: %s", gotErr)
	}

	if m.Version != 1 || m.Name != "atlas-rearchitecture" || m.Project != "Innostream" {
		t.Errorf("header = %+v", m)
	}
	if m.ProjectRoot != "/p/inno" {
		t.Errorf("ProjectRoot = %q, want /p/inno", m.ProjectRoot)
	}
	if m.RepoPaths["bankenstein"] != "/p/inno/bankenstein" || m.RepoPaths["ballista"] != "/p/inno/ballista" {
		t.Errorf("RepoPaths = %v", m.RepoPaths)
	}
	if !strings.HasSuffix(m.Path, "atlas-rearchitecture.json") {
		t.Errorf("Path = %q", m.Path)
	}
	if m.Defaults.Model != "sonnet" || m.Defaults.Mode != "acceptEdits" {
		t.Errorf("Defaults = %+v", m.Defaults)
	}
	if got := m.Repos["bankenstein"].Bootstrap; len(got) != 3 || got[0] != "go" {
		t.Errorf("bootstrap = %v", got)
	}
	if got := m.Repos["ballista"].SeedFiles; len(got) != 1 || got[0] != ".env.local" {
		t.Errorf("seed_files = %v", got)
	}
	if len(m.Tasks) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(m.Tasks))
	}
	// Explicit timeout wins; the manifest default fills the rest.
	if m.Tasks[0].Check.ResolvedTimeout != 10*time.Minute {
		t.Errorf("task 1 timeout = %s, want 10m", m.Tasks[0].Check.ResolvedTimeout)
	}
	if m.Tasks[1].Check.ResolvedTimeout != 12*time.Minute {
		t.Errorf("task 2 timeout = %s, want the 12m default", m.Tasks[1].Check.ResolvedTimeout)
	}
	if m.Tasks[0].Check.Env["CGO_ENABLED"] != "0" {
		t.Errorf("env = %v", m.Tasks[0].Check.Env)
	}
	if got := m.Tasks[0].Produces[0].Fingerprint; len(got) != 2 {
		t.Errorf("fingerprint = %v", got)
	}

	// The derived graph is the spec's chain, and it is acyclic.
	g := BuildGraph(m)
	wantEdges := []Edge{
		{From: "schema", To: "auth-api", Artifact: "account-schema"},
		{From: "auth-api", To: "ballista-client", Artifact: "auth-openapi"},
	}
	if fmt.Sprint(g.Edges) != fmt.Sprint(wantEdges) {
		t.Errorf("edges = %v, want %v", g.Edges, wantEdges)
	}
	if ce := DetectCycle(g, m.Name); ce != nil {
		t.Errorf("unexpected cycle: %v", ce)
	}
}

// TestTimeoutCapped: a manifest may not buy itself an unbounded check. The cap
// is applied and REPORTED — a silently shortened timeout is a check that fails
// for a reason the author cannot see.
func TestTimeoutCapped(t *testing.T) {
	m, gotErr := loadSingle(t, "m", wrap(`{"id":"t","repo":"bankenstein","authorization":"a","check":{"cmd":["go"],"timeout":"90m"}}`))
	if gotErr != "" {
		t.Fatalf("load failed: %s", gotErr)
	}
	if m.Tasks[0].Check.ResolvedTimeout != CheckTimeoutMax {
		t.Errorf("timeout = %s, want cap %s", m.Tasks[0].Check.ResolvedTimeout, CheckTimeoutMax)
	}
	if !hasWarning(m, "capped") {
		t.Errorf("cap was applied silently; warnings = %+v", m.Warnings)
	}
}

func hasWarning(m Manifest, substr string) bool {
	for _, w := range m.Warnings {
		if strings.Contains(w.Text, substr) {
			return true
		}
	}
	return false
}

// TestWarnings_AreWarningsNotErrors is §4.4 rule 10: every one of these
// manifests LOADS, and each carries the finding a human should glance at.
func TestWarnings_AreWarningsNotErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"overlapping paths in the same repo",
			wrap(`{"id":"a","repo":"bankenstein","paths":["internal/auth/**"],"authorization":"z","check":{"cmd":["go"]}},
			      {"id":"b","repo":"bankenstein","paths":["internal/auth/token.go"],"authorization":"z","check":{"cmd":["go"]}}`),
			"overlaps task",
		},
		{
			"leaf that produces nothing",
			wrap(`{"id":"a","repo":"bankenstein","authorization":"z","check":{"cmd":["go"]}}`),
			"produces no artifacts",
		},
		{
			"check command not on PATH",
			wrap(`{"id":"a","repo":"bankenstein","authorization":"z","produces":[{"id":"x","path":"x"}],"check":{"cmd":["loom-no-such-binary-xyz"]}}`),
			"is not on PATH",
		},
		{
			"bypassPermissions is flagged with the task id",
			wrap(`{"id":"a","repo":"bankenstein","mode":"bypassPermissions","authorization":"z","produces":[{"id":"x","path":"x"}],"check":{"cmd":["go"]}}`),
			"bypassPermissions",
		},
		{
			"repos entry no task uses",
			wrap(okTask, `"repos":{"ballista":{"bootstrap":["npm","ci"]}},`),
			`repos["ballista"] is declared but no task uses it`,
		},
		{
			"repos entry outside the project",
			wrap(okTask, `"repos":{"zeta":{"bootstrap":["npm","ci"]}},`),
			"is not a repo of project",
		},
		{
			"escaping seed file",
			wrap(okTask, `"repos":{"bankenstein":{"seed_files":["../../.ssh/id_rsa"]}},`),
			"escapes the repo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, gotErr := loadSingle(t, "m", tc.body)
			if gotErr != "" {
				t.Fatalf("want a warning, got a load error: %s", gotErr)
			}
			if !hasWarning(m, tc.want) {
				t.Fatalf("want warning containing %q, got %+v", tc.want, m.Warnings)
			}
		})
	}
}

func TestWarnings_NoFalsePositives(t *testing.T) {
	// Different repos cannot collide (the worktree is what separates them), and
	// sibling directories that merely share a prefix are not an overlap.
	body := wrap(`{"id":"a","repo":"bankenstein","paths":["internal/account/**"],"authorization":"z","produces":[{"id":"p","path":"a"}],"check":{"cmd":["go"]}},
	              {"id":"b","repo":"ballista","paths":["internal/account/**"],"authorization":"z","produces":[{"id":"q","path":"b"}],"check":{"cmd":["go"]}},
	              {"id":"c","repo":"bankenstein","paths":["internal/accounts/**"],"authorization":"z","produces":[{"id":"r","path":"c"}],"check":{"cmd":["go"]}}`)
	m, gotErr := loadSingle(t, "m", body)
	if gotErr != "" {
		t.Fatalf("load failed: %s", gotErr)
	}
	if hasWarning(m, "overlaps task") {
		t.Fatalf("unexpected overlap warning: %+v", m.Warnings)
	}
}

func TestGlobsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"db/migrations/**", "db/migrations/0007.sql", true},
		{"internal/auth/**", "internal/auth/**", true},
		{"internal/account/**", "internal/accounts/**", false},
		{"internal/auth/x.go", "internal/auth/y.go", false}, // two literal files are two files
		{"src/a/**", "src/b/**", false},
		{"**/*.go", "anything", true}, // a leading wildcard matches everywhere
		{"./db/x", "db/x", true},
	}
	for _, tc := range cases {
		if got := globsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("globsOverlap(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := globsOverlap(tc.b, tc.a); got != tc.want {
			t.Errorf("globsOverlap(%q,%q) [reversed] = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestResolveInside(t *testing.T) {
	base := filepath.Join("/repo", "root")
	cases := []struct {
		rel     string
		want    string
		wantErr bool
	}{
		{"", base, false},
		{".", base, false},
		{"a/b.go", filepath.Join(base, "a/b.go"), false},
		{"./a/../a/b.go", filepath.Join(base, "a/b.go"), false},
		{"a/../..", "", true},
		{"..", "", true},
		{"../sibling", "", true},
		{"a/../../root-evil", "", true},
		{"/etc/passwd", "", true},
	}
	for _, tc := range cases {
		got, err := ResolveInside(base, tc.rel)
		if tc.wantErr {
			if !errors.Is(err, ErrEscapesRepo) {
				t.Errorf("ResolveInside(%q) err = %v, want ErrEscapesRepo", tc.rel, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ResolveInside(%q) = %q, %v; want %q", tc.rel, got, err, tc.want)
		}
	}
}

// ---------------------------------------------------------------- cycles

// chain builds a graph of n tasks t0→t1→…→t(n-1); closed==true adds the
// back edge that makes it a cycle.
func chain(n int, closed bool) Graph {
	var m Manifest
	for i := range n {
		t := Task{ID: fmt.Sprintf("t%d", i), Produces: []Artifact{{ID: fmt.Sprintf("a%d", i)}}}
		if i > 0 {
			t.Needs = []string{fmt.Sprintf("a%d", i-1)}
		} else if closed {
			t.Needs = []string{fmt.Sprintf("a%d", n-1)}
		}
		m.Tasks = append(m.Tasks, t)
	}
	return BuildGraph(m)
}

// graphOf builds a graph from a "task → the tasks it depends on" adjacency.
// Every task produces exactly one artifact named after itself, so an edge's
// artifact label in these tests reads as the PRODUCER's id — the distinctly
// named artifacts live in TestLoad_RejectsCycle, which asserts §4.5's exact
// message.
func graphOf(order []string, needs map[string][]string) Graph {
	var m Manifest
	for _, id := range order {
		m.Tasks = append(m.Tasks, Task{ID: id, Needs: needs[id], Produces: []Artifact{{ID: id}}})
	}
	return BuildGraph(m)
}

// TestDetectCycle is §4.5, BINDING. A cycle is a silent deadlock that reads as
// healthy progress, so each case asserts not just detection but the NAMED path:
// an error that says "there is a cycle somewhere" is not actionable.
func TestDetectCycle(t *testing.T) {
	cases := []struct {
		name    string
		g       Graph
		wantMsg string // "" means: must be acyclic
	}{
		{
			"self loop (length 1)",
			graphOf([]string{"a"}, map[string][]string{"a": {"a"}}),
			"a → (a) → a",
		},
		{
			"two-cycle",
			graphOf([]string{"a", "b"}, map[string][]string{"a": {"b"}, "b": {"a"}}),
			"a → (a) → b → (b) → a",
		},
		{
			"three-cycle",
			graphOf([]string{"a", "b", "c"}, map[string][]string{"a": {"c"}, "b": {"a"}, "c": {"b"}}),
			"a → (a) → b → (b) → c → (c) → a",
		},
		{
			"cycle reachable only from a later root",
			graphOf([]string{"root", "x", "y"}, map[string][]string{"x": {"y"}, "y": {"x"}}),
			"x → (x) → y → (y) → x",
		},
		{
			"cycle hanging off an acyclic prefix",
			graphOf([]string{"start", "a", "b"}, map[string][]string{"a": {"start", "b"}, "b": {"a"}}),
			"a → (a) → b → (b) → a",
		},
		{
			"diamond is NOT a cycle",
			graphOf([]string{"top", "l", "r", "bot"}, map[string][]string{"l": {"top"}, "r": {"top"}, "bot": {"l", "r"}}),
			"",
		},
		{"empty graph", Graph{}, ""},
		{"long chain is acyclic", chain(200, false), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ce := DetectCycle(tc.g, "atlas")
			if tc.wantMsg == "" {
				if ce != nil {
					t.Fatalf("unexpected cycle: %v", ce)
				}
				return
			}
			if ce == nil {
				t.Fatal("want a cycle, got nil")
			}
			msg := ce.Error()
			if !strings.HasPrefix(msg, `manifest "atlas": dependency cycle:`) {
				t.Errorf("message header wrong: %q", msg)
			}
			if !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("message = %q, want it to contain %q", msg, tc.wantMsg)
			}
			// The path must actually be a cycle: consecutive edges join, and the
			// last one closes back on the first node.
			for i := 1; i < len(ce.Path); i++ {
				if ce.Path[i-1].To != ce.Path[i].From {
					t.Fatalf("path is not connected at %d: %+v", i, ce.Path)
				}
			}
			if ce.Path[len(ce.Path)-1].To != ce.Path[0].From {
				t.Fatalf("path does not close: %+v", ce.Path)
			}
		})
	}
}

// TestDetectCycle_LongCycleIsIterative is the stack-growth guard: 200 nodes deep
// is enough to catch a recursive rewrite in review, and the whole cycle must be
// reported, not a truncated fragment.
func TestDetectCycle_LongCycleIsIterative(t *testing.T) {
	ce := DetectCycle(chain(200, true), "long")
	if ce == nil {
		t.Fatal("want a cycle")
	}
	if len(ce.Path) != 200 {
		t.Fatalf("want all 200 edges in the reported path, got %d", len(ce.Path))
	}
	if !strings.Contains(ce.Error(), "t0 → (a0) → t1") {
		t.Errorf("message = %q", ce.Error())
	}
}

// TestDetectCycle_Deterministic: a validation error that names a different cycle
// on every run is a validation error nobody believes.
func TestDetectCycle_Deterministic(t *testing.T) {
	g := graphOf([]string{"a", "b", "c", "d"}, map[string][]string{
		"a": {"b"}, "b": {"a"}, // cycle 1
		"c": {"d"}, "d": {"c"}, // cycle 2
	})
	first := DetectCycle(g, "m").Error()
	for range 50 {
		if got := DetectCycle(g, "m").Error(); got != first {
			t.Fatalf("nondeterministic: %q then %q", first, got)
		}
	}
}

// TestLoad_RejectsCycle is the load-time half of the BINDING rule: the cycle
// never becomes a Manifest at all.
func TestLoad_RejectsCycle(t *testing.T) {
	body := `{"manifest":1,"name":"m","project":"Innostream","tasks":[
	  {"id":"auth-api","repo":"bankenstein","authorization":"z","needs":["account-schema"],
	   "produces":[{"id":"auth-openapi","path":"api/auth.yaml"}],"check":{"cmd":["go"]}},
	  {"id":"ballista-client","repo":"ballista","authorization":"z","needs":["auth-openapi"],
	   "produces":[{"id":"ballista-types","path":"types.ts"}],"check":{"cmd":["go"]}},
	  {"id":"schema","repo":"bankenstein","authorization":"z","needs":["ballista-types"],
	   "produces":[{"id":"account-schema","path":"db/0007.sql"}],"check":{"cmd":["go"]}}]}`
	_, gotErr := loadSingle(t, "m", body)
	want := "auth-api → (auth-openapi) → ballista-client → (ballista-types) → schema → (account-schema) → auth-api"
	if !strings.Contains(gotErr, want) {
		t.Fatalf("error = %q, want it to name the cycle %q", gotErr, want)
	}
}

// ---------------------------------------------------------------- graph

func TestBuildGraph(t *testing.T) {
	m := Manifest{Tasks: []Task{
		{ID: "b", Needs: []string{"pa", "pa"}, Produces: []Artifact{{ID: "pb"}}}, // duplicate need
		{ID: "a", Produces: []Artifact{{ID: "pa"}}},
		{ID: "c", Needs: []string{"pb", "ghost"}}, // ghost is unproduced
	}}
	g := BuildGraph(m)
	if fmt.Sprint(g.TaskIDs) != "[b a c]" {
		t.Errorf("TaskIDs = %v, want manifest order", g.TaskIDs)
	}
	if g.Producer["pa"] != "a" || g.Producer["pb"] != "b" {
		t.Errorf("Producer = %v", g.Producer)
	}
	want := []Edge{{From: "a", To: "b", Artifact: "pa"}, {From: "b", To: "c", Artifact: "pb"}}
	if fmt.Sprint(g.Edges) != fmt.Sprint(want) {
		t.Errorf("Edges = %v, want %v (deduped, no edge for the unproduced artifact)", g.Edges, want)
	}
}

// ---------------------------------------------------------------- Ready

// TestReady is §9.1: BOTH halves are required, and each is negative-tested on
// its own. Producer-verified without a published artifact means the check did
// not cover the handoff; published without verification means an untested one.
func TestReady(t *testing.T) {
	g := graphOf([]string{"a", "b"}, map[string][]string{"b": {"a"}})

	cases := []struct {
		name      string
		states    map[string]TaskState
		published map[string]bool
		want      string
	}{
		{"nothing done yet: only the task with no needs", map[string]TaskState{}, map[string]bool{}, "[a]"},
		{"producer verified and artifact published", map[string]TaskState{"a": StateVerified}, map[string]bool{"a": true}, "[b]"},
		{"producer verified, artifact NOT published", map[string]TaskState{"a": StateVerified}, map[string]bool{}, "[]"},
		{"artifact published, producer NOT verified", map[string]TaskState{"a": StateRunning}, map[string]bool{"a": true}, "[]"},
		{"producer merged counts as verified", map[string]TaskState{"a": StateMerged}, map[string]bool{"a": true}, "[b]"},
		{"producer failed", map[string]TaskState{"a": StateFailed}, map[string]bool{"a": true}, "[]"},
		{"consumer already spawned is not re-proposed", map[string]TaskState{"a": StateVerified, "b": StateRunning}, map[string]bool{"a": true}, "[]"},
		{"consumer explicitly pending", map[string]TaskState{"a": StateVerified, "b": StatePending}, map[string]bool{"a": true}, "[b]"},
		{"consumer already ready stays ready", map[string]TaskState{"a": StateVerified, "b": StateReady}, map[string]bool{"a": true}, "[b]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Ready(g, tc.states, tc.published)
			if fmt.Sprint(got) != tc.want {
				t.Fatalf("Ready = %v, want %s", got, tc.want)
			}
		})
	}
}

func TestReady_NoNeedsIsReadyImmediately(t *testing.T) {
	g := graphOf([]string{"solo"}, nil)
	if got := Ready(g, map[string]TaskState{}, map[string]bool{}); fmt.Sprint(got) != "[solo]" {
		t.Fatalf("Ready = %v", got)
	}
}

func TestReady_IsPure(t *testing.T) {
	g := graphOf([]string{"a", "b"}, map[string][]string{"b": {"a"}})
	states := map[string]TaskState{"a": StateVerified}
	published := map[string]bool{"a": true}
	first := fmt.Sprint(Ready(g, states, published))
	second := fmt.Sprint(Ready(g, states, published))
	if first != second {
		t.Fatalf("not pure: %s then %s", first, second)
	}
	if len(states) != 1 || len(published) != 1 || len(g.TaskIDs) != 2 {
		t.Fatal("Ready mutated its inputs")
	}
}

// TestReady_UnproducedNeedIsNeverReady: BuildGraph is total, so an unsatisfiable
// dependency reaches Ready as a need with no producer. It must block forever
// rather than be treated as satisfied — the load-time error is the fix, and a
// permissive scheduler would hide it.
func TestReady_UnproducedNeedIsNeverReady(t *testing.T) {
	g := BuildGraph(Manifest{Tasks: []Task{{ID: "x", Needs: []string{"ghost"}}}})
	if got := Ready(g, map[string]TaskState{}, map[string]bool{"ghost": true}); len(got) != 0 {
		t.Fatalf("Ready = %v, want none", got)
	}
}

// ---------------------------------------------------------------- resolver

func TestNewResolver(t *testing.T) {
	r := testResolver()

	sc, err := r.ResolveProject("Innostream")
	if err != nil {
		t.Fatalf("Innostream: %v", err)
	}
	if sc.Root != "/p/inno" || sc.Repos["ballista"] != "/p/inno/ballista" {
		t.Errorf("scope = %+v", sc)
	}
	if _, err := r.ResolveProject("Nope"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("Nope: err = %v, want ErrProjectNotFound", err)
	}
	if _, err := r.ResolveProject("Twin"); !errors.Is(err, ErrProjectAmbiguous) {
		t.Errorf("Twin: err = %v, want ErrProjectAmbiguous", err)
	}
	// A target with no project root belongs to no project and must not become
	// one: it is Ungrouped, and §3's containment rule has nothing to contain.
	if _, err := r.ResolveProject(""); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("empty name: err = %v", err)
	}
	if _, ok := r.(repoOwner).OwnerOfRepo("loose"); ok {
		t.Error("an ungrouped target became a repo of some project")
	}
	owner, ok := r.(repoOwner).OwnerOfRepo("zeta")
	if !ok || owner.Name != "Other" {
		t.Errorf("OwnerOfRepo(zeta) = %+v, %v", owner, ok)
	}
}

func TestManifestDir(t *testing.T) {
	if got, want := ManifestDir("/p/inno"), filepath.Join("/p/inno", ".loom", "manifests"); got != want {
		t.Errorf("ManifestDir = %q, want %q", got, want)
	}
}
