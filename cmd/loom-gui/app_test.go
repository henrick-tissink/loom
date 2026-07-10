package main

import (
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/tmux"
)

func TestApp_ListSessions_pollErrorReturnsEmpty(t *testing.T) {
	eng := status.NewEngine(tmux.New(), nil, t.TempDir())
	app := newApp(eng, tmux.New(), nil, nil, nil, func() time.Time { return time.Unix(0, 0) })

	got := app.ListSessions()
	if got == nil {
		t.Fatal("ListSessions must never return nil (marshals to [])")
	}
	if len(got) != 0 {
		t.Fatalf("want 0 on poll error, got %d", len(got))
	}
}

func TestApp_CloseUnknownIsNoop(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	app.CloseSession("does-not-exist") // must not panic
}

func TestApp_ListProjects_nonNil(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if app.ListProjects() == nil {
		t.Fatal("ListProjects must return non-nil (marshals to [])")
	}
}

func TestApp_LaunchSession_nilLauncherErrors(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if _, err := app.LaunchSession("/ws/loom", "", "", ""); err == nil {
		t.Fatal("LaunchSession with nil launcher must error")
	}
}

func TestApp_ListRecent_nilStoreNonNil(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if app.ListRecent() == nil {
		t.Fatal("ListRecent must return non-nil (marshals to [])")
	}
}

func TestApp_KillSession_nilTmErrors(t *testing.T) {
	app := newApp(nil, nil, nil, nil, nil, time.Now)
	if err := app.KillSession("x"); err == nil {
		t.Fatal("KillSession with nil tmux must error")
	}
}

func TestApp_AttachSession_nilTmErrors(t *testing.T) {
	app := newApp(nil, nil, nil, nil, nil, time.Now)
	if err := app.AttachSession("x"); err == nil {
		t.Fatal("AttachSession with nil tmux must error (no panic)")
	}
}

func TestApp_DismissSession_nilStoreErrors(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if err := app.DismissSession("x"); err == nil {
		t.Fatal("DismissSession with nil store must error")
	}
}

func TestApp_ResumeSession_nilErrors(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if _, err := app.ResumeSession("x"); err == nil {
		t.Fatal("ResumeSession with nil launcher/store must error")
	}
}
