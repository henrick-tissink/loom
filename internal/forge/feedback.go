package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Note is one comment in a piece of feedback.
type Note struct {
	Author string
	Body   string
}

// Thread is one piece of review feedback: an unresolved diff thread (Path
// set), or a review summary or PR-level comment. Those last two cannot be
// resolved, so only the ones posted after the last push count: feedback on
// the code as it stands, not every remark the PR ever had.
type Thread struct {
	Path     string
	Line     int
	Outdated bool   // the diff moved on under the thread
	Review   string // a review summary's verdict (changes_requested, commented, approved); "" otherwise
	Notes    []Note
	at       time.Time
}

// Feedback returns a PR's open review feedback: unresolved diff threads
// first (by path), then review summaries and comments since the last push,
// oldest first. GitHub's newest 100 unresolved-or-not threads are read.
func Feedback(ctx context.Context, run Runner, dir string, pr *PR) ([]Thread, error) {
	var ts []Thread
	var err error
	if pr.Kind == "github" {
		ts, err = githubFeedback(ctx, run, dir, pr.Number)
	} else {
		ts, err = gitlabFeedback(ctx, run, dir, pr.Number)
	}
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if (a.Path != "") != (b.Path != "") {
			return a.Path != ""
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Path != "" {
			return a.Line < b.Line
		}
		return a.at.Before(b.at)
	})
	return ts, nil
}

const githubFeedbackQuery = `query($owner:String!,$repo:String!,$n:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$n){
commits(last:1){nodes{commit{committedDate checkSuites(first:1){nodes{createdAt}}}}}
reviews(last:50){nodes{author{login} state body submittedAt}}
comments(last:50){nodes{author{__typename login} body createdAt}}
reviewThreads(last:100){nodes{isResolved isOutdated path line originalLine comments(first:50){nodes{author{login} body}}}}
}}}`

type ghAuthor struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

func (a *ghAuthor) name() string {
	if a == nil || a.Login == "" {
		return "ghost" // a deleted account
	}
	return a.Login
}

func githubFeedback(ctx context.Context, run Runner, dir string, n int) ([]Thread, error) {
	out, err := run(ctx, dir, "gh", "api", "graphql", "-f", "query="+githubFeedbackQuery,
		"-F", "owner={owner}", "-F", "repo={repo}", "-F", "n="+strconv.Itoa(n))
	if err != nil {
		return nil, err
	}
	var d struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					Commits struct {
						Nodes []struct {
							Commit struct {
								CommittedDate time.Time `json:"committedDate"`
								CheckSuites   struct {
									Nodes []struct {
										CreatedAt time.Time `json:"createdAt"`
									} `json:"nodes"`
								} `json:"checkSuites"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
					Reviews struct {
						Nodes []struct {
							Author      *ghAuthor `json:"author"`
							State       string    `json:"state"`
							Body        string    `json:"body"`
							SubmittedAt time.Time `json:"submittedAt"`
						} `json:"nodes"`
					} `json:"reviews"`
					Comments struct {
						Nodes []struct {
							Author    *ghAuthor `json:"author"`
							Body      string    `json:"body"`
							CreatedAt time.Time `json:"createdAt"`
						} `json:"nodes"`
					} `json:"comments"`
					ReviewThreads struct {
						Nodes []struct {
							IsResolved   bool   `json:"isResolved"`
							IsOutdated   bool   `json:"isOutdated"`
							Path         string `json:"path"`
							Line         *int   `json:"line"`
							OriginalLine *int   `json:"originalLine"`
							Comments     struct {
								Nodes []struct {
									Author *ghAuthor `json:"author"`
									Body   string    `json:"body"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, fmt.Errorf("gh: unreadable review output: %w", err)
	}
	p := d.Data.Repository.PullRequest
	if p == nil {
		return nil, fmt.Errorf("gh: PR #%d not found", n)
	}
	// "Since the last push": GitHub no longer reports when a commit was
	// pushed, so the head commit's first check suite (created by the push)
	// stands in when it is later than the commit's own date, as it is for
	// a commit made well before it was pushed.
	var since time.Time
	if c := p.Commits.Nodes; len(c) > 0 {
		since = c[0].Commit.CommittedDate
		if cs := c[0].Commit.CheckSuites.Nodes; len(cs) > 0 && cs[0].CreatedAt.After(since) {
			since = cs[0].CreatedAt
		}
	}
	var ts []Thread
	for _, t := range p.ReviewThreads.Nodes {
		if t.IsResolved || len(t.Comments.Nodes) == 0 {
			continue
		}
		th := Thread{Path: t.Path, Outdated: t.IsOutdated}
		if t.Line != nil {
			th.Line = *t.Line
		} else if t.OriginalLine != nil {
			th.Line = *t.OriginalLine
		}
		for _, c := range t.Comments.Nodes {
			th.Notes = append(th.Notes, Note{Author: c.Author.name(), Body: c.Body})
		}
		ts = append(ts, th)
	}
	for _, r := range p.Reviews.Nodes {
		if strings.TrimSpace(r.Body) == "" || !r.SubmittedAt.After(since) {
			continue
		}
		ts = append(ts, Thread{Review: strings.ToLower(r.State), at: r.SubmittedAt,
			Notes: []Note{{Author: r.Author.name(), Body: r.Body}}})
	}
	for _, c := range p.Comments.Nodes {
		if strings.TrimSpace(c.Body) == "" || !c.CreatedAt.After(since) {
			continue
		}
		// An app's PR comment (coverage, preview deploy) is posted on every
		// push and is not review; apps reviewing code do it as reviews.
		if c.Author != nil && c.Author.Typename == "Bot" {
			continue
		}
		ts = append(ts, Thread{at: c.CreatedAt, Notes: []Note{{Author: c.Author.name(), Body: c.Body}}})
	}
	return ts, nil
}

type glNote struct {
	Body       string    `json:"body"`
	System     bool      `json:"system"`
	Resolvable bool      `json:"resolvable"`
	Resolved   bool      `json:"resolved"`
	CreatedAt  time.Time `json:"created_at"`
	Author     struct {
		Username string `json:"username"`
	} `json:"author"`
	Position *struct {
		NewPath string `json:"new_path"`
		OldPath string `json:"old_path"`
		NewLine *int   `json:"new_line"`
		OldLine *int   `json:"old_line"`
	} `json:"position"`
}

func gitlabFeedback(ctx context.Context, run Runner, dir string, iid int) ([]Thread, error) {
	base := "projects/:id/merge_requests/" + strconv.Itoa(iid)
	// Discussions come oldest first and every push or label change adds a
	// system one, so a busy MR's newest threads sit past the first page:
	// all pages are read (glab prints them as consecutive JSON arrays).
	out, err := run(ctx, dir, "glab", "api", "--paginate", base+"/discussions?per_page=100")
	if err != nil {
		return nil, err
	}
	var ds []struct {
		Notes []glNote `json:"notes"`
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var page []struct {
			Notes []glNote `json:"notes"`
		}
		if err := dec.Decode(&page); err != nil {
			return nil, fmt.Errorf("glab: unreadable review output: %w", err)
		}
		ds = append(ds, page...)
	}
	// The newest diff version is created by the last push: "since the
	// last push" exactly, whatever the commits' own dates say.
	var since time.Time
	if out, err := run(ctx, dir, "glab", "api", base+"/versions?per_page=1"); err == nil {
		var vs []struct {
			CreatedAt time.Time `json:"created_at"`
		}
		if json.Unmarshal(out, &vs) == nil && len(vs) > 0 {
			since = vs[0].CreatedAt
		}
	}
	var ts []Thread
	for _, d := range ds {
		if len(d.Notes) == 0 || d.Notes[0].System {
			continue
		}
		first := d.Notes[0]
		th := Thread{at: first.CreatedAt}
		if first.Resolvable {
			if first.Resolved {
				continue
			}
		} else if !first.CreatedAt.After(since) {
			continue // a plain comment: only the ones since the latest commit
		}
		if p := first.Position; p != nil {
			th.Path = p.NewPath
			if th.Path == "" {
				th.Path = p.OldPath
			}
			if p.NewLine != nil {
				th.Line = *p.NewLine
			} else if p.OldLine != nil {
				th.Line = *p.OldLine
			}
		}
		for _, n := range d.Notes {
			if !n.System {
				th.Notes = append(th.Notes, Note{Author: n.Author.Username, Body: n.Body})
			}
		}
		ts = append(ts, th)
	}
	return ts, nil
}

// noteLimit bounds one comment in the prompt: a pasted log or a bot's
// coverage table would otherwise crowd out the feedback around it.
const noteLimit = 4000

// Prompt composes feedback into the message sent to the session. The
// operator sees and can edit it before it is sent.
func Prompt(pr *PR, ts []Thread) string {
	ref := fmt.Sprintf("PR #%d", pr.Number)
	if pr.Kind == "gitlab" {
		ref = fmt.Sprintf("MR !%d", pr.Number)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Review feedback on %s (%s): %s\n", ref, pr.Title, pr.URL)
	b.WriteString("Address each point below: change the code where the reviewer is right, and say why where you disagree.\n")
	for i, t := range ts {
		b.WriteString("\n")
		switch {
		case t.Path != "":
			fmt.Fprintf(&b, "%d. %s", i+1, t.Path)
			if t.Line > 0 {
				fmt.Fprintf(&b, ":%d", t.Line)
			}
			if t.Outdated {
				b.WriteString(" (outdated: the code has changed since)")
			}
			b.WriteString("\n")
		case t.Review != "":
			fmt.Fprintf(&b, "%d. Review (%s):\n", i+1, strings.ReplaceAll(t.Review, "_", " "))
		default:
			fmt.Fprintf(&b, "%d. Comment:\n", i+1)
		}
		for _, n := range t.Notes {
			body := strings.TrimSpace(n.Body)
			if len(body) > noteLimit {
				cut := noteLimit
				for cut > 0 && !utf8.RuneStart(body[cut]) {
					cut--
				}
				body = body[:cut] + " […]"
			}
			fmt.Fprintf(&b, "   %s: %s\n", n.Author, strings.ReplaceAll(body, "\n", "\n   "))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
