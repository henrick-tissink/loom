package ccsessions

import (
	"context"
	"errors"
	"testing"
	"time"
)

// observed on Claude Code 2.1.303: a sleeping failed session (no pid), a
// running one, and an interactive one that is the registry's to report.
const agentsJSON = `[
 {"id":"08b74d36","cwd":"/w/web","kind":"background","startedAt":1783465411871,"sessionId":"08b74d36-ef64","name":"Investigate captcha","state":"failed"},
 {"pid":82496,"id":"908c2218","cwd":"/w/loom","kind":"background","startedAt":1792391044567,"sessionId":"908c2218-0914","name":"loom-probe","status":"busy","state":"working"},
 {"pid":42512,"cwd":"/w","kind":"interactive","startedAt":1790089000812,"sessionId":"24538944","name":"x","status":"idle"}
]`

func TestParseAgentsKeepsBackgroundOnly(t *testing.T) {
	got, ok := ParseAgents([]byte(agentsJSON))
	if !ok || len(got) != 2 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	if a := got[0]; a.ID != "08b74d36" || a.SessionID != "08b74d36-ef64" || a.Cwd != "/w/web" || a.State != "failed" || a.PID != 0 || a.Name != "Investigate captcha" || a.StartedAt != 1783465411871 {
		t.Fatalf("sleeping agent = %+v", a)
	}
	if a := got[1]; a.ID != "908c2218" || a.PID != 82496 || a.State != "working" {
		t.Fatalf("running agent = %+v", a)
	}
	if _, ok := ParseAgents([]byte(`{"oops"`)); ok {
		t.Fatal("malformed → not ok")
	}
	if got, ok := ParseAgents([]byte(`[]`)); !ok || len(got) != 0 {
		t.Fatalf("empty array → ok and empty, got %+v %v", got, ok)
	}
}

func TestAgentsCacheThrottlesAndKeepsLastGood(t *testing.T) {
	runs := 0
	out, fail := []byte(agentsJSON), false
	c := &AgentsCache{Every: 10 * time.Second, Run: func(context.Context) ([]byte, error) {
		runs++
		if fail {
			return nil, errors.New("claude missing")
		}
		return out, nil
	}}
	t0 := time.Unix(1000, 0)
	if got := c.List(t0); len(got) != 2 || runs != 1 {
		t.Fatalf("first list: %d agents, %d runs", len(got), runs)
	}
	if c.List(t0.Add(5 * time.Second)); runs != 1 {
		t.Fatalf("within Every: want no new run, got %d", runs)
	}
	fail = true
	if got := c.List(t0.Add(11 * time.Second)); len(got) != 2 || runs != 2 {
		t.Fatalf("failed run keeps the last list: %d agents, %d runs", len(got), runs)
	}
	fail, out = false, []byte(`[]`)
	if got := c.List(t0.Add(22 * time.Second)); len(got) != 0 {
		t.Fatalf("a good empty answer replaces the list, got %+v", got)
	}
}
