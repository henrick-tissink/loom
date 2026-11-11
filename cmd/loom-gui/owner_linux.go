//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/unix"
)

// flock owns the GUI independently of D-Bus. The socket only forwards activation;
// a failed forward never permits a second poller. The kernel releases ownership
// on crash; the next owner removes the stale socket while holding the lock.
func acquireGUI() (func(), bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, false, err
	}
	return acquireGUIAt(filepath.Join(home, ".loom"))
}

func acquireGUIAt(dir string) (func(), bool, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, false, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "gui.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	socket := filepath.Join(dir, "gui.sock")
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return nil, false, err
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			conn, e := net.DialTimeout("unix", socket, 200*time.Millisecond)
			if e == nil {
				conn.SetDeadline(deadline)
				fmt.Fprintln(conn, "activate")
				reply, _ := bufio.NewReader(conn).ReadString('\n')
				conn.Close()
				if reply == "queued\n" {
					return func() {}, true, nil
				}
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil, false, fmt.Errorf("Loom is already running but activation failed; close its existing window and retry (agent sessions are preserved)")
	}
	os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		lock.Close()
		return nil, false, err
	}
	if err := os.Chmod(socket, 0600); err != nil {
		listener.Close()
		lock.Close()
		return nil, false, err
	}
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.SetDeadline(time.Now().Add(time.Second))
			msg, _ := bufio.NewReader(c).ReadString('\n')
			if msg == "activate\n" {
				select {
				case activation <- struct{}{}:
				default:
				}
				fmt.Fprintln(c, "queued")
			}
			c.Close()
		}
	}()
	return func() { listener.Close(); lock.Close() }, false, nil
}

var activation = make(chan struct{}, 1)

func listenActivation(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-activation:
				wruntime.WindowUnminimise(ctx)
				wruntime.WindowShow(ctx)
			}
		}
	}()
}
