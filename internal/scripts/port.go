package scripts

import (
	"errors"
	"net"
	"strconv"
	"syscall"
)

// portFree reports whether nothing listens on the port, on either loopback
// address a dev server might take. A machine without IPv6 loopback can't
// bind ::1 at all; that is not a busy port.
func portFree(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			if errors.Is(err, syscall.EADDRINUSE) || host == "127.0.0.1" {
				return false
			}
			continue
		}
		l.Close()
	}
	return true
}
