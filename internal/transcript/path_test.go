package transcript

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProjectDirName(t *testing.T) {
	cases := map[string]string{
		"/Users/henricktissink/Sauce/HappyPay": "-Users-henricktissink-Sauce-HappyPay",
		"/a/b.c/d_e":                           "-a-b-c-d-e",
		"/x/HappyPay.Web":                      "-x-HappyPay-Web",
	}
	for in, want := range cases {
		if got := ProjectDirName(in); got != want {
			t.Errorf("ProjectDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPath(t *testing.T) {
	got := Path("/home/u/.claude", "/w/proj", "abc-123")
	want := filepath.Join("/home/u/.claude", "projects", "-w-proj", "abc-123.jsonl")
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestNewestSince(t *testing.T) {
	ccd := t.TempDir()
	dir := filepath.Join(ccd, "projects", "-w-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, mod time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	write("old.jsonl", now.Add(-time.Hour))
	write("new.jsonl", now.Add(time.Minute))
	id, err := NewestSince(ccd, "/w/proj", now)
	if err != nil || id != "new" {
		t.Fatalf("NewestSince = %q, %v (want new)", id, err)
	}
	id, err = NewestSince(ccd, "/w/proj", now.Add(2*time.Minute))
	if err != nil || id != "" {
		t.Fatalf("NewestSince (none) = %q, %v (want empty)", id, err)
	}
	id, err = NewestSince(ccd, "/no/such", now)
	if err != nil || id != "" {
		t.Fatalf("NewestSince (missing dir) = %q, %v (want empty, nil)", id, err)
	}
}
