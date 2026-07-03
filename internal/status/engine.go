package status

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
)

const activeWindow = 3 * time.Second

// orphanGrace is how long a store row is protected from
// MarkLiveOrphansEnded even if its tmux session isn't (yet) observed live —
// guards the launch-vs-reconcile race (finding 2a): a session just launched
// can be polled before its tmux session is visible to ListSessions.
const orphanGrace = 5 * time.Second

type Row struct {
	store.SessionRow
	Status   Status
	LastTool string
	Activity int64 // unix seconds of last tmux session activity; 0 = unknown
}

type Snapshot struct {
	Live   []Row
	Recent []store.SessionRow
}

// Engine performs one reconcile pass per Poll. tmux is the source of truth
// for live sessions; the store owns history (spec §6).
type Engine struct {
	tm      *tmux.Client
	st      *store.Store
	ccd     string
	readers map[string]*transcript.Reader

	// mu serializes Poll end-to-end. Defense in depth against finding 1: the
	// UI must never fire two concurrent Poll goroutines against the same
	// Engine (that's the primary fix, in ui/app.go), but this mutex makes a
	// concurrent-Poll bug a serialization delay instead of a `fatal error:
	// concurrent map writes` crash on e.readers.
	mu sync.Mutex

	// pollDepth/maxPollDepth are test-only instrumentation (no behavioral
	// effect): pollDepth tracks how many goroutines are currently inside the
	// mu-guarded critical section of Poll, and maxPollDepth records the
	// high-water mark. TestPollConcurrentSafe asserts maxPollDepth stays at
	// 1 for the whole concurrent run — `go test -race` alone doesn't catch a
	// deleted mu.Lock/Unlock here because the store's single SQLite
	// connection already serializes most of the interesting access,
	// masking the race. This gauge asserts mutual exclusion directly.
	pollDepth    atomic.Int32
	maxPollDepth atomic.Int32
}

func NewEngine(tm *tmux.Client, st *store.Store, claudeConfigDir string) *Engine {
	return &Engine{tm: tm, st: st, ccd: claudeConfigDir,
		readers: map[string]*transcript.Reader{}}
}

func (e *Engine) Poll(now time.Time) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// test-only depth gauge (see field docs above): records how many
	// goroutines are concurrently inside this critical section.
	d := e.pollDepth.Add(1)
	defer e.pollDepth.Add(-1)
	for {
		cur := e.maxPollDepth.Load()
		if d <= cur || e.maxPollDepth.CompareAndSwap(cur, d) {
			break
		}
	}

	sessions, err := e.tm.ListSessions()
	if err != nil {
		return Snapshot{}, err
	}

	var aliveNames []string
	activity := map[string]int64{}
	for _, s := range sessions {
		if _, ok := session.SessionIDFromTmuxName(s.Name); !ok {
			continue // not ours
		}
		ps, err := e.tm.PaneStatus(s.Name)
		if err != nil {
			continue // raced a kill; next poll settles it
		}
		if row, ok, _ := e.st.Get(s.Name); !ok {
			// adopt orphan BEFORE branching on Dead: a tmux session found on
			// startup with no store row must be recorded before it can be
			// reaped, dead or alive (spec §6 "record before reap").
			id, _ := session.SessionIDFromTmuxName(s.Name)
			_ = e.st.Upsert(store.SessionRow{
				Name: s.Name, ClaudeSessionID: id,
				ProjectLabel: filepath.Base(ps.CurrentPath), Cwd: ps.CurrentPath,
				CreatedAt: now.Unix(), EndedAt: -1, ExitCode: -1,
				LastStatus: string(Unknown),
			})
		} else if row.LastStatus == string(Done) || row.LastStatus == string(Error) {
			// resurrection (finding 2b): a launch-vs-reconcile race can let a
			// poll observe the store row as already retired (terminal status)
			// while its tmux session is, in fact, still alive — e.g. the
			// session finished quickly and MarkLiveOrphansEnded fired on a
			// poll that raced the tmux session's own teardown/creation. tmux
			// is the source of truth for liveness (spec §6): if it says the
			// pane is alive, the row must not stay stuck in a terminal
			// status, or the session becomes permanently invisible to Live().
			_ = e.st.SetStatus(s.Name, string(Unknown))
		}
		if ps.Dead {
			st := "done"
			if ps.ExitCode != 0 {
				st = "error"
			}
			_ = e.st.MarkEnded(s.Name, st, int64(ps.ExitCode), now.Unix())
			_ = e.tm.KillSession(s.Name) // reap after recording (spec §6)
			continue
		}
		aliveNames = append(aliveNames, s.Name)
		activity[s.Name] = s.Activity
	}

	// drop cached readers for anything not observed live this poll, so
	// sessions that vanish from tmux without a dead pane (e.g. an external
	// `tmux kill-session`) don't leak their Reader forever.
	e.GC(aliveNames)

	// store rows that claim live but have no tmux backing → history (never
	// deleted). graceUnix protects rows created within orphanGrace of now so
	// a poll that races a session's own launch never retires it (finding 2a).
	if err := e.st.MarkLiveOrphansEnded(aliveNames, now.Add(-orphanGrace).Unix(), now.Unix()); err != nil {
		return Snapshot{}, err
	}

	liveRows, err := e.st.Live()
	if err != nil {
		return Snapshot{}, err
	}
	var live []Row
	for _, r := range liveRows {
		rd, ok := e.readers[r.Name]
		if !ok {
			rd = transcript.NewReader(transcript.Path(e.ccd, r.Cwd, r.ClaudeSessionID))
			e.readers[r.Name] = rd
		}
		rs, _ := rd.Poll() // read errors degrade to prior state: best-effort
		paneActive := now.Unix()-activity[r.Name] <= int64(activeWindow/time.Second)
		st := Fuse(rs.State, paneActive)
		if string(st) != r.LastStatus {
			_ = e.st.SetStatus(r.Name, string(st))
			r.LastStatus = string(st)
		}
		live = append(live, Row{SessionRow: r, Status: st, LastTool: rs.LastTool, Activity: activity[r.Name]})
	}

	recent, err := e.st.Recent(10)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Live: live, Recent: recent}, nil
}

// GC drops cached readers for names not in the given set. Called once per
// Poll with the set of tmux-alive session names, so readers for anything
// that vanished from tmux (dead pane or external kill) don't leak.
func (e *Engine) GC(names []string) {
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	for n := range e.readers {
		if !keep[n] {
			delete(e.readers, n)
		}
	}
}
