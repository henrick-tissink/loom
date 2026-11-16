package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/gitdiff"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/taskfile"
	"github.com/henricktissink/loom/internal/transcript"
)

// Restart with a brief: a session that drifts, spins or outgrows its
// context ends, and a fresh one starts in the same place with a brief of
// what the old one knew (Anthropic's advice: a new session per task, with a
// brief of the task, its constraints, the files and what was ruled out).
// loom composes the brief from the task file, the diff and the transcript,
// without a model; the operator edits it before it is sent.

// restartIn is what a brief is composed from.
type restartIn struct {
	title    string // the rail's title: the ask when there is no task file
	outcome  string
	scope    string // ## Scope of the task file, as written
	notes    string // ## Notes
	checks   []string
	open     []bool
	files    []string // changed since the task started, as "label: path" when several repos
	moreFile int      // files left out of the list
	failing  []transcript.TestRun
	spin     *SpinDTO
	drift    []DriftDTO
}

// maxBriefFiles and maxBriefFails bound the brief's lists.
const (
	maxBriefFiles = 40
	maxBriefFails = 5
)

// restartBrief composes the brief as the new session's first message.
func restartBrief(in restartIn) string {
	var b strings.Builder
	b.WriteString("You are taking over a task from an earlier session that I ended because it ")
	switch {
	case in.spin != nil:
		b.WriteString("kept repeating a failure")
	case len(in.drift) > 0:
		b.WriteString("was drifting")
	default:
		b.WriteString("ran out of room")
	}
	b.WriteString(". Its changes are still in the working tree. Check the current state before you change anything.\n\n")

	b.WriteString("## Task\n")
	switch {
	case in.outcome != "":
		b.WriteString(in.outcome + "\n")
	case in.title != "":
		b.WriteString(in.title + "\n")
	default:
		b.WriteString("(not recorded: ask me)\n")
	}
	if s := strings.TrimSpace(in.scope); s != "" {
		b.WriteString("\n## Scope\n" + s + "\n")
	}
	if len(in.checks) > 0 {
		b.WriteString("\n## Done when\n")
		for i, c := range in.checks {
			box := "[x]"
			if i < len(in.open) && in.open[i] {
				box = "[ ]"
			}
			fmt.Fprintf(&b, "- %s %s\n", box, c)
		}
		b.WriteString("Ticked checks are the earlier session's word: re-run them before relying on them.\n")
	}
	if len(in.files) > 0 {
		b.WriteString("\n## Files changed so far\n")
		for _, f := range in.files {
			b.WriteString("- " + f + "\n")
		}
		if in.moreFile > 0 {
			fmt.Fprintf(&b, "- and %d more (git status)\n", in.moreFile)
		}
	}
	var ruled []string
	if s := in.spin; s != nil {
		what := s.Tool
		if s.Subject != "" {
			what += " " + s.Subject
		}
		line := fmt.Sprintf("The same failure %d times in a row (%s)", s.Count, what)
		if s.Sample != "" {
			line += ": " + s.Sample
		}
		ruled = append(ruled, line)
	}
	for _, t := range in.failing {
		line := "Failed: `" + shortCmd(t.Command) + "`"
		if t.Sample != "" {
			line += ": " + t.Sample
		}
		ruled = append(ruled, line)
	}
	for _, d := range in.drift {
		ruled = append(ruled, d.Detail)
	}
	if len(ruled) > 0 {
		b.WriteString("\n## What did not work\n")
		for _, r := range ruled {
			b.WriteString("- " + r + "\n")
		}
		b.WriteString("Don't repeat these approaches without a reason you can state.\n")
	}
	if s := strings.TrimSpace(in.notes); s != "" {
		b.WriteString("\n## Notes from the earlier session\n" + s + "\n")
	}
	b.WriteString("\nStart by checking where the work stands against the Done-when checks, then name the one next step.")
	return b.String()
}

// shortCmd folds a command onto one line for a list item.
func shortCmd(s string) string {
	s = oneLine(s)
	if r := []rune(s); len(r) > 160 {
		s = string(r[:157]) + "..."
	}
	return s
}

// lastFailures is the latest failing run of each distinct command, newest
// first, at most n.
func lastFailures(runs []transcript.TestRun, n int) []transcript.TestRun {
	var out []transcript.TestRun
	seen := map[string]bool{}
	for i := len(runs) - 1; i >= 0 && len(out) < n; i-- {
		r := runs[i]
		k := shortCmd(r.Command)
		if seen[k] {
			continue
		}
		seen[k] = true // a later pass of the same command means it is not ruled out
		if r.Status == "failed" {
			out = append(out, r)
		}
	}
	return out
}

// taskSection is a "## name" section of a task file, without placeholder
// text in parentheses.
func taskSection(text, name string) string {
	var out []string
	in := false
	for _, ln := range strings.Split(text, "\n") {
		if strings.HasPrefix(ln, "## ") {
			in = strings.EqualFold(strings.TrimSpace(ln[3:]), name)
			continue
		}
		if in {
			out = append(out, ln)
		}
	}
	s := strings.TrimSpace(strings.Join(out, "\n"))
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && !strings.Contains(s, "\n") {
		return "" // the template's placeholder
	}
	return s
}

// RestartBrief composes the brief for restarting a live session.
func (a *App) RestartBrief(name string) (string, error) {
	e, ok := a.railRow(name)
	if !ok {
		return "", fmt.Errorf("%s is not on the rail any more", name)
	}
	row := e.row
	in := restartIn{title: e.dto.Title, spin: e.dto.Spin, drift: e.dto.Drift}
	started := row.CreatedAt // without a task file: since the session began
	if row.HasTask {
		started = row.Task.StartedAt
		in.outcome, in.checks, in.open = row.Task.Outcome, row.Task.Checks, row.Task.Open
		if a.launcher != nil && row.ClaudeSessionID != "" {
			p := filepath.Join(taskfile.Dir(a.launcher.ClaudeConfigDir), "tasks", row.ClaudeSessionID+".md")
			if b, err := os.ReadFile(p); err == nil {
				in.scope, in.notes = taskSection(string(b), "Scope"), taskSection(string(b), "Notes")
			}
		}
	}
	if p := a.transcriptFor(row.Cwd, row.ClaudeSessionID); p != "" {
		if act, err := transcript.ScanActivity(p, started); err == nil {
			in.failing = lastFailures(act.Runs, maxBriefFails)
		}
	}
	branch := ""
	if e.dto.Worktree != nil {
		branch = e.dto.Worktree.Branch
	}
	dirs := sessionDirs(row.SessionRow)
	labels := a.targetLabels()
	for _, dir := range dirs {
		br := ""
		if dir == row.Cwd {
			br = branch
		}
		base, _ := diffBase(dir, br, started)
		d := gitdiff.Against(dir, base)
		if d.Error != "" {
			continue
		}
		fs := append(append([]string{}, d.Files...), d.Untracked...)
		sort.Strings(fs)
		for _, f := range fs {
			if len(dirs) > 1 {
				l := labels[dir]
				if l == "" {
					l = filepath.Base(dir)
				}
				f = l + ": " + f
			}
			if len(in.files) < maxBriefFiles {
				in.files = append(in.files, f)
			} else {
				in.moreFile++
			}
		}
	}
	return restartBrief(in), nil
}

// RestartDTO is what RestartWithBrief did.
type RestartDTO struct {
	Name string `json:"name"` // the new session
	// EndPending: the old session ends once the brief is delivered, and
	// stays open if it never is.
	EndPending bool `json:"endPending"`
}

// RestartWithBrief starts a fresh session with the old one's launch shape
// and brief as its first message. With end set, the old session is ended
// once the brief has reached the new claude (the launcher's seed status):
// a seed that fails, or a session that never becomes ready, leaves the old
// one open, so the operator never loses both.
func (a *App) RestartWithBrief(name, brief string, end bool) (RestartDTO, error) {
	if strings.TrimSpace(brief) == "" {
		return RestartDTO{}, fmt.Errorf("the brief is empty")
	}
	r, err := a.freshRecipe(name)
	if err != nil {
		return RestartDTO{}, err
	}
	r.Seed = brief
	nn, err := a.launcher.Launch(r, launchCols, launchRows, a.now())
	if err != nil {
		return RestartDTO{}, err
	}
	if !end {
		return RestartDTO{Name: nn}, nil
	}
	go func() {
		seed := func() string {
			row, ok, err := a.st.Get(nn)
			if err != nil || !ok {
				return "failed"
			}
			return row.SeedStatus
		}
		if res := endAfterSeed(seed, func() error { return a.KillSession(name) }, seedWait, time.Second); res != "ended" {
			log.Printf("restart: %s left open: %s", name, res)
		}
	}()
	return RestartDTO{Name: nn, EndPending: true}, nil
}

// seedWait is how long the old session waits on the new one's brief: past
// the launcher's ready timeout (60s) and its 15-minute wait on a trust
// dialog.
const seedWait = 16 * time.Minute

// endAfterSeed polls the seed's delivery and ends the old session once it
// is sent. It returns "ended", or why the old session was left open.
func endAfterSeed(seed func() string, kill func() error, wait, every time.Duration) string {
	deadline := time.Now().Add(wait)
	for {
		switch seed() {
		case "sent":
			if err := kill(); err != nil {
				return "could not end it: " + err.Error()
			}
			return "ended"
		case "failed":
			return "the brief was not delivered"
		}
		if time.Now().After(deadline) {
			return "the brief was not delivered in time"
		}
		time.Sleep(every)
	}
}

// freshRecipe is a live session's launch shape (cwd, add-dirs, model,
// mode, fence) with no seed.
func (a *App) freshRecipe(name string) (session.Recipe, error) {
	if a.launcher == nil || a.st == nil {
		return session.Recipe{}, fmt.Errorf("launcher unavailable")
	}
	row, ok, err := a.st.Get(name)
	if err != nil {
		return session.Recipe{}, err
	}
	if !ok {
		return session.Recipe{}, fmt.Errorf("session %q not found", name)
	}
	if row.Cwd == "" {
		return session.Recipe{}, fmt.Errorf("session has no recorded directory")
	}
	return session.Recipe{
		ProjectLabel: row.ProjectLabel, Cwd: row.Cwd, Model: row.Model, Mode: row.Mode,
		AddDirs: session.DecodeAddDirs(row.AddDirs), Fenced: session.IsFenced(row.Tags),
	}, nil
}
