package eventstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
)

const (
	appendLockName       = ".append.lock"
	allFamiliesLockScope = "*"
)

type appendLock struct {
	file      *os.File
	processMu *sync.Mutex
	gate      *sync.RWMutex
}

func acquireAppendLock(segRoot string) (*appendLock, error) {
	if err := os.MkdirAll(segRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create event segment root: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(segRoot, appendLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open event append lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock event append stream: %w", err)
	}
	return &appendLock{file: file}, nil
}

func (l *appendLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock event append stream: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close event append lock: %w", closeErr)
	}
	return nil
}

// shardedAppendLock is a per-family named lock so concurrent appends to
// DIFFERENT families (a proof chain writes act_, pol_, rec_, tr_ in sequence;
// hook events are their own family) do not serialize on one file. Prior to
// 2026-08-02 a single .append.lock flock guarded every put, and with ~29
// concurrent Claude sessions the hot PreToolUse path measured 11.4s/15.3s/41.5s
// per tool call under a 25-process queue on one inode (load 265). Sharding by
// family keeps the invariant the lock exists to protect — a family's segment
// is appended by at most one process at a time — while letting families make
// progress in parallel. Because every call reuses one cached flock descriptor,
// flock alone does not exclude goroutines in this process: a per-family mutex
// supplies that exclusion. A shared/exclusive gate lets ordinary families run
// concurrently while maintenance freezes every family as one boundary.
//
// PACKAGE-level fd cache, keyed by segment root: real consumers call
// eventstore.Open per operation (per tool call), so an engine-scoped fd map
// would open a fresh set of lock files per call and leak them in long-lived
// processes (the console supervisor, a drain daemon). Mirroring the hot segment
// cache, the map below holds ONE set of family lock fds per absolute segRoot
// for the process lifetime; the flock itself is cheap on an already-open fd.
type shardedAppendLock struct {
	dir       string
	bindErr   error
	gate      sync.RWMutex
	mu        sync.Mutex
	fd        map[string]*os.File
	processMu map[string]*sync.Mutex
	waiting   func(string)         // test-only contention observer
	acquired  func(string)         // test-only acquisition observer
	rawFlock  func(int, int) error // test-only syscall seam; defaults to syscall.Flock
}

var sharedAppendLockCache = struct {
	mu     sync.Mutex
	byRoot map[string]*shardedAppendLock
}{byRoot: map[string]*shardedAppendLock{}}

// shardedAppendLocksFor returns the shared per-family lock set for a segment
// root, opening the first lock file lazily on acquire.
func shardedAppendLocksFor(segRoot string) *shardedAppendLock {
	key, err := canonicalPathThroughExistingAncestor(segRoot)
	if err != nil {
		abs, absErr := filepath.Abs(filepath.Clean(segRoot))
		if absErr == nil {
			segRoot = abs
		}
		key = segRoot
	}
	sharedAppendLockCache.mu.Lock()
	defer sharedAppendLockCache.mu.Unlock()
	if l, ok := sharedAppendLockCache.byRoot[key]; ok {
		return l
	}
	l := &shardedAppendLock{
		dir:       segRoot,
		bindErr:   err,
		fd:        map[string]*os.File{},
		processMu: map[string]*sync.Mutex{},
		rawFlock:  syscall.Flock,
	}
	sharedAppendLockCache.byRoot[key] = l
	return l
}

// canonicalPathThroughExistingAncestor resolves aliases even before the
// segments directory exists. The event store is local and single-user for the
// public alpha; live retargeting of a trusted home alias is outside that
// contract, but two stable spellings of one home must share one process lock.
func canonicalPathThroughExistingAncestor(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("canonicalize event segment root: %w", err)
	}
	current := abs
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", fmt.Errorf("canonicalize event segment root: %w", resolveErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("canonicalize event segment root: no existing ancestor for %s", path)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func (s *shardedAppendLock) mutexFor(family string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	mu := s.processMu[family]
	if mu == nil {
		mu = &sync.Mutex{}
		s.processMu[family] = mu
	}
	return mu
}

func (s *shardedAppendLock) lockFile(family string) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if file := s.fd[family]; file != nil {
		return file, nil
	}
	file, err := os.OpenFile(filepath.Join(s.dir, appendLockName+"."+family), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open event append lock: %w", err)
	}
	s.fd[family] = file
	return file, nil
}

func (s *shardedAppendLock) acquireFamily(family string) (*appendLock, error) {
	processMu := s.mutexFor(family)
	processMu.Lock()
	file, err := s.lockFile(family)
	if err != nil {
		processMu.Unlock()
		return nil, err
	}
	if err := s.rawFlock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		processMu.Unlock()
		return nil, fmt.Errorf("lock event append stream: %w", err)
	}
	return &appendLock{file: file, processMu: processMu}, nil
}

func (s *shardedAppendLock) unlock(file *os.File) error {
	for {
		err := s.rawFlock(int(file.Fd()), syscall.LOCK_UN)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// acquire returns the same live file handle (held open) for a family, flocking
// it. The per-process map means one OPEN of each family's lock file, not one
// open+flock per put; the flock is what serializes across processes, and
// flock on an already-open fd is cheap.
func (s *shardedAppendLock) acquire(family string) (*appendLock, error) {
	if s.bindErr != nil {
		return nil, s.bindErr
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, fmt.Errorf("create event segment root: %w", err)
	}
	if !s.gate.TryRLock() {
		if s.waiting != nil {
			s.waiting(family)
		}
		s.gate.RLock()
	}
	lock, err := s.acquireFamily(family)
	if err != nil {
		s.gate.RUnlock()
		return nil, err
	}
	lock.gate = &s.gate
	return lock, nil
}

// release unlocks the family's held file without closing it (the handle stays
// cached for the next append). Distinct from the plain appendLock.release,
// which closes — used by the non-sharded paths (rebaseline, scan) that open
// fresh each time.
func (s *shardedAppendLock) release(family string, l *appendLock) error {
	return s.releaseHeld(family, l)
}

func (s *shardedAppendLock) releaseHeld(family string, l *appendLock) error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := s.unlock(l.file)
	var closeErr error
	if unlockErr != nil {
		// Keep both local exclusion layers held until the failed kernel lock is
		// retired. Only remove this exact descriptor from this family's cache;
		// another generation, if one ever appears, remains authoritative.
		s.mu.Lock()
		if s.fd[family] == l.file {
			delete(s.fd, family)
		}
		closeErr = l.file.Close()
		s.mu.Unlock()
	}
	if l.processMu != nil {
		l.processMu.Unlock()
	}
	if l.gate != nil {
		l.gate.RUnlock()
	}
	var wrappedUnlockErr error
	if unlockErr != nil {
		wrappedUnlockErr = fmt.Errorf("unlock event append stream: %w", unlockErr)
	}
	var wrappedCloseErr error
	if closeErr != nil {
		wrappedCloseErr = fmt.Errorf("close event append lock after failed unlock: %w", closeErr)
	}
	return errors.Join(wrappedUnlockErr, wrappedCloseErr)
}

// acquireAll locks every family shard in sorted order (deadlock-free across
// concurrent callers) and returns a release func. Maintenance paths that can
// rewrite more than one family's segments (engine.remove) use this so they stay
// mutually exclusive with put's per-family appends — a remove must never
// truncate a segment while a put appends to it.
func (s *shardedAppendLock) acquireAll(families []string) (func() error, error) {
	names := append([]string(nil), families...)
	sort.Strings(names)
	unique := names[:0]
	for _, name := range names {
		if len(unique) == 0 || unique[len(unique)-1] != name {
			unique = append(unique, name)
		}
	}
	names = unique
	type held struct {
		family string
		lock   *appendLock
	}
	var heldLocks []held
	if !s.gate.TryLock() {
		if s.waiting != nil {
			s.waiting(allFamiliesLockScope)
		}
		s.gate.Lock()
	}
	releaseAll := func() (retErr error) {
		for i := len(heldLocks) - 1; i >= 0; i-- {
			held := heldLocks[i]
			if err := s.releaseHeld(held.family, held.lock); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("release event append family %s: %w", held.family, err))
			}
		}
		s.gate.Unlock()
		return retErr
	}
	if s.bindErr != nil {
		return nil, errors.Join(s.bindErr, releaseAll())
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, errors.Join(fmt.Errorf("create event segment root: %w", err), releaseAll())
	}
	for _, family := range names {
		l, err := s.acquireFamily(family)
		if err != nil {
			return nil, errors.Join(err, releaseAll())
		}
		heldLocks = append(heldLocks, held{family: family, lock: l})
	}
	if s.acquired != nil {
		s.acquired(allFamiliesLockScope)
	}
	return releaseAll, nil
}
