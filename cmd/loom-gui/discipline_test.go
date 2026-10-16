package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/discipline"
	"github.com/henricktissink/loom/internal/tmux"
)

// Everything here runs on a layout under t.TempDir(): ConfigDir, Home and
// the state dir all live there, so no test can touch the machine's own
// ~/.claude or ~/.loom.

func discApp(t *testing.T) (*App, discipline.Layout) {
	t.Helper()
	tmp := t.TempDir()
	ccd := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(ccd, 0o755); err != nil {
		t.Fatal(err)
	}
	l := discipline.NewLayout(ccd, tmp, "")
	clock := time.Date(2026, 10, 16, 18, 0, 0, 0, time.Local)
	now := func() time.Time { return clock }
	a := newApp(nil, tmux.New(), nil, nil, nil, now)
	d := newDisciplineState(l, now)
	d.skipGate = true
	d.env = discipline.Env{
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		Run: func(_ context.Context, name string, _ ...string) (string, error) {
			switch name {
			case "python3":
				return "3.12.4", nil
			case "claude":
				return "2.1.300 (Claude Code)", nil
			case "tmux":
				return "PATH=" + filepath.Dir(l.Deadline()) + ":/usr/bin", nil
			}
			return filepath.Dir(l.Deadline()) + ":/usr/bin:/bin", nil
		},
		Writable:    func(string) bool { return true },
		Quarantined: func(string) bool { return false },
		Shell:       "/bin/zsh",
		Now:         now,
	}
	a.disc = d
	return a, l
}

func TestDisciplineDoctorNotInstalled(t *testing.T) {
	a, _ := discApp(t)
	dto := a.DisciplineDoctor(false)
	if dto.Report.Rev.Layout != discipline.LayoutNone || dto.Action != "install" || dto.FlippedAt != 0 || dto.Staged {
		t.Fatalf("not installed: layout %q action %q flipped %d staged %v", dto.Report.Rev.Layout, dto.Action, dto.FlippedAt, dto.Staged)
	}
	if dto.Ledger == nil || dto.Backups == nil {
		t.Fatal("ledger and backups must marshal as [] not null")
	}
}

func TestDisciplineDoctorUnavailable(t *testing.T) {
	a := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if dto := a.DisciplineDoctor(true); dto.Headline == "" || dto.Action != "" {
		t.Fatalf("nil state: %+v", dto)
	}
	if _, err := a.DisciplineStage(discipline.DefaultPlanOptions()); err == nil {
		t.Fatal("stage without a state must fail")
	}
}

func TestDisciplineStageApplyUninstall(t *testing.T) {
	a, l := discApp(t)
	if _, err := a.DisciplineApply(false); err == nil {
		t.Fatal("apply before stage must fail")
	}
	st, err := a.DisciplineStage(discipline.DefaultPlanOptions())
	if err != nil || st.Error != "" {
		t.Fatalf("stage: %v %q", err, st.Error)
	}
	if !st.Gate.Passed || st.Plan == nil || st.Plan.Rev != discipline.BundledRev() {
		t.Fatalf("stage result: %+v", st)
	}
	// Staging is invisible: nothing a session uses exists yet.
	if _, err := os.Lstat(l.HooksDir()); err == nil {
		t.Fatal("stage created hooks/enforce")
	}
	if !a.DisciplineDoctor(true).Staged {
		t.Fatal("dto must report the staged revision")
	}
	res, err := a.DisciplineApply(false)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, strings.Join(res.Log, "\n"))
	}
	dto := a.DisciplineDoctor(false)
	if dto.Report.Rev.Layout != discipline.LayoutRev || dto.Report.Rev.Installed != discipline.BundledRev() {
		t.Fatalf("after apply: %+v", dto.Report.Rev)
	}
	if dto.Report.Health != "green" || dto.Action != "" || !strings.HasPrefix(dto.Headline, "Enforcing · "+discipline.BundledRev()) {
		t.Fatalf("after apply: health %s action %q headline %q issues %+v", dto.Report.Health, dto.Action, dto.Headline, dto.Report.Issues)
	}
	if len(dto.Ledger) != 1 || dto.Ledger[0].Op != "setup" || dto.FlippedAt != dto.Ledger[0].TS.Unix() || dto.Staged {
		t.Fatalf("ledger %+v flipped %d staged %v", dto.Ledger, dto.FlippedAt, dto.Staged)
	}
	if dto.Receipt == nil || dto.Receipt.Rev != discipline.BundledRev() || !strings.HasPrefix(dto.Receipt.LoomBuild, "gui-") {
		t.Fatalf("receipt %+v", dto.Receipt)
	}
	if _, err := a.DisciplineApply(false); err == nil {
		t.Fatal("a second apply with nothing staged must fail")
	}

	// Local edit: Diff shows it, Keep records it, Reset restores the bundle.
	guard := filepath.Join(l.HooksDir(), "bash_guard.py")
	if d, err := a.DisciplineAction("diff", "bash_guard.py"); err != nil || d != "" {
		t.Fatalf("diff of a current file: %q %v", d, err)
	}
	gb, _ := os.ReadFile(guard)
	if err := os.WriteFile(guard, append(gb, []byte("\n# mine\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := a.DisciplineAction("diff", "bash_guard.py")
	if err != nil || !strings.Contains(d, "+# mine") {
		t.Fatalf("diff after edit: %v\n%s", err, d)
	}
	if fs := fileState(a.DisciplineDoctor(true), "bash_guard.py"); fs != discipline.StateModified {
		t.Fatalf("state after edit: %s", fs)
	}
	if msg, err := a.DisciplineAction("keep", "bash_guard.py"); err != nil || !strings.HasPrefix(msg, "kept:") {
		t.Fatalf("keep: %q %v", msg, err)
	}
	dto = a.DisciplineDoctor(false)
	if dto.Receipt == nil || len(dto.Receipt.Kept) != 1 || !strings.Contains(dto.Headline, "1 local edit") {
		t.Fatalf("after keep: kept %v headline %q", dto.Receipt.Kept, dto.Headline)
	}
	// The island matches kept files by the report's spelling of the path
	// (through the current link), not the receipt's resolved one.
	if want := installedPath(dto, "bash_guard.py"); len(dto.KeptPaths) != 1 || dto.KeptPaths[0] != want || want == "" {
		t.Fatalf("keptPaths %v, want [%s]", dto.KeptPaths, want)
	}
	// Keep records the choice; the doctor still reports the content as
	// modified, and the island shows "kept · yours" without a keep button.
	if fs := fileState(dto, "bash_guard.py"); fs != discipline.StateModified {
		t.Fatalf("a kept file reports as %s, not modified", fs)
	}
	if msg, err := a.DisciplineAction("reset", "bash_guard.py"); err != nil || !strings.HasPrefix(msg, "reset:") {
		t.Fatalf("reset: %q %v", msg, err)
	}
	if fs := fileState(a.DisciplineDoctor(false), "bash_guard.py"); fs != discipline.StateCurrent {
		t.Fatalf("state after reset: %s", fs)
	}
	if _, err := a.DisciplineAction("burn", "bash_guard.py"); err == nil {
		t.Fatal("unknown action must fail")
	}

	// Uninstall from the island.
	up, err := a.DisciplineUninstallPlan()
	if err != nil || up.HasErrors() || up.Op != "uninstall" {
		t.Fatalf("uninstall plan: %v %+v", err, up)
	}
	if _, err := a.DisciplineUninstall(); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	dto = a.DisciplineDoctor(false)
	if dto.Report.Rev.Layout != discipline.LayoutNone || dto.Action != "install" || dto.Receipt != nil {
		t.Fatalf("after uninstall: %+v action %q receipt %v", dto.Report.Rev, dto.Action, dto.Receipt)
	}
	if n := len(dto.Ledger); n != 4 || dto.Ledger[3].Op != "uninstall" || dto.FlippedAt != dto.Ledger[3].TS.Unix() {
		t.Fatalf("ledger after uninstall: %+v", dto.Ledger)
	}
	if _, err := os.Lstat(l.LockPath()); err == nil {
		t.Fatal("lock left behind")
	}
}

func installedPath(dto DisciplineDTO, base string) string {
	for _, f := range dto.Report.Files {
		if filepath.Base(f.Path) == base {
			return f.InstalledPath
		}
	}
	return ""
}

func fileState(dto DisciplineDTO, base string) string {
	for _, f := range dto.Report.Files {
		if filepath.Base(f.Path) == base {
			return f.State
		}
	}
	return "?"
}

// The sheet's rule: Install is refused until the gate passed and any
// verdict flips are acknowledged. Neither path reaches Commit, so nothing
// is written.
func TestDisciplineApplyRefusesFailedGateAndUnackedFlips(t *testing.T) {
	a, l := discApp(t)
	d := a.disc
	d.plan = &discipline.Plan{Op: "setup"}
	d.staged = &discipline.Staged{Gate: discipline.GateResult{Passed: false}}
	if _, err := a.DisciplineApply(true); err != discipline.ErrGateFailed {
		t.Fatalf("failed gate: %v", err)
	}
	d.staged = &discipline.Staged{Gate: discipline.GateResult{Passed: true, Replay: discipline.ReplayResult{Ran: true, NewAllows: []string{"Bash rm -rf x"}}}}
	if _, err := a.DisciplineApply(false); err == nil || !strings.Contains(err.Error(), "1 verdict") {
		t.Fatalf("unacked flips: %v", err)
	}
	if _, err := os.Stat(l.ReceiptPath()); err == nil {
		t.Fatal("a refused apply wrote a receipt")
	}
	a.DisciplineAbandon()
	if d.staged != nil || d.plan != nil {
		t.Fatal("abandon must clear the staged plan")
	}
}

// Stage twice: the second call abandons the first revision dir before
// writing its own, so a changed choice never leaves a stray directory.
func TestDisciplineRestageAbandonsPrevious(t *testing.T) {
	a, _ := discApp(t)
	first, err := a.DisciplineStage(discipline.DefaultPlanOptions())
	if err != nil || first.Error != "" {
		t.Fatalf("stage 1: %v %q", err, first.Error)
	}
	dir := a.disc.staged.RevDir
	opts := discipline.DefaultPlanOptions()
	opts.Env = true
	second, err := a.DisciplineStage(opts)
	if err != nil || second.Error != "" {
		t.Fatalf("stage 2: %v %q", err, second.Error)
	}
	if a.disc.staged == nil || !a.disc.plan.Options.Env {
		t.Fatal("second stage must hold the new options")
	}
	// A fresh install stages into the bundle's rev dir both times: the
	// second stage removed the first dir and wrote its own in its place.
	if a.disc.staged.RevDir != dir {
		t.Fatalf("second stage dir %s, first %s", a.disc.staged.RevDir, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("staged dir missing after restage: %v", err)
	}
	a.DisciplineAbandon()
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("abandon left the staged dir")
	}
}

// The report is cached by the mtimes it depends on plus a time floor, so
// the 1.5s poll never shells out; an action invalidates it at once.
func TestDisciplineDoctorCache(t *testing.T) {
	a, l := discApp(t)
	runs := 0
	a.disc.env.LookPath = func(name string) (string, error) { runs++; return "/usr/bin/" + name, nil }
	a.DisciplineDoctor(false)
	a.DisciplineDoctor(false)
	if runs == 0 {
		t.Fatal("first call must run the doctor")
	}
	n := runs
	a.DisciplineDoctor(false)
	if runs != n {
		t.Fatal("cached report must not re-run the doctor")
	}
	// A settings.json write changes the signature.
	if err := os.WriteFile(l.SettingsPath(), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.DisciplineDoctor(false)
	if runs == n {
		t.Fatal("a changed settings.json must re-run the doctor")
	}
	n = runs
	a.DisciplineDoctor(true)
	if runs == n {
		t.Fatal("force must re-run the doctor")
	}
}
