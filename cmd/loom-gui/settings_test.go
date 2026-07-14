package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsStore_DefaultsAndRoundtrip(t *testing.T) {
	dir := t.TempDir()

	// Missing file → defaults (notifications on, editor auto).
	s := newSettingsStore(dir)
	if got := s.get(); got.Notifications != true || got.Editor != "" {
		t.Fatalf("defaults = %+v, want {Editor:\"\" Notifications:true}", got)
	}

	// Set + persist, then reload from disk.
	if err := s.set(Settings{Editor: "code", Notifications: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatalf("settings.json not written: %v", err)
	}
	reloaded := newSettingsStore(dir)
	if got := reloaded.get(); got.Editor != "code" || got.Notifications != false {
		t.Errorf("reloaded = %+v, want {code, false}", got)
	}
}

func TestSettingsStore_NormalizesEditorAndPartialFile(t *testing.T) {
	dir := t.TempDir()
	s := newSettingsStore(dir)

	// Unknown editor is clamped to auto ("").
	_ = s.set(Settings{Editor: "sublime", Notifications: true})
	if got := s.get().Editor; got != "" {
		t.Errorf("unknown editor → want auto (\"\"), got %q", got)
	}

	// A partial file (editor only) keeps notifications at its default (true).
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"editor":"zed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := newSettingsStore(dir)
	if got := loaded.get(); got.Editor != "zed" || got.Notifications != true {
		t.Errorf("partial file = %+v, want {zed, true}", got)
	}
}
