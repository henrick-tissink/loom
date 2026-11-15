package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/taskfile"
)

func notesNow() time.Time { return time.Unix(5000, 0) }

func TestCommentLifecycleKeepsAndReloads(t *testing.T) {
	dir := t.TempDir()
	s := newCommentStore(dir, notesNow)
	if _, err := s.add("sid", Comment{File: "a.go", Line: 3, Body: "   "}); err == nil {
		t.Fatal("an empty comment was added")
	}
	if _, err := s.add("sid", Comment{File: "a.go", Body: "x"}); err == nil {
		t.Fatal("a comment with no line was added")
	}
	n, err := s.add("sid", Comment{Repo: "/r", File: "a.go", Line: 3, EndLine: 1, Side: "weird", Quote: "x := 1", Body: " use a const "})
	if err != nil {
		t.Fatal(err)
	}
	c := n.Comments[0]
	if c.ID == "" || c.State != cmOpen || c.Side != "new" || c.EndLine != 3 || c.Body != "use a const" || c.Created != 5000 {
		t.Fatalf("added = %+v", c)
	}
	if _, err := s.setState("sid", c.ID, cmInvalid, " "); err == nil {
		t.Fatal("invalid without a reason")
	}
	if _, err := s.setState("sid", c.ID, "done", ""); err == nil {
		t.Fatal("an unknown state")
	}
	if n, _ = s.markSent("sid", []string{c.ID, "nope"}); n.Comments[0].State != cmSent || n.Comments[0].Sent != 5000 {
		t.Fatalf("sent = %+v", n.Comments[0])
	}
	if n, _ = s.setState("sid", c.ID, cmInvalid, "it is generated code"); n.Comments[0].State != cmInvalid || n.Comments[0].Reason != "it is generated code" {
		t.Fatalf("invalid = %+v", n.Comments[0])
	}
	// reopening clears the reason and the sent time: it goes out again
	if n, _ = s.setState("sid", c.ID, cmOpen, ""); n.Comments[0].Reason != "" || n.Comments[0].Sent != 0 {
		t.Fatalf("reopened = %+v", n.Comments[0])
	}
	if n, _ = s.edit("sid", c.ID, "use a named const"); n.Comments[0].Body != "use a named const" {
		t.Fatalf("edited = %+v", n.Comments[0])
	}
	if _, err := s.setViewed("sid", "since", "/r", "a.go", "h1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.setViewed("sid", "everything", "/r", "a.go", "h1"); err == nil {
		t.Fatal("an unknown diff scope")
	}
	if _, err := s.setFinding("sid", "k", "wontfix", ""); err == nil {
		t.Fatal("an unknown finding state")
	}
	if _, err := s.setFinding("sid", FindingKey("a.go", 3, "nil deref"), cmInvalid, ""); err == nil {
		t.Fatal("an invalid finding without a reason")
	}
	if _, err := s.setFinding("sid", FindingKey("a.go", 3, "nil deref"), cmInvalid, "checked above"); err != nil {
		t.Fatal(err)
	}

	// a fresh store (an app restart) reads it all back
	r := newCommentStore(dir, notesNow).get("sid")
	if len(r.Comments) != 1 || r.Comments[0].Body != "use a named const" || r.Viewed[viewedKey("since", "/r", "a.go")] != "h1" || r.Viewed[viewedKey("uncommitted", "/r", "a.go")] != "" ||
		r.Findings["a.go:3:nil deref"].Reason != "checked above" {
		t.Fatalf("reloaded = %+v", r)
	}
	if n, _ = s.setViewed("sid", "since", "/r", "a.go", ""); len(n.Viewed) != 0 {
		t.Fatalf("unviewed = %+v", n.Viewed)
	}
	if n, _ = s.setFinding("sid", "a.go:3:nil deref", "", ""); len(n.Findings) != 0 {
		t.Fatalf("finding cleared = %+v", n.Findings)
	}
	if n, _ = s.remove("sid", c.ID); len(n.Comments) != 0 {
		t.Fatalf("removed = %+v", n.Comments)
	}
	if _, err := s.remove("sid", c.ID); err == nil {
		t.Fatal("removed a missing comment")
	}
	// another session's notes are its own
	if o := s.get("other"); len(o.Comments) != 0 || o.Viewed == nil || o.Findings == nil {
		t.Fatalf("other = %+v", o)
	}
}

func TestCommentSaveFailureLeavesNotes(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := newCommentStore(filepath.Join(blocker, "comments"), notesNow) // MkdirAll fails under a file
	if _, err := s.add("sid", Comment{File: "a.go", Line: 1, Body: "x"}); err == nil {
		t.Fatal("a save under a file succeeded")
	}
	if n := s.get("sid"); len(n.Comments) != 0 {
		t.Fatalf("an unsaved comment is kept: %+v", n.Comments)
	}
}

func TestCommentsPrompt(t *testing.T) {
	cs := []Comment{
		{ID: "3", Repo: "/w", File: "b.go", Line: 9, EndLine: 9, Side: "new", Quote: "y()", Body: "why?", State: cmOpen},
		{ID: "1", Repo: "/w", File: "a.go", Line: 4, EndLine: 6, Side: "new", Quote: "a\nb\nc", Body: "split this", State: cmOpen},
		{ID: "2", Repo: "/w", File: "a.go", Line: 1, Body: "already sent", State: cmSent},
		{ID: "4", Repo: "/other", File: "c.md", Line: 2, EndLine: 2, Side: "old", Quote: "has ``` inside", Body: "keep this line", State: cmOpen},
	}
	p, ids := commentsPrompt("/w", cs)
	if strings.Join(ids, ",") != "4,1,3" {
		t.Errorf("ids = %v", ids)
	}
	for _, want := range []string{
		"Review comments on your changes (3).",
		"1. /other/c.md:2 (removed lines; numbers are in the old file)\n````\nhas ``` inside\n````\nkeep this line\n",
		"2. a.go:4-6\n```\na\nb\nc\n```\nsplit this\n",
		"3. b.go:9\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "already sent") {
		t.Error("a sent comment went out again")
	}
	if p, ids := commentsPrompt("/w", cs[2:3]); p != "" || ids != nil {
		t.Errorf("no open comments gave %q %v", p, ids)
	}
}

func TestCommentsOnTheRail(t *testing.T) {
	a := &App{notes: newCommentStore("", notesNow)}
	if _, err := a.AddComment("loom-1", Comment{File: "a.go", Line: 1, Body: "x"}); err == nil {
		t.Fatal("a comment for a session not on the rail")
	}
	a.noteRail([]SessionDTO{{Name: "loom-1"}}, []status.Row{
		{SessionRow: store.SessionRow{Name: "loom-1", ClaudeSessionID: "sid1", Cwd: "/w"}},
	})
	n, err := a.AddComment("loom-1", Comment{Repo: "/w", File: "a.go", Line: 1, Quote: "x", Body: "rename"})
	if err != nil || len(n.Comments) != 1 {
		t.Fatalf("add = %+v %v", n, err)
	}
	p, err := a.CommentsPrompt("loom-1")
	if err != nil || !strings.Contains(p, "1. a.go:1\n") {
		t.Fatalf("prompt = %q %v", p, err)
	}
	// no tmux: the send fails and nothing is marked sent
	if err := a.SendComments("loom-1", p); err == nil {
		t.Fatal("sent without tmux")
	}
	if n, _ := a.ReviewNotes("loom-1"); n.Comments[0].State != cmOpen {
		t.Fatalf("a failed send marked it %s", n.Comments[0].State)
	}
	if _, err := (&App{}).ReviewNotes("loom-1"); err == nil {
		t.Fatal("notes without a store")
	}
}

func TestDiffSinceTaskStart(t *testing.T) {
	dir := t.TempDir()
	git := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), append([]string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}, env...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	at := func(ts int64) []string {
		d := fmt.Sprintf("@%d +0000", ts)
		return []string{"GIT_AUTHOR_DATE=" + d, "GIT_COMMITTER_DATE=" + d}
	}
	git(nil, "init", "-q")
	write("before.go", "package x\n")
	git(at(1000), "add", ".")
	git(at(1000), "commit", "-qm", "before")
	write("during.go", "package x\n")
	git(at(3000), "add", ".")
	git(at(3000), "commit", "-qm", "during")
	write("new.go", "package x\n")

	a := &App{}
	row := func(started int64) {
		a.noteRail([]SessionDTO{{Name: "loom-1"}}, []status.Row{{
			SessionRow: store.SessionRow{Name: "loom-1", ClaudeSessionID: "sid1", Cwd: dir},
			Task:       taskfile.Task{StartedAt: started}, HasTask: started > 0,
		}})
	}
	row(2000)
	d, err := a.SessionDiffSince("loom-1")
	if err != nil || len(d.Repos) != 1 {
		t.Fatalf("diff = %+v %v", d, err)
	}
	r := d.Repos[0]
	if !r.Dirty || r.Commits != 1 || !strings.Contains(r.Patch, "during.go") || !strings.Contains(r.Patch, "new.go") ||
		strings.Contains(r.Patch, "before.go") || !strings.Contains(d.Scopes[0], "since the task started") || r.Label == "" {
		t.Fatalf("since = %+v %v", r, d.Scopes)
	}
	// no task: the uncommitted changes, and the scope says so
	row(0)
	if d, _ = a.SessionDiffSince("loom-1"); strings.Contains(d.Repos[0].Patch, "during.go") || d.Scopes[0] != "uncommitted changes and new files" {
		t.Fatalf("no task = %+v %v", d.Repos[0], d.Scopes)
	}
	if _, err := a.SessionDiffSince("loom-9"); err == nil {
		t.Fatal("a diff for a session not on the rail")
	}
}
