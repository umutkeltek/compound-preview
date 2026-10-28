package runattempt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"compound/internal/home"
)

const (
	runattemptLegacyLockRelative = "lock/runattempts.lock"
	runattemptLockDirRelative    = "lock/runattempts"
)

// These narrow seams make durability and release ambiguity deterministic in
// tests. Production callers always use the real UID, flock, close, and sync.
var (
	runattemptCurrentUID    = os.Geteuid
	runattemptFlock         = func(fd int, operation int) error { return syscall.Flock(fd, operation) }
	runattemptCloseLockFile = func(file *os.File) error {
		return file.Close()
	}
	runattemptLockBeforeOpen = func(_, _ string) {}
	runattemptSyncDirectory  = syncRunAttemptDirectory
)

type runAttemptLockRoot struct {
	root          *os.Root
	canonicalHome string
}

type runAttemptLockLease struct {
	file          *os.File
	displayPath   string
	locked        bool
	unlockProcess func()
}

func canonicalRunAttemptHome(home string) (string, error) {
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("runattempt lock: canonicalize home: %w", err)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("runattempt lock: absolute home: %w", err)
	}
	return filepath.Clean(absolute), nil
}

// openRunAttemptLockRoot permits an explicitly selected symlink alias for the
// home itself, then pins and validates the resolved directory identity. Every
// component beneath that pinned root is handled with Lstat/open/Lstat checks.
func openRunAttemptLockRoot(home string) (*runAttemptLockRoot, error) {
	canonicalHome, err := canonicalRunAttemptHome(home)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(canonicalHome)
	if err != nil {
		return nil, fmt.Errorf("runattempt lock: inspect home %s: %w", canonicalHome, err)
	}
	if err := validateRunAttemptHomeInfo(before); err != nil {
		return nil, fmt.Errorf("runattempt lock: unsafe home %s: %w", canonicalHome, err)
	}
	root, err := os.OpenRoot(canonicalHome)
	if err != nil {
		return nil, fmt.Errorf("runattempt lock: open rooted home %s: %w", canonicalHome, err)
	}
	opened, openErr := root.Stat(".")
	after, afterErr := os.Lstat(canonicalHome)
	if err := errors.Join(openErr, afterErr); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("runattempt lock: pin home %s: %w", canonicalHome, err)
	}
	if err := errors.Join(validateRunAttemptHomeInfo(opened), validateRunAttemptHomeInfo(after)); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("runattempt lock: unsafe pinned home %s: %w", canonicalHome, err)
	}
	if !os.SameFile(before, opened) || !os.SameFile(before, after) {
		_ = root.Close()
		return nil, fmt.Errorf("runattempt lock: home identity changed while opening: %s", canonicalHome)
	}
	return &runAttemptLockRoot{root: root, canonicalHome: canonicalHome}, nil
}

func validateRunAttemptHomeInfo(info os.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("home must be a non-symlink directory")
	}
	if !runAttemptOwnedByCurrentUID(info) {
		return fmt.Errorf("home must be owned by current uid %d", runattemptCurrentUID())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("home must not be group/world writable: mode=%04o", info.Mode().Perm())
	}
	return nil
}

func runAttemptOwnedByCurrentUID(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == runattemptCurrentUID()
}

func runAttemptSingleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Nlink) == 1
}

func (r *runAttemptLockRoot) close(cause error) error {
	if r == nil || r.root == nil {
		return cause
	}
	closeErr := r.root.Close()
	r.root = nil
	if closeErr != nil {
		closeErr = fmt.Errorf("runattempt lock: close rooted home: %w", closeErr)
	}
	return errors.Join(cause, closeErr)
}

func (r *runAttemptLockRoot) display(relative string) string {
	return filepath.Join(r.canonicalHome, filepath.FromSlash(relative))
}

func (r *runAttemptLockRoot) snapshot(relative, leafKind string) ([]os.FileInfo, error) {
	relative = filepath.Clean(filepath.FromSlash(relative))
	if relative == "." {
		info, err := r.root.Stat(".")
		if err != nil {
			return nil, err
		}
		if err := validateRunAttemptHomeInfo(info); err != nil {
			return nil, err
		}
		return []os.FileInfo{info}, nil
	}
	if !filepath.IsLocal(relative) {
		return nil, fmt.Errorf("runattempt lock: path escapes pinned home: %s", relative)
	}
	components := strings.Split(relative, string(filepath.Separator))
	infos := make([]os.FileInfo, 0, len(components))
	for index := range components {
		componentPath := filepath.Join(components[:index+1]...)
		info, err := r.root.Lstat(componentPath)
		if err != nil {
			return nil, err
		}
		leaf := index == len(components)-1
		if !leaf || leafKind == "directory" {
			if err := validateRunAttemptLockDirectory(componentPath, info); err != nil {
				return nil, err
			}
		} else if leafKind == "file" {
			if err := validateRunAttemptLockFile(componentPath, info); err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("runattempt lock: unsupported leaf kind %q", leafKind)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func validateRunAttemptLockDirectory(relative string, info os.FileInfo) error {
	display := filepath.ToSlash(relative)
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("runattempt lock: %s must be a non-symlink directory", display)
	}
	if !runAttemptOwnedByCurrentUID(info) {
		return fmt.Errorf("runattempt lock: %s must be owned by current uid %d", display, runattemptCurrentUID())
	}
	mode := info.Mode().Perm()
	switch display {
	case "lock":
		if mode&0o022 != 0 {
			return fmt.Errorf("runattempt lock: lock directory must not be group/world writable: mode=%04o", mode)
		}
	case runattemptLockDirRelative:
		if mode != 0o700 {
			return fmt.Errorf("runattempt lock: per-attempt directory mode=%04o, want 0700", mode)
		}
	default:
		return fmt.Errorf("runattempt lock: unowned lock directory %s", display)
	}
	return nil
}

func validateRunAttemptLockFile(relative string, info os.FileInfo) error {
	display := filepath.ToSlash(relative)
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("runattempt lock: %s must be a non-symlink regular file", display)
	}
	if !runAttemptOwnedByCurrentUID(info) {
		return fmt.Errorf("runattempt lock: %s must be owned by current uid %d", display, runattemptCurrentUID())
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		return fmt.Errorf("runattempt lock: %s mode=%04o, want 0600", display, mode)
	}
	if !runAttemptSingleLink(info) {
		return fmt.Errorf("runattempt lock: %s must have exactly one hard link", display)
	}
	return nil
}

func sameRunAttemptLockSnapshot(before, after []os.FileInfo) bool {
	if len(before) != len(after) {
		return false
	}
	for index := range before {
		if !os.SameFile(before[index], after[index]) {
			return false
		}
	}
	return true
}

func (r *runAttemptLockRoot) verifyDirectory(relative string) ([]os.FileInfo, error) {
	before, err := r.snapshot(relative, "directory")
	if err != nil {
		return nil, err
	}
	directory, err := r.root.Open(filepath.FromSlash(relative))
	if err != nil {
		return nil, err
	}
	opened, statErr := directory.Stat()
	after, afterErr := r.snapshot(relative, "directory")
	closeErr := directory.Close()
	if err := errors.Join(statErr, afterErr, closeErr); err != nil {
		return nil, err
	}
	if !sameRunAttemptLockSnapshot(before, after) || !opened.IsDir() || !os.SameFile(before[len(before)-1], opened) {
		return nil, fmt.Errorf("runattempt lock: directory identity changed: %s", r.display(relative))
	}
	return after, nil
}

func (r *runAttemptLockRoot) ensureDirectory(relative string, createMode os.FileMode) error {
	if _, err := r.verifyDirectory(relative); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))
	parentBefore, err := r.verifyDirectory(parent)
	if err != nil {
		return err
	}
	created := false
	if err := r.root.Mkdir(filepath.FromSlash(relative), createMode); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("runattempt lock: create directory %s: %w", r.display(relative), err)
		}
	} else {
		created = true
	}
	if _, err := r.verifyDirectory(relative); err != nil {
		return err
	}
	parentAfter, err := r.verifyDirectory(parent)
	if err != nil {
		return err
	}
	if !sameRunAttemptLockSnapshot(parentBefore, parentAfter) {
		return fmt.Errorf("runattempt lock: parent identity changed while creating %s", r.display(relative))
	}
	if created {
		return r.syncDirectory(parent)
	}
	return nil
}

func (r *runAttemptLockRoot) syncDirectory(relative string) error {
	before, err := r.snapshot(relative, "directory")
	if err != nil {
		return err
	}
	directory, err := r.root.Open(filepath.FromSlash(relative))
	if err != nil {
		return err
	}
	opened, statErr := directory.Stat()
	after, afterErr := r.snapshot(relative, "directory")
	if err := errors.Join(statErr, afterErr); err != nil {
		_ = directory.Close()
		return err
	}
	if !sameRunAttemptLockSnapshot(before, after) || !opened.IsDir() || !os.SameFile(before[len(before)-1], opened) {
		_ = directory.Close()
		return fmt.Errorf("runattempt lock: directory identity changed before sync: %s", r.display(relative))
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if errors.Is(syncErr, syscall.EINVAL) {
		syncErr = nil
	}
	return errors.Join(syncErr, closeErr)
}

func (r *runAttemptLockRoot) openLockFile(relative, kind string) (*os.File, error) {
	relativeOS := filepath.FromSlash(relative)
	parent := filepath.ToSlash(filepath.Dir(relativeOS))
	parentBefore, err := r.verifyDirectory(parent)
	if err != nil {
		return nil, err
	}
	before, err := r.snapshot(relative, "file")
	created := false
	var file *os.File
	if errors.Is(err, os.ErrNotExist) {
		file, err = r.root.OpenFile(relativeOS, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			created = true
		} else if errors.Is(err, os.ErrExist) {
			before, err = r.snapshot(relative, "file")
			if err == nil {
				runattemptLockBeforeOpen(kind, r.display(relative))
				file, err = r.root.OpenFile(relativeOS, os.O_RDWR, 0)
			}
		}
	} else if err == nil {
		runattemptLockBeforeOpen(kind, r.display(relative))
		file, err = r.root.OpenFile(relativeOS, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("runattempt lock: open %s: %w", r.display(relative), err)
	}
	opened, statErr := file.Stat()
	after, afterErr := r.snapshot(relative, "file")
	if err := errors.Join(statErr, afterErr); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := validateRunAttemptLockFile(relativeOS, opened); err != nil {
		_ = file.Close()
		return nil, err
	}
	if before != nil && !created && !sameRunAttemptLockSnapshot(before, after) {
		_ = file.Close()
		return nil, fmt.Errorf("runattempt lock: path identity changed while opening %s", r.display(relative))
	}
	if !opened.Mode().IsRegular() || !os.SameFile(after[len(after)-1], opened) {
		_ = file.Close()
		return nil, fmt.Errorf("runattempt lock: file identity changed while opening %s", r.display(relative))
	}
	parentAfter := after[:len(after)-1]
	if !sameRunAttemptLockSnapshot(parentBefore, parentAfter) {
		_ = file.Close()
		return nil, fmt.Errorf("runattempt lock: parent identity changed while opening %s", r.display(relative))
	}
	if created {
		if err := r.syncDirectory(parent); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("runattempt lock: sync new lock file %s: %w", r.display(relative), err)
		}
	}
	return file, nil
}

func acquireRunAttemptLockLease(key, displayPath string, file *os.File, nonblocking bool) (*runAttemptLockLease, bool, error) {
	unlockProcess, acquired := acquireProcessAttemptLock(key, nonblocking)
	if !acquired {
		closeErr := runattemptCloseLockFile(file)
		if closeErr != nil {
			return nil, false, fmt.Errorf("runattempt lock: close unacquired %s: %w", displayPath, closeErr)
		}
		return nil, false, nil
	}
	operation := syscall.LOCK_EX
	if nonblocking {
		operation |= syscall.LOCK_NB
	}
	if err := runattemptFlock(int(file.Fd()), operation); err != nil {
		closeErr := runattemptCloseLockFile(file)
		unlockProcess()
		if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			if closeErr != nil {
				return nil, false, fmt.Errorf("runattempt lock: close busy %s: %w", displayPath, closeErr)
			}
			return nil, false, nil
		}
		return nil, false, errors.Join(
			fmt.Errorf("runattempt lock: acquire %s: %w", displayPath, err),
			wrapRunAttemptLockClose(displayPath, closeErr),
		)
	}
	return &runAttemptLockLease{file: file, displayPath: displayPath, locked: true, unlockProcess: unlockProcess}, true, nil
}

func wrapRunAttemptLockClose(displayPath string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("runattempt lock: close %s: %w", displayPath, err)
}

func (lease *runAttemptLockLease) release() error {
	if lease == nil {
		return nil
	}
	var unlockErr error
	if lease.locked {
		if err := runattemptFlock(int(lease.file.Fd()), syscall.LOCK_UN); err != nil {
			unlockErr = fmt.Errorf("runattempt lock: release %s: %w", lease.displayPath, err)
		}
		lease.locked = false
	}
	closeErr := wrapRunAttemptLockClose(lease.displayPath, runattemptCloseLockFile(lease.file))
	if lease.unlockProcess != nil {
		lease.unlockProcess()
		lease.unlockProcess = nil
	}
	return errors.Join(unlockErr, closeErr)
}

func (s *Store) withLegacyRunAttemptLockMode(nonblocking bool, fn func() error) (bool, error) {
	root, err := openRunAttemptLockRoot(s.home)
	if err != nil {
		return false, err
	}
	if err := root.ensureDirectory("lock", 0o755); err != nil {
		return false, root.close(err)
	}
	legacyFile, err := root.openLockFile(runattemptLegacyLockRelative, "legacy")
	if err != nil {
		return false, root.close(err)
	}
	legacyKey := root.canonicalHome + "\x00legacy"
	legacyLease, acquired, err := acquireRunAttemptLockLease(legacyKey, root.display(runattemptLegacyLockRelative), legacyFile, nonblocking)
	if err != nil || !acquired {
		return false, root.close(err)
	}
	callbackErr := fn()
	releaseErr := legacyLease.release()
	return true, root.close(errors.Join(callbackErr, releaseErr))
}

func (s *Store) withPerAttemptLockMode(id string, nonblocking bool, fn func() error) (bool, error) {
	root, err := openRunAttemptLockRoot(s.home)
	if err != nil {
		return false, err
	}
	if err := root.ensureDirectory("lock", 0o755); err != nil {
		return false, root.close(err)
	}
	if err := root.ensureDirectory(runattemptLockDirRelative, 0o700); err != nil {
		return false, root.close(err)
	}
	attemptRelative := filepath.ToSlash(filepath.Join(runattemptLockDirRelative, id+".lock"))
	attemptFile, err := root.openLockFile(attemptRelative, "per-attempt")
	if err != nil {
		return false, root.close(err)
	}
	attemptKey := root.canonicalHome + "\x00" + id
	attemptLease, acquired, err := acquireRunAttemptLockLease(attemptKey, root.display(attemptRelative), attemptFile, nonblocking)
	if err != nil || !acquired {
		return false, root.close(err)
	}
	callbackErr := fn()
	releaseErr := attemptLease.release()
	return true, root.close(errors.Join(callbackErr, releaseErr))
}

func (s *Store) withAttemptMutationLocks(id string, nonblocking bool, fn func() error) (bool, error) {
	legacyAcquired := false
	perAttemptAcquired, err := s.withPerAttemptLockMode(id, nonblocking, func() error {
		var legacyErr error
		legacyAcquired, legacyErr = s.withLegacyRunAttemptLockMode(nonblocking, fn)
		return legacyErr
	})
	if err != nil || !perAttemptAcquired {
		return false, err
	}
	return legacyAcquired, nil
}

func (s *Store) withPerAttemptLock(id string, fn func() error) error {
	acquired, err := s.withPerAttemptLockMode(id, false, fn)
	if err != nil {
		return err
	}
	if !acquired {
		return errors.New("runattempt lock: blocking per-attempt acquisition returned without lock")
	}
	return nil
}

func syncRunAttemptDirectory(directoryPath string) error {
	before, err := os.Lstat(directoryPath)
	if err != nil {
		return fmt.Errorf("runattempt write: inspect directory %s: %w", directoryPath, err)
	}
	if err := validateRunAttemptRecordDirectoryInfo(directoryPath, before); err != nil {
		return err
	}
	directory, err := os.Open(directoryPath)
	if err != nil {
		return fmt.Errorf("runattempt write: open directory %s: %w", directoryPath, err)
	}
	opened, statErr := directory.Stat()
	after, afterErr := os.Lstat(directoryPath)
	if err := errors.Join(statErr, afterErr); err != nil {
		_ = directory.Close()
		return fmt.Errorf("runattempt write: validate directory %s: %w", directoryPath, err)
	}
	if err := validateRunAttemptRecordDirectoryInfo(directoryPath, after); err != nil {
		_ = directory.Close()
		return err
	}
	if !opened.IsDir() ||
		!os.SameFile(before, opened) || !os.SameFile(before, after) {
		_ = directory.Close()
		return fmt.Errorf("runattempt write: containing directory identity changed: %s", directoryPath)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if errors.Is(syncErr, syscall.EINVAL) {
		syncErr = nil
	}
	return errors.Join(syncErr, closeErr)
}

func validateRunAttemptRecordDirectoryInfo(path string, info os.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("runattempt write: containing path must be a non-symlink directory: %s", path)
	}
	if !runAttemptOwnedByCurrentUID(info) {
		return fmt.Errorf("runattempt write: containing directory must be owned by current uid %d: %s", runattemptCurrentUID(), path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("runattempt write: containing directory must not be group/world writable: %s mode=%04o", path, info.Mode().Perm())
	}
	return nil
}

const launchEvidenceSchemaVersion = "compound.runattempt.launch_evidence.v0.1"

const (
	launchEvidencePrepared   = "prepared"
	launchEvidenceCommitted  = "committed"
	launchEvidenceStartError = "start_error"
)

var (
	errLaunchEvidenceInvalidDocument    = errors.New("runattempt launch evidence: invalid document")
	errLaunchEvidenceTrailingDocument   = errors.New("runattempt launch evidence: trailing document")
	errLaunchEvidenceUnsupportedVersion = errors.New("runattempt launch evidence: unsupported version")
	errLaunchEvidenceIdentityMismatch   = errors.New("runattempt launch evidence: identity mismatch")
	errLaunchEvidenceEventTypeMismatch  = errors.New("runattempt launch evidence: event type mismatch")
	errLaunchEvidenceInvalidTimestamp   = errors.New("runattempt launch evidence: invalid timestamp")
)

type launchEvidence struct {
	SchemaVersion string    `json:"schema_version"`
	AttemptID     string    `json:"attempt_id"`
	Kind          string    `json:"kind"`
	At            time.Time `json:"at"`
}

var (
	runattemptWriteLaunchEvidence = writeLaunchEvidence
	runattemptReadLaunchEvidence  = readLaunchEvidence
)

func launchEvidenceDirectory(homePath string) string {
	return filepath.Join(homePath, "run_attempts", "launch")
}

func launchEvidencePath(homePath, id, kind string) string {
	return filepath.Join(launchEvidenceDirectory(homePath), id+"."+kind+".json")
}

func writeLaunchEvidence(homePath string, evidence launchEvidence) error {
	if err := validateLaunchEvidence(evidence, evidence.AttemptID, evidence.Kind); err != nil {
		return err
	}
	if existing, err := readLaunchEvidence(homePath, evidence.AttemptID, evidence.Kind); err == nil {
		if reflect.DeepEqual(existing, evidence) {
			if err := validateLaunchEvidenceDirectory(homePath); err != nil {
				return err
			}
			if err := syncRunAttemptDirectory(launchEvidenceDirectory(homePath)); err != nil {
				return fmt.Errorf("runattempt launch evidence: resync existing %s/%s: %w", evidence.AttemptID, evidence.Kind, err)
			}
			return nil
		}
		return fmt.Errorf("runattempt launch evidence: immutable %s/%s already differs", evidence.AttemptID, evidence.Kind)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, conflictingKind := range conflictingLaunchEvidenceKinds(evidence.Kind) {
		if _, err := readLaunchEvidence(homePath, evidence.AttemptID, conflictingKind); err == nil {
			return fmt.Errorf("runattempt launch evidence: %s disposition conflicts with existing %s", evidence.Kind, conflictingKind)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if evidence.Kind != launchEvidencePrepared {
		if _, err := readLaunchEvidence(homePath, evidence.AttemptID, launchEvidencePrepared); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("runattempt launch evidence: %s disposition requires prepared evidence", evidence.Kind)
			}
			return err
		}
	}
	directory := launchEvidenceDirectory(homePath)
	if err := home.EnsureRootedDirectoryDurable(homePath, directory, "RunAttempt launch evidence", 0o700); err != nil {
		return fmt.Errorf("runattempt launch evidence: ensure directory: %w", err)
	}
	if err := validateLaunchEvidenceDirectory(homePath); err != nil {
		return err
	}
	body, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return fmt.Errorf("runattempt launch evidence: encode %s/%s: %w", evidence.AttemptID, evidence.Kind, err)
	}
	path := launchEvidencePath(homePath, evidence.AttemptID, evidence.Kind)
	if err := home.WriteRootedNewFileDurable(homePath, path, "RunAttempt launch evidence", body, 0o600); err != nil {
		return fmt.Errorf("runattempt launch evidence: publish %s/%s: %w", evidence.AttemptID, evidence.Kind, err)
	}
	return nil
}

func validateLaunchEvidenceDirectory(homePath string) error {
	_, err := inspectLaunchEvidenceDirectory(homePath)
	return err
}

func inspectLaunchEvidenceDirectory(homePath string) (os.FileInfo, error) {
	directory := launchEvidenceDirectory(homePath)
	before, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("runattempt launch evidence: inspect directory: %w", err)
	}
	if err := validateLaunchEvidenceDirectoryInfo(directory, before); err != nil {
		return nil, err
	}
	exists, err := home.RootedDirectoryExists(homePath, directory, "RunAttempt launch evidence")
	if err != nil {
		return nil, fmt.Errorf("runattempt launch evidence: validate rooted directory: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("runattempt launch evidence: directory does not exist: %s", directory)
	}
	after, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("runattempt launch evidence: reinspect directory: %w", err)
	}
	if err := validateLaunchEvidenceDirectoryInfo(directory, after); err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) {
		return nil, fmt.Errorf("runattempt launch evidence: directory identity changed while validating: %s", directory)
	}
	return after, nil
}

func validateLaunchEvidenceDirectoryInfo(path string, info os.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		!runAttemptOwnedByCurrentUID(info) || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("runattempt launch evidence: directory must be rooted, current-uid-owned, non-symlink, and mode=0700: %s", path)
	}
	return nil
}

func readLaunchEvidence(homePath, id, kind string) (launchEvidence, error) {
	directoryBefore, err := inspectLaunchEvidenceDirectory(homePath)
	if err != nil {
		return launchEvidence{}, err
	}
	path := launchEvidencePath(homePath, id, kind)
	before, err := os.Lstat(path)
	if err != nil {
		return launchEvidence{}, err
	}
	if err := validateLaunchEvidenceFileInfo(path, before); err != nil {
		return launchEvidence{}, err
	}
	body, err := home.ReadRootedRegularFile(homePath, path, "RunAttempt launch evidence")
	if err != nil {
		return launchEvidence{}, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return launchEvidence{}, err
	}
	if err := validateLaunchEvidenceFileInfo(path, after); err != nil {
		return launchEvidence{}, err
	}
	if !os.SameFile(before, after) {
		return launchEvidence{}, fmt.Errorf("runattempt launch evidence: file identity changed while reading %s", path)
	}
	directoryAfter, err := inspectLaunchEvidenceDirectory(homePath)
	if err != nil {
		return launchEvidence{}, err
	}
	if !os.SameFile(directoryBefore, directoryAfter) {
		return launchEvidence{}, fmt.Errorf("runattempt launch evidence: directory identity changed while reading %s", path)
	}
	var evidence launchEvidence
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return launchEvidence{}, errLaunchEvidenceInvalidDocument
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return launchEvidence{}, errLaunchEvidenceTrailingDocument
	}
	if err := validateLaunchEvidence(evidence, id, kind); err != nil {
		return launchEvidence{}, err
	}
	return evidence, nil
}

func validateLaunchEvidenceFileInfo(path string, info os.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("runattempt launch evidence: file must be a non-symlink regular file: %s", path)
	}
	if !runAttemptOwnedByCurrentUID(info) || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("runattempt launch evidence: file must be current-uid-owned mode=0600: %s", path)
	}
	if !runAttemptSingleLink(info) {
		return fmt.Errorf("runattempt launch evidence: file must have exactly one hard link: %s", path)
	}
	return nil
}

func conflictingLaunchEvidenceKinds(kind string) []string {
	switch kind {
	case launchEvidencePrepared:
		return []string{launchEvidenceCommitted, launchEvidenceStartError}
	case launchEvidenceCommitted:
		return []string{launchEvidenceStartError}
	case launchEvidenceStartError:
		return []string{launchEvidenceCommitted}
	default:
		return nil
	}
}

func validateLaunchEvidence(evidence launchEvidence, id, kind string) error {
	if evidence.SchemaVersion != launchEvidenceSchemaVersion {
		return errLaunchEvidenceUnsupportedVersion
	}
	if !Valid(evidence.AttemptID) || evidence.AttemptID != id {
		return errLaunchEvidenceIdentityMismatch
	}
	if evidence.Kind != kind {
		return errLaunchEvidenceEventTypeMismatch
	}
	if evidence.At.IsZero() {
		return errLaunchEvidenceInvalidTimestamp
	}
	switch kind {
	case launchEvidencePrepared, launchEvidenceCommitted, launchEvidenceStartError:
	default:
		return errLaunchEvidenceEventTypeMismatch
	}
	return nil
}

func loadLaunchLifecycle(homePath, id string) (*LaunchLifecycle, error) {
	// Sidecars are immutable and additive, and writeLaunchEvidence refuses to
	// publish either disposition before durable prepared evidence. A reader can
	// nevertheless straddle those two publications: prepared absent on its
	// first read, disposition present on its next. Once that disposition is
	// visible, one complete retry must also see prepared; if it does not, the
	// store is genuinely corrupt rather than concurrently advancing.
	for generation := 0; generation < 2; generation++ {
		type evidenceResult struct {
			evidence launchEvidence
			exists   bool
		}
		read := func(kind string) (evidenceResult, error) {
			evidence, err := runattemptReadLaunchEvidence(homePath, id, kind)
			if errors.Is(err, os.ErrNotExist) {
				return evidenceResult{}, nil
			}
			if err != nil {
				return evidenceResult{}, err
			}
			return evidenceResult{evidence: evidence, exists: true}, nil
		}
		prepared, err := read(launchEvidencePrepared)
		if err != nil {
			return nil, err
		}
		committed, err := read(launchEvidenceCommitted)
		if err != nil {
			return nil, err
		}
		startError, err := read(launchEvidenceStartError)
		if err != nil {
			return nil, err
		}
		if !prepared.exists {
			if committed.exists || startError.exists {
				if generation == 0 {
					continue
				}
				return nil, fmt.Errorf("runattempt launch evidence %s: disposition exists without prepared boundary", id)
			}
			return nil, nil
		}
		if committed.exists && startError.exists {
			return nil, fmt.Errorf("runattempt launch evidence %s: conflicting committed and start_error dispositions", id)
		}
		lifecycle := &LaunchLifecycle{PreparedAt: timePointer(prepared.evidence.At)}
		if committed.exists {
			if !committed.evidence.At.Equal(prepared.evidence.At) {
				return nil, fmt.Errorf("runattempt launch evidence %s: committed timestamp differs from prepared boundary", id)
			}
			lifecycle.CommittedAt = timePointer(committed.evidence.At)
		}
		if startError.exists {
			if !startError.evidence.At.Equal(prepared.evidence.At) {
				return nil, fmt.Errorf("runattempt launch evidence %s: start_error timestamp differs from prepared boundary", id)
			}
			lifecycle.StartErrorAt = timePointer(startError.evidence.At)
		}
		return lifecycle, nil
	}
	return nil, fmt.Errorf("runattempt launch evidence %s: lifecycle read exhausted", id)
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}
