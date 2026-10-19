// Package forge finds the pull request (GitHub) or merge request (GitLab)
// for a branch, with its CI and review state, through the gh and glab CLIs
// the operator is already logged in to. Loom holds no tokens: a forge it
// cannot ask through a CLI is a forge it does not show.
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// PR is a branch's pull or merge request, reduced to what a session row
// shows.
type PR struct {
	Kind   string // github | gitlab
	Number int    // the PR number, or the MR's project-scoped iid
	Title  string
	State  string // open | draft | merged | closed
	URL    string
	Checks string // passing | failing | running | "" (no CI reported)
	Review string // approved | changes_requested | review_required | ""
	// Comments counts an open PR's review feedback (see Feedback); 0 when
	// there is none or it could not be read.
	Comments int
}

// Runner runs name with args in dir and returns its stdout. An error should
// carry the tool's own message (see Exec).
type Runner func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

// Exec is the real Runner.
func Exec(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%s is not installed", name)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			msg, _, _ := strings.Cut(strings.TrimSpace(string(ee.Stderr)), "\n")
			return nil, fmt.Errorf("%s: %s", name, msg)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// Detect names the forge a remote URL points at: "github", "gitlab", or ""
// for any other host.
func Detect(remoteURL string) string {
	u := strings.ToLower(remoteURL)
	switch {
	case strings.Contains(u, "github"):
		return "github"
	case strings.Contains(u, "gitlab"):
		return "gitlab"
	}
	return ""
}

// Lookup returns the newest pull or merge request whose source is branch in
// the repository at dir, or nil when there is none, the branch is the
// default branch (it is a PR's target, never its source), or origin is not
// a forge loom can ask. When the forge's CLI fails, the error comes with a
// PR carrying only Kind, so the failure can name the forge.
func Lookup(ctx context.Context, run Runner, dir, branch string) (*PR, error) {
	if branch == "" || branch == "HEAD" {
		return nil, nil
	}
	remote, err := run(ctx, dir, "git", "remote", "get-url", "origin")
	if err != nil {
		return nil, nil // no origin: nothing to ask
	}
	kind := Detect(strings.TrimSpace(string(remote)))
	if kind == "" || isDefault(ctx, run, dir, branch) {
		return nil, nil
	}
	ask := gitlab
	if kind == "github" {
		ask = github
	}
	pr, err := ask(ctx, run, dir, branch)
	if err != nil {
		return &PR{Kind: kind}, err
	}
	// The count is a hint on the pill; the feedback itself is fetched fresh
	// when the operator opens it, and a failure shows there.
	if pr != nil && (pr.State == "open" || pr.State == "draft") {
		if ts, err := Feedback(ctx, run, dir, pr); err == nil {
			pr.Comments = len(ts)
		}
	}
	return pr, nil
}

// isDefault reports whether branch is origin's default branch, falling back
// to the usual names when origin/HEAD was never fetched.
func isDefault(ctx context.Context, run Runner, dir, branch string) bool {
	if out, err := run(ctx, dir, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/") == branch
	}
	return branch == "main" || branch == "master"
}

type ghPR struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	State          string `json:"state"`
	IsDraft        bool   `json:"isDraft"`
	URL            string `json:"url"`
	ReviewDecision string `json:"reviewDecision"`
	Checks         []struct {
		Typename   string `json:"__typename"`
		Status     string `json:"status"`     // CheckRun
		Conclusion string `json:"conclusion"` // CheckRun
		State      string `json:"state"`      // StatusContext
	} `json:"statusCheckRollup"`
}

func github(ctx context.Context, run Runner, dir, branch string) (*PR, error) {
	out, err := run(ctx, dir, "gh", "pr", "list", "--head", branch, "--state", "all", "--limit", "1",
		"--json", "number,title,state,isDraft,url,reviewDecision,statusCheckRollup")
	if err != nil {
		return nil, err
	}
	var prs []ghPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("gh: unreadable output: %w", err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	p := prs[0]
	pr := &PR{Kind: "github", Number: p.Number, Title: p.Title, URL: p.URL, State: strings.ToLower(p.State)}
	if pr.State == "open" && p.IsDraft {
		pr.State = "draft"
	}
	pr.Review = strings.ToLower(p.ReviewDecision)
	// failing outranks running outranks passing: one red check is the news
	failing, running, any := false, false, false
	for _, c := range p.Checks {
		any = true
		switch {
		case c.Typename == "StatusContext":
			switch c.State {
			case "FAILURE", "ERROR":
				failing = true
			case "PENDING", "EXPECTED":
				running = true
			}
		case c.Status != "COMPLETED":
			running = true
		default:
			switch c.Conclusion {
			case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
				failing = true
			}
		}
	}
	switch {
	case failing:
		pr.Checks = "failing"
	case running:
		pr.Checks = "running"
	case any:
		pr.Checks = "passing"
	}
	return pr, nil
}

type glMR struct {
	IID                 int    `json:"iid"`
	Title               string `json:"title"`
	State               string `json:"state"`
	Draft               bool   `json:"draft"`
	WebURL              string `json:"web_url"`
	DetailedMergeStatus string `json:"detailed_merge_status"`
	HeadPipeline        *struct {
		Status string `json:"status"`
	} `json:"head_pipeline"`
}

// gitlab takes two calls: the list finds the newest MR from the branch in
// any state, and only a single MR's view carries its head pipeline.
func gitlab(ctx context.Context, run Runner, dir, branch string) (*PR, error) {
	out, err := run(ctx, dir, "glab", "mr", "list", "--source-branch", branch, "--all", "--per-page", "1", "-F", "json")
	if err != nil {
		return nil, err
	}
	var mrs []glMR
	if err := json.Unmarshal(out, &mrs); err != nil {
		return nil, fmt.Errorf("glab: unreadable output: %w", err)
	}
	if len(mrs) == 0 {
		return nil, nil
	}
	m := mrs[0]
	if out, err := run(ctx, dir, "glab", "mr", "view", strconv.Itoa(m.IID), "-F", "json"); err == nil {
		var v glMR
		if json.Unmarshal(out, &v) == nil && v.IID == m.IID {
			m = v
		}
	}
	pr := &PR{Kind: "gitlab", Number: m.IID, Title: m.Title, URL: m.WebURL}
	switch m.State {
	case "opened":
		pr.State = "open"
		if m.Draft {
			pr.State = "draft"
		}
	case "merged":
		pr.State = "merged"
	default: // closed, locked
		pr.State = "closed"
	}
	// GitLab's MR JSON says when approval is missing, never that it was
	// given (that needs the approvals API, a third call), so a GitLab MR is
	// never shown "approved".
	switch m.DetailedMergeStatus {
	case "not_approved":
		pr.Review = "review_required"
	case "requested_changes":
		pr.Review = "changes_requested"
	}
	if m.HeadPipeline != nil {
		switch m.HeadPipeline.Status {
		case "success":
			pr.Checks = "passing"
		case "failed", "canceled":
			pr.Checks = "failing"
		case "running", "pending", "created", "preparing", "waiting_for_resource", "scheduled":
			pr.Checks = "running"
		}
	}
	return pr, nil
}
