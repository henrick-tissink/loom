package main

import (
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/transcript"
)

func kinds(ds []DriftDTO) string {
	var ks []string
	for _, d := range ds {
		ks = append(ks, d.Kind)
	}
	return strings.Join(ks, ",")
}

func TestDriftSignals(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	quiet := transcript.TurnStats{Start: now.Add(-10 * time.Minute).Unix(), Calls: 40, TestFails: 2, OutTokens: 20_000, Agrees: 1}
	if ds := driftSignals(quiet, "running", false, now); len(ds) != 0 {
		t.Fatalf("an ordinary turn drifts: %+v", ds)
	}
	loud := transcript.TurnStats{Start: now.Add(-50 * time.Minute).Unix(), Calls: 151, TestFails: 3, LastTest: "go test ./...",
		OutTokens: 160_000, Agrees: 2, AgreeSample: "You're absolutely right."}
	ds := driftSignals(loud, "running", false, now)
	if kinds(ds) != "tests,long,calls,tokens,agree" {
		t.Fatalf("kinds = %s", kinds(ds))
	}
	if ds[1].Label != "turn 50m" || ds[0].Label != "tests failing ×3" || ds[3].Label != "160k tokens out" {
		t.Fatalf("labels = %+v", ds)
	}
	// a turn that handed back keeps only the agreement signal
	if k := kinds(driftSignals(loud, "needs_you", false, now)); k != "agree" {
		t.Fatalf("needs_you: %s", k)
	}
	// a spin owns repeated test failures
	if k := kinds(driftSignals(loud, "running", true, now)); k != "long,calls,tokens,agree" {
		t.Fatalf("spinning: %s", k)
	}
	// unknown start: no long-turn signal
	loud.Start = 0
	if k := kinds(driftSignals(loud, "running", true, now)); strings.Contains(k, "long") {
		t.Fatalf("long without a start: %s", k)
	}
}

func TestNudgePromptForDrift(t *testing.T) {
	a := &App{}
	now := time.Unix(1_800_000_000, 0)
	ds := driftSignals(transcript.TurnStats{Start: now.Add(-2 * time.Hour).Unix(), Agrees: 3, AgreeSample: "Good catch."}, "running", false, now)
	a.noteRail([]SessionDTO{{Name: "loom-d", Drift: ds}, {Name: "loom-q"}}, nil)
	p, err := a.NudgePrompt("loom-d")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"run for 2h 0m", "opened with agreement 3 times", "\"Good catch.\"", "with the evidence", "the check that will show it worked", "agreed with a correction", "say so and stop"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(strings.ToLower(p), "keep going") {
		t.Fatalf("prompt says keep going:\n%s", p)
	}
	if strings.Contains(driftPrompt(ds[:1]), "agreed with a correction") {
		t.Fatal("agreement question without an agreement signal")
	}
	if _, err := a.NudgePrompt("loom-q"); err == nil {
		t.Fatal("a nudge for a quiet session")
	}
}
