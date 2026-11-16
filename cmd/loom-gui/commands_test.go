package main

import (
	"strings"
	"testing"
)

func TestCommandRefusal(t *testing.T) {
	for _, c := range []struct {
		cmd, status, dialog string
		bg, ext             bool
		want                string // "" = typed
	}{
		{"/recap", "needs_you", "", false, false, ""},
		{"/code-review", "idle", "", false, false, ""},
		{"/rewind", "idle", "", false, false, ""},
		{"/goal clear", "needs_you", "", false, false, ""},
		{"/clear", "idle", "", false, false, "not one of the commands"},
		{"/recap now", "idle", "", false, false, "not one of the commands"},
		{"rm -rf /", "idle", "", false, false, "not one of the commands"},
		{"/recap", "running", "", false, false, "mid-turn"},
		{"/recap", "done", "", false, false, "not live"},
		{"/recap", "needs_you", "dialog open", false, false, "waiting on a dialog"},
		{"/recap", "idle", "", true, false, "background"},
		{"/recap", "idle", "", false, true, "didn't start"},
	} {
		got := commandRefusal(c.cmd, c.status, c.dialog, c.bg, c.ext)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%q %s %q bg=%v ext=%v: got %q, want %q", c.cmd, c.status, c.dialog, c.bg, c.ext, got, c.want)
		}
	}
}

func TestSendCommandWithoutTmux(t *testing.T) {
	if err := (&App{}).SendCommand("x", "/recap"); err == nil {
		t.Fatal("no tmux: no error")
	}
}
