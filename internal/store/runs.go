// Workflow run store (Phase 3, spec docs/superpowers/specs/
// 2026-07-03-workflows-design.md §2.4/§2.6/§2.7): persisted state for saved
// multi-step workflow chains (migration v5, store.go). AdvanceRunCAS is THE
// concurrency primitive — every advance is a compare-and-swap claim BEFORE
// any launch, closing double-press and two-instance races (spec §2.6).
package store

import (
	"database/sql"
	"encoding/json"
)

// RunRow is one row of `workflow_runs`. SessionNames is (de)serialized as a
// JSON array in the session_names TEXT column — INVARIANT:
// len(SessionNames) == StepIdx+1, which callers (the Runner, Task 2)
// maintain by construction; the store only enforces the CAS on write, not
// this invariant.
type RunRow struct {
	ID           int64
	Name         string
	DefJSON      string
	StepIdx      int64
	SessionNames []string
	PendingSeed  string
	Status       string
	CreatedAt    int64
	UpdatedAt    int64
}

const runCols = "id, name, def_json, step_idx, session_names, pending_seed, status, created_at, updated_at"

// InsertRun creates a run row BEFORE step 1 launches (spec §2.10: the run id
// is needed for the step-1 tag `wf:<name>#<runID>:step1`). step_idx=0,
// session_names='[]' (empty JSON array, never NULL/”), status='running'.
// Step 1 itself is recorded via the runner's first
// AdvanceRunCAS(id, 0, 0, [name1], ...) — a same-index write.
func (s *Store) InsertRun(name, defJSON string, now int64) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO workflow_runs (name, def_json, step_idx, session_names, pending_seed, status, created_at, updated_at)
		 VALUES (?, ?, 0, '[]', '', 'running', ?, ?)`,
		name, defJSON, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetRun(id int64) (RunRow, bool, error) {
	r, err := scanRun(s.db.QueryRow("SELECT "+runCols+" FROM workflow_runs WHERE id=?", id))
	if err == sql.ErrNoRows {
		return RunRow{}, false, nil
	}
	if err != nil {
		return RunRow{}, false, err
	}
	return r, true, nil
}

// ActiveRuns returns every status='running' run, newest first (the RUNS
// dashboard section, spec §4).
func (s *Store) ActiveRuns() ([]RunRow, error) {
	rows, err := s.db.Query("SELECT " + runCols + " FROM workflow_runs WHERE status='running' ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunRow
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRunStatus transitions a run to 'done' or 'abandoned' (spec §2.7/§2.12).
// Unlike AdvanceRunCAS this is not conditioned on the current status: a
// terminal-step "finish" confirm and an abandon can both be issued directly
// by the UI once the caller has already decided the transition is valid.
func (s *Store) SetRunStatus(id int64, status string, now int64) error {
	_, err := s.db.Exec("UPDATE workflow_runs SET status=?, updated_at=? WHERE id=?", status, now, id)
	return err
}

// FinishRunCAS marks run id 'done' iff its step_idx still matches
// expectedStepIdx AND status is still 'running' (spec §2.7 CAS gate,
// closing the same TOCTOU window AdvanceRunCAS closes for advances):
// finishCmd's fresh pre-read decides "this run is still at the snapshot the
// confirm showed, done is safe" but that decision and the write are two
// separate moments — an advance or a second finish can land in between.
// claimed=false (RowsAffected==0) means the snapshot went stale meanwhile;
// the row is left completely untouched, same discipline as AdvanceRunCAS.
func (s *Store) FinishRunCAS(id int64, expectedStepIdx int64, now int64) (claimed bool, err error) {
	res, err := s.db.Exec(
		`UPDATE workflow_runs SET status='done', updated_at=? WHERE id=? AND step_idx=? AND status='running'`,
		now, id, expectedStepIdx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// AbandonRunCAS marks run id 'abandoned' iff it is still status='running'
// (spec §2.12 narrowing): closes the abandon-vs-finish TOCTOU window — an
// abandon confirm opened against a running snapshot must not silently
// overwrite a run that finished concurrently. claimed=false covers BOTH
// "already abandoned" and "already done"; the caller (Runner.Abandon)
// re-reads the row to tell those apart — the former is a harmless
// idempotent no-op, the latter is the narrowing violation this exists to
// catch.
func (s *Store) AbandonRunCAS(id int64, now int64) (claimed bool, err error) {
	res, err := s.db.Exec(
		`UPDATE workflow_runs SET status='abandoned', updated_at=? WHERE id=? AND status='running'`,
		now, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClearPendingSeed marks a run's undelivered continue/fork seed as
// delivered (spec §2.9).
func (s *Store) ClearPendingSeed(id int64, now int64) error {
	_, err := s.db.Exec("UPDATE workflow_runs SET pending_seed='', updated_at=? WHERE id=?", now, id)
	return err
}

// AdvanceRunCAS is THE concurrency primitive (spec §2.6): the claim happens
// BEFORE any launch. It atomically writes step_idx=newIdx,
// session_names=names, pending_seed=pendingSeed iff id matches AND
// step_idx==expectedStepIdx (the snapshot the caller acted on) AND
// status='running'. RowsAffected()==0 means the snapshot is stale — a
// double-press, a second instance, or a run that moved to done/abandoned —
// so claimed=false and the caller must not launch anything; the row is left
// completely untouched.
func (s *Store) AdvanceRunCAS(id int64, expectedStepIdx, newIdx int64, names []string, pendingSeed string, now int64) (claimed bool, err error) {
	if names == nil {
		names = []string{}
	}
	namesJSON, err := json.Marshal(names)
	if err != nil {
		return false, err
	}
	res, err := s.db.Exec(
		`UPDATE workflow_runs SET step_idx=?, session_names=?, pending_seed=?, updated_at=?
		 WHERE id=? AND step_idx=? AND status='running'`,
		newIdx, string(namesJSON), pendingSeed, now, id, expectedStepIdx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

type runScanner interface{ Scan(dest ...any) error }

func scanRun(row runScanner) (RunRow, error) {
	var r RunRow
	var namesJSON string
	if err := row.Scan(&r.ID, &r.Name, &r.DefJSON, &r.StepIdx, &namesJSON,
		&r.PendingSeed, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return RunRow{}, err
	}
	if err := json.Unmarshal([]byte(namesJSON), &r.SessionNames); err != nil {
		return RunRow{}, err
	}
	if r.SessionNames == nil {
		r.SessionNames = []string{}
	}
	return r, nil
}
