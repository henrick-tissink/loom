package discipline

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/jsonedit"
)

func parse(t *testing.T, s string) *jsonedit.Node {
	t.Helper()
	n, err := jsonedit.Parse([]byte(s))
	must(t, err)
	return n
}

func TestMergeHooksAddsFixesAndKeepsOrder(t *testing.T) {
	l := NewLayout("/h/.claude", "/h", "")
	regs, err := LoadRegistrations()
	must(t, err)
	// One of ours with a wrong timeout, another tool's hook first, a foreign key.
	doc := parse(t, `{"zeta": 1, "hooks": {"Stop": [
	  {"hooks": [{"type":"command","command":"/h/.local/bin/lava hook","async":true}]},
	  {"hooks": [{"type":"command","command":"python3 /h/.claude/hooks/enforce/stop_guard.py","timeout":5}]}
	]}, "alpha": {"keep": "me"}}`)
	ch := MergeHooks(doc, l, regs)
	acts := map[string]string{}
	for _, c := range ch {
		acts[c.ID] = c.Action
	}
	if acts["Stop||stop_guard.py"] != "fixed" {
		t.Fatalf("stop guard: %v", ch)
	}
	added := 0
	for _, c := range ch {
		if c.Action == "added" {
			added++
		}
	}
	if added != len(regs.Hooks)-1 {
		t.Fatalf("added %d, want %d", added, len(regs.Hooks)-1)
	}
	out := string(doc.Marshal("  "))
	if !strings.HasPrefix(out, "{\n  \"zeta\": 1,\n  \"hooks\"") || !strings.Contains(out, `"alpha"`) {
		t.Fatalf("order or foreign keys lost:\n%s", out)
	}
	stop := doc.Path("hooks", "Stop")
	if stop.Len() != 2 || stop.Items[0].Path("hooks").Items[0].Get("async") == nil {
		t.Fatalf("other tool's hook moved or lost:\n%s", out)
	}
	if stop.Items[1].Path("hooks").Items[0].Get("timeout").IntVal() != 300 {
		t.Fatalf("timeout not fixed:\n%s", out)
	}
	// Idempotent.
	for _, c := range MergeHooks(doc, l, regs) {
		if c.Action != "present" {
			t.Fatalf("second merge: %+v", c)
		}
	}
}

func TestMergeHooksReportsDuplicate(t *testing.T) {
	l := NewLayout("/h/.claude", "/h", "")
	regs, err := LoadRegistrations()
	must(t, err)
	doc := parse(t, `{"hooks": {"PreToolUse": [
	  {"matcher":"Bash","hooks":[{"type":"command","command":"/h/.claude/hooks/enforce/bash_guard.py","timeout":5,"onFailure":"block"}]},
	  {"matcher":"Bash","hooks":[{"type":"command","command":"python3 /h/.claude/hooks/enforce/bash_guard.py","timeout":5,"onFailure":"block"}]}
	]}}`)
	for _, c := range MergeHooks(doc, l, regs) {
		if c.ID == "PreToolUse|Bash|bash_guard.py" && c.Action != "duplicate" {
			t.Fatalf("%+v", c)
		}
	}
}

func TestUnmergeHooksRemovesOnlyListed(t *testing.T) {
	l := NewLayout("/h/.claude", "/h", "")
	regs, err := LoadRegistrations()
	must(t, err)
	doc := parse(t, `{"hooks": {"Stop": [{"hooks": [{"type":"command","command":"/h/.local/bin/lava hook"}]}]}}`)
	ch := MergeHooks(doc, l, regs)
	var ids []string
	for _, c := range ch {
		if c.Action == "added" && c.ID != "Stop||stop_guard.py" {
			ids = append(ids, c.ID)
		}
	}
	n := UnmergeHooks(doc, l, regs, ids)
	if n != len(ids) {
		t.Fatalf("removed %d, want %d", n, len(ids))
	}
	out := string(doc.Marshal(""))
	if !strings.Contains(out, "stop_guard.py") || !strings.Contains(out, "lava hook") || strings.Contains(out, "bash_guard") {
		t.Fatalf("%s", out)
	}
	UnmergeHooks(doc, l, regs, []string{"Stop||stop_guard.py"})
	if doc.Path("hooks", "Stop").Len() != 1 {
		t.Fatalf("lava group gone: %s", doc.Marshal(""))
	}
}

func TestMergeStatusLineEnvDeny(t *testing.T) {
	l := NewLayout("/h/.claude", "/h", "")
	regs, err := LoadRegistrations()
	must(t, err)
	doc := parse(t, `{"statusLine": {"type":"command","command":"other"}, "env": {"CLAUDE_CODE_SUBAGENT_MODEL": "haiku"}}`)
	if w := MergeStatusLine(doc, l.StatusLineCommand(regs)); w != "" {
		t.Fatalf("overwrote another tool's status line")
	}
	w := MergeEnv(doc, regs.Env)
	if _, ok := w["CLAUDE_CODE_SUBAGENT_MODEL"]; ok || len(w) != len(regs.Env)-1 {
		t.Fatalf("env written %v", w)
	}
	if doc.Path("env", "CLAUDE_CODE_SUBAGENT_MODEL").Str() != "haiku" {
		t.Fatal("user's env value replaced")
	}
	a := MergeDeny(doc, l.DenyEntries(regs))
	if len(a) != 2 || len(MergeDeny(doc, l.DenyEntries(regs))) != 0 {
		t.Fatalf("deny %v", a)
	}
	// Undo with the user having changed one env value since.
	doc.Path("env").Set("CLAUDE_CODE_AUTO_COMPAST_WINDOW", jsonedit.String("x"))
	doc.Path("env").Set("CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH", jsonedit.String("3"))
	n := UnmergeEnv(doc, w)
	if n != len(w)-1 || doc.Path("env", "CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH").Str() != "3" {
		t.Fatalf("unmerge env %d: %s", n, doc.Marshal(""))
	}
	if UnmergeDeny(doc, a) != 2 || doc.Get("permissions") != nil {
		t.Fatalf("deny undo: %s", doc.Marshal(""))
	}
	if !UnmergeStatusLine(parse(t, `{"statusLine":{"type":"command","command":"X"}}`), "X") {
		t.Fatal("status line not removed")
	}
	// wrapped by loom's usage tap, it is still the one loom wrote
	tapped := `{"statusLine":{"type":"command","command":"sh '/h/.loom/usage/statusline-tap.sh' 'X'"}}`
	tdoc := parse(t, tapped)
	if !UnmergeStatusLine(tdoc, "X") || tdoc.Path("statusLine", "command").Str() != "sh '/h/.loom/usage/statusline-tap.sh' ''" {
		t.Fatalf("tapped status line: %s", tdoc.Marshal(""))
	}
	// the tap with nothing inside is an empty slot: loom's command goes in it
	if MergeStatusLine(tdoc, "X") != "X" || string(tdoc.Marshal("")) != string(parse(t, tapped).Marshal("")) {
		t.Fatalf("merge into empty tap: %s", tdoc.Marshal(""))
	}
	if UnmergeStatusLine(parse(t, `{"statusLine":{"type":"command","command":"Y"}}`), "X") {
		t.Fatal("another tool's status line removed")
	}
}

func TestClaudeMdReplaceInlineAndUndo(t *testing.T) {
	rules := mustRead(rulesName)
	cur := []byte("# mine\n\nmy own rule\n\n" + string(rules) + "\n\n## Team\nteam text\n")
	out, action := PlanClaudeMD(cur, rules)
	if action != ClaudeMDReplaceInline {
		t.Fatalf("action %s", action)
	}
	want := "# mine\n\nmy own rule\n\n" + ImportLine + "\n\n## Team\nteam text\n"
	if string(out) != want {
		t.Fatalf("got:\n%s", out)
	}
	if _, a := PlanClaudeMD(out, rules); a != ClaudeMDNone {
		t.Fatalf("second plan %s", a)
	}
	back, changed := UndoClaudeMD(out, rules, true)
	if !changed || strings.TrimSpace(string(back)) != strings.TrimSpace(string(cur)) {
		t.Fatalf("undo:\n%s", back)
	}
}

func TestClaudeMdAppendAndUndo(t *testing.T) {
	rules := mustRead(rulesName)
	out, action := PlanClaudeMD([]byte("# mine\n"), rules)
	if action != ClaudeMDAppend || string(out) != "# mine\n\n"+ImportLine+"\n" {
		t.Fatalf("%s:\n%s", action, out)
	}
	out, action = PlanClaudeMD(nil, rules)
	if action != ClaudeMDAppend || string(out) != ImportLine+"\n" {
		t.Fatalf("%s:\n%s", action, out)
	}
	back, changed := UndoClaudeMD([]byte("# mine\n\n"+ImportLine+"\n"), rules, false)
	if !changed || string(back) != "# mine\n" {
		t.Fatalf("undo:\n%q", back)
	}
}

// legacyWithGaps is the legacy fixture missing one registration, the
// status line and the deny entries: what setup must add and uninstall
// must take away again.
func legacyWithGaps(t *testing.T) (Layout, []byte) {
	l, settings := fixture(t, LayoutLegacy)
	delete(settings["hooks"].(map[string]any), "PreCompact")
	delete(settings, "statusLine")
	delete(settings, "permissions")
	writeJSON(t, l.SettingsPath(), settings)
	orig, _ := os.ReadFile(l.SettingsPath())
	return l, orig
}

func TestPlanSetupLegacy(t *testing.T) {
	l, _ := legacyWithGaps(t)
	rep := Doctor(context.Background(), l, fakeEnv(l), Options{})
	p := PlanSetup(l, rep, DefaultPlanOptions(), time.Date(2026, 10, 15, 16, 0, 0, 0, time.UTC))
	if p.HasErrors() {
		t.Fatalf("errors: %v", p.Errors)
	}
	kinds := map[string]Step{}
	for _, s := range p.Steps {
		if s.Enabled {
			kinds[s.Kind+":"+s.Target] = s
		}
	}
	for _, want := range []string{StepInstallRev + ":" + l.RevDir(BundledRev()), StepGate + ":" + l.RevDir(BundledRev()), StepMigrate + ":" + l.HooksDir(), StepMigrate + ":" + l.ContextBar(), StepLink + ":" + l.CurrentLink(), StepHooks + ":" + l.SettingsPath(), StepStatusLine + ":statusLine", StepDeny + ":permissions.deny", StepSettings + ":" + l.SettingsPath(), StepClaudeMD + ":" + l.ClaudeMD(), StepReceipt + ":" + l.ReceiptPath()} {
		if _, ok := kinds[want]; !ok {
			t.Errorf("missing enabled step %s\n%s", want, RenderPlan(p))
		}
	}
	if _, ok := kinds[StepEnv+":env"]; ok {
		t.Error("env is opt-in")
	}
	if _, ok := kinds[StepDeadline+":"+l.Deadline()]; ok {
		t.Error("deadline is current; must not be rewritten")
	}
	if !strings.Contains(kinds[StepClaudeMD+":"+l.ClaudeMD()].Detail, ClaudeMDReplaceInline) {
		t.Errorf("CLAUDE.md step: %+v", kinds[StepClaudeMD+":"+l.ClaudeMD()])
	}
	if d := kinds[StepSettings+":"+l.SettingsPath()].Diff; !strings.Contains(d, "+") || !strings.Contains(d, "precompact_checkpoint.py") {
		t.Errorf("settings diff:\n%s", d)
	}
}

func TestPlanSetupRevCurrentIsNoop(t *testing.T) {
	l, _ := fixture(t, LayoutRev)
	rep := Doctor(context.Background(), l, fakeEnv(l), Options{})
	p := PlanSetup(l, rep, DefaultPlanOptions(), time.Now())
	if p.HasErrors() {
		t.Fatalf("errors: %v", p.Errors)
	}
	for _, s := range p.Steps {
		if s.Enabled && s.Kind != StepGate && s.Kind != StepReceipt && s.Kind != StepMkdir {
			t.Errorf("unexpected enabled step on a current install: %+v", s)
		}
	}
}

func TestPlanSetupRefusesWithoutPython(t *testing.T) {
	l, _ := legacyWithGaps(t)
	rep := Doctor(context.Background(), l, fakeEnv(l), Options{})
	rep.Prereq.Python3.Runs = false
	if p := PlanSetup(l, rep, DefaultPlanOptions(), time.Now()); !p.HasErrors() {
		t.Fatal("no error without python3")
	}
}

func TestSetupUninstallRoundtrip(t *testing.T) {
	l, origSettings := legacyWithGaps(t)
	origRules, _ := os.ReadFile(l.ClaudeMD())
	// A local edit in the legacy hooks: must survive, moved aside, not carried into r3.
	guard := filepath.Join(l.HooksDir(), "bash_guard.py")
	gb, _ := os.ReadFile(guard)
	must(t, os.WriteFile(guard, append(gb, []byte("\n# local tweak\n")...), 0o755))
	ctx := context.Background()
	now := time.Date(2026, 10, 15, 16, 0, 0, 0, time.UTC)
	opts := ApplyOptions{Now: now, Build: "test"}
	if _, err := exec.LookPath("python3"); err != nil {
		opts.SkipGate = true
		t.Log("python3 not found; gate skipped")
	}

	rep := Doctor(ctx, l, fakeEnv(l), Options{})
	p := PlanSetup(l, rep, DefaultPlanOptions(), now)
	if p.HasErrors() {
		t.Fatalf("plan: %v", p.Errors)
	}
	st, err := Stage(ctx, l, p, opts)
	if err != nil {
		t.Fatalf("stage: %v\n%s", err, RenderGate(st.Gate))
	}
	if !opts.SkipGate && (!st.Gate.Suites.Passed || st.Gate.Replay.Ran) {
		t.Fatalf("gate: %+v", st.Gate)
	}
	// Staging is invisible: the legacy hooks dir is untouched.
	if fi, err := os.Lstat(l.HooksDir()); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("stage touched hooks/enforce")
	}
	res, err := Commit(ctx, l, p, st, opts)
	if err != nil {
		t.Fatalf("commit: %v\n%s", err, strings.Join(res.Log, "\n"))
	}
	after := Doctor(ctx, l, fakeEnv(l), Options{})
	if after.Rev.Layout != LayoutRev || after.Rev.Installed != BundledRev() || after.Health != "green" {
		t.Fatalf("after setup: %+v health %s issues %+v", after.Rev, after.Health, after.Issues)
	}
	if after.Registrations.Matched != after.Registrations.Expected {
		t.Fatalf("registrations %d/%d", after.Registrations.Matched, after.Registrations.Expected)
	}
	rc, err := LoadReceipt(l)
	must(t, err)
	if rc == nil || rc.Rev != BundledRev() || len(rc.Settings.HooksAdded) != 1 || rc.Settings.StatusLine == "" || len(rc.Settings.Deny) != 2 || !rc.ClaudeMD.ReplacedInline || rc.MigratedFrom == "" {
		t.Fatalf("receipt: %+v", rc)
	}
	if b, _ := os.ReadFile(filepath.Join(rc.MigratedFrom, "bash_guard.py")); !strings.Contains(string(b), "local tweak") {
		t.Fatal("local edit lost in the move aside")
	}
	if b, _ := os.ReadFile(filepath.Join(l.RevDir(BundledRev()), "enforce", "bash_guard.py")); strings.Contains(string(b), "local tweak") {
		t.Fatal("local edit carried into the revision")
	}
	if b, _ := os.ReadFile(l.ClaudeMD()); !strings.Contains(string(b), ImportLine) || strings.Contains(string(b), "### Finish line first") {
		t.Fatalf("CLAUDE.md after setup:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(res.BackupDir, "settings.json")); err != nil {
		t.Fatal("no settings backup")
	}
	// Running setup again plans nothing but the gate and the receipt.
	p2 := PlanSetup(l, after, DefaultPlanOptions(), now.Add(time.Hour))
	for _, s := range p2.Steps {
		if s.Enabled && s.Kind != StepGate && s.Kind != StepReceipt && s.Kind != StepMkdir {
			t.Errorf("second setup would %s %s: %s", s.Kind, s.Target, s.Detail)
		}
	}

	// Uninstall.
	up := PlanUninstall(l, after, rc)
	if up.HasErrors() {
		t.Fatalf("uninstall plan: %v", up.Errors)
	}
	if _, err := ApplyUninstall(ctx, l, up, rc, ApplyOptions{Now: now.Add(2 * time.Hour)}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	back := Doctor(ctx, l, fakeEnv(l), Options{})
	if back.Rev.Layout != LayoutLegacy {
		t.Fatalf("after uninstall: %+v", back.Rev)
	}
	if b, _ := os.ReadFile(guard); !strings.Contains(string(b), "local tweak") {
		t.Fatal("legacy hooks not restored")
	}
	var want, got any
	must(t, json.Unmarshal(origSettings, &want))
	nowB, _ := os.ReadFile(l.SettingsPath())
	must(t, json.Unmarshal(nowB, &got))
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("settings not restored:\n%s", nowB)
	}
	if b, _ := os.ReadFile(l.ClaudeMD()); strings.TrimSpace(string(b)) != strings.TrimSpace(string(origRules)) {
		t.Fatalf("CLAUDE.md not restored:\n%s", b)
	}
	if _, err := os.Stat(l.RevDir(BundledRev())); !os.IsNotExist(err) {
		t.Error("revision dir left behind")
	}
	if r, _ := LoadReceipt(l); r != nil {
		t.Error("receipt left behind")
	}
	led, _ := ReadLedger(l)
	if len(led) != 2 || led[0].Op != "setup" || led[1].Op != "uninstall" {
		t.Fatalf("ledger: %+v", led)
	}
}

func TestStageAbandonsOnGateFailure(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3")
	}
	l, _ := legacyWithGaps(t)
	rep := Doctor(context.Background(), l, fakeEnv(l), Options{})
	p := PlanSetup(l, rep, DefaultPlanOptions(), time.Now())
	// A gate that cannot pass: no bash on PATH for the suites.
	t.Setenv("PATH", "/nonexistent")
	st, err := Stage(context.Background(), l, p, ApplyOptions{Now: time.Now()})
	if err == nil {
		t.Fatal("gate passed without python3 on PATH")
	}
	if _, serr := os.Stat(l.RevDir(BundledRev())); !os.IsNotExist(serr) {
		t.Fatal("staged dir not removed after gate failure")
	}
	if st != nil && st.Created {
		t.Fatal("staged still marked created")
	}
}

// A revision-layout install with a local edit: keep leaves the edit and
// records it; --replace overwrites it with a backup. Conflicts must name
// the real r<N> path, not the discipline/current path the doctor shows.
func TestSetupRevLayoutKeepAndReplace(t *testing.T) {
	l, _ := fixture(t, LayoutRev)
	ctx := context.Background()
	now := time.Date(2026, 10, 15, 17, 0, 0, 0, time.UTC)
	opts := ApplyOptions{Now: now, SkipGate: true}
	real := filepath.Join(l.RevDir(BundledRev()), "enforce", "bash_guard.py")
	b, _ := os.ReadFile(real)
	edited := append(b, []byte("\n# my tweak\n")...)
	must(t, os.WriteFile(real, edited, 0o755))

	rep := Doctor(ctx, l, fakeEnv(l), Options{})
	p := PlanSetup(l, rep, DefaultPlanOptions(), now)
	if len(p.Conflicts) != 1 || p.Conflicts[0].Choice != "keep" || mustEval(p.Conflicts[0].Path) != mustEval(real) {
		t.Fatalf("conflicts %+v", p.Conflicts)
	}
	st, err := Stage(ctx, l, p, opts)
	must(t, err)
	if _, err := Commit(ctx, l, p, st, opts); err != nil {
		t.Fatalf("commit with keep: %v", err)
	}
	if got, _ := os.ReadFile(real); string(got) != string(edited) {
		t.Fatal("keep overwrote the edit")
	}
	rc, _ := LoadReceipt(l)
	if _, ok := rc.Kept[mustEval(real)]; !ok {
		t.Fatalf("kept not recorded: %+v", rc.Kept)
	}
	// The doctor stops reporting it and the next plan has no conflict to ask about.
	rep = Doctor(ctx, l, fakeEnv(l), Options{})
	// --replace by the path the doctor shows (through discipline/current).
	shown := filepath.Join(l.CurrentLink(), "enforce", "bash_guard.py")
	o := DefaultPlanOptions()
	o.Replace = []string{shown}
	p = PlanSetup(l, rep, o, now.Add(time.Hour))
	if len(p.Conflicts) != 1 || p.Conflicts[0].Choice != "replace" {
		t.Fatalf("conflicts %+v", p.Conflicts)
	}
	st, err = Stage(ctx, l, p, ApplyOptions{Now: now.Add(time.Hour), SkipGate: true})
	must(t, err)
	if _, err := Commit(ctx, l, p, st, ApplyOptions{Now: now.Add(time.Hour), SkipGate: true}); err != nil {
		t.Fatalf("commit with replace: %v", err)
	}
	if got, _ := os.ReadFile(real); string(got) != string(b) {
		t.Fatal("replace did not restore the bundle copy")
	}
	rc, _ = LoadReceipt(l)
	if _, ok := rc.Kept[mustEval(real)]; ok {
		t.Fatal("kept entry survived replace")
	}
	if _, err := os.Stat(filepath.Join(l.BackupsDir(), now.Add(time.Hour).Format("20060102-150405")+"-replace", "bash_guard.py")); err != nil {
		t.Fatal("replaced file was not backed up")
	}
}

// What was there before loom comes back on uninstall: a dotfiles symlink
// at scripts/context-bar.sh and a deadline wrapper of the person's own.
func TestUninstallRestoresPreLoomSymlinkAndDeadline(t *testing.T) {
	l, _ := legacyWithGaps(t)
	dotfiles := filepath.Join(l.Home, "dotfiles", "context-bar.sh")
	must(t, os.MkdirAll(filepath.Dir(dotfiles), 0o755))
	must(t, os.WriteFile(dotfiles, []byte("#!/bin/sh\necho mine\n"), 0o755))
	must(t, os.Remove(l.ContextBar()))
	must(t, os.Symlink(dotfiles, l.ContextBar()))
	myDeadline := []byte("#!/bin/sh\nexec \"$@\"\n")
	must(t, os.WriteFile(l.Deadline(), myDeadline, 0o755))

	ctx := context.Background()
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	rep := Doctor(ctx, l, fakeEnv(l), Options{})
	o := DefaultPlanOptions()
	o.Replace = []string{l.Deadline()}
	p := PlanSetup(l, rep, o, now)
	if p.HasErrors() {
		t.Fatalf("plan: %v", p.Errors)
	}
	st, err := Stage(ctx, l, p, ApplyOptions{Now: now, SkipGate: true})
	must(t, err)
	if _, err := Commit(ctx, l, p, st, ApplyOptions{Now: now, SkipGate: true}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rc, _ := LoadReceipt(l)
	if rc.PrevLinks[l.ContextBar()] != dotfiles || rc.Restore[l.Deadline()] == "" {
		t.Fatalf("receipt: prevLinks %v restore %v", rc.PrevLinks, rc.Restore)
	}
	if got, _ := os.ReadFile(l.Deadline()); string(got) == string(myDeadline) {
		t.Fatal("--replace did not replace the deadline wrapper")
	}
	after := Doctor(ctx, l, fakeEnv(l), Options{})
	up := PlanUninstall(l, after, rc)
	if _, err := ApplyUninstall(ctx, l, up, rc, ApplyOptions{Now: now.Add(time.Hour)}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got, _ := os.Readlink(l.ContextBar()); got != dotfiles {
		t.Fatalf("context-bar.sh after uninstall: %q, want the dotfiles symlink", got)
	}
	if got, _ := os.ReadFile(l.Deadline()); string(got) != string(myDeadline) {
		t.Fatalf("deadline after uninstall:\n%s", got)
	}
}
