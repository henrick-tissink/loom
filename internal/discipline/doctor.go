package discipline

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/henricktissink/loom/internal/diet"
	"github.com/henricktissink/loom/internal/usage"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Severity of a doctor issue. Red means sessions are, or are about to be,
// broken (a fail-closed guard that cannot run denies every Bash call).
// Amber means enforcement is weaker than the bundle intends. Info is
// something the person may want to know and nothing loom would act on.
const (
	SevRed   = "red"
	SevAmber = "amber"
	SevInfo  = "info"
)

// File states, see docs/DISCIPLINE.md §4.
const (
	StateCurrent  = "current"  // matches the bundled revision
	StateStale    = "stale"    // matches a known earlier revision or snapshot: safe to replace
	StateModified = "modified" // matches nothing shipped: the user's
	StateMissing  = "missing"
	StateAbsent   = "absent" // this layout has no place for the file (legacy: examples, REV, …)
	StateInline   = "inline" // legacy rules: the text sits inside CLAUDE.md
)

// Issue is one doctor finding.
type Issue struct {
	Severity string `json:"severity"`
	Group    string `json:"group"`
	Message  string `json:"message"`
	Fix      string `json:"fix,omitempty"`
}

// Report is everything `loom doctor` learned. Field names are the JSON
// contract the GUI and the Done-when checks read.
type Report struct {
	CheckedAt     time.Time      `json:"checkedAt"`
	Rev           RevReport      `json:"rev"`
	Health        string         `json:"health"` // green | amber | red
	Issues        []Issue        `json:"issues"`
	Prereq        PrereqReport   `json:"prereq"`
	Files         []FileState    `json:"files"`
	Registrations RegReport      `json:"registrations"`
	Settings      SettingsReport `json:"settings"`
	Operate       OperateReport  `json:"operate"`
	Path          PathReport     `json:"path"`
	State         StateReport    `json:"state"`
	Rules         RulesReport    `json:"rules"`
	Firing        FiringReport   `json:"firing"`
	Suites        *SuitesReport  `json:"suites,omitempty"`
	Layout        LayoutReport   `json:"layout"`
	// Diet is the context-diet group (phase 5): filled by the caller, which
	// has the transcript index and the project list the doctor does not.
	Diet *diet.Report `json:"diet,omitempty"`
}

type RevReport struct {
	Bundled   string `json:"bundled"`
	Installed string `json:"installed"` // r3 | local | none
	Layout    string `json:"layout"`    // rev | legacy | none
}

type LayoutReport struct {
	ConfigDir  string `json:"configDir"`
	StateDir   string `json:"stateDir"`
	HooksDir   string `json:"hooksDir"`
	LogDir     string `json:"logDir"` // where the logs are being written today
	Deadline   string `json:"deadline"`
	Settings   string `json:"settings"`
	ClaudeMD   string `json:"claudeMd"`
	CurrentRev string `json:"currentRevDir,omitempty"`
}

type Tool struct {
	Found   bool   `json:"found"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Runs    bool   `json:"runs"`             // python3: actually executes
	Tested  string `json:"tested,omitempty"` // claude: tested | newer | older | unknown
}

type PrereqReport struct {
	Python3 Tool `json:"python3"`
	Jq      Tool `json:"jq"`
	Git     Tool `json:"git"`
	Tmux    Tool `json:"tmux"`
	Claude  Tool `json:"claude"`
}

type FileState struct {
	Path          string `json:"path"`
	Target        string `json:"target"`
	InstalledPath string `json:"installedPath,omitempty"`
	State         string `json:"state"`
	Matches       string `json:"matches,omitempty"` // for stale: which rev/snapshot
	Executable    bool   `json:"executable"`
	WantExec      bool   `json:"wantExec"`
	Quarantined   bool   `json:"quarantined"`
}

type RegItem struct {
	Event      string `json:"event"`
	Matcher    string `json:"matcher"`
	Script     string `json:"script"`
	Found      int    `json:"found"` // how many settings entries match
	Timeout    int    `json:"timeout"`
	WantTO     int    `json:"wantTimeout"`
	OnFailure  string `json:"onFailure,omitempty"`
	WantOnFail string `json:"wantOnFailure,omitempty"`
	ScriptOK   bool   `json:"scriptExists"`
	OK         bool   `json:"ok"`
}

type RegReport struct {
	Expected        int       `json:"expected"`
	Matched         int       `json:"matched"`
	Items           []RegItem `json:"items"`
	DisableAllHooks bool      `json:"disableAllHooks"`
	Others          []string  `json:"others"` // hook commands registered that are not the bundle's
	SettingsParsed  bool      `json:"settingsParsed"`
}

type SettingsReport struct {
	StatusLineSet  bool              `json:"statusLineSet"`
	StatusLineOurs bool              `json:"statusLineOurs"`
	StatusLine     string            `json:"statusLine,omitempty"`
	Env            map[string]EnvVal `json:"env"`
	AutoCompactOK  bool              `json:"autoCompactAboveBudget"`
	PathHasLocal   bool              `json:"envPathHasLocalBin"`
	ContextBudget  int               `json:"contextBudget"`
}

type EnvVal struct {
	Want string `json:"want"`
	Have string `json:"have,omitempty"`
	Set  bool   `json:"set"`
}

type OperateReport struct {
	DenyRule    bool     `json:"denyRule"`
	DenyEntries []string `json:"denyEntries"`
}

type Resolves struct {
	Known    bool `json:"known"` // the PATH could be read at all
	Deadline bool `json:"deadline"`
	Python3  bool `json:"python3"`
}

type PathReport struct {
	Login       Resolves `json:"login"`
	SettingsEnv Resolves `json:"settingsEnv"`
	Tmux        Resolves `json:"tmux"`
}

type DirState struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Writable bool   `json:"writable"`
}

type StateReport struct {
	Dirs      []DirState `json:"dirs"`
	LegacyLog bool       `json:"legacyLogs"` // logs still written beside the hooks
}

type RulesReport struct {
	ImportLines int    `json:"importLines"`
	Inline      bool   `json:"inline"` // the bundled rules text appears verbatim in CLAUDE.md
	RulesState  string `json:"rulesFileState"`
	ClaudeMD    bool   `json:"claudeMdExists"`
}

type FiringReport struct {
	LogsFound bool              `json:"logsFound"`
	Last      map[string]string `json:"lastRecord"` // hook -> ts
	Quiet     []string          `json:"quiet"`      // hooks with no record in 7 days while others fired
}

type SuitesReport struct {
	Ran    bool   `json:"ran"`
	Passed bool   `json:"passed"`
	Output string `json:"output"`
}

// Env is what the doctor asks the machine; tests replace it.
type Env struct {
	LookPath    func(string) (string, error)
	Run         func(ctx context.Context, name string, args ...string) (string, error)
	Writable    func(string) bool
	Quarantined func(string) bool
	Shell       string
	Now         func() time.Time
}

// DefaultEnv asks the real machine. Every command it runs is read-only.
func DefaultEnv() Env {
	return Env{
		LookPath: exec.LookPath,
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, name, args...).Output()
			return strings.TrimSpace(string(out)), err
		},
		Writable: func(p string) bool { return unix.Access(p, unix.W_OK) == nil },
		Quarantined: func(p string) bool {
			if runtime.GOOS != "darwin" {
				return false
			}
			err := exec.Command("xattr", "-p", "com.apple.quarantine", p).Run()
			return err == nil
		},
		Shell: firstNonEmpty(os.Getenv("SHELL"), "/bin/sh"),
		Now:   time.Now,
	}
}

// Options for one doctor run.
type Options struct {
	Deep bool // also run the bundled suites against the installed hooks (read-only: temp dirs)
}

// Doctor inspects the machine and never writes to it.
func Doctor(ctx context.Context, l Layout, env Env, opts Options) Report {
	r := Report{CheckedAt: env.Now(), Issues: []Issue{}}
	man, err := LoadManifest()
	if err != nil {
		r.add(SevRed, "bundle", "embedded manifest unreadable: "+err.Error(), "")
	}
	regs, err := LoadRegistrations()
	if err != nil {
		r.add(SevRed, "bundle", "embedded registrations unreadable: "+err.Error(), "")
	}
	kind, rev := l.Installed()
	if kind == LayoutNone {
		rev = "none"
	}
	r.Rev = RevReport{Bundled: BundledRev(), Installed: rev, Layout: kind}
	r.Layout = LayoutReport{
		ConfigDir: l.ConfigDir, StateDir: l.StateDir, HooksDir: l.HooksDir(),
		Deadline: l.Deadline(), Settings: l.SettingsPath(), ClaudeMD: l.ClaudeMD(),
	}
	if kind == LayoutRev {
		if t, err := os.Readlink(l.CurrentLink()); err == nil {
			r.Layout.CurrentRev = t
		}
	}
	settings := readSettings(l.SettingsPath())
	r.Registrations.SettingsParsed = settings != nil

	r.prereq(ctx, env, man)
	r.files(l, env, man, regs, kind)
	r.registrations(l, regs, settings, kind)
	r.settings(l, regs, settings)
	r.operate(l, settings)
	r.paths(ctx, l, env, settings)
	r.state(l, env)
	r.rules(l, man, kind)
	r.firing(l, regs, env)
	if kind == LayoutLegacy {
		r.add(SevAmber, "layout", "legacy layout: hooks/enforce is a real directory, so an upgrade cannot swap all hook files at once",
			"loom setup (phase 2) moves it to a per-revision directory behind a symlink")
	}
	if kind == LayoutNone {
		r.add(SevAmber, "layout", "the discipline bundle is not installed", "loom setup (phase 2)")
	}
	if opts.Deep && kind != LayoutNone && r.Prereq.Python3.Runs {
		r.Suites = runSuites(ctx, l, env)
		if r.Suites.Ran && !r.Suites.Passed {
			r.add(SevAmber, "suites", "the bundled suites fail against the installed hooks", "see the suites output")
		}
	}
	r.Health = "green"
	for _, is := range r.Issues {
		if is.Severity == SevRed {
			r.Health = "red"
			break
		}
		if is.Severity == SevAmber {
			r.Health = "amber"
		}
	}
	return r
}

func (r *Report) add(sev, group, msg, fix string) {
	r.Issues = append(r.Issues, Issue{Severity: sev, Group: group, Message: msg, Fix: fix})
}

// --- prerequisites -------------------------------------------------------

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

func (r *Report) prereq(ctx context.Context, env Env, man Manifest) {
	look := func(name string) Tool {
		p, err := env.LookPath(name)
		return Tool{Found: err == nil, Path: p}
	}
	py := look("python3")
	if py.Found {
		out, err := env.Run(ctx, "python3", "-c", "import sys; print('.'.join(map(str, sys.version_info[:3])))")
		py.Runs = err == nil && versionRe.MatchString(out)
		py.Version = versionRe.FindString(out)
	}
	r.Prereq.Python3 = py
	if !py.Found || !py.Runs {
		r.add(SevRed, "prereq", "python3 does not run: every hook is a python script and bash_guard fails closed, so Bash would be denied in every session",
			"install python3 (on macOS: xcode-select --install, or brew install python)")
	}
	r.Prereq.Jq = look("jq")
	if !r.Prereq.Jq.Found {
		r.add(SevAmber, "prereq", "jq not found: executable Done-when checks written with jq will fail", "brew install jq")
	}
	r.Prereq.Git = look("git")
	r.Prereq.Tmux = look("tmux")
	cl := look("claude")
	if cl.Found {
		out, _ := env.Run(ctx, "claude", "--version")
		cl.Version = versionRe.FindString(out)
		cl.Tested = testedState(cl.Version, man.ClaudeCodeTested)
		if cl.Tested == "newer" {
			r.add(SevInfo, "prereq", "claude "+cl.Version+" is newer than the versions this bundle was tested with ("+strings.Join(man.ClaudeCodeTested, ", ")+")", "")
		}
	} else {
		r.add(SevAmber, "prereq", "claude not found on PATH", "")
	}
	r.Prereq.Claude = cl
}

// testedState compares a version with the min and max of the tested list.
func testedState(v string, tested []string) string {
	if v == "" || len(tested) == 0 {
		return "unknown"
	}
	lo, hi := tested[0], tested[0]
	for _, t := range tested[1:] {
		if cmpVersion(t, lo) < 0 {
			lo = t
		}
		if cmpVersion(t, hi) > 0 {
			hi = t
		}
	}
	switch {
	case cmpVersion(v, lo) < 0:
		return "older"
	case cmpVersion(v, hi) > 0:
		return "newer"
	}
	return "tested"
}

func cmpVersion(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// --- files ---------------------------------------------------------------

func (r *Report) files(l Layout, env Env, man Manifest, regs Registrations, kind string) {
	// Only what Claude Code or a shell executes by path must carry the exec
	// bit: the registered hook scripts and the deadline wrapper. Report
	// tools and the context bar are invoked through an interpreter.
	registered := map[string]bool{"bin/deadline": true}
	for _, h := range regs.Hooks {
		registered[h.Script] = true
	}
	for _, f := range man.Files {
		fsx := FileState{Path: f.Path, Target: f.Target, WantExec: f.Mode == "0755" && registered[f.Path]}
		p, ok := l.InstalledPath(f, kind)
		if !ok {
			fsx.State = StateAbsent
			r.Files = append(r.Files, fsx)
			continue
		}
		fsx.InstalledPath = p
		if kind == LayoutLegacy && f.Path == rulesName {
			// The legacy install carries the rules inline in CLAUDE.md.
			b, err := os.ReadFile(p)
			switch {
			case err != nil:
				fsx.State = StateMissing
			case bytes.Contains(b, mustRead(f.Path)):
				fsx.State = StateInline
			default:
				fsx.State = StateModified
			}
			r.Files = append(r.Files, fsx)
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			fsx.State = StateMissing
			r.Files = append(r.Files, fsx)
			if Dir(f.Path) == "enforce" || f.Path == "bin/deadline" {
				r.add(SevRed, "files", p+" is missing", "loom setup")
			}
			continue
		}
		fsx.State, fsx.Matches = classify(sha256File(p), f, man)
		fsx.Executable = fi.Mode()&0o100 != 0
		fsx.Quarantined = env.Quarantined(p)
		if fsx.WantExec && !fsx.Executable {
			r.add(SevRed, "files", p+" is not executable: the registered hook (or the deadline wrapper) cannot run", "chmod 755 "+p)
		}
		if fsx.Quarantined && fsx.WantExec {
			r.add(SevRed, "files", p+" carries the macOS quarantine attribute and may be refused", "xattr -d com.apple.quarantine "+p)
		}
		if fsx.State == StateStale {
			r.add(SevAmber, "files", p+" matches "+fsx.Matches+", not "+man.Rev, "loom setup upgrades it")
		}
		if fsx.State == StateModified {
			r.add(SevInfo, "files", p+" is locally modified (matches no shipped revision)", "")
		}
		r.Files = append(r.Files, fsx)
	}
}

// classify compares an installed file's hash with the bundle: current when
// it matches this revision, stale when it matches a revision or snapshot in
// the manifest's history (safe to replace), modified otherwise.
func classify(sum string, f ManifestFile, man Manifest) (state, matches string) {
	if sum == f.SHA256 {
		return StateCurrent, ""
	}
	names := make([]string, 0, len(man.History))
	for name := range man.History {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if man.History[name][f.Path] == sum {
			return StateStale, name
		}
	}
	return StateModified, ""
}

func mustRead(p string) []byte {
	b, _ := fs.ReadFile(Bundle(), p)
	return bytes.TrimSpace(b)
}

func sha256File(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- settings.json -------------------------------------------------------

func readSettings(p string) map[string]any {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

type hookEntry struct {
	event, matcher, command, onFailure string
	timeout                            int
}

func settingsHooks(settings map[string]any) []hookEntry {
	var out []hookEntry
	hooks, _ := settings["hooks"].(map[string]any)
	for event, groups := range hooks {
		gs, _ := groups.([]any)
		for _, g := range gs {
			gm, _ := g.(map[string]any)
			matcher, _ := gm["matcher"].(string)
			hs, _ := gm["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				cmd, _ := hm["command"].(string)
				of, _ := hm["onFailure"].(string)
				to := 0
				if t, ok := hm["timeout"].(float64); ok {
					to = int(t)
				}
				out = append(out, hookEntry{event: event, matcher: matcher, command: cmd, onFailure: of, timeout: to})
			}
		}
	}
	return out
}

func (r *Report) registrations(l Layout, regs Registrations, settings map[string]any, kind string) {
	r.Registrations.Expected = len(regs.Hooks)
	r.Registrations.Others = []string{}
	if settings == nil {
		r.add(SevRed, "registrations", l.SettingsPath()+" is missing or not valid JSON", "")
		return
	}
	if v, ok := settings["disableAllHooks"].(bool); ok && v {
		r.Registrations.DisableAllHooks = true
		r.add(SevRed, "registrations", "disableAllHooks is set: no hook runs", "remove disableAllHooks from settings.json")
	}
	entries := settingsHooks(settings)
	used := make([]bool, len(entries))
	for _, want := range regs.Hooks {
		item := RegItem{Event: want.Event, Matcher: want.Matcher, Script: want.Script, WantTO: want.Timeout, WantOnFail: want.OnFailure}
		wantPath := filepath.Join(l.HooksDir(), filepath.Base(want.Script))
		for i, e := range entries {
			if e.event != want.Event || normMatcher(e.matcher) != normMatcher(want.Matcher) {
				continue
			}
			if l.NormalizeCommand(e.command) != wantPath {
				continue
			}
			used[i] = true
			item.Found++
			item.Timeout, item.OnFailure = e.timeout, e.onFailure
		}
		if _, err := os.Stat(wantPath); err == nil {
			item.ScriptOK = true
		}
		item.OK = item.Found == 1 && item.ScriptOK && item.Timeout == want.Timeout && item.OnFailure == want.OnFailure
		label := want.Event + "/" + filepath.Base(want.Script)
		switch {
		case item.Found == 0:
			r.add(SevRed, "registrations", label+" is not registered in settings.json", "loom setup")
		case item.Found > 1:
			r.add(SevRed, "registrations", label+" is registered "+strconv.Itoa(item.Found)+" times: Claude Code runs every copy", "remove the duplicates")
		case !item.ScriptOK:
			r.add(SevRed, "registrations", label+" is registered but "+wantPath+" does not exist", "loom setup")
		default:
			if item.Timeout != want.Timeout {
				r.add(SevAmber, "registrations", fmt.Sprintf("%s timeout is %d, bundle expects %d", label, item.Timeout, want.Timeout), "")
			}
			if item.OnFailure != want.OnFailure {
				sev := SevAmber
				if want.OnFailure == "block" {
					sev = SevRed
				}
				r.add(sev, "registrations", fmt.Sprintf("%s onFailure is %q, bundle expects %q: a guard crash would let commands through", label, item.OnFailure, want.OnFailure), "")
			}
		}
		if item.Found == 1 && item.ScriptOK {
			r.Registrations.Matched++
		}
		r.Registrations.Items = append(r.Registrations.Items, item)
	}
	for i, e := range entries {
		if !used[i] {
			r.Registrations.Others = append(r.Registrations.Others, e.event+": "+e.command)
		}
	}
	sort.Strings(r.Registrations.Others)
}

// normMatcher: an absent, empty or "*" matcher all mean "every tool".
func normMatcher(m string) string {
	if m == "*" {
		return ""
	}
	return m
}

func (r *Report) settings(l Layout, regs Registrations, settings map[string]any) {
	r.Settings.Env = map[string]EnvVal{}
	r.Settings.ContextBudget = 200000
	if settings == nil {
		return
	}
	if sl, ok := settings["statusLine"].(map[string]any); ok {
		cmd, _ := sl["command"].(string)
		cmd, _ = usage.Untap(cmd) // loom's usage tap is not a status line of its own
		r.Settings.StatusLineSet = cmd != ""
		r.Settings.StatusLine = cmd
		r.Settings.StatusLineOurs = strings.Contains(cmd, "context-bar.sh")
	}
	if !r.Settings.StatusLineSet {
		r.add(SevAmber, "settings", "no statusLine: the context bar (budget, task file state) is not shown", "loom setup can wire scripts/context-bar.sh")
	} else if !r.Settings.StatusLineOurs {
		r.add(SevInfo, "settings", "statusLine belongs to another tool; loom leaves it alone", "")
	}
	envm, _ := settings["env"].(map[string]any)
	get := func(k string) (string, bool) {
		v, ok := envm[k]
		if !ok {
			return "", false
		}
		return fmt.Sprint(v), true
	}
	if v, ok := get("CLAUDE_CONTEXT_BUDGET"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			r.Settings.ContextBudget = n
		}
	}
	for k, want := range regs.Env {
		have, ok := get(k)
		r.Settings.Env[k] = EnvVal{Want: want, Have: have, Set: ok}
	}
	if v, ok := get("CLAUDE_CODE_AUTO_COMPACT_WINDOW"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			r.Settings.AutoCompactOK = n > r.Settings.ContextBudget
			if !r.Settings.AutoCompactOK {
				r.add(SevAmber, "settings", "CLAUDE_CODE_AUTO_COMPACT_WINDOW is not above the context budget, so auto-compaction fires before the budget nudge", "")
			}
		}
	}
	if p, ok := get("PATH"); ok {
		r.Settings.PathHasLocal = pathContains(p, filepath.Dir(l.Deadline()))
	}
}

func (r *Report) operate(l Layout, settings map[string]any) {
	r.Operate.DenyEntries = []string{}
	if settings == nil {
		return
	}
	perms, _ := settings["permissions"].(map[string]any)
	deny, _ := perms["deny"].([]any)
	for _, d := range deny {
		s, _ := d.(string)
		if strings.Contains(s, "hooks") && (strings.HasPrefix(s, "Edit(") || strings.HasPrefix(s, "Write(") || strings.HasPrefix(s, "MultiEdit(")) {
			r.Operate.DenyEntries = append(r.Operate.DenyEntries, s)
		}
	}
	r.Operate.DenyRule = len(r.Operate.DenyEntries) > 0
	if !r.Operate.DenyRule {
		r.add(SevInfo, "operate", "no permissions.deny entry stops Claude editing the hook directory (the handbook's operate layer)", "loom setup offers it; or add Edit("+l.ConfigDir+"/hooks/**) to permissions.deny (it covers writes too)")
	}
}

// --- PATH ----------------------------------------------------------------

func (r *Report) paths(ctx context.Context, l Layout, env Env, settings map[string]any) {
	resolve := func(p string, known bool) Resolves {
		if !known {
			return Resolves{}
		}
		return Resolves{Known: true, Deadline: execIn(p, "deadline"), Python3: execIn(p, "python3")}
	}
	login, err := env.Run(ctx, env.Shell, "-lc", `printf %s "$PATH"`)
	r.Path.Login = resolve(login, err == nil && login != "")
	var sp string
	if envm, ok := settings["env"].(map[string]any); ok {
		sp, _ = envm["PATH"].(string)
	}
	r.Path.SettingsEnv = resolve(sp, sp != "")
	tp, err := env.Run(ctx, "tmux", "-L", "loom", "show-environment", "-g", "PATH")
	tp = strings.TrimPrefix(tp, "PATH=")
	r.Path.Tmux = resolve(tp, err == nil && tp != "")
	anyKnown := r.Path.Login.Known || r.Path.SettingsEnv.Known || r.Path.Tmux.Known
	anyDeadline := r.Path.Login.Deadline || r.Path.SettingsEnv.Deadline || r.Path.Tmux.Deadline
	switch {
	case anyKnown && !anyDeadline:
		r.add(SevRed, "path", "deadline resolves in no PATH loom could read: background commands exit 127 while bash_guard still demands the wrapper",
			"add "+filepath.Dir(l.Deadline())+" to PATH in your shell profile")
	case r.Path.Login.Known && !r.Path.Login.Deadline:
		r.add(SevAmber, "path", "deadline is not on the login shell PATH: terminal sessions cannot run background commands", "add "+filepath.Dir(l.Deadline())+" to PATH in your shell profile")
	}
	if r.Path.Login.Known && !r.Path.Login.Python3 {
		r.add(SevRed, "path", "python3 is not on the login shell PATH", "")
	}
}

func pathContains(pathList, dir string) bool {
	for _, d := range filepath.SplitList(pathList) {
		if filepath.Clean(d) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

func execIn(pathList, name string) bool {
	for _, d := range filepath.SplitList(pathList) {
		if d == "" {
			continue
		}
		if fi, err := os.Stat(filepath.Join(d, name)); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

// --- state dirs and logs ---------------------------------------------------

func (r *Report) state(l Layout, env Env) {
	dirs := []string{l.StateDir}
	for _, s := range StateSubdirs {
		dirs = append(dirs, filepath.Join(l.StateDir, s))
	}
	for _, d := range dirs {
		ds := DirState{Path: d}
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			ds.Exists = true
			ds.Writable = env.Writable(d)
		}
		r.State.Dirs = append(r.State.Dirs, ds)
		if !ds.Exists {
			r.add(SevAmber, "state", d+" does not exist: the hooks create it on first use, but the task file nudge fires until then", "loom setup creates it")
		} else if !ds.Writable {
			r.add(SevRed, "state", d+" is not writable: hooks cannot record the finish line", "")
		}
	}
	if _, err := os.Stat(filepath.Join(l.LegacyLogDir(), "decisions.jsonl")); err == nil {
		r.State.LegacyLog = true
	}
	if r.State.LegacyLog && r.Rev.Layout != LayoutRev {
		r.Layout.LogDir = l.LegacyLogDir()
	} else {
		r.Layout.LogDir = l.LogDir()
	}
}

// --- rules ---------------------------------------------------------------

func (r *Report) rules(l Layout, man Manifest, kind string) {
	b, err := os.ReadFile(l.ClaudeMD())
	r.Rules.ClaudeMD = err == nil
	for _, f := range r.Files {
		if f.Path == rulesName {
			r.Rules.RulesState = f.State
		}
	}
	if err != nil {
		r.add(SevAmber, "rules", l.ClaudeMD()+" is missing: the rules are not loaded", "loom setup")
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == ImportLine {
			r.Rules.ImportLines++
		}
	}
	r.Rules.Inline = bytes.Contains(b, mustRead(rulesName))
	switch {
	case r.Rules.ImportLines > 1:
		r.add(SevAmber, "rules", "CLAUDE.md imports the rules more than once", "keep one "+ImportLine+" line")
	case r.Rules.ImportLines == 1 && r.Rules.Inline:
		r.add(SevAmber, "rules", "CLAUDE.md both imports the rules and carries them inline: the model reads them twice", "remove the inline copy")
	case r.Rules.ImportLines == 0 && !r.Rules.Inline:
		r.add(SevAmber, "rules", "CLAUDE.md carries neither the import line nor the bundled rules text", "loom setup adds "+ImportLine)
	case kind == LayoutRev && r.Rules.ImportLines == 0 && r.Rules.Inline:
		r.add(SevInfo, "rules", "CLAUDE.md carries the rules inline rather than importing the installed copy", "")
	}
}

// --- firing: are the hooks actually producing records? ----------------------

var logHookNames = map[string]string{
	"bash_guard.py": "bash_guard", "test_rerun_log.py": "test_rerun", "stop_guard.py": "stop_guard",
	"ctx_budget.py": "ctx_budget", "precompact_checkpoint.py": "precompact", "session_checkpoint.py": "session_checkpoint",
}

func (r *Report) firing(l Layout, regs Registrations, env Env) {
	r.Firing.Last = map[string]string{}
	r.Firing.Quiet = []string{}
	var paths []string
	for _, d := range []string{l.LegacyLogDir(), l.LogDir()} {
		paths = append(paths, filepath.Join(d, "decisions.jsonl"), filepath.Join(d, "allows.jsonl"))
	}
	for _, p := range paths {
		for hook, ts := range lastRecordPerHook(p) {
			if ts > r.Firing.Last[hook] {
				r.Firing.Last[hook] = ts
			}
			r.Firing.LogsFound = true
		}
	}
	if !r.Firing.LogsFound {
		return
	}
	now := env.Now()
	newest := ""
	for _, ts := range r.Firing.Last {
		if ts > newest {
			newest = ts
		}
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", newest, time.Local); err != nil || now.Sub(t) > 24*time.Hour {
		return // nothing fired recently at all: not a per-hook signal
	}
	seen := map[string]bool{}
	for _, reg := range regs.Hooks {
		name := logHookNames[filepath.Base(reg.Script)]
		// precompact only fires when a session compacts; a quiet week is normal.
		if name == "" || name == "precompact" || seen[name] {
			continue
		}
		seen[name] = true
		t, err := time.ParseInLocation("2006-01-02T15:04:05", r.Firing.Last[name], time.Local)
		if err != nil || now.Sub(t) > 7*24*time.Hour {
			r.Firing.Quiet = append(r.Firing.Quiet, name)
		}
	}
	sort.Strings(r.Firing.Quiet)
	if len(r.Firing.Quiet) > 0 {
		r.add(SevAmber, "firing", "no log record in 7 days from: "+strings.Join(r.Firing.Quiet, ", ")+" (other hooks fired today): a hook may be failing silently or Claude Code's payload shape changed", "run loom doctor --deep")
	}
}

// lastRecordPerHook reads the tail of a JSONL log (at most 4 MB) and
// returns the newest timestamp per hook.
func lastRecordPerHook(p string) map[string]string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	const tail = 4 << 20
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	if fi.Size() > tail {
		if _, err := f.Seek(fi.Size()-tail, io.SeekStart); err != nil {
			return nil
		}
	}
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var rec struct {
			TS   string `json:"ts"`
			Hook string `json:"hook"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Hook == "" {
			continue
		}
		if rec.TS > out[rec.Hook] {
			out[rec.Hook] = rec.TS
		}
	}
	return out
}

// --- deep: the suites against the installed hooks ---------------------------

func runSuites(ctx context.Context, l Layout, env Env) *SuitesReport {
	rep := &SuitesReport{}
	tmp, err := os.MkdirTemp("", "loom-doctor-suites-")
	if err != nil {
		rep.Output = err.Error()
		return rep
	}
	defer os.RemoveAll(tmp)
	tests := filepath.Join(tmp, "tests")
	if err := copyBundleDir("tests", tests); err != nil {
		rep.Output = err.Error()
		return rep
	}
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(tests, "run_all.sh"))
	cmd.Env = append(os.Environ(), "HOOKS_DIR="+l.HooksDir(), "CLAUDE_DECISIONS_LOG=/dev/null", "CLAUDE_ALLOWS_LOG=/dev/null")
	// Kill the whole process group on timeout, not just bash: a suite's
	// grandchild holding stdout open would otherwise outlive the deadline.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	rep.Ran = true
	rep.Output = string(out)
	rep.Passed = err == nil && strings.Contains(rep.Output, "OVERALL: PASS")
	return rep
}

// copyBundleDir writes one bundle directory to disk (used only for temp dirs).
func copyBundleDir(dir, dst string) error {
	return fs.WalkDir(Bundle(), dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, filepath.FromSlash(p))
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := fs.ReadFile(Bundle(), p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if fileMode(p) == "0755" {
			mode = 0o755
		}
		return os.WriteFile(out, b, mode)
	})
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
