package brief

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func ts(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }

func jl(t *testing.T, recs ...map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, r := range recs {
		j, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

func user(at time.Duration, cwd string, content any) map[string]any {
	return map[string]any{"type": "user", "timestamp": ts(at), "cwd": cwd, "message": map[string]any{"content": content}}
}

func asst(at time.Duration, cwd string, blocks ...map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "timestamp": ts(at), "cwd": cwd, "message": map[string]any{"content": blocks}}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The scanner keeps only in-window, in-scope records; pairs a command with
// its result; keeps task-file versions; drops harness text; and skips a
// brief's own study session entirely.
func TestScanTranscripts(t *testing.T) {
	dir := t.TempDir()
	proj := "/w/acme"
	main := jl(t,
		user(-48*time.Hour, proj, "too early, outside the window"),
		user(time.Minute, proj, "add the export button"),
		user(2*time.Minute, proj, "<system-reminder>harness text</system-reminder>"),
		asst(3*time.Minute, proj,
			map[string]any{"type": "text", "text": "Adding it now."},
			map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "go test ./... && git commit -m 'feat: export'"}},
			map[string]any{"type": "tool_use", "id": "tu2", "name": "Bash", "input": map[string]any{"command": "ls -la"}},
			map[string]any{"type": "tool_use", "id": "tu3", "name": "Write", "input": map[string]any{"file_path": "/h/.claude/state/tasks/x.md", "content": "## Outcome\nexport works\nStatus: done"}},
			map[string]any{"type": "tool_use", "id": "tu4", "name": "Edit", "input": map[string]any{"file_path": proj + "/export.go", "new_string": "x"}},
		),
		user(4*time.Minute, proj, []map[string]any{{"type": "tool_result", "tool_use_id": "tu1", "content": "ok  acme 0.3s\n[main abc123] feat: export"}}),
		user(5*time.Minute, "/w/other", "work in another project"),
	)
	writeFile(t, filepath.Join(dir, "-w-acme", "s1.jsonl"), main)
	study := jl(t, user(time.Minute, proj, SeedMarker+" I am studying a task"), asst(2*time.Minute, proj, map[string]any{"type": "text", "text": "ready"}))
	writeFile(t, filepath.Join(dir, "-w-acme", "s2.jsonl"), study)
	sub := jl(t, user(6*time.Minute, proj, "You are a verifier"), asst(7*time.Minute, proj, map[string]any{"type": "text", "text": "verified"}))
	writeFile(t, filepath.Join(dir, "-w-acme", "s1", "subagents", "agent-1.jsonl"), sub)

	ss, err := scanTranscripts(scanOptions{projectsDir: dir, since: t0.Add(-time.Hour), until: t0.Add(time.Hour),
		inScope: func(c string) bool { return under(c, proj) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 {
		t.Fatalf("want main + subagent (study session skipped), got %d", len(ss))
	}
	s := ss[0]
	kinds := map[string][]string{}
	for _, e := range s.Entries {
		kinds[e.Kind] = append(kinds[e.Kind], e.Text)
	}
	if len(kinds[KindYou]) != 1 || kinds[KindYou][0] != "add the export button" {
		t.Errorf("typed text: %q", kinds[KindYou])
	}
	if len(kinds[KindCommand]) != 1 || !strings.Contains(kinds[KindCommand][0], "→ (ok)") || !strings.Contains(kinds[KindCommand][0], "[main abc123]") {
		t.Errorf("command with result: %q", kinds[KindCommand])
	}
	if len(kinds[KindTaskFile]) != 1 || !strings.Contains(kinds[KindTaskFile][0], "Status: done") {
		t.Errorf("task file: %q", kinds[KindTaskFile])
	}
	if strings.Contains(fmt.Sprint(kinds), "another project") || strings.Contains(fmt.Sprint(kinds), "too early") || strings.Contains(fmt.Sprint(kinds), "harness") {
		t.Errorf("out-of-scope, out-of-window or harness text leaked: %v", kinds)
	}
	if len(s.Files) != 1 || s.Files[0] != proj+"/export.go" {
		t.Errorf("edited files: %v", s.Files)
	}
	if !ss[1].Subagent || ss[1].Entries[0].Kind != KindDelegate || !strings.HasPrefix(ss[1].Title, "subagent: ") {
		t.Errorf("subagent prompt must be 'delegated' with a subagent title: %+v", ss[1])
	}
}

// Over budget, a session keeps every anchor and loses the middle of its
// conversation, never the head or the tail.
func TestFitSessionsKeepsAnchors(t *testing.T) {
	s := &Session{}
	for i := 0; i < 50; i++ {
		s.Entries = append(s.Entries, Entry{Line: i, Kind: KindClaude, Text: strings.Repeat("x", 500)})
		if i%10 == 0 {
			s.Entries = append(s.Entries, Entry{Line: i, Kind: KindCommand, Text: "$ git commit", Anchor: true})
		}
	}
	small := &Session{Entries: []Entry{{Kind: KindYou, Text: "hi"}}}
	fitSessions([]*Session{s, small}, 8000)
	anchors := 0
	for _, e := range s.Entries {
		if e.Anchor {
			anchors++
		}
	}
	if anchors != 5 {
		t.Errorf("anchors kept = %d, want 5", anchors)
	}
	if s.Trimmed == 0 || s.Entries[0].Line != 0 || s.Entries[len(s.Entries)-1].Line != 49 {
		t.Errorf("want head and tail kept and middle trimmed; trimmed=%d first=%d last=%d", s.Trimmed, s.Entries[0].Line, s.Entries[len(s.Entries)-1].Line)
	}
	if len(small.Entries) != 1 {
		t.Error("a small session must keep everything")
	}
}

// fakeGit answers git/gh calls from a table keyed by "dir|args".
type fakeGit map[string]string

func (f fakeGit) run(_ context.Context, dir, name string, args ...string) (string, error) {
	k := dir + "|" + name + " " + strings.Join(args, " ")
	for pat, out := range f {
		if strings.HasPrefix(k, pat) {
			if out == "ERR" {
				return "", fmt.Errorf("fail")
			}
			return out, nil
		}
	}
	return "", fmt.Errorf("unexpected: %s", k)
}

// Commits are deduplicated across worktrees of one repo and across clones,
// authors are matched by email, and a squash-merged branch is reported as
// merged via its PR, not as unmerged.
func TestCollectGit(t *testing.T) {
	wt := t.TempDir()
	tip := fmt.Sprint(t0.Add(-time.Hour).Unix())
	log := "aaa\x1fme@x.io\x1fMe\x1f1760529600\x1f\x1ffeat: export\nbbb\x1fpat@x.io\x1fPat\x1f1760529700\x1f\x1ffix: other\n"
	f := fakeGit{
		"/r1|git rev-parse --show-toplevel":      "/r1\n",
		"/r1-wt|git rev-parse --show-toplevel":   "/r1-wt\n",
		"/r1|git rev-parse --path-format":        "/r1/.git\n",
		"/r1-wt|git rev-parse --path-format":     "/r1/.git\n", // a worktree of r1
		"/r2|git rev-parse --show-toplevel":      "/r2\n",
		"/r2|git rev-parse --path-format":        "/r2/.git\n", // a second clone
		"/r1|git config --get user.email":        "me@x.io\n",
		"/r2|git config --get user.email":        "ERR",
		"/r1|git config --get remote.origin.url": "https://github.com/acme/r.git\n",
		"/r2|git config --get remote.origin.url": "https://github.com/acme/r.git\n",
		"/r1|git symbolic-ref":                   "origin/main\n",
		"/r2|git symbolic-ref":                   "origin/main\n",
		"/r1|git log":                            log,
		"/r2|git log":                            log,
		"/r1|git for-each-ref":                   "feat\x1fccc\x1forigin/feat\x1f[gone]\x1f" + tip + "\nlocal\x1fddd\x1f\x1f\x1f" + tip + "\nold\x1feee\x1f\x1f\x1f100\n",
		"/r2|git for-each-ref":                   "",
		"/r1|git merge-base --is-ancestor ccc":   "ERR",
		"/r1|git merge-base --is-ancestor ddd":   "ERR",
		"/r1|gh pr list --head feat":             `[{"number":12,"state":"MERGED","url":"https://github.com/acme/r/pull/12"}]`,
		"/r1|gh pr list --head local":            `[]`,
		"/r1|git worktree list":                  "worktree " + wt + "\nbranch refs/heads/main\n",
		"/r2|git worktree list":                  "",
		wt + "|git status --porcelain":           " M a.go\n?? b.go\n",
		"/r1|git show --stat":                    " export.go | 3 +++\n",
		"/r1|git show --format= --unified=2 aaa": "diff --git a/export.go b/export.go\n+func Export() {}\n",
	}
	g := collectGit(context.Background(), f.run, []string{"/r1", "/r1-wt", "/r2"}, t0.Add(-24*time.Hour), t0, nil)
	if len(g.Mine) != 1 || g.Mine[0].SHA != "aaa" || len(g.Others) != 1 {
		t.Fatalf("want one of mine and one other, deduplicated across clones: mine=%v others=%v", g.Mine, g.Others)
	}
	if g.Mine[0].Stat == "" || !strings.Contains(g.Mine[0].Patch, "Export") {
		t.Errorf("my commit lacks stat or patch: %+v", g.Mine[0])
	}
	states := map[string]string{}
	for _, b := range g.Branches {
		states[b.Name] = b.State
	}
	if states["feat"] != "merged via PR (squash or rebase)" {
		t.Errorf("squash-merged branch: %q", states["feat"])
	}
	if states["local"] != "local only (never pushed)" {
		t.Errorf("local branch: %q", states["local"])
	}
	if _, ok := states["old"]; ok {
		t.Error("a branch whose tip is outside the window must not be listed")
	}
	if len(g.Dirty) != 1 || g.Dirty[0].Changed != 2 {
		t.Errorf("dirty worktrees: %+v", g.Dirty)
	}
}

func TestSplitTasks(t *testing.T) {
	src := "# P\n\n## Overview\nall good\n\n## Task: Export button\n### Outcome\nit exports\n```\n## Task: not a heading inside code\n```\n\n## Task: Fix login\n### Outcome\nfixed\n\n## Decisions and consultations\n- asked X\n"
	parts := Split(src)
	var titles []string
	for _, p := range parts {
		if p.Task > 0 {
			titles = append(titles, p.Title)
		}
	}
	if strings.Join(titles, "|") != "Export button|Fix login" {
		t.Fatalf("tasks = %q", titles)
	}
	p, ok := Task(src, 2)
	if !ok || strings.Contains(p.MD, "Decisions") || !strings.Contains(p.MD, "fixed") {
		t.Errorf("task 2 must stop at the next level-2 heading: %q", p.MD)
	}
	if last := parts[len(parts)-1]; last.Task != 0 || !strings.Contains(last.MD, "asked X") {
		t.Errorf("trailing section lost: %+v", last)
	}
}

// Model text never becomes markup or script.
func TestHTMLEscapesRawHTML(t *testing.T) {
	out := HTML("hi <script>alert(1)</script> [x](javascript:alert(1)) **b**")
	if strings.Contains(out, "<script>") || strings.Contains(out, "javascript:") {
		t.Errorf("unsafe HTML rendered: %s", out)
	}
	if !strings.Contains(out, "<strong>b</strong>") {
		t.Errorf("markdown not rendered: %s", out)
	}
	page := Page(Meta{ProjectName: "<b>x</b>", CreatedAt: t0.Unix()}, "## Task: A\nbody")
	if strings.Contains(page, "<b>x</b>") || !strings.Contains(page, `id="task-1"`) {
		t.Errorf("page title not escaped or task anchor missing")
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	a := Archive{Dir: t.TempDir()}
	m, err := a.Save(Meta{ProjectRoot: "/w/acme", ProjectName: "Acme Co", Window: "3d", Sessions: 2}, "# brief", "# evidence", t0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m.ID, "acme-co-") {
		t.Errorf("id = %q", m.ID)
	}
	ms, err := a.List("/w/acme", "Acme Co")
	if err != nil || len(ms) != 1 || ms[0].ID != m.ID {
		t.Fatalf("list = %v, %v", ms, err)
	}
	if other, _ := a.List("/w/other", "Acme Co"); len(other) != 0 {
		t.Error("two projects with one name must not share briefs")
	}
	got, md, err := a.Load(m.ID)
	if err != nil || md != "# brief" || got.Sessions != 2 {
		t.Fatalf("load = %+v %q %v", got, md, err)
	}
	for _, bad := range []string{"../../etc/passwd", "acme/../../x", ""} {
		if _, _, err := a.Load(bad); err == nil {
			t.Errorf("Load(%q) must refuse", bad)
		}
	}
}

// The writer runs the hardened argv with the evidence on stdin.
func TestWriterArgvAndStdin(t *testing.T) {
	dir := t.TempDir()
	argsOut, stdinOut := filepath.Join(dir, "args"), filepath.Join(dir, "stdin")
	script := filepath.Join(dir, "claude")
	writeFile(t, script, fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\ncat > %q\necho '## Overview'\n", argsOut, stdinOut))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := Writer{Binary: script, WorkDir: dir}.Write(context.Background(), "EVIDENCE")
	if err != nil || out != "## Overview" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	args, _ := os.ReadFile(argsOut)
	stdin, _ := os.ReadFile(stdinOut)
	for _, want := range []string{"--model\nopus\n", "--tools\n\n", "--strict-mcp-config", "--no-session-persistence", "--setting-sources\n\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("argv lacks %q:\n%s", want, args)
		}
	}
	if string(stdin) != "EVIDENCE" {
		t.Errorf("stdin = %q", stdin)
	}
}

// Hiding fails closed: a hidden project has no brief, and Ungrouped has none.
func TestServiceScopeRefusesHidden(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, p := range []store.Project{{Root: "/w/open", Name: "Open", Origin: "created"}, {Root: "/w/secret", Name: "Secret", Origin: "created"}} {
		if err := st.UpsertProject(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetProjectHidden("/w/secret", true, 1); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: st}
	if sc, err := s.Scope("open"); err != nil || sc.Root != "/w/open" {
		t.Errorf("scope by name: %+v %v", sc, err)
	}
	if _, err := s.Scope("/w/secret"); err == nil {
		t.Error("a hidden project must be refused")
	}
	if _, err := s.Generate(context.Background(), &Bundle{Scope: Scope{Name: "Open"}}, Windows[0]); err == nil {
		t.Error("an empty bundle must not reach the writer")
	}
}
