//go:build !darwin && !linux

package landlease

// newLeaseWatcher has no supported file-event primitive off Darwin/Linux (compound's runtime targets).
// Returning errWatchUnsupported makes AcquireWait degrade cleanly to the clock-driven bounded poll — the
// watch is a fast path, never the sole liveness. (internal/landlease is already Unix-only via
// syscall.Flock; this stub only keeps the package compiling wherever Flock does.)
func newLeaseWatcher(stateDir string) (leaseWatcher, error) {
	return nil, errWatchUnsupported
}
