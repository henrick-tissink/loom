package transcript

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"
)

// Limit is a usage limit a session stopped on: claude writes a synthetic
// assistant record with "error":"rate_limit" and the quota's reset time
// (seen on 2.1.283):
//
//	{"type":"assistant","error":"rate_limit","isApiErrorMessage":true,
//	 "quotaLimits":{"status":"rejected","resetsAt":1790946000,"rateLimitType":"seven_day"},
//	 "message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your weekly limit · resets …"}]}}
type Limit struct {
	ResetsAt time.Time
	Type     string // five_hour | seven_day | …
	Text     string
}

type limitRec struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Error       string `json:"error"`
	QuotaLimits *struct {
		ResetsAt      int64  `json:"resetsAt"`
		RateLimitType string `json:"rateLimitType"`
	} `json:"quotaLimits"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// LastLimit reports whether the transcript's last conversation record (user
// or assistant, outside subagents) is a usage-limit stop, and its reset. A
// prompt typed after the stop ends it.
func LastLimit(path string) (Limit, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Limit{}, false, nil
		}
		return Limit{}, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Limit{}, false, err
	}
	size := fi.Size()
	for window := int64(64 << 10); ; window *= 2 {
		if window > size {
			window = size
		}
		buf := make([]byte, window)
		if _, err := f.ReadAt(buf, size-window); err != nil && err != io.EOF {
			return Limit{}, false, err
		}
		lines := bytes.Split(buf, []byte("\n"))
		if window < size && len(lines) > 0 {
			lines = lines[1:] // the first line may be cut
		}
		for i := len(lines) - 1; i >= 0; i-- {
			l := bytes.TrimSpace(lines[i])
			if len(l) == 0 || !(bytes.Contains(l, []byte(`"type":"assistant"`)) || bytes.Contains(l, []byte(`"type":"user"`))) {
				continue
			}
			var r limitRec
			if json.Unmarshal(l, &r) != nil || (r.Type != "assistant" && r.Type != "user") || r.IsSidechain {
				continue
			}
			if r.Type != "assistant" || r.Error != "rate_limit" || r.QuotaLimits == nil || r.QuotaLimits.ResetsAt <= 0 {
				return Limit{}, false, nil
			}
			lim := Limit{ResetsAt: time.Unix(r.QuotaLimits.ResetsAt, 0), Type: r.QuotaLimits.RateLimitType}
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(r.Message.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "text" {
						lim.Text = b.Text
					}
				}
			}
			return lim, true, nil
		}
		if window == size || window >= 8<<20 {
			return Limit{}, false, nil
		}
	}
}
