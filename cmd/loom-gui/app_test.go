package main

import (
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/tmux"
)

func TestApp_ListSessions_pollErrorReturnsEmpty(t *testing.T) {
	// A store-less engine: Poll will error/panic; ListSessions must degrade to [].
	eng := status.NewEngine(tmux.New(), nil, t.TempDir())
	app := newApp(eng, tmux.New(), func() time.Time { return time.Unix(0, 0) })

	got := app.ListSessions()
	if got == nil {
		t.Fatal("ListSessions must never return nil (marshals to [])")
	}
	if len(got) != 0 {
		t.Fatalf("want 0 on poll error, got %d", len(got))
	}
}

func TestApp_CloseUnknownIsNoop(t *testing.T) {
	app := newApp(nil, tmux.New(), time.Now)
	app.CloseSession("does-not-exist") // must not panic
}
