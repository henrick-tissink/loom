package discipline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/jsonedit"
)

// Execution of a plan, in two halves so a caller can show the gate's
// result (and ask about replay flips) before anything a session can see
// changes:
//
//	Stage   writes the revision directory and runs the gate. Invisible to
//	        sessions: hooks/enforce still points wherever it did.
//	Commit  moves a legacy install aside, flips the symlinks, writes the
//	        deadline wrapper, merges settings.json and CLAUDE.md, verifies,
//	        and writes the receipt. Every step is undone in reverse if a
//	        later one fails.

// Staged is a revision directory that passed (or failed) the gate.
type Staged struct {
	RevDir  string     `json:"revDir"`
	Created bool       `json:"created"` // the directory is new and may be removed on abandon
	Gate    GateResult `json:"gate"`
}

type ApplyResult struct {
	BackupDir string   `json:"backupDir,omitempty"`
	Receipt   *Receipt `json:"receipt,omitempty"`
	Log       []string `json:"log"`
}

type ApplyOptions struct {
	Now      time.Time
	Build    string // loom build id for the receipt
	SkipGate bool   // tests only
}

var ErrGateFailed = errors.New("the install gate failed; nothing was changed")

// Stage writes the bundle's "rev" files into discipline/<rev> (a fresh
// directory, or a same-revision refresh of stale and chosen files) and
// runs the gate against it.
func Stage(ctx context.Context, l Layout, p *Plan, opts ApplyOptions) (*Staged, error) {
	if p.Op != "setup" {
		return nil, errors.New("stage: not a setup plan")
	}
	if p.HasErrors() {
		return nil, fmt.Errorf("plan has errors: %s", strings.Join(p.Errors, "; "))
	}
	man, err := LoadManifest()
	if err != nil {
		return nil, err
	}
	st := &Staged{RevDir: l.RevDir(man.Rev)}
	refresh := stepDetailHas(p, StepInstallRev, "refresh")
	fresh := stepEnabled(p, StepInstallRev) && !refresh
	switch {
	case fresh:
		tmp := filepath.Join(l.DisciplineDir(), "."+man.Rev+".tmp-"+strconv.Itoa(os.Getpid()))
		os.RemoveAll(tmp)
		if err := os.MkdirAll(tmp, 0o755); err != nil {
			return nil, err
		}
		// Every bundle file goes into the revision dir, the deadline wrapper
		// included: the dir is a complete, immutable copy of the bundle.
		for _, f := range man.Files {
			if err := writeBundleFile(f, filepath.Join(tmp, filepath.FromSlash(f.Path))); err != nil {
				os.RemoveAll(tmp)
				return nil, err
			}
		}
		if err := copyEmbedded(manifestName, filepath.Join(tmp, manifestName), 0o644); err != nil {
			os.RemoveAll(tmp)
			return nil, err
		}
		os.RemoveAll(st.RevDir)
		if err := os.Rename(tmp, st.RevDir); err != nil {
			os.RemoveAll(tmp)
			return nil, err
		}
		st.Created = true
	case refresh:
		for _, c := range p.Conflicts {
			if c.Choice == "replace" && strings.HasPrefix(c.Path, st.RevDir) {
				if _, err := Backup(l, "replace", opts.Now, c.Path); err != nil {
					return nil, err
				}
			}
		}
		for _, f := range man.Files {
			dst := filepath.Join(st.RevDir, filepath.FromSlash(f.Path))
			sum := sha256File(dst)
			if sum == f.SHA256 {
				continue
			}
			state, _ := classify(sum, f, man)
			if state == StateModified && !p.Options.replace(dst) {
				continue
			}
			if err := writeBundleFile(f, dst); err != nil {
				return nil, err
			}
		}
	}
	if opts.SkipGate {
		st.Gate = GateResult{Passed: true, Preflight: []string{}}
		return st, nil
	}
	installed := ""
	if target, err := filepath.EvalSymlinks(l.HooksDir()); err == nil && !strings.HasPrefix(target, st.RevDir) {
		installed = target
	}
	logPath := filepath.Join(l.LogDir(), "decisions.jsonl")
	if _, err := os.Stat(filepath.Join(l.LegacyLogDir(), "decisions.jsonl")); err == nil {
		logPath = filepath.Join(l.LegacyLogDir(), "decisions.jsonl")
	}
	st.Gate = RunGate(ctx, st.RevDir, installed, logPath)
	if !st.Gate.Passed {
		Abandon(st)
		return st, ErrGateFailed
	}
	return st, nil
}

// Abandon removes a staged directory that this run created.
func Abandon(st *Staged) {
	if st != nil && st.Created {
		os.RemoveAll(st.RevDir)
		st.Created = false
	}
}

// Commit performs the visible part of a setup plan.
func Commit(ctx context.Context, l Layout, p *Plan, st *Staged, opts ApplyOptions) (*ApplyResult, error) {
	if !st.Gate.Passed {
		return nil, ErrGateFailed
	}
	regs, err := LoadRegistrations()
	if err != nil {
		return nil, err
	}
	man, err := LoadManifest()
	if err != nil {
		return nil, err
	}
	unlock, err := lock(l)
	if err != nil {
		return nil, err
	}
	defer unlock()

	res := &ApplyResult{Log: []string{}}
	var undo []func()
	fail := func(err error) (*ApplyResult, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		Abandon(st)
		res.Log = append(res.Log, "rolled back: "+err.Error())
		return res, err
	}
	logf := func(format string, a ...any) { res.Log = append(res.Log, fmt.Sprintf(format, a...)) }
	prev, _ := LoadReceipt(l)
	prevFiles := map[string]string{}
	if prev != nil {
		prevFiles = prev.Files
	}
	rc := &Receipt{ConfigDir: l.ConfigDir, Rev: man.Rev, InstalledAt: opts.Now, LoomBuild: opts.Build,
		Files: map[string]string{}, Links: map[string]string{}, Kept: map[string]string{},
		PrevLinks: map[string]string{}, Restore: map[string]string{}}
	if prev != nil {
		rc.Settings = prev.Settings
		rc.ClaudeMD = prev.ClaudeMD
		rc.MigratedFrom, rc.ContextBarFrom, rc.LogsMovedFrom = prev.MigratedFrom, prev.ContextBarFrom, prev.LogsMovedFrom
		for k, v := range prev.Kept {
			rc.Kept[k] = v
		}
		for k, v := range prev.PrevLinks {
			rc.PrevLinks[k] = v
		}
		for k, v := range prev.Restore {
			rc.Restore[k] = v
		}
	}
	if rc.Settings.Env == nil {
		rc.Settings.Env = map[string]string{}
	}

	// Backups first.
	dir, err := Backup(l, "setup", opts.Now, l.SettingsPath(), l.ClaudeMD())
	if err != nil {
		return fail(err)
	}
	res.BackupDir = dir

	// 1. state dirs
	for _, d := range append([]string{l.StateDir, l.LogDir()}, stateDirs(l)...) {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fail(err)
		}
	}

	// 2. legacy install aside, logs under state
	aside := filepath.Join(l.DisciplineDir(), p.Stamp)
	if stepEnabledTarget(p, StepMigrate, l.HooksDir()) {
		dst := filepath.Join(aside, "enforce")
		if err := os.MkdirAll(aside, 0o755); err != nil {
			return fail(err)
		}
		if err := os.Rename(l.HooksDir(), dst); err != nil {
			return fail(err)
		}
		undo = append(undo, func() { os.Rename(dst, l.HooksDir()) })
		rc.MigratedFrom = dst
		logf("moved %s to %s", l.HooksDir(), dst)
	}
	if stepEnabledTarget(p, StepMigrate, l.ContextBar()) {
		dst := filepath.Join(aside, "scripts", "context-bar.sh")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fail(err)
		}
		if err := os.Rename(l.ContextBar(), dst); err != nil {
			return fail(err)
		}
		undo = append(undo, func() { os.Rename(dst, l.ContextBar()) })
		rc.ContextBarFrom = dst
		logf("moved %s to %s", l.ContextBar(), dst)
	}
	if stepEnabled(p, StepLogs) {
		src := l.LegacyLogDir() // the live hooks dir (a symlink into a revision on the rev layout)
		if stepEnabledTarget(p, StepMigrate, l.HooksDir()) {
			src = rc.MigratedFrom
		}
		for _, name := range []string{"decisions.jsonl", "allows.jsonl"} {
			from := filepath.Join(src, name)
			if _, err := os.Stat(from); err != nil {
				continue
			}
			if err := appendMove(from, filepath.Join(l.LogDir(), name)); err != nil {
				return fail(err)
			}
		}
		if rc.MigratedFrom != "" && src == rc.MigratedFrom {
			rc.LogsMovedFrom = src
		}
		logf("logs moved to %s", l.LogDir())
	}

	// 3. links
	if stepEnabled(p, StepLink) {
		links := []struct{ path, target string }{
			{l.CurrentLink(), man.Rev},
			{l.HooksDir(), filepath.Join("..", "discipline", "current", "enforce")},
			{l.ContextBar(), filepath.Join("..", "discipline", "current", "scripts", "context-bar.sh")},
		}
		for _, ln := range links {
			prevTarget, hadLink := readLink(ln.path)
			if fi, err := os.Lstat(ln.path); err == nil && fi.Mode()&os.ModeSymlink == 0 {
				return fail(fmt.Errorf("%s exists and is not a symlink", ln.path))
			}
			if err := replaceSymlink(ln.path, ln.target); err != nil {
				return fail(err)
			}
			path, target := ln.path, ln.target
			undo = append(undo, func() {
				if hadLink {
					replaceSymlink(path, prevTarget)
				} else {
					os.Remove(path)
				}
			})
			rc.Links[path] = target
			if hadLink && prev == nil && prevTarget != target {
				// Someone else's symlink (dotfiles, say): uninstall puts it back.
				rc.PrevLinks[path] = prevTarget
			}
		}
		logf("current → %s", man.Rev)
	} else if t, ok := readLink(l.CurrentLink()); ok {
		rc.Links[l.CurrentLink()] = t
		if t2, ok := readLink(l.HooksDir()); ok {
			rc.Links[l.HooksDir()] = t2
		}
		if t3, ok := readLink(l.ContextBar()); ok {
			rc.Links[l.ContextBar()] = t3
		}
	}

	// 4. deadline
	wroteDeadline := false
	if stepEnabled(p, StepDeadline) {
		wroteDeadline = true
		old, hadOld := readFileIf(l.Deadline())
		if hadOld {
			bdir, err := Backup(l, "deadline", opts.Now, l.Deadline())
			if err != nil {
				return fail(err)
			}
			if _, already := rc.Restore[l.Deadline()]; !already {
				rc.Restore[l.Deadline()] = filepath.Join(bdir, "deadline")
			}
		}
		for _, f := range man.Files {
			if f.Path == "bin/deadline" {
				if err := writeBundleFile(f, l.Deadline()); err != nil {
					return fail(err)
				}
			}
		}
		undo = append(undo, func() {
			if hadOld {
				os.WriteFile(l.Deadline(), old, 0o755)
			} else {
				os.Remove(l.Deadline())
			}
		})
		logf("wrote %s", l.Deadline())
	}
	for _, c := range p.Conflicts {
		real := mustEval(c.Path)
		if c.Choice == "keep" {
			rc.Kept[real] = sha256File(real)
		} else {
			delete(rc.Kept, real)
		}
	}

	// 5. settings.json: re-read now and merge again (another writer may
	// have changed it since the plan was shown).
	if stepEnabled(p, StepSettings) || stepEnabled(p, StepHooks) {
		doc, _, err := jsonedit.ReadFile(l.SettingsPath())
		if err != nil {
			return fail(err)
		}
		before, _ := os.ReadFile(l.SettingsPath())
		for _, h := range MergeHooks(doc, l, regs) {
			if h.Action == "duplicate" {
				return fail(fmt.Errorf("settings.json changed since the plan: hook %s is now duplicated", h.ID))
			}
			if h.Action == "added" && !contains(rc.Settings.HooksAdded, h.ID) {
				rc.Settings.HooksAdded = append(rc.Settings.HooksAdded, h.ID)
			}
		}
		if stepEnabled(p, StepStatusLine) {
			if w := MergeStatusLine(doc, l.StatusLineCommand(regs)); w != "" {
				rc.Settings.StatusLine = w
			}
		}
		if stepEnabled(p, StepEnv) {
			for k, v := range MergeEnv(doc, regs.Env) {
				rc.Settings.Env[k] = v
			}
		}
		if stepEnabled(p, StepDeny) {
			for _, e := range MergeDeny(doc, l.DenyEntries(regs)) {
				if !contains(rc.Settings.Deny, e) {
					rc.Settings.Deny = append(rc.Settings.Deny, e)
				}
			}
		}
		if err := jsonedit.WriteFile(l.SettingsPath(), doc); err != nil {
			return fail(err)
		}
		undo = append(undo, func() { restoreBytes(l.SettingsPath(), before) })
		logf("merged %s", l.SettingsPath())
	}

	// 6. CLAUDE.md
	if stepEnabled(p, StepClaudeMD) {
		before, _ := os.ReadFile(l.ClaudeMD())
		out, action := PlanClaudeMD(before, mustRead(rulesName))
		if action != ClaudeMDNone {
			if err := jsonedit.WriteBytes(l.ClaudeMD(), out); err != nil {
				return fail(err)
			}
			undo = append(undo, func() { restoreBytes(l.ClaudeMD(), before) })
			rc.ClaudeMD.ImportLineAdded = true
			rc.ClaudeMD.ReplacedInline = rc.ClaudeMD.ReplacedInline || action == ClaudeMDReplaceInline
			logf("CLAUDE.md: %s", action)
		}
	}

	// 7. verify
	for _, f := range man.Files {
		dsts := []string{filepath.Join(st.RevDir, filepath.FromSlash(f.Path))}
		if f.Target != "rev" {
			dsts = append(dsts, l.expand(f.Target))
		}
		for i, dst := range dsts {
			if _, kept := rc.Kept[mustEval(dst)]; kept {
				continue
			}
			sum := sha256File(dst)
			if sum != f.SHA256 {
				return fail(fmt.Errorf("verify: %s does not match the bundle after install", dst))
			}
			// The receipt lists what loom wrote: the revision dir always, a
			// file outside it only when this or an earlier setup wrote it.
			_, prevWrote := prevFiles[dst]
			if i == 0 || wroteDeadline || prevWrote {
				rc.Files[dst] = sum
			}
		}
	}
	if target, err := filepath.EvalSymlinks(l.HooksDir()); err != nil || !strings.HasPrefix(target, mustEval(st.RevDir)) {
		return fail(fmt.Errorf("verify: %s does not resolve into %s", l.HooksDir(), st.RevDir))
	}
	if doc, _, err := jsonedit.ReadFile(l.SettingsPath()); err == nil {
		for _, h := range MergeHooks(doc, l, regs) {
			if h.Action != "present" {
				return fail(fmt.Errorf("verify: registration %s is %s after the merge", h.ID, h.Action))
			}
		}
	}

	// 8. receipt and ledger
	if err := SaveReceipt(l, rc); err != nil {
		return fail(err)
	}
	res.Receipt = rc
	op := "setup"
	if prev != nil {
		op = "upgrade"
	}
	fromRev := p.FromRev
	entry := LedgerEntry{TS: opts.Now, Op: op, FromRev: fromRev, ToRev: man.Rev, Backup: dir,
		Gate: &GateSummary{Suites: st.Gate.Suites.Passed, NewAllows: len(st.Gate.Replay.NewAllows), NewDenies: len(st.Gate.Replay.NewDenies)}}
	if err := AppendLedger(l, entry); err != nil {
		logf("ledger not written: %v", err)
	}
	logf("installed %s", man.Rev)
	return res, nil
}

// ApplyUninstall reverses the receipt.
func ApplyUninstall(ctx context.Context, l Layout, p *Plan, rc *Receipt, opts ApplyOptions) (*ApplyResult, error) {
	if p.Op != "uninstall" || rc == nil {
		return nil, errors.New("uninstall: no plan or receipt")
	}
	if p.HasErrors() {
		return nil, fmt.Errorf("plan has errors: %s", strings.Join(p.Errors, "; "))
	}
	regs, err := LoadRegistrations()
	if err != nil {
		return nil, err
	}
	unlock, err := lock(l)
	if err != nil {
		return nil, err
	}
	defer unlock()
	res := &ApplyResult{Log: []string{}}
	logf := func(format string, a ...any) { res.Log = append(res.Log, fmt.Sprintf(format, a...)) }
	dir, err := Backup(l, "uninstall", opts.Now, l.SettingsPath(), l.ClaudeMD())
	if err != nil {
		return res, err
	}
	res.BackupDir = dir

	if stepEnabled(p, StepUnmerge) {
		doc, _, err := jsonedit.ReadFile(l.SettingsPath())
		if err != nil {
			return res, err
		}
		UnmergeHooks(doc, l, regs, rc.Settings.HooksAdded)
		UnmergeStatusLine(doc, rc.Settings.StatusLine)
		UnmergeEnv(doc, rc.Settings.Env)
		UnmergeDeny(doc, rc.Settings.Deny)
		if err := jsonedit.WriteFile(l.SettingsPath(), doc); err != nil {
			return res, err
		}
		logf("settings.json: loom's entries removed")
	}
	if stepEnabled(p, StepClaudeMDUndo) {
		cur, _ := os.ReadFile(l.ClaudeMD())
		out, changed := UndoClaudeMD(cur, revFile(l, rc.Rev, rulesName), rc.ClaudeMD.ReplacedInline)
		if changed {
			if err := jsonedit.WriteBytes(l.ClaudeMD(), out); err != nil {
				return res, err
			}
			logf("CLAUDE.md: import line removed")
		}
	}
	if stepEnabled(p, StepUnlink) {
		for _, path := range []string{l.HooksDir(), l.ContextBar(), l.CurrentLink()} {
			if want, ok := rc.Links[path]; ok {
				if got, isLink := readLink(path); isLink && got == want {
					os.Remove(path)
					if prevTarget, had := rc.PrevLinks[path]; had {
						if err := os.Symlink(prevTarget, path); err != nil {
							return res, err
						}
						logf("put back %s → %s", path, prevTarget)
					}
				}
			}
		}
		logf("symlinks removed")
	}
	if stepEnabled(p, StepRemoveRev) {
		revDir := l.RevDir(rc.Rev)
		for path, sha := range rc.Files {
			if strings.HasPrefix(path, revDir) && sha256File(path) == sha {
				os.Remove(path)
			}
		}
		os.Remove(filepath.Join(revDir, manifestName))
		removeEmptyDirs(revDir)
		logf("%s: files loom installed removed", revDir)
	}
	if stepEnabled(p, StepRemoveDL) {
		if from := rc.Restore[l.Deadline()]; from != "" {
			b, err := os.ReadFile(from)
			if err != nil {
				return res, fmt.Errorf("%s: the pre-loom deadline backup is gone: %w", l.Deadline(), err)
			}
			if err := jsonedit.WriteBytes(l.Deadline(), b); err != nil {
				return res, err
			}
			os.Chmod(l.Deadline(), 0o755)
			logf("put back the pre-loom %s", l.Deadline())
		} else {
			os.Remove(l.Deadline())
			logf("removed %s", l.Deadline())
		}
	}
	if stepEnabled(p, StepRestoreLegacy) {
		os.RemoveAll(l.HooksDir()) // a dangling symlink, or nothing
		if err := os.MkdirAll(filepath.Dir(l.HooksDir()), 0o755); err != nil {
			return res, err
		}
		if err := os.Rename(rc.MigratedFrom, l.HooksDir()); err != nil {
			return res, err
		}
		if rc.ContextBarFrom != "" {
			if _, err := os.Stat(rc.ContextBarFrom); err == nil {
				os.Remove(l.ContextBar())
				os.Rename(rc.ContextBarFrom, l.ContextBar())
			}
		}
		if rc.LogsMovedFrom != "" {
			// The legacy hooks log beside themselves; give them their history back.
			for _, name := range []string{"decisions.jsonl", "allows.jsonl"} {
				from := filepath.Join(l.LogDir(), name)
				if _, err := os.Stat(from); err == nil {
					if err := appendMove(from, filepath.Join(l.HooksDir(), name)); err != nil {
						return res, err
					}
				}
			}
			logf("logs moved back to %s", l.HooksDir())
		}
		removeEmptyDirs(filepath.Dir(rc.MigratedFrom))
		logf("restored %s from %s", l.HooksDir(), rc.MigratedFrom)
	}
	if err := RemoveReceipt(l); err != nil {
		return res, err
	}
	AppendLedger(l, LedgerEntry{TS: opts.Now, Op: "uninstall", FromRev: rc.Rev, Backup: dir})
	logf("uninstalled %s", rc.Rev)
	return res, nil
}

// --- helpers ---------------------------------------------------------------

func stateDirs(l Layout) []string {
	out := make([]string, 0, len(StateSubdirs))
	for _, s := range StateSubdirs {
		out = append(out, filepath.Join(l.StateDir, s))
	}
	return out
}

func stepEnabled(p *Plan, kind string) bool {
	for _, s := range p.Steps {
		if s.Kind == kind && s.Enabled {
			return true
		}
	}
	return false
}

func stepEnabledTarget(p *Plan, kind, target string) bool {
	for _, s := range p.Steps {
		if s.Kind == kind && s.Enabled && s.Target == target {
			return true
		}
	}
	return false
}

func stepDetailHas(p *Plan, kind, word string) bool {
	for _, s := range p.Steps {
		if s.Kind == kind && s.Enabled && strings.Contains(s.Detail, word) {
			return true
		}
	}
	return false
}

func writeBundleFile(f ManifestFile, dst string) error {
	mode := os.FileMode(0o644)
	if f.Mode == "0755" {
		mode = 0o755
	}
	return copyEmbedded(f.Path, dst, mode)
}

func copyEmbedded(src, dst string, mode os.FileMode) error {
	b, err := fs.ReadFile(Bundle(), src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := jsonedit.WriteBytes(dst, b); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func readLink(path string) (string, bool) {
	t, err := os.Readlink(path)
	return t, err == nil
}

// replaceSymlink creates path → target atomically (temp link + rename),
// so a session reading hooks/enforce never sees it missing.
func replaceSymlink(path, target string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func readFileIf(path string) ([]byte, bool) {
	b, err := os.ReadFile(path)
	return b, err == nil
}

func restoreBytes(path string, before []byte) {
	if before == nil {
		os.Remove(path)
		return
	}
	jsonedit.WriteBytes(path, before)
}

// appendMove appends src to dst (creating it) and removes src.
func appendMove(src, dst string) error {
	if _, err := os.Stat(dst); err != nil {
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.Rename(src, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

func removeEmptyDirs(root string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error { return nil })
	var dirs []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // fails, silently, when not empty
	}
}

func mustEval(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// lock keeps the GUI and the CLI from applying at the same time. A lock
// whose process is gone is taken over.
func lock(l Layout) (func(), error) {
	if err := os.MkdirAll(l.ReceiptDir(), 0o755); err != nil {
		return nil, err
	}
	path := l.LockPath()
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprint(f, os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		b, rerr := os.ReadFile(path)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if rerr == nil && pid > 0 && processAlive(pid) {
			return nil, fmt.Errorf("another loom setup is running (pid %d, lock %s)", pid, path)
		}
		os.Remove(path)
	}
	return nil, fmt.Errorf("could not take the lock at %s", path)
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscallSignalZero) == nil
}
