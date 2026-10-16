package hooklog

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Mark is a moment the hooks changed. Solid marks come from loom's ledger;
// inferred ones from the logs (the first record carrying a new rev) and
// from the edit times of a pre-loom hooks directory. Rates are never summed
// across a mark: a number that mixes two versions of a hook says nothing
// about either.
type Mark struct {
	At       time.Time `json:"at"`
	Label    string    `json:"label"`
	Inferred bool      `json:"inferred"`
}

// Day is one calendar day's counts inside a span, for a sparkline.
type Day struct {
	Day    string `json:"day"` // YYYY-MM-DD, local
	Runs   int    `json:"runs"`
	Holds  int    `json:"holds"`
	Nudges int    `json:"nudges"`
}

// Span is one hook between two marks. Runs = Silent + Logged.
type Span struct {
	From      time.Time      `json:"from"`
	To        time.Time      `json:"to"`
	Mark      string         `json:"mark,omitempty"` // the mark that opened it
	Inferred  bool           `json:"inferred,omitempty"`
	Rev       string         `json:"rev,omitempty"` // the rev stamp seen in this span, if any
	Runs      int            `json:"runs"`
	Silent    int            `json:"silent"`
	Logged    int            `json:"logged"`
	Holds     int            `json:"holds"`
	Nudges    int            `json:"nudges"`
	Tests     int            `json:"tests"`
	Decisions map[string]int `json:"decisions"`
	Days      []Day          `json:"days"`
}

// HookRow is one hook's spans, oldest first.
type HookRow struct {
	Hook  string `json:"hook"`
	Rev   string `json:"rev,omitempty"` // latest rev stamp seen
	Spans []Span `json:"spans"`
}

// Rates splits the records from `since` at the marks and counts per hook
// per span. A zero `since` means everything. Spans with no records are
// left out.
func Rates(recs []Record, since time.Time, marks []Mark, now time.Time) []HookRow {
	ms := sortedMarks(marks)
	bounds := []time.Time{}
	for _, m := range ms {
		if since.IsZero() || m.At.After(since) {
			bounds = append(bounds, m.At)
		}
	}
	type key struct {
		hook string
		span int
	}
	spans := map[key]*Span{}
	hooks := map[string]bool{}
	latestRev := map[string]string{}
	for _, r := range recs {
		if !since.IsZero() && r.TS.Before(since) {
			continue
		}
		idx := sort.Search(len(bounds), func(i int) bool { return bounds[i].After(r.TS) })
		k := key{r.Hook, idx}
		s := spans[k]
		if s == nil {
			s = &Span{Decisions: map[string]int{}, Days: []Day{}}
			if idx > 0 {
				m := markAt(ms, bounds[idx-1])
				s.From, s.Mark, s.Inferred = m.At, m.Label, m.Inferred
			} else {
				// The first span starts at the window; the mark that
				// opened it (the latest one at or before `since`) is the
				// one it sits after, so "since change" shows that change.
				s.From = since
				if m, ok := markBefore(ms, since); ok {
					s.Mark, s.Inferred = m.Label, m.Inferred
				}
			}
			if idx < len(bounds) {
				s.To = bounds[idx]
			} else {
				s.To = now
			}
			spans[k] = s
		}
		hooks[r.Hook] = true
		count(s, r)
		if r.Rev != "" {
			s.Rev = r.Rev
			latestRev[r.Hook] = r.Rev
		}
	}
	names := make([]string, 0, len(hooks))
	for h := range hooks {
		names = append(names, h)
	}
	sort.Strings(names)
	out := make([]HookRow, 0, len(names))
	for _, h := range names {
		row := HookRow{Hook: h, Rev: latestRev[h]}
		for i := 0; i <= len(bounds); i++ {
			if s := spans[key{h, i}]; s != nil {
				row.Spans = append(row.Spans, *s)
			}
		}
		out = append(out, row)
	}
	return out
}

func count(s *Span, r Record) {
	s.Runs++
	cls := r.Class()
	switch cls {
	case "silent":
		s.Silent++
	default:
		s.Logged++
		s.Decisions[r.Decision]++
	}
	switch cls {
	case "hold":
		s.Holds++
	case "nudge":
		s.Nudges++
	case "test":
		s.Tests++
	}
	day := r.TS.Format("2006-01-02")
	n := len(s.Days)
	if n == 0 || s.Days[n-1].Day != day {
		s.Days = append(s.Days, Day{Day: day})
		n++
	}
	d := &s.Days[n-1]
	d.Runs++
	if cls == "hold" {
		d.Holds++
	} else if cls == "nudge" {
		d.Nudges++
	}
}

func sortedMarks(marks []Mark) []Mark {
	ms := append([]Mark(nil), marks...)
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].At.Before(ms[j].At) })
	return ms
}

// markBefore is the latest mark at or before t.
func markBefore(ms []Mark, t time.Time) (Mark, bool) {
	if t.IsZero() {
		return Mark{}, false
	}
	var out Mark
	ok := false
	for _, m := range ms {
		if !m.At.After(t) {
			out, ok = m, true
		}
	}
	return out, ok
}

func markAt(ms []Mark, at time.Time) Mark {
	for _, m := range ms {
		if m.At.Equal(at) {
			return m
		}
	}
	return Mark{At: at}
}

// LastMark is the newest mark, for the "since change" window; ok is false
// when there is none.
func LastMark(marks []Mark) (Mark, bool) {
	ms := sortedMarks(marks)
	if len(ms) == 0 {
		return Mark{}, false
	}
	return ms[len(ms)-1], true
}

// InferredMarks finds hook changes the ledger does not know about: the
// first record stamped with each rev, and the edit times of the files in
// legacy hook directories (the backup names the owner's own edits left
// behind count too). Times within `within` of each other collapse into
// one mark at the latest of them; inferred marks within `within` of a
// solid one are dropped, since the ledger already explains them.
func InferredMarks(recs []Record, legacyDirs []string, solid []Mark, within time.Duration) []Mark {
	var cands []Mark
	seenRev := map[string]bool{}
	for _, r := range recs {
		if r.Rev != "" && !seenRev[r.Rev] {
			seenRev[r.Rev] = true
			cands = append(cands, Mark{At: r.TS, Label: "first " + r.Rev + " record", Inferred: true})
		}
	}
	for _, dir := range legacyDirs {
		for _, t := range legacyEditTimes(dir) {
			cands = append(cands, Mark{At: t, Label: "hooks edited by hand", Inferred: true})
		}
	}
	cands = sortedMarks(cands)
	var out []Mark
	for _, c := range cands {
		near := false
		for _, s := range solid {
			if abs(c.At.Sub(s.At)) <= within {
				near = true
				break
			}
		}
		if near {
			continue
		}
		if n := len(out); n > 0 && c.At.Sub(out[n-1].At) <= within {
			out[n-1].At = c.At // the cluster's latest time
			if out[n-1].Label != c.Label {
				out[n-1].Label = "hooks changed"
			}
			continue
		}
		out = append(out, c)
	}
	return out
}

var backupStamp = regexp.MustCompile(`(20\d{6})[-]?(\d{6})`)

// legacyEditTimes lists the modification times of the python files in a
// legacy hooks directory plus any timestamps in the backup names beside
// them.
func legacyEditTimes(dir string) []time.Time {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []time.Time
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() && !strings.HasPrefix(name, "backup") {
			continue
		}
		if m := backupStamp.FindStringSubmatch(name); m != nil {
			if t, err := time.ParseInLocation("20060102150405", m[1]+m[2], time.Local); err == nil {
				out = append(out, t)
				continue
			}
		}
		if filepath.Ext(name) == ".py" {
			if fi, err := e.Info(); err == nil {
				out = append(out, fi.ModTime())
			}
		}
	}
	return out
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// ProjectRow is the test re-run report for one working directory.
type ProjectRow struct {
	Project      string    `json:"project"` // the cwd the tests ran in
	Runs         int       `json:"runs"`
	First        int       `json:"first"`
	AfterChange  int       `json:"afterChange"`
	NoChange     int       `json:"noChange"`
	NoChangeSame int       `json:"noChangeSameCmd"`
	Unknown      int       `json:"unknown"`
	MedianGapS   int       `json:"medianGapS"` // between a no-change re-run and the run before it
	Commands     []string  `json:"commands"`   // distinct no-change re-run commands, latest first
	LastRun      time.Time `json:"lastRun"`
}

// Reruns groups test_rerun records by cwd. Rows are sorted by runs, most
// first. A zero `since` means everything.
func Reruns(recs []Record, since time.Time) []ProjectRow {
	rows := map[string]*ProjectRow{}
	gaps := map[string][]float64{}
	for _, r := range recs {
		if r.Hook != "test_rerun" || r.Class() != "test" || (!since.IsZero() && r.TS.Before(since)) {
			continue
		}
		p := rows[r.Cwd]
		if p == nil {
			p = &ProjectRow{Project: r.Cwd, Commands: []string{}}
			rows[r.Cwd] = p
		}
		p.Runs++
		if r.TS.After(p.LastRun) {
			p.LastRun = r.TS
		}
		switch r.Changed {
		case "first":
			p.First++
		case "yes":
			p.AfterChange++
		case "no":
			p.NoChange++
			if r.SameCmd {
				p.NoChangeSame++
			}
			if r.SinceLast > 0 {
				gaps[r.Cwd] = append(gaps[r.Cwd], r.SinceLast)
			}
			if r.Command != "" {
				p.Commands = pushFront(p.Commands, r.Command, 5)
			}
		default:
			p.Unknown++
		}
	}
	out := make([]ProjectRow, 0, len(rows))
	for cwd, p := range rows {
		if g := gaps[cwd]; len(g) > 0 {
			sort.Float64s(g)
			p.MedianGapS = int(g[len(g)/2])
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Runs != out[j].Runs {
			return out[i].Runs > out[j].Runs
		}
		return out[i].Project < out[j].Project
	})
	return out
}

// pushFront keeps a distinct list with the newest first, capped at n.
func pushFront(list []string, s string, n int) []string {
	out := []string{s}
	for _, x := range list {
		if x != s && len(out) < n {
			out = append(out, x)
		}
	}
	return out
}

// Held is one hook+decision that blocked a session repeatedly.
type Held struct {
	Hook     string    `json:"hook"`
	Decision string    `json:"decision"`
	Count    int       `json:"count"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	Command  string    `json:"command,omitempty"` // the latest one
}

// HeldThreshold is how many repeats of one hold make a session "held".
const HeldThreshold = 3

// SessionStats is what a session row shows from its own records.
type SessionStats struct {
	Holds  int   `json:"holds"`
	Nudges int   `json:"nudges"`
	Tests  int   `json:"tests"`
	Reruns int   `json:"reruns"` // test runs with no change since the last
	Held   *Held `json:"held,omitempty"`
	// LastStop is the stop guard's latest Stop verdict (not SubagentStop).
	LastStop *Record `json:"lastStop,omitempty"`
	// Groups are every hold grouped by hook+decision, most repeated first.
	Groups []Held `json:"groups"`
}

// Session counts one session's records from `since` (zero: all of them).
// The held chip needs the current task file's version: pass the task's
// start so a new task file resets the count.
func Session(recs []Record, since time.Time) SessionStats {
	st := SessionStats{Groups: []Held{}}
	groups := map[string]*Held{}
	for i := range recs {
		r := recs[i]
		if !since.IsZero() && r.TS.Before(since) {
			continue
		}
		switch r.Class() {
		case "hold":
			st.Holds++
			k := r.Hook + "|" + r.Decision
			g := groups[k]
			if g == nil {
				g = &Held{Hook: r.Hook, Decision: r.Decision, First: r.TS}
				groups[k] = g
			}
			g.Count++
			g.Last = r.TS
			if r.Command != "" {
				g.Command = r.Command
			}
		case "nudge":
			st.Nudges++
		case "test":
			st.Tests++
			if r.Changed == "no" {
				st.Reruns++
			}
		}
		if r.Hook == "stop_guard" && r.Event == "Stop" && !r.Silent {
			rc := r
			st.LastStop = &rc
		}
	}
	for _, g := range groups {
		st.Groups = append(st.Groups, *g)
	}
	sort.Slice(st.Groups, func(i, j int) bool {
		if st.Groups[i].Count != st.Groups[j].Count {
			return st.Groups[i].Count > st.Groups[j].Count
		}
		return st.Groups[i].Last.After(st.Groups[j].Last)
	})
	if len(st.Groups) > 0 && st.Groups[0].Count >= HeldThreshold {
		h := st.Groups[0]
		st.Held = &h
	}
	return st
}

// ClassLine is one "N of M <unit> <verb>" line for the hooks panel.
type ClassLine struct {
	Hook  string `json:"hook"`
	Class string `json:"class"` // hold | nudge | test
	Count int    `json:"count"`
	Of    int    `json:"of"` // the denominator: this hook's runs (tests: test runs)
}

// Classes gives the per-hook denominators for a session: holds and nudges
// over the hook's runs, no-change re-runs over test runs.
func Classes(recs []Record, since time.Time) []ClassLine {
	type agg struct{ runs, holds, nudges, tests, reruns int }
	per := map[string]*agg{}
	for _, r := range recs {
		if !since.IsZero() && r.TS.Before(since) {
			continue
		}
		a := per[r.Hook]
		if a == nil {
			a = &agg{}
			per[r.Hook] = a
		}
		a.runs++
		switch r.Class() {
		case "hold":
			a.holds++
		case "nudge":
			a.nudges++
		case "test":
			a.tests++
			if r.Changed == "no" {
				a.reruns++
			}
		}
	}
	hooks := make([]string, 0, len(per))
	for h := range per {
		hooks = append(hooks, h)
	}
	sort.Strings(hooks)
	var out []ClassLine
	for _, h := range hooks {
		a := per[h]
		if a.holds > 0 {
			out = append(out, ClassLine{Hook: h, Class: "hold", Count: a.holds, Of: a.runs})
		}
		if a.nudges > 0 {
			out = append(out, ClassLine{Hook: h, Class: "nudge", Count: a.nudges, Of: a.runs})
		}
		if a.tests > 0 {
			out = append(out, ClassLine{Hook: h, Class: "test", Count: a.reruns, Of: a.tests})
		}
	}
	return out
}
