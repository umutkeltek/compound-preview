package candidates

// R3F_testhelpers.go: test-only seam used by the red test at
// internal/candidates/R3F_replay_identity_test.go. Every function in this
// file is intentionally a thin wrapper around a stable package-private
// symbol so the production code carries no extra surface area.

import (
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// withCandidatePublicationLockForTest mirrors withCandidatePublicationLock
// for the test path so the red test can drive authorizeCandidateReplacement
// from a sibling _test.go file (Go forbids cross-package unexported
// references; this wrapper is the seam).
func withCandidatePublicationLockForTest(homePath string, fn func(*candidateInbox) error) error {
	home, err := openCandidateHome(homePath)
	if err != nil {
		return err
	}
	defer func() { _ = home.close() }()
	lockDir, err := home.openDirectory("lock", true)
	if err != nil {
		return err
	}
	defer func() { _ = lockDir.close() }()
	lockFile, err := lockDir.openPublicationLock()
	if err != nil {
		return err
	}
	defer func() { _ = lockFile.Close() }()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	defer func() {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	}()
	dir, err := home.openDirectory(InboxDir(Memory), true)
	if err != nil {
		return err
	}
	defer func() { _ = dir.close() }()
	return fn(&candidateInbox{home: home, directory: dir})
}

// readCandidateRecordFromInbox is a thin wrapper that lets the red test
// drive readCandidateRecordFromDirectory without an extra public API
// surface.
func readCandidateRecordFromInbox(homePath string, kind Kind, id string) (Record, error) {
	var rec Record
	err := withCandidatePublicationLockForTest(homePath, func(inbox *candidateInbox) error {
		got, err := readCandidateRecordFromDirectory(inbox, kind, id)
		if err != nil {
			return err
		}
		rec = got
		return nil
	})
	return rec, err
}

// identityForTest returns the Fstat-level file identity a fresh open of
// path would yield. The red test compares this against the fileIdentity
// captured during authorizeCandidateReplacement.
func identityForTest(path string) (candidateFileIdentity, error) {
	f, err := os.Open(path)
	if err != nil {
		return candidateFileIdentity{}, err
	}
	defer f.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return candidateFileIdentity{}, err
	}
	return candidateIdentityFromStat(&stat), nil
}

// jsonRecord marshals a raw JSON byte slice through encoding/json so
// WriteCandidate (which canonicalizes JSON) can re-emit it.
func jsonRecord(body []byte) any {
	var x map[string]any
	if err := json.Unmarshal(body, &x); err != nil {
		return nil
	}
	return x
}
