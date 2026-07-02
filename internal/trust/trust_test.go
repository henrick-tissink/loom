package trust

import (
	"os"
	"path/filepath"
	"testing"
)

func writeJSON(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIsTrusted(t *testing.T) {
	p := writeJSON(t, `{"projects":{
		"/w/trusted":  {"hasTrustDialogAccepted": true},
		"/w/declined": {"hasTrustDialogAccepted": false},
		"/w/other":    {"someOtherKey": 1}
	}}`)
	cases := map[string]bool{
		"/w/trusted":  true,
		"/w/declined": false,
		"/w/other":    false,
		"/w/unknown":  false,
	}
	for cwd, want := range cases {
		got, err := IsTrusted(p, cwd)
		if err != nil {
			t.Fatalf("IsTrusted(%q): %v", cwd, err)
		}
		if got != want {
			t.Errorf("IsTrusted(%q) = %v, want %v", cwd, got, want)
		}
	}
}

func TestIsTrustedMissingFile(t *testing.T) {
	got, err := IsTrusted(filepath.Join(t.TempDir(), "nope.json"), "/w/x")
	if err != nil || got {
		t.Fatalf("= %v, %v (want false, nil)", got, err)
	}
}

func TestIsTrustedMalformed(t *testing.T) {
	p := writeJSON(t, `{not json`)
	if _, err := IsTrusted(p, "/w/x"); err == nil {
		t.Fatal("want error for malformed json")
	}
}
