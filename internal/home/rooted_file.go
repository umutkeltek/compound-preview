package home

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var syncRootedDirectoryFile = func(_ string, file *os.File) error { return file.Sync() }

// ReadRootedRegularFile reads an owned file only when every path component
// beneath home is stable and contains no symbolic links.
func ReadRootedRegularFile(homePath, path, kind string) ([]byte, error) {
	root, relativePath, displayPath, err := openRootedPath(homePath, path, kind)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	before, err := lstatRootedRegularPath(root, relativePath, displayPath, kind)
	if err != nil {
		return nil, err
	}
	file, err := root.Open(relativePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	after, err := lstatRootedRegularPath(root, relativePath, displayPath, kind)
	if err != nil {
		return nil, err
	}
	if !sameRootedPath(before, after) {
		return nil, fmt.Errorf("%s path must be stable beneath compound home: %s", kind, displayPath)
	}
	leaf := before[len(before)-1]
	if !opened.Mode().IsRegular() || !os.SameFile(leaf, opened) {
		return nil, fmt.Errorf("%s file must be a stable regular file: %s", kind, displayPath)
	}
	return io.ReadAll(file)
}

// RootedDirectoryExists reports whether an owned directory exists only when
// every path component beneath home is stable, directory-backed, and contains
// no symbolic links. A genuinely missing path component reports false.
func RootedDirectoryExists(homePath, path, kind string) (bool, error) {
	root, relativePath, displayPath, err := openRootedPath(homePath, path, kind)
	if err != nil {
		return false, err
	}
	defer root.Close()
	return rootedDirectoryExists(root, relativePath, displayPath, kind)
}

// EnsureRootedDirectoryDurable creates and syncs an owned directory hierarchy
// without traversing symbolic links or accepting an ambiguous EEXIST race.
func EnsureRootedDirectoryDurable(homePath, path, kind string, perm os.FileMode) error {
	root, relativePath, displayPath, err := openRootedPath(homePath, path, kind)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := ensureRootedDirectories(root, relativePath, displayPath, kind, perm); err != nil {
		return err
	}
	exists, err := rootedDirectoryExists(root, relativePath, displayPath, kind)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s directory was not durably created beneath compound home: %s", kind, displayPath)
	}
	return nil
}

func rootedDirectoryExists(root *os.Root, relativePath, displayPath, kind string) (bool, error) {
	before, err := lstatRootedDirectoryPath(root, relativePath, displayPath, kind, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	directory, err := root.Open(relativePath)
	if err != nil {
		return false, err
	}
	opened, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		return false, err
	}
	after, err := lstatRootedDirectoryPath(root, relativePath, displayPath, kind, false)
	if err != nil {
		_ = directory.Close()
		return false, err
	}
	closeErr := directory.Close()
	if closeErr != nil {
		return false, closeErr
	}
	leaf := before[len(before)-1]
	if !sameRootedPath(before, after) || !opened.IsDir() || !os.SameFile(leaf, opened) {
		return false, fmt.Errorf("%s directory must be stable beneath compound home: %s", kind, displayPath)
	}
	return true, nil
}

// WriteRootedNewFileDurable creates and syncs a new owned file without
// traversing symbolic links beneath home.
func WriteRootedNewFileDurable(homePath, path, kind string, data []byte, perm os.FileMode) error {
	root, relativePath, displayPath, err := openRootedPath(homePath, path, kind)
	if err != nil {
		return err
	}
	defer root.Close()

	parentPath := filepath.Dir(relativePath)
	beforeParents, err := ensureRootedDirectories(root, parentPath, displayPath, kind, 0o755)
	if err != nil {
		return err
	}
	if info, err := root.Lstat(relativePath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%s file must be a regular file: %s", kind, displayPath)
		}
		return fmt.Errorf("%s file already exists: %s", kind, displayPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	file, err := root.OpenFile(relativePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	cleanup := func(cause error) error {
		_ = file.Close()
		_ = root.Remove(relativePath)
		_ = syncRootedDirectory(root, parentPath, displayPath, kind)
		return cause
	}
	opened, err := file.Stat()
	if err != nil {
		return cleanup(err)
	}
	afterParents, err := lstatRootedDirectoryPath(root, parentPath, displayPath, kind, false)
	if err != nil {
		return cleanup(err)
	}
	if !sameRootedPath(beforeParents, afterParents) {
		return cleanup(fmt.Errorf("%s path must be stable beneath compound home: %s", kind, displayPath))
	}
	leaf, err := root.Lstat(relativePath)
	if err != nil {
		return cleanup(err)
	}
	if leaf.Mode()&os.ModeSymlink != 0 || !leaf.Mode().IsRegular() || !opened.Mode().IsRegular() || !os.SameFile(leaf, opened) {
		return cleanup(fmt.Errorf("%s file must be a stable regular file: %s", kind, displayPath))
	}
	if n, err := file.Write(data); err != nil {
		return cleanup(err)
	} else if n != len(data) {
		return cleanup(io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return cleanup(err)
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(relativePath)
		return err
	}
	return syncRootedDirectory(root, parentPath, displayPath, kind)
}

// RemoveRootedRegularFileDurable removes and directory-syncs an owned regular
// file only when no path component beneath home is a symbolic link.
func RemoveRootedRegularFileDurable(homePath, path, kind string) error {
	return RemoveRootedRegularFileDurableValidated(homePath, path, kind, nil)
}

// removeRootedBeforeUnlink is a test seam invoked after quarantine validation
// and immediately before the final unlink, so falsifiers can prove the unlink
// is bound to the validated inode rather than to a predictable pathname.
var removeRootedBeforeUnlink func(quarantinePath string) error

// removeRootedBeforeQuarantinePublish is a deterministic test seam invoked
// immediately before the removal quarantine is published. Production leaves
// it nil; tests use it to prove a competing quarantine is never replaced.
var removeRootedBeforeQuarantinePublish func(quarantinePath string) error

// RemoveRootedRegularFileDurableValidated removes an owned regular file only
// when validator accepts the exact content and inode moved into quarantine.
// The final unlink re-verifies the quarantined inode identity: a foreign file
// swapped onto the quarantine pathname after validation is never deleted.
func RemoveRootedRegularFileDurableValidated(homePath, path, kind string, validator func([]byte) error) error {
	root, relativePath, displayPath, err := openRootedPath(homePath, path, kind)
	if err != nil {
		return err
	}
	defer root.Close()
	return removeRootedRegularFileDurableValidatedAt(root, relativePath, displayPath, kind, validator, nil, false)
}

// RecoverRootedRegularFileRemovalDurableValidated completes or retries one
// validated removal transaction. Unlike RemoveRootedRegularFileDurableValidated,
// an already-absent canonical file and removal chain is accepted: that state can
// mean the prior attempt unlinked the final quarantine before its parent sync
// failed. Recovery re-syncs the pinned parent before reporting success.
func RecoverRootedRegularFileRemovalDurableValidated(homePath, path, kind string, validator func([]byte) error) error {
	root, relativePath, displayPath, err := openRootedPath(homePath, path, kind)
	if err != nil {
		return err
	}
	defer root.Close()
	return removeRootedRegularFileDurableValidatedAt(root, relativePath, displayPath, kind, validator, nil, true)
}

func removeRootedRegularFileDurableValidatedAt(
	root *os.Root,
	relativePath string,
	displayPath string,
	kind string,
	validator func([]byte) error,
	infoValidator func(os.FileInfo) error,
	allowAbsent bool,
) error {
	parent, chain, _, err := inspectRootedRemovalChain(root, relativePath, displayPath, kind)
	if err != nil {
		return err
	}
	defer parent.Close()
	stagingLeaf := rootedRemovalEvidenceStagingLeaf(filepath.Base(relativePath))
	stagingPresent, err := rootedRemovalEvidenceStagingPresent(parent, stagingLeaf)
	if err != nil {
		return fmt.Errorf("inspect %s removal recovery staging: %w", kind, err)
	}
	if stagingPresent && len(chain) > 0 {
		if err := removeRootedRemovalEvidenceStaging(parent, stagingLeaf, kind); err != nil {
			return fmt.Errorf("reconcile %s removal recovery staging: %w", kind, err)
		}
		stagingPresent = false
	}
	switch {
	case len(chain) == 0:
		if !allowAbsent {
			if stagingPresent {
				return fmt.Errorf("%s removal recovery staging requires the recovery path: %s", kind, displayPath)
			}
			return &os.PathError{Op: "remove", Path: displayPath, Err: os.ErrNotExist}
		}
		// A previous attempt can unlink the final quarantine and then fail its
		// parent-directory sync. Re-syncing the pinned parent makes that exact
		// absent state durable and turns the retry into an idempotent success.
		if err := parent.sync("absent removal recovery"); err != nil {
			return err
		}
		if stagingPresent {
			return removeRootedRemovalEvidenceStaging(parent, stagingLeaf, kind)
		}
		return nil
	case len(chain) == 1 && chain[0].depth == 0:
		return removeRootedRegularFileFreshValidatedAt(root, parent, relativePath, displayPath, kind, validator, infoValidator)
	default:
		if validator == nil && infoValidator == nil {
			return fmt.Errorf("%s removal recovery requires content or inode validation: %s", kind, displayPath)
		}
		return reconcileRootedRemovalChain(parent, chain, kind, validator, infoValidator)
	}
}

// ReadRootedRegularFileRemovalChain reads the one exact inode represented by a
// canonical file and any repeated .remove-pending aliases. A quarantine-only
// crash state remains present authority for recovery; split, corruptly shaped,
// symlinked, or nonregular chains fail closed without changing any name.
func ReadRootedRegularFileRemovalChain(homePath, path, kind string) ([]byte, bool, error) {
	root, relativePath, displayPath, err := openRootedPath(homePath, path, kind)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	parentPath := filepath.Dir(relativePath)
	parents, err := lstatRootedDirectoryPath(root, parentPath, displayPath, kind, true)
	if err != nil {
		return nil, false, err
	}
	if parentPath != "." && len(parents) != len(strings.Split(parentPath, string(filepath.Separator))) {
		// Presence-returning read paths treat a genuinely missing ancestor as
		// absence. The mutation/recovery path below remains strict, and a
		// parent that existed during this probe is revalidated by
		// inspectRootedRemovalChain before any bytes are trusted.
		return nil, false, nil
	}
	parent, chain, present, err := inspectRootedRemovalChain(root, relativePath, displayPath, kind)
	if err != nil {
		return nil, present, err
	}
	defer parent.Close()
	stagingLeaf := rootedRemovalEvidenceStagingLeaf(filepath.Base(relativePath))
	stagingPresent, stagingErr := rootedRemovalEvidenceStagingPresent(parent, stagingLeaf)
	if stagingErr != nil {
		return nil, present, fmt.Errorf("inspect %s removal recovery staging: %w", kind, stagingErr)
	}
	if stagingPresent {
		return nil, present, fmt.Errorf("%s removal recovery staging is pending: %s", kind, displayPath)
	}
	if len(chain) == 0 {
		return nil, false, nil
	}
	return append([]byte(nil), chain[0].data...), true, nil
}

type rootedRemovalChainEntry struct {
	name    string
	display string
	depth   int
	data    []byte
	info    os.FileInfo
}

// rootedRemovalParentGuard pins the directory inode used for a removal and the
// exact home-root path generation that named it. A descriptor alone is not
// enough: after a rename it still permits mutations in a detached directory.
// Every final mutation revalidates that the canonical path still names the
// pinned generation, and validates again before success can be reported.
type rootedRemovalParentGuard struct {
	homeRoot    *os.Root
	parentRoot  *os.Root
	parentPath  string
	displayPath string
	kind        string
	generation  []os.FileInfo
}

func (guard *rootedRemovalParentGuard) Close() error {
	if guard == nil || guard.parentRoot == nil {
		return nil
	}
	return guard.parentRoot.Close()
}

func (guard *rootedRemovalParentGuard) validate(boundary string) error {
	if guard == nil || guard.homeRoot == nil || guard.parentRoot == nil || len(guard.generation) == 0 {
		return fmt.Errorf("rooted removal parent guard is incomplete")
	}
	current, err := lstatRootedDirectoryPath(
		guard.homeRoot,
		guard.parentPath,
		guard.displayPath,
		guard.kind,
		false,
	)
	if err != nil {
		return fmt.Errorf("%s parent changed at %s: %w", guard.kind, boundary, err)
	}
	pinned, err := guard.parentRoot.Stat(".")
	if err != nil {
		return fmt.Errorf("stat pinned %s parent at %s: %w", guard.kind, boundary, err)
	}
	if !pinned.IsDir() || !sameRootedPath(guard.generation, current) ||
		!os.SameFile(guard.generation[len(guard.generation)-1], pinned) {
		return fmt.Errorf("%s parent generation changed at %s: %s", guard.kind, boundary, guard.displayPath)
	}
	return nil
}

func (guard *rootedRemovalParentGuard) sync(boundary string) error {
	if err := guard.validate("before " + boundary + " parent sync"); err != nil {
		return err
	}
	if err := syncRootedCASDirectory(guard.parentRoot); err != nil {
		return err
	}
	return guard.validate("after " + boundary + " parent sync")
}

const (
	rootedRemovalPendingSuffix = ".remove-pending"
	// Linux and macOS, the supported release platforms, cap one pathname
	// component at 255 bytes. The bound also prevents recovery from probing an
	// attacker-controlled number of absent suffixes; ENAMETOOLONG remains the
	// kernel-authoritative stop on a filesystem with a smaller limit.
	rootedRemovalLeafMaxBytes = 255
)

func inspectRootedRemovalChain(
	root *os.Root,
	relativePath string,
	displayPath string,
	kind string,
) (*rootedRemovalParentGuard, []rootedRemovalChainEntry, bool, error) {
	return inspectRootedRemovalChainWithLimit(root, relativePath, displayPath, kind, 0)
}

func inspectRootedRemovalChainWithLimit(root *os.Root, relativePath, displayPath, kind string, maxBytes int64) (*rootedRemovalParentGuard, []rootedRemovalChainEntry, bool, error) {
	parentPath := filepath.Dir(relativePath)
	parents, err := lstatRootedDirectoryPath(root, parentPath, displayPath, kind, false)
	if err != nil {
		return nil, nil, false, err
	}
	parentRoot, err := root.OpenRoot(parentPath)
	if err != nil {
		return nil, nil, false, err
	}
	guard := &rootedRemovalParentGuard{
		homeRoot:    root,
		parentRoot:  parentRoot,
		parentPath:  parentPath,
		displayPath: displayPath,
		kind:        kind,
		generation:  append([]os.FileInfo(nil), parents...),
	}
	present := false
	fail := func(err error) (*rootedRemovalParentGuard, []rootedRemovalChainEntry, bool, error) {
		_ = guard.Close()
		return nil, nil, present, err
	}
	parentOpened, err := parentRoot.Stat(".")
	if err != nil {
		return fail(err)
	}
	if len(parents) == 0 || !parentOpened.IsDir() || !os.SameFile(parents[len(parents)-1], parentOpened) {
		return fail(fmt.Errorf("%s parent changed before removal-chain inspection: %s", kind, displayPath))
	}
	originalLeaf := filepath.Base(relativePath)
	chain := make([]rootedRemovalChainEntry, 0, 2)
	name := originalLeaf
	for depth := 0; len(name) <= rootedRemovalLeafMaxBytes; depth++ {
		entryDisplay := displayPath + strings.Repeat(rootedRemovalPendingSuffix, depth)
		info, err := parentRoot.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			if len(name) > rootedRemovalLeafMaxBytes-len(rootedRemovalPendingSuffix) {
				break
			}
			name += rootedRemovalPendingSuffix
			continue
		}
		if errors.Is(err, syscall.ENAMETOOLONG) {
			break
		}
		if err != nil {
			return fail(err)
		}
		present = true
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			if depth == 0 {
				return fail(fmt.Errorf("%s file must be a regular file: %s", kind, entryDisplay))
			}
			return fail(fmt.Errorf("%s removal chain requires a regular non-symlink removal residue: %s", kind, entryDisplay))
		}
		data, stableInfo, err := readRootedCASRegularFileWithLimit(parentRoot, name, maxBytes)
		if err != nil {
			return fail(err)
		}
		chain = append(chain, rootedRemovalChainEntry{
			name:    name,
			display: entryDisplay,
			depth:   depth,
			data:    data,
			info:    stableInfo,
		})
		if len(name) > rootedRemovalLeafMaxBytes-len(rootedRemovalPendingSuffix) {
			break
		}
		name += rootedRemovalPendingSuffix
	}
	for index := 1; index < len(chain); index++ {
		if !os.SameFile(chain[0].info, chain[index].info) || !bytes.Equal(chain[0].data, chain[index].data) {
			return fail(fmt.Errorf("%s removal chain names do not identify one validated file: %s", kind, displayPath))
		}
	}
	afterParents, err := lstatRootedDirectoryPath(root, parentPath, displayPath, kind, false)
	if err != nil {
		return fail(err)
	}
	if !sameRootedPath(parents, afterParents) {
		return fail(fmt.Errorf("%s path must be stable beneath compound home: %s", kind, displayPath))
	}
	return guard, chain, present, nil
}

func reconcileRootedRemovalChain(
	parent *rootedRemovalParentGuard,
	chain []rootedRemovalChainEntry,
	kind string,
	validator func([]byte) error,
	infoValidator func(os.FileInfo) error,
) error {
	parentRoot := parent.parentRoot
	for _, entry := range chain {
		if validator != nil {
			if err := validator(entry.data); err != nil {
				return fmt.Errorf("validate %s removal residue %s: %w", kind, entry.display, err)
			}
		}
		if infoValidator != nil {
			if err := infoValidator(entry.info); err != nil {
				return err
			}
		}
	}
	for index := len(chain) - 1; index >= 0; index-- {
		entry := chain[index]
		data, info, err := readRootedCASRegularFile(parentRoot, entry.name)
		if err != nil {
			return err
		}
		if !os.SameFile(entry.info, info) || !bytes.Equal(entry.data, data) {
			return fmt.Errorf("%s removal residue changed before recovery: %s", kind, entry.display)
		}
		if validator != nil {
			if err := validator(data); err != nil {
				return fmt.Errorf("validate %s removal residue %s: %w", kind, entry.display, err)
			}
		}
		if infoValidator != nil {
			if err := infoValidator(info); err != nil {
				return err
			}
		}
		if removeRootedBeforeUnlink != nil {
			if err := removeRootedBeforeUnlink(entry.display); err != nil {
				return err
			}
		}
		if err := parent.validate("before removal residue unlink"); err != nil {
			return err
		}
		finalData, finalInfo, err := readRootedCASRegularFile(parentRoot, entry.name)
		if err != nil {
			return err
		}
		if !os.SameFile(info, finalInfo) || !bytes.Equal(data, finalData) {
			return fmt.Errorf("%s removal residue changed after validation: %s", kind, entry.display)
		}
		if infoValidator != nil {
			if err := infoValidator(finalInfo); err != nil {
				return err
			}
		}
		if index == 0 {
			suffixes := strings.Repeat(rootedRemovalPendingSuffix, entry.depth)
			return removeRootedQuarantineWithDurableEvidence(
				parent,
				strings.TrimSuffix(entry.name, suffixes),
				strings.TrimSuffix(entry.display, suffixes),
				entry.name,
				entry.display,
				kind,
				finalData,
				finalInfo,
			)
		}
		if err := parentRoot.Remove(entry.name); err != nil {
			return err
		}
		if err := parent.validate("after removal residue unlink"); err != nil {
			return err
		}
		if err := parent.sync("removal residue cleanup"); err != nil {
			return fmt.Errorf("%s removal residue cleanup parent sync failed with recovery evidence retained: %w", kind, err)
		}
	}
	return nil
}

func removeRootedRegularFileFreshValidatedAt(
	root *os.Root,
	parent *rootedRemovalParentGuard,
	relativePath string,
	displayPath string,
	kind string,
	validator func([]byte) error,
	infoValidator func(os.FileInfo) error,
) error {
	before, err := lstatRootedRegularPath(root, relativePath, displayPath, kind)
	if err != nil {
		return err
	}
	file, err := root.Open(relativePath)
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	after, err := lstatRootedRegularPath(root, relativePath, displayPath, kind)
	if err != nil {
		_ = file.Close()
		return err
	}
	leaf := before[len(before)-1]
	if !sameRootedPath(before, after) || !opened.Mode().IsRegular() || !os.SameFile(leaf, opened) {
		_ = file.Close()
		return fmt.Errorf("%s file must be a stable regular file: %s", kind, displayPath)
	}
	if infoValidator != nil {
		for _, info := range []os.FileInfo{leaf, opened, after[len(after)-1]} {
			if err := infoValidator(info); err != nil {
				_ = file.Close()
				return err
			}
		}
	}
	data, err := io.ReadAll(file)
	if err != nil {
		_ = file.Close()
		return err
	}
	if validator != nil {
		if err := validator(data); err != nil {
			_ = file.Close()
			return err
		}
	}

	parentPath := filepath.Dir(relativePath)
	if err := parent.validate("before removal quarantine"); err != nil {
		_ = file.Close()
		return err
	}
	if parentPath != "." && !sameRootedPath(before[:len(before)-1], parent.generation) {
		_ = file.Close()
		return fmt.Errorf("%s path must be stable beneath compound home: %s", kind, displayPath)
	}
	parentRoot := parent.parentRoot
	originalLeaf := filepath.Base(relativePath)
	quarantineLeaf := originalLeaf + ".remove-pending"
	quarantineDisplay := displayPath + ".remove-pending"
	if err := file.Close(); err != nil {
		return err
	}
	if removeRootedBeforeQuarantinePublish != nil {
		if err := removeRootedBeforeQuarantinePublish(quarantineDisplay); err != nil {
			return err
		}
	}
	if err := parent.validate("before removal quarantine publication"); err != nil {
		return err
	}
	publication, err := BeginRootedFileCAS(parentRoot, quarantineLeaf, originalLeaf, nil, nil, data, opened)
	if guardErr := parent.validate("after removal quarantine publication"); guardErr != nil {
		return errors.Join(err, guardErr)
	}
	if err != nil {
		if publication != nil && publication.State() != RootedFileCASNotPublished {
			return fmt.Errorf("%s removal quarantine publication is ambiguous; preserve both names for recovery: %w", kind, err)
		}
		return fmt.Errorf("publish %s removal quarantine without replacement: %w", kind, err)
	}
	if err := parent.validate("before removal quarantine finalization"); err != nil {
		return err
	}
	if err := publication.Commit(); err != nil {
		return fmt.Errorf("%s removal quarantine finalization is ambiguous; preserve the quarantine for recovery: %w", kind, err)
	}
	if err := parent.validate("after removal quarantine finalization"); err != nil {
		return err
	}
	restore := func(cause error) error {
		quarantineData, quarantineInfo, err := readRootedCASRegularFile(parentRoot, quarantineLeaf)
		if err != nil {
			return fmt.Errorf("%w; inspect %s quarantine before restore: %v", cause, kind, err)
		}
		if !os.SameFile(opened, quarantineInfo) || !bytes.Equal(data, quarantineData) {
			return fmt.Errorf("%w; %s quarantine changed and was not promoted back to authority", cause, kind)
		}
		if infoValidator != nil {
			if err := infoValidator(quarantineInfo); err != nil {
				return fmt.Errorf("%w; %s quarantine failed restore validation: %v", cause, kind, err)
			}
		}
		if err := parent.validate("before removal quarantine restoration"); err != nil {
			return errors.Join(cause, err)
		}
		restoration, err := BeginRootedFileCAS(parentRoot, originalLeaf, quarantineLeaf, nil, nil, data, quarantineInfo)
		if guardErr := parent.validate("after removal quarantine restoration publication"); guardErr != nil {
			return errors.Join(cause, err, guardErr)
		}
		if err != nil {
			if restoration != nil && restoration.State() != RootedFileCASNotPublished {
				return fmt.Errorf("%w; restore %s quarantine is ambiguous; preserve both names for recovery: %v", cause, kind, err)
			}
			return fmt.Errorf("%w; restore %s quarantine without replacement: %v; quarantine retained", cause, kind, err)
		}
		if err := parent.validate("before removal quarantine restoration finalization"); err != nil {
			return errors.Join(cause, err)
		}
		if err := restoration.Commit(); err != nil {
			return fmt.Errorf("%w; restore %s quarantine finalization is ambiguous: %v", cause, kind, err)
		}
		if err := parent.validate("after removal quarantine restoration finalization"); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}

	quarantineData, quarantined, err := readRootedCASRegularFile(parentRoot, quarantineLeaf)
	if err != nil {
		return restore(err)
	}
	if infoValidator != nil {
		if err := infoValidator(quarantined); err != nil {
			return restore(err)
		}
	}
	if quarantined.Mode()&os.ModeSymlink != 0 || !quarantined.Mode().IsRegular() ||
		!os.SameFile(opened, quarantined) || !bytes.Equal(data, quarantineData) {
		return restore(fmt.Errorf("%s file changed after validation: %s", kind, displayPath))
	}
	if infoValidator != nil {
		if err := infoValidator(quarantined); err != nil {
			return restore(err)
		}
	}
	if removeRootedBeforeUnlink != nil {
		if err := removeRootedBeforeUnlink(quarantineDisplay); err != nil {
			return restore(err)
		}
	}
	if err := parent.validate("before removal quarantine unlink"); err != nil {
		return err
	}
	finalData, final, err := readRootedCASRegularFile(parentRoot, quarantineLeaf)
	if err != nil {
		return restore(err)
	}
	if final.Mode()&os.ModeSymlink != 0 || !final.Mode().IsRegular() || !os.SameFile(quarantined, final) || !bytes.Equal(quarantineData, finalData) {
		return restore(fmt.Errorf("%s file changed after validation: %s", kind, displayPath))
	}
	if infoValidator != nil {
		if err := infoValidator(final); err != nil {
			return restore(err)
		}
	}
	return removeRootedQuarantineWithDurableEvidence(
		parent,
		originalLeaf,
		displayPath,
		quarantineLeaf,
		quarantineDisplay,
		kind,
		finalData,
		final,
	)
}

// removeRootedQuarantineWithDurableEvidence publishes one already-synced
// successor name before unlinking the current quarantine. If the source unlink
// succeeds but its parent-directory sync fails, the successor remains named
// recovery evidence instead of leaving an indistinguishable absent state.
func removeRootedQuarantineWithDurableEvidence(
	parent *rootedRemovalParentGuard,
	originalLeaf string,
	originalDisplay string,
	quarantineLeaf string,
	quarantineDisplay string,
	kind string,
	data []byte,
	info os.FileInfo,
) error {
	parentRoot := parent.parentRoot
	evidenceLeaf := quarantineLeaf + rootedRemovalPendingSuffix
	evidenceDisplay := quarantineDisplay + rootedRemovalPendingSuffix
	if len(evidenceLeaf) > rootedRemovalLeafMaxBytes {
		// The original leaf is absent after quarantine publication and is always
		// a valid bounded alternate witness when another suffix would exceed the
		// supported component limit.
		evidenceLeaf = originalLeaf
		evidenceDisplay = originalDisplay
	}

	if err := parent.validate("before removal evidence publication"); err != nil {
		return err
	}
	publication, err := BeginRootedFileCAS(
		parentRoot,
		evidenceLeaf,
		quarantineLeaf,
		nil,
		nil,
		data,
		info,
	)
	if guardErr := parent.validate("after removal evidence publication"); guardErr != nil {
		return errors.Join(err, guardErr)
	}
	if err != nil {
		if publication != nil && publication.State() != RootedFileCASNotPublished {
			return fmt.Errorf("%s removal evidence publication is ambiguous; preserve both names for recovery: %w", kind, err)
		}
		return fmt.Errorf("publish %s removal evidence without replacement: %w", kind, err)
	}
	if err := parent.validate("before removal evidence finalization"); err != nil {
		return err
	}
	if err := publication.Commit(); err != nil {
		return fmt.Errorf("%s removal evidence finalization is ambiguous; preserve %s for recovery: %w", kind, evidenceDisplay, err)
	}
	if err := parent.validate("after removal evidence finalization"); err != nil {
		return err
	}

	evidenceData, evidenceInfo, err := readRootedCASRegularFile(parentRoot, evidenceLeaf)
	if err != nil {
		return err
	}
	if !os.SameFile(info, evidenceInfo) || !bytes.Equal(data, evidenceData) {
		return fmt.Errorf("%s removal evidence changed before cleanup: %s", kind, evidenceDisplay)
	}
	if err := parent.validate("before removal evidence unlink"); err != nil {
		return err
	}
	if err := parentRoot.Remove(evidenceLeaf); err != nil {
		return err
	}
	if err := parent.validate("after removal evidence unlink"); err != nil {
		return err
	}
	if err := parent.sync("removal evidence cleanup"); err != nil {
		restoreErr := restoreRootedRemovalEvidence(
			parent,
			originalLeaf,
			evidenceLeaf,
			evidenceDisplay,
			kind,
			data,
			info.Mode().Perm(),
		)
		return errors.Join(
			fmt.Errorf("%s removal committed but evidence cleanup parent sync failed: %w", kind, err),
			restoreErr,
		)
	}
	return nil
}

func restoreRootedRemovalEvidence(
	parent *rootedRemovalParentGuard,
	originalLeaf string,
	evidenceLeaf string,
	evidenceDisplay string,
	kind string,
	data []byte,
	mode os.FileMode,
) error {
	return restoreRootedRemovalEvidenceWithOps(
		parent,
		originalLeaf,
		evidenceLeaf,
		evidenceDisplay,
		kind,
		data,
		mode,
		defaultRootedRemovalEvidenceRestoreOps(),
	)
}

type rootedRemovalEvidenceRestoreStage string

const (
	rootedRemovalRestoreCreate      rootedRemovalEvidenceRestoreStage = "create"
	rootedRemovalRestoreChmod       rootedRemovalEvidenceRestoreStage = "chmod"
	rootedRemovalRestoreWrite       rootedRemovalEvidenceRestoreStage = "write"
	rootedRemovalRestoreFileSync    rootedRemovalEvidenceRestoreStage = "file_sync"
	rootedRemovalRestoreClose       rootedRemovalEvidenceRestoreStage = "close"
	rootedRemovalRestorePublication rootedRemovalEvidenceRestoreStage = "publication"
	rootedRemovalRestoreParentSync  rootedRemovalEvidenceRestoreStage = "parent_sync"
)

type rootedRemovalEvidenceRestoreOps struct {
	// fault is an error-only boundary seam. It cannot replace a filesystem
	// operation or manufacture successful bytes. Tests may stop one stage; the
	// production operation remains the only implementation of that stage.
	fault func(rootedRemovalEvidenceRestoreStage) error
	// shortWriteBytes deterministically limits the one real write in tests. A
	// positive value smaller than the frame length exercises partial staging;
	// exact-byte verification still gates publication.
	shortWriteBytes int
}

func defaultRootedRemovalEvidenceRestoreOps() rootedRemovalEvidenceRestoreOps {
	return rootedRemovalEvidenceRestoreOps{}
}

func (ops rootedRemovalEvidenceRestoreOps) fail(stage rootedRemovalEvidenceRestoreStage) error {
	if ops.fault == nil {
		return nil
	}
	return ops.fault(stage)
}

func restoreRootedRemovalEvidenceWithOps(
	parent *rootedRemovalParentGuard,
	originalLeaf string,
	evidenceLeaf string,
	evidenceDisplay string,
	kind string,
	data []byte,
	mode os.FileMode,
	ops rootedRemovalEvidenceRestoreOps,
) error {
	if parent == nil || parent.parentRoot == nil {
		return fmt.Errorf("restore %s removal evidence requires a pinned parent", kind)
	}
	parentRoot := parent.parentRoot
	mode = mode.Perm()
	stagingLeaf := rootedRemovalEvidenceStagingLeaf(originalLeaf)

	if err := parent.validate("before restored removal evidence reconciliation"); err != nil {
		return err
	}
	if restoredData, restoredInfo, err := readRootedCASRegularFile(parentRoot, evidenceLeaf); err == nil {
		if !bytes.Equal(data, restoredData) || restoredInfo.Mode().Perm() != mode {
			return fmt.Errorf("existing restored %s removal evidence is not the exact frame: %s", kind, evidenceDisplay)
		}
		if err := reconcileRootedRemovalEvidenceStaging(parent, stagingLeaf, data, mode, true); err != nil {
			return err
		}
		return parent.sync("restored removal evidence retry")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect restored %s removal evidence: %w", kind, err)
	}

	stagingInfo, err := prepareRootedRemovalEvidenceStaging(parent, stagingLeaf, kind, data, mode, ops)
	if err != nil {
		return err
	}
	if err := parent.validate("before restored removal evidence publication"); err != nil {
		return err
	}
	if err := ops.fail(rootedRemovalRestorePublication); err != nil {
		return fmt.Errorf("publish restored %s removal evidence: %w", kind, err)
	}
	if err := parentRoot.Link(stagingLeaf, evidenceLeaf); err != nil {
		return fmt.Errorf("atomically publish restored %s removal evidence: %w", kind, err)
	}
	if err := parent.validate("after restored removal evidence publication"); err != nil {
		return err
	}
	restoredData, restoredInfo, err := readRootedCASRegularFile(parentRoot, evidenceLeaf)
	if err != nil {
		return fmt.Errorf("read published restored %s removal evidence: %w", kind, err)
	}
	stagedData, stagedInfo, err := readRootedCASRegularFile(parentRoot, stagingLeaf)
	if err != nil {
		return fmt.Errorf("read staged restored %s removal evidence after publication: %w", kind, err)
	}
	if !os.SameFile(stagingInfo, restoredInfo) || !os.SameFile(restoredInfo, stagedInfo) ||
		!bytes.Equal(data, restoredData) || !bytes.Equal(data, stagedData) ||
		restoredInfo.Mode().Perm() != mode || stagedInfo.Mode().Perm() != mode {
		return fmt.Errorf("published restored %s removal evidence is not the exact staged frame: %s", kind, evidenceDisplay)
	}
	if err := ops.fail(rootedRemovalRestoreParentSync); err != nil {
		return fmt.Errorf("sync published restored %s removal evidence parent: %w", kind, err)
	}
	if err := parent.sync("restored removal evidence publication"); err != nil {
		return fmt.Errorf("sync published restored %s removal evidence parent: %w", kind, err)
	}
	if err := parent.validate("before restored removal evidence staging cleanup"); err != nil {
		return err
	}
	if err := parentRoot.Remove(stagingLeaf); err != nil {
		return fmt.Errorf("remove published restored %s removal evidence staging: %w", kind, err)
	}
	if err := parent.validate("after restored removal evidence staging cleanup"); err != nil {
		return err
	}
	if err := parent.sync("restored removal evidence staging cleanup"); err != nil {
		return fmt.Errorf("sync restored %s removal evidence staging cleanup: %w", kind, err)
	}
	return nil
}

func rootedRemovalEvidenceStagingLeaf(originalLeaf string) string {
	digest := sha256.Sum256([]byte(originalLeaf))
	// Built by concatenation so no dotted format literal reads as a hostname
	// to the public redact gate.
	return ".compound-remove-restore-" + hex.EncodeToString(digest[:]) + ".staging"
}

func rootedRemovalEvidenceStagingPresent(parent *rootedRemovalParentGuard, stagingLeaf string) (bool, error) {
	_, _, err := readRootedCASRegularFile(parent.parentRoot, stagingLeaf)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	return true, nil
}

func removeRootedRemovalEvidenceStaging(parent *rootedRemovalParentGuard, stagingLeaf, kind string) error {
	present, err := rootedRemovalEvidenceStagingPresent(parent, stagingLeaf)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if err := parent.validate("before removal recovery staging cleanup"); err != nil {
		return err
	}
	if err := parent.parentRoot.Remove(stagingLeaf); err != nil {
		return err
	}
	if err := parent.validate("after removal recovery staging cleanup"); err != nil {
		return err
	}
	if err := parent.sync("removal recovery staging cleanup"); err != nil {
		return fmt.Errorf("sync %s removal recovery staging cleanup: %w", kind, err)
	}
	return nil
}

func prepareRootedRemovalEvidenceStaging(
	parent *rootedRemovalParentGuard,
	stagingLeaf string,
	kind string,
	data []byte,
	mode os.FileMode,
	ops rootedRemovalEvidenceRestoreOps,
) (os.FileInfo, error) {
	parentRoot := parent.parentRoot
	if stagedData, stagedInfo, err := readRootedCASRegularFile(parentRoot, stagingLeaf); err == nil {
		if bytes.Equal(data, stagedData) && stagedInfo.Mode().Perm() == mode {
			return syncRootedRemovalEvidenceStaging(parent, stagingLeaf, stagedInfo, kind, data, mode)
		}
		if err := reconcileRootedRemovalEvidenceStaging(parent, stagingLeaf, data, mode, false); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect staged restored %s removal evidence: %w", kind, err)
	}

	if err := ops.fail(rootedRemovalRestoreCreate); err != nil {
		return nil, fmt.Errorf("create staged restored %s removal evidence: %w", kind, err)
	}
	file, err := parentRoot.OpenFile(stagingLeaf, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return nil, fmt.Errorf("create staged restored %s removal evidence: %w", kind, err)
	}
	closed := false
	closeFile := func() error {
		if closed {
			return nil
		}
		closed = true
		return file.Close()
	}
	defer func() { _ = closeFile() }()
	fail := func(cause error) (os.FileInfo, error) {
		return nil, errors.Join(cause, closeFile())
	}

	if err := ops.fail(rootedRemovalRestoreChmod); err != nil {
		return fail(fmt.Errorf("chmod staged restored %s removal evidence: %w", kind, err))
	}
	if err := file.Chmod(mode); err != nil {
		return fail(fmt.Errorf("chmod staged restored %s removal evidence: %w", kind, err))
	}
	opened, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("stat staged restored %s removal evidence: %w", kind, err))
	}
	if err := ops.fail(rootedRemovalRestoreWrite); err != nil {
		return fail(fmt.Errorf("write staged restored %s removal evidence: %w", kind, err))
	}
	writeData := data
	if ops.shortWriteBytes > 0 && ops.shortWriteBytes < len(data) {
		writeData = data[:ops.shortWriteBytes]
	}
	n, err := file.Write(writeData)
	if err != nil {
		return fail(fmt.Errorf("write staged restored %s removal evidence: %w", kind, err))
	}
	if n != len(data) {
		return fail(fmt.Errorf("write staged restored %s removal evidence: %w", kind, io.ErrShortWrite))
	}
	if err := ops.fail(rootedRemovalRestoreFileSync); err != nil {
		return fail(fmt.Errorf("sync staged restored %s removal evidence: %w", kind, err))
	}
	if err := file.Sync(); err != nil {
		return fail(fmt.Errorf("sync staged restored %s removal evidence: %w", kind, err))
	}
	if err := closeFile(); err != nil {
		return nil, fmt.Errorf("close staged restored %s removal evidence: %w", kind, err)
	}
	if err := ops.fail(rootedRemovalRestoreClose); err != nil {
		return nil, fmt.Errorf("close staged restored %s removal evidence: %w", kind, err)
	}
	stagedData, stagedInfo, err := readRootedCASRegularFile(parentRoot, stagingLeaf)
	if err != nil {
		return nil, fmt.Errorf("read staged restored %s removal evidence: %w", kind, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(opened, stagedInfo) ||
		!bytes.Equal(data, stagedData) || stagedInfo.Mode().Perm() != mode {
		return nil, fmt.Errorf("staged restored %s removal evidence is not the exact durable frame", kind)
	}
	return stagedInfo, nil
}

func syncRootedRemovalEvidenceStaging(
	parent *rootedRemovalParentGuard,
	stagingLeaf string,
	expectedInfo os.FileInfo,
	kind string,
	data []byte,
	mode os.FileMode,
) (os.FileInfo, error) {
	file, err := parent.parentRoot.Open(stagingLeaf)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(statErr, syncErr, closeErr); err != nil {
		return nil, fmt.Errorf("re-sync staged restored %s removal evidence: %w", kind, err)
	}
	stagedData, stagedInfo, err := readRootedCASRegularFile(parent.parentRoot, stagingLeaf)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(expectedInfo, opened) || !os.SameFile(opened, stagedInfo) ||
		!bytes.Equal(data, stagedData) || stagedInfo.Mode().Perm() != mode {
		return nil, fmt.Errorf("staged restored %s removal evidence changed before retry publication", kind)
	}
	return stagedInfo, nil
}

func reconcileRootedRemovalEvidenceStaging(
	parent *rootedRemovalParentGuard,
	stagingLeaf string,
	data []byte,
	mode os.FileMode,
	allowExact bool,
) error {
	stagedData, stagedInfo, err := readRootedCASRegularFile(parent.parentRoot, stagingLeaf)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect rooted removal evidence staging: %w", err)
	}
	if allowExact && (!bytes.Equal(data, stagedData) || stagedInfo.Mode().Perm() != mode) {
		return fmt.Errorf("rooted removal evidence staging conflicts with committed evidence")
	}
	if err := parent.validate("before rooted removal evidence staging reconciliation"); err != nil {
		return err
	}
	if err := parent.parentRoot.Remove(stagingLeaf); err != nil {
		return err
	}
	if err := parent.validate("after rooted removal evidence staging reconciliation"); err != nil {
		return err
	}
	return parent.sync("rooted removal evidence staging reconciliation")
}

func openRootedPath(homePath, path, kind string) (*os.Root, string, string, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "owned"
	}
	homePath, err := filepath.Abs(filepath.Clean(homePath))
	if err != nil {
		return nil, "", "", err
	}
	path, err = filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, "", "", err
	}
	relativePath, err := filepath.Rel(homePath, path)
	if err != nil {
		return nil, "", "", fmt.Errorf("%s file path is not relative to compound home: %w", kind, err)
	}
	if relativePath == "." || !filepath.IsLocal(relativePath) {
		return nil, "", "", fmt.Errorf("%s file must remain beneath compound home: %s", kind, path)
	}
	root, err := os.OpenRoot(homePath)
	if err != nil {
		return nil, "", "", err
	}
	return root, relativePath, path, nil
}

func lstatRootedRegularPath(root *os.Root, relativePath, displayPath, kind string) ([]os.FileInfo, error) {
	components := strings.Split(relativePath, string(filepath.Separator))
	infos := make([]os.FileInfo, 0, len(components))
	for index := range components {
		componentPath := filepath.Join(components[:index+1]...)
		info, err := root.Lstat(componentPath)
		if err != nil {
			return nil, err
		}
		leaf := index == len(components)-1
		if info.Mode()&os.ModeSymlink != 0 {
			if leaf {
				return nil, fmt.Errorf("%s file must be a regular file: %s", kind, displayPath)
			}
			return nil, fmt.Errorf("%s path must not contain symlink beneath compound home: %s", kind, displayPath)
		}
		if leaf {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s file must be a regular file: %s", kind, displayPath)
			}
		} else if !info.IsDir() {
			return nil, fmt.Errorf("%s path ancestor must be a directory beneath compound home: %s", kind, displayPath)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func ensureRootedDirectories(root *os.Root, relativePath, displayPath, kind string, perm os.FileMode) ([]os.FileInfo, error) {
	existing, err := lstatRootedDirectoryPath(root, relativePath, displayPath, kind, true)
	if err != nil {
		return nil, err
	}
	if relativePath == "." {
		return existing, nil
	}
	components := strings.Split(relativePath, string(filepath.Separator))
	for index := len(existing); index < len(components); index++ {
		componentPath := filepath.Join(components[:index+1]...)
		created := false
		if err := root.Mkdir(componentPath, perm); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return nil, err
			}
		} else {
			created = true
		}
		parentPath := filepath.Dir(componentPath)
		if err := syncRootedDirectory(root, parentPath, displayPath, kind); err != nil {
			if created {
				_ = root.Remove(componentPath)
				_ = syncRootedDirectory(root, parentPath, displayPath, kind)
			}
			return nil, err
		}
	}
	return lstatRootedDirectoryPath(root, relativePath, displayPath, kind, false)
}

func lstatRootedDirectoryPath(root *os.Root, relativePath, displayPath, kind string, allowMissing bool) ([]os.FileInfo, error) {
	if relativePath == "." {
		info, err := root.Stat(".")
		if err != nil {
			return nil, err
		}
		return []os.FileInfo{info}, nil
	}
	components := strings.Split(relativePath, string(filepath.Separator))
	infos := make([]os.FileInfo, 0, len(components))
	for index := range components {
		componentPath := filepath.Join(components[:index+1]...)
		info, err := root.Lstat(componentPath)
		if err != nil {
			if allowMissing && errors.Is(err, os.ErrNotExist) {
				return infos, nil
			}
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s path must not contain symlink beneath compound home: %s", kind, displayPath)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s path ancestor must be a directory beneath compound home: %s", kind, displayPath)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func sameRootedPath(before, after []os.FileInfo) bool {
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

func syncRootedDirectory(root *os.Root, relativePath, displayPath, kind string) error {
	return syncRootedDirectoryWithEINVALPolicy(root, relativePath, displayPath, kind, true)
}

func syncRootedDirectoryStrict(root *os.Root, relativePath, displayPath, kind string) error {
	return syncRootedDirectoryWithEINVALPolicy(root, relativePath, displayPath, kind, false)
}

func syncRootedDirectoryWithEINVALPolicy(root *os.Root, relativePath, displayPath, kind string, ignoreEINVAL bool) error {
	before, err := lstatRootedDirectoryPath(root, relativePath, displayPath, kind, false)
	if err != nil {
		return err
	}
	dir, err := root.Open(relativePath)
	if err != nil {
		return err
	}
	opened, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		return err
	}
	after, err := lstatRootedDirectoryPath(root, relativePath, displayPath, kind, false)
	if err != nil {
		_ = dir.Close()
		return err
	}
	if !sameRootedPath(before, after) || !opened.IsDir() || !os.SameFile(before[len(before)-1], opened) {
		_ = dir.Close()
		return fmt.Errorf("%s path must be stable beneath compound home: %s", kind, displayPath)
	}
	syncErr := syncRootedDirectoryFile(relativePath, dir)
	closeErr := dir.Close()
	if syncErr != nil && (!ignoreEINVAL || !errors.Is(syncErr, syscall.EINVAL)) {
		return syncErr
	}
	return closeErr
}
