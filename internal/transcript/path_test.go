package transcript

import (
	"path/filepath"
	"testing"
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
