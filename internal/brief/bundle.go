package brief

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Scope is the project a brief is about.
type Scope struct {
	Root  string   // the project root
	Name  string   // its display name
	Repos []string // member repo paths that exist
}

// Options configures one evidence collection.
type Options struct {
	Scope           Scope
	Since, Until    time.Time
	ClaudeConfigDir string
	MyEmails        []string // added to every repo's user.email
	Budget          int      // characters of evidence; DefaultBudget when 0
	Run             Runner   // ExecRunner when nil
}

// DefaultBudget keeps the evidence near 100k tokens: room for a busy day on a
// multi-repo project with the writer's own output on top.
const DefaultBudget = 420_000

// gitShare is the most of the budget git facts and diffs may take.
const gitShare = 140_000

// Bundle is the evidence for one brief.
type Bundle struct {
	Scope    Scope
	Since    time.Time
	Until    time.Time
	Sessions []*Session
	Git      GitFacts
}

// Collect builds the bundle: transcript evidence attributed to the project
// by each record's own cwd (the launch directory a session was started in
// says nothing about where its work happened), then git facts for every repo
// the project has or the work touched.
func Collect(ctx context.Context, o Options) (*Bundle, error) {
	if o.Scope.Root == "" {
		return nil, fmt.Errorf("brief: a project root is required")
	}
	run := o.Run
	if run == nil {
		run = ExecRunner
	}
	sc := newScoper(ctx, run, o.Scope)
	sessions, err := scanTranscripts(scanOptions{
		projectsDir: filepath.Join(o.ClaudeConfigDir, "projects"),
		since:       o.Since, until: o.Until,
		inScope: sc.in,
	})
	if err != nil {
		return nil, fmt.Errorf("brief: read transcripts: %w", err)
	}
	for i, s := range sessions {
		s.ID = fmt.Sprintf("S%d", i+1)
	}

	paths := append([]string{o.Scope.Root}, o.Scope.Repos...)
	paths = append(paths, childRepos(o.Scope.Root)...)
	for _, s := range sessions {
		paths = append(paths, s.Cwds...)
	}
	git := collectGit(ctx, run, existing(dedupe(paths)), o.Since, o.Until, o.MyEmails)

	b := &Bundle{Scope: o.Scope, Since: o.Since, Until: o.Until, Sessions: sessions, Git: git}
	budget := o.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	gitLen := len(b.GitMarkdown()) + len(b.DiffMarkdown())
	if gitLen > gitShare {
		gitLen = gitShare
	}
	fitSessions(sessions, budget-gitLen)
	return b, nil
}

// scoper answers "is this cwd part of the project?" by path first, then by
// git common dir, so a worktree outside the root (under /private/tmp, or a
// tool's own worktree folder) still counts for its repo.
type scoper struct {
	ctx     context.Context
	run     Runner
	prefix  []string
	commons map[string]bool
	cache   map[string]bool
	loaded  bool
	scope   Scope
}

func newScoper(ctx context.Context, run Runner, s Scope) *scoper {
	sc := &scoper{ctx: ctx, run: run, cache: map[string]bool{}, scope: s}
	for _, p := range append([]string{s.Root}, s.Repos...) {
		if p != "" {
			sc.prefix = append(sc.prefix, filepath.Clean(p))
		}
	}
	return sc
}

func (sc *scoper) in(cwd string) bool {
	if cwd == "" {
		return false
	}
	cwd = filepath.Clean(cwd)
	if v, ok := sc.cache[cwd]; ok {
		return v
	}
	v := false
	for _, p := range sc.prefix {
		if under(cwd, p) {
			v = true
			break
		}
	}
	if !v {
		v = sc.sameRepo(cwd)
	}
	sc.cache[cwd] = v
	return v
}

func (sc *scoper) sameRepo(cwd string) bool {
	if _, err := os.Stat(cwd); err != nil {
		return false
	}
	if !sc.loaded {
		sc.loaded = true
		sc.commons = map[string]bool{}
		for _, p := range existing(append(append([]string{sc.scope.Root}, sc.scope.Repos...), childRepos(sc.scope.Root)...)) {
			if c := commonDir(sc.ctx, sc.run, p); c != "" {
				sc.commons[c] = true
			}
		}
	}
	c := commonDir(sc.ctx, sc.run, cwd)
	return c != "" && sc.commons[c]
}

func commonDir(ctx context.Context, run Runner, dir string) string {
	out, err := run(ctx, dir, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	return filepath.Clean(strings.TrimSpace(out))
}

func under(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// childRepos is every git checkout directly under root: a project's member
// list can lag what is on disk.
func childRepos(root string) []string {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if x == "" {
			continue
		}
		x = filepath.Clean(x)
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func existing(xs []string) []string {
	var out []string
	for _, x := range xs {
		if fi, err := os.Stat(x); err == nil && fi.IsDir() {
			out = append(out, x)
		}
	}
	return out
}

// entrySize is roughly what an entry costs once rendered.
func entrySize(e Entry) int { return len(e.Text) + 40 }

func sessionSize(s *Session) int {
	n := 200
	for _, e := range s.Entries {
		n += entrySize(e)
	}
	return n
}

// fitSessions trims the sessions to the budget. Small sessions keep
// everything and hand their unused share to large ones; a large session
// keeps every anchor (task files, commands, lava, summaries) and the head
// and tail of its conversation, losing the middle.
func fitSessions(ss []*Session, budget int) {
	if len(ss) == 0 || budget <= 0 {
		return
	}
	sizes := make([]int, len(ss))
	total := 0
	for i, s := range ss {
		sizes[i] = sessionSize(s)
		total += sizes[i]
	}
	if total <= budget {
		return
	}
	order := make([]int, len(ss))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return sizes[order[a]] < sizes[order[b]] })
	left, n := budget, len(ss)
	for k, i := range order {
		share := left / (n - k)
		if sizes[i] <= share {
			left -= sizes[i]
			continue
		}
		trimSession(ss[i], share)
		left -= sessionSize(ss[i])
	}
}

func trimSession(s *Session, limit int) {
	anchors := 200
	var talk []int
	for i, e := range s.Entries {
		if e.Anchor {
			anchors += entrySize(e)
		} else {
			talk = append(talk, i)
		}
	}
	room := limit - anchors
	if room < 0 {
		room = 0
	}
	keep := map[int]bool{}
	used := 0
	h := 0
	for ; h < len(talk) && used+entrySize(s.Entries[talk[h]]) <= room*2/5; h++ {
		keep[talk[h]] = true
		used += entrySize(s.Entries[talk[h]])
	}
	for t := len(talk) - 1; t >= h && used+entrySize(s.Entries[talk[t]]) <= room; t-- {
		keep[talk[t]] = true
		used += entrySize(s.Entries[talk[t]])
	}
	var out []Entry
	dropped := 0
	for i, e := range s.Entries {
		if e.Anchor || keep[i] {
			out = append(out, e)
		} else {
			dropped++
		}
	}
	s.Entries, s.Trimmed = out, s.Trimmed+dropped
}

// Stats are the bundle's headline counts.
type Stats struct {
	Sessions     int
	Commits      int // yours
	OtherCommits int
	Chars        int
	InputTokens  int // a rough estimate: 4 characters per token
}

// Stats counts the bundle.
func (b *Bundle) Stats() Stats {
	md := b.Markdown()
	return Stats{
		Sessions: len(b.Sessions), Commits: len(b.Git.Mine), OtherCommits: len(b.Git.Others),
		Chars: len(md), InputTokens: len(md) / 4,
	}
}

func stamp(t time.Time) string { return t.Local().Format("Mon 2 Jan 15:04") }

// Markdown renders the whole bundle: the writer's input, and the file a
// study session reads when asked about a task.
func (b *Bundle) Markdown() string {
	var w strings.Builder
	fmt.Fprintf(&w, "# Evidence: %s, %s to %s\n\n", b.Scope.Name, stamp(b.Since), stamp(b.Until))
	fmt.Fprintf(&w, "Project root: %s\n", b.Scope.Root)
	if len(b.Scope.Repos) > 0 {
		fmt.Fprintf(&w, "Member repos: %s\n", strings.Join(b.Scope.Repos, ", "))
	}
	w.WriteString("\nEvidence ids are `S<n>:L<line>`: session n of this bundle, line of its transcript file.\n")
	w.WriteString("Kinds: task-file (finish-line versions), cmd (git/forge/test/build command, with result), lava (coordination event), you (Henrick typed), delegated (a parent agent's prompt to a subagent), claude (the agent said), summary (compaction summary).\n\n")
	w.WriteString(b.SessionsMarkdown())
	w.WriteString(b.GitMarkdown())
	w.WriteString(b.DiffMarkdown())
	return w.String()
}

// SessionsMarkdown renders the transcript evidence.
func (b *Bundle) SessionsMarkdown() string {
	var w strings.Builder
	w.WriteString("## Sessions\n\n")
	if len(b.Sessions) == 0 {
		w.WriteString("No session worked in this project during the window.\n\n")
	}
	bySID := map[string]string{}
	for _, s := range b.Sessions {
		if !s.Subagent {
			bySID[s.SessionID] = s.ID
		}
	}
	for _, s := range b.Sessions {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&w, "### %s · %s\n", s.ID, oneLine(title))
		fmt.Fprintf(&w, "session %s · %s → %s", s.SessionID, stamp(s.First), stamp(s.Last))
		if s.Subagent {
			parent := filepath.Base(filepath.Dir(filepath.Dir(s.File)))
			if id, ok := bySID[parent]; ok {
				fmt.Fprintf(&w, " · subagent of %s", id)
			} else {
				fmt.Fprintf(&w, " · subagent of session %s", parent)
			}
		}
		w.WriteString("\n")
		fmt.Fprintf(&w, "file: %s\n", s.File)
		fmt.Fprintf(&w, "cwd: %s\n", strings.Join(s.Cwds, ", "))
		if len(s.Files) > 0 {
			fmt.Fprintf(&w, "edited: %s\n", strings.Join(capList(s.Files, 40), ", "))
		}
		if s.Trimmed > 0 {
			fmt.Fprintf(&w, "(%d conversation entries from the middle of this session were left out for size; anchors are complete)\n", s.Trimmed)
		}
		w.WriteString("\n")
		for _, e := range s.Entries {
			fmt.Fprintf(&w, "- [%s:L%d %s %s] %s\n", s.ID, e.Line, e.At.Local().Format("Mon 15:04"), e.Kind, indent(e.Text))
		}
		w.WriteString("\n")
	}
	return w.String()
}

// GitMarkdown renders the git facts. The same text is appended to the saved
// brief, so its status claims can always be checked against it.
func (b *Bundle) GitMarkdown() string {
	g := b.Git
	var w strings.Builder
	w.WriteString("## Git facts\n\n")
	fmt.Fprintf(&w, "Read-only, collected %s. Henrick's commits are matched by email: %s.\n\n", stamp(b.Until), strings.Join(g.MyEmails, ", "))
	if len(g.Repos) > 0 {
		w.WriteString("Repositories (remote state is only as fresh as each repo's last fetch; loom never fetches):\n\n")
		for _, r := range g.Repos {
			fetched := "never fetched"
			if !r.FetchedAt.IsZero() {
				fetched = "last fetch " + stamp(r.FetchedAt)
			}
			fmt.Fprintf(&w, "- %s · default %s · %s\n", r.Path, orDash(r.Default), fetched)
		}
		w.WriteString("\n")
	}
	fmt.Fprintf(&w, "### Commits by Henrick in the window (%d)\n\n", len(g.Mine))
	for _, c := range g.Mine {
		refs := ""
		if c.Refs != "" {
			refs = " (" + c.Refs + ")"
		}
		fmt.Fprintf(&w, "- `%s` %s · %s · %s%s\n", c.SHA[:min(10, len(c.SHA))], stamp(c.At), filepath.Base(c.Repo), c.Subject, refs)
		if c.Stat != "" {
			fmt.Fprintf(&w, "  ```\n%s\n  ```\n", indentBlock(c.Stat))
		}
	}
	if len(g.Mine) == 0 {
		w.WriteString("None.\n")
	}
	w.WriteString("\n### Branches whose tip moved in the window\n\n")
	if len(g.Branches) == 0 {
		w.WriteString("None.\n")
	}
	for _, br := range g.Branches {
		pr := ""
		if br.PR != "" {
			pr = " · " + br.PR
		}
		fmt.Fprintf(&w, "- %s · `%s` @ %s · %s%s\n", filepath.Base(br.Repo), br.Name, br.Tip, br.State, pr)
	}
	w.WriteString("\n### Uncommitted work\n\n")
	if len(g.Dirty) == 0 {
		w.WriteString("None.\n")
	}
	for _, d := range g.Dirty {
		fmt.Fprintf(&w, "- %s (%s): %d changed files\n", d.Path, orDash(d.Branch), d.Changed)
	}
	fmt.Fprintf(&w, "\n### Other people's commits in the window (%d)\n\n", len(g.Others))
	for i, c := range g.Others {
		if i >= 40 {
			fmt.Fprintf(&w, "- … and %d more\n", len(g.Others)-i)
			break
		}
		fmt.Fprintf(&w, "- `%s` %s · %s · %s · %s\n", c.SHA[:min(10, len(c.SHA))], stamp(c.At), c.Author, filepath.Base(c.Repo), c.Subject)
	}
	w.WriteString("\n")
	return w.String()
}

// DiffMarkdown renders the patch excerpts of Henrick's commits: the writer
// needs them to explain mechanisms, the saved brief does not carry them.
func (b *Bundle) DiffMarkdown() string {
	var w strings.Builder
	any := false
	for _, c := range b.Git.Mine {
		if c.Patch == "" {
			continue
		}
		if !any {
			w.WriteString("## Diff excerpts (Henrick's commits)\n\n")
			any = true
		}
		fmt.Fprintf(&w, "### `%s` %s\n```diff\n%s\n```\n\n", c.SHA[:min(10, len(c.SHA))], c.Subject, c.Patch)
	}
	return w.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func indent(s string) string { return strings.ReplaceAll(s, "\n", "\n    ") }

func indentBlock(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + strings.TrimSpace(l)
	}
	return strings.Join(lines, "\n")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func capList(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(append([]string{}, xs[:n]...), fmt.Sprintf("… %d more", len(xs)-n))
}
