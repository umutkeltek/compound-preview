//go:build darwin

package landlease

import (
	"time"

	"golang.org/x/sys/unix"
)

// kqueueWatcher is the Darwin leaseWatcher: a kqueue armed with an EVFILT_VNODE filter on the land-lease
// state DIRECTORY fd. The atomic temp-file rename in writeLeaseAtomic (and the os.Remove in Release)
// mutates the directory, firing NOTE_WRITE/NOTE_DELETE/NOTE_RENAME so a blocked waiter wakes the instant
// the holder releases. Watching the directory (not the file) sidesteps the rename-swaps-the-inode
// problem and the file-may-be-gone race at arm time. EV_CLEAR makes it edge-triggered: each wait drains
// to a fresh state. No net/provider I/O — pure local kernel event wait, bounded by a Timespec.
type kqueueWatcher struct {
	kq  int
	dir int
}

func newLeaseWatcher(stateDir string) (leaseWatcher, error) {
	dirFd, err := unix.Open(stateDir, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	kq, err := unix.Kqueue()
	if err != nil {
		_ = unix.Close(dirFd)
		return nil, err
	}
	// kqueue() has no CLOEXEC flag on Darwin; set it explicitly so a forked land helper never inherits it.
	unix.CloseOnExec(kq)
	var ev unix.Kevent_t
	unix.SetKevent(&ev, dirFd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR)
	ev.Fflags = unix.NOTE_WRITE | unix.NOTE_DELETE | unix.NOTE_RENAME | unix.NOTE_EXTEND
	// Register the filter (changes non-nil, events nil ⇒ returns immediately without blocking).
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		_ = unix.Close(kq)
		_ = unix.Close(dirFd)
		return nil, err
	}
	return &kqueueWatcher{kq: kq, dir: dirFd}, nil
}

// wait blocks in the kernel until the watched directory changes OR bound elapses, whichever is first.
// n>0 means the directory changed; n==0 means the bound elapsed. Either way it returns nil so the caller
// re-attempts Acquire — the bound guarantees the wait can never exceed a single poll interval, so a
// missed/coalesced event can never wedge the waiter past its deadline.
func (w *kqueueWatcher) wait(bound time.Duration) error {
	if bound < 0 {
		bound = 0
	}
	ts := unix.NsecToTimespec(bound.Nanoseconds())
	events := make([]unix.Kevent_t, 1)
	if _, err := unix.Kevent(w.kq, nil, events, &ts); err != nil {
		if err == unix.EINTR {
			return nil // interrupted syscall: treat as a spurious wake; the outer loop re-checks the deadline
		}
		return err
	}
	return nil
}

func (w *kqueueWatcher) close() error {
	err1 := unix.Close(w.kq)
	err2 := unix.Close(w.dir)
	if err1 != nil {
		return err1
	}
	return err2
}
