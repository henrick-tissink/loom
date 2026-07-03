// Package memory's indexer (Task 4, spec §2.1/§2.4/§2.5/§2.7/§7.2) sweeps
// $CLAUDE_CONFIG_DIR/projects incrementally: per source FILE (not per
// session — subagent files arrive while a parent session is still live), a
// fingerprint (size, mtime) decides whether to re-extract. Changed files
// are re-extracted (extract.go) and their docs replaced (store.
// ReplaceFileDocs); the extraction is then merged into the session's
// transcripts row. Main files own title/ask/outcome/first_ts/msg_count/cwd;
// subagent files (attributed to the PARENT session_id — their containing
// directory name) merge only Files (deduped, parent's files first) and
// extend last_ts. Rows whose main file has vanished are kept (spec §2.5)
// with file_missing flagged, never deleted.
package memory

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/transcript"
)

// Status is a point-in-time snapshot of the indexer's progress (spec §6,
// shown as the search frame's right annotation: "N sessions · indexing…").
// Swept/Changed reflect the MOST RECENTLY COMPLETED (or in-progress) sweep
// pass only — they are reset at the start of each Sweep, not accumulated
// across the indexer's lifetime. Total is the number of known sessions
// (transcripts rows) as of that sweep.
type Status struct {
	Swept   int64
	Changed int64
	Total   int64
	Active  bool
}

// Indexer performs incremental sweeps of the claude archive into the memory
// store. Safe for concurrent use: Status/Sweep may be called from different
// goroutines (e.g. Run's background loop and a UI poll reading Status).
type Indexer struct {
	st  *store.Store
	ccd string

	// sweepMu serializes Sweep bodies so overlapping calls (e.g. a manual
	// Sweep racing Run's ticker) degrade to a queued delay rather than two
	// concurrent passes contending over the same rowid ranges.
	sweepMu sync.Mutex

	swept   atomic.Int64
	changed atomic.Int64
	total   atomic.Int64
	active  atomic.Bool
}

// NewIndexer constructs an Indexer against st, sweeping
// claudeConfigDir/projects.
func NewIndexer(st *store.Store, claudeConfigDir string) *Indexer {
	return &Indexer{st: st, ccd: claudeConfigDir}
}

// Status returns a snapshot of the indexer's current/last-sweep counters.
func (ix *Indexer) Status() Status {
	return Status{
		Swept:   ix.swept.Load(),
		Changed: ix.changed.Load(),
		Total:   ix.total.Load(),
		Active:  ix.active.Load(),
	}
}

// Run sweeps immediately, then every `every` duration, until ctx is
// cancelled (spec §2.4: "Sweep at startup + every 10 min, background
// goroutine"). Sweep errors are not fatal to the loop — a bad sweep (e.g. a
// transient stat error) just waits for the next tick.
func (ix *Indexer) Run(ctx context.Context, every time.Duration) {
	ix.Sweep() //nolint:errcheck // best-effort; loop continues regardless
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ix.Sweep() //nolint:errcheck
		}
	}
}

// Sweep performs one full incremental pass over the archive. Safe to call
// repeatedly (each call is independent; unchanged files are cheap fingerprint
// checks). Returns an error only for sweep-level failures (e.g. the projects
// directory itself can't be listed); per-file problems (a file vanishing
// between listing and stat, a single file's extraction failing) are skipped
// so one bad file never aborts the rest of the sweep — the same
// never-abort discipline extract.go applies per line.
func (ix *Indexer) Sweep() error {
	ix.sweepMu.Lock()
	defer ix.sweepMu.Unlock()

	ix.active.Store(true)
	defer ix.active.Store(false)

	var swept, changed int64

	projectsDir := filepath.Join(ix.ccd, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			ix.swept.Store(0)
			ix.changed.Store(0)
			ix.total.Store(0)
			return nil
		}
		return err
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirName := e.Name()
		dirPath := filepath.Join(projectsDir, dirName)

		mainFiles, _ := filepath.Glob(filepath.Join(dirPath, "*.jsonl"))
		sort.Strings(mainFiles)
		for _, f := range mainFiles {
			sessionID := strings.TrimSuffix(filepath.Base(f), ".jsonl")
			swept++
			did, err := ix.indexFile(f, sessionID, dirName, false)
			if err != nil {
				continue // skip this file; sweep continues
			}
			if did {
				changed++
			}
		}

		subEntries, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, se := range subEntries {
			if !se.IsDir() {
				continue
			}
			sessionID := se.Name() // the session-uuid path component
			subRoot := filepath.Join(dirPath, se.Name())
			var subFiles []string
			_ = filepath.WalkDir(subRoot, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil // skip unreadable entries, keep walking
				}
				if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
					subFiles = append(subFiles, path)
				}
				return nil
			})
			sort.Strings(subFiles)
			for _, f := range subFiles {
				swept++
				did, err := ix.indexFile(f, sessionID, dirName, true)
				if err != nil {
					continue
				}
				if did {
					changed++
				}
			}
		}
	}

	// file_missing pass (spec §2.5): check EVERY known session's main file,
	// not just ones touched this pass — a project dir's main file can vanish
	// entirely between sweeps and thus not appear in any directory listing.
	all, err := ix.st.ListTranscripts()
	if err != nil {
		return err
	}
	for _, t := range all {
		mainPath := filepath.Join(projectsDir, t.ProjectDir, t.SessionID+".jsonl")
		_, statErr := os.Stat(mainPath)
		missing := os.IsNotExist(statErr)
		if missing != t.FileMissing {
			if err := ix.st.SetFileMissing(t.SessionID, missing); err != nil {
				return err
			}
		}
	}

	ix.swept.Store(swept)
	ix.changed.Store(changed)
	ix.total.Store(int64(len(all)))
	return nil
}

// indexFile fingerprints one source file (main or subagent), skipping it
// when unchanged. A changed file is re-extracted, its docs replaced, and the
// extraction merged into its session's transcripts row. Returns
// indexed=true when the file was (re)extracted this call.
func (ix *Indexer) indexFile(path, sessionID, projectDir string, isSubagent bool) (indexed bool, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	size, mtime := fi.Size(), fi.ModTime().Unix()

	existing, ok, err := ix.st.GetIndexedFile(path)
	if err != nil {
		return false, err
	}
	if ok && existing.Size == size && existing.Mtime == mtime {
		return false, nil // fingerprint unchanged: skip
	}

	roleOverride := ""
	if isSubagent {
		roleOverride = "agent"
	}
	ex, err := ExtractFile(path, roleOverride)
	if err != nil {
		return false, err
	}

	idxFile := store.IndexedFile{Path: path, SessionID: sessionID, Size: size, Mtime: mtime}
	if ok {
		idxFile.FirstRowid = existing.FirstRowid
		idxFile.LastRowid = existing.LastRowid
	}
	if err := ix.st.ReplaceFileDocs(idxFile, ex.Docs); err != nil {
		return false, err
	}

	if err := ix.mergeTranscript(sessionID, projectDir, ex, isSubagent); err != nil {
		return false, err
	}
	return true, nil
}

// mergeTranscript read-modify-writes the session's transcripts row (spec
// §2.4/§2.7): main-file extractions own title/ask/outcome/first_ts/
// msg_count/cwd outright; subagent extractions merge only Files (deduped,
// parent's files first) and extend last_ts. llm_summary/summary_at are
// untouched here regardless of what's set on the struct — UpsertTranscript
// itself never overwrites them (SetLLMSummary is the sole writer).
func (ix *Indexer) mergeTranscript(sessionID, projectDir string, ex Extraction, isSubagent bool) error {
	cur, ok, err := ix.st.GetTranscript(sessionID)
	if err != nil {
		return err
	}
	if !ok {
		// Session-uuid dirs may exist without a main jsonl (a subagent ran
		// before/without ever seeing its parent's main file indexed, or the
		// main file was pruned) — attribute anyway; the row just starts with
		// an empty ask (spec §2.1 discovery note).
		cur = store.Transcript{SessionID: sessionID, ProjectDir: projectDir}
	}
	prevTitle, prevFiles := cur.Title, cur.Files

	curFiles := splitFiles(cur.Files)

	if !isSubagent {
		cur.ProjectDir = projectDir
		cur.Title = ex.Title
		cur.Ask = ex.Ask
		cur.Outcome = ex.Outcome
		cur.FirstTS = ex.FirstTS
		cur.LastTS = ex.LastTS
		// msg_count = main-file docs only: subagent docs would otherwise
		// inflate the visible message count shown in the UI for a session
		// whose own transcript only has a handful of turns.
		cur.MsgCount = int64(ex.MsgCount)
		cur.Cwd = selectCwd(ex.Cwds, projectDir, cur.Cwd)
		curFiles = mergeMainFiles(ex.Files, curFiles)
		cur.FileMissing = false // this file exists — we just read it
	} else {
		if cur.ProjectDir == "" {
			cur.ProjectDir = projectDir
		}
		curFiles = appendFiles(curFiles, ex.Files)
		if ex.LastTS > cur.LastTS {
			cur.LastTS = ex.LastTS
		}
	}
	cur.Files = joinFiles(curFiles)

	if err := ix.st.UpsertTranscript(cur); err != nil {
		return err
	}

	if cur.Title != prevTitle || cur.Files != prevFiles {
		return ix.indexMetaDoc(sessionID, cur.Title, cur.Files, cur.LastTS)
	}
	return nil
}

// indexMetaDoc keeps a synthetic per-session FTS doc in sync with the
// transcript's title and touched-files list (spec §2.2 coverage gap):
// titles and the touched-files list are never emitted as their own doc by
// ExtractFile (Title/Files are distillation fields, not indexed Docs), so
// without this, filename/title searches would never match a session. One
// doc — role "meta" — is written per session under the pseudo-file path
// "loom://meta/<session_id>", replaced via store.ReplaceFileDocs (the same
// delete-by-fingerprint-then-reinsert path real files use) whenever the
// merged title or files differ from what was last indexed; skipped
// entirely when both are empty. Size/Mtime are synthesized (len(content),
// 0) since there's no real file to fingerprint — callers never fingerprint
// this path against a stat call.
//
// This path is NEVER treated as a real file by Sweep's file_missing pass:
// that pass iterates ListTranscripts (not indexed_files) and stats a path
// derived from project_dir+session_id+".jsonl" — it never reads or stats
// any indexed_files.Path, so "loom://meta/..." rows can never be flagged
// file_missing or otherwise confused with a real transcript file.
func (ix *Indexer) indexMetaDoc(sessionID, title, files string, lastTS int64) error {
	if title == "" && files == "" {
		return nil
	}
	content := CleanText(title + "\n" + files)
	path := "loom://meta/" + sessionID

	idxFile := store.IndexedFile{Path: path, SessionID: sessionID, Size: int64(len(content)), Mtime: 0}
	if existing, ok, err := ix.st.GetIndexedFile(path); err != nil {
		return err
	} else if ok {
		idxFile.FirstRowid = existing.FirstRowid
		idxFile.LastRowid = existing.LastRowid
	}

	return ix.st.ReplaceFileDocs(idxFile, []store.Doc{{Content: content, Role: "meta", TS: lastTS}})
}

// selectCwd implements spec §2.7: prefer the cwd whose loom path-encoding
// (transcript.ProjectDirName) equals this session's project_dir; else the
// first cwd seen; else fall back to whatever cwd the row already had (a main
// file with zero cwd fields — never seen in real transcripts, but a
// defensive guard against clobbering a known-good value with "").
func selectCwd(cwds []string, projectDir, prev string) string {
	for _, c := range cwds {
		if transcript.ProjectDirName(c) == projectDir {
			return c
		}
	}
	if len(cwds) > 0 {
		return cwds[0]
	}
	return prev
}

// mergeMainFiles rebuilds the Files list when the MAIN file is (re)indexed:
// the main file's own touched files come first (in its order), followed by
// any previously-known files (typically subagent-contributed) not already
// present, in their prior order. Rebuilding from ex.Files each time (rather
// than just appending) keeps "parent's files first" true even when the main
// file is reprocessed after subagent files already contributed entries.
func mergeMainFiles(mainFiles, prevFiles []string) []string {
	seen := make(map[string]bool, len(mainFiles)+len(prevFiles))
	out := make([]string, 0, len(mainFiles)+len(prevFiles))
	for _, f := range mainFiles {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, f := range prevFiles {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// appendFiles adds a subagent's touched files after whatever's already
// known, deduping (spec: subagent extractions merge ONLY Files, keeping the
// parent's files first — prevFiles already has the parent's files at the
// front by construction of mergeMainFiles).
func appendFiles(prevFiles, subFiles []string) []string {
	seen := make(map[string]bool, len(prevFiles)+len(subFiles))
	out := make([]string, 0, len(prevFiles)+len(subFiles))
	for _, f := range prevFiles {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, f := range subFiles {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func splitFiles(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func joinFiles(files []string) string {
	return strings.Join(files, "\n")
}
