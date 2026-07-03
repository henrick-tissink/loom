package transcript

import (
	"bytes"
	"io"
	"os"
)

// Reader incrementally consumes a growing JSONL transcript. Only complete
// newline-terminated lines are parsed (spec §6); a trailing partial line is
// buffered until its newline arrives.
type Reader struct {
	path    string
	offset  int64
	partial []byte
	cls     Classifier
}

func NewReader(path string) *Reader { return &Reader{path: path} }

// ReaderSnapshot is the classifier state after consuming all complete lines.
type ReaderSnapshot struct {
	State     State
	LastTool  string
	Title     string
	CtxTokens int64
}

func (r *Reader) snap() ReaderSnapshot {
	return ReaderSnapshot{
		State: r.cls.State(), LastTool: r.cls.LastTool(),
		Title: r.cls.Title(), CtxTokens: r.cls.CtxTokens(),
	}
}

func (r *Reader) Poll() (ReaderSnapshot, error) {
	f, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return ReaderSnapshot{State: StateUnknown}, nil // not written yet: fine
		}
		return r.snap(), err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return r.snap(), err
	}
	if info.Size() < r.offset {
		// truncated/replaced: start over
		r.offset = 0
		r.partial = nil
		r.cls = Classifier{}
	}
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return r.snap(), err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return r.snap(), err
	}
	r.offset += int64(len(data))

	buf := append(r.partial, data...)
	for {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			break
		}
		line := buf[:nl]
		buf = buf[nl+1:]
		if len(bytes.TrimSpace(line)) > 0 {
			r.cls.Feed(line)
		}
	}
	r.partial = buf
	return r.snap(), nil
}
