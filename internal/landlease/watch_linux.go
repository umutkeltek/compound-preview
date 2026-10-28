//go:build linux

package landlease

import (
	"time"

	"golang.org/x/sys/unix"
)

// inotifyWatcher is the Linux leaseWatcher: an inotify watch on the land-lease state DIRECTORY. The
// atomic temp-file rename in writeLeaseAtomic surfaces as IN_MOVED_TO (and the os.Remove in Release as
// IN_DELETE) so a blocked waiter wakes the instant the holder releases. Watching the directory (not the
// file) survives the rename swapping the inode. The wait is bounded with poll(2) on the inotify fd so a
// missed event can never hang the waiter past its deadline. No net/provider I/O — pure local kernel
// event wait.
type inotifyWatcher struct {
	fd int
	wd int
}

func newLeaseWatcher(stateDir string) (leaseWatcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return nil, err
	}
	const mask = unix.IN_CREATE | unix.IN_MOVED_TO | unix.IN_MOVED_FROM |
		unix.IN_DELETE | unix.IN_MODIFY | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF
	wd, err := unix.InotifyAddWatch(fd, stateDir, mask)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &inotifyWatcher{fd: fd, wd: wd}, nil
}

// wait blocks up to bound on the inotify fd via poll(2), returning the instant the directory changes.
// n>0 ⇒ change (drained so the fd doesn't immediately re-fire); n==0 ⇒ bound elapsed. Either way it
// returns nil so the caller re-attempts Acquire under the bounded-poll safety net.
func (w *inotifyWatcher) wait(bound time.Duration) error {
	if bound < 0 {
		bound = 0
	}
	ms := int(bound / time.Millisecond)
	if ms == 0 && bound > 0 {
		ms = 1 // never collapse a positive bound into a non-blocking (0) or infinite (-1) poll
	}
	fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, ms)
	if err != nil {
		if err == unix.EINTR {
			return nil
		}
		return err
	}
	if n > 0 {
		var buf [4096]byte
		_, _ = unix.Read(w.fd, buf[:]) // drain queued events; readability already guaranteed by poll
	}
	return nil
}

func (w *inotifyWatcher) close() error {
	// Closing the inotify fd removes the watch; an explicit InotifyRmWatch is unnecessary.
	return unix.Close(w.fd)
}
