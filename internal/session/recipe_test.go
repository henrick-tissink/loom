package session

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestSetClaudeTheme(t *testing.T) {
	defer SetClaudeTheme("light") // restore the default for the other tests
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	argvStr := func() string { return strings.Join(Recipe{}.Argv(id), " ") }

	SetClaudeTheme("dark")
	if !strings.Contains(argvStr(), `{"theme":"dark"}`) {
		t.Errorf("dark theme not injected: %s", argvStr())
	}
	SetClaudeTheme("light")
	if !strings.Contains(argvStr(), `{"theme":"light"}`) {
		t.Errorf("light theme not injected: %s", argvStr())
	}
	SetClaudeTheme("weird") // unknown → light
	if !strings.Contains(argvStr(), `{"theme":"light"}`) {
		t.Errorf("unknown theme should fall back to light: %s", argvStr())
	}
}

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
	if got := ResumeShellCommand(id, nil); got != "'claude' '--resume' '"+id+"' "+settings {
		t.Errorf("ResumeShellCommand = %q", got)
	}
}

// TestArgvAddDirs pins spec §5's flag shape: one --add-dir per directory,
// appended after --model/--permission-mode so pre-multi-repo argvs are
// byte-identical (TestArgv above is the untouched proof of that).
func TestArgvAddDirs(t *testing.T) {
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	base := []string{"claude", "--session-id", id, "--settings", `{"theme":"light"}`}
	with := func(rest ...string) []string { return append(append([]string{}, base...), rest...) }
	cases := []struct {
		name string
		r    Recipe
		want []string
	}{
		{"nil AddDirs unchanged", Recipe{}, base},
		{"empty AddDirs unchanged", Recipe{AddDirs: []string{}}, base},
		{"one dir", Recipe{AddDirs: []string{"/w/ballista"}}, with("--add-dir", "/w/ballista")},
		{"order preserved", Recipe{AddDirs: []string{"/w/b", "/w/a"}},
			with("--add-dir", "/w/b", "--add-dir", "/w/a")},
		{"after model and mode", Recipe{Model: "opus", Mode: "plan", AddDirs: []string{"/w/b"}},
			with("--model", "opus", "--permission-mode", "plan", "--add-dir", "/w/b")},
		{"empty entries skipped", Recipe{AddDirs: []string{"", "/w/b", ""}}, with("--add-dir", "/w/b")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.r.Argv(id); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Argv = %v, want %v", got, c.want)
			}
		})
	}
}

// A path with a space or a quote must survive the shell round-trip intact —
// project directories under "~/My Repos" are ordinary, and a broken quote
// would silently split one --add-dir into two bogus arguments.
func TestShellCommandAddDirsQuoting(t *testing.T) {
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	settings := `'--settings' '{"theme":"light"}'`
	r := Recipe{AddDirs: []string{"/w/My Repos/a", "/w/it's"}}
	want := "'claude' '--session-id' '" + id + "' " + settings +
		` '--add-dir' '/w/My Repos/a' '--add-dir' '/w/it'\''s'`
	if got := r.ShellCommand(id); got != want {
		t.Errorf("ShellCommand = %q,\n            want %q", got, want)
	}
	rwant := "'claude' '--resume' '" + id + "' " + settings + ` '--add-dir' '/w/My Repos/a'`
	if got := ResumeShellCommand(id, []string{"/w/My Repos/a"}); got != rwant {
		t.Errorf("ResumeShellCommand = %q, want %q", got, rwant)
	}
	if got := ResumeShellCommand(id, nil); got != "'claude' '--resume' '"+id+"' "+settings {
		t.Errorf("ResumeShellCommand(nil dirs) = %q, want the pre-multi-repo shape", got)
	}
}

func TestEncodeDecodeAddDirs(t *testing.T) {
	cases := []struct {
		name    string
		dirs    []string
		encoded string
		decoded []string
	}{
		{"none", nil, "", nil},
		{"empty slice encodes as the v8 default", []string{}, "", nil},
		{"two", []string{"/w/a", "/w/b"}, `["/w/a","/w/b"]`, []string{"/w/a", "/w/b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EncodeAddDirs(c.dirs); got != c.encoded {
				t.Fatalf("EncodeAddDirs = %q, want %q", got, c.encoded)
			}
			if got := DecodeAddDirs(c.encoded); !reflect.DeepEqual(got, c.decoded) {
				t.Fatalf("DecodeAddDirs(%q) = %v, want %v", c.encoded, got, c.decoded)
			}
		})
	}
	// A corrupt column must degrade to single-repo, never block a resume.
	for _, bad := range []string{"not json", `{"a":1}`, `[`, "   "} {
		if got := DecodeAddDirs(bad); got != nil {
			t.Errorf("DecodeAddDirs(%q) = %v, want nil", bad, got)
		}
	}
}
