package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/gitdiff"
	"github.com/henricktissink/loom/internal/hooklog"
	"github.com/henricktissink/loom/internal/taskfile"
	"github.com/henricktissink/loom/internal/transcript"
)

// The review sheet: one page to judge a session's work by. It puts the
// task's finish line next to who proved each check (the stop guard ran it,
// or the model only says so), the test commands the transcript shows ran
// and how they ended, what changed, and the risks loom can see for itself.
// All of it is read on the click; nothing is asked of a model.

// ReviewDTO is a session's review sheet.
type ReviewDTO struct {
	Name       string `json:"name"`
	TaskStatus string `json:"taskStatus"` // "" without a task file
	TaskPath   string `json:"taskPath"`
	Outcome    string `json:"outcome"`
	// Since is when the task started (unix seconds); tests are counted
	// from it. 0: the whole session.
	Since  int64         `json:"since"`
	Checks []ReviewCheck `json:"checks"`
	// Tests are the latest test runs, oldest first; TestsTotal counts all.
	Tests      []transcript.TestRun `json:"tests"`
	TestsTotal int                  `json:"testsTotal"`
	Changes    []ReviewRepo         `json:"changes"`
	Worktree   *WorktreeDTO         `json:"worktree,omitempty"`
	Risks      []ReviewRisk         `json:"risks"`
}

// ReviewCheck is one Done-when check and who proved it. Proof is
// guard_passed (the stop guard ran its command on this version of the
// task file and it passed), guard_failed (it ran and failed), claimed
// (ticked with a command the guard has not run on this version), observed
// (ticked with no command: the model's word) or open.
type ReviewCheck struct {
	Name     string `json:"name"`
	Command  string `json:"command,omitempty"`
	Text     string `json:"text"` // the check as written, without its evidence
	Evidence string `json:"evidence,omitempty"`
	Ticked   bool   `json:"ticked"`
	Proof    string `json:"proof"`
}

// ReviewRepo is one directory's uncommitted changes.
type ReviewRepo struct {
	Label     string   `json:"label"`
	Path      string   `json:"path"`
	Stat      string   `json:"stat"`
	Files     []string `json:"files"`
	Untracked []string `json:"untracked"`
	Error     string   `json:"error,omitempty"`
}

// ReviewRisk is something to look at before accepting the work: bad is a
// known problem, warn a gap in what was shown.
type ReviewRisk struct {
	Level string `json:"level"` // bad | warn
	Text  string `json:"text"`
}

// checkCmd is the hooks' executable-check form (_CHECK_CMD in
// enforce/common.py): `name: $ command`.
var checkCmd = regexp.MustCompile(`^([^:$]+?)\s*:\s*\$\s+(\S.*)$`)

// splitCheck reads a Done-when check: its name, its command if it has
// one, and the evidence a ticked check carries after " # ".
func splitCheck(text string, ticked bool) (name, cmd, body, evidence string) {
	body = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), " #"))
	if ticked {
		if i := strings.LastIndex(body, " # "); i >= 0 {
			body, evidence = strings.TrimSpace(body[:i]), strings.TrimSpace(body[i+3:])
		}
	}
	if m := checkCmd.FindStringSubmatch(body); m != nil && !strings.HasPrefix(m[2], "<") {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), body, evidence
	}
	name, _, _ = strings.Cut(body, ":")
	return strings.TrimSpace(name), "", body, evidence
}

// reviewIn is everything the sheet is built from.
type reviewIn struct {
	dto      SessionDTO
	task     taskfile.Task
	hasTask  bool
	taskPath string
	// guardRun is the stop guard's latest run of the `$` checks (passed or
	// failed), or nil. A later "already nudged" verdict or a promise block
	// does not run them, so it is not the latest verdict but the latest run.
	guardRun *hooklog.Record
	act      transcript.Activity
	repos    []gitdiff.RepoDiff
	dirs     []string // the session's directories: edits outside them (the task file) don't count
}

// guardRan is the stop guard's verdicts that ran every `$` check: one that
// failed lists the failed checks' names, so the rest passed.
var guardRan = map[string]bool{"block_done_check_failed": true, "allow_done_checks_passed": true}

// lastGuardRun is the latest Stop record that ran the checks.
func lastGuardRun(recs []hooklog.Record) *hooklog.Record {
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if r.Hook == "stop_guard" && r.Event == "Stop" && !r.Silent && guardRan[r.Decision] {
			return &r
		}
	}
	return nil
}

func buildReview(in reviewIn) ReviewDTO {
	d := in.dto
	out := ReviewDTO{Name: d.Name, Checks: []ReviewCheck{}, Tests: []transcript.TestRun{}, Changes: []ReviewRepo{}, Risks: []ReviewRisk{}, Worktree: d.Worktree}
	risk := func(level, f string, a ...any) {
		out.Risks = append(out.Risks, ReviewRisk{level, fmt.Sprintf(f, a...)})
	}

	var failedNames map[string]bool // set when the guard ran this version's checks
	if in.hasTask {
		out.TaskStatus, out.TaskPath, out.Outcome, out.Since = in.task.Status, in.taskPath, in.task.Outcome, in.task.StartedAt
		// a run older than the file was about an earlier version
		if s := in.guardRun; s != nil && !in.task.Verified && s.TS.Unix() >= in.task.Modified {
			failedNames = map[string]bool{}
			for _, n := range s.Failed {
				failedNames[strings.TrimSpace(n)] = true
			}
		}
		for i, text := range in.task.Checks {
			ticked := !in.task.Open[i]
			name, cmd, body, ev := splitCheck(text, ticked)
			c := ReviewCheck{Name: name, Command: cmd, Text: body, Evidence: ev, Ticked: ticked}
			switch {
			case cmd != "" && in.task.Verified:
				c.Proof = "guard_passed"
			case cmd != "" && failedNames[name]:
				c.Proof = "guard_failed"
			case cmd != "" && failedNames != nil:
				c.Proof = "guard_passed"
			case !ticked:
				c.Proof = "open"
			case cmd != "":
				c.Proof = "claimed"
			default:
				c.Proof = "observed"
			}
			out.Checks = append(out.Checks, c)
		}
	}

	out.Tests = append(out.Tests, in.act.Runs...)
	out.TestsTotal = in.act.Total

	for _, r := range in.repos {
		rr := ReviewRepo{Label: r.Label, Path: r.Path, Stat: r.Stat, Files: r.Files, Untracked: r.Untracked, Error: r.Error}
		if rr.Files == nil {
			rr.Files = []string{}
		}
		if rr.Untracked == nil {
			rr.Untracked = []string{}
		}
		out.Changes = append(out.Changes, rr)
	}

	// Risks, worst first.
	if d.Spin != nil {
		risk("bad", "It is repeating a failure: %s, %d times this turn.", d.Spin.Tool, d.Spin.Count)
	}
	var failed, claimed, observed []string
	for _, c := range out.Checks {
		switch c.Proof {
		case "guard_failed":
			failed = append(failed, c.Name)
		case "claimed":
			claimed = append(claimed, c.Name)
		case "observed":
			observed = append(observed, c.Name)
		}
	}
	if len(failed) > 0 {
		risk("bad", "The stop guard ran %s on this version of the task file and %s.", list(failed), plural(len(failed), "it failed", "they failed"))
	}
	var last, lastPass *transcript.TestRun
	for i := range out.Tests {
		r := &out.Tests[i]
		last = r
		if r.Status == "passed" {
			lastPass = r
		}
	}
	since := "since the task started"
	if out.Since == 0 {
		since = "in this session"
	}
	switch {
	case out.TestsTotal == 0:
		risk("warn", "No test command ran %s.", since)
	case last.Status == "failed":
		risk("bad", "The last test run failed: %s (%s).", last.Command, last.Sample)
	case last.Status == "unknown":
		risk("warn", "The last test run went to the background and its result is not in the transcript: %s.", last.Command)
	}
	if lastPass != nil {
		var after []string
		for _, e := range in.act.Edits {
			if e.At > lastPass.At && under(e.Path, in.dirs) {
				after = append(after, filepath.Base(e.Path))
			}
		}
		if len(after) > 0 {
			risk("warn", "%s edited after the last passing test run: %s.", plural(len(after), "1 file was", fmt.Sprintf("%d files were", len(after))), list(after))
		}
	}
	if !in.hasTask {
		risk("warn", "There is no task file, so there is no finish line to check the work against.")
	} else {
		if out.TaskStatus == "open" {
			risk("warn", "The task file is still open: %d of %d checks ticked.", in.task.Done, in.task.Total)
		}
		if len(claimed) > 0 {
			risk("warn", "%s ticked by the model but not run by the stop guard on this version of the task file: %s.", plural(len(claimed), "1 check is", fmt.Sprintf("%d checks are", len(claimed))), list(claimed))
		}
		if len(observed) > 0 {
			risk("warn", "%s no command, so %s on the model's word: %s.", plural(len(observed), "1 ticked check has", fmt.Sprintf("%d ticked checks have", len(observed))), plural(len(observed), "it rests", "they rest"), list(observed))
		}
	}
	for _, r := range out.Changes {
		if n := len(r.Untracked); n > 0 {
			risk("warn", "%s in %s not tracked by git yet, easy to leave out of a commit: %s.", plural(n, "1 new file", fmt.Sprintf("%d new files", n)), r.Label, list(r.Untracked))
		}
	}
	for _, o := range d.Overlaps {
		risk("warn", "Session %s also has pending changes to %s.", o.With, list(o.Files))
	}
	if d.Held != nil {
		risk("warn", "A hook held it %d times: %s %s.", d.Held.Count, d.Held.Hook, strings.ReplaceAll(d.Held.Decision, "_", " "))
	}
	return out
}

func under(path string, dirs []string) bool {
	for _, d := range dirs {
		if rel, err := filepath.Rel(d, path); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// list names up to five things.
func list(xs []string) string {
	if len(xs) > 5 {
		return strings.Join(xs[:5], ", ") + fmt.Sprintf(" and %d more", len(xs)-5)
	}
	return strings.Join(xs, ", ")
}

// ReviewSheet builds the review sheet of a session on the rail.
func (a *App) ReviewSheet(name string) (ReviewDTO, error) {
	e, ok := a.railRow(name)
	if !ok {
		return ReviewDTO{}, fmt.Errorf("%s is not on the rail any more", name)
	}
	row := e.row
	in := reviewIn{dto: e.dto}
	sid := row.ClaudeSessionID
	in.task, in.hasTask = row.Task, row.HasTask
	if in.hasTask && a.launcher != nil {
		in.taskPath = filepath.Join(taskfile.Dir(a.launcher.ClaudeConfigDir), "tasks", sid+".md")
	}
	if a.hooks != nil && sid != "" {
		a.hooks.reader.Refresh()
		recs := a.hooks.reader.BySession(sid)
		var since time.Time
		if in.hasTask && in.task.StartedAt > 0 {
			since = time.Unix(in.task.StartedAt, 0)
		}
		start := len(recs)
		for i := range recs {
			if !recs[i].TS.Before(since) {
				start = i
				break
			}
		}
		in.guardRun = lastGuardRun(recs[start:])
	}
	if p := a.transcriptFor(row.Cwd, sid); p != "" {
		var since int64
		if in.hasTask {
			since = in.task.StartedAt
		}
		act, err := transcript.ScanActivity(p, since)
		if err != nil {
			return ReviewDTO{}, fmt.Errorf("reading the transcript: %w", err)
		}
		in.act = act
	}
	labels := a.targetLabels()
	in.dirs = sessionDirs(row.SessionRow)
	for _, dir := range in.dirs {
		r := gitdiff.WorkingTree(dir)
		r.Patch = ""
		r.Label = labels[dir]
		if r.Label == "" {
			r.Label = filepath.Base(dir)
		}
		in.repos = append(in.repos, r)
	}
	return buildReview(in), nil
}
