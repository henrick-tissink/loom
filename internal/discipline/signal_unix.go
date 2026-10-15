package discipline

import "syscall"

// syscallSignalZero probes whether a process exists without affecting it.
var syscallSignalZero = syscall.Signal(0)
