package main

import (
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// captureEmitter records events in a goroutine-safe way and lets tests wait
// for a topic prefix to appear.
type captureEmitter struct {
	mu     sync.Mutex
	events []string
}

func (c *captureEmitter) emit(event string, _ ...any) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *captureEmitter) waitFor(prefix string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, e := range c.events {
			if strings.HasPrefix(e, prefix) {
				c.mu.Unlock()
				return true
			}
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestPTYRegistry_attachIdempotentAndClose(t *testing.T) {
	em := &captureEmitter{}
	reg := newPTYRegistry(func(string) *exec.Cmd { return exec.Command("sleep", "5") }, em.emit)

	if err := reg.attach("s1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := reg.attach("s1"); err != nil {
		t.Fatalf("second attach should be a no-op, got: %v", err)
	}
	if reg.count() != 1 {
		t.Fatalf("want 1 registered pty, got %d", reg.count())
	}
	if !reg.has("s1") {
		t.Fatal("has(s1) should be true")
	}

	reg.close("s1")
	if reg.count() != 0 {
		t.Fatalf("want 0 after close, got %d", reg.count())
	}
}

func TestPTYRegistry_readLoopExitDeregisters(t *testing.T) {
	em := &captureEmitter{}
	reg := newPTYRegistry(func(string) *exec.Cmd { return exec.Command("true") }, em.emit)

	if err := reg.attach("s2"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !em.waitFor("pty:exit:s2", 2*time.Second) {
		t.Fatal("expected pty:exit:s2 after the command exits")
	}
	// Give the deregister path a beat to run after the emit.
	time.Sleep(50 * time.Millisecond)
	if reg.count() != 0 {
		t.Fatalf("want 0 after read-loop EOF, got %d", reg.count())
	}
}

func TestPTYRegistry_sendEchoesData(t *testing.T) {
	em := &captureEmitter{}
	reg := newPTYRegistry(func(string) *exec.Cmd { return exec.Command("cat") }, em.emit)

	if err := reg.attach("s3"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer reg.close("s3")

	if err := reg.send("s3", "ping\n"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !em.waitFor("pty:data:s3", 2*time.Second) {
		t.Fatal("expected pty:data:s3 after cat echoes input")
	}
}

func TestPTYRegistry_sendUnknownIsError(t *testing.T) {
	reg := newPTYRegistry(func(string) *exec.Cmd { return exec.Command("cat") }, (&captureEmitter{}).emit)
	if err := reg.send("nope", "x"); err == nil {
		t.Fatal("send to unattached name should error")
	}
}

// TestPTYRegistry_closeReapsProcess verifies the explicit-close path calls
// Wait() on the child so it doesn't linger as a zombie. cmd.ProcessState is
// only populated once Wait() has returned, so this is a genuine reap check
// rather than a proxy for it.
func TestPTYRegistry_closeReapsProcess(t *testing.T) {
	em := &captureEmitter{}
	reg := newPTYRegistry(func(string) *exec.Cmd { return exec.Command("sleep", "5") }, em.emit)

	if err := reg.attach("reap1"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	reg.mu.Lock()
	h, ok := reg.ptys["reap1"]
	reg.mu.Unlock()
	if !ok {
		t.Fatal("expected reap1 to be registered after attach")
	}

	reg.close("reap1")

	if h.cmd.ProcessState == nil {
		t.Fatal("expected cmd.ProcessState to be set (process reaped) after close")
	}
}

// TestPTYRegistry_naturalExitReapsProcess verifies the read-loop's
// deregister path (natural EOF, not an explicit close) also reaps the
// child. This deliberately does NOT read h.cmd.ProcessState: that field is
// written by the read-loop goroutine's Wait() call with no
// happens-before edge exposed to an external observer (deregister unlocks
// the registry mutex *before* calling Wait, per the required lock
// discipline), so polling it from the test goroutine would be a genuine,
// -race-flagged data race. Instead this asks the OS directly: a reaped
// child's pid is freed, so kill(pid, 0) starts failing with ESRCH once
// Wait() completes; before that the pid is a zombie and still answers
// signal 0 successfully. That is real kernel state, not shared Go memory,
// so it carries no race.
func TestPTYRegistry_naturalExitReapsProcess(t *testing.T) {
	em := &captureEmitter{}
	reg := newPTYRegistry(func(string) *exec.Cmd { return exec.Command("true") }, em.emit)

	if err := reg.attach("reap2"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	reg.mu.Lock()
	h, ok := reg.ptys["reap2"]
	reg.mu.Unlock()
	if !ok {
		t.Fatal("expected reap2 to be registered after attach")
	}
	// cmd.Process is assigned once by pty.Start, before attach() returns,
	// and never reassigned; reading its pid here is race-free.
	pid := h.cmd.Process.Pid

	if !em.waitFor("pty:exit:reap2", 2*time.Second) {
		t.Fatal("expected pty:exit:reap2 after the command exits")
	}

	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = syscall.Kill(pid, 0)
		if lastErr == syscall.ESRCH {
			return // pid gone: reaped
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected pid %d to be reaped (kill(pid,0) -> ESRCH), last result: %v", pid, lastErr)
}
