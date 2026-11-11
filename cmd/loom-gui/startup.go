package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
)

func startupError(err error) {
	fmt.Fprintln(os.Stderr, "loom-gui:", err)
	if runtime.GOOS != "linux" {
		return
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return
	}
	dir := filepath.Join(home, ".loom")
	if os.MkdirAll(dir, 0700) == nil {
		// Bounded last-startup record, deliberately excludes transcript contents.
		os.WriteFile(filepath.Join(dir, "startup-error.log"), []byte(err.Error()+"\n"), 0600)
	}
	if e := desktopCommand("zenity", "--error", "--title=Loom could not start", "--text="+err.Error()); e != nil {
		if e := desktopCommand("notify-send", "--app-name=Loom", "--icon=loom", "--", "Loom could not start", err.Error()); e != nil {
			log.Printf("startup message unavailable: %v", e)
		}
	}
}
