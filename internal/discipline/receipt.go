package discipline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/jsonedit"
)

// Receipt records exactly what loom wrote on this machine, so upgrade can
// replace only what still matches and uninstall can undo only that
// (decision D6). It lives under ~/.loom, not in settings.json.
type Receipt struct {
	ConfigDir   string            `json:"configDir"`
	Rev         string            `json:"rev"`
	InstalledAt time.Time         `json:"installedAt"`
	LoomBuild   string            `json:"loomBuild,omitempty"`
	Files       map[string]string `json:"files"` // installed path -> sha256
	Links       map[string]string `json:"links"` // link path -> target
	Settings    ReceiptSettings   `json:"settings"`
	ClaudeMD    ReceiptClaudeMD   `json:"claudeMd"`
	// MigratedFrom is where a legacy hooks/enforce directory was moved
	// (discipline/local-<ts>); uninstall moves it back.
	MigratedFrom string `json:"migratedFrom,omitempty"`
	// ContextBarFrom is where a pre-existing scripts/context-bar.sh file went.
	ContextBarFrom string `json:"contextBarFrom,omitempty"`
	LogsMovedFrom  string `json:"logsMovedFrom,omitempty"`
	// Kept lists files the person chose to keep over the bundle's copy
	// (real paths, symlinks resolved).
	Kept map[string]string `json:"kept,omitempty"`
	// PrevLinks are symlinks that existed before the first install and
	// were replaced by loom's; uninstall puts them back.
	PrevLinks map[string]string `json:"prevLinks,omitempty"`
	// Restore maps a file loom overwrote outside the revision dir to the
	// backup copy uninstall must put back instead of removing the file.
	Restore map[string]string `json:"restore,omitempty"`
}

type ReceiptSettings struct {
	HooksAdded []string          `json:"hooksAdded"` // HookIDs loom added (not the ones adopted)
	StatusLine string            `json:"statusLine,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Deny       []string          `json:"permissionsDeny,omitempty"`
}

type ReceiptClaudeMD struct {
	ImportLineAdded bool `json:"importLineAdded"`
	ReplacedInline  bool `json:"replacedInline"`
}

// LedgerEntry is one line of ~/.loom/discipline/ledger.jsonl: the change
// marks the reports split on.
type LedgerEntry struct {
	TS      time.Time         `json:"ts"`
	Op      string            `json:"op"` // setup | upgrade | uninstall | reset | keep | restore | rule
	FromRev string            `json:"fromRev,omitempty"`
	ToRev   string            `json:"toRev,omitempty"`
	Backup  string            `json:"backup,omitempty"`
	Gate    *GateSummary      `json:"gate,omitempty"`
	Files   map[string]string `json:"files,omitempty"`
	Note    string            `json:"note,omitempty"`
}

type GateSummary struct {
	Suites    bool `json:"suitesPassed"`
	NewAllows int  `json:"newAllows"`
	NewDenies int  `json:"newDenies"`
}

func (l Layout) LoomDir() string     { return filepath.Join(l.Home, ".loom") }
func (l Layout) ReceiptDir() string  { return filepath.Join(l.LoomDir(), "discipline") }
func (l Layout) ReceiptPath() string { return filepath.Join(l.ReceiptDir(), "receipt.json") }
func (l Layout) LedgerPath() string  { return filepath.Join(l.ReceiptDir(), "ledger.jsonl") }
func (l Layout) LockPath() string    { return filepath.Join(l.ReceiptDir(), "lock") }
func (l Layout) BackupsDir() string  { return filepath.Join(l.LoomDir(), "backups") }

// LoadReceipt returns nil, nil when none exists.
func LoadReceipt(l Layout) (*Receipt, error) {
	b, err := os.ReadFile(l.ReceiptPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Receipt
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func SaveReceipt(l Layout, r *Receipt) error {
	if err := os.MkdirAll(l.ReceiptDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return jsonedit.WriteBytes(l.ReceiptPath(), append(b, '\n'))
}

func RemoveReceipt(l Layout) error {
	err := os.Remove(l.ReceiptPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func AppendLedger(l Layout, e LedgerEntry) error {
	if err := os.MkdirAll(l.ReceiptDir(), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.LedgerPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// ReadLedger returns all entries, oldest first.
func ReadLedger(l Layout) ([]LedgerEntry, error) {
	b, err := os.ReadFile(l.LedgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []LedgerEntry
	for _, line := range splitLines(string(b)) {
		var e LedgerEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// Backup copies the given files (those that exist) into a new directory
// under ~/.loom/backups/<ts>-<op>/ and prunes the directory to the newest
// twenty. Returns the directory ("" when nothing existed to back up).
func Backup(l Layout, op string, now time.Time, paths ...string) (string, error) {
	dir := filepath.Join(l.BackupsDir(), now.Format("20060102-150405")+"-"+op)
	// Two writes in one second (two rule changes) each keep their own copy.
	for i, base := 2, dir; ; i++ {
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			break
		}
		dir = fmt.Sprintf("%s-%d", base, i)
	}
	made := false
	origins := map[string]string{} // backup name → original path
	for _, p := range paths {
		src, err := os.Open(p)
		if err != nil {
			continue
		}
		if !made {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				src.Close()
				return "", err
			}
			made = true
		}
		name := filepath.Base(p)
		for i := 2; origins[name] != ""; i++ {
			name = fmt.Sprintf("%s.%d", filepath.Base(p), i)
		}
		origins[name] = p
		dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			src.Close()
			return "", err
		}
		_, err = io.Copy(dst, src)
		src.Close()
		dst.Close()
		if err != nil {
			return "", err
		}
	}
	if !made {
		return "", nil
	}
	ob, _ := json.MarshalIndent(origins, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, originsName), ob, 0o600); err != nil {
		return "", err
	}
	pruneBackups(l.BackupsDir(), 20)
	return dir, nil
}

const originsName = "origins.json"

// ListBackups returns the backup directory names, oldest first.
func ListBackups(l Layout) []string {
	entries, err := os.ReadDir(l.BackupsDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// RestoreBackup copies every file of a backup back to where it came from
// (the current copies are backed up first) and returns the paths written.
// name is the directory name or its timestamp prefix.
func RestoreBackup(l Layout, name string, now time.Time) ([]string, error) {
	var dir string
	for _, n := range ListBackups(l) {
		if n == name { // an exact name wins over a longer one it prefixes
			dir = filepath.Join(l.BackupsDir(), n)
			break
		}
		if strings.HasPrefix(n, name+"-") || strings.HasPrefix(n, name) {
			dir = filepath.Join(l.BackupsDir(), n)
		}
	}
	if dir == "" {
		return nil, fmt.Errorf("no backup named %s under %s", name, l.BackupsDir())
	}
	ob, err := os.ReadFile(filepath.Join(dir, originsName))
	if err != nil {
		return nil, fmt.Errorf("%s has no %s; restore its files by hand", dir, originsName)
	}
	origins := map[string]string{}
	if err := json.Unmarshal(ob, &origins); err != nil {
		return nil, err
	}
	var targets []string
	for _, p := range origins {
		targets = append(targets, p)
	}
	sort.Strings(targets)
	if _, err := Backup(l, "restore", now, targets...); err != nil {
		return nil, err
	}
	var done []string
	for bname, p := range origins {
		b, err := os.ReadFile(filepath.Join(dir, bname))
		if err != nil {
			return done, err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return done, err
		}
		if err := jsonedit.WriteBytes(p, b); err != nil {
			return done, err
		}
		done = append(done, p)
	}
	sort.Strings(done)
	files := map[string]string{}
	for _, p := range done {
		files[p] = sha256File(p)
	}
	AppendLedger(l, LedgerEntry{TS: now, Op: "restore", Note: filepath.Base(dir), Files: files})
	return done, nil
}

func pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// Rule changes are kept apart from the rest, so a run of them never
	// prunes the backup a setup made of the person's original files.
	var rule, other []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if ruleBackupRe.MatchString(e.Name()) {
			rule = append(rule, e.Name())
		} else {
			other = append(other, e.Name())
		}
	}
	for _, names := range [][]string{rule, other} {
		sort.Strings(names)
		for len(names) > keep {
			os.RemoveAll(filepath.Join(dir, names[0]))
			names = names[1:]
		}
	}
}

var ruleBackupRe = regexp.MustCompile(`^\d{8}-\d{6}-rule(-\d+)?$`)
