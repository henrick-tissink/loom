package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/godbus/dbus/v5"
)

func (a *App) Platform() string { return runtime.GOOS }

func (a *App) BuildInfo() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		revision := "development"
		dirty := ""
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
			if setting.Key == "vcs.modified" && setting.Value == "true" {
				dirty = " (uncommitted changes)"
			}
		}
		return runtime.GOOS + "/" + runtime.GOARCH + " · " + revision + dirty
	}
	return "development"
}

// desktopCommand bounds only the dispatch process, never its process group.
func desktopCommand(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s did not complete within five seconds", name)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func desktopOpen(target string) error {
	if runtime.GOOS != "linux" {
		return desktopCommand("open", target)
	}
	if _, err := exec.LookPath("xdg-open"); err == nil {
		return desktopCommand("xdg-open", target)
	}
	if _, err := exec.LookPath("gio"); err == nil {
		return desktopCommand("gio", "open", target)
	}
	return fmt.Errorf("install xdg-utils or gio to open files and links")
}

func desktopReveal(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return desktopCommand("open", "-R", abs)
	}
	if fi.IsDir() {
		return desktopOpen(abs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if conn, err := dbus.Connect(os.Getenv("DBUS_SESSION_BUS_ADDRESS"), dbus.WithContext(ctx)); err == nil {
		defer conn.Close()
		uri := (&url.URL{Scheme: "file", Path: abs}).String()
		err = conn.Object("org.freedesktop.FileManager1", "/org/freedesktop/FileManager1").CallWithContext(ctx, "org.freedesktop.FileManager1.ShowItems", 0, []string{uri}, "").Err
		if err == nil {
			return nil
		}
	}
	return desktopOpen(filepath.Dir(abs))
}
