package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/discipline"
	"github.com/henricktissink/loom/internal/rules"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
)

func TestTeachAdoptsOnTrialAndCountsBreaks(t *testing.T) {
	home := t.TempDir()
	l := discipline.NewLayout(filepath.Join(home, ".claude"), home, "")
	if err := os.MkdirAll(l.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 20, 11, 0, 0, 0, time.Local)
	now := func() time.Time { return clock }
	corrPath := filepath.Join(home, ".loom", "corrections.json")
	a := &App{now: now, rules: newRulesState(l), corr: newCorrectionStore(corrPath, now)}
	if _, err := a.TeachDraft("loom-1", "c1", "x", ""); err == nil {
		t.Fatal("taught from a session not on the rail")
	}
	a.noteRail([]SessionDTO{{Name: "loom-1"}, {Name: "loom-2"}}, []status.Row{
		{SessionRow: store.SessionRow{Name: "loom-1", ClaudeSessionID: "sid-one-long", Cwd: "/w/proj"}},
		{SessionRow: store.SessionRow{Name: "loom-2", ClaudeSessionID: "sid-two", Cwd: "/w/proj"}},
	})
	if _, err := a.TeachDraft("loom-1", "c1", "   ", "a.go:3"); err == nil {
		t.Fatal("taught from an empty correction")
	}
	d, err := a.TeachDraft("loom-1", "c1", "Name the\nconstant.", "a.go:3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.Key, "corr/sid-one-/") || d.Draft != "Name the constant." || d.Lapses != "2026-11-03" ||
		d.Why != "On trial: taught from a correction on a.go:3 in proj" {
		t.Fatalf("draft = %+v", d)
	}
	// the sheet adopts it, through the ordinary plan
	clock = clock.Add(30 * time.Second) // adopted in the minute it was taught
	c := discipline.RuleChange{Op: "adopt", Key: d.Key, Text: d.Draft, Why: d.Why, Lapses: d.Lapses}
	p, err := a.RulePlan(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RuleApply(p.Change, p.Token); err != nil {
		t.Fatal(err)
	}
	if err := a.BreaksRule("loom-2", "corr/none/x", "c9", "again"); err == nil {
		t.Fatal("broke a rule that is not adopted")
	}
	clock = clock.Add(time.Hour)
	for i := 0; i < 2; i++ { // the same correction twice counts once
		if err := a.BreaksRule("loom-2", d.Key, "c9", "magic number again"); err != nil {
			t.Fatal(err)
		}
	}
	rs, _ := a.AdoptedRules()
	if len(rs) != 1 || !rs[0].Trial() {
		t.Fatalf("adopted = %+v", rs)
	}
	// the rules tab: before 1 (the taught correction), after 1 (the break)
	st := rules.Status(rs, newCorrectionStore(corrPath, now).events(), clock)
	if st[0].Before != 1 || st[0].After != 1 || !st[0].Trial {
		t.Fatalf("status = %+v", st[0])
	}
	dto := a.RuleProposals()
	if len(dto.Rules) != 1 || dto.Rules[0].After != 1 || len(dto.Proposals) != 0 {
		t.Fatalf("tab = %+v", dto)
	}
}

func TestTeachKeyIsValid(t *testing.T) {
	k := teachKey("0b1f6c2e-aaaa-bbbb", "cmd/x.go:3:a summary with spaces · and dots")
	if err := rules.Validate(rules.Rule{Text: "t", Key: k, Since: "2026-10-20 11:00", Lapses: "2026-11-03"}); err != nil {
		t.Fatalf("%s: %v", k, err)
	}
	if teachKey("s", "a") == teachKey("s", "b") {
		t.Fatal("two corrections share a key")
	}
}
