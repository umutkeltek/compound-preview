package landlease

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// StatusReadOnly returns the same lease read-model as Status without creating
// the lock directory or file. When the writer-owned lock exists it takes a
// shared flock, so the state read is atomic with Status/Acquire/Refresh writes.
func StatusReadOnly(home string, now time.Time) (StatusResult, error) {
	return statusReadOnlyBound(home, now, nil)
}
func statusReadOnlyBound(home string, now time.Time, binding *leaseBinding) (StatusResult, error) {
	home = strings.TrimSpace(home)
	if home == "" {
		return StatusResult{}, fmt.Errorf("COMPOUND_HOME required")
	}
	lockPath := filepath.Join(home, lockRelPath)
	lockFile, err := os.OpenFile(lockPath, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		leaseStatePath, pathErr := statePath(home)
		if pathErr != nil {
			return StatusResult{}, pathErr
		}
		if _, stateErr := os.Lstat(leaseStatePath); stateErr == nil {
			return StatusResult{}, fmt.Errorf("land-lease state exists without its lock: %s", leaseStatePath)
		} else if !errors.Is(stateErr, os.ErrNotExist) {
			return StatusResult{}, stateErr
		}
		return leaseStatusResult(nil, false, now), nil
	}
	if err != nil {
		return StatusResult{}, fmt.Errorf("open existing land-lease lock %s: %w", lockPath, err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_SH); err != nil {
		return StatusResult{}, fmt.Errorf("lock existing land-lease lock %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	leaseStatePath, err := statePath(home)
	if err != nil {
		return StatusResult{}, err
	}
	current, exists, err := readLease(leaseStatePath)
	if err != nil {
		return StatusResult{}, err
	}
	if err := binding.validate(current); err != nil {
		return StatusResult{}, err
	}
	return leaseStatusResult(current, exists, now), nil
}

func leaseStatusResult(current *Lease, exists bool, now time.Time) StatusResult {
	result := StatusResult{SchemaVersion: SchemaVersion}
	if !exists || current == nil {
		return result
	}
	result.ProofCoordination = cloneProofCoordination(current.ProofCoordination)
	result.Holder = current.Holder
	result.AcquiredAt = current.AcquiredAt
	result.ExpiresAt = current.ExpiresAt
	result.TTLSeconds = current.TTLSeconds
	result.PriorHolder = current.PriorHolder
	result.StealReason = current.StealReason
	result.StolenAt = current.StolenAt
	result.TerminationRequestedBy = current.TerminationRequestedBy
	result.TerminationRequestReason = current.TerminationRequestReason
	result.TerminationRequestedAt = current.TerminationRequestedAt
	result.RemainingSeconds = current.remainingSeconds(now)
	result.Held = current.live(now)
	result.IsExpired = !result.Held
	return result
}
