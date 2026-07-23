// Orchestrator claims (slice 2, spec docs/superpowers/specs/
// 2026-07-22-orchestrator-brief-design.md §9): one row per project naming the
// orchestrator session that project currently has, or last had.
//
// This file owns the rows only. Assembly, spawn and drift live in
// internal/orchestrator, above the store — nothing here knows what a brief is.
//
// The singleton (§2, "at most one orchestrator per project at a time") is
// enforced by the primary key and by ClaimOrchestrator's conflict predicate,
// NOT by a UI guard: two Loom instances against one DB is a supported state
// and a UI guard is per-process. The row is otherwise a pointer, not a record —
// §9 is explicit that everything in it is rederivable by scanning live sessions
// for the `orch` tag, so losing the table costs a singleton guarantee for one
// poll interval and never a session or a note.
package store

import "database/sql"

// Orchestrator is one row of `orchestrators` (migration v10).
//
// SessionName == "" means a claim is in flight: §7 takes the claim BEFORE the
// launch (the AdvanceRunCAS claim-before-side-effect discipline), and
// Launcher.Launch mints the session id itself, so there is a window in which
// the claim exists and the session does not. EndedAt == -1 means live.
type Orchestrator struct {
	ProjectRoot     string
	SessionName     string
	ClaudeSessionID string
	SpawnedAt       int64
	EndedAt         int64 // -1 = live (or claim in flight)
}

const orchCols = "project_root, session_name, claude_session_id, spawned_at, ended_at"

// ClaimOrchestrator takes the per-project singleton claim. It is the whole of
// §2's guarantee, and it is a compare-and-swap in the same sense
// AdvanceRunCAS is: the claim is atomic with respect to any other writer, and
// it happens BEFORE the launch, so two Loom instances pressing Spawn at the
// same moment produce exactly one launch.
//
// The conflict predicate is the interesting part. A terminated orchestrator's
// row is KEPT (§9: the overview says "last orchestrator ran Tuesday"), so the
// row's mere existence cannot mean "occupied". Occupancy is `ended_at = -1`:
//
//   - no row            → INSERT, claimed
//   - row, ended_at !=-1 → the previous orchestrator is finished; DO UPDATE
//     overwrites it with the new claim, claimed
//   - row, ended_at ==-1 → live, or a claim in flight; the upsert's WHERE
//     rejects it, RowsAffected()==0, NOT claimed
//
// A rejected claim returns the existing row so the caller can name the winner
// in its error (§7.3: "ErrOrchestratorExists, naming the existing session").
// The error value itself is the caller's — internal/orchestrator owns the spawn
// vocabulary; the store reports the fact, not the policy.
//
// The read of the existing row shares the claim's transaction: reading it
// afterwards would be a second moment, and the row could have been swept
// between the two, leaving the caller with a refusal it cannot explain.
func (s *Store) ClaimOrchestrator(root string, now int64) (claimed bool, existing Orchestrator, err error) {
	err = s.tx(func(tx *sql.Tx) error {
		res, e := tx.Exec(`INSERT INTO orchestrators (`+orchCols+`)
			VALUES (?, '', '', ?, -1)
			ON CONFLICT(project_root) DO UPDATE SET
				session_name='', claude_session_id='',
				spawned_at=excluded.spawned_at, ended_at=-1
			WHERE orchestrators.ended_at != -1`, root, now)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n > 0 {
			claimed = true
			existing = Orchestrator{ProjectRoot: root, SpawnedAt: now, EndedAt: -1}
			return nil
		}
		return tx.QueryRow("SELECT "+orchCols+" FROM orchestrators WHERE project_root=?", root).
			Scan(&existing.ProjectRoot, &existing.SessionName, &existing.ClaudeSessionID,
				&existing.SpawnedAt, &existing.EndedAt)
	})
	if err != nil {
		return false, Orchestrator{}, err
	}
	return claimed, existing, nil
}

// BindOrchestratorSession fills in the session identity once the launch has
// actually produced one (§7 step 6). It is guarded on `session_name = ”` so a
// late-arriving bind cannot overwrite a NEWER claim's session: the sweep can
// delete a stranded claim and a second spawn can win the root while the first
// launch is still in flight, and the loser must not then stamp its dead session
// name over the winner's live one.
//
// claimed=false means the claim this bind belonged to is gone. The caller has a
// live session with no row — visible via its `orch` tag, adopted by the next
// sweep — which is the same shape as workflows' disclosed stranded-launch
// window, and strictly better than a silently mislabelled singleton.
func (s *Store) BindOrchestratorSession(root, sessionName, claudeID string) (claimed bool, err error) {
	res, err := s.db.Exec(
		`UPDATE orchestrators SET session_name=?, claude_session_id=?
		 WHERE project_root=? AND session_name='' AND ended_at=-1`,
		sessionName, claudeID, root)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetOrchestratorClaudeSessionID records the conversation id, which the
// transcript correlates asynchronously and which is therefore usually unknown
// at bind time. Unguarded on purpose: it is keyed on the session name, so it
// can only ever touch the row that still names that session.
func (s *Store) SetOrchestratorClaudeSessionID(root, sessionName, claudeID string) error {
	_, err := s.db.Exec(
		"UPDATE orchestrators SET claude_session_id=? WHERE project_root=? AND session_name=?",
		claudeID, root, sessionName)
	return err
}

// EndOrchestrator retires a live claim when its session row goes terminal.
// Stamped by internal/orchestrator's sweep, never by status.Engine — the engine
// stays project-unaware (slice 1 §6.2a), which is why this is a store call and
// not a hook.
//
// CAS on `ended_at = -1`: two Loom instances sweep the same dead session on
// their own poll loops, and the second must not overwrite the first's
// timestamp. "Last orchestrator ran Tuesday" has to mean Tuesday.
func (s *Store) EndOrchestrator(root string, endedAt int64) (claimed bool, err error) {
	res, err := s.db.Exec(
		"UPDATE orchestrators SET ended_at=? WHERE project_root=? AND ended_at=-1", endedAt, root)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) GetOrchestrator(root string) (Orchestrator, bool, error) {
	var o Orchestrator
	err := s.db.QueryRow("SELECT "+orchCols+" FROM orchestrators WHERE project_root=?", root).
		Scan(&o.ProjectRoot, &o.SessionName, &o.ClaudeSessionID, &o.SpawnedAt, &o.EndedAt)
	if err == sql.ErrNoRows {
		return Orchestrator{}, false, nil
	}
	if err != nil {
		return Orchestrator{}, false, err
	}
	return o, true, nil
}

// ListOrchestrators returns every row, live and retired. It exists so the GUI
// can populate N projects' orchestrator sub-objects from ONE query joined in
// memory inside the existing ListProjectDetails call (§10) — a per-project
// lookup would be the N+1 that section forbids.
func (s *Store) ListOrchestrators() ([]Orchestrator, error) {
	rows, err := s.db.Query("SELECT " + orchCols + " FROM orchestrators ORDER BY project_root")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Orchestrator
	for rows.Next() {
		var o Orchestrator
		if err := rows.Scan(&o.ProjectRoot, &o.SessionName, &o.ClaudeSessionID,
			&o.SpawnedAt, &o.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SweepStaleOrchestratorClaims deletes claims that never became a session:
// `session_name = ”`, still live, and older than the caller's cutoff (§7 uses
// 60 s). This is the recovery for the disclosed failure mode — a launch that
// fails after a successful claim would otherwise lock the project out of ever
// spawning again.
//
// The asymmetry is deliberate and stated in §7: a stranded claim row is cheap
// and recoverable, a double orchestrator is not. So the claim is taken first
// and reaped late, never the reverse.
//
// It deletes rather than ending, because a claim that never produced a session
// is not history — "last orchestrator ran Tuesday" must not be answered by a
// launch that never happened.
func (s *Store) SweepStaleOrchestratorClaims(olderThan int64) (int64, error) {
	res, err := s.db.Exec(
		"DELETE FROM orchestrators WHERE session_name='' AND ended_at=-1 AND spawned_at < ?",
		olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AdoptOrchestrator writes a row for a live `orch`-tagged session the sweep
// found with no claim behind it (§9's adopt-before-reap, the engine's own
// idiom). It never clobbers a live claim — that is what the conflict predicate
// checks — so an adoption racing a legitimate spawn loses, and the tagged
// session it found is then a second orchestrator the sweep will report rather
// than silently rebadge.
func (s *Store) AdoptOrchestrator(o Orchestrator) (adopted bool, err error) {
	res, err := s.db.Exec(`INSERT INTO orchestrators (`+orchCols+`)
		VALUES (?,?,?,?,?)
		ON CONFLICT(project_root) DO UPDATE SET
			session_name=excluded.session_name,
			claude_session_id=excluded.claude_session_id,
			spawned_at=excluded.spawned_at, ended_at=excluded.ended_at
		WHERE orchestrators.ended_at != -1`,
		o.ProjectRoot, o.SessionName, o.ClaudeSessionID, o.SpawnedAt, o.EndedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
