package secondopinion

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The two shapes /code-review printed in a headless run (2.1.325), verbatim.
const lowOut = "a.go:6 — Loop starts at i := 1 and runs to i <= len(xs), so xs[0] is skipped and xs[len(xs)] is out of range.\n\na.go:8 — `s / len(xs)` divides by zero for an empty slice."

const mediumOut = "4 findings, most severe first:\n\n1. `a.go:6`: Loop indexes `xs[i]` with `i <= len(xs)`, so the last iteration reads one past the end.\n\n   Failure scenario: avg([]int{1,2,3}) reaches i == 3 and `xs[3]` panics with index out of range. Every non-empty input panics.\n\n2. `a.go:5`: The loop starts at i := 1, so xs[0] is never added to the sum.\n\n   Failure scenario: Even if the upper bound were fixed, avg([]int{10, 20, 30}) would sum only 20+30.\n   A single-element slice would skip the loop and return 0."

func TestParseLow(t *testing.T) {
	r := Parse(lowOut)
	want := []Finding{
		{File: "a.go", Line: 6, Summary: "Loop starts at i := 1 and runs to i <= len(xs), so xs[0] is skipped and xs[len(xs)] is out of range."},
		{File: "a.go", Line: 8, Summary: "`s / len(xs)` divides by zero for an empty slice."},
	}
	if r.None || !reflect.DeepEqual(r.Findings, want) {
		t.Fatalf("Parse(low) = %+v", r)
	}
}

func TestParseMedium(t *testing.T) {
	r := Parse(mediumOut)
	if len(r.Findings) != 2 {
		t.Fatalf("Parse(medium) = %+v", r.Findings)
	}
	f := r.Findings[0]
	if f.File != "a.go" || f.Line != 6 || !strings.HasPrefix(f.Summary, "Loop indexes") || !strings.HasPrefix(f.Detail, "avg([]int{1,2,3})") {
		t.Errorf("first = %+v", f)
	}
	if d := r.Findings[1].Detail; !strings.HasSuffix(d, "would sum only 20+30. A single-element slice would skip the loop and return 0.") {
		t.Errorf("second detail = %q", d)
	}
}

func TestParseShapes(t *testing.T) {
	for _, c := range []struct {
		name, text string
		none       bool
		files      []string
	}{
		{"none", "(none)", true, nil},
		{"empty array", "[]", true, nil},
		{"none sentence", "No findings: the diff only renames a variable.", true, nil},
		{"json", `[{"file":"x/y.ts","line":3,"summary":"s","failure_scenario":"f"}]`, false, []string{"x/y.ts"}},
		{"fenced json", "```json\n[{\"file\":\"z.py\",\"line\":1,\"summary\":\"s\"}]\n```", false, []string{"z.py"}},
		{"fenced json after a sentence", "Here are the findings:\n\n```json\n[\n  {\"file\":\"avg.go\",\"line\":5,\"summary\":\"s\",\"failure_scenario\":\"f\"}\n]\n```\n", false, []string{"avg.go"}},
		{"fenced empty array after a sentence", "Nothing survived.\n```json\n[]\n```", true, nil},
		{"bare array after a sentence", "Findings:\n[{\"file\":\"q.go\",\"line\":2,\"summary\":\"s\"}]", false, []string{"q.go"}},
		{"bold, range, dash", "- **cmd/app.go:10-14** - wrong guard\n* Makefile:3: target never runs", false, []string{"cmd/app.go", "Makefile"}},
		{"prose closes a finding", "a.go:1 — bad\n\nOverall the change is small.", false, []string{"a.go"}},
		{"quoted json is not the findings", "1. `cfg/x.json:3`: bad array\n   ```json\n   [{\"name\":\"a\"}]\n   ```", false, []string{"cfg/x.json"}},
		{"indented path stays detail", "1. `a.go:6`: wrong bound\n   Failure scenario: it panics, and\n   internal/foo.go:40 - also calls it", false, []string{"a.go"}},
		{"unknown shape kept", "The diff looks fine but see the README.", false, nil},
		{"not a path", "Note:3 things to check", false, nil},
	} {
		r := Parse(c.text)
		var files []string
		for _, f := range r.Findings {
			files = append(files, f.File)
		}
		if r.None != c.none || !reflect.DeepEqual(files, c.files) || r.Text != strings.TrimSpace(c.text) {
			t.Errorf("%s: Parse = %+v", c.name, r)
		}
		if c.name == "prose closes a finding" && r.Findings[0].Detail != "" {
			t.Errorf("prose joined the finding: %q", r.Findings[0].Detail)
		}
	}
}

func TestReadOutput(t *testing.T) {
	r, err := ReadOutput([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"a.go:6 — bad","total_cost_usd":0.07,"duration_ms":6470}`))
	if err != nil || len(r.Findings) != 1 || r.CostUSD != 0.07 || r.DurationMS != 6470 {
		t.Fatalf("ReadOutput = %+v, %v", r, err)
	}
	if _, err := ReadOutput([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":""}`)); err == nil || !strings.Contains(err.Error(), "error_max_turns") {
		t.Errorf("an error envelope: %v", err)
	}
	if _, err := ReadOutput([]byte("Not logged in")); err == nil {
		t.Error("plain text read as a result")
	}
}

func TestArgsAreReadOnly(t *testing.T) {
	a := strings.Join(Args("low"), " ")
	for _, want := range []string{"-p /code-review low", "--output-format json", "--no-session-persistence", "--setting-sources  ", "--allowedTools Read Grep Glob"} {
		if !strings.Contains(a, want) {
			t.Errorf("args %q lack %q", a, want)
		}
	}
	for _, bad := range []string{"bypassPermissions", "Edit", "Write", "Bash(git:*)", "dangerously"} {
		if strings.Contains(a, bad) {
			t.Errorf("args %q contain %q", a, bad)
		}
	}
}

func TestRunWithAFakeClaude(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	// echoes its argv's level and its cwd into a findings line
	script := "#!/bin/sh\nprintf '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"%s:1 — reviewed at %s\",\"total_cost_usd\":0.01}' \"$(basename \"$PWD\")/x.go\" \"$(echo $2 | cut -d' ' -f2)\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "repo")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), bin, work, "medium")
	if err != nil || len(r.Findings) != 1 || r.Findings[0].File != "repo/x.go" || r.Findings[0].Summary != "reviewed at medium" {
		t.Fatalf("Run = %+v, %v", r, err)
	}
	if _, err := Run(context.Background(), bin, work, "ultra"); err == nil {
		t.Error("ran an unoffered level")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, bin, work, "low"); err == nil {
		t.Error("a cancelled run returned a result")
	}
}

// TestLiveReview runs the real claude on a scratch repo with a planted
// off-by-one. Opt-in: it spends tokens. LOOM_LIVE_REVIEW=1 (or a level).
func TestLiveReview(t *testing.T) {
	lv := os.Getenv("LOOM_LIVE_REVIEW")
	if lv == "" {
		t.Skip("set LOOM_LIVE_REVIEW=1 (or low|medium|high) to run the real /code-review")
	}
	levels := Levels[:2]
	if ValidLevel(lv) {
		levels = []string{lv}
	}
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "avg.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("package main\n\nfunc avg(xs []int) int {\n\ts := 0\n\tfor _, x := range xs {\n\t\ts += x\n\t}\n\treturn s / len(xs)\n}\n")
	git("add", ".")
	git("commit", "-qm", "init")
	write("package main\n\nfunc avg(xs []int) int {\n\ts := 0\n\tfor i := 1; i <= len(xs); i++ {\n\t\ts += xs[i]\n\t}\n\treturn s / len(xs)\n}\n")
	for _, level := range levels {
		r, err := Run(context.Background(), "claude", dir, level)
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}
		t.Logf("%s: %d findings, $%.3f, %d ms\n%s", level, len(r.Findings), r.CostUSD, r.DurationMS, r.Text)
		hit := false
		for _, f := range r.Findings {
			hit = hit || (strings.HasSuffix(f.File, "avg.go") && f.Line >= 5 && f.Line <= 6)
		}
		if !hit {
			t.Errorf("%s: no parsed finding on avg.go:5-6: %+v", level, r.Findings)
		}
	}
}
