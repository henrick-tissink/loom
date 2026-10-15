package jsonedit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const doc = `{
  "model": "opus[1m]",
  "env": {
    "PATH": "/a:/b",
    "X": "1"
  },
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/x/lava hook",
            "async": true,
            "timeout": 6
          }
        ]
      }
    ]
  },
  "permissions": {
    "defaultMode": "auto",
    "deny": []
  },
  "n": 1.5e3,
  "u": "caf\u00e9",
  "t": true,
  "z": null
}
`

func TestJSONRoundTripKeepsOrderAndValues(t *testing.T) {
	n, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	out := string(n.Marshal("  ")) + "\n"
	// Only the number is re-rendered (as parsed, 1.5e3 is kept verbatim via
	// json.Number) and the escaped string is re-encoded as UTF-8.
	want := doc
	want = replace(want, `"u": "caf\u00e9"`, `"u": "café"`)
	if out != want {
		t.Fatalf("round trip changed the document:\n%s\nwant:\n%s", out, want)
	}
	if n.Path("hooks", "Stop").Items[0].Get("hooks").Items[0].Get("async").BoolVal() != true {
		t.Fatal("unknown key lost")
	}
}

func replace(s, old, new string) string {
	b := []byte(s)
	i := indexOf(b, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}

func indexOf(b []byte, sub string) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return i
		}
	}
	return -1
}

func TestJSONEditOperations(t *testing.T) {
	n, _ := Parse([]byte(doc))
	n.Ensure("env").Set("NEW", String("v"))
	n.Ensure("statusLine").Set("type", String("command"))
	n.Path("permissions").EnsureArray("deny").Append(String("Edit(x)"))
	stop := n.Path("hooks", "Stop")
	stop.Append(NewValue(map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/y"}}}))
	if stop.Len() != 2 {
		t.Fatal("append")
	}
	dropped := stop.Filter(func(it *Node) bool { return it.Get("hooks").Items[0].Get("command").Str() != "/y" })
	if dropped != 1 || stop.Len() != 1 {
		t.Fatal("filter")
	}
	if !n.Delete("z") || n.Delete("z") {
		t.Fatal("delete")
	}
	keys := []string{}
	for _, m := range n.Members {
		keys = append(keys, m.Key)
	}
	want := []string{"model", "env", "hooks", "permissions", "n", "u", "t", "statusLine"}
	if len(keys) != len(want) {
		t.Fatalf("keys %v", keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("order %v, want %v", keys, want)
		}
	}
	if n.Path("env", "NEW").Str() != "v" || n.Path("permissions", "deny").Len() != 1 {
		t.Fatal("set/append")
	}
	if n.Get("n").IntVal() != 1500 {
		t.Fatalf("number %d", n.Get("n").IntVal())
	}
}

func TestJSONParseErrors(t *testing.T) {
	for _, bad := range []string{`{"a":}`, `{"a":1} x`, `[1,`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestJSONWriteThroughSymlinkKeepsMode(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(`{"a": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	n, exists, err := ReadFile(link)
	if err != nil || !exists {
		t.Fatal(err, exists)
	}
	n.Set("b", Int(2))
	if err := WriteFile(link, n); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a file")
	}
	fi, _ := os.Stat(real)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(real)
	if string(b) != "{\n  \"a\": 1,\n  \"b\": 2\n}\n" {
		t.Fatalf("content %q", b)
	}
	if _, exists, err := ReadFile(filepath.Join(dir, "missing.json")); err != nil || exists {
		t.Fatal("missing file should read as an empty object")
	}
}

func TestJSONKeepsAmpersands(t *testing.T) {
	in := []byte(`{"cmd":"a && b < c > d"}`)
	n, err := Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	if out := string(n.Marshal("")); out != string(in) {
		t.Fatalf("rewritten: %s", out)
	}
	n.Set("x", String("<y>&"))
	if out := string(n.Marshal("")); !strings.Contains(out, `"<y>&"`) {
		t.Fatalf("escaped: %s", out)
	}
}
