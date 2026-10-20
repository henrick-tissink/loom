package hooklog

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Reader keeps the records of a log directory in memory and reads only
// what was appended since the last Refresh. Each file remembers its inode,
// offset and first bytes: a new inode or different first bytes (rotation;
// Linux hands a deleted file's inode straight to the next one) or a size
// below the offset (truncation) restarts that file at zero and drops what
// it held.
//
// Legacy directories (the hooks' old location beside the scripts, or the
// directory loom moved it to) are read once, in full, for history, and
// never re-read.
type Reader struct {
	mu     sync.Mutex
	files  []*logFile
	merged []Record
	dirty  bool
}

type logFile struct {
	path   string
	silent bool
	legacy bool
	loaded bool
	inode  uint64
	offset int64
	head   []byte    // the file's first bytes, read from complete lines
	mtime  time.Time // as of the last read
	recs   []Record
}

// headLen is how much of a file's start identifies it: every line begins
// with its timestamp.
const headLen = 64

// New reads decisions.jsonl and allows.jsonl under logDir.
func New(logDir string) *Reader {
	r := &Reader{dirty: true}
	r.files = append(r.files,
		&logFile{path: filepath.Join(logDir, "decisions.jsonl")},
		&logFile{path: filepath.Join(logDir, "allows.jsonl"), silent: true})
	return r
}

// AddLegacyDir registers a directory whose decisions.jsonl and allows.jsonl
// hold pre-loom history. Missing files are fine.
func (r *Reader) AddLegacyDir(dir string) {
	if dir == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.files {
		if f.legacy && filepath.Dir(f.path) == dir {
			return
		}
	}
	r.files = append(r.files,
		&logFile{path: filepath.Join(dir, "decisions.jsonl"), legacy: true},
		&logFile{path: filepath.Join(dir, "allows.jsonl"), silent: true, legacy: true})
	r.dirty = true
}

// Refresh reads what is new. It never fails: an unreadable file simply
// contributes what it last held.
func (r *Reader) Refresh() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.files {
		if f.legacy && f.loaded {
			continue
		}
		if r.read(f) {
			r.dirty = true
		}
	}
}

// read appends the file's new complete lines; it reports whether anything
// changed.
func (r *Reader) read(f *logFile) bool {
	fi, err := os.Stat(f.path)
	if err != nil {
		f.loaded = true
		if f.offset == 0 && len(f.recs) == 0 {
			return false
		}
		f.offset, f.inode, f.head, f.recs = 0, 0, nil, nil
		return true
	}
	ino := inode(fi)
	changed := false
	if (f.inode != 0 && ino != f.inode) || fi.Size() < f.offset {
		f.offset, f.head, f.recs = 0, nil, nil
		changed = true
	}
	f.inode = ino
	f.loaded = true
	if fi.Size() == f.offset && fi.ModTime().Equal(f.mtime) {
		return changed
	}
	f.mtime = fi.ModTime()
	fh, err := os.Open(f.path)
	if err != nil {
		return changed
	}
	defer fh.Close()
	if len(f.head) > 0 {
		got := make([]byte, len(f.head))
		if _, err := io.ReadFull(fh, got); err != nil || !bytes.Equal(got, f.head) {
			f.offset, f.head, f.recs = 0, nil, nil
			changed = true
		}
	}
	if _, err := fh.Seek(f.offset, io.SeekStart); err != nil {
		return changed
	}
	data, err := io.ReadAll(fh)
	if err != nil && len(data) == 0 {
		return changed
	}
	// Only complete lines count; a line still being written is read next time.
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return changed
	}
	for _, line := range bytes.Split(data[:end], []byte{'\n'}) {
		if rec, ok := Parse(line, f.silent); ok {
			rec.Legacy = f.legacy
			f.recs = append(f.recs, rec)
			changed = true
		}
	}
	if f.offset == 0 {
		f.head = append([]byte(nil), data[:min(headLen, end+1)]...)
	}
	f.offset += int64(end + 1)
	return changed
}

// Records returns every record, oldest first (stable across files). The
// slice is shared: callers must not modify it.
func (r *Reader) Records() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dirty {
		n := 0
		for _, f := range r.files {
			n += len(f.recs)
		}
		out := make([]Record, 0, n)
		for _, f := range r.files {
			out = append(out, f.recs...)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
		r.merged, r.dirty = out, false
	}
	return r.merged
}

// BySession returns the records of one claude session, oldest first.
// SubagentStop records carry the parent's session id (the hooks are handed
// the parent's registry), so a helper's holds land on the session that
// spawned it.
func (r *Reader) BySession(sid string) []Record {
	if sid == "" {
		return nil
	}
	var out []Record
	for _, rec := range r.Records() {
		if rec.Session == sid {
			out = append(out, rec)
		}
	}
	return out
}
