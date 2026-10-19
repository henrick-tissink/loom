// Package taskfile reads the per-session state the operator's Claude Code
// hooks write under <claude config dir>/state: the task file a session pins
// its finish line in, and the checkpoint it writes before a hand-over.
//
// Loom does not own these files and never writes them. The shape is the
// hooks' contract (hooks/enforce/common.py, TASK_SHAPE and parse_task); this
// reader is tolerant in the same places that parser is, and a file that
// does not parse simply contributes nothing.
package taskfile

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Task is the finish line a session pinned: its Status header, its Done-when
// checks counted, and the Outcome paragraph.
type Task struct {
	Status  string // open | done | consultation
	Done    int    // ticked Done-when checks
	Total   int    // all Done-when checks
	Outcome string // first paragraph of ## Outcome, whitespace-folded
	// Checks are the Done-when checks' text as written (without the box),
	// and Open says which of them are not ticked yet.
	Checks []string
	Open   []bool
	// StartedAt is the Started: header as unix seconds (0 when absent or
	// unparsable). A new task file in the same session has a new start,
	// which is what "the current task file version" means to the hooks
	// panel and the held chip.
	StartedAt int64
	// Modified is the file's mtime as unix seconds: a verdict the hooks
	// logged before it was about an older version of the file.
	Modified int64
	// Verified is true when the stop guard's marker beside the file
	// (<file>.verified) is at least as new as the file: it ran the `$`
	// checks on this version and they passed. Set by Cache.Task only.
	Verified bool
}

// Checkpoint is a hand-over note: when it was written (file mtime) and its
// one-line next step.
type Checkpoint struct {
	WrittenAt int64 // unix seconds; 0 when there is no checkpoint
	NextStep  string
}

// Dir is the hooks' state directory: $CLAUDE_STATE_DIR, else
// <claudeConfigDir>/state.
func Dir(claudeConfigDir string) string {
	if d := os.Getenv("CLAUDE_STATE_DIR"); d != "" {
		return d
	}
	return filepath.Join(claudeConfigDir, "state")
}

var (
	headRe  = regexp.MustCompile(`^(Session|Started|Cwd|Status):\s*(.*)$`)
	sectRe  = regexp.MustCompile(`^##\s+(.+?)\s*$`)
	checkRe = regexp.MustCompile(`^\s*[-*]\s*\[([ xX])\]\s*(.*?)\s*$`)
)

// ParseTask parses a task file's text. ok is false for an empty file or one
// with no sections at all, which is how a placeholder or a stray file reads.
func ParseTask(text string) (t Task, ok bool) {
	if strings.TrimSpace(text) == "" {
		return Task{}, false
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if i >= 12 {
			break
		}
		if m := headRe.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "Status":
				t.Status = strings.ToLower(strings.TrimSpace(m[2]))
			case "Started":
				t.StartedAt = parseStarted(strings.TrimSpace(m[2]))
			}
		}
	}
	sections := map[string][]string{}
	name := ""
	for _, line := range lines {
		if m := sectRe.FindStringSubmatch(line); m != nil {
			name = strings.ToLower(strings.TrimSpace(m[1]))
			sections[name] = nil
			continue
		}
		if name != "" {
			sections[name] = append(sections[name], line)
		}
	}
	if len(sections) == 0 {
		return Task{}, false
	}
	for _, line := range sections["done when"] {
		m := checkRe.FindStringSubmatch(line)
		if m == nil || strings.HasPrefix(m[2], "<") {
			continue // not a check, or the template placeholder
		}
		t.Total++
		done := strings.EqualFold(m[1], "x")
		if done {
			t.Done++
		}
		t.Checks = append(t.Checks, m[2])
		t.Open = append(t.Open, !done)
	}
	t.Outcome = firstParagraph(sections["outcome"])
	switch t.Status {
	case "open", "done", "consultation":
	default:
		t.Status = "open" // what the hooks assume for an unlabelled file
	}
	return t, true
}

// parseStarted reads the hooks' "2006-01-02 15:04" (local) header, or the
// RFC 3339 form; 0 for anything else.
func parseStarted(s string) int64 {
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// firstParagraph folds the first run of non-blank lines into one line,
// skipping the template's "(…)" placeholder.
func firstParagraph(lines []string) string {
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			if len(out) > 0 {
				break
			}
			continue
		}
		out = append(out, l)
	}
	s := strings.Join(out, " ")
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		return ""
	}
	return s
}

// ParseCheckpoint pulls the "## Next step" section's first paragraph.
func ParseCheckpoint(text string) string {
	sc := bufio.NewScanner(strings.NewReader(text))
	in := false
	var body []string
	for sc.Scan() {
		line := sc.Text()
		if m := sectRe.FindStringSubmatch(line); m != nil {
			if in {
				break
			}
			in = strings.EqualFold(strings.TrimSpace(m[1]), "next step")
			continue
		}
		if in {
			body = append(body, line)
		}
	}
	return firstParagraph(body)
}

// Cache reads task files and checkpoints by claude session id, re-parsing a
// file only when its size or mtime changes. The engine asks for every live
// row on every poll, so the steady state is one stat per file per poll.
type Cache struct {
	dir string
	mu  sync.Mutex
	tf  map[string]entry[Task]
	cp  map[string]entry[Checkpoint]
}

type entry[T any] struct {
	size  int64
	mtime int64
	val   T
	ok    bool
}

func NewCache(stateDir string) *Cache {
	return &Cache{dir: stateDir, tf: map[string]entry[Task]{}, cp: map[string]entry[Checkpoint]{}}
}

// Task returns the session's task file, if it has one.
func (c *Cache) Task(sid string) (Task, bool) {
	if c == nil || sid == "" {
		return Task{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	path := filepath.Join(c.dir, "tasks", sid+".md")
	t, ok := read(c.tf, sid, path, func(b []byte, fi os.FileInfo) (Task, bool) {
		t, ok := ParseTask(string(b))
		t.Modified = fi.ModTime().Unix()
		return t, ok
	})
	if ok {
		// The marker lands after the file's last edit without touching the
		// file, so it is checked on every call rather than cached with it.
		t.Verified = verified(path)
	}
	return t, ok
}

// verified reports whether <path>.verified holds an mtime (the hooks write
// repr(st_mtime)) at least as new as the file itself.
func verified(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	b, err := os.ReadFile(path + ".verified")
	if err != nil {
		return false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return false
	}
	// Python's st_mtime is a float with sub-second precision; allow for
	// the rounding on either side.
	return v+1e-3 >= float64(fi.ModTime().UnixNano())/1e9
}

// Checkpoint returns the session's model-written checkpoint, if any. The
// hooks' mechanical snapshot (<sid>.mechanical.md) is not a hand-over note
// and is ignored.
func (c *Cache) Checkpoint(sid string) (Checkpoint, bool) {
	if c == nil || sid == "" {
		return Checkpoint{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return read(c.cp, sid, filepath.Join(c.dir, "checkpoints", sid+".md"), func(b []byte, fi os.FileInfo) (Checkpoint, bool) {
		return Checkpoint{WrittenAt: fi.ModTime().Unix(), NextStep: ParseCheckpoint(string(b))}, true
	})
}

func read[T any](m map[string]entry[T], sid, path string, parse func([]byte, os.FileInfo) (T, bool)) (T, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		delete(m, sid)
		var zero T
		return zero, false
	}
	if e, hit := m[sid]; hit && e.size == fi.Size() && e.mtime == fi.ModTime().UnixNano() {
		return e.val, e.ok
	}
	b, err := os.ReadFile(path)
	if err != nil {
		var zero T
		return zero, false
	}
	val, ok := parse(b, fi)
	m[sid] = entry[T]{size: fi.Size(), mtime: fi.ModTime().UnixNano(), val: val, ok: ok}
	return val, ok
}

// GoalTurns is the default turn cap of a goal: under claude's limit of 9
// consecutive stop-hook blocks in one turn, so the cap is what ends it.
const GoalTurns = 8

// GoalMaxLen is the longest message claude still reads as a slash command
// when it arrives as one paste: longer, it becomes pasted text and /goal is
// never run (claude 2.1.303: 800 sets a goal, 801 does not).
const GoalMaxLen = 800

// GoalCondition is a /goal condition for the task's finish line: its
// Done-when checks verbatim (a ticked check's evidence after "#" is
// dropped, the check itself is kept) plus a turn cap. The goal's evaluator
// reads the transcript and runs nothing, so the condition asks for each
// check's command and output to be shown there. One line, naming the file:
// "the task file" alone left a live claude guessing which one. When the
// verbatim form would make "/goal …" longer than GoalMaxLen, the checks are
// named only, to be run as the file writes them; ok is false when even that
// does not fit.
func GoalCondition(t Task, path string, turns int) (cond string, ok bool) {
	checks := make([]string, len(t.Checks))
	names := make([]string, len(t.Checks))
	for i, c := range t.Checks {
		if !t.Open[i] {
			if j := strings.LastIndex(c, " # "); j >= 0 {
				c = strings.TrimSpace(c[:j])
			}
		}
		c = strings.TrimSpace(strings.TrimSuffix(c, " #")) // an empty evidence slot
		checks[i] = fmt.Sprintf("(%d) %s", i+1, c)
		name, _, _ := strings.Cut(c, ":")
		names[i] = fmt.Sprintf("(%d) %s", i+1, strings.TrimSpace(name))
	}
	tail := fmt.Sprintf("; and that file says Status: done, or stop after %d turns", turns)
	cond = fmt.Sprintf("every Done-when check of the task file %s holds, each shown in this transcript by its command and that command's output (or, for a check with no command, the observation it names): %s%s",
		path, strings.Join(checks, "; "), tail)
	if len("/goal ")+len(cond) <= GoalMaxLen {
		return cond, true
	}
	cond = fmt.Sprintf("every Done-when check of the task file %s holds, each shown in this transcript by running its command exactly as that file writes it, with the output (or the observation it names): %s%s",
		path, strings.Join(names, "; "), tail)
	return cond, len("/goal ")+len(cond) <= GoalMaxLen
}
