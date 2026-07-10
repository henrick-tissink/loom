package main

import (
	"os/exec"
	"strings"
	"sync"
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
