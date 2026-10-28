package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var (
	homeMutationGuardFlock = func(fd int, operation int) error {
		return syscall.Flock(fd, operation)
	}
	homeMutationGuardBeforeFlock = func(string) {}
	homeMutationGuardAfterFlock  = func(string) {}
)

type homeMutationProcessLock struct {
	mu   sync.Mutex
	refs int
}

var homeMutationProcessLocks = struct {
	sync.Mutex
	byHome map[string]*homeMutationProcessLock
}{byHome: make(map[string]*homeMutationProcessLock)}

// HomeMutationGuard is a callback-scoped capability proving that one exact
// COMPOUND_HOME generation is pinned and exclusively flocked. It is the
// outermost filesystem mutation lock beneath autonomy admission. Descendant
// mutation guards may rely on it to prevent a renamed authority directory
// from becoming an independently lockable replacement inode.
//
// The guard never creates COMPOUND_HOME or any descendant path. Replacing
// COMPOUND_HOME itself remains the explicit trusted-root boundary: validation
// detects that loss, but no lock inside the old root can fence a new root.
type HomeMutationGuard struct {
	requestedHome string
	canonicalHome string
	generation    os.FileInfo
	opened        os.FileInfo
	anchor        *os.File
	scope         *homeMutationGuardScope
}

type homeMutationGuardScope struct {
	mu       sync.Mutex
	drained  *sync.Cond
	active   bool
	inFlight int
}

// WithHomeMutationGuard runs fn while holding the canonical same-process home
// mutex and the exact COMPOUND_HOME directory flock.
func WithHomeMutationGuard(homePath string, fn func(*HomeMutationGuard) error) error {
	acquired, err := withHomeMutationGuardMode(homePath, false, fn)
	if err != nil {
		return err
	}
	if !acquired {
		return errors.New("home mutation guard: blocking acquisition returned without lock")
	}
	return nil
}

// TryWithHomeMutationGuard invokes fn only when both the canonical
// same-process mutex and the COMPOUND_HOME directory flock can be acquired
// immediately. A busy guard returns (false, nil) without invoking fn.
func TryWithHomeMutationGuard(homePath string, fn func(*HomeMutationGuard) error) (bool, error) {
	return withHomeMutationGuardMode(homePath, true, fn)
}

// Validate proves the callback scope is active and still owns the exact home
// generation selected at admission. Alias paths are accepted only while they
// continue resolving to that same generation.
func (g *HomeMutationGuard) Validate(homePath string) error {
	return g.WithUse(homePath, func() error { return nil })
}

// WithUse retains the exact guard generation for fn. A use that begins while
// the outer callback is active prevents invalidation, home-flock release, and
// anchor close until fn and the final identity check finish. Once invalidation
// begins, new uses fail before fn can mutate anything.
func (g *HomeMutationGuard) WithUse(homePath string, fn func() error) error {
	if fn == nil {
		return errors.New("home mutation guard: use callback required")
	}
	release, err := g.retainUse()
	if err != nil {
		return err
	}
	defer release()
	if err := g.validateIdentity(homePath); err != nil {
		return err
	}

	var callbackErr error
	var panicValue any
	func() {
		defer func() {
			panicValue = recover()
		}()
		callbackErr = fn()
	}()
	validationErr := g.validateIdentity(homePath)
	if panicValue != nil {
		if validationErr != nil {
			panic(errors.Join(
				fmt.Errorf("home mutation guard use callback panic: %v", panicValue),
				validationErr,
			))
		}
		panic(panicValue)
	}
	return errors.Join(callbackErr, validationErr)
}

func (g *HomeMutationGuard) retainUse() (func(), error) {
	if g == nil || g.scope == nil {
		return nil, errors.New("home mutation guard: zero guard")
	}
	g.scope.mu.Lock()
	if !g.scope.active {
		g.scope.mu.Unlock()
		return nil, errors.New("home mutation guard: scope expired")
	}
	g.scope.inFlight++
	g.scope.mu.Unlock()
	return func() {
		g.scope.mu.Lock()
		g.scope.inFlight--
		if g.scope.inFlight == 0 && g.scope.drained != nil {
			g.scope.drained.Broadcast()
		}
		g.scope.mu.Unlock()
	}, nil
}

func (g *HomeMutationGuard) validateIdentity(homePath string) error {
	if g.anchor == nil || g.generation == nil || g.opened == nil {
		return errors.New("home mutation guard: incomplete guard")
	}
	canonical, current, err := prepareHomeMutationIdentity(homePath)
	if err != nil {
		return fmt.Errorf("home mutation guard: revalidate requested home: %w", err)
	}
	selected, _, err := prepareHomeMutationIdentity(g.requestedHome)
	if err != nil {
		return fmt.Errorf("home mutation guard: revalidate selected home: %w", err)
	}
	opened, err := g.anchor.Stat()
	if err != nil {
		return fmt.Errorf("home mutation guard: stat pinned home: %w", err)
	}
	if err := validateWorkAdmissionHomeInfo(opened); err != nil {
		return fmt.Errorf("home mutation guard: unsafe pinned home: %w", err)
	}
	if canonical != g.canonicalHome || selected != g.canonicalHome ||
		!os.SameFile(g.generation, current) ||
		!os.SameFile(g.generation, g.opened) ||
		!os.SameFile(g.generation, opened) {
		return fmt.Errorf("home mutation guard: trusted home identity changed while locked: %s", g.canonicalHome)
	}
	return nil
}

func (g *HomeMutationGuard) invalidate() {
	if g == nil || g.scope == nil {
		return
	}
	g.scope.mu.Lock()
	g.scope.active = false
	for g.scope.inFlight > 0 {
		g.scope.drained.Wait()
	}
	g.scope.mu.Unlock()
}

func withHomeMutationGuardMode(homePath string, nonblocking bool, fn func(*HomeMutationGuard) error) (acquired bool, resultErr error) {
	if fn == nil {
		return false, errors.New("home mutation guard: callback required")
	}
	canonicalHome, generation, err := prepareHomeMutationIdentity(homePath)
	if err != nil {
		return false, err
	}

	releaseProcessLock, processAcquired := acquireHomeMutationProcessLock(canonicalHome, nonblocking)
	if !processAcquired {
		return false, nil
	}
	defer releaseProcessLock()

	anchor, err := os.Open(canonicalHome)
	if err != nil {
		return false, fmt.Errorf("home mutation guard: open trusted home: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, anchor.Close())
	}()
	opened, err := anchor.Stat()
	if err != nil {
		return false, fmt.Errorf("home mutation guard: stat trusted home: %w", err)
	}
	if err := validateWorkAdmissionHomeInfo(opened); err != nil {
		return false, fmt.Errorf("home mutation guard: unsafe trusted home: %w", err)
	}
	if !os.SameFile(generation, opened) {
		return false, fmt.Errorf("home mutation guard: trusted home identity changed while opening: %s", canonicalHome)
	}

	operation := syscall.LOCK_EX
	if nonblocking {
		operation |= syscall.LOCK_NB
	}
	homeMutationGuardBeforeFlock(canonicalHome)
	if err := homeMutationGuardFlock(int(anchor.Fd()), operation); err != nil {
		if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return false, nil
		}
		return false, fmt.Errorf("home mutation guard: flock trusted home %s: %w", canonicalHome, err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, homeMutationGuardFlock(int(anchor.Fd()), syscall.LOCK_UN))
	}()
	homeMutationGuardAfterFlock(canonicalHome)

	scope := &homeMutationGuardScope{active: true}
	scope.drained = sync.NewCond(&scope.mu)
	guard := &HomeMutationGuard{
		requestedHome: homePath,
		canonicalHome: canonicalHome,
		generation:    generation,
		opened:        opened,
		anchor:        anchor,
		scope:         scope,
	}
	defer guard.invalidate()
	if err := guard.Validate(homePath); err != nil {
		return false, err
	}
	var callbackErr error
	var panicValue any
	func() {
		defer func() {
			panicValue = recover()
		}()
		callbackErr = fn(guard)
	}()
	validationErr := guard.Validate(homePath)
	if panicValue != nil {
		if validationErr != nil {
			panic(errors.Join(
				fmt.Errorf("home mutation callback panic: %v", panicValue),
				validationErr,
			))
		}
		panic(panicValue)
	}
	return true, errors.Join(callbackErr, validationErr)
}

func prepareHomeMutationIdentity(homePath string) (string, os.FileInfo, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return "", nil, errors.New("home mutation guard: COMPOUND_HOME required")
	}
	absolute, err := filepath.Abs(filepath.Clean(homePath))
	if err != nil {
		return "", nil, fmt.Errorf("home mutation guard: resolve home: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", nil, fmt.Errorf("home mutation guard: canonicalize home: %w", err)
	}
	canonical := filepath.Clean(resolved)
	generation, err := os.Lstat(canonical)
	if err != nil {
		return "", nil, fmt.Errorf("home mutation guard: inspect canonical home: %w", err)
	}
	if err := validateWorkAdmissionHomeInfo(generation); err != nil {
		return "", nil, fmt.Errorf("home mutation guard: unsafe canonical home: %w", err)
	}
	return canonical, generation, nil
}

func acquireHomeMutationProcessLock(home string, nonblocking bool) (func(), bool) {
	homeMutationProcessLocks.Lock()
	entry := homeMutationProcessLocks.byHome[home]
	if entry == nil {
		entry = &homeMutationProcessLock{}
		homeMutationProcessLocks.byHome[home] = entry
	}
	entry.refs++
	homeMutationProcessLocks.Unlock()

	acquired := false
	if nonblocking {
		acquired = entry.mu.TryLock()
	} else {
		entry.mu.Lock()
		acquired = true
	}
	if !acquired {
		releaseHomeMutationProcessLockRef(home, entry)
		return nil, false
	}
	return func() {
		entry.mu.Unlock()
		releaseHomeMutationProcessLockRef(home, entry)
	}, true
}

func releaseHomeMutationProcessLockRef(home string, entry *homeMutationProcessLock) {
	homeMutationProcessLocks.Lock()
	defer homeMutationProcessLocks.Unlock()
	entry.refs--
	if entry.refs == 0 && homeMutationProcessLocks.byHome[home] == entry {
		delete(homeMutationProcessLocks.byHome, home)
	}
}
