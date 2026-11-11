//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGUIOwnershipForwardAndRestart(t *testing.T) {
	dir := t.TempDir()
	release, forwarded, err := acquireGUIAt(dir)
	if err != nil || forwarded {
		t.Fatalf("owner: %v %v", forwarded, err)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	second, forwarded, err := acquireGUIAt(dir)
	if err != nil || !forwarded {
		t.Fatalf("forward: %v %v", forwarded, err)
	}
	second()
	select {
	case <-activation:
	default:
		t.Fatal("activation not queued")
	}
	release()
	release = nil
	// Stale socket left by a crash must not prevent the next owner.
	os.WriteFile(filepath.Join(dir, "gui.sock"), []byte("stale"), 0600)
	third, forwarded, err := acquireGUIAt(dir)
	if err != nil || forwarded {
		t.Fatalf("restart: %v %v", forwarded, err)
	}
	third()
}
