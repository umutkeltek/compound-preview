//go:build darwin || linux || freebsd || openbsd || netbsd

package browser

import (
	"errors"
	"os"
	"syscall"
)

// lockSelfHealFile takes an advisory flock on path. wait=false refuses at
// once when another process holds it (one task per profile); wait=true blocks
// for the short registry read-modify-write.
func lockSelfHealFile(path string, wait bool) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		_ = file.Close()
		return nil, errors.New("held by another process")
	}
	return file, nil
}

func unlockSelfHealFile(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}
