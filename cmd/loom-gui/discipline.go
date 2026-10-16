package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/diet"
	"github.com/henricktissink/loom/internal/discipline"
)

// The GUI's view of the Agent Discipline layer (docs/DISCIPLINE.md §5,
// phase 3): the doctor's report behind a health dot and an island, and the
// gate sheet, which is the one place the GUI writes to the machine. Every
// write goes through internal/discipline exactly as `loom setup` does; the
// GUI adds no step of its own. It is a human surface, so it does not call
// RefuseIfAgent: nothing here runs unless a person clicks Install.

type disciplineState struct {
	mu     sync.Mutex
	layout discipline.Layout
	env    discipline.Env
	now    func() time.Time

	// The report is recomputed when a watched path's mtime changes or the
	// last check is older than the floor; the doctor shells out (claude
	// --version, the login shell's PATH) so it is not run per poll.
	report    *discipline.Report
	checkedAt time.Time
	stamp     string
	floor     time.Duration

	// One staged revision at a time, between DisciplineStage and
	// DisciplineApply or DisciplineAbandon.
	plan   *discipline.Plan
	staged *discipline.Staged

	skipGate bool // tests only: Stage without running the gate

	// diet builds the context-diet group for a report; nil leaves it out.
	// Set by main from the store and the resolver, which the doctor
	// itself does not know. It shells out (`claude plugin details` per
	// enabled plugin), so it is cached on its own: ~/.claude.json, which
	// it reads, changes on nearly every hook run and cannot be its stamp.
	diet      func(context.Context) *diet.Report
	dietRep   *diet.Report
	dietAt    time.Time
	dietStamp string
	dietFloor time.Duration
}

// dietSignature is the mtimes of the files whose change means the plugin
// list changed: the registry and settings.json (enabledPlugins).
func (d *disciplineState) dietSignature() string {
	var b strings.Builder
	for _, p := range []string{d.layout.SettingsPath(), filepath.Join(d.layout.ConfigDir, "plugins", "installed_plugins.json")} {
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%s=%d;", p, fi.ModTime().UnixNano())
		}
	}
	return b.String()
}

// dietCached returns the diet group, rebuilt when the plugin files changed,
// the floor passed, or force is set. Called with d.mu held.
func (d *disciplineState) dietCached(force bool) *diet.Report {
	if d.diet == nil {
		return nil
	}
	sig := d.dietSignature()
	if !force && d.dietRep != nil && sig == d.dietStamp && d.now().Sub(d.dietAt) < d.dietFloor {
		return d.dietRep
	}
	d.dietRep, d.dietStamp, d.dietAt = d.diet(context.Background()), sig, d.now()
	return d.dietRep
}

func newDisciplineState(l discipline.Layout, now func() time.Time) *disciplineState {
	return &disciplineState{layout: l, env: discipline.DefaultEnv(), now: now, floor: 60 * time.Second, dietFloor: 10 * time.Minute}
}

// DisciplineDTO is what the island and the dock dot render.
type DisciplineDTO struct {
	Report    discipline.Report        `json:"report"`
	Receipt   *discipline.Receipt      `json:"receipt"`
	Ledger    []discipline.LedgerEntry `json:"ledger"`
	Backups   []string                 `json:"backups"`
	FlippedAt int64                    `json:"flippedAt"` // unix seconds of the last setup, upgrade or uninstall; 0 = never
	// KeptPaths are the report's installedPath values the receipt lists as
	// kept. The receipt keys them by real path (symlinks resolved); the
	// report spells them through the current link, so the match is made here.
	KeptPaths []string `json:"keptPaths"`
	Headline  string   `json:"headline"`
	Action    string   `json:"action"` // "" | install | upgrade | repair
	Staged    bool     `json:"staged"`
	CheckedAt int64    `json:"checkedAt"`
}

// DisciplineDTO builds the DTO from a report plus the receipt side.
func (d *disciplineState) dto(rep discipline.Report) DisciplineDTO {
	out := DisciplineDTO{Report: rep, CheckedAt: rep.CheckedAt.Unix(), Backups: discipline.ListBackups(d.layout), Staged: d.staged != nil}
	if out.Backups == nil {
		out.Backups = []string{}
	}
	rc, _ := discipline.LoadReceipt(d.layout)
	out.Receipt = rc
	out.KeptPaths = []string{}
	if rc != nil && len(rc.Kept) > 0 {
		for _, f := range rep.Files {
			if f.InstalledPath == "" {
				continue
			}
			real := f.InstalledPath
			if r, err := filepath.EvalSymlinks(real); err == nil {
				real = r
			}
			if _, ok := rc.Kept[real]; ok {
				out.KeptPaths = append(out.KeptPaths, f.InstalledPath)
			}
		}
	}
	led, _ := discipline.ReadLedger(d.layout)
	if led == nil {
		led = []discipline.LedgerEntry{}
	}
	out.Ledger = led
	for _, e := range led {
		switch e.Op {
		case "setup", "upgrade", "uninstall":
			if e.TS.Unix() > out.FlippedAt {
				out.FlippedAt = e.TS.Unix()
			}
		}
	}
	out.Headline, out.Action = headline(rep, rc)
	return out
}

// headline is the island's one line and its one button.
func headline(rep discipline.Report, rc *discipline.Receipt) (string, string) {
	kept := 0
	if rc != nil {
		kept = len(rc.Kept)
	}
	edits := ""
	if kept == 1 {
		edits = " + 1 local edit"
	} else if kept > 1 {
		edits = fmt.Sprintf(" + %d local edits", kept)
	}
	switch rep.Rev.Layout {
	case discipline.LayoutNone:
		return "Not installed. Sessions run without the guards, the finish line and the context budget.", "install"
	case discipline.LayoutLegacy:
		return "An install loom did not make is in place (hooks copied by hand). Loom can take it over without losing your edits.", "install"
	}
	if rep.Health == "red" {
		for _, is := range rep.Issues {
			if is.Severity == "red" {
				return "Broken: " + is.Message, "repair"
			}
		}
		return "Broken.", "repair"
	}
	if rep.Rev.Installed != rep.Rev.Bundled {
		return fmt.Sprintf("%s available (installed %s).", rep.Rev.Bundled, rep.Rev.Installed), "upgrade"
	}
	stale := 0
	for _, f := range rep.Files {
		if f.State == discipline.StateStale || f.State == discipline.StateMissing {
			stale++
		}
	}
	if stale > 0 || rep.Registrations.Matched < rep.Registrations.Expected {
		return fmt.Sprintf("Enforcing · %s, but %d files or registrations need a refresh.", rep.Rev.Installed, stale+rep.Registrations.Expected-rep.Registrations.Matched), "repair"
	}
	if rep.Health == "amber" {
		return "Enforcing · " + rep.Rev.Installed + edits + " · " + firstIssue(rep), ""
	}
	return "Enforcing · " + rep.Rev.Installed + edits, ""
}

func firstIssue(rep discipline.Report) string {
	for _, is := range rep.Issues {
		if is.Severity == "amber" {
			return is.Message
		}
	}
	return ""
}

// signature is the mtimes of everything the report depends on.
func (d *disciplineState) signature() string {
	l := d.layout
	var b strings.Builder
	for _, p := range []string{l.SettingsPath(), l.ClaudeMD(), l.ReceiptPath(), l.LedgerPath(), l.Deadline(), filepath.Join(l.LogDir(), "decisions.jsonl")} {
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%s=%d;", p, fi.ModTime().UnixNano())
		}
	}
	for _, p := range []string{l.CurrentLink(), l.HooksDir(), l.ContextBar()} {
		if t, err := os.Readlink(p); err == nil {
			fmt.Fprintf(&b, "%s->%s;", p, t)
		} else if fi, err := os.Lstat(p); err == nil {
			fmt.Fprintf(&b, "%s=%d;", p, fi.ModTime().UnixNano())
		}
	}
	return b.String()
}

func (d *disciplineState) check(force bool) discipline.Report {
	d.mu.Lock()
	defer d.mu.Unlock()
	sig := d.signature()
	if !force && d.report != nil && sig == d.stamp && d.now().Sub(d.checkedAt) < d.floor {
		return *d.report
	}
	rep := discipline.Doctor(context.Background(), d.layout, d.env, discipline.Options{})
	rep.Diet = d.dietCached(force)
	d.report, d.stamp, d.checkedAt = &rep, sig, d.now()
	return rep
}

func (d *disciplineState) invalidate() {
	d.mu.Lock()
	d.report = nil
	d.mu.Unlock()
}

// DisciplineDoctor returns the current report (cached by mtime and a
// one-minute floor; force re-runs it now).
func (a *App) DisciplineDoctor(force bool) DisciplineDTO {
	if a.disc == nil {
		return DisciplineDTO{Headline: "Discipline is unavailable in this build.", Ledger: []discipline.LedgerEntry{}, Backups: []string{}, KeptPaths: []string{}}
	}
	rep := a.disc.check(force)
	a.disc.mu.Lock()
	defer a.disc.mu.Unlock()
	return a.disc.dto(rep)
}

// DisciplinePlan computes what setup would do with these options. Reads only.
func (a *App) DisciplinePlan(opts discipline.PlanOptions) (*discipline.Plan, error) {
	if a.disc == nil {
		return nil, errors.New("discipline unavailable")
	}
	rep := a.disc.check(true)
	return discipline.PlanSetup(a.disc.layout, rep, opts, a.now()), nil
}

// StageDTO is the gate sheet's second screen.
type StageDTO struct {
	Plan  *discipline.Plan      `json:"plan"`
	Gate  discipline.GateResult `json:"gate"`
	Error string                `json:"error,omitempty"`
}

// DisciplineStage plans, writes the revision dir where no session can see
// it, and runs the gate. Nothing a session uses changes. The result stays
// staged until DisciplineApply or DisciplineAbandon.
func (a *App) DisciplineStage(opts discipline.PlanOptions) (*StageDTO, error) {
	if a.disc == nil {
		return nil, errors.New("discipline unavailable")
	}
	d := a.disc
	d.mu.Lock()
	if d.staged != nil {
		discipline.Abandon(d.staged)
		d.staged, d.plan = nil, nil
	}
	d.mu.Unlock()
	rep := d.check(true)
	plan := discipline.PlanSetup(d.layout, rep, opts, a.now())
	out := &StageDTO{Plan: plan}
	if plan.HasErrors() {
		out.Error = strings.Join(plan.Errors, "; ")
		return out, nil
	}
	st, err := discipline.Stage(context.Background(), d.layout, plan, discipline.ApplyOptions{Now: a.now(), Build: guiBuild(), SkipGate: d.skipGate})
	if st != nil {
		out.Gate = st.Gate
	}
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	d.mu.Lock()
	d.plan, d.staged = plan, st
	d.mu.Unlock()
	return out, nil
}

// DisciplineApply commits the staged revision. ackFlips must be true when
// the replay found verdict changes; the sheet shows them first.
func (a *App) DisciplineApply(ackFlips bool) (*discipline.ApplyResult, error) {
	if a.disc == nil {
		return nil, errors.New("discipline unavailable")
	}
	d := a.disc
	d.mu.Lock()
	plan, st := d.plan, d.staged
	d.mu.Unlock()
	if st == nil || plan == nil {
		return nil, errors.New("nothing is staged: run the gate first")
	}
	if !st.Gate.Passed {
		return nil, discipline.ErrGateFailed
	}
	if st.Gate.Flips() > 0 && !ackFlips {
		return nil, fmt.Errorf("the replay found %d verdict changes; acknowledge them to install", st.Gate.Flips())
	}
	res, err := discipline.Commit(context.Background(), d.layout, plan, st, discipline.ApplyOptions{Now: a.now(), Build: guiBuild()})
	d.mu.Lock()
	d.plan, d.staged = nil, nil
	d.mu.Unlock()
	d.invalidate()
	return res, err
}

// DisciplineAbandon removes a staged revision this process created.
func (a *App) DisciplineAbandon() {
	if a.disc == nil {
		return
	}
	d := a.disc
	d.mu.Lock()
	defer d.mu.Unlock()
	discipline.Abandon(d.staged)
	d.plan, d.staged = nil, nil
}

// DisciplineUninstallPlan shows what uninstall would undo. Reads only.
func (a *App) DisciplineUninstallPlan() (*discipline.Plan, error) {
	if a.disc == nil {
		return nil, errors.New("discipline unavailable")
	}
	rc, err := discipline.LoadReceipt(a.disc.layout)
	if err != nil {
		return nil, err
	}
	rep := a.disc.check(true)
	return discipline.PlanUninstall(a.disc.layout, rep, rc), nil
}

// DisciplineUninstall reverses the receipt.
func (a *App) DisciplineUninstall() (*discipline.ApplyResult, error) {
	if a.disc == nil {
		return nil, errors.New("discipline unavailable")
	}
	d := a.disc
	rc, err := discipline.LoadReceipt(d.layout)
	if err != nil {
		return nil, err
	}
	rep := d.check(true)
	plan := discipline.PlanUninstall(d.layout, rep, rc)
	if plan.HasErrors() {
		return nil, errors.New(strings.Join(plan.Errors, "; "))
	}
	res, err := discipline.ApplyUninstall(context.Background(), d.layout, plan, rc, discipline.ApplyOptions{Now: a.now(), Build: guiBuild()})
	d.invalidate()
	return res, err
}

// DisciplineAction is the modified-file row's Diff, Keep and Reset.
// Diff returns the unified diff (empty when the file matches); Keep and
// Reset return a one-line result.
func (a *App) DisciplineAction(action, file string) (string, error) {
	if a.disc == nil {
		return "", errors.New("discipline unavailable")
	}
	d := a.disc
	kind, _ := d.layout.Installed()
	f, installed, err := discipline.ResolveFile(d.layout, kind, file)
	if err != nil {
		return "", err
	}
	switch action {
	case "diff":
		return discipline.FileDiff(f, installed)
	case "keep":
		if err := discipline.Keep(d.layout, f, installed, a.now()); err != nil {
			return "", err
		}
		d.invalidate()
		return "kept: " + installed + " is yours; upgrades leave it alone", nil
	case "reset":
		dir, err := discipline.Reset(d.layout, f, installed, a.now())
		if err != nil {
			return "", err
		}
		d.invalidate()
		return "reset: bundle copy restored; previous copy in " + dir, nil
	}
	return "", fmt.Errorf("unknown action %q", action)
}

// guiBuild is the loom build id for receipts.
func guiBuild() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "gui-dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return "gui-" + v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 8 {
			return "gui-" + s.Value[:8]
		}
	}
	return "gui-dev"
}
