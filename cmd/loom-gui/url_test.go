package main

import "testing"

func TestIsHTTPURL(t *testing.T) {
	ok := []string{"http://localhost:3000", "https://example.com/a/b?x=1", "http://x"}
	for _, s := range ok {
		if !isHTTPURL(s) {
			t.Errorf("isHTTPURL(%q) = false, want true", s)
		}
	}
	bad := []string{"file:///etc/passwd", "javascript:alert(1)", "ftp://x", "localhost:3000", "/Users/x/.env", ""}
	for _, s := range bad {
		if isHTTPURL(s) {
			t.Errorf("isHTTPURL(%q) = true, want false", s)
		}
	}
}

func TestOpenURL_RejectsNonHTTP(t *testing.T) {
	a := &App{} // no ctx; scheme check happens first
	if err := a.OpenURL("file:///etc/passwd"); err == nil {
		t.Error("expected refusal for file:// URL")
	}
	if err := a.OpenURL("javascript:alert(1)"); err == nil {
		t.Error("expected refusal for javascript: URL")
	}
}
