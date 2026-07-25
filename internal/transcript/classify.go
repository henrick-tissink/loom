package transcript

import "encoding/json"

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
	Message     *struct {
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
}

// Classifier folds JSONL lines into the session's turn-boundary state.
// It NEVER classifies on sidecar records (mode, permission-mode, last-prompt,
// ai-title, file-history-snapshot, attachment, queue-operation, system) — real
// transcripts flush those after the final turn (spec §4.3, P0).
type Classifier struct {
	state     State
	lastTool  string
	title     string
	ctxTokens int64
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
	if r.IsSidechain || (r.Type != "assistant" && r.Type != "user") {
		return // sidecar or subagent record: not a turn boundary
	}
	blocks := parseBlocks(r)
	switch r.Type {
	case "assistant":
		if r.Message != nil && r.Message.Usage != nil {
			u := r.Message.Usage
			c.ctxTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens + u.OutputTokens
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

func (c *Classifier) State() State     { return c.state }
func (c *Classifier) LastTool() string { return c.lastTool }
func (c *Classifier) Title() string    { return c.title }
func (c *Classifier) CtxTokens() int64 { return c.ctxTokens }
