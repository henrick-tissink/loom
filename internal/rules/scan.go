package rules

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The transcript side: tool calls that returned an error. A file is read
// twice at most: once for the lines that carry an error (a cheap byte
// test skips the rest), and, only when there were any, once more for the
// tool_use blocks that name the failing tools.

// Transcripts lists the session and subagent transcripts under root
// (<ccd>/projects) modified at or after since.
func Transcripts(root string, since time.Time) []string {
	var out []string
	for _, pat := range []string{"*/*.jsonl", "*/*/subagents/*.jsonl"} {
		paths, _ := filepath.Glob(filepath.Join(root, pat))
		for _, p := range paths {
			if fi, err := os.Stat(p); err == nil && !fi.ModTime().Before(since) {
				out = append(out, p)
			}
		}
	}
	return out
}

// SessionOf is the session a transcript belongs to: its own name, or for
// a subagent's file the session directory it sits under.
func SessionOf(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "subagents" {
		return filepath.Base(filepath.Dir(dir))
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

var (
	errNeedle = []byte(`"is_error":true`)
	useNeedle = []byte(`"tool_use"`)
)

type scanRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type scanBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// ScanErrors returns the tool errors in one transcript as events. Errors
// that are not the agent's own doing are left out (see Skip).
func ScanErrors(path string) ([]Event, error) {
	type pending struct {
		ev Event
		id string
	}
	var errs []pending
	err := eachLine(path, errNeedle, func(line []byte) {
		var r scanRecord
		if json.Unmarshal(line, &r) != nil || r.Type != "user" {
			return
		}
		var blocks []scanBlock
		if json.Unmarshal(r.Message.Content, &blocks) != nil {
			return
		}
		ts, _ := time.Parse(time.RFC3339Nano, r.Timestamp)
		for _, b := range blocks {
			if b.Type != "tool_result" || !b.IsError {
				continue
			}
			errs = append(errs, pending{id: b.ToolUseID, ev: Event{Source: "tool", Cwd: r.Cwd, TS: ts.Local(), Sample: resultText(b.Content)}})
		}
	})
	if err != nil || len(errs) == 0 {
		return nil, err
	}
	names := map[string]string{}
	for _, p := range errs {
		names[p.id] = ""
	}
	_ = eachLine(path, useNeedle, func(line []byte) {
		var r scanRecord
		if json.Unmarshal(line, &r) != nil || r.Type != "assistant" {
			return
		}
		var blocks []scanBlock
		if json.Unmarshal(r.Message.Content, &blocks) != nil {
			return
		}
		for _, b := range blocks {
			if _, want := names[b.ID]; want && b.Type == "tool_use" {
				names[b.ID] = b.Name
			}
		}
	})
	session := SessionOf(path)
	var out []Event
	for _, p := range errs {
		tool := names[p.id]
		if tool == "" {
			tool = "unknown"
		}
		msg := Normalize(p.ev.Sample)
		if msg == "" || Skip(tool, msg) {
			continue
		}
		sum := sha1.Sum([]byte(tool + "\x00" + msg))
		e := p.ev
		e.Key = "tool/" + tool + "/" + hex.EncodeToString(sum[:4])
		e.Title = tool + " failed: " + msg
		e.Session = session
		if len(e.Sample) > 400 {
			e.Sample = e.Sample[:400]
		}
		out = append(out, e)
	}
	return out, nil
}

// eachLine calls fn for every line of the file that contains needle.
func eachLine(path string, needle []byte, fn func([]byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, needle) {
			fn(line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// resultText is a tool_result's content: a string, or text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text + "\n")
			}
		}
		return b.String()
	}
	return ""
}

var (
	tagRe    = regexp.MustCompile(`</?tool_use_error>`)
	pathRe   = regexp.MustCompile(`(~|\.{0,2})/[^\s"'` + "`" + `:,)]+`)
	digitsRe = regexp.MustCompile(`\d+`)
)

// Normalize reduces an error to the line that identifies it: the first
// non-blank line, without paths and numbers, at most 100 characters, so
// the same failure on two files is one failure.
func Normalize(text string) string {
	text = tagRe.ReplaceAllString(text, "")
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln == "" {
			continue
		}
		ln = pathRe.ReplaceAllString(ln, "…")
		ln = digitsRe.ReplaceAllString(ln, "N")
		if r := []rune(ln); len(r) > 100 {
			ln = string(r[:100])
		}
		return ln
	}
	return ""
}

// Skip is true for errors no rule could prevent or that are counted
// elsewhere: a command's own non-zero exit (a failing test is work, not a
// mistake), the user declining a call, interruptions, the permission
// classifier, model outages, and the hooks' own denials (their records in
// the hook log are the evidence for those).
func Skip(tool, msg string) bool {
	l := strings.ToLower(msg)
	if tool == "Bash" && strings.HasPrefix(l, "exit code") {
		return true
	}
	for _, s := range []string{
		"doesn't want to proceed", "was rejected", "interrupted", "auto mode", "classifier",
		"temporarily unavailable", "pretooluse:", "hook error", "blocked:", "permission for this action",
	} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}
