package rules

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/hooklog"
)

// Event is one failure: a hook hold, a test re-run with no change, or a
// tool call that returned an error.
type Event struct {
	Key     string    `json:"key"`    // hook/<hook>/<decision> or tool/<tool>/<hash>
	Source  string    `json:"source"` // hook | tool
	Title   string    `json:"title"`
	Session string    `json:"session"`
	Cwd     string    `json:"cwd"`
	TS      time.Time `json:"ts"`
	Sample  string    `json:"sample,omitempty"` // the command or the error text
}

// A failure becomes a proposal when it repeats this often, in this many
// sessions, inside the window. One session stuck in a loop is not a
// pattern; three sessions are.
const (
	MinCount    = 5
	MinSessions = 3
	Window      = 14 * 24 * time.Hour
	// DefaultLapse is how long an adopted rule holds before loom asks
	// whether it is still needed.
	DefaultLapse = 30 * 24 * time.Hour
)

// FromHooks picks the failures out of the hooks' records: every hold, and
// every test re-run with no change since the run before it.
func FromHooks(recs []hooklog.Record) []Event {
	var out []Event
	for _, r := range recs {
		var title string
		switch {
		case r.Class() == "hold":
			verb := "blocked"
			if strings.HasPrefix(r.Decision, "deny") {
				verb = "denied"
			}
			what := strings.ReplaceAll(strings.TrimPrefix(strings.TrimPrefix(r.Decision, "block_"), "deny_"), "_", " ")
			title = r.Hook + " " + verb + ": " + what
		case r.Hook == "test_rerun" && r.Class() == "test" && r.Changed == "no":
			title = "tests re-run with no change"
		default:
			continue
		}
		out = append(out, Event{Key: "hook/" + r.Hook + "/" + r.Decision, Source: "hook", Title: title,
			Session: r.Session, Cwd: r.Cwd, TS: r.TS, Sample: r.Command})
	}
	return out
}

// Proposal is a repeated failure with no adopted rule yet.
type Proposal struct {
	Key      string    `json:"key"`
	Source   string    `json:"source"`
	Title    string    `json:"title"`
	Count    int       `json:"count"`
	Sessions int       `json:"sessions"`
	Projects []string  `json:"projects"` // the cwds, most failures first, at most 5
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	Samples  []string  `json:"samples"` // distinct, latest first, at most 3
	Draft    string    `json:"draft"`   // "" when loom has no wording to offer
	Why      string    `json:"why"`
}

type group struct {
	p        Proposal
	sessions map[string]bool
	cwds     map[string]int
}

// Mine groups the events from `since` by key and proposes the ones that
// repeat. Hook events also start at hookSince (the last hook change):
// holds from before a hook changed say nothing about the hook now. Keys
// with an adopted rule, lapsed or not, are left to Status.
func Mine(events []Event, adopted []Rule, since, hookSince time.Time) []Proposal {
	have := map[string]bool{}
	for _, r := range adopted {
		have[r.Key] = true
	}
	groups := map[string]*group{}
	for _, e := range events {
		if e.TS.Before(since) || (e.Source == "hook" && e.TS.Before(hookSince)) || have[e.Key] {
			continue
		}
		g := groups[e.Key]
		if g == nil {
			g = &group{p: Proposal{Key: e.Key, Source: e.Source, Title: e.Title, First: e.TS, Samples: []string{}}, sessions: map[string]bool{}, cwds: map[string]int{}}
			groups[e.Key] = g
		}
		g.p.Count++
		if e.TS.Before(g.p.First) {
			g.p.First = e.TS
		}
		if e.TS.After(g.p.Last) {
			g.p.Last = e.TS
			if e.Sample != "" {
				g.p.Samples = pushFront(g.p.Samples, e.Sample, 3)
			}
		}
		g.sessions[e.Session] = true
		if e.Cwd != "" {
			g.cwds[e.Cwd]++
		}
	}
	out := []Proposal{}
	for _, g := range groups {
		p := g.p
		p.Sessions = len(g.sessions)
		if p.Count < MinCount || p.Sessions < MinSessions {
			continue
		}
		p.Projects = topKeys(g.cwds, 5)
		p.Draft = Draft(p.Key, p.Title)
		p.Why = CleanWhy(fmt.Sprintf("%s, %d times in %d sessions between %s and %s.", p.Title, p.Count, p.Sessions,
			p.First.Format(DateLayout), p.Last.Format(DateLayout)))
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// RuleStatus is an adopted rule with what happened since.
type RuleStatus struct {
	Rule
	Lapsed        bool `json:"lapsed"`
	After         int  `json:"after"` // its failure since the rule was adopted
	AfterSessions int  `json:"afterSessions"`
	Before        int  `json:"before"` // in the Window before that
}

// Status measures each adopted rule against the events.
func Status(adopted []Rule, events []Event, now time.Time) []RuleStatus {
	out := make([]RuleStatus, 0, len(adopted))
	for _, r := range adopted {
		st := RuleStatus{Rule: r, Lapsed: r.Lapsed(now)}
		since, err := time.ParseInLocation(SinceLayout, r.Since, now.Location())
		if err == nil {
			sessions := map[string]bool{}
			for _, e := range events {
				if e.Key != r.Key {
					continue
				}
				switch {
				case !e.TS.Before(since):
					st.After++
					sessions[e.Session] = true
				case !e.TS.Before(since.Add(-Window)):
					st.Before++
				}
			}
			st.AfterSessions = len(sessions)
		}
		out = append(out, st)
	}
	return out
}

func pushFront(list []string, s string, n int) []string {
	out := []string{s}
	for _, x := range list {
		if x != s && len(out) < n {
			out = append(out, x)
		}
	}
	return out
}

func topKeys(m map[string]int, n int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}

// hookDrafts are the wordings for the holds the bundled hooks make. They
// restate what the hook enforces, as an instruction the agent can follow
// before the hook has to stop it.
var hookDrafts = map[string]string{
	"hook/bash_guard/deny_no_deadline":           "Start every background or long-running Bash command with `deadline <duration>` (for example `deadline 20m bash -c '…'`); the guard denies it otherwise.",
	"hook/bash_guard/deny_detach":                "Never detach a process with nohup, disown, setsid or a trailing &; run it as a background task under `deadline` instead.",
	"hook/stop_guard/block_unaccounted_tasks":    "Before ending a turn, account for every background task you started: finished with its result, or stopped.",
	"hook/stop_guard/block_done_claim_task_open": "Tick the task file's Done-when checks with their evidence and set Status: done before you say the work is done.",
	"hook/stop_guard/block_done_unticked_checks": "Tick each Done-when check with its evidence before handing back.",
	"hook/stop_guard/block_done_check_failed":    "Run every `$` Done-when check yourself and see it pass before handing back.",
	"hook/stop_guard/block_unbacked_promise":     "Never promise to wait, watch or follow up unless a wake-up is registered (a background task notification, Monitor or ScheduleWakeup).",
	"hook/test_rerun/log_test_rerun_no_change":   "Do not re-run tests when nothing changed since the last run; read that run's output instead.",
}

// toolDrafts match a tool error's text.
var toolDrafts = []struct{ match, draft string }{
	{"file has not been read yet", "Read a file before you Write it, even when you are replacing all of it."},
	{"file has been modified since read", "Re-read a file right before you edit it when anything else (a formatter, a hook, the user, another session) may have changed it."},
	{"string to replace not found", "Copy Edit's old_string from a fresh Read of the file, whitespace included, never from memory."},
	{"matches of the string to replace", "Give Edit an old_string that occurs once in the file (add surrounding lines), or set replace_all."},
	{"file does not exist", "Check that a path exists (ls or Glob) before you Read it; relative paths start at the session's working directory."},
}

// Draft is loom's suggested wording for a failure, or "" when it has none
// and the person writes the rule.
func Draft(key, title string) string {
	if d, ok := hookDrafts[key]; ok {
		return d
	}
	if strings.HasPrefix(key, "tool/") {
		t := strings.ToLower(title)
		for _, d := range toolDrafts {
			if strings.Contains(t, d.match) {
				return d.draft
			}
		}
	}
	return ""
}
