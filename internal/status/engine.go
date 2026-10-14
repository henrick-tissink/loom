package status

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/henricktissink/loom/internal/ccsessions"
	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/taskfile"
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
	Status    Status
	LastTool  string
	Activity  int64 // unix seconds of last tmux session activity; 0 = unknown
	Title     string
	CtxTokens int64
	// WaitingFor is what the pane waits on according to claude's registry
	// ("dialog open"), or "shell"; "" when the registry has nothing to add.
	WaitingFor string
	// Ask is the text claude last ended a turn with and Since when the
	// session started waiting (unix seconds; 0 = unknown). Both are set only
	// while Status is NeedsYou.
	Ask   string
	Since int64
	// Task is the finish line the session pinned in its task file, if any
	// (the operator's hooks write it; see internal/taskfile).
	Task    taskfile.Task
	HasTask bool
	// Checkpoint is the session's last hand-over note, if any.
	Checkpoint taskfile.Checkpoint
	// External marks a claude loom did not start and has no pane for: a
	// process in claude's registry running in some terminal outside loom's
	// tmux server. Its row is synthesised from the registry and its
	// transcript each poll and is never stored; there is nothing to attach
	// to, reply into, or kill.
	External bool
}

type Snapshot struct {
	Live   []Row
	Recent []store.SessionRow

	// NewlyNeedsYou lists the SESSION NAMES (the store PK, and this engine's
	// own identity key) of sessions that flipped to NeedsYou on THIS poll
	// (last_status != needs_you && new == needs_you). Computed before
	// SetStatus persists the new status, so the once-only property holds
	// naturally: the next poll observes LastStatus already needs_you and
	// doesn't re-fire.
	//
	// Names, not pre-rendered "ProjectLabel · Title" strings (spec §4): a
	// label cannot be attributed back to anything, because ProjectLabel is
	// filepath.Base(cwd) for adopted orphans — two same-basename repos in
	// different directories collide, which the multi-repo model makes
	// routine. Consumers join against Live (same snapshot, same poll — every
	// name here is present there) to recover cwd and label, and render the
	// display string themselves. This keeps the engine ignorant of projects
	// (§6.2a); it reports identity, not grouping.
	NewlyNeedsYou []string
}

// Engine performs one reconcile pass per Poll. tmux is the source of truth
// for live sessions; the store owns history (spec §6).
type Engine struct {
	tm      *tmux.Client
	st      *store.Store
	ccd     string
	readers map[string]*transcript.Reader
	tasks   *taskfile.Cache
	// extStatus is the last status per external row, which the store holds
	// for loom's own rows: it is what makes the needs-you bell once-only.
	extStatus map[string]string

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
		readers:   map[string]*transcript.Reader{},
		tasks:     taskfile.NewCache(taskfile.Dir(claudeConfigDir)),
		extStatus: map[string]string{}}
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

	// one tmux exec per poll: every session's active pane with its liveness
	// and the session activity stamp. list-sessions + list-panes per session
	// was 1+N spawns every 1.5 s.
	panes, err := e.tm.ListPanes()
	if err != nil {
		return Snapshot{}, err
	}

	var aliveNames []string
	activity := map[string]int64{}
	for _, p := range panes {
		if _, ok := session.SessionIDFromTmuxName(p.Name); !ok {
			continue // not ours
		}
		ps := p.PaneStatus
		if row, ok, _ := e.st.Get(p.Name); !ok {
			// adopt orphan BEFORE branching on Dead: a tmux session found on
			// startup with no store row must be recorded before it can be
			// reaped, dead or alive (spec §6 "record before reap").
			id, _ := session.SessionIDFromTmuxName(p.Name)
			_ = e.st.Upsert(store.SessionRow{
				Name: p.Name, ClaudeSessionID: id,
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
			_ = e.st.SetStatus(p.Name, string(Unknown))
		}
		if ps.Dead {
			st := "done"
			if ps.ExitCode != 0 {
				st = "error"
			}
			_ = e.st.MarkEnded(p.Name, st, int64(ps.ExitCode), now.Unix())
			_ = e.tm.KillSession(p.Name) // reap after recording (spec §6)
			continue
		}
		aliveNames = append(aliveNames, p.Name)
		activity[p.Name] = p.Activity
	}

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
	// claude's own registry is the only place a pane's CURRENT session id
	// is written once /clear or an in-pane resume mints a new one; read it
	// once per poll (a handful of tiny files) and rebind below.
	entries := ccsessions.List(ccsessions.Dir(e.ccd))
	reg := map[string]ccsessions.Entry{}
	for _, ent := range entries {
		if ent.TmuxSession == "" {
			continue
		}
		if prev, dup := reg[ent.TmuxSession]; dup && prev.UpdatedAt >= ent.UpdatedAt {
			continue
		}
		reg[ent.TmuxSession] = ent
	}

	var live []Row
	var newly []string
	liveIDs := map[string]bool{}
	for _, r := range liveRows {
		liveIDs[r.ClaudeSessionID] = true
		ent, hasEnt := reg[r.Name]
		if hasEnt && ent.SessionID != r.ClaudeSessionID {
			// rebind: the pane's claude now runs a different session id, so
			// the transcript this row was tailing is abandoned and its status
			// would freeze forever. Follow the registry, persist it (resume,
			// search and summaries key on this column), and drop the reader
			// so the next lookup opens the live transcript. The registry's
			// cwd is not adopted: loom launched claude in r.Cwd, and the
			// project grouping must not move because of a /clear.
			if err := e.st.SetClaudeSessionID(r.Name, ent.SessionID); err == nil {
				r.ClaudeSessionID = ent.SessionID
				delete(e.readers, r.Name)
			}
		}
		rd, ok := e.readers[r.Name]
		if !ok {
			rd = transcript.NewReader(transcript.Path(e.ccd, r.Cwd, r.ClaudeSessionID))
			e.readers[r.Name] = rd
		}
		rs, _ := rd.Poll() // read errors degrade to prior state: best-effort
		if rs.Title != "" && rs.Title != r.Title {
			_ = e.st.SetTitle(r.Name, rs.Title)
			r.Title = rs.Title
		}
		paneActive := now.Unix()-activity[r.Name] <= int64(activeWindow/time.Second)
		st := Fuse(rs.State, paneActive)
		waitingFor := ""
		if hasEnt {
			st, waitingFor = Merge(st, ent.Status, ent.WaitingFor)
		}
		if st == NeedsYou && r.LastStatus != string(NeedsYou) {
			newly = append(newly, r.Name)
		}
		if string(st) != r.LastStatus {
			_ = e.st.SetStatus(r.Name, string(st))
			r.LastStatus = string(st)
		}
		live = append(live, e.row(r, rs, st, waitingFor, activity[r.Name], ent, hasEnt))
	}

	// external sessions: every live registry entry that is not in a loom
	// pane. Shown, never stored: the registry file is its liveness and the
	// transcript its state, so there is nothing for the store to retire.
	// A conversation a loom row already tails (the same id resumed in a
	// terminal) is not shown twice.
	keep := append([]string(nil), aliveNames...)
	seen := map[string]bool{}
	for _, ent := range entries {
		if _, loom := session.SessionIDFromTmuxName(ent.TmuxSession); loom || ent.Cwd == "" || liveIDs[ent.SessionID] {
			continue
		}
		name := "ext-" + ent.SessionID
		if seen[name] {
			continue
		}
		seen[name] = true
		keep = append(keep, name)
		r := store.SessionRow{Name: name, ClaudeSessionID: ent.SessionID,
			ProjectLabel: filepath.Base(ent.Cwd), Cwd: ent.Cwd,
			CreatedAt: ent.StartedAt / 1000, EndedAt: -1, ExitCode: -1,
			LastStatus: e.extStatus[name]}
		rd, ok := e.readers[name]
		if !ok {
			rd = transcript.NewReader(transcript.Path(e.ccd, ent.Cwd, ent.SessionID))
			e.readers[name] = rd
		}
		rs, _ := rd.Poll()
		r.Title = rs.Title
		st, waitingFor := Merge(Fuse(rs.State, false), ent.Status, ent.WaitingFor)
		if st == NeedsYou && e.extStatus[name] != string(NeedsYou) {
			newly = append(newly, name)
		}
		e.extStatus[name] = string(st)
		r.LastStatus = string(st)
		row := e.row(r, rs, st, waitingFor, 0, ent, true)
		row.External = true
		live = append(live, row)
	}
	for name := range e.extStatus {
		if !seen[name] {
			delete(e.extStatus, name)
		}
	}

	// drop cached readers for anything not observed live this poll, so
	// sessions that vanish from tmux without a dead pane (e.g. an external
	// `tmux kill-session`), and external claudes that exit, don't leak
	// their Reader forever.
	e.GC(keep)

	recent, err := e.st.Recent(10)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Live: live, Recent: recent, NewlyNeedsYou: newly}, nil
}

// row assembles a live row from its store row, transcript snapshot, fused
// status and registry entry: the ask and its age, and the task file and
// checkpoint the session's hooks wrote.
func (e *Engine) row(r store.SessionRow, rs transcript.ReaderSnapshot, st Status, waitingFor string,
	activity int64, ent ccsessions.Entry, hasEnt bool) Row {
	row := Row{SessionRow: r, Status: st, LastTool: rs.LastTool,
		Activity: activity, Title: r.Title, CtxTokens: rs.CtxTokens,
		WaitingFor: waitingFor}
	if st == NeedsYou {
		// the wait is measured from the ask; a registry "waiting" (a
		// dialog) is timed by the registry, which is what flipped it.
		row.Ask, row.Since = rs.Ask, rs.AskAt
		if hasEnt && ent.Status == "waiting" && ent.StatusUpdatedAt > 0 {
			row.Since = ent.StatusUpdatedAt / 1000
		}
	}
	row.Task, row.HasTask = e.tasks.Task(r.ClaudeSessionID)
	row.Checkpoint, _ = e.tasks.Checkpoint(r.ClaudeSessionID)
	return row
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
