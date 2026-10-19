package main

import (
	"slices"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

func TestBackgroundJobID(t *testing.T) {
	for name, want := range map[string]string{"bg-908c2218": "908c2218", "bg-": "", "loom-908c2218": "", "ext-x": ""} {
		id, ok := backgroundJobID(name)
		if id != want || ok != (want != "") {
			t.Errorf("backgroundJobID(%q) = %q, %v", name, id, ok)
		}
	}
}

func TestBackgroundAttachCmd(t *testing.T) {
	t.Setenv("TMUX", "/tmp/x,1,0")
	cmd := backgroundAttachCmd("908c2218")
	if got := cmd.Args; !slices.Equal(got, []string{"claude", "attach", "908c2218"}) {
		t.Fatalf("args = %v", got)
	}
	for _, e := range cmd.Env {
		if e == "TMUX=/tmp/x,1,0" {
			t.Fatal("TMUX must be stripped from the attach client's env")
		}
	}
}

func TestBackgroundKillStopsAndReplyRefuses(t *testing.T) {
	var stopped []string
	orig := stopBackground
	stopBackground = func(id string) error { stopped = append(stopped, id); return nil }
	t.Cleanup(func() { stopBackground = orig })
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if err := app.KillSession("bg-908c2218"); err != nil || !slices.Equal(stopped, []string{"908c2218"}) {
		t.Fatalf("kill → stop: err=%v stopped=%v", err, stopped)
	}
	if err := app.SendReply("bg-908c2218", "hi"); err == nil {
		t.Fatal("reply into a background session must be refused")
	}
}

func TestDTOCarriesBackground(t *testing.T) {
	snap := status.Snapshot{Live: []status.Row{
		{SessionRow: store.SessionRow{Name: "bg-908c2218", Cwd: "/w/b"}, Status: status.Idle, Background: true, JobID: "908c2218"},
	}}
	got := snapshotToDTOs(snap, nil)
	if len(got) != 1 || !got[0].Background || got[0].JobID != "908c2218" || got[0].External {
		t.Fatalf("dto = %+v", got)
	}
}

func TestAttachRoutesBackgroundToClaude(t *testing.T) {
	app := newApp(nil, tmux.New(), nil, nil, nil, time.Now)
	if got := app.reg.start("bg-908c2218").Args; !slices.Equal(got, []string{"claude", "attach", "908c2218"}) {
		t.Fatalf("bg row attach = %v", got)
	}
	if got := app.reg.start("loom-abc").Args; got[0] != "tmux" {
		t.Fatalf("loom row attach = %v", got)
	}
}
