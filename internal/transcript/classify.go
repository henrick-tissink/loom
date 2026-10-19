package transcript

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

type State int

const (
	StateUnknown State = iota
	StateRunning
	StateNeedsYou
	StateIdle
)

func (s State) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateNeedsYou:
		return "needs_you"
	case StateIdle:
		return "idle"
	default:
		return "unknown"
	}
}

type record struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	AiTitle     string `json:"aiTitle"`
	Timestamp   string `json:"timestamp"` // RFC 3339, written on every turn record
	IsMeta      bool   `json:"isMeta"`
	Subtype     string `json:"subtype"`
	// Content is a system record's text; raw, so a record whose content is
	// not a string still parses
	Content    json.RawMessage `json:"content"`
	Attachment *struct {
		Type       string `json:"type"`
		Met        bool   `json:"met"`
		Sentinel   bool   `json:"sentinel"`
		Condition  string `json:"condition"`
		Reason     string `json:"reason"`
		Iterations int    `json:"iterations"`
	} `json:"attachment"`
	Message *struct {
		Content json.RawMessage `json:"content"`
		Usage   *usage          `json:"usage"`
		// StopReason is the API stop reason on an assistant message. It is the
		// authoritative "did the turn end?" signal: "tool_use"/"pause_turn"
		// mean Claude is still working (a thinking or text message emitted
		// mid-turn before the tool call lands), while "end_turn" (and the other
		// terminal reasons) mean control returned to the user. Keying needs_you
		// on the absence of a tool_use BLOCK instead misfired on every mid-turn
		// thinking/text message — the "needs you while busy" bug.
		StopReason string `json:"stop_reason"`
	} `json:"message"`
}

type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

type block struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Text string `json:"text"`
}

// Goal is the session's /goal as claude records it: a goal_status
// attachment when it is set (sentinel, not met), after each check (met or
// not, with the evaluator's reason), and when it is cleared (sentinel,
// met); and a system notice when claude pauses it.
type Goal struct {
	Condition string
	State     string // active | paused | met | cleared; "" = no goal ever set
	Checks    int    // evaluator checks since it was set
	Reason    string // the last check's reason, or the pause notice
	At        int64  // unix seconds of the last change
}

// askLimit bounds the captured question: the rail shows one line of it and
// the reply modal a few, so the rest of a long answer is dead weight.
const askLimit = 300

// Classifier folds JSONL lines into the session's turn-boundary state.
// It NEVER classifies on sidecar records (mode, permission-mode, last-prompt,
// ai-title, file-history-snapshot, attachment, queue-operation, system) — real
// transcripts flush those after the final turn (spec §4.3, P0).
type Classifier struct {
	state     State
	lastTool  string
	title     string
	ctxTokens int64
	// ask is the text claude last ended a turn with — the question or
	// hand-back the user is being waited on for — and askAt when it was
	// written. Both are set only on the needs-you transition, so a row's
	// wait age is measured from the ask, not from the last tool call.
	ask   string
	askAt int64
	goal  Goal
}

func (c *Classifier) Feed(line []byte) {
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		return // partial/garbage line: ignore, keep prior state
	}
	if r.Type == "ai-title" {
		if !r.IsSidechain && r.AiTitle != "" {
			c.title = r.AiTitle
		}
		return // sidecar: never a turn boundary
	}
	if !r.IsSidechain {
		c.feedGoal(r)
	}
	if r.IsSidechain || (r.Type != "assistant" && r.Type != "user") {
		return // sidecar or subagent record: not a turn boundary
	}
	blocks := parseBlocks(r)
	switch r.Type {
	case "assistant":
		if r.Message != nil && r.Message.Usage != nil {
			u := r.Message.Usage
			// input + cache creation + cache read is the context the turn was
			// sent with; output is not part of the window the next turn pays
			// for. This is also the operator's ctx_budget hook's formula, so
			// the gauge and the hook's nudge agree.
			c.ctxTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
		}
		if name, ok := findBlock(blocks, "tool_use"); ok {
			c.lastTool = name
			c.state = StateRunning // tool pending: its result would be a LATER user record
			return
		}
		// No tool_use block. The session is genuinely waiting ONLY when it has
		// delivered a TEXT answer AND the turn has ended. Two mid-turn shapes
		// used to misfire as needs_you (the "needs you while busy" bug), both
		// verified against a real transcript:
		//   • thinking-only messages — ALWAYS mid-turn (225/225 followed by more
		//     assistant activity). Thinking precedes a tool call or the text
		//     answer, so their stop_reason is unreliable: 61 carried "end_turn"
		//     yet the text answer landed ~7s later.
		//   • text messages whose stop_reason is "tool_use"/"pause_turn" — a
		//     preamble before the next tool call (168/168 continued).
		// An empty stop_reason (older transcripts, pre stop_reason) is treated
		// as terminal, preserving the original text→needs_you behavior.
		sr := stopReason(r)
		_, hasText := findBlock(blocks, "text")
		if hasText && sr != "tool_use" && sr != "pause_turn" {
			c.state = StateNeedsYou
			c.ask = lastText(blocks)
			c.askAt = parseTS(r.Timestamp)
			return
		}
		c.state = StateRunning // thinking mid-turn, or a text preamble before a tool
	case "user":
		if _, ok := findBlock(blocks, "tool_result"); ok {
			c.state = StateRunning // claude is consuming the result
			return
		}
		c.state = StateIdle // human prompt; fusion upgrades to Running while streaming
	}
}

// feedGoal folds the goal records; none of them is a turn boundary.
func (c *Classifier) feedGoal(r record) {
	at := parseTS(r.Timestamp)
	switch {
	case r.Type == "attachment" && r.Attachment != nil && r.Attachment.Type == "goal_status":
		a := r.Attachment
		switch {
		case a.Sentinel && !a.Met:
			c.goal = Goal{Condition: a.Condition, State: "active"}
		case a.Sentinel:
			c.goal.State, c.goal.Reason = "cleared", ""
		default:
			c.goal.Condition = a.Condition
			c.goal.Checks++
			if a.Iterations > 0 {
				c.goal.Checks = a.Iterations
			}
			c.goal.Reason = oneLine(a.Reason)
			c.goal.State = "active"
			if a.Met {
				c.goal.State = "met"
			}
		}
		c.goal.At = at
	case r.Type == "system" && c.goal.State == "active" && bytes.HasPrefix(r.Content, []byte(`"Goal paused`)):
		var s string
		_ = json.Unmarshal(r.Content, &s)
		c.goal.State, c.goal.Reason, c.goal.At = "paused", s, at
	case r.Type == "user" && !r.IsMeta && c.goal.State == "paused":
		// "send a message to continue": any human message resumes it
		if _, ok := findBlock(parseBlocks(r), "tool_result"); !ok {
			c.goal.State, c.goal.At = "active", at
		}
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if rs := []rune(s); len(rs) > askLimit {
		return string(rs[:askLimit]) + "…"
	}
	return s
}

func parseBlocks(r record) []block {
	if r.Message == nil {
		return nil
	}
	var bs []block
	// content is either a plain string (user prompt) or a block array
	if err := json.Unmarshal(r.Message.Content, &bs); err != nil {
		return nil
	}
	return bs
}

func stopReason(r record) string {
	if r.Message == nil {
		return ""
	}
	return r.Message.StopReason
}

func findBlock(bs []block, typ string) (name string, ok bool) {
	for _, b := range bs {
		if b.Type == typ {
			return b.Name, true
		}
	}
	return "", false
}

// lastText is the final text block's content, cut to askLimit runes. The
// last block rather than the first: a turn that explains and then asks puts
// the question at the end.
func lastText(bs []block) string {
	for i := len(bs) - 1; i >= 0; i-- {
		if bs[i].Type != "text" {
			continue
		}
		rs := []rune(bs[i].Text)
		if len(rs) > askLimit {
			return string(rs[:askLimit]) + "…"
		}
		return bs[i].Text
	}
	return ""
}

func parseTS(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

func (c *Classifier) State() State     { return c.state }
func (c *Classifier) LastTool() string { return c.lastTool }
func (c *Classifier) Title() string    { return c.title }
func (c *Classifier) CtxTokens() int64 { return c.ctxTokens }
func (c *Classifier) Ask() string      { return c.ask }
func (c *Classifier) AskAt() int64     { return c.askAt }
func (c *Classifier) Goal() Goal       { return c.goal }
