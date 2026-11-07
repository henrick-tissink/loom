package transcript

import (
	"encoding/json"
	"hash/fnv"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Two things the classifier keeps beside the turn state: the tool calls
// that keep failing the same way within one turn (a session "spinning":
// retrying what already failed), and the files the session has edited
// through its file tools (for the rail's overlap check).
//
// A turn is everything under one promptId. claude mints one per prompt (a
// person's message, a task notification), and the turn's tool results,
// stop-hook feedback and compaction summary all carry it, so a /goal run
// or a stop guard sending the agent back is one turn. A transcript
// without promptIds falls back to: a user record that is not a tool
// result and not meta starts a turn.

// SpinThreshold is how many times the same failure must repeat within a
// turn before the session counts as spinning. Three is where "trying it
// again" stops being diagnosis.
const SpinThreshold = 3

// spinRecent is how many tool results a spin stays shown after its last
// hit: an agent that has moved on to other work is no longer stuck.
const spinRecent = 8

const (
	sampleLimit  = 160
	subjectLimit = 200
	normLimit    = 4000
)

// Spin is the turn's most repeated tool failure, once it has repeated
// SpinThreshold times and was hit within the last few tool results.
type Spin struct {
	Tool    string // the failing tool
	Count   int    // times it failed this way since the turn began
	Sample  string // the failure's telling line, as claude saw it
	Subject string // the last failing call's command or file
	First   int64  // unix seconds of the first and the latest hit
	Last    int64
}

type toolCall struct {
	name string
	// subject is what a later success is matched on: a Bash command
	// (whitespace collapsed) or a file tool's path; "" for other tools.
	subject string
	// path is the file an edit tool writes, absolute; "" otherwise.
	path string
}

type failure struct {
	spin     Spin
	seq      int // c.results at its latest hit
	subjects map[string]bool
}

var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

type toolInput struct {
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
}

func callOf(b block, cwd string) toolCall {
	call := toolCall{name: b.Name}
	var in toolInput
	_ = json.Unmarshal(b.Input, &in)
	switch {
	case b.Name == "Bash":
		call.subject = strings.Join(strings.Fields(in.Command), " ")
	case in.FilePath != "" || in.NotebookPath != "":
		p := in.FilePath
		if p == "" {
			p = in.NotebookPath
		}
		if !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		call.subject = filepath.Clean(p)
		if editTools[b.Name] && filepath.IsAbs(p) {
			call.path = call.subject
		}
	}
	return call
}

// noteCalls remembers an assistant record's tool calls until their
// results arrive.
func (c *Classifier) noteCalls(blocks []block, cwd string) {
	for _, b := range blocks {
		if b.Type != "tool_use" || b.ID == "" {
			continue
		}
		if c.calls == nil {
			c.calls = map[string]toolCall{}
		}
		c.calls[b.ID] = callOf(b, cwd)
	}
}

// noteTurn starts a new turn's counts when a user record opens one.
func (c *Classifier) noteTurn(r record, blocks []block) {
	if r.PromptID != "" {
		if r.PromptID != c.turn {
			c.turn = r.PromptID
			c.resetTurn()
		}
		return
	}
	if _, ok := findBlock(blocks, "tool_result"); !ok && !r.IsMeta {
		c.turn = ""
		c.resetTurn()
	}
}

func (c *Classifier) resetTurn() {
	c.results = 0
	c.calls = nil
	c.fails = nil
}

// noteResults folds a user record's tool results: a failure counts toward
// its key, a success clears the failures of the same command or file and
// records an edit.
func (c *Classifier) noteResults(blocks []block, at int64) {
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		c.results++
		call, ok := c.calls[b.ToolUseID]
		if !ok {
			continue // its call is not in this transcript: nothing to key on
		}
		delete(c.calls, b.ToolUseID)
		if b.IsError {
			c.failed(call, contentText(b.Content), at)
			continue
		}
		if call.subject != "" {
			for k, f := range c.fails {
				if f.subjects[call.subject] {
					delete(c.fails, k)
				}
			}
		}
		if call.path != "" {
			if c.edited == nil {
				c.edited = map[string]int64{}
			}
			c.edited[call.path] = at
			c.editedDirty = true
		}
	}
}

func (c *Classifier) failed(call toolCall, text string, at int64) {
	key, sample, ok := failureKey(call, text)
	if !ok {
		return
	}
	if c.fails == nil {
		c.fails = map[string]*failure{}
	}
	f := c.fails[key]
	if f == nil {
		f = &failure{spin: Spin{Tool: call.name, First: at}, subjects: map[string]bool{}}
		c.fails[key] = f
	}
	f.spin.Count++
	f.spin.Last = at
	f.spin.Sample = sample
	f.spin.Subject = cut(call.subject, subjectLimit)
	f.seq = c.results
	if call.subject != "" {
		f.subjects[call.subject] = true
	}
}

// Spin is the turn's most repeated recent failure, or nil when nothing
// has repeated SpinThreshold times or the agent has since moved on.
func (c *Classifier) Spin() *Spin {
	var best *failure
	for _, f := range c.fails {
		if f.spin.Count < SpinThreshold || c.results-f.seq >= spinRecent {
			continue
		}
		if best == nil || f.spin.Count > best.spin.Count ||
			(f.spin.Count == best.spin.Count && (f.spin.Last > best.spin.Last ||
				(f.spin.Last == best.spin.Last && f.spin.Sample < best.spin.Sample))) {
			best = f
		}
	}
	if best == nil {
		return nil
	}
	s := best.spin
	return &s
}

// Edit is a file the session wrote and when it last did (unix seconds;
// 0 = unknown).
type Edit struct {
	Path string
	At   int64
}

// Edited lists the absolute paths the session has written through its
// file tools (Edit, Write, MultiEdit, NotebookEdit) with a successful
// result, sorted by path. Edits made through Bash are not seen.
func (c *Classifier) Edited() []Edit {
	if c.editedDirty {
		c.editedList = make([]Edit, 0, len(c.edited))
		for p, at := range c.edited {
			c.editedList = append(c.editedList, Edit{Path: p, At: at})
		}
		sort.Slice(c.editedList, func(i, j int) bool { return c.editedList[i].Path < c.editedList[j].Path })
		c.editedDirty = false
	}
	return c.editedList
}

var (
	toolErrTag = regexp.MustCompile(`</?tool_use_error>`)
	exitLine   = regexp.MustCompile(`^Exit code (\d+)$`)
	hexRun     = regexp.MustCompile(`\b[0-9a-fA-F]{7,}\b`)
	digitRun   = regexp.MustCompile(`\d+`)
	errorish   = regexp.MustCompile(`(?i)error|fail|panic|exception|traceback|cannot|can't|could not|couldn't|undefined|not found|no such|denied|refused|fatal|invalid|unexpected|missing`)
)

// notTheAgents marks failures that are not a retry loop: the person
// declining or interrupting a call, a hook's hold (the held chip counts
// those), the auto-mode classifier denying a call (its text is the same
// whatever the command), and a model outage.
var notTheAgents = []string{
	"doesn't want to proceed", "was rejected", "interrupted by user",
	"pretooluse:", "hook error", "temporarily unavailable", "gave no verdict",
	"auto mode classifier", "permission for this action was denied",
}

// failureKey reduces a failed call to what makes two failures "the same":
// the tool, a file tool's file, and the error text with numbers, hashes
// and spacing evened out. A Bash failure keys on its output, not its
// command, so a different command hitting the same error counts; with no
// output to go on it keys on the command and exit code. sample is the
// line that tells the failure, for the row.
func failureKey(call toolCall, text string) (key, sample string, ok bool) {
	text = strings.TrimSpace(toolErrTag.ReplaceAllString(text, ""))
	low := strings.ToLower(text)
	for _, s := range notTheAgents {
		if strings.Contains(low, s) {
			return "", "", false
		}
	}
	if strings.HasPrefix(low, "blocked:") {
		return "", "", false
	}
	body, exit := text, ""
	if call.name == "Bash" {
		first, rest, _ := strings.Cut(text, "\n")
		if m := exitLine.FindStringSubmatch(strings.TrimSpace(first)); m != nil {
			body, exit = rest, m[1]
		}
	}
	norm := normalize(body)
	h := fnv.New64a()
	h.Write([]byte(call.name))
	h.Write([]byte{0})
	switch {
	case call.name != "Bash":
		h.Write([]byte(call.subject))
		h.Write([]byte{0})
		h.Write([]byte(norm))
	case len(norm) >= 3:
		h.Write([]byte(norm))
	default:
		h.Write([]byte("cmd\x00" + normalize(call.subject) + "\x00" + exit))
	}
	sample = tellingLine(body)
	if exit != "" {
		if sample == "" {
			sample = "exit " + exit
		} else {
			sample = "exit " + exit + " · " + sample
		}
	}
	return strconv.FormatUint(h.Sum64(), 16), cut(sample, sampleLimit), true
}

func normalize(s string) string {
	s = hexRun.ReplaceAllString(s, "H")
	s = digitRun.ReplaceAllString(s, "N")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > normLimit {
		s = s[:normLimit]
	}
	return s
}

// tellingLine is the first line that reads as an error, else the first
// non-blank one.
func tellingLine(s string) string {
	first := ""
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln == "" {
			continue
		}
		if first == "" {
			first = ln
		}
		if errorish.MatchString(ln) {
			return ln
		}
	}
	return first
}

// contentText is a tool_result's content: a string, or its text blocks.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
