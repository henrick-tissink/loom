package status

import (
	"path/filepath"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
)

const activeWindow = 3 * time.Second

type Row struct {
	store.SessionRow
	Status   Status
	LastTool string
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
}

func NewEngine(tm *tmux.Client, st *store.Store, claudeConfigDir string) *Engine {
	return &Engine{tm: tm, st: st, ccd: claudeConfigDir,
		readers: map[string]*transcript.Reader{}}
}

func (e *Engine) Poll(now time.Time) (Snapshot, error) {
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
		if ps.Dead {
			st := "done"
			if ps.ExitCode != 0 {
				st = "error"
			}
			_ = e.st.MarkEnded(s.Name, st, int64(ps.ExitCode), now.Unix())
			_ = e.tm.KillSession(s.Name) // reap after recording (spec §6)
			delete(e.readers, s.Name)
			continue
		}
		if _, ok, _ := e.st.Get(s.Name); !ok {
			// adopt orphan: rebuild what we can from tmux alone (spec §3)
			id, _ := session.SessionIDFromTmuxName(s.Name)
			_ = e.st.Upsert(store.SessionRow{
				Name: s.Name, ClaudeSessionID: id,
				ProjectLabel: filepath.Base(ps.CurrentPath), Cwd: ps.CurrentPath,
				CreatedAt: now.Unix(), EndedAt: -1, ExitCode: -1,
				LastStatus: string(Unknown),
			})
		}
		aliveNames = append(aliveNames, s.Name)
		activity[s.Name] = s.Activity
	}

	// store rows that claim live but have no tmux backing → history (never deleted)
	if err := e.st.MarkLiveOrphansEnded(aliveNames, now.Unix()); err != nil {
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
		ts, tool, _ := rd.Poll() // read errors degrade to prior state: best-effort
		paneActive := now.Unix()-activity[r.Name] <= int64(activeWindow/time.Second)
		st := Fuse(ts, paneActive)
		if string(st) != r.LastStatus {
			_ = e.st.SetStatus(r.Name, string(st))
			r.LastStatus = string(st)
		}
		live = append(live, Row{SessionRow: r, Status: st, LastTool: tool})
	}

	recent, err := e.st.Recent(10)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Live: live, Recent: recent}, nil
}

// GC drops cached readers for names not in the given set (call rarely; cheap).
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
