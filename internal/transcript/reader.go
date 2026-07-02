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

func (r *Reader) Poll() (State, string, error) {
	f, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return StateUnknown, "", nil // not written yet: fine
		}
		return r.cls.State(), r.cls.LastTool(), err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return r.cls.State(), r.cls.LastTool(), err
	}
	if info.Size() < r.offset {
		// truncated/replaced: start over
		r.offset = 0
		r.partial = nil
		r.cls = Classifier{}
	}
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return r.cls.State(), r.cls.LastTool(), err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return r.cls.State(), r.cls.LastTool(), err
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
	return r.cls.State(), r.cls.LastTool(), nil
}
