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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Task is the finish line a session pinned: its Status header, its Done-when
// checks counted, and the Outcome paragraph.
type Task struct {
	Status  string // open | done | consultation
	Done    int    // ticked Done-when checks
	Total   int    // all Done-when checks
	Outcome string // first paragraph of ## Outcome, whitespace-folded
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
		if m := headRe.FindStringSubmatch(line); m != nil && m[1] == "Status" {
			t.Status = strings.ToLower(strings.TrimSpace(m[2]))
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
		if strings.EqualFold(m[1], "x") {
			t.Done++
		}
	}
	t.Outcome = firstParagraph(sections["outcome"])
	switch t.Status {
	case "open", "done", "consultation":
	default:
		t.Status = "open" // what the hooks assume for an unlabelled file
	}
	return t, true
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
	return read(c.tf, sid, filepath.Join(c.dir, "tasks", sid+".md"), func(b []byte, _ os.FileInfo) (Task, bool) {
		return ParseTask(string(b))
	})
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
