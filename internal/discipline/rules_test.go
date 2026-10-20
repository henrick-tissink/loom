package discipline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuleAdoptRenewDrop(t *testing.T) {
	home := t.TempDir()
	l := NewLayout(filepath.Join(home, ".claude"), home, "")
	must(t, os.MkdirAll(l.ConfigDir, 0o755))
	must(t, os.WriteFile(l.ClaudeMD(), []byte(ImportLine+"\n\n## Mine\nmy text\n"), 0o644))
	now := time.Date(2026, 10, 20, 11, 0, 0, 0, time.Local)
	adopt := RuleChange{Op: "adopt", Key: "tool/Write/aa", Text: "Read a file before you Write it.", Why: "Write failed, 21 times in 16 sessions.", Lapses: "2026-11-19"}

	p, err := PlanRule(l, adopt, now)
	must(t, err)
	if len(p.Files) != 2 || p.Files[0].Path != l.RulesPath() || p.Files[1].Path != l.ClaudeMD() || !strings.Contains(p.Files[1].Diff, "+"+RulesImportLine) {
		t.Fatalf("plan: %+v", p.Files)
	}
	if _, err := os.Stat(l.RulesPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("planning wrote a file")
	}

	// A file that moves after the review makes the apply refuse.
	must(t, os.WriteFile(l.ClaudeMD(), []byte(ImportLine+"\n\n## Mine\nmy text, edited\n"), 0o644))
	if _, err := ApplyRule(l, adopt, p.Token, now); !errors.Is(err, ErrStale) {
		t.Fatalf("stale apply: %v", err)
	}
	p, err = PlanRule(l, adopt, now)
	must(t, err)
	// Applied a minute after the review: what is written is what was shown.
	dir, err := ApplyRule(l, p.Change, p.Token, now.Add(time.Minute))
	must(t, err)
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); !strings.Contains(string(b), "edited") {
		t.Fatalf("CLAUDE.md not backed up in %s", dir)
	}
	md, _ := os.ReadFile(l.ClaudeMD())
	if string(md) != ImportLine+"\n\n## Mine\nmy text, edited\n\n"+RulesImportLine+"\n" {
		t.Fatalf("CLAUDE.md:\n%s", md)
	}
	rb, _ := os.ReadFile(l.RulesPath())
	if !strings.Contains(string(rb), "- Read a file before you Write it.\n  Why: Write failed, 21 times in 16 sessions. · since 2026-10-20 11:00 · lapses 2026-11-19 · key tool/Write/aa\n") {
		t.Fatalf("rules.md:\n%s", rb)
	}

	// A second rule leaves CLAUDE.md alone.
	second := RuleChange{Op: "adopt", Key: "hook/bash_guard/deny_detach", Text: "Never detach.", Why: "w", Lapses: "2026-11-19"}
	p, err = PlanRule(l, second, now)
	must(t, err)
	if len(p.Files) != 1 {
		t.Fatalf("second adopt touches %d files", len(p.Files))
	}
	_, err = ApplyRule(l, second, p.Token, now)
	must(t, err)

	renew := RuleChange{Op: "renew", Key: "tool/Write/aa", Lapses: "2027-01-31"}
	for _, bad := range []string{"2026-10-20", "2028-01-01", "x"} {
		if _, err := PlanRule(l, RuleChange{Op: "renew", Key: "tool/Write/aa", Lapses: bad}, now); err == nil {
			t.Errorf("renew to %s accepted", bad)
		}
	}
	p, err = PlanRule(l, renew, now)
	must(t, err)
	_, err = ApplyRule(l, renew, p.Token, now)
	must(t, err)

	drop := RuleChange{Op: "drop", Key: "tool/Write/aa"}
	p, err = PlanRule(l, drop, now)
	must(t, err)
	_, err = ApplyRule(l, drop, p.Token, now)
	must(t, err)
	rb, _ = os.ReadFile(l.RulesPath())
	if strings.Contains(string(rb), "tool/Write/aa") || !strings.Contains(string(rb), "key hook/bash_guard/deny_detach") {
		t.Fatalf("after drop:\n%s", rb)
	}
	if _, err := PlanRule(l, drop, now); err == nil {
		t.Fatal("dropped a rule twice")
	}

	if bk := ListBackups(l); len(bk) != 4 { // the stale apply wrote nothing
		t.Fatalf("each apply keeps its own backup: %v", bk)
	}

	led, _ := ReadLedger(l)
	var notes []string
	for _, e := range led {
		if e.Op == "rule" {
			notes = append(notes, e.Note)
		}
	}
	want := "adopt tool/Write/aa until 2026-11-19|adopt hook/bash_guard/deny_detach until 2026-11-19|renew tool/Write/aa until 2027-01-31|drop tool/Write/aa"
	if strings.Join(notes, "|") != want {
		t.Fatalf("ledger: %v", notes)
	}
}

func TestPruneKeepsSetupBackupsAndRestoreExact(t *testing.T) {
	home := t.TempDir()
	l := NewLayout(filepath.Join(home, ".claude"), home, "")
	f := filepath.Join(home, "f")
	must(t, os.WriteFile(f, []byte("orig"), 0o644))
	now := time.Date(2026, 10, 20, 11, 0, 0, 0, time.Local)
	setup, err := Backup(l, "setup", now, f)
	must(t, err)
	must(t, os.WriteFile(f, []byte("one"), 0o644))
	first, _ := Backup(l, "rule", now.Add(time.Hour), f)
	must(t, os.WriteFile(f, []byte("two"), 0o644))
	Backup(l, "rule", now.Add(time.Hour), f) // same second: <first>-2
	if _, err := RestoreBackup(l, filepath.Base(first), now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f); string(b) != "one" {
		t.Fatalf("restored %q, want the exact backup's \"one\"", b)
	}
	for i := 0; i < 25; i++ {
		Backup(l, "rule", now.Add(time.Duration(3+i)*time.Hour), f)
	}
	if _, err := os.Stat(setup); err != nil {
		t.Fatal("rule backups pruned the setup backup")
	}
}
