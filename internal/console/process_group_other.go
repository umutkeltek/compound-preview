//go:build !linux

package console

import "syscall"

// consoleProcessGroupAlive uses the signal-0 probe. On darwin the launchd
// reaper collects orphaned group members promptly, so a successful probe
// means a live process; the linux variant (console_process_linux.go) scans
// /proc instead because zombies there satisfy the probe until reaped.
func ProcessGroupAlive(pgid int) (bool, error) {
	err := syscall.Kill(-pgid, 0)
	switch err {
	case nil, syscall.EPERM:
		return true, nil
	case syscall.ESRCH:
		return false, nil
	default:
		return false, err
	}
}
