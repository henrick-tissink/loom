package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
	"github.com/henricktissink/loom/internal/transcript"
)

func TestGoalPrompt(t *testing.T) {
	t.Setenv("CLAUDE_STATE_DIR", "")
	cfg := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "loom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := newApp(nil, tmux.New(), st, &session.Launcher{ClaudeConfigDir: cfg}, nil, time.Now)
	sid := "22222222-2222-2222-2222-222222222222"
	st.Upsert(store.SessionRow{Name: "loom-g", ClaudeSessionID: sid, Cwd: t.TempDir(), CreatedAt: 1, EndedAt: -1, LastStatus: "idle"})

	if _, err := app.GoalPrompt("loom-g"); err == nil || !strings.Contains(err.Error(), "no task file yet") {
		t.Fatalf("no task file: %v", err)
	}
	tf := filepath.Join(cfg, "state", "tasks", sid+".md")
	os.MkdirAll(filepath.Dir(tf), 0o755)
	body := "# Task\nStatus: open\n\n## Outcome\nX.\n\n## Done when\n- [x] build: $ go build ./...  # exit 0\n- [ ] live: the row shows it\n"
	os.WriteFile(tf, []byte(body), 0o644)
	p, err := app.GoalPrompt("loom-g")
	if err != nil || !strings.HasPrefix(p, "/goal every Done-when check of the task file "+tf+" holds,") ||
		!strings.Contains(p, "(1) build: $ go build ./...; (2) live: the row shows it; ") ||
		!strings.HasSuffix(p, "or stop after 8 turns") || strings.Contains(p, "exit 0") {
		t.Fatalf("prompt %q, %v", p, err)
	}
	os.WriteFile(tf, []byte(strings.Replace(body, "Status: open", "Status: done", 1)), 0o644)
	if _, err := app.GoalPrompt("loom-g"); err == nil || !strings.Contains(err.Error(), "Status: done") {
		t.Fatalf("closed task: %v", err)
	}

	if err := app.SendPrompt("loom-g", "/goal "+strings.Repeat("x", 800)); err == nil || !strings.Contains(err.Error(), "at most 800 characters") {
		t.Fatalf("an over-long /goal: %v", err)
	}

	rows := snapshotToDTOs(status.Snapshot{Live: []status.Row{
		{SessionRow: store.SessionRow{Name: "a"}, Goal: transcript.Goal{State: "active", Condition: "C", Checks: 2}},
		{SessionRow: store.SessionRow{Name: "b"}},
	}}, nil)
	if g := rows[0].Goal; g == nil || g.State != "active" || g.Checks != 2 || rows[1].Goal != nil {
		t.Fatalf("dto goal: %+v %+v", rows[0].Goal, rows[1].Goal)
	}
}
