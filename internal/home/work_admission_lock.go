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

const (
	workAdmissionAuthorityRootRelative = "work"
	workAdmissionDirectoryRelative     = "lock/capability-lifecycle-work-admission"
)

var (
	workAdmissionCurrentUID          = os.Geteuid
	workAdmissionFlock               = func(fd int, operation int) error { return syscall.Flock(fd, operation) }
	workAdmissionBeforeOpen          = func(string) {}
	workAdmissionAfterAuthorityFlock = func(string) {}
	workAdmissionAfterFlock          = func(string) {}
)

type workAdmissionRoot struct {
	root                *os.Root
	requestedHome       string
	canonicalHome       string
	homeInfo            os.FileInfo
	authority           *os.File
	authoritySnapshot   []os.FileInfo
	authorityOpenedInfo os.FileInfo
}

type workAdmissionFile struct {
	file        *os.File
	relative    string
	displayPath string
	snapshot    []os.FileInfo
	openedInfo  os.FileInfo
}

// WorkAdmissionGuard is a callback-scoped proof that the exact work-admission
// lock is currently held. Its fields are deliberately private: callers can
// receive a guard only from WithWorkAdmissionGuard or
// TryWithWorkAdmissionGuard, and a copied or escaped guard shares the same
// expiry state.
type WorkAdmissionGuard struct {
	canonicalHome string
	workID        string
	homeGuard     *HomeMutationGuard
	root          *workAdmissionRoot
	lockFile      *workAdmissionFile
	scope         *workAdmissionGuardScope
}

type workAdmissionGuardScope struct {
	mu       sync.Mutex
	drained  *sync.Cond
	active   bool
	inFlight int
}

// Validate proves that the guard is still active and is bound to the exact
// canonical home and work ID supplied by the caller. It also revalidates the
// pinned home and lock-file custody before the guard can authorize nested work.
func (g *WorkAdmissionGuard) Validate(homePath, workID string) error {
	return g.WithUse(homePath, workID, func() error { return nil })
}

// WithUse retains the exact work-admission generation for fn. A use that
// begins while the outer callback is active prevents invalidation, work/home
// flock release, and descriptor close until fn and the final custody check
// finish. Once invalidation begins, new uses fail before fn can mutate.
func (g *WorkAdmissionGuard) WithUse(homePath, workID string, fn func() error) error {
	if fn == nil {
		return errors.New("work admission guard: use callback required")
	}
	release, err := g.retainUse()
	if err != nil {
		return err
	}
	defer release()
	if err := g.validateIdentity(homePath, workID); err != nil {
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
	validationErr := g.validateIdentity(homePath, workID)
	if panicValue != nil {
		if validationErr != nil {
			panic(errors.Join(
				fmt.Errorf("work admission guard use callback panic: %v", panicValue),
				validationErr,
			))
		}
		panic(panicValue)
	}
	return errors.Join(callbackErr, validationErr)
}

func (g *WorkAdmissionGuard) retainUse() (func(), error) {
	if g == nil || g.scope == nil {
		return nil, errors.New("work admission guard: zero guard")
	}
	g.scope.mu.Lock()
	if !g.scope.active {
		g.scope.mu.Unlock()
		return nil, errors.New("work admission guard: scope expired")
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

func (g *WorkAdmissionGuard) validateIdentity(homePath, workID string) error {
	if g == nil || g.scope == nil {
		return errors.New("work admission guard: zero guard")
	}
	canonicalHome, _, _, err := workAdmissionLockIdentity(homePath, workID)
	if err != nil {
		return fmt.Errorf("work admission guard: validate identity: %w", err)
	}
	if canonicalHome != g.canonicalHome {
		return fmt.Errorf("work admission guard: canonical home mismatch: got %s", canonicalHome)
	}
	if workID != g.workID {
		return fmt.Errorf("work admission guard: work id mismatch: got %q", workID)
	}
	if g.root == nil || g.lockFile == nil {
		return errors.New("work admission guard: incomplete guard")
	}
	if g.homeGuard == nil {
		return errors.New("work admission guard: missing home mutation guard")
	}
	if err := g.homeGuard.Validate(homePath); err != nil {
		return fmt.Errorf("work admission guard: revalidate home mutation guard: %w", err)
	}
	if err := g.root.verifyLockFile(g.lockFile); err != nil {
		return fmt.Errorf("work admission guard: revalidate held lock: %w", err)
	}
	return nil
}

// HomeMutationGuard returns the exact active outer guard only after the full
// work-admission custody proof has been revalidated. Nested mutation owners
// use this capability to retain their legacy leaf locks without recursively
// acquiring COMPOUND_HOME's non-reentrant directory flock.
func (g *WorkAdmissionGuard) HomeMutationGuard(homePath, workID string) (*HomeMutationGuard, error) {
	if err := g.Validate(homePath, workID); err != nil {
		return nil, err
	}
	return g.homeGuard, nil
}

func (g *WorkAdmissionGuard) invalidate() {
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

// WithWorkAdmissionLock serializes liveness and lifecycle mutations for one
// work ID across goroutines and processes. The path deliberately preserves the
// historical capability-lifecycle admission inode so mixed installed binaries
// continue to exclude each other during the cutover.
// v0.1 correctness contract: a pinned, exclusively flocked
// COMPOUND_HOME/work authority inode serializes all work IDs. Cross-work
// non-convoy is intentionally outside the public support contract. The
// callback is non-reentrant, and this function never materializes authority.
func WithWorkAdmissionLock(homePath, workID string, fn func() error) error {
	acquired, err := withWorkAdmissionLockMode(homePath, workID, false, fn)
	if err != nil {
		return err
	}
	if !acquired {
		return errors.New("work admission lock: blocking acquisition returned without lock")
	}
	return nil
}

// TryWithWorkAdmissionLock invokes fn only when the work-key lock can be
// acquired immediately. A busy lock returns (false, nil).
// Any active work admission, including a different work ID, reports busy.
func TryWithWorkAdmissionLock(homePath, workID string, fn func() error) (bool, error) {
	return withWorkAdmissionLockMode(homePath, workID, true, fn)
}

// EnsureWorkAdmissionAuthorityRoot materializes the home's work authority
// directory (<home>/work) when absent so a fresh home can still acquire the
// work admission lock. The lock itself never materializes authority
// (deliberate design: the authority inode must already exist for the
// identity check the lock relies on). Idempotent and safe to call
// concurrently with an existing home; an already-materialized authority
// directory that fails safety validation produces an error so a planted
// insecure path cannot silently escape materialization.
//
// Mode 0700 mirrors the durable home invariant that the admission core
// revalidates (validateWorkAdmissionHomeInfo); materialization here only
// creates the leaf, the lock still pins and validates it before holding it.
func EnsureWorkAdmissionAuthorityRoot(homePath string) error {
	if strings.TrimSpace(homePath) == "" {
		return errors.New("work admission: home path required for authority root")
	}
	canonicalHome, err := canonicalWorkAdmissionHome(homePath)
	if err != nil {
		return err
	}
	target := filepath.Join(canonicalHome, workAdmissionAuthorityRootRelative)
	info, err := os.Lstat(target)
	if err == nil {
		if err := validateWorkAdmissionHomeInfo(info); err != nil {
			return fmt.Errorf("work admission: authority root %s unsafe: %w", target, err)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("work admission: stat authority root %s: %w", target, err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("work admission: create authority root %s: %w", target, err)
		}
	}
	return nil
}

// WithWorkAdmissionGuard serializes one work ID and supplies an unforgeable
// guard that is valid only for the duration of fn.
func WithWorkAdmissionGuard(homePath, workID string, fn func(*WorkAdmissionGuard) error) error {
	acquired, err := withWorkAdmissionGuardMode(homePath, workID, false, fn)
	if err != nil {
		return err
	}
	if !acquired {
		return errors.New("work admission guard: blocking acquisition returned without lock")
	}
	return nil
}

// TryWithWorkAdmissionGuard invokes fn with an active guard only when the
// exact work-admission lock can be acquired immediately. A busy lock returns
// (false, nil) without creating a guard or invoking fn.
func TryWithWorkAdmissionGuard(homePath, workID string, fn func(*WorkAdmissionGuard) error) (bool, error) {
	return withWorkAdmissionGuardMode(homePath, workID, true, fn)
}

func withWorkAdmissionLockMode(homePath, workID string, nonblocking bool, fn func() error) (acquired bool, err error) {
	if fn == nil {
		return withWorkAdmissionLockCore(homePath, workID, nonblocking, nil)
	}
	return withWorkAdmissionLockCore(homePath, workID, nonblocking, func(_ string, _ *HomeMutationGuard, _ *workAdmissionRoot, _ *workAdmissionFile) error {
		return fn()
	})
}

func withWorkAdmissionGuardMode(homePath, workID string, nonblocking bool, fn func(*WorkAdmissionGuard) error) (acquired bool, err error) {
	if fn == nil {
		return withWorkAdmissionLockCore(homePath, workID, nonblocking, nil)
	}
	return withWorkAdmissionLockCore(homePath, workID, nonblocking, func(canonicalHome string, homeGuard *HomeMutationGuard, root *workAdmissionRoot, lockFile *workAdmissionFile) error {
		scope := &workAdmissionGuardScope{active: true}
		scope.drained = sync.NewCond(&scope.mu)
		guard := &WorkAdmissionGuard{
			canonicalHome: canonicalHome,
			workID:        workID,
			homeGuard:     homeGuard,
			root:          root,
			lockFile:      lockFile,
			scope:         scope,
		}
		defer guard.invalidate()
		return fn(guard)
	})
}

type workAdmissionLockCallback func(canonicalHome string, homeGuard *HomeMutationGuard, root *workAdmissionRoot, lockFile *workAdmissionFile) error

func withWorkAdmissionLockCore(homePath, workID string, nonblocking bool, fn workAdmissionLockCallback) (acquired bool, err error) {
	if _, _, _, err := workAdmissionLockIdentity(homePath, workID); err != nil {
		return false, err
	}
	if fn == nil {
		return false, errors.New("work admission lock: callback required")
	}
	innerAcquired := false
	outerAcquired, err := withHomeMutationGuardMode(homePath, nonblocking, func(homeGuard *HomeMutationGuard) error {
		if err := workAdmissionRecoveryFence(homePath); err != nil {
			return err
		}
		var innerErr error
		innerAcquired, innerErr = withWorkAdmissionLockCoreAdmitted(homeGuard, homePath, workID, nonblocking, fn)
		return innerErr
	})
	if err != nil {
		return innerAcquired, err
	}
	if !outerAcquired {
		return false, nil
	}
	return innerAcquired, nil
}

func withWorkAdmissionLockCoreAdmitted(homeGuard *HomeMutationGuard, homePath, workID string, nonblocking bool, fn workAdmissionLockCallback) (acquired bool, err error) {
	canonicalHome, relative, _, err := workAdmissionLockIdentity(homePath, workID)
	if err != nil {
		return false, err
	}
	if fn == nil {
		return false, errors.New("work admission lock: callback required")
	}
	if err := homeGuard.Validate(homePath); err != nil {
		return false, fmt.Errorf("work admission lock: validate home mutation guard: %w", err)
	}

	root, err := openWorkAdmissionRoot(homePath, canonicalHome)
	if err != nil {
		return false, err
	}
	defer func() {
		err = root.close(err)
	}()
	authorityOperation := syscall.LOCK_EX
	if nonblocking {
		authorityOperation |= syscall.LOCK_NB
	}
	if err := workAdmissionFlock(int(root.authority.Fd()), authorityOperation); err != nil {
		if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return false, nil
		}
		return false, fmt.Errorf(
			"work admission lock: flock authority root %s: %w",
			root.display(workAdmissionAuthorityRootRelative),
			err,
		)
	}
	defer func() {
		err = errors.Join(err, workAdmissionFlock(int(root.authority.Fd()), syscall.LOCK_UN))
	}()
	workAdmissionAfterAuthorityFlock(root.display(workAdmissionAuthorityRootRelative))
	if err := root.verifyAuthorityRoot(); err != nil {
		return false, err
	}
	if err := root.ensureDirectory("lock", 0o755); err != nil {
		return false, err
	}
	if err := root.ensureDirectory(workAdmissionDirectoryRelative, 0o755); err != nil {
		return false, err
	}
	lockFile, err := root.openLockFile(relative)
	if err != nil {
		return false, err
	}

	operation := syscall.LOCK_EX
	if nonblocking {
		operation |= syscall.LOCK_NB
	}
	if err := workAdmissionFlock(int(lockFile.file.Fd()), operation); err != nil {
		closeErr := lockFile.file.Close()
		if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return false, closeErr
		}
		return false, errors.Join(
			fmt.Errorf("work admission lock: flock %s: %w", lockFile.displayPath, err),
			closeErr,
		)
	}
	defer func() {
		unlockErr := workAdmissionFlock(int(lockFile.file.Fd()), syscall.LOCK_UN)
		closeErr := lockFile.file.Close()
		err = errors.Join(err, unlockErr, closeErr)
	}()

	workAdmissionAfterFlock(lockFile.displayPath)
	if err := root.verifyLockFile(lockFile); err != nil {
		return false, err
	}
	var callbackErr error
	var panicValue any
	func() {
		defer func() {
			panicValue = recover()
		}()
		callbackErr = fn(canonicalHome, homeGuard, root, lockFile)
	}()
	custodyErr := root.verifyLockFile(lockFile)
	if custodyErr != nil {
		recoveryErr := publishWorkAdmissionRecoveryRequired(root, workID, custodyErr)
		if panicValue != nil {
			panic(errors.Join(
				fmt.Errorf("work admission callback panic: %v", panicValue),
				fmt.Errorf("work admission recovery_required for %s: %w", workID, custodyErr),
				recoveryErr,
			))
		}
		return true, errors.Join(
			callbackErr,
			fmt.Errorf("work admission recovery_required for %s: %w", workID, custodyErr),
			recoveryErr,
		)
	}
	if panicValue != nil {
		panic(panicValue)
	}
	return true, callbackErr
}

func workAdmissionLockIdentity(homePath, workID string) (canonicalHome, relative, key string, err error) {
	homePath = strings.TrimSpace(homePath)
	canonicalWorkID := strings.TrimSpace(workID)
	if homePath == "" {
		return "", "", "", errors.New("work admission lock: COMPOUND_HOME required")
	}
	if canonicalWorkID == "" || canonicalWorkID != workID ||
		canonicalWorkID == "." || canonicalWorkID == ".." ||
		filepath.Base(canonicalWorkID) != canonicalWorkID {
		return "", "", "", fmt.Errorf("work admission lock: invalid work id %q", workID)
	}
	canonicalHome, err = canonicalWorkAdmissionHome(homePath)
	if err != nil {
		return "", "", "", err
	}
	relative = filepath.Join(workAdmissionDirectoryRelative, canonicalWorkID+".lock")
	if !filepath.IsLocal(relative) {
		return "", "", "", fmt.Errorf("work admission lock: path escapes pinned home: %s", relative)
	}
	key = filepath.Join(canonicalHome, relative)
	return canonicalHome, relative, key, nil
}

func canonicalWorkAdmissionHome(homePath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(homePath)
	if err != nil {
		return "", fmt.Errorf("work admission lock: canonicalize home: %w", err)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("work admission lock: absolute home: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func openWorkAdmissionRoot(requestedHome, canonicalHome string) (*workAdmissionRoot, error) {
	before, err := os.Lstat(canonicalHome)
	if err != nil {
		return nil, fmt.Errorf("work admission lock: inspect home %s: %w", canonicalHome, err)
	}
	if err := validateWorkAdmissionHomeInfo(before); err != nil {
		return nil, fmt.Errorf("work admission lock: unsafe home %s: %w", canonicalHome, err)
	}
	root, err := os.OpenRoot(canonicalHome)
	if err != nil {
		return nil, fmt.Errorf("work admission lock: open rooted home %s: %w", canonicalHome, err)
	}
	opened, openErr := root.Stat(".")
	after, afterErr := os.Lstat(canonicalHome)
	if err := errors.Join(openErr, afterErr); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("work admission lock: pin home %s: %w", canonicalHome, err)
	}
	if err := errors.Join(validateWorkAdmissionHomeInfo(opened), validateWorkAdmissionHomeInfo(after)); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("work admission lock: unsafe pinned home %s: %w", canonicalHome, err)
	}
	if !os.SameFile(before, opened) || !os.SameFile(before, after) {
		_ = root.Close()
		return nil, fmt.Errorf("work admission lock: home identity changed while opening: %s", canonicalHome)
	}
	resolved, err := canonicalWorkAdmissionHome(requestedHome)
	if err != nil || resolved != canonicalHome {
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("work admission lock: selected home changed from %s to %s", canonicalHome, resolved)
	}
	pinned := &workAdmissionRoot{
		root:          root,
		requestedHome: requestedHome,
		canonicalHome: canonicalHome,
		homeInfo:      before,
	}
	authorityBefore, err := pinned.snapshotDirectory(workAdmissionAuthorityRootRelative)
	if err != nil {
		return nil, pinned.close(fmt.Errorf(
			"work admission lock: authority root %s must be materialized and safe: %w",
			pinned.display(workAdmissionAuthorityRootRelative),
			err,
		))
	}
	authority, err := root.Open(workAdmissionAuthorityRootRelative)
	if err != nil {
		return nil, pinned.close(fmt.Errorf(
			"work admission lock: open authority root %s: %w",
			pinned.display(workAdmissionAuthorityRootRelative),
			err,
		))
	}
	authorityOpened, statErr := authority.Stat()
	authorityAfter, snapshotErr := pinned.snapshotDirectory(workAdmissionAuthorityRootRelative)
	if err := errors.Join(statErr, snapshotErr); err != nil {
		_ = authority.Close()
		return nil, pinned.close(fmt.Errorf("work admission lock: pin authority root: %w", err))
	}
	if !authorityOpened.IsDir() || !sameWorkAdmissionSnapshot(authorityBefore, authorityAfter) ||
		!os.SameFile(authorityAfter[len(authorityAfter)-1], authorityOpened) {
		_ = authority.Close()
		return nil, pinned.close(fmt.Errorf(
			"work admission lock: authority root identity changed while opening: %s",
			pinned.display(workAdmissionAuthorityRootRelative),
		))
	}
	pinned.authority = authority
	pinned.authoritySnapshot = authorityAfter
	pinned.authorityOpenedInfo = authorityOpened
	return pinned, nil
}

func validateWorkAdmissionHomeInfo(info os.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("home must be a non-symlink directory")
	}
	if !workAdmissionOwnedByCurrentUID(info) {
		return fmt.Errorf("home must be owned by current uid %d", workAdmissionCurrentUID())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("home must not be group/world writable: mode=%04o", info.Mode().Perm())
	}
	return nil
}

func workAdmissionOwnedByCurrentUID(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == workAdmissionCurrentUID()
}

func workAdmissionSingleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Nlink) == 1
}

func (r *workAdmissionRoot) close(cause error) error {
	if r == nil {
		return cause
	}
	var authorityCloseErr error
	if r.authority != nil {
		authorityCloseErr = r.authority.Close()
		r.authority = nil
		if authorityCloseErr != nil {
			authorityCloseErr = fmt.Errorf("work admission lock: close authority root: %w", authorityCloseErr)
		}
	}
	var rootCloseErr error
	if r.root != nil {
		rootCloseErr = r.root.Close()
		r.root = nil
		if rootCloseErr != nil {
			rootCloseErr = fmt.Errorf("work admission lock: close rooted home: %w", rootCloseErr)
		}
	}
	return errors.Join(cause, authorityCloseErr, rootCloseErr)
}

func (r *workAdmissionRoot) display(relative string) string {
	return filepath.Join(r.canonicalHome, relative)
}

func (r *workAdmissionRoot) verifyHomeIdentity() error {
	opened, openErr := r.root.Stat(".")
	current, currentErr := os.Lstat(r.canonicalHome)
	resolved, resolveErr := canonicalWorkAdmissionHome(r.requestedHome)
	if err := errors.Join(openErr, currentErr, resolveErr); err != nil {
		return fmt.Errorf("work admission lock: revalidate home %s: %w", r.canonicalHome, err)
	}
	if err := errors.Join(validateWorkAdmissionHomeInfo(opened), validateWorkAdmissionHomeInfo(current)); err != nil {
		return err
	}
	if resolved != r.canonicalHome || !os.SameFile(r.homeInfo, opened) || !os.SameFile(r.homeInfo, current) {
		return fmt.Errorf("work admission lock: home identity changed while locked: %s", r.canonicalHome)
	}
	return nil
}

// verifyAuthorityRoot proves the pinned work authority-directory inode is
// still the exact path generation selected before flock. v0.1 deliberately
// serializes every work mutation through this stable inode so replacement of
// lock descendants cannot mint a second admission path. Replacing the work
// authority root itself while held is unsupported: this check detects it, but
// an anchor inside the old inode cannot exclude a process using a new inode.
func (r *workAdmissionRoot) verifyAuthorityRoot() error {
	if err := r.verifyHomeIdentity(); err != nil {
		return err
	}
	if r.authority == nil || len(r.authoritySnapshot) == 0 || r.authorityOpenedInfo == nil {
		return errors.New("work admission lock: authority root custody is incomplete")
	}
	opened, statErr := r.authority.Stat()
	current, pathErr := r.snapshotDirectory(workAdmissionAuthorityRootRelative)
	if err := errors.Join(statErr, pathErr); err != nil {
		return fmt.Errorf("work admission lock: revalidate authority root: %w", err)
	}
	if !opened.IsDir() || !sameWorkAdmissionSnapshot(r.authoritySnapshot, current) ||
		!os.SameFile(r.authorityOpenedInfo, opened) || !os.SameFile(opened, current[len(current)-1]) {
		return fmt.Errorf(
			"work admission lock: authority root identity changed while locked: %s",
			r.display(workAdmissionAuthorityRootRelative),
		)
	}
	return nil
}

func (r *workAdmissionRoot) snapshotDirectory(relative string) ([]os.FileInfo, error) {
	relative = filepath.Clean(relative)
	if relative == "." {
		info, err := r.root.Stat(".")
		if err != nil {
			return nil, err
		}
		if err := validateWorkAdmissionHomeInfo(info); err != nil {
			return nil, err
		}
		return []os.FileInfo{info}, nil
	}
	if !filepath.IsLocal(relative) {
		return nil, fmt.Errorf("work admission lock: path escapes pinned home: %s", relative)
	}
	components := strings.Split(relative, string(filepath.Separator))
	infos := make([]os.FileInfo, 0, len(components))
	for index := range components {
		componentPath := filepath.Join(components[:index+1]...)
		info, err := r.root.Lstat(componentPath)
		if err != nil {
			return nil, err
		}
		if err := validateWorkAdmissionDirectory(componentPath, info); err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func validateWorkAdmissionDirectory(relative string, info os.FileInfo) error {
	display := filepath.ToSlash(relative)
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("work admission lock: %s must be a non-symlink directory", display)
	}
	if display != workAdmissionAuthorityRootRelative && display != "lock" && display != workAdmissionDirectoryRelative {
		return fmt.Errorf("work admission lock: unowned lock directory %s", display)
	}
	if !workAdmissionOwnedByCurrentUID(info) {
		return fmt.Errorf("work admission lock: %s must be owned by current uid %d", display, workAdmissionCurrentUID())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("work admission lock: %s must not be group/world writable: mode=%04o", display, info.Mode().Perm())
	}
	return nil
}

func validateWorkAdmissionFile(relative string, info os.FileInfo) error {
	display := filepath.ToSlash(relative)
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("work admission lock: %s must be a non-symlink regular file", display)
	}
	if !workAdmissionOwnedByCurrentUID(info) {
		return fmt.Errorf("work admission lock: %s must be owned by current uid %d", display, workAdmissionCurrentUID())
	}
	mode := info.Mode().Perm()
	if mode&0o600 != 0o600 || mode&0o133 != 0 {
		return fmt.Errorf("work admission lock: %s has unsafe mode=%04o", display, mode)
	}
	if !workAdmissionSingleLink(info) {
		return fmt.Errorf("work admission lock: %s must have exactly one hard link", display)
	}
	return nil
}

func (r *workAdmissionRoot) snapshotFile(relative string) ([]os.FileInfo, error) {
	parent := filepath.Dir(relative)
	infos, err := r.snapshotDirectory(parent)
	if err != nil {
		return nil, err
	}
	info, err := r.root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if err := validateWorkAdmissionFile(relative, info); err != nil {
		return nil, err
	}
	return append(infos, info), nil
}

func sameWorkAdmissionSnapshot(before, after []os.FileInfo) bool {
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

func (r *workAdmissionRoot) ensureDirectory(relative string, mode os.FileMode) error {
	if _, err := r.snapshotDirectory(relative); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(relative)
	parentBefore, err := r.snapshotDirectory(parent)
	if err != nil {
		return err
	}
	created := false
	if err := r.root.Mkdir(relative, mode); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("work admission lock: create directory %s: %w", r.display(relative), err)
		}
	} else {
		created = true
	}
	if _, err := r.snapshotDirectory(relative); err != nil {
		return err
	}
	parentAfter, err := r.snapshotDirectory(parent)
	if err != nil {
		return err
	}
	if !sameWorkAdmissionSnapshot(parentBefore, parentAfter) {
		return fmt.Errorf("work admission lock: parent identity changed while creating %s", r.display(relative))
	}
	if created {
		return r.syncDirectory(parent)
	}
	return nil
}

func (r *workAdmissionRoot) syncDirectory(relative string) error {
	before, err := r.snapshotDirectory(relative)
	if err != nil {
		return err
	}
	directory, err := r.root.Open(relative)
	if err != nil {
		return err
	}
	opened, statErr := directory.Stat()
	after, afterErr := r.snapshotDirectory(relative)
	if err := errors.Join(statErr, afterErr); err != nil {
		_ = directory.Close()
		return err
	}
	if !sameWorkAdmissionSnapshot(before, after) || !opened.IsDir() || !os.SameFile(before[len(before)-1], opened) {
		_ = directory.Close()
		return fmt.Errorf("work admission lock: directory identity changed before sync: %s", r.display(relative))
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if errors.Is(syncErr, syscall.EINVAL) {
		syncErr = nil
	}
	return errors.Join(syncErr, closeErr)
}

func (r *workAdmissionRoot) openLockFile(relative string) (*workAdmissionFile, error) {
	parent := filepath.Dir(relative)
	parentBefore, err := r.snapshotDirectory(parent)
	if err != nil {
		return nil, err
	}
	before, err := r.snapshotFile(relative)
	created := false
	var file *os.File
	if errors.Is(err, os.ErrNotExist) {
		file, err = r.root.OpenFile(relative, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
		if err == nil {
			created = true
		} else if errors.Is(err, os.ErrExist) {
			before, err = r.snapshotFile(relative)
			if err == nil {
				workAdmissionBeforeOpen(r.display(relative))
				file, err = r.root.OpenFile(relative, os.O_RDWR, 0)
			}
		}
	} else if err == nil {
		workAdmissionBeforeOpen(r.display(relative))
		file, err = r.root.OpenFile(relative, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("work admission lock: open %s: %w", r.display(relative), err)
	}
	opened, statErr := file.Stat()
	after, afterErr := r.snapshotFile(relative)
	if err := errors.Join(statErr, afterErr); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := validateWorkAdmissionFile(relative, opened); err != nil {
		_ = file.Close()
		return nil, err
	}
	if before != nil && !created && !sameWorkAdmissionSnapshot(before, after) {
		_ = file.Close()
		return nil, fmt.Errorf("work admission lock: path identity changed while opening %s", r.display(relative))
	}
	if !os.SameFile(after[len(after)-1], opened) {
		_ = file.Close()
		return nil, fmt.Errorf("work admission lock: file identity changed while opening %s", r.display(relative))
	}
	if !sameWorkAdmissionSnapshot(parentBefore, after[:len(after)-1]) {
		_ = file.Close()
		return nil, fmt.Errorf("work admission lock: parent identity changed while opening %s", r.display(relative))
	}
	if created {
		if err := r.syncDirectory(parent); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("work admission lock: sync new lock file %s: %w", r.display(relative), err)
		}
	}
	return &workAdmissionFile{
		file:        file,
		relative:    relative,
		displayPath: r.display(relative),
		snapshot:    after,
		openedInfo:  opened,
	}, nil
}

func (r *workAdmissionRoot) verifyLockFile(lockFile *workAdmissionFile) error {
	if err := r.verifyAuthorityRoot(); err != nil {
		return err
	}
	opened, statErr := lockFile.file.Stat()
	current, pathErr := r.snapshotFile(lockFile.relative)
	if err := errors.Join(statErr, pathErr); err != nil {
		return err
	}
	if err := validateWorkAdmissionFile(lockFile.relative, opened); err != nil {
		return err
	}
	if !sameWorkAdmissionSnapshot(lockFile.snapshot, current) ||
		!os.SameFile(lockFile.openedInfo, opened) ||
		!os.SameFile(opened, current[len(current)-1]) {
		return fmt.Errorf("work admission lock: locked path identity changed: %s", lockFile.displayPath)
	}
	return nil
}
