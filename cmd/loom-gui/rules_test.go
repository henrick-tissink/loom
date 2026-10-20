package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/discipline"
	"github.com/henricktissink/loom/internal/tmux"
)

// writeErrTranscript writes a session whose one Write call failed.
func writeErrTranscript(t *testing.T, ccd, sid, cwd string, at time.Time) {
	t.Helper()
	dir := filepath.Join(ccd, "projects", "-w")
	os.MkdirAll(dir, 0o755)
	body := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_` + sid + `","name":"Write","input":{}}]}}` + "\n" +
		`{"type":"user","cwd":"` + cwd + `","timestamp":"` + at.UTC().Format(time.RFC3339) + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_` + sid + `","is_error":true,"content":"<tool_use_error>File has not been read yet. Read it first before writing to it.</tool_use_error>"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRuleProposalsAndApply(t *testing.T) {
	home := t.TempDir()
	l := discipline.NewLayout(filepath.Join(home, ".claude"), home, "")
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.Local)
	for i := 0; i < 5; i++ {
		writeErrTranscript(t, l.ConfigDir, fmt.Sprintf("s%d", i), visRepo, now.Add(-time.Duration(i+1)*time.Hour))
	}
	writeErrTranscript(t, l.ConfigDir, "h0", hidRepo, now.Add(-time.Hour))
	r := hookReader(t, []string{
		rec("2026-10-16T10:00:00", "bash_guard", "PreToolUse", "deny_detach", "a", visRepo, ``),
	}, nil)
	a := newApp(nil, tmux.New(), nil, nil, nil, func() time.Time { return now })
	a.hooks = &hookState{reader: r}
	a.rules = newRulesState(l)

	d := a.RuleProposals()
	if d.Err != "" || d.Scanned != 6 || len(d.Proposals) != 1 || len(d.Rules) != 0 || d.Lapse != "2026-11-15" {
		t.Fatalf("proposals: %+v", d)
	}
	p := d.Proposals[0]
	if p.Count != 6 || p.Sessions != 6 || p.Draft == "" || !strings.HasPrefix(p.Key, "tool/Write/") {
		t.Fatalf("proposal: %+v", p)
	}

	c := discipline.RuleChange{Op: "adopt", Key: p.Key, Text: p.Draft, Why: p.Why, Lapses: d.Lapse}
	plan, err := a.RulePlan(c)
	if err != nil || len(plan.Files) != 2 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	msg, err := a.RuleApply(c, plan.Token)
	if err != nil || !strings.HasPrefix(msg, "Rule adopted") {
		t.Fatalf("apply: %q %v", msg, err)
	}
	d = a.RuleProposals()
	if len(d.Proposals) != 0 || len(d.Rules) != 1 || d.Rules[0].Key != p.Key || d.Rules[0].Before != 6 || d.Rules[0].Lapsed {
		t.Fatalf("after adopt: %+v", d)
	}
	if _, err := a.RuleApply(c, plan.Token); err == nil {
		t.Fatal("the same reviewed change applied twice")
	}

	// A hidden project's failures are left out, and say so.
	a.projects = nil
	if evs, hidden := a.visibleEvents(nil); evs != nil || hidden {
		t.Fatal("no resolver hides nothing")
	}

	// No rules state: empty lists, never nil.
	b := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if x := b.RuleProposals(); x.Proposals == nil || x.Rules == nil || x.Err == "" {
		t.Fatalf("nil state: %+v", x)
	}
}
