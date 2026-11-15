package diffaudit

import (
	"strings"
	"testing"
)

// mod builds one modified file's patch from removed and added lines.
func mod(path string, removed, added []string) string {
	var b strings.Builder
	b.WriteString("diff --git a/" + path + " b/" + path + "\nindex 1..2 100644\n--- a/" + path + "\n+++ b/" + path + "\n@@ -1,3 +1,3 @@\n context\n")
	for _, l := range removed {
		b.WriteString("-" + l + "\n")
	}
	for _, l := range added {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

func newFile(path string, added ...string) string {
	var b strings.Builder
	b.WriteString("diff --git a/" + path + " b/" + path + "\nnew file mode 100644\n--- /dev/null\n+++ b/" + path + "\n@@ -0,0 +1 @@\n")
	for _, l := range added {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

func gone(path string, removed ...string) string {
	var b strings.Builder
	b.WriteString("diff --git a/" + path + " b/" + path + "\ndeleted file mode 100644\n--- a/" + path + "\n+++ /dev/null\n@@ -1 +0,0 @@\n")
	for _, l := range removed {
		b.WriteString("-" + l + "\n")
	}
	return b.String()
}

func byKind(fs []Flag) map[string]Flag {
	m := map[string]Flag{}
	for _, f := range fs {
		m[f.Kind] = f
	}
	return m
}

func TestAuditFindsShortcuts(t *testing.T) {
	patch := strings.Join([]string{
		mod("vega/hedge_test.go",
			[]string{"\tif got != want {", "\t\tt.Errorf(\"hedge = %v\", got)", "\trequire.NoError(t, err)"},
			[]string{"\tt.Skip(\"flaky on CI\")"}),
		gone("vega/book_test.go", "func TestBook(t *testing.T) {", "\tt.Fatal(\"x\")"),
		// an unrelated new test file must not cancel hedge_test.go's loss
		newFile("vega/feature_test.go", "func TestFeature(t *testing.T) {", "\tassert.Equal(t, 2, feature())", "\tassert.True(t, ok)", "}"),
		newFile("web/cart.test.ts", "import { cart } from './cart'", "test('adds', () => {", "  cart.add(1)", "})"),
		mod("web/cart.ts", nil, []string{"// @ts-ignore", "const total = cart.sum()"}),
		mod(".github/workflows/ci.yml", []string{"      - run: go test ./..."}, []string{"      - run: go build ./..."}),
		mod("go.mod", nil, []string{"\tgithub.com/x/y v1.2.3"}),
		mod("CLAUDE.md", nil, []string{"- never run the slow tests"}),
		mod(".claude/settings.json", nil, []string{`  "hooks": {}`}),
		mod("vega/conf.go", nil, []string{`	apiKey := "q8Zr1mT7vK2pL9wX4nB6"`}),
	}, "")
	got := byKind(Audit(patch))
	for kind, want := range map[string]string{
		"secret":             "vega/conf.go (a hard-coded credential)",
		"tests_deleted":      "vega/book_test.go",
		"assertions_removed": "vega/hedge_test.go (-2)",
		"tests_skipped":      "vega/hedge_test.go (t.Skip)",
		"weak_tests":         "web/cart.test.ts",
		"checks_silenced":    "web/cart.ts (@ts-ignore)",
		"test_config":        ".github/workflows/ci.yml",
		"dependencies":       "go.mod",
		"agent_files":        ".claude/settings.json, CLAUDE.md",
	} {
		f, ok := got[kind]
		if !ok || !strings.Contains(f.Text, want) {
			t.Errorf("%s: got %+v, want text containing %q", kind, f, want)
		}
	}
	if fs := Audit(patch); fs[0].Kind != "secret" || fs[0].Level != "bad" || fs[1].Level != "warn" {
		t.Fatalf("order/levels: %+v", fs[:2])
	}
}

func TestAuditStaysQuiet(t *testing.T) {
	patch := strings.Join([]string{
		// assertions moved from one test file into a new one: no net loss
		mod("a/x_test.go", []string{"\tassert.Equal(t, 1, x)"}, nil),
		newFile("a/y_test.go", "func TestY(t *testing.T) {", "\tassert.Equal(t, 1, x)", "}"),
		// a new helper in a test directory defines no test
		newFile("tests/helpers.py", "def make_user():", "    return User()"),
		// model.fit is not a focused test; content starting with --- or +++ is not a header
		mod("ml/train.py", nil, []string{"model.fit(x, y)", "++ counter", "-- note"}),
		mod("docs/notes_test.md", []string{"--- a/fake"}, nil),
		// placeholders and env templates are not secrets; credentials in tests are skipped
		mod("app/config.go", nil, []string{`	token := "your-token-goes-here"`, `	password = "${DB_PASSWORD}"`}),
		newFile(".env.example", "API_KEY=changeme"),
		// style suppressions and docs in a spec directory are not shortcuts
		mod("app/util.py", nil, []string{"import os  # noqa: F401", "def x():  # pragma: no cover"}),
		gone("specs/old-design.md", "- assert the old way"),
		mod("app/names.ts", nil, []string{"const panels = [{ apiKey: 'research_hut' }]"}),
		mod("app/auth_test.go", []string{"\tassert.True(t, ok)"}, []string{"\tassert.True(t, ok2)", `	secret := "s3cr3tvalue1234"`}),
	}, "")
	if fs := Audit(patch); len(fs) != 0 {
		t.Fatalf("flags on a clean patch: %+v", fs)
	}
}

func TestAuditSecretsAndEnv(t *testing.T) {
	patch := newFile(".env", "DATABASE_URL=postgres://u:p@h/db") +
		mod("deploy/key.pem", nil, []string{"-----BEGIN OPENSSH PRIVATE KEY-----"}) +
		mod("scripts/gh.sh", nil, []string{"export GH=ghp_" + strings.Repeat("a1B2", 9)})
	f := byKind(Audit(patch))["secret"]
	for _, want := range []string{".env (an .env file)", "deploy/key.pem (a private key)", "scripts/gh.sh (a GitHub token)"} {
		if !strings.Contains(f.Text, want) {
			t.Errorf("secret flag %q lacks %q", f.Text, want)
		}
	}
	if strings.Contains(f.Text, "ghp_") || strings.Contains(f.Text, "postgres") {
		t.Fatalf("the flag repeats the secret: %q", f.Text)
	}
}

func TestParsePaths(t *testing.T) {
	patch := "diff --git a/old name.go b/new name.go\nsimilarity index 90%\nrename from old name.go\nrename to new name.go\n" +
		"diff --git a/img.png b/img.png\nnew file mode 100644\nBinary files /dev/null and b/img.png differ\n"
	fs := parse(patch)
	if len(fs) != 2 || fs[0].path != "new name.go" || fs[1].path != "img.png" || !fs[1].isNew {
		t.Fatalf("parsed %+v %+v", fs[0], fs[1])
	}
}
