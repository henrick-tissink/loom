package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fake answers each command by its joined argv prefix; anything unlisted
// fails, like a git with no origin/HEAD.
func fake(answers map[string]string) Runner {
	return func(_ context.Context, _, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		for k, v := range answers {
			if strings.HasPrefix(cmd, k) {
				if strings.HasPrefix(v, "ERR ") {
					return nil, errors.New(v[4:])
				}
				return []byte(v), nil
			}
		}
		return nil, errors.New("unexpected: " + cmd)
	}
}

func TestDetect(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/a/b.git":       "github",
		"git@gitlab.com:a/b.git":           "gitlab",
		"http://source.example:1500/scm/x": "",
	} {
		if got := Detect(url); got != want {
			t.Errorf("Detect(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestLookupGitHubChecks(t *testing.T) {
	gh := func(rollup string) Runner {
		return fake(map[string]string{
			"git remote get-url origin": "https://github.com/a/b.git\n",
			"git symbolic-ref":          "origin/main\n",
			"gh pr list --head feat":    `[{"number":7,"title":"t","state":"OPEN","isDraft":false,"url":"https://github.com/a/b/pull/7","reviewDecision":"CHANGES_REQUESTED","statusCheckRollup":` + rollup + `}]`,
		})
	}
	for rollup, want := range map[string]string{
		`[]`: "",
		`[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"StatusContext","state":"SUCCESS"}]`:                 "passing",
		`[{"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":""},{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SKIPPED"}]`: "running",
		`[{"__typename":"CheckRun","status":"IN_PROGRESS"},{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE"}]`:                 "failing",
		`[{"__typename":"StatusContext","state":"ERROR"}]`: "failing",
	} {
		pr, err := Lookup(context.Background(), gh(rollup), "/r", "feat")
		if err != nil || pr == nil {
			t.Fatalf("%s: %v %v", rollup, pr, err)
		}
		if pr.Checks != want || pr.Number != 7 || pr.State != "open" || pr.Review != "changes_requested" {
			t.Errorf("%s: got %+v, want checks %q", rollup, pr, want)
		}
	}
}

func TestLookupSkips(t *testing.T) {
	ctx := context.Background()
	// the default branch is a target, never a source: no gh call at all
	run := fake(map[string]string{"git remote get-url origin": "https://github.com/a/b\n", "git symbolic-ref": "origin/trunk\n"})
	if pr, err := Lookup(ctx, run, "/r", "trunk"); pr != nil || err != nil {
		t.Fatalf("default branch: %v %v", pr, err)
	}
	// origin/HEAD never fetched: main and master are taken as default
	run = fake(map[string]string{"git remote get-url origin": "https://github.com/a/b\n"})
	if pr, err := Lookup(ctx, run, "/r", "master"); pr != nil || err != nil {
		t.Fatalf("master without origin/HEAD: %v %v", pr, err)
	}
	// a host loom cannot ask, and a repo with no origin, are not errors
	run = fake(map[string]string{"git remote get-url origin": "http://source.example/scm/x\n"})
	if pr, err := Lookup(ctx, run, "/r", "feat"); pr != nil || err != nil {
		t.Fatalf("other host: %v %v", pr, err)
	}
	if pr, err := Lookup(ctx, fake(nil), "/r", "feat"); pr != nil || err != nil {
		t.Fatalf("no origin: %v %v", pr, err)
	}
	// no PR for the branch
	run = fake(map[string]string{"git remote get-url origin": "https://github.com/a/b\n", "git symbolic-ref": "origin/main\n", "gh pr list": "[]"})
	if pr, err := Lookup(ctx, run, "/r", "feat"); pr != nil || err != nil {
		t.Fatalf("no PR: %v %v", pr, err)
	}
	// a CLI failure is reported, not swallowed into "no PR"
	run = fake(map[string]string{"git remote get-url origin": "https://github.com/a/b\n", "git symbolic-ref": "origin/main\n", "gh pr list": "ERR gh: not logged in"})
	if pr, err := Lookup(ctx, run, "/r", "feat"); err == nil || pr == nil || pr.Kind != "github" {
		t.Fatalf("gh failure must surface, naming the forge: %v %v", pr, err)
	}
}

func TestLookupGitLab(t *testing.T) {
	run := fake(map[string]string{
		"git remote get-url origin":         "git@gitlab.com:g/p.git\n",
		"git symbolic-ref":                  "origin/main\n",
		"glab mr list --source-branch feat": `[{"iid":55,"title":"t","state":"opened","draft":true,"web_url":"https://gitlab.com/g/p/-/merge_requests/55"}]`,
		"glab mr view 55":                   `{"iid":55,"title":"t","state":"opened","draft":true,"web_url":"https://gitlab.com/g/p/-/merge_requests/55","detailed_merge_status":"not_approved","head_pipeline":{"status":"failed"}}`,
	})
	pr, err := Lookup(context.Background(), run, "/r", "feat")
	if err != nil || pr == nil {
		t.Fatalf("%v %v", pr, err)
	}
	want := PR{Kind: "gitlab", Number: 55, Title: "t", State: "draft", URL: "https://gitlab.com/g/p/-/merge_requests/55", Checks: "failing", Review: "review_required"}
	if *pr != want {
		t.Fatalf("got %+v, want %+v", *pr, want)
	}
}
