// Package secondopinion runs Claude Code's own /code-review headless in a
// directory and reads its findings back. Loom writes no reviewer prompt: the
// native command decides how to review, and this package only launches it
// as a fresh, read-only run and parses what it prints.
//
// What `claude -p "/code-review <level>"` prints (2.1.325): the JSON
// envelope's `result` is prose. At low, one finding per line,
// "path/file.go:12 — what is wrong", or "(none)". At medium and high the
// review's own subagent returns a JSON array, but the result is re-rendered
// as numbered items, "1. `path/file.go:12`: summary", followed by an
// indented "Failure scenario: …" paragraph. Parse reads all three shapes
// (and a bare JSON array, should the result ever carry one) and keeps the
// text, so a shape it does not know is shown rather than lost.
package secondopinion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/memory"
)

// Levels are the /code-review effort levels loom offers. max and ultra
// start many reviewers (ultra in the cloud, billed); they stay a typed
// command.
var Levels = []string{"low", "medium", "high"}

// Timeout bounds one run.
const Timeout = 20 * time.Minute

// Finding is one thing the review reported.
type Finding struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"` // the failure scenario, when given
}

// Result is a finished review.
type Result struct {
	Findings []Finding `json:"findings"`
	// None: the review said it found nothing.
	None bool `json:"none"`
	// Text is the review's output as printed.
	Text       string  `json:"text"`
	CostUSD    float64 `json:"costUsd"`
	DurationMS int64   `json:"durationMs"`
}

// readOnlyTools is all the review may use: read files and read git. Any
// other tool is refused (a headless run has no one to ask).
var readOnlyTools = []string{"Read", "Grep", "Glob",
	"Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git status:*)",
	"Bash(git merge-base:*)", "Bash(git rev-parse:*)", "Bash(git branch:*)"}

// Args is the argv after the binary. --setting-sources "" keeps the user's
// hooks (a task-file prompt, a stop guard) out of the reviewer's run while
// keeping its login; --no-session-persistence keeps it off the session
// lists.
func Args(level string) []string {
	args := []string{"-p", "/code-review " + level,
		"--output-format", "json",
		"--no-session-persistence",
		"--setting-sources", "",
		"--allowedTools"}
	return append(args, readOnlyTools...)
}

// ValidLevel says whether level is one loom offers.
func ValidLevel(level string) bool {
	for _, l := range Levels {
		if l == level {
			return true
		}
	}
	return false
}

// Run reviews dir's changes (the native default scope: the branch against
// its upstream or main, plus uncommitted changes) at level.
func Run(ctx context.Context, binary, dir, level string) (Result, error) {
	if !ValidLevel(level) {
		return Result{}, fmt.Errorf("unknown review level %q", level)
	}
	if binary == "" {
		binary = "claude"
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, Args(level)...)
	cmd.Dir = dir
	cmd.Env = memory.ScrubEnv(os.Environ())
	cmd.Stdin = nil // /dev/null: -p otherwise waits 3 s for stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return Result{}, fmt.Errorf("the review ran past %s and was stopped", Timeout)
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	res, perr := ReadOutput(stdout.Bytes())
	if perr != nil {
		if err != nil {
			return Result{}, fmt.Errorf("%w: %s", err, firstLine(stderr.String()))
		}
		return Result{}, perr
	}
	return res, nil
}

// envelope is the part of `--output-format json` loom reads.
type envelope struct {
	Type       string  `json:"type"`
	Subtype    string  `json:"subtype"`
	IsError    bool    `json:"is_error"`
	Result     string  `json:"result"`
	CostUSD    float64 `json:"total_cost_usd"`
	DurationMS int64   `json:"duration_ms"`
}

// ReadOutput reads a run's stdout: the JSON envelope, then its result.
func ReadOutput(out []byte) (Result, error) {
	var e envelope
	if err := json.Unmarshal(bytes.TrimSpace(out), &e); err != nil {
		return Result{}, fmt.Errorf("the review's output is not claude's JSON: %s", firstLine(string(out)))
	}
	if e.IsError || (e.Subtype != "" && e.Subtype != "success") {
		msg := firstLine(e.Result)
		if msg == "" {
			msg = e.Subtype
		}
		return Result{}, fmt.Errorf("the review failed: %s", msg)
	}
	r := Parse(e.Result)
	r.CostUSD, r.DurationMS = e.CostUSD, e.DurationMS
	return r, nil
}

// head is a finding's first line: an optional list marker or bold, then
// path:line (backticked or not, a line range allowed), then a separator.
var head = regexp.MustCompile("^\\s*(?:\\d+[.)]\\s+|[-*•]\\s+)?(?:\\*\\*)?`?([^\\s`:*]+):(\\d+)(?:[-–]\\d+)?`?(?:\\*\\*)?\\s*(?:—|–|-|:)\\s*(.*)$")

// scenario is the label of a finding's failure-scenario paragraph.
var scenario = regexp.MustCompile(`(?i)^\s*(?:\*\*)?failure scenario(?:\*\*)?\s*:\s*(?:\*\*)?\s*`)

// Parse reads a review's printed findings.
func Parse(text string) Result {
	r := Result{Findings: []Finding{}, Text: strings.TrimSpace(text)}
	t := r.Text
	if t == "" || t == "(none)" || t == "[]" || t == "`(none)`" {
		r.None = true
		return r
	}
	if fs, ok := parseJSON(t); ok {
		r.Findings, r.None = fs, len(fs) == 0
		return r
	}
	var cur *Finding
	var detail []string
	flush := func() {
		if cur == nil {
			return
		}
		cur.Detail = strings.TrimSpace(strings.Join(detail, " "))
		r.Findings = append(r.Findings, *cur)
		cur, detail = nil, nil
	}
	blank := false
	for _, ln := range strings.Split(t, "\n") {
		indented := strings.HasPrefix(ln, " ") || strings.HasPrefix(ln, "\t")
		// inside a finding, an indented line is its detail even when it
		// starts with a path ("   internal/foo.go:40 - also calls it")
		if m := head.FindStringSubmatch(ln); m != nil && looksLikePath(m[1]) && (cur == nil || !indented) {
			flush()
			n, _ := strconv.Atoi(m[2])
			cur = &Finding{File: m[1], Line: n, Summary: strings.TrimSpace(m[3])}
			blank = false
			continue
		}
		if strings.TrimSpace(ln) == "" {
			blank = true
			continue
		}
		if cur == nil {
			continue
		}
		// A finding's detail is indented or labelled; flush prose after a
		// blank line ("Overall, …") is not part of it.
		if blank && !indented && !scenario.MatchString(ln) {
			flush()
			continue
		}
		blank = false
		detail = append(detail, strings.TrimSpace(scenario.ReplaceAllString(ln, "")))
	}
	flush()
	if len(r.Findings) == 0 && isNoneSentence(t) {
		r.None = true
	}
	return r
}

// looksLikePath keeps "Note:3 …" style text out: a path has a dot or a
// slash, or is a known extensionless file name.
func looksLikePath(p string) bool {
	if strings.ContainsAny(p, "./") {
		return true
	}
	switch p {
	case "Makefile", "Dockerfile", "Justfile", "Rakefile", "Gemfile", "Procfile":
		return true
	}
	return false
}

var noneSentence = regexp.MustCompile(`(?i)^(?:\(none\)|no (?:findings|issues|bugs)\b|nothing (?:found|to report)\b)`)

func isNoneSentence(t string) bool { return noneSentence.MatchString(strings.TrimSpace(t)) }

// fence is a fenced block, json-tagged or not.
var fence = regexp.MustCompile("(?s)```(?:json)?[ \\t]*\\n(.*?)\\n[ \\t]*```")

// parseJSON reads a findings array: the whole reply, the first fenced
// block that holds one (medium/high sometimes pass the array through, with
// a sentence before it), or a bare array starting on its own line.
func parseJSON(t string) ([]Finding, bool) {
	for _, m := range fence.FindAllStringSubmatch(t, -1) {
		if fs, ok := parseArray(m[1]); ok {
			return fs, true
		}
	}
	if fs, ok := parseArray(t); ok {
		return fs, true
	}
	if i := strings.Index(t, "\n["); i >= 0 {
		if j := strings.LastIndex(t, "]"); j > i {
			return parseArray(t[i+1 : j+1])
		}
	}
	return nil, false
}

func parseArray(s string) ([]Finding, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return nil, false
	}
	var raw []struct {
		File     string `json:"file"`
		Line     int    `json:"line"`
		Summary  string `json:"summary"`
		Scenario string `json:"failure_scenario"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, false
	}
	// An array quoted inside a finding (a JSON config, say) is not the
	// findings: every element must name a file or say something.
	out := make([]Finding, 0, len(raw))
	for _, f := range raw {
		if f.File == "" && f.Summary == "" {
			return nil, false
		}
		out = append(out, Finding{File: f.File, Line: f.Line, Summary: f.Summary, Detail: f.Scenario})
	}
	return out, true
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
