package main

import (
	"os"
	"sort"
	"time"

	"github.com/henricktissink/loom/internal/discipline"
	"github.com/henricktissink/loom/internal/hooklog"
	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/status"
)

// The hooks' own logs on the GUI (docs/DISCIPLINE.md, phase 4): what held
// a session, how often each hook intervenes between hook changes, and how
// often tests are re-run with no change. Loom reads the logs; it never
// writes them.

// hookState owns the incremental reader and the paths the marks come from.
type hookState struct {
	reader    *hooklog.Reader
	layout    discipline.Layout
	hasLayout bool
}

// newHookState reads state/log under the layout, plus the legacy hooks dir
// (a pre-loom install writes beside its scripts) and the directory loom
// moved such an install to, for history.
func newHookState(l discipline.Layout) *hookState {
	h := &hookState{reader: hooklog.New(l.LogDir()), layout: l, hasLayout: true}
	for _, dir := range h.legacyDirs() {
		h.reader.AddLegacyDir(dir)
	}
	return h
}

func (h *hookState) legacyDirs() []string {
	var dirs []string
	if fi, err := os.Lstat(h.layout.HooksDir()); err == nil && fi.IsDir() {
		dirs = append(dirs, h.layout.HooksDir())
	}
	if rc, _ := discipline.LoadReceipt(h.layout); rc != nil && rc.MigratedFrom != "" {
		dirs = append(dirs, rc.MigratedFrom) // the moved hooks/enforce directory itself
	}
	return dirs
}

// marks are the hook changes: the ledger's (solid) and the inferred ones.
func (h *hookState) marks(recs []hooklog.Record) []hooklog.Mark {
	var solid []hooklog.Mark
	if led, _ := discipline.ReadLedger(h.layout); len(led) > 0 {
		for _, e := range led {
			switch e.Op {
			case "setup", "upgrade", "uninstall", "reset", "restore":
				label := e.Op
				if e.FromRev != "" || e.ToRev != "" {
					label += " " + orDash(e.FromRev) + " → " + orDash(e.ToRev)
				}
				solid = append(solid, hooklog.Mark{At: e.TS, Label: label})
			}
		}
	}
	inferred := hooklog.InferredMarks(recs, h.legacyDirs(), solid, time.Hour)
	out := append(solid, inferred...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// sessionHooks is the slice of a session's records the row shows.
type sessionHooks struct {
	Holds        int           `json:"holds"`
	Nudges       int           `json:"nudges"`
	Held         *hooklog.Held `json:"held,omitempty"`
	ChecksFailed bool          `json:"checksFailed"`
}

// hooksFor computes a row's counts since its current task file started
// (a new task file resets the held count), or over everything when there
// is no task file.
func hooksFor(reader *hooklog.Reader, row status.Row) sessionHooks {
	if reader == nil || row.ClaudeSessionID == "" {
		return sessionHooks{}
	}
	recs := reader.BySession(row.ClaudeSessionID)
	if len(recs) == 0 {
		return sessionHooks{}
	}
	var since time.Time
	if row.HasTask && row.Task.StartedAt > 0 {
		since = time.Unix(row.Task.StartedAt, 0)
	}
	st := hooklog.Session(recs, since)
	out := sessionHooks{Holds: st.Holds, Nudges: st.Nudges, Held: st.Held}
	// ✗ is the stop guard's last word on this version of the task file: a
	// failed `$` check, unticked checks, or a hand-back let through because
	// the failure was already reported once. An edit to the file after the
	// verdict makes it stale, and the verified marker clears it.
	if st.LastStop != nil && !row.Task.Verified && (row.Task.Modified == 0 || row.Task.Modified <= st.LastStop.TS.Unix()) {
		switch st.LastStop.Decision {
		case "block_done_check_failed", "block_done_unticked_checks", "allow_done_checks_failed_already_nudged":
			out.ChecksFailed = true
		}
	}
	return out
}

// applyHooks fills the hook fields of the DTOs from the snapshot's rows.
func applyHooks(dtos []SessionDTO, snap status.Snapshot, reader *hooklog.Reader) {
	if reader == nil {
		return
	}
	byName := map[string]status.Row{}
	for _, r := range snap.Live {
		byName[r.Name] = r
	}
	for i := range dtos {
		row, ok := byName[dtos[i].Name]
		if !ok {
			continue
		}
		h := hooksFor(reader, row)
		dtos[i].Holds, dtos[i].Nudges, dtos[i].Held, dtos[i].ChecksFailed = h.Holds, h.Nudges, h.Held, h.ChecksFailed
		dtos[i].Verified = row.HasTask && row.Task.Verified
	}
}

// HooksPanelDTO is the stage's hooks panel for one session.
type HooksPanelDTO struct {
	Session string               `json:"session"`
	Since   int64                `json:"since"` // task start (unix seconds), 0 = all records
	Stats   hooklog.SessionStats `json:"stats"`
	Classes []hooklog.ClassLine  `json:"classes"`
	Recent  []hooklog.Record     `json:"recent"` // latest logged decisions, newest first, at most 30
}

// SessionHooks returns the hooks panel for a live session.
func (a *App) SessionHooks(name string) HooksPanelDTO {
	out := HooksPanelDTO{Session: name, Classes: []hooklog.ClassLine{}, Recent: []hooklog.Record{}}
	if a.hooks == nil || a.engine == nil {
		return out
	}
	snap, err := a.engine.Poll(a.now())
	if err != nil {
		return out
	}
	var row status.Row
	found := false
	for _, r := range snap.Live {
		if r.Name == name {
			row, found = r, true
			break
		}
	}
	if !found || row.ClaudeSessionID == "" {
		return out
	}
	a.hooks.reader.Refresh()
	recs := a.hooks.reader.BySession(row.ClaudeSessionID)
	var since time.Time
	if row.HasTask && row.Task.StartedAt > 0 {
		since = time.Unix(row.Task.StartedAt, 0)
		out.Since = row.Task.StartedAt
	}
	out.Stats = hooklog.Session(recs, since)
	if cls := hooklog.Classes(recs, since); cls != nil {
		out.Classes = cls
	}
	for i := len(recs) - 1; i >= 0 && len(out.Recent) < 30; i-- {
		if !recs[i].Silent && (since.IsZero() || !recs[i].TS.Before(since)) {
			out.Recent = append(out.Recent, recs[i])
		}
	}
	return out
}

// RatesDTO is the island's Rates tab.
type RatesDTO struct {
	Window string            `json:"window"` // change | 7d | 30d | all
	Since  int64             `json:"since"`  // unix seconds; 0 = all
	Marks  []hooklog.Mark    `json:"marks"`
	Rows   []hooklog.HookRow `json:"rows"`
	Hidden bool              `json:"hidden"` // some records were left out by the hide/solo rules
}

// visibleRecords applies the hide/solo rules: a decision is placed by its
// cwd; a silent allow has none, so while anything is hidden it is left
// out too (a denominator can leak which project was busy).
func visibleRecords(res *projects.Resolver, recs []hooklog.Record) ([]hooklog.Record, bool) {
	if res == nil || !res.Filtering() {
		return recs, false
	}
	out := make([]hooklog.Record, 0, len(recs))
	hidden := false
	for _, r := range recs {
		if r.Silent || r.Cwd == "" || !res.Visible(r.Cwd) {
			hidden = true
			continue
		}
		out = append(out, r)
	}
	return out, hidden
}

func (a *App) window(name string, marks []hooklog.Mark) (time.Time, string) {
	now := a.now()
	switch name {
	case "7d":
		return now.Add(-7 * 24 * time.Hour), name
	case "30d":
		return now.Add(-30 * 24 * time.Hour), name
	case "all":
		return time.Time{}, name
	}
	if m, ok := hooklog.LastMark(marks); ok {
		return m.At, "change"
	}
	return time.Time{}, "all"
}

// HookRates returns the per-hook rates split at the change marks.
func (a *App) HookRates(window string) RatesDTO {
	out := RatesDTO{Window: window, Marks: []hooklog.Mark{}, Rows: []hooklog.HookRow{}}
	if a.hooks == nil {
		return out
	}
	a.hooks.reader.Refresh()
	all := a.hooks.reader.Records()
	marks := a.hooks.marks(all)
	recs, hidden := visibleRecords(a.resolver(), all)
	since, w := a.window(window, marks)
	out.Window, out.Hidden, out.Marks = w, hidden, marks
	if !since.IsZero() {
		out.Since = since.Unix()
	}
	if rows := hooklog.Rates(recs, since, marks, a.now()); rows != nil {
		out.Rows = rows
	}
	return out
}

// RerunsDTO is the island's Re-runs tab.
type RerunsDTO struct {
	Window string               `json:"window"`
	Since  int64                `json:"since"`
	Rows   []hooklog.ProjectRow `json:"rows"`
	Hidden bool                 `json:"hidden"`
}

// HookReruns returns the test re-run report per project.
func (a *App) HookReruns(window string) RerunsDTO {
	out := RerunsDTO{Window: window, Rows: []hooklog.ProjectRow{}}
	if a.hooks == nil {
		return out
	}
	a.hooks.reader.Refresh()
	all := a.hooks.reader.Records()
	marks := a.hooks.marks(all)
	recs, hidden := visibleRecords(a.resolver(), all)
	since, w := a.window(window, marks)
	out.Window, out.Hidden = w, hidden
	if !since.IsZero() {
		out.Since = since.Unix()
	}
	if rows := hooklog.Reruns(recs, since); rows != nil {
		out.Rows = rows
	}
	return out
}
