package discipline

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/jsonedit"
)

// A Plan is the list of writes setup or uninstall would make, computed
// from a doctor report and never from guesses. It is what the gate sheet
// and `loom setup --plan` show; Stage/Commit execute it.

// Step kinds, in execution order for setup.
const (
	StepMkdir      = "mkdir"
	StepInstallRev = "install-rev"
	StepGate       = "gate"
	StepMigrate    = "migrate-legacy"
	StepLogs       = "logs"
	StepLink       = "link"
	StepDeadline   = "deadline"
	StepHooks      = "settings-hooks"
	StepStatusLine = "settings-statusline"
	StepEnv        = "settings-env"
	StepDeny       = "settings-deny"
	StepDenyRetire = "settings-deny-retire"
	StepSettings   = "settings-write"
	StepClaudeMD   = "claudemd"
	StepReceipt    = "receipt"
	// uninstall
	StepUnmerge       = "settings-unmerge"
	StepClaudeMDUndo  = "claudemd-undo"
	StepUnlink        = "unlink"
	StepRemoveRev     = "remove-rev"
	StepRemoveDL      = "remove-deadline"
	StepRestoreLegacy = "restore-legacy"
	StepRemoveReceipt = "remove-receipt"
)

// Consent groups: core is on by default and cannot be split; the others
// are the person's call (docs/DISCIPLINE.md §5, gate sheet).
const (
	GroupCore       = "core"
	GroupRules      = "rules"
	GroupStatusLine = "statusline"
	GroupEnv        = "env"
	GroupDeny       = "deny"
)

type Step struct {
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Detail  string `json:"detail,omitempty"`
	Group   string `json:"group"`
	Enabled bool   `json:"enabled"`
	Diff    string `json:"diff,omitempty"`
}

// Conflict is an installed file that matches no shipped revision. The
// default choice is keep; Replace lists what the person chose to replace.
type Conflict struct {
	Path   string `json:"path"`
	Choice string `json:"choice"` // keep | replace
}

type Plan struct {
	Op        string       `json:"op"` // setup | uninstall
	Rev       string       `json:"rev"`
	FromRev   string       `json:"fromRev"`
	Layout    string       `json:"layout"`
	Steps     []Step       `json:"steps"`
	Errors    []string     `json:"errors"`
	Warnings  []string     `json:"warnings"`
	Conflicts []Conflict   `json:"conflicts"`
	Hooks     []HookChange `json:"hooks"`
	Options   PlanOptions  `json:"options"`
	Stamp     string       `json:"stamp,omitempty"` // discipline/<stamp> holds what setup moved aside
}

type PlanOptions struct {
	Rules      bool     `json:"rules"`      // import line in CLAUDE.md (default on)
	StatusLine bool     `json:"statusLine"` // set the status line when none is set (default on)
	Env        bool     `json:"env"`        // the three optional env keys (default off)
	Deny       bool     `json:"deny"`       // permissions.deny for the hook dir (default on)
	Replace    []string `json:"replace"`    // installed paths whose local edits may be overwritten
}

func DefaultPlanOptions() PlanOptions {
	return PlanOptions{Rules: true, StatusLine: true, Deny: true}
}

func (p *Plan) add(s Step) { p.Steps = append(p.Steps, s) }
func (p *Plan) errorf(format string, a ...any) {
	p.Errors = append(p.Errors, fmt.Sprintf(format, a...))
}
func (p *Plan) warnf(format string, a ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, a...))
}

// HasErrors reports whether the plan must not be applied.
func (p *Plan) HasErrors() bool { return len(p.Errors) > 0 }

func (o PlanOptions) replace(path string) bool {
	for _, r := range o.Replace {
		if samePath(r, path) {
			return true
		}
	}
	return false
}

// samePath compares two paths with symlinks resolved, so
// discipline/current/x and discipline/r3/x are the same file.
func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b) || mustEval(a) == mustEval(b)
}

// PlanSetup computes an install or upgrade from the doctor's report.
func PlanSetup(l Layout, rep Report, opts PlanOptions, now time.Time) *Plan {
	p := &Plan{Op: "setup", Rev: BundledRev(), FromRev: rep.Rev.Installed, Layout: rep.Rev.Layout,
		Errors: []string{}, Warnings: []string{}, Conflicts: []Conflict{}, Options: opts,
		Stamp: "local-" + now.Format("20060102-1504")}
	man, err := LoadManifest()
	if err != nil {
		p.errorf("embedded manifest: %v", err)
		return p
	}
	regs, err := LoadRegistrations()
	if err != nil {
		p.errorf("embedded registrations: %v", err)
		return p
	}
	if !rep.Prereq.Python3.Runs {
		p.errorf("python3 does not run on this machine; installing fail-closed hooks would deny every Bash call")
	}
	if rep.Rev.Layout == LayoutRev && cmpRev(rep.Rev.Installed, man.Rev) > 0 {
		p.errorf("installed %s is newer than the bundled %s: loom does not downgrade", rep.Rev.Installed, man.Rev)
	}

	// 1. state dirs
	var missing []string
	for _, d := range rep.State.Dirs {
		if !d.Exists {
			missing = append(missing, d.Path)
		}
	}
	if _, err := os.Stat(l.LogDir()); err != nil {
		missing = append(missing, l.LogDir())
	}
	if len(missing) > 0 {
		p.add(Step{Kind: StepMkdir, Target: l.StateDir, Detail: fmt.Sprintf("%d directories", len(missing)), Group: GroupCore, Enabled: true})
	}

	// 2. the revision directory
	revDir := l.RevDir(man.Rev)
	sameRev := rep.Rev.Layout == LayoutRev && rep.Rev.Installed == man.Rev
	if sameRev {
		var fill []string
		for _, f := range rep.Files {
			if f.Target != "rev" {
				continue
			}
			switch f.State {
			case StateStale, StateMissing:
				fill = append(fill, f.Path)
			case StateModified:
				real := filepath.Join(revDir, filepath.FromSlash(f.Path))
				if opts.replace(real) {
					p.Conflicts = append(p.Conflicts, Conflict{Path: real, Choice: "replace"})
					fill = append(fill, f.Path)
				} else {
					p.Conflicts = append(p.Conflicts, Conflict{Path: real, Choice: "keep"})
				}
			}
		}
		if len(fill) == 0 {
			p.add(Step{Kind: StepInstallRev, Target: revDir, Detail: "already current", Group: GroupCore})
		} else {
			p.add(Step{Kind: StepInstallRev, Target: revDir, Detail: fmt.Sprintf("refresh %d files in place", len(fill)), Group: GroupCore, Enabled: true})
		}
	} else {
		detail := fmt.Sprintf("%d files, new directory", len(man.Files))
		if _, err := os.Stat(revDir); err == nil {
			detail += " (an existing directory from an earlier attempt is replaced)"
		}
		p.add(Step{Kind: StepInstallRev, Target: revDir, Detail: detail, Group: GroupCore, Enabled: true})
	}
	p.add(Step{Kind: StepGate, Target: revDir, Detail: "preflight, 7 suites, 30-day replay of logged denials", Group: GroupCore, Enabled: true})

	// 3. legacy layout moved aside, logs moved under state
	if rep.Rev.Layout == LayoutLegacy {
		aside := filepath.Join(l.DisciplineDir(), p.Stamp)
		p.add(Step{Kind: StepMigrate, Target: l.HooksDir(), Detail: "moved to " + aside + " (nothing deleted)", Group: GroupCore, Enabled: true})
		for _, f := range rep.Files {
			if f.State == StateModified && Dir(f.Path) == "enforce" {
				p.warnf("%s is locally modified; it stays in %s and is not carried into %s (loom discipline diff shows the change)", f.InstalledPath, aside, man.Rev)
			}
		}
		if fi, err := os.Lstat(l.ContextBar()); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			p.add(Step{Kind: StepMigrate, Target: l.ContextBar(), Detail: "moved to " + filepath.Join(aside, "scripts"), Group: GroupCore, Enabled: true})
		}
		if rep.State.LegacyLog {
			p.add(Step{Kind: StepLogs, Target: l.LogDir(), Detail: "decisions.jsonl and allows.jsonl move from the hook dir (appended if the destination exists)", Group: GroupCore, Enabled: true})
		}
	}
	if rep.Rev.Layout == LayoutRev && rep.State.LegacyLog {
		p.add(Step{Kind: StepLogs, Target: l.LogDir(), Detail: "records that hooks already running at the last switch wrote beside the hooks are appended to the log", Group: GroupCore, Enabled: true})
	}
	if rep.Rev.Layout == LayoutRev {
		if fi, err := os.Lstat(l.HooksDir()); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			p.errorf("%s is a real directory although discipline/current exists; move it aside by hand", l.HooksDir())
		}
	}

	// 4. links
	linkDetail := "current → " + man.Rev + "; hooks/enforce and scripts/context-bar.sh point into it"
	if sameRev {
		p.add(Step{Kind: StepLink, Target: l.CurrentLink(), Detail: "unchanged", Group: GroupCore})
	} else {
		p.add(Step{Kind: StepLink, Target: l.CurrentLink(), Detail: linkDetail, Group: GroupCore, Enabled: true})
	}

	// 5. deadline
	for _, f := range rep.Files {
		if f.Path != "bin/deadline" {
			continue
		}
		switch f.State {
		case StateCurrent:
			p.add(Step{Kind: StepDeadline, Target: l.Deadline(), Detail: "current", Group: GroupCore})
		case StateModified:
			if opts.replace(l.Deadline()) {
				p.Conflicts = append(p.Conflicts, Conflict{Path: l.Deadline(), Choice: "replace"})
				p.add(Step{Kind: StepDeadline, Target: l.Deadline(), Detail: "replace your modified copy (backed up)", Group: GroupCore, Enabled: true})
			} else {
				p.Conflicts = append(p.Conflicts, Conflict{Path: l.Deadline(), Choice: "keep"})
				p.add(Step{Kind: StepDeadline, Target: l.Deadline(), Detail: "kept: locally modified", Group: GroupCore})
			}
		default:
			p.add(Step{Kind: StepDeadline, Target: l.Deadline(), Detail: "write (" + f.State + ")", Group: GroupCore, Enabled: true})
		}
	}

	// 6. settings.json
	doc, exists, err := jsonedit.ReadFile(l.SettingsPath())
	if err != nil {
		p.errorf("%v", err)
	} else {
		before := string(doc.Marshal("  "))
		if !exists {
			p.warnf("%s does not exist and will be created", l.SettingsPath())
		}
		p.Hooks = MergeHooks(doc, l, regs)
		var added, fixed, dup int
		for _, h := range p.Hooks {
			switch h.Action {
			case "added":
				added++
			case "fixed":
				fixed++
			case "duplicate":
				dup++
				p.errorf("hook %s is registered more than once in settings.json; remove the duplicate first", h.ID)
			}
		}
		p.add(Step{Kind: StepHooks, Target: l.SettingsPath(), Detail: fmt.Sprintf("add %d, fix %d, keep %d registrations", added, fixed, len(p.Hooks)-added-fixed-dup), Group: GroupCore, Enabled: added+fixed > 0})
		sl := l.StatusLineCommand(regs)
		switch {
		case rep.Settings.StatusLineSet:
			d := "already set"
			if !rep.Settings.StatusLineOurs {
				d = "another tool's; left alone"
			}
			p.add(Step{Kind: StepStatusLine, Target: "statusLine", Detail: d, Group: GroupStatusLine})
		case opts.StatusLine:
			MergeStatusLine(doc, sl)
			p.add(Step{Kind: StepStatusLine, Target: "statusLine", Detail: sl, Group: GroupStatusLine, Enabled: true})
		default:
			p.add(Step{Kind: StepStatusLine, Target: "statusLine", Detail: "skipped by option", Group: GroupStatusLine})
		}
		if opts.Env {
			w := MergeEnv(doc, regs.Env)
			p.add(Step{Kind: StepEnv, Target: "env", Detail: fmt.Sprintf("%d of %d keys absent, written", len(w), len(regs.Env)), Group: GroupEnv, Enabled: len(w) > 0})
		} else {
			p.add(Step{Kind: StepEnv, Target: "env", Detail: "off (--with env)", Group: GroupEnv})
		}
		if opts.Deny {
			a := MergeDeny(doc, l.DenyEntries(regs))
			p.add(Step{Kind: StepDeny, Target: "permissions.deny", Detail: fmt.Sprintf("%d entries added", len(a)), Group: GroupDeny, Enabled: len(a) > 0})
		} else {
			p.add(Step{Kind: StepDeny, Target: "permissions.deny", Detail: "off (--with deny)", Group: GroupDeny})
		}
		if rc, _ := LoadReceipt(l); rc != nil {
			if old := RetiredDeny(rc.Settings.Deny, l.DenyEntries(regs)); len(old) > 0 {
				detail := "remove entries an earlier setup added that the bundle no longer asks for: "
				if UnmergeDeny(doc, old) == 0 {
					detail = "forget entries an earlier setup added, already gone from settings.json: "
				}
				p.add(Step{Kind: StepDenyRetire, Target: "permissions.deny", Detail: detail + strings.Join(old, ", "), Group: GroupCore, Enabled: true})
			}
		}
		after := string(doc.Marshal("  "))
		diff := UnifiedDiff("settings.json", before, after)
		p.add(Step{Kind: StepSettings, Target: l.SettingsPath(), Detail: "re-read and merged again right before writing; backed up first", Group: GroupCore, Enabled: diff != "", Diff: diff})
	}

	// 7. CLAUDE.md
	if opts.Rules {
		cur, _ := os.ReadFile(l.ClaudeMD())
		out, action := PlanClaudeMD(cur, mustRead(rulesName))
		diff := UnifiedDiff("CLAUDE.md", string(cur), string(out))
		p.add(Step{Kind: StepClaudeMD, Target: l.ClaudeMD(), Detail: action, Group: GroupRules, Enabled: action != ClaudeMDNone, Diff: diff})
	} else {
		p.add(Step{Kind: StepClaudeMD, Target: l.ClaudeMD(), Detail: "off (--without rules)", Group: GroupRules})
	}
	p.add(Step{Kind: StepReceipt, Target: l.ReceiptPath(), Group: GroupCore, Enabled: true})
	return p
}

// PlanUninstall computes the reversal of what the receipt records.
func PlanUninstall(l Layout, rep Report, rc *Receipt) *Plan {
	p := &Plan{Op: "uninstall", Rev: BundledRev(), FromRev: rep.Rev.Installed, Layout: rep.Rev.Layout,
		Errors: []string{}, Warnings: []string{}, Conflicts: []Conflict{}, Options: DefaultPlanOptions()}
	if rc == nil {
		p.errorf("nothing to uninstall: no receipt at %s (loom only removes what it installed)", l.ReceiptPath())
		return p
	}
	regs, err := LoadRegistrations()
	if err != nil {
		p.errorf("embedded registrations: %v", err)
		return p
	}
	p.FromRev = rc.Rev
	doc, _, err := jsonedit.ReadFile(l.SettingsPath())
	if err != nil {
		p.errorf("%v", err)
	} else {
		before := string(doc.Marshal("  "))
		n := UnmergeHooks(doc, l, regs, rc.Settings.HooksAdded)
		UnmergeStatusLine(doc, rc.Settings.StatusLine)
		UnmergeEnv(doc, rc.Settings.Env)
		UnmergeDeny(doc, rc.Settings.Deny)
		diff := UnifiedDiff("settings.json", before, string(doc.Marshal("  ")))
		p.add(Step{Kind: StepUnmerge, Target: l.SettingsPath(), Detail: fmt.Sprintf("remove %d registrations loom added; status line, env and deny entries only if still loom's", n), Group: GroupCore, Enabled: diff != "", Diff: diff})
	}
	if rc.ClaudeMD.ImportLineAdded {
		cur, _ := os.ReadFile(l.ClaudeMD())
		rules := revFile(l, rc.Rev, rulesName)
		out, changed := UndoClaudeMD(cur, rules, rc.ClaudeMD.ReplacedInline)
		d := "remove the import line"
		if rc.ClaudeMD.ReplacedInline {
			d = "put the rules text back in place of the import line"
		}
		p.add(Step{Kind: StepClaudeMDUndo, Target: l.ClaudeMD(), Detail: d, Group: GroupRules, Enabled: changed, Diff: UnifiedDiff("CLAUDE.md", string(cur), string(out))})
	}
	ud := "current, hooks/enforce, scripts/context-bar.sh (only if they are loom's symlinks)"
	if len(rc.PrevLinks) > 0 {
		ud += fmt.Sprintf("; %d symlinks that were there before loom are put back", len(rc.PrevLinks))
	}
	p.add(Step{Kind: StepUnlink, Target: l.CurrentLink(), Detail: ud, Group: GroupCore, Enabled: true})
	keepN := 0
	for path, sha := range rc.Files {
		if strings.HasPrefix(path, l.RevDir(rc.Rev)) && sha256File(path) != sha {
			keepN++
		}
	}
	d := fmt.Sprintf("files still matching the receipt are removed; %d modified files are left", keepN)
	p.add(Step{Kind: StepRemoveRev, Target: l.RevDir(rc.Rev), Detail: d, Group: GroupCore, Enabled: true})
	if sha, ok := rc.Files[l.Deadline()]; ok {
		switch {
		case sha256File(l.Deadline()) != sha:
			p.add(Step{Kind: StepRemoveDL, Target: l.Deadline(), Detail: "kept: modified since install", Group: GroupCore})
		case rc.Restore[l.Deadline()] != "":
			p.add(Step{Kind: StepRemoveDL, Target: l.Deadline(), Detail: "the copy that was there before loom is put back from " + rc.Restore[l.Deadline()], Group: GroupCore, Enabled: true})
		default:
			p.add(Step{Kind: StepRemoveDL, Target: l.Deadline(), Group: GroupCore, Enabled: true})
		}
	}
	if rc.MigratedFrom != "" {
		if _, err := os.Stat(rc.MigratedFrom); err == nil {
			p.add(Step{Kind: StepRestoreLegacy, Target: l.HooksDir(), Detail: "moved back from " + rc.MigratedFrom, Group: GroupCore, Enabled: true})
		} else {
			p.warnf("%s (the pre-install hook dir) no longer exists; nothing to restore", rc.MigratedFrom)
		}
	}
	p.add(Step{Kind: StepRemoveReceipt, Target: l.ReceiptPath(), Group: GroupCore, Enabled: true})
	return p
}

// revFile reads a file from an installed revision dir, falling back to
// the embedded bundle.
func revFile(l Layout, rev, path string) []byte {
	if b, err := os.ReadFile(filepath.Join(l.RevDir(rev), filepath.FromSlash(path))); err == nil {
		return b
	}
	b, _ := fs.ReadFile(Bundle(), path)
	return b
}

// cmpRev orders "r3" < "r10"; unknown forms compare as 0.
func cmpRev(a, b string) int {
	ai, aok := revNum(a)
	bi, bok := revNum(b)
	if !aok || !bok {
		return 0
	}
	return ai - bi
}

func revNum(r string) (int, bool) {
	if !strings.HasPrefix(r, "r") {
		return 0, false
	}
	n := 0
	for _, c := range r[1:] {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, len(r) > 1
}

// RenderPlan prints a plan for the terminal.
func RenderPlan(p *Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · bundle %s · installed %s (%s layout)\n", p.Op, p.Rev, p.FromRev, p.Layout)
	for _, s := range p.Steps {
		mark := "[ ]"
		if s.Enabled {
			mark = "[x]"
		}
		fmt.Fprintf(&b, "%s %-20s %s", mark, s.Kind, s.Target)
		if s.Detail != "" {
			fmt.Fprintf(&b, "  — %s", s.Detail)
		}
		b.WriteString("\n")
	}
	for _, c := range p.Conflicts {
		fmt.Fprintf(&b, "conflict  %s: %s\n", c.Choice, c.Path)
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(&b, "warning   %s\n", w)
	}
	for _, e := range p.Errors {
		fmt.Fprintf(&b, "error     %s\n", e)
	}
	for _, s := range p.Steps {
		if s.Enabled && s.Diff != "" {
			b.WriteString("\n" + s.Diff)
		}
	}
	return b.String()
}
