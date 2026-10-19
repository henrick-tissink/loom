package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/session"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

func TestControlFromPhone(t *testing.T) {
	cfg := t.TempDir()
	app := newApp(nil, tmux.New(), nil, &session.Launcher{ClaudeConfigDir: cfg}, nil, time.Now)
	os.MkdirAll(filepath.Join(cfg, "sessions"), 0o755)
	reg := fmt.Sprintf(`{"pid":%d,"sessionId":"s","tmux":"loom-p:@0.%%0","status":"idle","bridgeSessionId":"session_abc"}`, os.Getpid())
	os.WriteFile(filepath.Join(cfg, "sessions", fmt.Sprintf("%d.json", os.Getpid())), []byte(reg), 0o644)
	if err := app.ControlFromPhone("loom-p"); err == nil || !strings.Contains(err.Error(), "already under Remote Control: https://claude.ai/code/session_abc") {
		t.Fatalf("already remote: %v", err)
	}
	// a permission dialog open: the paste's Enter would answer it
	reg = fmt.Sprintf(`{"pid":%d,"sessionId":"s","tmux":"loom-p:@0.%%0","status":"waiting","waitingFor":"dialog open"}`, os.Getpid())
	os.WriteFile(filepath.Join(cfg, "sessions", fmt.Sprintf("%d.json", os.Getpid())), []byte(reg), 0o644)
	if err := app.ControlFromPhone("loom-p"); err == nil || !strings.Contains(err.Error(), "waiting on a dialog") {
		t.Fatalf("dialog open: %v", err)
	}

	rows := snapshotToDTOs(status.Snapshot{Live: []status.Row{
		{SessionRow: store.SessionRow{Name: "a"}, RemoteURL: "https://claude.ai/code/session_abc"},
	}}, nil)
	if rows[0].RemoteURL != "https://claude.ai/code/session_abc" {
		t.Fatalf("dto: %+v", rows[0])
	}
}
