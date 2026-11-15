package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/taskfile"
)

// The "looked at" layer over the queue. A session is ready for review when
// its task file says done and nobody has reviewed that version of it (the
// file's mtime): opening the review sheet or clicking Reviewed marks it,
// and a new task (a rewritten file) brings it back. A needs-you ask can be
// snoozed until the session next comes back: the mark holds the ask's
// time, so a newer ask is not snoozed. Marks are per Claude session id, in
// ~/.loom/attention.json.

// reviewWindow: a task finished longer ago than this is history, not
// something waiting for review (else the first run lists months of work).
const reviewWindow = 7 * 24 * time.Hour

type attnMark struct {
	// Reviewed is the task file mtime that was reviewed (unix seconds).
	Reviewed int64 `json:"reviewed,omitempty"`
	// Snoozed holds the ask time it was snoozed at; SnoozedSet says it is
	// on (an ask's time can be 0, unknown).
	Snoozed    int64 `json:"snoozed,omitempty"`
	SnoozedSet bool  `json:"snoozedSet,omitempty"`
}

type attention struct {
	mu    sync.Mutex
	path  string // "" keeps marks in memory only
	marks map[string]attnMark
	now   func() time.Time
}

func newAttention(path string, now func() time.Time) *attention {
	a := &attention{path: path, marks: map[string]attnMark{}, now: now}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &a.marks)
			if a.marks == nil {
				a.marks = map[string]attnMark{}
			}
		}
	}
	return a
}

// save writes the marks; caller holds mu.
func (a *attention) save() error {
	if a.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(a.marks, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// reviewReady: the task is done, recent, and this version is unreviewed.
func (a *attention) reviewReady(sid string, t taskfile.Task, has bool) bool {
	if a == nil || sid == "" || !has || t.Status != "done" || t.Modified <= 0 {
		return false
	}
	if a.now().Sub(time.Unix(t.Modified, 0)) > reviewWindow {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.marks[sid].Reviewed < t.Modified
}

// snoozed: the ask at since is the one that was snoozed.
func (a *attention) snoozed(sid string, since int64) bool {
	if a == nil || sid == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.marks[sid]
	return m.SnoozedSet && m.Snoozed == since
}

func (a *attention) markReviewed(sid string, t taskfile.Task, has bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.marks[sid]
	at := a.now().Unix()
	if has && t.Modified > at {
		at = t.Modified // a clock skewed against the file's
	}
	m.Reviewed = at
	a.marks[sid] = m
	return a.save()
}

func (a *attention) setSnooze(sid string, on bool, since int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.marks[sid]
	m.SnoozedSet, m.Snoozed = on, 0
	if on {
		m.Snoozed = since
	}
	if m == (attnMark{}) {
		delete(a.marks, sid)
	} else {
		a.marks[sid] = m
	}
	return a.save()
}

// applyLive marks the poll's live rows: snoozed asks, and finished turns
// whose task is ready for review (not while it is still running).
func (a *attention) applyLive(dtos []SessionDTO, rows []status.Row) {
	if a == nil {
		return
	}
	byName := make(map[string]status.Row, len(rows))
	for _, r := range rows {
		byName[r.Name] = r
	}
	for i := range dtos {
		r, ok := byName[dtos[i].Name]
		if !ok {
			continue
		}
		sid := r.ClaudeSessionID
		dtos[i].Snoozed = dtos[i].Status == "needs_you" && a.snoozed(sid, r.Since)
		dtos[i].ReviewReady = dtos[i].Status != "running" && a.reviewReady(sid, r.Task, r.HasTask)
	}
}

// applyFinished marks ended sessions: their task status, and whether they
// wait for review.
func (a *attention) applyFinished(dtos []FinishedDTO, sids map[string]string, tasks *taskfile.Cache) {
	if tasks == nil {
		return
	}
	for i := range dtos {
		sid := sids[dtos[i].Name]
		t, has := tasks.Task(sid)
		if has {
			dtos[i].TaskStatus = t.Status
		}
		dtos[i].ReviewReady = a.reviewReady(sid, t, has)
	}
}

// sidFor is a session's Claude id, live or ended.
func (a *App) sidFor(name string) (string, error) {
	if e, ok := a.railRow(name); ok && e.row.ClaudeSessionID != "" {
		return e.row.ClaudeSessionID, nil
	}
	if a.st != nil {
		if r, ok, err := a.st.Get(name); err == nil && ok && r.ClaudeSessionID != "" {
			return r.ClaudeSessionID, nil
		}
	}
	return "", fmt.Errorf("%s has no Claude session", name)
}

// MarkReviewed marks the current version of each session's task file as
// reviewed (the review sheet calls it on open; the home's Reviewed button
// too). Unknown names are skipped; the first error is returned.
func (a *App) MarkReviewed(names []string) error {
	if a.attn == nil {
		return errors.New("attention marks are not available")
	}
	var first error
	for _, n := range names {
		sid, err := a.sidFor(n)
		if err != nil {
			continue
		}
		t, has := a.tasks.Task(sid)
		if err := a.attn.markReviewed(sid, t, has); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Snooze hides a session's current ask until it next comes back; on=false
// brings it back now.
func (a *App) Snooze(name string, on bool) error {
	if a.attn == nil {
		return errors.New("attention marks are not available")
	}
	e, ok := a.railRow(name)
	if !ok || e.row.ClaudeSessionID == "" {
		return fmt.Errorf("%s is not a live session", name)
	}
	if on && e.dto.Status != "needs_you" {
		return fmt.Errorf("%s is not waiting on you", name)
	}
	// the mark is the ask's time: with none, the next ask could not be told
	// from this one and the snooze would never lift
	if on && e.row.Since <= 0 {
		return fmt.Errorf("%s's ask has no time loom can read, so it cannot tell when the next one comes", name)
	}
	return a.attn.setSnooze(e.row.ClaudeSessionID, on, e.row.Since)
}
