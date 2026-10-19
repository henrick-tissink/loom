package forge

import (
	"context"
	"strings"
	"testing"
)

func TestGitHubFeedback(t *testing.T) {
	run := fake(map[string]string{"gh api graphql": `{"data":{"repository":{"pullRequest":{
"commits":{"nodes":[{"commit":{"committedDate":"2026-10-19T09:30:00Z","checkSuites":{"nodes":[{"createdAt":"2026-10-19T10:00:00Z"}]}}}]},
"reviews":{"nodes":[
 {"author":{"login":"old"},"state":"CHANGES_REQUESTED","body":"before the push","submittedAt":"2026-10-19T09:00:00Z"},
 {"author":{"login":"rev"},"state":"CHANGES_REQUESTED","body":"after the push","submittedAt":"2026-10-19T11:00:00Z"},
 {"author":{"login":"rev"},"state":"APPROVED","body":"","submittedAt":"2026-10-19T12:00:00Z"}]},
"comments":{"nodes":[{"author":null,"body":"a PR comment","createdAt":"2026-10-19T10:30:00Z"},
 {"author":{"login":"codecov","__typename":"Bot"},"body":"coverage 81%","createdAt":"2026-10-19T10:40:00Z"},
 {"author":{"login":"early","__typename":"User"},"body":"before the push, after the commit","createdAt":"2026-10-19T09:45:00Z"}]},
"reviewThreads":{"nodes":[
 {"isResolved":true,"isOutdated":false,"path":"a.go","line":3,"originalLine":3,"comments":{"nodes":[{"author":{"login":"x"},"body":"done"}]}},
 {"isResolved":false,"isOutdated":true,"path":"z.go","line":null,"originalLine":9,"comments":{"nodes":[{"author":{"login":"rev"},"body":"rename"},{"author":{"login":"me"},"body":"why?"}]}},
 {"isResolved":false,"isOutdated":false,"path":"b.go","line":12,"originalLine":12,"comments":{"nodes":[{"author":{"login":"rev"},"body":"nil check"}]}}]}
}}}}`})
	ts, err := Feedback(context.Background(), run, "/r", &PR{Kind: "github", Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	// unresolved threads by path, then what was said since the push, in order
	var got []string
	for _, th := range ts {
		got = append(got, th.Path+"|"+th.Review+"|"+th.Notes[0].Author+"|"+th.Notes[0].Body)
	}
	want := []string{"b.go||rev|nil check", "z.go||rev|rename", "||ghost|a PR comment", "|changes_requested|rev|after the push"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if ts[1].Line != 9 || !ts[1].Outdated || len(ts[1].Notes) != 2 {
		t.Fatalf("outdated thread: %+v", ts[1])
	}
}

func TestGitLabFeedback(t *testing.T) {
	run := fake(map[string]string{
		"glab api --paginate projects/:id/merge_requests/55/discussions": `[
 {"notes":[{"body":"added 2 commits","system":true,"created_at":"2026-10-19T11:00:00Z","author":{"username":"me"}}]},
 {"notes":[{"body":"fixed","system":false,"resolvable":true,"resolved":true,"created_at":"2026-10-19T11:00:00Z","author":{"username":"rev"}}]},
 {"notes":[{"body":"use a constant","system":false,"resolvable":true,"resolved":false,"created_at":"2026-10-19T09:00:00Z","author":{"username":"rev"},
   "position":{"new_path":"x.py","old_path":"x.py","new_line":null,"old_line":4}},
   {"body":"ok","system":false,"resolvable":true,"resolved":false,"created_at":"2026-10-19T09:05:00Z","author":{"username":"me"}}]},
 {"notes":[{"body":"old remark","system":false,"resolvable":false,"created_at":"2026-10-19T09:00:00Z","author":{"username":"rev"}}]},
 {"notes":[{"body":"before the push","system":false,"resolvable":false,"created_at":"2026-10-19T09:50:00Z","author":{"username":"rev"}}]}]
[{"notes":[{"body":"new remark","system":false,"resolvable":false,"created_at":"2026-10-19T11:00:00Z","author":{"username":"rev"}}]}]`,
		"glab api projects/:id/merge_requests/55/versions": `[{"created_at":"2026-10-19T10:00:00Z"}]`,
	})
	ts, err := Feedback(context.Background(), run, "/r", &PR{Kind: "gitlab", Number: 55})
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].Path != "x.py" || ts[0].Line != 4 || len(ts[0].Notes) != 2 || ts[1].Notes[0].Body != "new remark" {
		t.Fatalf("got %+v", ts)
	}
}

func TestPrompt(t *testing.T) {
	p := Prompt(&PR{Kind: "gitlab", Number: 55, Title: "Scan uploads", URL: "https://gitlab.com/g/p/-/merge_requests/55"}, []Thread{
		{Path: "x.py", Line: 4, Outdated: true, Notes: []Note{{"rev", "use a\nconstant"}, {"me", "ok"}}},
		{Review: "changes_requested", Notes: []Note{{"rev", "tests?"}}},
		{Notes: []Note{{"bot", strings.Repeat("é", noteLimit)}}},
	})
	for _, want := range []string{
		"Review feedback on MR !55 (Scan uploads): https://gitlab.com/g/p/-/merge_requests/55",
		"1. x.py:4 (outdated: the code has changed since)\n   rev: use a\n   constant\n   me: ok",
		"2. Review (changes requested):\n   rev: tests?",
		"3. Comment:\n   bot: ",
		" […]",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
	if !strings.HasSuffix(p, "[…]") || strings.Contains(p, "\uFFFD") {
		t.Fatal("a long comment is cut on a rune boundary")
	}
}
