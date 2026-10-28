//go:build unix

package work

import (
	"errors"
	"syscall"
)

// PIDAlive reports whether a process with pid is still alive by sending signal
// 0 (a liveness probe that delivers no signal). It never execs ps. A nil error
// means the process exists; EPERM means it exists but we may not signal it (so
// it is still alive); ESRCH means no such process.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
