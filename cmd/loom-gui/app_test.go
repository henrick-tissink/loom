package main

import (
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/tmux"
)

func TestApp_ListSessions_pollErrorReturnsEmpty(t *testing.T) {
	eng := status.NewEngine(tmux.New(), nil, t.TempDir())
	app := newApp(eng, tmux.New(), nil, nil, func() time.Time { return time.Unix(0, 0) })

	got := app.ListSessions()
	if got == nil {
		t.Fatal("ListSessions must never return nil (marshals to [])")
	}
	if len(got) != 0 {
		t.Fatalf("want 0 on poll error, got %d", len(got))
	}
}

func TestApp_CloseUnknownIsNoop(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, time.Now)
	app.CloseSession("does-not-exist") // must not panic
}

func TestApp_ListProjects_nonNil(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, time.Now)
	if app.ListProjects() == nil {
		t.Fatal("ListProjects must return non-nil (marshals to [])")
	}
}

func TestApp_LaunchSession_nilLauncherErrors(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, time.Now)
	if _, err := app.LaunchSession("/ws/loom", "", "", ""); err == nil {
		t.Fatal("LaunchSession with nil launcher must error")
	}
}
