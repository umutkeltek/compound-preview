package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const privateArtifactRelativeDir = "private/artifacts/sha256"

// withPrivateArtifactDirectoryLock excludes cooperative private artifact writers
// and recovery without creating another authority file. A nonblocking caller
// can distinguish a live writer from an abandoned stage.
func withPrivateArtifactDirectoryLock(homePath string, nonblocking bool, fn func(*os.Root) error) (acquired bool, resultErr error) {
	canonical, generation, err := prepareHomeMutationIdentity(homePath)
	if err != nil {
		return false, err
	}
	root, err := openStableOwnedHomeRootPermissive(homePath, privateCustodyHooks{})
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	openedHome, err := root.Stat(".")
	if err != nil || !os.SameFile(generation, openedHome) {
		return false, fmt.Errorf("private artifact home changed while opening: %w", err)
	}
	rel := filepath.FromSlash(privateArtifactRelativeDir)
	display := absDisplay(homePath, rel)
	before, err := lstatRootedDirectoryPath(root, rel, display, "private artifact lock", false)
	if err != nil {
		return false, err
	}
	for _, info := range before {
		if !ownedByCurrent(info) || info.Mode().Perm() != 0o700 {
			return false, fmt.Errorf("private artifact lock requires owned mode=0700 directories: %s", display)
		}
	}
	dir, err := root.Open(rel)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, dir.Close()) }()
	opened, err := dir.Stat()
	if err != nil {
		return false, err
	}
	after, err := lstatRootedDirectoryPath(root, rel, display, "private artifact lock", false)
	if err != nil || !sameRootedPath(before, after) || !os.SameFile(before[len(before)-1], opened) {
		return false, fmt.Errorf("private artifact directory changed while opening: %s: %w", display, err)
	}
	releaseProcess, ok := acquireHomeMutationProcessLock("private-artifact:"+canonical, nonblocking)
	if !ok {
		return false, nil
	}
	defer releaseProcess()
	operation := syscall.LOCK_EX
	if nonblocking {
		operation |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(dir.Fd()), operation); err != nil {
		if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return false, nil
		}
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, syscall.Flock(int(dir.Fd()), syscall.LOCK_UN)) }()
	current, err := lstatRootedDirectoryPath(root, rel, display, "private artifact lock", false)
	if err != nil || !sameRootedPath(before, current) {
		return false, fmt.Errorf("private artifact directory changed while locking: %s: %w", display, err)
	}
	selected, selectedInfo, err := prepareHomeMutationIdentity(homePath)
	if err != nil || selected != canonical || !os.SameFile(generation, selectedInfo) {
		return false, fmt.Errorf("private artifact home changed while locking: %w", err)
	}
	artifactRoot, err := root.OpenRoot(rel)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, artifactRoot.Close()) }()
	artifactInfo, err := artifactRoot.Stat(".")
	if err != nil || !os.SameFile(opened, artifactInfo) {
		return false, fmt.Errorf("private artifact directory changed before callback: %s: %v", display, err)
	}
	resultErr = fn(artifactRoot)
	current, err = lstatRootedDirectoryPath(root, rel, display, "private artifact lock", false)
	if err != nil || !sameRootedPath(before, current) {
		resultErr = errors.Join(resultErr, fmt.Errorf("private artifact directory changed while locked: %s: %w", display, err))
	}
	selected, selectedInfo, err = prepareHomeMutationIdentity(homePath)
	if err != nil || selected != canonical || !os.SameFile(generation, selectedInfo) {
		resultErr = errors.Join(resultErr, fmt.Errorf("private artifact home changed while locked: %w", err))
	}
	return true, resultErr
}
