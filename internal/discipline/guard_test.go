package discipline

import (
	"errors"
	"os"
	"testing"
)

func TestRefuseIfAgent(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	if err := RefuseIfAgent(os.Stdin); !errors.Is(err, ErrAgent) {
		t.Fatalf("CLAUDECODE set: %v", err)
	}
	t.Setenv("CLAUDECODE", "")
	devnull, err := os.Open(os.DevNull)
	must(t, err)
	defer devnull.Close()
	if err := RefuseIfAgent(devnull); !errors.Is(err, ErrNoTTY) {
		t.Fatalf("/dev/null stdin: %v (a character device is not a terminal)", err)
	}
	pr, pw, err := os.Pipe()
	must(t, err)
	defer pr.Close()
	defer pw.Close()
	if err := RefuseIfAgent(pr); !errors.Is(err, ErrNoTTY) {
		t.Fatalf("pipe stdin: %v", err)
	}
	if tty, err := os.Open("/dev/tty"); err == nil {
		defer tty.Close()
		if err := RefuseIfAgent(tty); err != nil {
			t.Fatalf("a real terminal: %v", err)
		}
	}
}
