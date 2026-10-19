package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/usage"
	"github.com/henricktissink/loom/internal/worktree"
)

type UsageWindowDTO struct {
	Name     string  `json:"name"` // five_hour | seven_day | …
	UsedPct  float64 `json:"usedPct"`
	ResetsAt int64   `json:"resetsAt"` // unix seconds
}

type UsageRowDTO struct {
	Name       string `json:"name"`
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheWrite int64  `json:"cacheWrite"`
	CacheRead  int64  `json:"cacheRead"`
}

type UsageTotalsDTO struct {
	All       UsageRowDTO   `json:"all"`
	ByModel   []UsageRowDTO `json:"byModel"`
	ByProject []UsageRowDTO `json:"byProject"`
}

// UsageDTO is the usage view: the plan's rate-limit windows from the status
// line tap (CapturedAt 0 = never captured), and token totals from the
// transcripts (nil until the first scan is done).
type UsageDTO struct {
	Windows    []UsageWindowDTO `json:"windows"`
	CapturedAt int64            `json:"capturedAt"`
	TapOn      bool             `json:"tapOn"`
	StatusLine string           `json:"statusLine"` // the status line command the tap wraps or would wrap
	FiveHour   *UsageTotalsDTO  `json:"fiveHour,omitempty"`
	SevenDay   *UsageTotalsDTO  `json:"sevenDay,omitempty"`
	ScannedAt  int64            `json:"scannedAt"`
	Err        string           `json:"err,omitempty"`
}

// usageEvery bounds how often the transcripts are rescanned; a warm rescan
// reads only the files written since, a cold one (startup) takes seconds.
const usageEvery = time.Minute

type usageState struct {
	settings string // claude's settings.json
	loomDir  string
	scanner  *usage.Scanner
	now      func() time.Time

	mu        sync.Mutex
	five      *UsageTotalsDTO
	seven     *UsageTotalsDTO
	scannedAt time.Time
	scanErr   string
	scanning  bool
}

func newUsageState(claudeConfigDir, loomDir string, now func() time.Time) *usageState {
	return &usageState{
		settings: filepath.Join(claudeConfigDir, "settings.json"),
		loomDir:  loomDir,
		scanner:  &usage.Scanner{Root: filepath.Join(claudeConfigDir, "projects")},
		now:      now,
	}
}

// Usage returns the usage view. The limits are read on every call (one
// small file); the token scan runs in the background, so this never waits
// on it, and its answer shows on the next call.
func (a *App) Usage() UsageDTO {
	u := a.usage
	if u == nil {
		return UsageDTO{Err: "usage unavailable"}
	}
	var d UsageDTO
	if l, ok, err := usage.ReadLimits(usage.StatusFile(u.loomDir)); err != nil {
		d.Err = err.Error()
	} else if ok {
		d.CapturedAt = l.CapturedAt.Unix()
		for _, w := range l.Windows {
			d.Windows = append(d.Windows, UsageWindowDTO{Name: w.Name, UsedPct: w.UsedPct, ResetsAt: w.ResetsAt})
		}
	}
	if cmd, tapped, err := usage.TapState(u.settings); err != nil {
		d.Err = err.Error()
	} else {
		d.TapOn = tapped
		d.StatusLine, _ = usage.Untap(cmd)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	d.FiveHour, d.SevenDay = u.five, u.seven
	if !u.scannedAt.IsZero() {
		d.ScannedAt = u.scannedAt.Unix()
	}
	if u.scanErr != "" && d.Err == "" {
		d.Err = u.scanErr
	}
	if !u.scanning && u.now().Sub(u.scannedAt) >= usageEvery {
		u.scanning = true
		go u.scan()
	}
	return d
}

func (u *usageState) scan() {
	now := u.now()
	ts, err := u.scanner.Scan(now, 5*time.Hour, 7*24*time.Hour)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.scanning = false
	u.scannedAt = now
	if err != nil {
		u.scanErr = "token scan: " + err.Error()
		return
	}
	u.scanErr = ""
	u.five, u.seven = totalsDTO(ts[0]), totalsDTO(ts[1])
}

// usageTop bounds each breakdown; the rest is folded into "other".
const usageTop = 8

func totalsDTO(t usage.Totals) *UsageTotalsDTO {
	row := func(name string, b usage.Bucket) UsageRowDTO {
		return UsageRowDTO{Name: name, Input: b.Input, Output: b.Output, CacheWrite: b.CacheWrite, CacheRead: b.CacheRead}
	}
	// a worktree's tokens are its repo's, and a project is named by its
	// directory
	projects := map[string]usage.Bucket{}
	for cwd, b := range t.ByProject {
		name := cwd
		if repo, _, _, ok := worktree.Parse(cwd); ok {
			name = repo
		}
		name = filepath.Base(name)
		if name == "." || name == "/" {
			name = "(unknown)"
		}
		p := projects[name]
		p.Input, p.Output, p.CacheWrite, p.CacheRead = p.Input+b.Input, p.Output+b.Output, p.CacheWrite+b.CacheWrite, p.CacheRead+b.CacheRead
		projects[name] = p
	}
	rank := func(m map[string]usage.Bucket) []UsageRowDTO {
		out := make([]UsageRowDTO, 0, len(m))
		for k, b := range m {
			out = append(out, row(k, b))
		}
		// output is what a turn produces, and the bulk of what quota weighs
		sort.Slice(out, func(i, j int) bool {
			if out[i].Output != out[j].Output {
				return out[i].Output > out[j].Output
			}
			return out[i].Name < out[j].Name
		})
		if len(out) > usageTop {
			other := UsageRowDTO{Name: fmt.Sprintf("%d others", len(out)-usageTop+1)}
			for _, r := range out[usageTop-1:] {
				other.Input += r.Input
				other.Output += r.Output
				other.CacheWrite += r.CacheWrite
				other.CacheRead += r.CacheRead
			}
			out = append(out[:usageTop-1], other)
		}
		return out
	}
	return &UsageTotalsDTO{All: row("all", t.All), ByModel: rank(t.ByModel), ByProject: rank(projects)}
}

// SetUsageTap turns the status line tap on or off (an explicit click in the
// usage view): it edits claude's settings.json, backed up first.
func (a *App) SetUsageTap(on bool) error {
	if a.usage == nil {
		return fmt.Errorf("usage unavailable")
	}
	return usage.SetTap(a.usage.settings, a.usage.loomDir, on, a.now())
}
