package brief

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Runner runs a command in dir and returns its stdout. Tests swap it.
type Runner func(ctx context.Context, dir, name string, args ...string) (string, error)

// ExecRunner runs real git / gh / glab. Everything it is asked to run is
// read-only: loom never fetches, never takes the index lock
// (GIT_OPTIONAL_LOCKS=0 keeps `git status` from refreshing it), and never
// prompts.
func ExecRunner(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1", "NO_COLOR=1")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	return string(out), err
}

// Commit is one commit inside the window, deduplicated by SHA across every
// clone and worktree of the project.
type Commit struct {
	SHA     string
	Repo    string // the repo it was first seen in (display path)
	Email   string
	Author  string
	At      time.Time
	Refs    string
	Subject string
	Mine    bool
	Stat    string // `git show --stat` (yours only)
	Patch   string // a capped patch excerpt (yours only; model input, not the brief)
}

// Branch is a local branch whose tip moved inside the window.
type Branch struct {
	Repo     string
	Name     string
	Tip      string
	Upstream string
	Track    string // "[ahead 2]", "[gone]", ""
	TipAt    time.Time
	State    string // see branchState
	PR       string // "#228 MERGED https://…", "" when unknown
}

// Dirty is a worktree with uncommitted changes.
type Dirty struct {
	Path    string
	Branch  string
	Changed int
}

// RepoFacts is per-repository context: where it lives and how fresh its
// remote-tracking refs are (loom never fetches, so "merged" can only be as
// fresh as the last fetch).
type RepoFacts struct {
	Path      string
	Remote    string
	Default   string // "origin/main"
	FetchedAt time.Time
}

// GitFacts is everything the brief states about git. Status lines in the
// brief come from here, never from the model's reading of a transcript.
type GitFacts struct {
	Repos    []RepoFacts
	Mine     []Commit
	Others   []Commit
	Branches []Branch
	Dirty    []Dirty
	MyEmails []string
}

const (
	maxMineDetailed = 60
	capStat         = 1500
	capPatch        = 2500
	capPatchTotal   = 90_000
	maxForgeLookups = 20
	maxWorktrees    = 60
)

// collectGit gathers the facts for the given repository paths. Paths that are
// not git repositories are skipped; clones that share a common git dir
// (worktrees) are read once for commits and branches.
func collectGit(ctx context.Context, run Runner, paths []string, since, until time.Time, myEmails []string) GitFacts {
	facts := GitFacts{}
	emails := map[string]bool{}
	for _, e := range myEmails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			emails[e] = true
		}
	}

	type repo struct {
		path, common string
	}
	var repos []repo
	commonSeen := map[string]bool{}
	for _, p := range paths {
		top, err := run(ctx, p, "git", "rev-parse", "--show-toplevel")
		if err != nil {
			continue
		}
		top = strings.TrimSpace(top)
		common, err := run(ctx, top, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			continue
		}
		common = filepath.Clean(strings.TrimSpace(common))
		if commonSeen[common] {
			continue
		}
		commonSeen[common] = true
		repos = append(repos, repo{top, common})
		if e, err := run(ctx, top, "git", "config", "--get", "user.email"); err == nil {
			if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
				emails[e] = true
			}
		}
	}
	for e := range emails {
		facts.MyEmails = append(facts.MyEmails, e)
	}
	sort.Strings(facts.MyEmails)

	seen := map[string]bool{}
	forgeLeft := maxForgeLookups
	for _, r := range repos {
		rf := RepoFacts{Path: r.path}
		if u, err := run(ctx, r.path, "git", "config", "--get", "remote.origin.url"); err == nil {
			rf.Remote = strings.TrimSpace(u)
		}
		rf.Default = defaultBranch(ctx, run, r.path)
		if fi, err := os.Stat(filepath.Join(r.common, "FETCH_HEAD")); err == nil {
			rf.FetchedAt = fi.ModTime()
		}
		facts.Repos = append(facts.Repos, rf)

		out, err := run(ctx, r.path, "git", "log", "--all",
			"--since="+since.Format(time.RFC3339), "--until="+until.Format(time.RFC3339),
			"--format=%H%x1f%ae%x1f%an%x1f%at%x1f%D%x1f%s")
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				f := strings.Split(line, "\x1f")
				if len(f) < 6 || seen[f[0]] {
					continue
				}
				seen[f[0]] = true
				sec, _ := strconv.ParseInt(f[3], 10, 64)
				c := Commit{SHA: f[0], Repo: r.path, Email: f[1], Author: f[2], At: time.Unix(sec, 0), Refs: f[4], Subject: f[5]}
				c.Mine = emails[strings.ToLower(c.Email)]
				if c.Mine {
					facts.Mine = append(facts.Mine, c)
				} else {
					facts.Others = append(facts.Others, c)
				}
			}
		}

		facts.Branches = append(facts.Branches, branches(ctx, run, r.path, rf, since, &forgeLeft)...)
		facts.Dirty = append(facts.Dirty, dirtyWorktrees(ctx, run, r.path)...)
	}

	sort.Slice(facts.Mine, func(i, j int) bool { return facts.Mine[i].At.Before(facts.Mine[j].At) })
	sort.Slice(facts.Others, func(i, j int) bool { return facts.Others[i].At.Before(facts.Others[j].At) })
	patchLeft := capPatchTotal
	for i := range facts.Mine {
		if i >= maxMineDetailed {
			break
		}
		c := &facts.Mine[i]
		if st, err := run(ctx, c.Repo, "git", "show", "--stat=120", "--format=", c.SHA); err == nil {
			c.Stat = cut(st, capStat)
		}
		if patchLeft > 0 {
			if p, err := run(ctx, c.Repo, "git", "show", "--format=", "--unified=2", c.SHA, "--",
				".", ":(exclude)*.lock", ":(exclude)*lock.json", ":(exclude)go.sum", ":(exclude)*.min.*", ":(exclude)*/dist/*"); err == nil {
				c.Patch = cut(p, min(capPatch, patchLeft))
				patchLeft -= len(c.Patch)
			}
		}
	}
	return facts
}

func defaultBranch(ctx context.Context, run Runner, dir string) string {
	if out, err := run(ctx, dir, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "" {
			return b
		}
	}
	for _, b := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, err := run(ctx, dir, "git", "rev-parse", "--verify", "--quiet", b); err == nil {
			return b
		}
	}
	return ""
}

// branches lists local branches whose tip moved inside the window, each
// with one honest state. Ancestry answers "merged" only for merge commits
// and fast-forwards; a squash-merged branch (v-atlas's way) is never an
// ancestor of main, so the forge is asked before the branch is called
// unmerged.
func branches(ctx context.Context, run Runner, dir string, rf RepoFacts, since time.Time, forgeLeft *int) []Branch {
	out, err := run(ctx, dir, "git", "for-each-ref", "refs/heads",
		"--format=%(refname:short)%1f%(objectname:short)%1f%(upstream:short)%1f%(upstream:track)%1f%(committerdate:unix)")
	if err != nil {
		return nil
	}
	var bs []Branch
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) < 5 {
			continue
		}
		sec, _ := strconv.ParseInt(f[4], 10, 64)
		b := Branch{Repo: dir, Name: f[0], Tip: f[1], Upstream: f[2], Track: f[3], TipAt: time.Unix(sec, 0)}
		if b.TipAt.Before(since) {
			continue
		}
		def := strings.TrimPrefix(rf.Default, "origin/")
		if b.Name == def {
			b.State = "default branch"
			if b.Track != "" {
				b.State += " " + b.Track
			}
			bs = append(bs, b)
			continue
		}
		merged := false
		if rf.Default != "" {
			_, err := run(ctx, dir, "git", "merge-base", "--is-ancestor", b.Tip, rf.Default)
			merged = err == nil
		}
		if !merged && *forgeLeft > 0 {
			*forgeLeft--
			b.PR = forgePR(ctx, run, dir, rf.Remote, b.Name)
		}
		b.State = branchState(b, merged)
		bs = append(bs, b)
	}
	return bs
}

// branchState is the one-line status the brief prints for a branch.
func branchState(b Branch, mergedByAncestry bool) string {
	switch {
	case mergedByAncestry:
		return "merged into the default branch"
	case strings.Contains(b.PR, "MERGED") || strings.Contains(b.PR, "merged"):
		return "merged via PR (squash or rebase)"
	case strings.Contains(b.PR, "OPEN") || strings.Contains(b.PR, "opened"):
		return "PR open"
	case strings.Contains(b.PR, "CLOSED") || strings.Contains(b.PR, "closed"):
		return "PR closed without merge"
	case b.Upstream == "":
		return "local only (never pushed)"
	case strings.Contains(b.Track, "gone"):
		return "upstream deleted (often: merged and branch removed)"
	case strings.Contains(b.Track, "ahead"):
		return "pushed, with unpushed commits " + b.Track
	default:
		return "pushed, no PR found"
	}
}

// forgePR asks GitHub or GitLab (read-only list calls) for the newest PR/MR
// from the branch. Any failure leaves it unknown: no forge CLI, no auth, no
// network is a smaller truth, not an error.
func forgePR(ctx context.Context, run Runner, dir, remote, branch string) string {
	switch {
	case strings.Contains(remote, "github.com"):
		out, err := run(ctx, dir, "gh", "pr", "list", "--head", branch, "--state", "all", "--limit", "1", "--json", "number,state,url")
		if err != nil {
			return ""
		}
		var prs []struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			URL    string `json:"url"`
		}
		if json.Unmarshal([]byte(out), &prs) != nil || len(prs) == 0 {
			return ""
		}
		return "#" + strconv.Itoa(prs[0].Number) + " " + prs[0].State + " " + prs[0].URL
	case strings.Contains(remote, "gitlab"):
		out, err := run(ctx, dir, "glab", "mr", "list", "--source-branch", branch, "--all", "--output", "json")
		if err != nil {
			return ""
		}
		var mrs []struct {
			IID    int    `json:"iid"`
			State  string `json:"state"`
			WebURL string `json:"web_url"`
		}
		if json.Unmarshal([]byte(out), &mrs) != nil || len(mrs) == 0 {
			return ""
		}
		return "!" + strconv.Itoa(mrs[0].IID) + " " + mrs[0].State + " " + mrs[0].WebURL
	}
	return ""
}

// dirtyWorktrees reports every worktree of the repo with uncommitted
// changes. Work that was never committed is still work, and the riskiest
// kind to report as done.
func dirtyWorktrees(ctx context.Context, run Runner, dir string) []Dirty {
	out, err := run(ctx, dir, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var res []Dirty
	var path, branch string
	flush := func() {
		if path == "" || len(res) >= maxWorktrees {
			return
		}
		if _, err := os.Stat(path); err != nil {
			return
		}
		st, err := run(ctx, path, "git", "status", "--porcelain")
		if err != nil {
			return
		}
		if n := countLines(st); n > 0 {
			res = append(res, Dirty{Path: path, Branch: branch, Changed: n})
		}
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			path, branch = strings.TrimPrefix(line, "worktree "), ""
		case strings.HasPrefix(line, "branch "):
			branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()
	return res
}

func countLines(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}
