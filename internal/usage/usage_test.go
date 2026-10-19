package usage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUntapRoundTrip(t *testing.T) {
	script := "/Users/a b/.loom/usage/statusline-tap.sh"
	for _, orig := range []string{"bash ~/.claude/scripts/context-bar.sh", `echo 'it''s' "x"`, ""} {
		got, ok := Untap(TapCommand(script, orig))
		if !ok || got != orig {
			t.Fatalf("Untap(TapCommand(%q)) = %q, %v", orig, got, ok)
		}
	}
	if _, ok := Untap("bash ~/.claude/scripts/context-bar.sh"); ok {
		t.Fatal("a plain command is not tapped")
	}
	if _, ok := Untap("sh '/x/other.sh' 'y'"); ok {
		t.Fatal("another script is not the tap")
	}
}

func TestSetTap(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	loom := filepath.Join(dir, "loom")
	os.WriteFile(settings, []byte(`{"model":"opus","statusLine":{"type":"command","command":"bash ~/bar.sh","padding":0}}`+"\n"), 0o644)
	now := time.Date(2026, 10, 19, 13, 0, 0, 0, time.UTC)
	if err := SetTap(settings, loom, true, now); err != nil {
		t.Fatal(err)
	}
	cmd, tapped, err := TapState(settings)
	if err != nil || !tapped {
		t.Fatalf("after on: %q %v %v", cmd, tapped, err)
	}
	if orig, _ := Untap(cmd); orig != "bash ~/bar.sh" {
		t.Fatalf("wrapped %q", orig)
	}
	b, _ := os.ReadFile(settings)
	if !strings.Contains(string(b), `"padding": 0`) || !strings.Contains(string(b), `"model": "opus"`) {
		t.Fatalf("other settings lost:\n%s", b)
	}
	if _, err := os.Stat(settings + ".bak-loom-usage-20261019130000"); err != nil {
		t.Fatalf("no backup: %v", err)
	}
	if _, err := os.Stat(TapScript(loom)); err != nil {
		t.Fatalf("no tap script: %v", err)
	}
	if err := SetTap(settings, loom, true, now); err != nil { // already on: no double wrap
		t.Fatal(err)
	}
	if c2, _, _ := TapState(settings); c2 != cmd {
		t.Fatalf("tapped twice: %q", c2)
	}
	if err := SetTap(settings, loom, false, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if cmd, tapped, _ := TapState(settings); tapped || cmd != "bash ~/bar.sh" {
		t.Fatalf("after off: %q %v", cmd, tapped)
	}
	// no status line before: on adds the tap alone, off removes it again
	os.WriteFile(settings, []byte(`{}`), 0o644)
	SetTap(settings, loom, true, now)
	SetTap(settings, loom, false, now)
	if b, _ := os.ReadFile(settings); strings.Contains(string(b), "statusLine") {
		t.Fatalf("status line left behind:\n%s", b)
	}
}

// The tap passes the input and output through unchanged and saves the input.
func TestTapScript(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "tap.sh")
	os.WriteFile(script, []byte(tapScript), 0o755)
	in := `{"rate_limits":{"five_hour":{"used_percentage":8,"resets_at":1792418400},"seven_day":{"used_percentage":20.5,"resets_at":1792760400},"spend_limit":{"used_usd":1}}}`
	cmd := exec.Command("sh", script, "tr a-z A-Z; exit 3")
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(in)
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Fatalf("exit status not passed through: %v", err)
	}
	if string(out) != strings.ToUpper(in) {
		t.Fatalf("output %q", out)
	}
	l, ok, err := ReadLimits(StatusFile(filepath.Join(home, ".loom")))
	if err != nil || !ok || len(l.Windows) != 2 || l.Windows[0].Name != "five_hour" || l.Windows[1].UsedPct != 20.5 {
		t.Fatalf("limits %+v %v %v", l, ok, err)
	}
	// a render without rate limits keeps the last capture
	cmd = exec.Command("sh", script, "cat")
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(`{"session_id":"x"}`)
	if out, err := cmd.Output(); err != nil || string(out) != `{"session_id":"x"}` {
		t.Fatalf("no-limits render: %q %v", out, err)
	}
	if l, ok, _ := ReadLimits(StatusFile(filepath.Join(home, ".loom"))); !ok || len(l.Windows) != 2 {
		t.Fatalf("capture lost: %+v %v", l, ok)
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	rec := func(at time.Time, id, model string, out int) string {
		return `{"type":"assistant","timestamp":"` + at.Format(time.RFC3339Nano) + `","cwd":"/r/a","message":{"id":"` + id + `","model":"` + model + `","usage":{"input_tokens":1,"output_tokens":` + itoa(out) + `,"cache_creation_input_tokens":2,"cache_read_input_tokens":100}}}` + "\n"
	}
	body := rec(now.Add(-time.Hour), "m1", "opus", 10) +
		rec(now.Add(-time.Hour), "m1", "opus", 10) + // the same message's second block
		rec(now.Add(-3*24*time.Hour), "m2", "sonnet", 5) +
		rec(now.Add(-time.Hour), "m3", "<synthetic>", 7) +
		`{"type":"user","message":{"content":"usage assistant"}}` + "\n"
	os.MkdirAll(filepath.Join(root, "p", "s", "subagents"), 0o755)
	os.WriteFile(filepath.Join(root, "p", "s.jsonl"), []byte(body), 0o644)
	os.WriteFile(filepath.Join(root, "p", "s", "subagents", "agent-1.jsonl"), []byte(rec(now.Add(-2*time.Hour), "m9", "haiku", 4)), 0o644)
	// a forked session's file repeats m1
	os.WriteFile(filepath.Join(root, "p", "fork.jsonl"), []byte(rec(now.Add(-time.Hour), "m1", "opus", 10)), 0o644)
	s := &Scanner{Root: root}
	ts, err := s.Scan(now, 5*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ts[0].All.Output != 14 || ts[0].ByModel["opus"].Output != 10 || ts[0].ByModel["haiku"].Output != 4 {
		t.Fatalf("5h: %+v", ts[0])
	}
	if ts[1].All.Output != 19 || ts[1].All.CacheRead != 300 || ts[1].ByProject["/r/a"].Output != 19 {
		t.Fatalf("7d: %+v", ts[1])
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
