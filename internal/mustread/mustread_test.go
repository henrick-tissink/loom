package mustread

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	r := Defaults()
	for path, want := range map[string]string{
		"internal/auth/token.go":         "auth",
		"web/src/OAuthCallback.tsx":      "auth",
		"api/login_handler.go":           "auth",
		"svc/payments/charge.go":         "payments",
		"pkg/rbac/roles.go":              "permissions",
		"db/migrations/0042_users.up.go": "migrations",
		"schema/0001_init.sql":           "migrations",
		".github/workflows/ci.yml":       "CI",
		".gitlab-ci.yml":                 "CI",
		"cmd/loom-gui/auth.go":           "auth",
	} {
		got, ok := r.Match(path)
		if !ok || got.Why != want {
			t.Errorf("%s: got %+v %v, want %s", path, got, ok, want)
		}
	}
	for _, path := range []string{"cmd/loom-gui/author.go", "README.md", "internal/session/store.go", "docs/github/workflows.md"} {
		if got, ok := r.Match(path); ok {
			t.Errorf("%s matched %+v", path, got)
		}
	}
}

func TestParseAndAnchoring(t *testing.T) {
	r := Parse([]byte(`# who must read what
src/core/**/money.go	ledger maths
deploy/                 infra changes
*.tf
/Makefile build
`))
	if r.Source != File || len(r.Rules) != 4 {
		t.Fatalf("rules = %+v", r)
	}
	cases := map[string]string{
		"src/core/money.go":     "ledger maths",
		"src/core/a/b/money.go": "ledger maths",
		"deploy/k8s/app.yaml":   "infra changes",
		"svc/deploy/x.sh":       "infra changes", // no inner slash: any depth
		"infra/main.tf":         "must read",
		"Makefile":              "build",
	}
	for p, want := range cases {
		got, ok := r.Match(p)
		if !ok || got.Why != want {
			t.Errorf("%s: got %+v %v, want %q", p, got, ok, want)
		}
	}
	for _, p := range []string{"lib/src/core/money.go", "sub/Makefile", "deploy", "src/core/money.go.bak"} {
		if got, ok := r.Match(p); ok {
			t.Errorf("%s matched %+v", p, got)
		}
	}
}

func TestLoad(t *testing.T) {
	root := t.TempDir()
	r, err := Load(root)
	if err != nil || r.Source != "defaults" {
		t.Fatalf("no file: %+v %v", r, err)
	}
	os.MkdirAll(filepath.Join(root, ".loom"), 0o755)
	os.WriteFile(filepath.Join(root, File), []byte("only/this.go\n"), 0o644)
	r, err = Load(root)
	if err != nil || r.Source != File || len(r.Rules) != 1 {
		t.Fatalf("file: %+v %v", r, err)
	}
	// the file replaces the defaults: an empty file means nothing must be read
	if _, ok := r.Match("internal/auth/x.go"); ok {
		t.Fatal("defaults applied beside the file")
	}
}
