// Package memory implements Phase 2 Memory's extraction half (spec
// docs/superpowers/specs/2026-07-03-memory-design.md §2): turning one
// claude JSONL transcript file into filtered, control-stripped index docs
// plus the raw materials for L2 distillation (ask/outcome/title/files/
// cwds/timestamps/msg_count). Attribution across parent+subagent files and
// incremental indexing belong to the indexer (Task 4) — this file only
// ever looks at one file at a time.
package memory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

// Extraction is everything ExtractFile pulls out of one JSONL transcript
// file. Docs use role "user"/"assistant" for main files, or roleOverride
// ("agent") for subagent files. Ask/Outcome/Title are only ever populated
// from main files (roleOverride == "") — subagent text still passes the
// same filters and lands in Docs/Files, but per spec never feeds ask/
// outcome/title; attribution/merging across files is the indexer's job.
type Extraction struct {
	Docs     []store.Doc
	Title    string
	Ask      string
	Outcome  string
	Files    []string // distinct, ordered
	Cwds     []string // all distinct cwds seen, in order
	FirstTS  int64
	LastTS   int64
	MsgCount int
}

// record is the subset of a claude JSONL line's fields the extractor
// cares about. Deliberately richer than transcript.record (which only
// needs turn-boundary state): distillation also needs isMeta,
// isCompactSummary, cwd and timestamp. toolUseResult is intentionally not
// modeled here — never read.
type record struct {
	Type             string   `json:"type"`
	IsSidechain      bool     `json:"isSidechain"`
	IsMeta           bool     `json:"isMeta"`
	IsCompactSummary bool     `json:"isCompactSummary"`
	AiTitle          string   `json:"aiTitle"`
	Cwd              string   `json:"cwd"`
	Timestamp        string   `json:"timestamp"`
	Message          *message `json:"message"`
}

type message struct {
	Content json.RawMessage `json:"content"`
}

// contentBlock covers both user content blocks (text, tool_result) and
// assistant content blocks (text, thinking, tool_use) — the union of
// fields either shape needs.
type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// toolFileInput covers the two tool_use input shapes that name a touched
// file (spec §2.3): file_path (Edit/Write/MultiEdit) and notebook_path
// (NotebookEdit).
type toolFileInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
}

var wsRunRe = regexp.MustCompile(`[\n\r\t]+`)

// CleanText strips text for indexing/ask/outcome (spec §2.2, binding, and
// applied to ALL indexed text and ask/outcome): collapse any run of
// \n\r\t to a single space, then drop every remaining C0 control char
// (< 0x20). This protects the FTS snippet's \x01/\x02 highlight markers
// from being spoofed by transcript content.
func CleanText(s string) string {
	s = wsRunRe.ReplaceAllString(s, " ")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// excludedPrefixes: user text starting with any of these is neither
// indexed nor eligible as "ask" (spec §2 filter table rows 1-3): slash
// command wrappers, the Caveat: preamble, an interrupted-request marker,
// and local-command stdout wrapper.
var excludedPrefixes = []string{
	"<command-",
	"Caveat:",
	"[Request interrupted",
	"<local-command-stdout>",
}

func hasExcludedPrefix(s string) bool {
	for _, p := range excludedPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// ExtractFile streams one JSONL transcript file with bufio.Reader.
// ReadBytes('\n') — never bufio.Scanner, whose default 64KB line buffer
// silently drops real transcript lines that reach 2.8MB+ — and returns
// its filtered docs plus distillation materials. Malformed lines are
// skipped; the file is never aborted. The final line is processed even
// without a trailing newline.
//
// roleOverride is "" for main session files (role stays "user"/
// "assistant") or "agent" for subagent files (both roles become "agent").
func ExtractFile(path string, roleOverride string) (Extraction, error) {
	f, err := os.Open(path)
	if err != nil {
		return Extraction{}, err
	}
	defer f.Close()

	st := &extractState{isMain: roleOverride == "", roleOverride: roleOverride}
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			st.feed(line)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return st.result(), rerr
		}
	}
	return st.result(), nil
}

// extractState accumulates one file's Extraction across feed() calls.
type extractState struct {
	roleOverride string
	isMain       bool

	out Extraction

	cwdSeen    map[string]bool
	fileSeen   map[string]bool
	sawFirstTS bool
}

func (e *extractState) result() Extraction {
	e.out.MsgCount = len(e.out.Docs)
	return e.out
}

func (e *extractState) feed(line []byte) {
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		return // partial/garbage line: skip, never abort the file
	}

	if r.Type == "ai-title" {
		// Sidecar: never a turn boundary. Title only from main files, and
		// only when not itself a sidechain record (mirrors classify.go).
		if e.isMain && !r.IsSidechain && r.AiTitle != "" {
			e.out.Title = r.AiTitle
		}
		return
	}

	// spec decision 1: the isSidechain exclusion applies ONLY inside main
	// session files (subagent files are themselves isSidechain — that's
	// expected, not an exclusion signal).
	if e.isMain && r.IsSidechain {
		return
	}

	if r.Type != "user" && r.Type != "assistant" {
		return // other sidecar record types: mode, permission-mode, etc.
	}

	ts, hasTS := parseTS(r.Timestamp)
	if hasTS {
		if !e.sawFirstTS {
			e.out.FirstTS = ts
			e.sawFirstTS = true
		}
		if ts > e.out.LastTS {
			e.out.LastTS = ts
		}
	}

	if r.Cwd != "" {
		if e.cwdSeen == nil {
			e.cwdSeen = map[string]bool{}
		}
		if !e.cwdSeen[r.Cwd] {
			e.cwdSeen[r.Cwd] = true
			e.out.Cwds = append(e.out.Cwds, r.Cwd)
		}
	}

	if r.Message == nil {
		return
	}

	role := r.Type
	if e.roleOverride != "" {
		role = e.roleOverride
	}

	switch r.Type {
	case "user":
		e.feedUser(r, role, ts)
	case "assistant":
		e.feedAssistant(r, role, ts)
	}
}

// parseTS parses a claude transcript timestamp (RFC3339, tolerating
// fractional-second millis — time.Parse(time.RFC3339, ...) accepts an
// optional fractional second even though the layout doesn't spell one
// out). Failure (including empty string) → ok=false; caller leaves ts
// at its zero value for that record.
func parseTS(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

func (e *extractState) feedUser(r record, role string, ts int64) {
	if r.IsMeta {
		return
	}
	text, ok := extractUserText(r.Message.Content)
	if !ok {
		return
	}
	if hasExcludedPrefix(text) {
		return
	}
	clean := CleanText(text)
	if clean == "" {
		return
	}
	e.out.Docs = append(e.out.Docs, store.Doc{Content: clean, Role: role, TS: ts})
	if e.isMain && !r.IsCompactSummary && e.out.Ask == "" {
		e.out.Ask = clean
	}
}

// extractUserText implements filter-table rows 1/2/4: string content
// passes through as-is; list content containing any tool_result block is
// excluded entirely (ok=false); list content without one concatenates its
// text blocks, dropping any starting with "<system-reminder>" or
// "Base directory for this skill:".
func extractUserText(content json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s, true
	}

	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return "", false
		}
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		if strings.HasPrefix(b.Text, "<system-reminder>") || strings.HasPrefix(b.Text, "Base directory for this skill:") {
			continue
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n"), true
}

func (e *extractState) feedAssistant(r record, role string, ts int64) {
	texts, files := extractAssistantBlocks(r.Message.Content)
	for _, path := range files {
		if e.fileSeen == nil {
			e.fileSeen = map[string]bool{}
		}
		if !e.fileSeen[path] {
			e.fileSeen[path] = true
			e.out.Files = append(e.out.Files, path)
		}
	}
	for _, t := range texts {
		clean := CleanText(t)
		if clean == "" {
			continue
		}
		e.out.Docs = append(e.out.Docs, store.Doc{Content: clean, Role: role, TS: ts})
		if e.isMain {
			e.out.Outcome = clean
		}
	}
}

// extractAssistantBlocks pulls indexable text blocks (filter-table row 5:
// text yes, thinking/tool_use no) and touched-file paths from tool_use
// inputs (Edit/Write/MultiEdit's file_path, NotebookEdit's notebook_path)
// out of an assistant message's content.
func extractAssistantBlocks(content json.RawMessage) (texts, files []string) {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		if s != "" {
			texts = append(texts, s)
		}
		return
	}

	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "tool_use":
			switch b.Name {
			case "Edit", "Write", "MultiEdit":
				var in toolFileInput
				if json.Unmarshal(b.Input, &in) == nil && in.FilePath != "" {
					files = append(files, in.FilePath)
				}
			case "NotebookEdit":
				var in toolFileInput
				if json.Unmarshal(b.Input, &in) == nil && in.NotebookPath != "" {
					files = append(files, in.NotebookPath)
				}
			}
		}
	}
	return
}
