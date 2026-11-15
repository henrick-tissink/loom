package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/rules"
)

// Teach: a correction becomes a rule on trial. "Teach…" on a review
// comment or a second-opinion finding drafts a rule from it, adopted
// through the ordinary rule sheet (plan, diff, apply) with "On trial:" in
// its Why and a short lapse. "Breaks a rule…" on a later correction records
// it against an adopted rule. Both are kept as correction events in
// ~/.loom/corrections.json and fed to the rules tab, so rules.Status counts
// a rule's corrections before and since its adoption like any failure: the
// count since adoption is how often it was contradicted. At the end of the
// trial the rule is promoted (renewed without the trial mark) or retired
// (dropped).

// Correction is one recorded correction against a rule key.
type Correction struct {
	Key     string `json:"key"`
	Session string `json:"session"` // Claude session id
	Cwd     string `json:"cwd"`
	Ref     string `json:"ref"` // the comment id or finding key it came from
	Text    string `json:"text"`
	TS      int64  `json:"ts"`
}

type correctionStore struct {
	mu   sync.Mutex
	path string // "" keeps them in memory only
	list []Correction
	now  func() time.Time
}

func newCorrectionStore(path string, now func() time.Time) *correctionStore {
	c := &correctionStore{path: path, now: now}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &c.list)
		}
	}
	return c
}

// add records a correction; the same ref against the same key is kept once.
func (c *correctionStore) add(x Correction) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, y := range c.list {
		if y.Key == x.Key && y.Session == x.Session && y.Ref == x.Ref {
			return nil
		}
	}
	if x.TS == 0 {
		x.TS = c.now().Unix()
	}
	next := append(append([]Correction{}, c.list...), x)
	if c.path != "" {
		if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
			return err
		}
		b, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return err
		}
		tmp := c.path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, c.path); err != nil {
			return err
		}
	}
	c.list = next
	return nil
}

// events are the corrections as rule events.
func (c *correctionStore) events() []rules.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]rules.Event, 0, len(c.list))
	for _, x := range c.list {
		out = append(out, rules.Event{Key: x.Key, Source: "correction", Title: "a correction", Session: x.Session, Cwd: x.Cwd, TS: time.Unix(x.TS, 0), Sample: x.Text})
	}
	return out
}

// teachKey names a rule taught from one correction: the session and the
// correction, hashed into what rules.Validate accepts.
func teachKey(sid, ref string) string {
	h := sha256.Sum256([]byte(sid + "\x00" + ref))
	short := sid
	if len(short) > 8 {
		short = short[:8]
	}
	return "corr/" + short + "/" + hex.EncodeToString(h[:4])
}

// oneLine is text as a rule's one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// TeachDraftDTO is a rule ready for the adopt sheet.
type TeachDraftDTO struct {
	Key    string `json:"key"`
	Why    string `json:"why"`
	Draft  string `json:"draft"`
	Lapses string `json:"lapses"`
}

// TeachDraft drafts a rule on trial from a correction in a session (ref:
// the comment id or finding key; text: what was said; where: its place)
// and records the correction, so the rule's count before adoption is one.
func (a *App) TeachDraft(name, ref, text, where string) (TeachDraftDTO, error) {
	if a.corr == nil || a.rules == nil {
		return TeachDraftDTO{}, errors.New("rules unavailable")
	}
	sid, cwd, err := a.sessionReviewTarget(name)
	if err != nil {
		return TeachDraftDTO{}, err
	}
	text = oneLine(text)
	if text == "" || ref == "" {
		return TeachDraftDTO{}, errors.New("nothing to teach from")
	}
	key := teachKey(sid, ref)
	// A rule's Since is kept to the minute, so a rule adopted in the minute
	// it was taught would count its own correction as after adoption. The
	// taught correction is dated just before that minute: it is the one the
	// rule came from, never one that contradicts it.
	ts := a.now().Truncate(time.Minute).Add(-time.Second).Unix()
	if err := a.corr.add(Correction{Key: key, Session: sid, Cwd: cwd, Ref: ref, Text: text, TS: ts}); err != nil {
		return TeachDraftDTO{}, err
	}
	why := rules.TrialPrefix + "taught from a correction"
	if where = oneLine(where); where != "" {
		why += " on " + where
	}
	why += " in " + filepath.Base(cwd)
	draft := text
	if r := []rune(draft); len(r) > 300 {
		draft = string(r[:299]) + "…"
	}
	return TeachDraftDTO{Key: key, Why: why, Draft: draft, Lapses: a.now().Add(rules.TrialLapse).Format(rules.DateLayout)}, nil
}

// adopted reads the adopted rules.
func (a *App) adopted() ([]rules.Rule, error) {
	if a.rules == nil {
		return nil, errors.New("rules unavailable")
	}
	b, err := os.ReadFile(a.rules.layout.RulesPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return rules.Parse(b), nil
}

// AdoptedRules lists the adopted rules, for "Breaks a rule…".
func (a *App) AdoptedRules() ([]rules.Rule, error) {
	rs, err := a.adopted()
	if rs == nil {
		rs = []rules.Rule{}
	}
	return rs, err
}

// BreaksRule records a correction in a session against an adopted rule:
// the agent did what the rule says not to. It counts as the rule
// contradicted since it was adopted.
func (a *App) BreaksRule(name, key, ref, text string) error {
	if a.corr == nil {
		return errors.New("rules unavailable")
	}
	sid, cwd, err := a.sessionReviewTarget(name)
	if err != nil {
		return err
	}
	rs, err := a.adopted()
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Key == key {
			return a.corr.add(Correction{Key: key, Session: sid, Cwd: cwd, Ref: ref, Text: oneLine(text)})
		}
	}
	return fmt.Errorf("no adopted rule for %s", key)
}
