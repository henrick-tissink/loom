package session

import (
	"reflect"
	"regexp"
	"testing"
)

func TestNewSessionIDAndTmuxName(t *testing.T) {
	id := NewSessionID()
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("not a lowercase uuid: %q", id)
	}
	name := TmuxName(id)
	if name != "loom-"+id {
		t.Fatalf("TmuxName = %q", name)
	}
	got, ok := SessionIDFromTmuxName(name)
	if !ok || got != id {
		t.Fatalf("SessionIDFromTmuxName = %q, %v", got, ok)
	}
	if _, ok := SessionIDFromTmuxName("notloom-x"); ok {
		t.Fatal("accepted non-loom name")
	}
}

func TestArgv(t *testing.T) {
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	// Every launch forces Claude's light theme so its TUI text is legible on
	// loom's light terminal (see recipe.go).
	base := []string{"claude", "--session-id", id, "--settings", `{"theme":"light"}`}
	cases := []struct {
		r    Recipe
		want []string
	}{
		{Recipe{}, base},
		{Recipe{Model: "opus"}, append(append([]string{}, base...), "--model", "opus")},
		{Recipe{Mode: "plan"}, append(append([]string{}, base...), "--permission-mode", "plan")},
		{Recipe{Model: "sonnet", Mode: "auto"},
			append(append([]string{}, base...), "--model", "sonnet", "--permission-mode", "auto")},
	}
	for _, c := range cases {
		if got := c.r.Argv(id); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Argv(%+v) = %v, want %v", c.r, got, c.want)
		}
	}
}

func TestShellCommand(t *testing.T) {
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	r := Recipe{Model: "opus", Mode: "plan"}
	settings := `'--settings' '{"theme":"light"}'`
	want := "'claude' '--session-id' '" + id + "' " + settings + " '--model' 'opus' '--permission-mode' 'plan'"
	if got := r.ShellCommand(id); got != want {
		t.Errorf("ShellCommand = %q, want %q", got, want)
	}
	if got := ResumeShellCommand(id); got != "'claude' '--resume' '"+id+"' "+settings {
		t.Errorf("ResumeShellCommand = %q", got)
	}
}
