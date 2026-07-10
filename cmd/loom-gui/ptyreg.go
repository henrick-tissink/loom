package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// emitFunc pushes an event to the frontend. Backed by Wails runtime.EventsEmit
// in production; a capturing fake in tests.
type emitFunc func(event string, data ...any)

type ptyHandle struct {
	cmd *exec.Cmd
	f   *os.File
}

// ptyRegistry owns the live attach clients, keyed by session name. It never
// touches the underlying tmux session — only the PTY process wrapping the
// `tmux attach` invocation.
type ptyRegistry struct {
	mu    sync.Mutex
	ptys  map[string]*ptyHandle
	start func(name string) *exec.Cmd
	emit  emitFunc
}

func newPTYRegistry(start func(name string) *exec.Cmd, emit emitFunc) *ptyRegistry {
	return &ptyRegistry{ptys: map[string]*ptyHandle{}, start: start, emit: emit}
}

func (r *ptyRegistry) has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.ptys[name]
	return ok
}

func (r *ptyRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ptys)
}

// attach starts the command under a PTY and begins streaming. Idempotent: a
// second attach for an already-registered name is a no-op.
func (r *ptyRegistry) attach(name string) error {
	r.mu.Lock()
	if _, ok := r.ptys[name]; ok {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	cmd := r.start(name)
	f, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("attach %s: %w", name, err)
	}

	r.mu.Lock()
	// Lost a race with a concurrent attach — keep the first, drop this one.
	if _, ok := r.ptys[name]; ok {
		r.mu.Unlock()
		_ = f.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		go func() { _ = cmd.Wait() }() // reap the killed child (no zombie)
		return nil
	}
	r.ptys[name] = &ptyHandle{cmd: cmd, f: f}
	r.mu.Unlock()

	go r.readLoop(name, f)
	return nil
}

func (r *ptyRegistry) readLoop(name string, f *os.File) {
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			r.emit("pty:data:"+name, base64.StdEncoding.EncodeToString(buf[:n]))
		}
		if err != nil {
			break
		}
	}
	r.emit("pty:exit:" + name)
	r.deregister(name)
}

func (r *ptyRegistry) send(name, data string) error {
	r.mu.Lock()
	h, ok := r.ptys[name]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("send: %s not attached", name)
	}
	_, err := io.WriteString(h.f, data)
	return err
}

func (r *ptyRegistry) resize(name string, cols, rows uint16) error {
	r.mu.Lock()
	h, ok := r.ptys[name]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("resize: %s not attached", name)
	}
	return pty.Setsize(h.f, &pty.Winsize{Rows: rows, Cols: cols})
}

// close terminates the attach client and deregisters it. The tmux session is
// left running.
func (r *ptyRegistry) close(name string) {
	r.mu.Lock()
	h, ok := r.ptys[name]
	if ok {
		delete(r.ptys, name)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	_ = h.f.Close()
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	_ = h.cmd.Wait()
}

func (r *ptyRegistry) deregister(name string) {
	r.mu.Lock()
	h, ok := r.ptys[name]
	if ok {
		delete(r.ptys, name)
	}
	r.mu.Unlock()
	if ok {
		_ = h.f.Close()
		_ = h.cmd.Wait()
	}
}
