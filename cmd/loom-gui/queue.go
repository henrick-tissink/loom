package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
	"github.com/henricktissink/loom/internal/usage"
)

// The launch queue and resume-at-reset. A queued run is a New-session
// launch held until a time, or until the plan windows have room; it must
// carry a Done-when, written into its seed, so the digest can say what
// the run proved rather than only that it started. A session stopped on a
// usage limit (transcript.LastLimit) can be armed to get "continue" once
// the limit has reset. The scheduler ticks while loom runs; a laptop that
// slept launches what fell due on wake, marked late. State lives in
// ~/.loom/queue.json.

// QueuedRun is one held launch.
type QueuedRun struct {
	ID         string `json:"id"`
	Project    string `json:"project"` // the launch target path
	Seed       string `json:"seed"`
	DoneWhen   string `json:"doneWhen"`
	Model      string `json:"model,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Worktree   bool   `json:"worktree,omitempty"`
	At         int64  `json:"at"` // unix seconds; 0: as soon as the plan windows have room
	Created    int64  `json:"created"`
	LaunchedAt int64  `json:"launchedAt,omitempty"`
	LateBy     int64  `json:"lateBy,omitempty"` // seconds past At when it launched
	Session    string `json:"session,omitempty"`
	Err        string `json:"err,omitempty"`
	Cancelled  bool   `json:"cancelled,omitempty"`
}

// ResumeRec is a session armed to continue after its usage limit resets.
type ResumeRec struct {
	Session   string `json:"session"`
	ResetsAt  int64  `json:"resetsAt"`
	Armed     bool   `json:"armed"`
	ArmedAt   int64  `json:"armedAt"`
	ResumedAt int64  `json:"resumedAt,omitempty"`
	Err       string `json:"err,omitempty"`
}

// QueueSessionDTO is what the digest shows of a session.
type QueueSessionDTO struct {
	Title        string `json:"title"`
	Status       string `json:"status"`
	TaskStatus   string `json:"taskStatus"`
	TaskDone     int    `json:"taskDone"`
	TaskTotal    int    `json:"taskTotal"`
	Verified     bool   `json:"verified"`
	ChecksFailed bool   `json:"checksFailed"`
	Outcome      string `json:"outcome"`
}

// QueueDTO is the Queue page.
type QueueDTO struct {
	Runs    []QueuedRun `json:"runs"`
	Resumes []ResumeRec `json:"resumes"`
	Held    string      `json:"held,omitempty"` // why due runs wait, if they do
	// Sessions are the runs' and resumes' sessions, live or ended, by name.
	Sessions map[string]QueueSessionDTO `json:"sessions"`
}

type queueState struct {
	Runs    []QueuedRun `json:"runs"`
	Resumes []ResumeRec `json:"resumes"`
}

type queueStore struct {
	mu   sync.Mutex
	path string
	s    queueState
	// lastTick is the previous tick (not saved): a gap longer than
	// tickGap means loom was not running or the machine slept.
	lastTick int64
}

func newQueueStore(path string) *queueStore {
	q := &queueStore{path: path}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &q.s)
		}
	}
	return q
}

// update applies f to a copy of the state and saves it atomically.
func (q *queueStore) update(f func(s *queueState) error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	b, _ := json.Marshal(q.s)
	var next queueState
	_ = json.Unmarshal(b, &next)
	if err := f(&next); err != nil {
		return err
	}
	if q.path != "" {
		if err := os.MkdirAll(filepath.Dir(q.path), 0o700); err != nil {
			return err
		}
		out, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return err
		}
		tmp := q.path + ".tmp"
		if err := os.WriteFile(tmp, out, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, q.path); err != nil {
			return err
		}
	}
	q.s = next
	return nil
}

func (q *queueStore) snapshot() queueState {
	q.mu.Lock()
	defer q.mu.Unlock()
	b, _ := json.Marshal(q.s)
	var s queueState
	_ = json.Unmarshal(b, &s)
	return s
}

// queueHoldPct: due runs wait while any plan window is this full.
const queueHoldPct = 95

// lateAfter: a run launched this long past its time is marked late.
const lateAfter = 5 * 60

// tickGap: ticks further apart than this mean loom was not running or
// the machine slept.
const tickGap = 90

// resumeGrace is how long after a reset loom waits before continuing.
const resumeGrace = 60

// queueSeed is the seed a queued run launches with: its Done-when goes in
// as the finish line the task file must carry.
func queueSeed(seed, doneWhen string) string {
	return strings.TrimSpace(seed) + "\n\nDone when (put these in the task file's Done when, and prove each before you stop):\n" + strings.TrimSpace(doneWhen)
}

func newQueueID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// QueueRun holds a launch until at (unix seconds), or with at 0 until the
// plan windows have room. A Done-when is required.
func (a *App) QueueRun(project, model, mode, seed, doneWhen string, worktree bool, at int64) error {
	if a.queue == nil {
		return errors.New("queue unavailable")
	}
	if strings.TrimSpace(seed) == "" {
		return errors.New("a queued run needs a seed prompt")
	}
	if strings.TrimSpace(doneWhen) == "" {
		return errors.New("a queued run needs a Done-when: what must be true, and how to check it")
	}
	if _, err := buildRecipe(a.launchableTargets(), project, model, mode, seed, nil); err != nil {
		return err
	}
	if at != 0 && at < a.now().Unix()-60 {
		return errors.New("that time has passed")
	}
	r := QueuedRun{ID: newQueueID(), Project: project, Seed: seed, DoneWhen: doneWhen, Model: model, Mode: mode,
		Worktree: worktree, At: at, Created: a.now().Unix()}
	return a.queue.update(func(s *queueState) error { s.Runs = append(s.Runs, r); return nil })
}

// CancelQueued cancels a run that has not launched.
func (a *App) CancelQueued(id string) error {
	if a.queue == nil {
		return errors.New("queue unavailable")
	}
	return a.queue.update(func(s *queueState) error {
		for i := range s.Runs {
			if s.Runs[i].ID == id {
				if s.Runs[i].LaunchedAt != 0 {
					return errors.New("it has already launched")
				}
				s.Runs[i].Cancelled = true
				return nil
			}
		}
		return errors.New("no such queued run")
	})
}

// sessionLimit is a live session's usage-limit stop, or false.
func (a *App) sessionLimit(name string) (transcript.Limit, bool) {
	if a.st == nil {
		return transcript.Limit{}, false
	}
	row, ok, err := a.st.Get(name)
	if err != nil || !ok {
		return transcript.Limit{}, false
	}
	return a.rowLimit(row)
}

func (a *App) rowLimit(row store.SessionRow) (transcript.Limit, bool) {
	if row.EndedAt >= 0 || row.ClaudeSessionID == "" {
		return transcript.Limit{}, false
	}
	p := a.transcriptFor(row.Cwd, row.ClaudeSessionID)
	if p == "" {
		return transcript.Limit{}, false
	}
	return a.limitTails.read(p)
}

// limitCache keeps each transcript's LastLimit answer until the file
// changes, so the 1.5 s poll reads an idle session's tail once.
type limitCache struct {
	mu sync.Mutex
	m  map[string]limitEntry
}

type limitEntry struct {
	size int64
	mod  time.Time
	lim  transcript.Limit
	ok   bool
}

func (c *limitCache) read(p string) (transcript.Limit, bool) {
	fi, err := os.Stat(p)
	if err != nil {
		return transcript.Limit{}, false
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]limitEntry{}
	}
	e, hit := c.m[p]
	c.mu.Unlock()
	if hit && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
		return e.lim, e.ok
	}
	lim, ok, err := transcript.LastLimit(p)
	if err != nil {
		return transcript.Limit{}, false
	}
	c.mu.Lock()
	c.m[p] = limitEntry{size: fi.Size(), mod: fi.ModTime(), lim: lim, ok: ok}
	c.mu.Unlock()
	return lim, ok
}

// launchQueued launches a queued run as New session would.
func (a *App) launchQueued(r QueuedRun) (string, error) {
	if a.queueLaunch != nil {
		return a.queueLaunch(r)
	}
	return a.LaunchSession(r.Project, r.Model, r.Mode, queueSeed(r.Seed, r.DoneWhen), nil, r.Worktree)
}

// sendPrompt types a prompt into a session (SendPrompt, which refuses
// while a dialog is open).
func (a *App) sendPrompt(name, text string) error {
	if a.queueSend != nil {
		return a.queueSend(name, text)
	}
	return a.SendPrompt(name, text)
}

// LimitDTO is a row's usage-limit stop.
type LimitDTO struct {
	ResetsAt int64  `json:"resetsAt"`
	Type     string `json:"type"`
	Text     string `json:"text"`
	Armed    bool   `json:"armed"`
}

// limitFor is the row's limit stop and whether it is armed, or nil.
func (a *App) limitFor(name string) *LimitDTO {
	lim, ok := a.sessionLimit(name)
	if !ok {
		return nil
	}
	d := &LimitDTO{ResetsAt: lim.ResetsAt.Unix(), Type: lim.Type, Text: lim.Text}
	if a.queue != nil {
		for _, r := range a.queue.snapshot().Resumes {
			if r.Session == name && r.ResetsAt == d.ResetsAt && r.Armed && r.ResumedAt == 0 {
				d.Armed = true
			}
		}
	}
	return d
}

// ArmResume arms (or disarms) a session stopped on a usage limit to get
// "continue" once the limit has reset.
func (a *App) ArmResume(name string, on bool) error {
	if a.queue == nil {
		return errors.New("queue unavailable")
	}
	lim, ok := a.sessionLimit(name)
	if on && !ok {
		return errors.New("the session is not stopped on a usage limit")
	}
	return a.queue.update(func(s *queueState) error {
		for i := range s.Resumes {
			r := &s.Resumes[i]
			if r.Session == name && r.ResumedAt == 0 && (!ok || r.ResetsAt == lim.ResetsAt.Unix()) {
				r.Armed = on
				r.Err = ""
				return nil
			}
		}
		if on {
			s.Resumes = append(s.Resumes, ResumeRec{Session: name, ResetsAt: lim.ResetsAt.Unix(), Armed: true, ArmedAt: a.now().Unix()})
		}
		return nil
	})
}

// limitsFull says why launches wait: a plan window at or past
// queueHoldPct that has not reset yet, or a live session stopped on a
// usage limit that has not reset (the only witness when the status-line
// tap is off). "" when there is room as far as loom can tell.
func (a *App) limitsFull(now time.Time) string {
	if a.limits != nil {
		for _, w := range a.limits() {
			if w.UsedPct >= queueHoldPct && w.ResetsAt > now.Unix() {
				return fmt.Sprintf("the %s window is %d%% used until %s", w.Name, int(w.UsedPct), time.Unix(w.ResetsAt, 0).Format("Jan 2 15:04"))
			}
		}
	}
	if a.st == nil {
		return ""
	}
	rows, err := a.st.Live()
	if err != nil {
		return ""
	}
	for _, r := range rows {
		if lim, ok := a.rowLimit(r); ok && lim.ResetsAt.Unix() > now.Unix() {
			name := r.Title
			if name == "" {
				name = r.Name
			}
			return fmt.Sprintf("%s is stopped on a usage limit until %s", name, lim.ResetsAt.Format("Jan 2 15:04"))
		}
	}
	return ""
}

// readLimits is the plan windows from claude's last status line.
func readLimits(loomDir string) func() []UsageWindowDTO {
	return func() []UsageWindowDTO {
		l, ok, err := usage.ReadLimits(usage.StatusFile(loomDir))
		if err != nil || !ok {
			return nil
		}
		out := make([]UsageWindowDTO, 0, len(l.Windows))
		for _, w := range l.Windows {
			out = append(out, UsageWindowDTO{Name: w.Name, UsedPct: w.UsedPct, ResetsAt: w.ResetsAt})
		}
		return out
	}
}

// queueTick launches due runs and continues armed sessions whose limit has
// reset. It runs every 30 s, and once on wake (the ticker catches up).
func (a *App) queueTick(now time.Time) {
	if a.queue == nil {
		return
	}
	held := a.limitsFull(now)
	st := a.queue.snapshot()
	a.queue.mu.Lock()
	gap := a.queue.lastTick == 0 || now.Unix()-a.queue.lastTick > tickGap
	a.queue.lastTick = now.Unix()
	a.queue.mu.Unlock()
	for _, r := range st.Runs {
		if r.Cancelled || r.LaunchedAt != 0 || r.Err != "" {
			continue
		}
		if (r.At != 0 && now.Unix() < r.At) || held != "" {
			continue
		}
		name, err := a.launchQueued(r)
		_ = a.queue.update(func(s *queueState) error {
			for i := range s.Runs {
				if s.Runs[i].ID != r.ID {
					continue
				}
				if err != nil {
					s.Runs[i].Err = err.Error()
					return nil
				}
				s.Runs[i].LaunchedAt, s.Runs[i].Session = now.Unix(), name
				// late only across a gap: a run the limits held is not late
				if gap && r.At != 0 && now.Unix()-r.At > lateAfter {
					s.Runs[i].LateBy = now.Unix() - r.At
				}
			}
			return nil
		})
	}
	for _, r := range st.Resumes {
		if !r.Armed || r.ResumedAt != 0 || r.Err != "" || now.Unix() < r.ResetsAt+resumeGrace {
			continue
		}
		err := a.resumeAfterLimit(r)
		if errors.Is(err, errDialogWait) {
			continue // try again next tick, once the dialog is answered
		}
		_ = a.queue.update(func(s *queueState) error {
			for i := range s.Resumes {
				x := &s.Resumes[i]
				if x.Session == r.Session && x.ResetsAt == r.ResetsAt {
					if err != nil {
						x.Err, x.Armed = err.Error(), false
					} else {
						x.ResumedAt = now.Unix()
					}
				}
			}
			return nil
		})
	}
}

// errDialogWait: a dialog is open, so the resume waits for the next tick.
var errDialogWait = errors.New("a dialog is open")

// resumeAfterLimit sends "continue" to a session still stopped on the
// limit it was armed for, with no dialog open.
func (a *App) resumeAfterLimit(r ResumeRec) error {
	lim, ok := a.sessionLimit(r.Session)
	if !ok {
		return errors.New("not resumed: the session ended or is no longer stopped on the limit")
	}
	if lim.ResetsAt.Unix() != r.ResetsAt {
		return errors.New("not resumed: it stopped on a later limit; arm it again")
	}
	if a.dialogOpen(r.Session) != "" {
		return errDialogWait
	}
	return a.sendPrompt(r.Session, "continue")
}

// Queue lists queued runs and resumes, newest first.
func (a *App) Queue() QueueDTO {
	if a.queue == nil {
		return QueueDTO{Runs: []QueuedRun{}, Resumes: []ResumeRec{}, Sessions: map[string]QueueSessionDTO{}}
	}
	st := a.queue.snapshot()
	d := QueueDTO{Runs: []QueuedRun{}, Resumes: []ResumeRec{}, Held: a.limitsFull(a.now())}
	for i := len(st.Runs) - 1; i >= 0; i-- {
		d.Runs = append(d.Runs, st.Runs[i])
	}
	for i := len(st.Resumes) - 1; i >= 0; i-- {
		d.Resumes = append(d.Resumes, st.Resumes[i])
	}
	d.Sessions = a.queueSessions(d)
	return d
}

// queueSessions joins the digest to its sessions: a live one as the rail
// last showed it, an ended one from the store, its task file and the
// indexed outcome. Unknown names are left out ("session gone").
func (a *App) queueSessions(d QueueDTO) map[string]QueueSessionDTO {
	out := map[string]QueueSessionDTO{}
	if a.st == nil {
		return out
	}
	var names []string
	for _, r := range d.Runs {
		if r.Session != "" {
			names = append(names, r.Session)
		}
	}
	for _, r := range d.Resumes {
		names = append(names, r.Session)
	}
	rows := map[string]store.SessionRow{}
	var sids []string
	for _, n := range names {
		if _, seen := rows[n]; seen {
			continue
		}
		row, ok, err := a.st.Get(n)
		if err != nil || !ok {
			continue
		}
		rows[n] = row
		if row.ClaudeSessionID != "" {
			sids = append(sids, row.ClaudeSessionID)
		}
	}
	trs, _ := a.st.GetTranscripts(sids)
	for n, row := range rows {
		q := QueueSessionDTO{Title: row.Title, Outcome: trs[row.ClaudeSessionID].Outcome}
		if e, ok := a.railRow(n); ok && row.EndedAt < 0 {
			q.Title, q.Status = e.dto.Title, e.dto.Status
			q.TaskStatus, q.TaskDone, q.TaskTotal = e.dto.TaskStatus, e.dto.TaskDone, e.dto.TaskTotal
			q.Verified, q.ChecksFailed = e.dto.Verified, e.dto.ChecksFailed
			if q.Outcome == "" {
				q.Outcome = e.dto.TaskOutcome
			}
			out[n] = q
			continue
		}
		switch {
		case row.EndedAt < 0:
			q.Status = row.LastStatus
		case row.ExitCode > 0:
			q.Status = "error"
		default:
			q.Status = "done"
		}
		if t, ok := a.tasks.Task(row.ClaudeSessionID); ok {
			q.TaskStatus, q.TaskDone, q.TaskTotal, q.Verified = t.Status, t.Done, t.Total, t.Verified
			if q.Outcome == "" {
				q.Outcome = t.Outcome
			}
		}
		out[n] = q
	}
	return out
}

// runQueue ticks the scheduler until stop closes.
func (a *App) runQueue(stop <-chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	a.queueTick(time.Now())
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			a.queueTick(now)
		}
	}
}
