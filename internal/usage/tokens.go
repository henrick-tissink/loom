package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Bucket is tokens by kind. Cache reads are kept apart: they are most of
// any long session's input and cost a fraction of the rest.
type Bucket struct {
	Input      int64
	Output     int64
	CacheWrite int64
	CacheRead  int64
}

func (b *Bucket) add(o Bucket) {
	b.Input += o.Input
	b.Output += o.Output
	b.CacheWrite += o.CacheWrite
	b.CacheRead += o.CacheRead
}

// Totals is a window's tokens, overall and by model and by project (the
// session's cwd).
type Totals struct {
	All       Bucket
	ByModel   map[string]Bucket
	ByProject map[string]Bucket
}

type event struct {
	id    string // the API message id; "" when absent
	at    int64  // unix seconds
	model string
	cwd   string
	b     Bucket
}

type fileAgg struct {
	size   int64
	mod    time.Time
	events []event
}

// Scanner sums the tokens in claude's transcripts (subagents' included,
// they spend the same quota). Each file is parsed again only when it
// changed, so a rescan costs the files written since the last one.
type Scanner struct {
	Root  string // the claude projects directory
	mu    sync.Mutex
	files map[string]*fileAgg
}

// Scan returns the totals for each window ending at now; a transcript last
// written before the longest window is not read at all.
func (s *Scanner) Scan(now time.Time, windows ...time.Duration) ([]Totals, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string]*fileAgg{}
	}
	var longest time.Duration
	for _, w := range windows {
		longest = max(longest, w)
	}
	oldest := now.Add(-longest)
	seen := map[string]bool{}
	err := filepath.WalkDir(s.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == s.Root {
				return err
			}
			return nil // an unreadable corner never fails the scan
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.ModTime().Before(oldest) {
			return nil
		}
		seen[path] = true
		if a, ok := s.files[path]; ok && a.size == fi.Size() && a.mod.Equal(fi.ModTime()) {
			return nil
		}
		evs, err := parseFile(path)
		if err != nil {
			return nil
		}
		s.files[path] = &fileAgg{size: fi.Size(), mod: fi.ModTime(), events: evs}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for p := range s.files {
		if !seen[p] {
			delete(s.files, p)
		}
	}
	out := make([]Totals, len(windows))
	for i, w := range windows {
		from := now.Add(-w).Unix()
		t := Totals{ByModel: map[string]Bucket{}, ByProject: map[string]Bucket{}}
		// a forked session copies earlier messages into its own file
		ids := map[string]bool{}
		for _, a := range s.files {
			for _, e := range a.events {
				if e.at < from || e.at > now.Unix() || (e.id != "" && ids[e.id]) {
					continue
				}
				if e.id != "" {
					ids[e.id] = true
				}
				t.All.add(e.b)
				m := t.ByModel[e.model]
				m.add(e.b)
				t.ByModel[e.model] = m
				p := t.ByProject[e.cwd]
				p.add(e.b)
				t.ByProject[e.cwd] = p
			}
		}
		out[i] = t
	}
	return out, nil
}

// parseFile reads one transcript's assistant usage. claude writes one
// record per content block of a message, each carrying the message's full
// usage, so a message counts once (by its id).
func parseFile(path string) ([]event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	seen := map[string]bool{}
	var evs []event
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, []byte(`"usage"`)) && bytes.Contains(line, []byte(`"assistant"`)) {
			var rec struct {
				Type      string `json:"type"`
				Timestamp string `json:"timestamp"`
				Cwd       string `json:"cwd"`
				Message   *struct {
					ID    string `json:"id"`
					Model string `json:"model"`
					Usage *struct {
						Input      int64 `json:"input_tokens"`
						Output     int64 `json:"output_tokens"`
						CacheWrite int64 `json:"cache_creation_input_tokens"`
						CacheRead  int64 `json:"cache_read_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &rec) == nil && rec.Type == "assistant" && rec.Message != nil && rec.Message.Usage != nil {
				m := rec.Message
				ts, terr := time.Parse(time.RFC3339Nano, rec.Timestamp)
				// "<synthetic>" marks claude's own placeholder messages: no API call
				if terr == nil && m.Model != "<synthetic>" && (m.ID == "" || !seen[m.ID]) {
					if m.ID != "" {
						seen[m.ID] = true
					}
					u := m.Usage
					evs = append(evs, event{id: m.ID, at: ts.Unix(), model: m.Model, cwd: rec.Cwd,
						b: Bucket{Input: u.Input, Output: u.Output, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead}})
				}
			}
		}
		if err == io.EOF {
			return evs, nil
		}
		if err != nil {
			return evs, err
		}
	}
}
