package discipline

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Local edits to installed files (handbook: "the hooks are yours to
// change"). `loom discipline diff|keep|reset` work on one bundle file at
// a time; keep records the edit in the receipt so upgrades leave it alone
// and the doctor stops reporting it, reset puts the bundle's copy back.

// ResolveFile maps what a person typed (a bundle path such as
// enforce/bash_guard.py, a bare file name, or the installed absolute path)
// to the manifest entry and the installed path.
func ResolveFile(l Layout, kind string, arg string) (ManifestFile, string, error) {
	man, err := LoadManifest()
	if err != nil {
		return ManifestFile{}, "", err
	}
	arg = strings.TrimSpace(arg)
	abs := ""
	if filepath.IsAbs(arg) {
		abs = filepath.Clean(arg)
	}
	for _, f := range man.Files {
		installed, ok := l.InstalledPath(f, kind)
		if !ok {
			continue
		}
		switch {
		case abs != "" && (abs == installed || abs == mustEval(installed)):
			return f, installed, nil
		case abs == "" && (f.Path == filepath.ToSlash(arg) || filepath.Base(f.Path) == arg):
			return f, installed, nil
		}
	}
	return ManifestFile{}, "", fmt.Errorf("%s is not a bundle file (loom doctor lists them)", arg)
}

// FileDiff is the unified diff from the bundle's copy to the installed one.
func FileDiff(f ManifestFile, installed string) (string, error) {
	want, err := fs.ReadFile(Bundle(), f.Path)
	if err != nil {
		return "", err
	}
	have, err := os.ReadFile(installed)
	if err != nil {
		return "", err
	}
	d := UnifiedDiff(f.Path, string(want), string(have))
	if d == "" {
		return "", nil
	}
	return d, nil
}

// Keep records the installed file's current content as the person's.
func Keep(l Layout, f ManifestFile, installed string, now time.Time) error {
	rc, err := LoadReceipt(l)
	if err != nil {
		return err
	}
	if rc == nil {
		return fmt.Errorf("no receipt at %s: run loom setup first", l.ReceiptPath())
	}
	installed = mustEval(installed)
	sum := sha256File(installed)
	if sum == "" {
		return fmt.Errorf("%s does not exist", installed)
	}
	if sum == f.SHA256 {
		return fmt.Errorf("%s matches the bundle; nothing to keep", installed)
	}
	if rc.Kept == nil {
		rc.Kept = map[string]string{}
	}
	rc.Kept[installed] = sum
	if err := SaveReceipt(l, rc); err != nil {
		return err
	}
	return AppendLedger(l, LedgerEntry{TS: now, Op: "keep", Files: map[string]string{installed: sum}})
}

// Reset puts the bundle's copy back (the installed one is backed up).
func Reset(l Layout, f ManifestFile, installed string, now time.Time) (string, error) {
	rc, err := LoadReceipt(l)
	if err != nil {
		return "", err
	}
	installed = mustEval(installed)
	if sha256File(installed) == f.SHA256 {
		return "", fmt.Errorf("%s already matches the bundle", installed)
	}
	dir, err := Backup(l, "reset", now, installed)
	if err != nil {
		return "", err
	}
	if err := writeBundleFile(f, installed); err != nil {
		return dir, err
	}
	if rc != nil {
		delete(rc.Kept, installed)
		if rc.Files == nil {
			rc.Files = map[string]string{}
		}
		rc.Files[installed] = f.SHA256
		if err := SaveReceipt(l, rc); err != nil {
			return dir, err
		}
	}
	return dir, AppendLedger(l, LedgerEntry{TS: now, Op: "reset", Backup: dir, Files: map[string]string{installed: f.SHA256}})
}
