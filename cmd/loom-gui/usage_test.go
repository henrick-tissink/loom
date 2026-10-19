package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/tmux"
)

func TestUsage(t *testing.T) {
	cfg, loom := t.TempDir(), t.TempDir()
	now := time.Now()
	proj := filepath.Join(cfg, "projects", "p")
	os.MkdirAll(proj, 0o755)
	rec := `{"type":"assistant","timestamp":"` + now.Add(-time.Hour).UTC().Format(time.RFC3339) + `","cwd":"/r/app/.claude/worktrees/loom-1","message":{"id":"m1","model":"opus","usage":{"input_tokens":1,"output_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}}` + "\n"
	os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(rec), 0o644)
	os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"statusLine":{"type":"command","command":"bash bar.sh"}}`), 0o644)
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	app.usage = newUsageState(cfg, loom, time.Now)

	d := app.Usage()
	if d.TapOn || d.StatusLine != "bash bar.sh" || d.CapturedAt != 0 || d.Err != "" {
		t.Fatalf("before the tap: %+v", d)
	}
	// the scan runs in the background; its answer shows on a later call
	end := time.Now().Add(5 * time.Second)
	for d.FiveHour == nil && time.Now().Before(end) {
		time.Sleep(20 * time.Millisecond)
		d = app.Usage()
	}
	if d.FiveHour == nil || d.FiveHour.All.Output != 10 || len(d.FiveHour.ByProject) != 1 || d.FiveHour.ByProject[0].Name != "app" {
		t.Fatalf("tokens (a worktree folded into its repo): %+v", d.FiveHour)
	}
	if err := app.SetUsageTap(true); err != nil {
		t.Fatal(err)
	}
	if d = app.Usage(); !d.TapOn || d.StatusLine != "bash bar.sh" {
		t.Fatalf("after the tap: %+v", d)
	}
	os.MkdirAll(filepath.Join(loom, "usage"), 0o755)
	os.WriteFile(filepath.Join(loom, "usage", "statusline.json"), []byte(`{"rate_limits":{"five_hour":{"used_percentage":8,"resets_at":1792418400}}}`), 0o644)
	if d = app.Usage(); len(d.Windows) != 1 || d.Windows[0].UsedPct != 8 || d.CapturedAt == 0 {
		t.Fatalf("limits: %+v", d)
	}
}
