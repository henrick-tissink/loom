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
	cases := []struct {
		r    Recipe
		want []string
	}{
		{Recipe{}, []string{"claude", "--session-id", id}},
		{Recipe{Model: "opus"}, []string{"claude", "--session-id", id, "--model", "opus"}},
		{Recipe{Mode: "plan"}, []string{"claude", "--session-id", id, "--permission-mode", "plan"}},
		{Recipe{Model: "sonnet", Mode: "auto"},
			[]string{"claude", "--session-id", id, "--model", "sonnet", "--permission-mode", "auto"}},
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
	want := "'claude' '--session-id' '" + id + "' '--model' 'opus' '--permission-mode' 'plan'"
	if got := r.ShellCommand(id); got != want {
		t.Errorf("ShellCommand = %q, want %q", got, want)
	}
	if got := ResumeShellCommand(id); got != "'claude' '--resume' '"+id+"'" {
		t.Errorf("ResumeShellCommand = %q", got)
	}
}
