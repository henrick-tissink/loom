package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/discipline"
	"github.com/henricktissink/loom/internal/hooklog"
	"github.com/henricktissink/loom/internal/rules"
)

// Rules from repeated failures (the Agent Discipline island's Rules tab):
// loom mines the hook logs and the transcripts' tool errors, proposes a
// rule for each failure that keeps repeating, and writes one to
// <ccd>/discipline/rules.md only when the person applies a change whose
// diff they reviewed. A lapsed rule is shown, never removed by loom.

// rulesState caches each transcript's errors by size and mtime, so only
// files that grew are read again.
type rulesState struct {
	mu     sync.Mutex
	layout discipline.Layout
	files  map[string]scannedFile
}

type scannedFile struct {
	size   int64
	mtime  time.Time
	events []rules.Event
}

func newRulesState(l discipline.Layout) *rulesState {
	return &rulesState{layout: l, files: map[string]scannedFile{}}
}

// The transcripts read: the last month, or back to a Window before the
// oldest adopted rule (so its before-and-after holds), at most 90 days.
const (
	rulesScanDays    = 30
	rulesScanMaxDays = 90
)

// toolEvents returns the tool errors of the transcripts modified since.
func (r *rulesState) toolEvents(since time.Time) ([]rules.Event, int) {
	paths := rules.Transcripts(filepath.Join(r.layout.ConfigDir, "projects"), since)
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	var out []rules.Event
	for _, p := range paths {
		seen[p] = true
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		c, ok := r.files[p]
		if !ok || c.size != fi.Size() || !c.mtime.Equal(fi.ModTime()) {
			evs, err := rules.ScanErrors(p)
			if err != nil {
				continue
			}
			c = scannedFile{size: fi.Size(), mtime: fi.ModTime(), events: evs}
			r.files[p] = c
		}
		out = append(out, c.events...)
	}
	for p := range r.files {
		if !seen[p] {
			delete(r.files, p)
		}
	}
	return out, len(paths)
}

// RulesDTO is the Rules tab.
type RulesDTO struct {
	Path      string             `json:"path"`
	Proposals []rules.Proposal   `json:"proposals"`
	Rules     []rules.RuleStatus `json:"rules"`
	Since     int64              `json:"since"`     // the proposals' window start
	HookSince int64              `json:"hookSince"` // hook evidence starts here (the last hook change), when later
	Scanned   int                `json:"scanned"`   // transcripts read
	Hidden    bool               `json:"hidden"`    // hidden projects' failures were left out
	Lapse     string             `json:"lapse"`     // the default lapse date for a new rule
	Err       string             `json:"err,omitempty"`
}

// RuleProposals mines the evidence and reads the adopted rules.
func (a *App) RuleProposals() RulesDTO {
	out := RulesDTO{Proposals: []rules.Proposal{}, Rules: []rules.RuleStatus{}}
	if a.rules == nil {
		out.Err = "rules unavailable"
		return out
	}
	now := a.now()
	l := a.rules.layout
	out.Path = l.RulesPath()
	out.Lapse = now.Add(rules.DefaultLapse).Format(rules.DateLayout)
	content, err := os.ReadFile(l.RulesPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		out.Err = err.Error()
	}
	adopted := rules.Parse(content)

	scanFrom := now.Add(-rulesScanDays * 24 * time.Hour)
	for _, r := range adopted {
		if t, err := time.ParseInLocation(rules.SinceLayout, r.Since, now.Location()); err == nil && t.Add(-rules.Window).Before(scanFrom) {
			scanFrom = t.Add(-rules.Window)
		}
	}
	if floor := now.Add(-rulesScanMaxDays * 24 * time.Hour); scanFrom.Before(floor) {
		scanFrom = floor
	}
	events, scanned := a.rules.toolEvents(scanFrom)
	out.Scanned = scanned

	var hookSince time.Time
	if a.hooks != nil {
		a.hooks.reader.Refresh()
		all := a.hooks.reader.Records()
		if m, ok := hooklog.LastMark(a.hooks.marks(all)); ok {
			hookSince = m.At
		}
		events = append(events, rules.FromHooks(all)...)
	}
	events, out.Hidden = a.visibleEvents(events)
	sort.SliceStable(events, func(i, j int) bool { return events[i].TS.Before(events[j].TS) })

	since := now.Add(-rules.Window)
	out.Since = since.Unix()
	if hookSince.After(since) {
		out.HookSince = hookSince.Unix()
	}
	out.Proposals = rules.Mine(events, adopted, since, hookSince)
	out.Rules = rules.Status(adopted, events, now)
	return out
}

// visibleEvents applies the hide/solo rules by each failure's cwd. A
// failure with no cwd cannot be placed, so while anything is hidden it is
// left out.
func (a *App) visibleEvents(evs []rules.Event) ([]rules.Event, bool) {
	res := a.resolver()
	if res == nil || !res.Filtering() {
		return evs, false
	}
	out := make([]rules.Event, 0, len(evs))
	hidden := false
	for _, e := range evs {
		if e.Cwd == "" || !res.Visible(e.Cwd) {
			hidden = true
			continue
		}
		out = append(out, e)
	}
	return out, hidden
}

// RulePlan returns the diffs a change would make, for review.
func (a *App) RulePlan(c discipline.RuleChange) (*discipline.RulePlan, error) {
	if a.rules == nil {
		return nil, errors.New("rules unavailable")
	}
	return discipline.PlanRule(a.rules.layout, c, a.now())
}

// RuleApply makes a reviewed change; token is from RulePlan.
func (a *App) RuleApply(c discipline.RuleChange, token string) (string, error) {
	if a.rules == nil {
		return "", errors.New("rules unavailable")
	}
	dir, err := discipline.ApplyRule(a.rules.layout, c, token, a.now())
	if err != nil {
		return "", err
	}
	if a.disc != nil {
		a.disc.invalidate()
	}
	msg := map[string]string{"adopt": "Rule adopted", "drop": "Rule dropped", "renew": "Rule renewed"}[c.Op]
	if dir != "" {
		msg += "; previous files in " + dir
	}
	return msg, nil
}
