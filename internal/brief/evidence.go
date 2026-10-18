package brief

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// SeedMarker starts the seed of every session a brief launches ("Ask about
// this", "Review this"). A transcript whose first typed prompt carries it is
// a study session about earlier work, not work, and is left out of later
// briefs so a brief never reports its own reading as progress.
const SeedMarker = "[loom brief]"

// Entry kinds, in the order the writer is told to trust them.
const (
	KindTaskFile = "task-file" // a version of the session's finish-line task file
	KindCommand  = "cmd"       // a git / forge / test / build command with its result
	KindLava     = "lava"      // a coordination event (claim / done / finding)
	KindYou      = "you"       // what Henrick typed
	KindDelegate = "delegated" // the prompt a parent agent gave a subagent
	KindClaude   = "claude"    // what the agent said
	KindSummary  = "summary"   // a compaction summary
)

// Entry is one piece of evidence: a transcript line, cut to size.
type Entry struct {
	Line   int // 1-based line in the JSONL file: the evidence id's address
	At     time.Time
	Kind   string
	Text   string
	Anchor bool // never trimmed: task files, commands, lava events, summaries
}

// Session is one transcript's evidence inside the window.
type Session struct {
	ID        string // "S1", the evidence id prefix
	SessionID string
	File      string
	Subagent  bool
	Title     string
	Cwds      []string
	First     time.Time
	Last      time.Time
	Files     []string // files the session edited or wrote
	Entries   []Entry
	Trimmed   int // entries dropped by the budget
	firstAsk  string
	cwdSeen   map[string]bool
	fileSeen  map[string]bool
}

// scanOptions is what scanning needs from Options.
type scanOptions struct {
	projectsDir string
	since       time.Time
	until       time.Time
	inScope     func(cwd string) bool
}

// Caps on single entries, in characters.
const (
	capYou      = 2400
	capClaude   = 2400
	capTaskFile = 4000
	capCommand  = 400
	capResult   = 700
	capLava     = 600
	capSummary  = 3000
)

// excludedTyped are user-record texts that are not Henrick's words: slash
// command wrappers, harness notices, hand-backs from other agents.
var excludedTyped = []string{
	"<command-", "Caveat:", "[Request interrupted", "<local-command-stdout>",
	"<task-notification>", "<system-reminder>", "[SYSTEM NOTIFICATION",
	"Another Claude session sent a message", "<agent-message", "Stop hook feedback",
}

// notableCommand picks the Bash commands that are evidence of progress or
// proof: version control, forge, tests, builds, installs, coordination.
var notableCommand = regexp.MustCompile(`(?i)\b(git\s+(commit|push|merge|rebase|cherry-pick|tag|revert|reset|switch|checkout|worktree\s+add|stash)|gh\s+(pr|release|run|workflow)|glab\s+(mr|release|ci)|go\s+(test|build|vet)|npm\s+(run\s+)?(test|build)|pnpm\s+(run\s+)?(test|build)|yarn\s+(test|build)|pytest|cargo\s+(test|build)|vitest|jest|playwright|make\s+[\w-]+|xcodebuild|flutter\s+test|terraform\s+(plan|apply)|kubectl\s+(apply|rollout)|docker\s+(build|push)|lava\s+(intent|emit|done))\b`)

type rawRecord struct {
	Type             string          `json:"type"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	IsSidechain      bool            `json:"isSidechain"`
	AiTitle          string          `json:"aiTitle"`
	Cwd              string          `json:"cwd"`
	Timestamp        string          `json:"timestamp"`
	SessionID        string          `json:"sessionId"`
	Message          *rawMessage     `json:"message"`
	Summary          string          `json:"summary"`
	ToolUseResult    json.RawMessage `json:"toolUseResult"`
}

type rawMessage struct {
	Content json.RawMessage `json:"content"`
}

type rawBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// scanTranscripts walks every transcript (main and subagent) that could hold
// records inside the window and returns the sessions with evidence in scope,
// oldest activity first. A file's mtime is when its last record was written,
// so a file last touched before `since` cannot hold a record in the window.
func scanTranscripts(o scanOptions) ([]*Session, error) {
	var files []string
	dirs, err := os.ReadDir(o.projectsDir)
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		base := filepath.Join(o.projectsDir, d.Name())
		mains, _ := filepath.Glob(filepath.Join(base, "*.jsonl"))
		subs, _ := filepath.Glob(filepath.Join(base, "*", "subagents", "*.jsonl"))
		for _, f := range append(mains, subs...) {
			if fi, err := os.Stat(f); err == nil && !fi.ModTime().Before(o.since) {
				files = append(files, f)
			}
		}
	}
	var out []*Session
	for _, f := range files {
		s, err := scanFile(f, o)
		if err != nil || s == nil {
			continue // an unreadable transcript is skipped, never fatal
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].First.Before(out[j].First) })
	return out, nil
}

// scanFile reads one JSONL file. It returns nil when the file has no
// in-scope evidence inside the window, or is a brief's own study session.
func scanFile(path string, o scanOptions) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := &Session{
		File:      path,
		SessionID: strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		Subagent:  strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)),
		cwdSeen:   map[string]bool{},
		fileSeen:  map[string]bool{},
	}
	pending := map[string]int{} // tool_use id -> index in s.Entries awaiting its result
	lastCwd := ""
	firstTyped := true
	r := bufio.NewReader(f)
	for lineNo := 1; ; lineNo++ {
		line, rerr := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var rec rawRecord
			if json.Unmarshal(line, &rec) == nil {
				if rec.Cwd != "" {
					lastCwd = rec.Cwd
				}
				if rec.AiTitle != "" {
					s.Title = rec.AiTitle
				}
				if rec.Type == "summary" && rec.Summary != "" && s.Title == "" {
					s.Title = rec.Summary
				}
				// The study-session check runs on the first typed prompt
				// wherever it falls, before the window and scope filters.
				if firstTyped && rec.Type == "user" && !rec.IsMeta && rec.Message != nil {
					if t, ok := typedText(rec.Message.Content); ok {
						firstTyped = false
						if strings.Contains(t, SeedMarker) {
							return nil, nil
						}
					}
				}
				at, ok := parseTime(rec.Timestamp)
				if ok && !at.Before(o.since) && !at.After(o.until) && o.inScope(lastCwd) {
					s.take(rec, lineNo, at, lastCwd, pending)
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if len(s.Entries) == 0 {
		return nil, nil
	}
	if s.Title == "" && s.firstAsk != "" {
		s.Title = safePrefix(oneLine(s.firstAsk), 90)
		if s.Subagent {
			s.Title = "subagent: " + s.Title
		}
	}
	return s, nil
}

func (s *Session) take(rec rawRecord, line int, at time.Time, cwd string, pending map[string]int) {
	if s.First.IsZero() || at.Before(s.First) {
		s.First = at
	}
	if at.After(s.Last) {
		s.Last = at
	}
	if cwd != "" && !s.cwdSeen[cwd] {
		s.cwdSeen[cwd] = true
		s.Cwds = append(s.Cwds, cwd)
	}
	if rec.Message == nil {
		return
	}
	switch rec.Type {
	case "user":
		if rec.IsCompactSummary {
			if t, ok := typedText(rec.Message.Content); ok {
				s.add(Entry{Line: line, At: at, Kind: KindSummary, Text: cut(t, capSummary), Anchor: true})
			}
			return
		}
		s.takeResults(rec.Message.Content, pending)
		if rec.IsMeta {
			return
		}
		if t, ok := typedText(rec.Message.Content); ok && !excluded(t) {
			kind := KindYou
			if s.Subagent {
				kind = KindDelegate // a subagent's "user" is the agent that spawned it
			}
			if s.firstAsk == "" {
				s.firstAsk = t
			}
			s.add(Entry{Line: line, At: at, Kind: kind, Text: cut(t, capYou)})
		}
	case "assistant":
		var blocks []rawBlock
		if json.Unmarshal(rec.Message.Content, &blocks) != nil {
			return
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					s.add(Entry{Line: line, At: at, Kind: KindClaude, Text: cut(t, capClaude)})
				}
			case "tool_use":
				s.takeToolUse(b, line, at, pending)
			}
		}
	}
}

func (s *Session) add(e Entry) { s.Entries = append(s.Entries, e) }

// takeToolUse keeps the tool calls that are evidence: task-file versions,
// notable shell commands (their result is attached when it arrives), lava
// events, and the paths of edited files.
func (s *Session) takeToolUse(b rawBlock, line int, at time.Time, pending map[string]int) {
	var in struct {
		FilePath  string `json:"file_path"`
		Content   string `json:"content"`
		NewString string `json:"new_string"`
		Command   string `json:"command"`
	}
	_ = json.Unmarshal(b.Input, &in)
	switch {
	case b.Name == "Write" || b.Name == "Edit" || b.Name == "MultiEdit" || b.Name == "NotebookEdit":
		if strings.Contains(in.FilePath, "/.claude/state/tasks/") {
			body := in.Content
			label := "task file written"
			if b.Name != "Write" {
				body, label = in.NewString, "task file edited"
			}
			s.add(Entry{Line: line, At: at, Kind: KindTaskFile, Text: label + ":\n" + cut(body, capTaskFile), Anchor: true})
			return
		}
		if in.FilePath != "" && !s.fileSeen[in.FilePath] {
			s.fileSeen[in.FilePath] = true
			s.Files = append(s.Files, in.FilePath)
		}
	case b.Name == "Bash":
		if notableCommand.MatchString(in.Command) {
			s.add(Entry{Line: line, At: at, Kind: KindCommand, Text: "$ " + cut(in.Command, capCommand), Anchor: true})
			if b.ID != "" {
				pending[b.ID] = len(s.Entries) - 1
			}
		}
	case strings.HasPrefix(b.Name, "mcp__lava__lava_emit"):
		s.add(Entry{Line: line, At: at, Kind: KindLava, Text: cut(compactJSON(b.Input), capLava), Anchor: true})
	}
}

// takeResults attaches a pending command's result tail (the end of the
// output carries the verdict: test counts, the pushed ref, the PR URL).
func (s *Session) takeResults(content json.RawMessage, pending map[string]int) {
	var blocks []rawBlock
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		i, ok := pending[b.ToolUseID]
		if !ok {
			continue
		}
		delete(pending, b.ToolUseID)
		out := strings.TrimSpace(resultText(b.Content))
		status := "ok"
		if b.IsError {
			status = "error"
		}
		s.Entries[i].Text += "\n→ (" + status + ") " + tail(out, capResult)
	}
}

// typedText returns the human-typed text of a user record's content: a bare
// string, or its text blocks. A record that only carries tool results is not
// typed text.
func typedText(content json.RawMessage) (string, bool) {
	var str string
	if json.Unmarshal(content, &str) == nil {
		str = strings.TrimSpace(str)
		return str, str != ""
	}
	var blocks []rawBlock
	if json.Unmarshal(content, &blocks) != nil {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

func resultText(content json.RawMessage) string {
	var str string
	if json.Unmarshal(content, &str) == nil {
		return str
	}
	var blocks []rawBlock
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func excluded(t string) bool {
	for _, p := range excludedTyped {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

// cut keeps the head and tail of s within n characters (UTF-8 safe): the
// start of a message says what it is about, the end what it concluded.
func cut(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	head := n * 2 / 3
	return safePrefix(s, head) + "\n[…]\n" + safeSuffix(s, n-head)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "[…]" + safeSuffix(s, n)
}

func safePrefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func safeSuffix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
