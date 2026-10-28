package home

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// AdmittedDescendantRoot pins one authoritative directory generation beneath
// COMPOUND_HOME for the lifetime of a WorkAdmissionGuard callback. All name
// mutations are descriptor-relative and revalidate that the pinned directory
// is still the canonical pathname generation immediately before and after the
// operation.
//
// The type deliberately exposes operations rather than its *os.Root. Callers
// cannot accidentally fall back to pathname I/O after admission or skip the
// generation check around a publication boundary.
type AdmittedDescendantRoot struct {
	mu         sync.Mutex
	guard      *WorkAdmissionGuard
	homePath   string
	workID     string
	relative   string
	kind       string
	root       *os.Root
	generation []os.FileInfo
	opened     os.FileInfo
	closed     bool
}

var admittedDescendantTempSequence atomic.Uint64

// OpenDescendantRoot pins a pre-scaffolded relative directory beneath the exact
// home owned by guard. Missing authority is refused without creating anything:
// home init owns directory materialization and its durability evidence.
// relative must name a local descendant, never COMPOUND_HOME itself. The
// returned root must be closed before guard expires.
func (g *WorkAdmissionGuard) OpenDescendantRoot(
	homePath,
	workID,
	relative,
	kind string,
	_ os.FileMode,
) (*AdmittedDescendantRoot, error) {
	relative = filepath.Clean(strings.TrimSpace(relative))
	kind = strings.TrimSpace(kind)
	if relative == "." || !filepath.IsLocal(relative) {
		return nil, fmt.Errorf("open admitted descendant root: invalid relative path %q", relative)
	}
	if kind == "" {
		return nil, errors.New("open admitted descendant root: kind is required")
	}
	var admitted *AdmittedDescendantRoot
	useErr := g.WithUse(homePath, workID, func() error {
		before, err := snapshotAdmittedDescendant(g.root.root, relative)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"admitted %s root must be pre-scaffolded by home init: %s",
				kind,
				filepath.Join(g.canonicalHome, relative),
			)
		}
		if err != nil {
			return fmt.Errorf("inspect admitted %s root: %w", kind, err)
		}
		root, err := g.root.root.OpenRoot(relative)
		if err != nil {
			return fmt.Errorf("open admitted %s root: %w", kind, err)
		}
		opened, openErr := root.Stat(".")
		after, afterErr := snapshotAdmittedDescendant(g.root.root, relative)
		if err := errors.Join(openErr, afterErr); err != nil {
			_ = root.Close()
			return fmt.Errorf("pin admitted %s root: %w", kind, err)
		}
		if !opened.IsDir() || !sameWorkAdmissionSnapshot(before, after) ||
			!os.SameFile(after[len(after)-1], opened) {
			_ = root.Close()
			return fmt.Errorf("admitted %s root generation changed while opening: %s", kind, filepath.Join(g.canonicalHome, relative))
		}
		admitted = &AdmittedDescendantRoot{
			guard:      g,
			homePath:   homePath,
			workID:     workID,
			relative:   relative,
			kind:       kind,
			root:       root,
			generation: after,
			opened:     opened,
		}
		return nil
	})
	if useErr != nil {
		if admitted != nil {
			useErr = errors.Join(useErr, admitted.Close())
		}
		return nil, fmt.Errorf("open admitted %s root: %w", kind, useErr)
	}
	return admitted, nil
}

func snapshotAdmittedDescendant(root *os.Root, relative string) ([]os.FileInfo, error) {
	relative = filepath.Clean(relative)
	if relative == "." {
		info, err := root.Stat(".")
		if err != nil {
			return nil, err
		}
		if err := validateAdmittedDescendantDirectory(".", info); err != nil {
			return nil, err
		}
		return []os.FileInfo{info}, nil
	}
	if !filepath.IsLocal(relative) {
		return nil, fmt.Errorf("admitted descendant path escapes pinned home: %s", relative)
	}
	components := strings.Split(relative, string(filepath.Separator))
	infos := make([]os.FileInfo, 0, len(components))
	for index := range components {
		componentPath := filepath.Join(components[:index+1]...)
		info, err := root.Lstat(componentPath)
		if err != nil {
			return nil, err
		}
		if err := validateAdmittedDescendantDirectory(componentPath, info); err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func validateAdmittedDescendantDirectory(relative string, info os.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("admitted descendant %s must be a non-symlink directory", filepath.ToSlash(relative))
	}
	if !workAdmissionOwnedByCurrentUID(info) {
		return fmt.Errorf("admitted descendant %s must be owned by current uid %d", filepath.ToSlash(relative), workAdmissionCurrentUID())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("admitted descendant %s must not be group/world writable: mode=%04o", filepath.ToSlash(relative), info.Mode().Perm())
	}
	return nil
}

func (r *AdmittedDescendantRoot) validateIdentityLocked(boundary string) error {
	if r == nil || r.guard == nil || r.root == nil || r.opened == nil || len(r.generation) == 0 || r.closed {
		return errors.New("admitted descendant root is incomplete or closed")
	}
	current, currentErr := snapshotAdmittedDescendant(r.guard.root.root, r.relative)
	opened, openedErr := r.root.Stat(".")
	if err := errors.Join(currentErr, openedErr); err != nil {
		return fmt.Errorf("revalidate admitted %s root at %s: %w", r.kind, boundary, err)
	}
	if !opened.IsDir() || !sameWorkAdmissionSnapshot(r.generation, current) ||
		!os.SameFile(r.opened, opened) || !os.SameFile(opened, current[len(current)-1]) {
		return fmt.Errorf("admitted %s root generation changed at %s: %s", r.kind, boundary, filepath.Join(r.guard.canonicalHome, r.relative))
	}
	return nil
}

func (r *AdmittedDescendantRoot) withUse(
	beforeBoundary,
	afterBoundary string,
	fn func(*os.Root) error,
) error {
	if r == nil {
		return errors.New("admitted descendant root is nil")
	}
	if fn == nil {
		return errors.New("admitted descendant root operation callback is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.guard == nil || r.root == nil || r.opened == nil || len(r.generation) == 0 || r.closed {
		return errors.New("admitted descendant root is incomplete or closed")
	}
	return r.guard.WithUse(r.homePath, r.workID, func() error {
		if err := r.validateIdentityLocked(beforeBoundary); err != nil {
			return err
		}
		var callbackErr error
		var panicValue any
		func() {
			defer func() {
				panicValue = recover()
			}()
			callbackErr = fn(r.root)
		}()
		validationErr := r.validateIdentityLocked(afterBoundary)
		if panicValue != nil {
			if validationErr != nil {
				panic(errors.Join(
					fmt.Errorf("admitted %s root operation panic: %v", r.kind, panicValue),
					validationErr,
				))
			}
			panic(panicValue)
		}
		return errors.Join(callbackErr, validationErr)
	})
}

// Validate proves the descriptor and canonical pathname still identify the
// generation pinned by OpenDescendantRoot. Validation itself is a retained
// use so close and guard invalidation cannot race the descriptor checks.
func (r *AdmittedDescendantRoot) Validate(boundary string) error {
	return r.withUse(boundary, boundary, func(*os.Root) error { return nil })
}

// Close releases the pinned descriptor. It never mutates the directory.
func (r *AdmittedDescendantRoot) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.root == nil || r.closed {
		return nil
	}
	r.closed = true
	err := r.root.Close()
	r.root = nil
	return err
}

func validateAdmittedLeaf(leaf string) error {
	if leaf == "" || leaf == "." || leaf == ".." || filepath.Base(leaf) != leaf || !filepath.IsLocal(leaf) {
		return fmt.Errorf("invalid admitted descendant leaf %q", leaf)
	}
	return nil
}

// CreateTemp creates a unique sibling staging file inside the pinned root.
func (r *AdmittedDescendantRoot) CreateTemp(prefix string, perm os.FileMode) (*os.File, string, error) {
	if err := validateAdmittedLeaf(prefix); err != nil {
		return nil, "", err
	}
	var file *os.File
	var leaf string
	err := r.withUse("before staging create", "after staging create", func(root *os.Root) error {
		for attempts := 0; attempts < 100; attempts++ {
			leaf = fmt.Sprintf("%s%d-%d", prefix, os.Getpid(), admittedDescendantTempSequence.Add(1))
			opened, err := root.OpenFile(leaf, os.O_CREATE|os.O_EXCL|os.O_RDWR, perm.Perm())
			if errors.Is(err, os.ErrExist) {
				continue
			}
			if err != nil {
				return err
			}
			file = opened
			return nil
		}
		return errors.New("create admitted descendant temp: exhausted unique names")
	})
	if err != nil {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		return nil, leaf, err
	}
	return file, leaf, nil
}

// Open opens a local leaf beneath the pinned root after generation validation.
func (r *AdmittedDescendantRoot) Open(leaf string) (*os.File, error) {
	if err := validateAdmittedLeaf(leaf); err != nil {
		return nil, err
	}
	var file *os.File
	err := r.withUse("before open", "after open", func(root *os.Root) error {
		opened, err := root.Open(leaf)
		if err != nil {
			return err
		}
		file = opened
		return nil
	})
	if err != nil {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		return nil, err
	}
	return file, nil
}

// OpenFile opens or creates a local leaf beneath the pinned generation. It is
// primarily used for deterministic recovery staging whose name must survive a
// failed publication and be reusable by the next admitted retry.
func (r *AdmittedDescendantRoot) OpenFile(leaf string, flag int, perm os.FileMode) (*os.File, error) {
	if err := validateAdmittedLeaf(leaf); err != nil {
		return nil, err
	}
	var file *os.File
	err := r.withUse("before open file", "after open file", func(root *os.Root) error {
		opened, err := root.OpenFile(leaf, flag, perm)
		if err != nil {
			return err
		}
		file = opened
		return nil
	})
	if err != nil {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		return nil, err
	}
	return file, nil
}

// Lstat inspects a local leaf beneath the pinned root.
func (r *AdmittedDescendantRoot) Lstat(leaf string) (os.FileInfo, error) {
	if err := validateAdmittedLeaf(leaf); err != nil {
		return nil, err
	}
	var info os.FileInfo
	err := r.withUse("before lstat", "after lstat", func(root *os.Root) error {
		var err error
		info, err = root.Lstat(leaf)
		return err
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// ReadFile reads a local regular non-symlink leaf from the pinned root.
func (r *AdmittedDescendantRoot) ReadFile(leaf string) ([]byte, error) {
	if err := validateAdmittedLeaf(leaf); err != nil {
		return nil, err
	}
	var body []byte
	err := r.withUse("before read", "after read", func(root *os.Root) error {
		before, err := root.Lstat(leaf)
		if err != nil {
			return err
		}
		if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
			return fmt.Errorf("admitted %s leaf must be a regular non-symlink file: %s", r.kind, leaf)
		}
		file, err := root.Open(leaf)
		if err != nil {
			return err
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil {
			return err
		}
		after, err := root.Lstat(leaf)
		if err != nil {
			return err
		}
		if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
			return fmt.Errorf("admitted %s leaf changed while opening: %s", r.kind, leaf)
		}
		body, err = io.ReadAll(file)
		return err
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// Rename atomically publishes one local leaf over another inside the pinned
// directory. Generation drift is refused before the rename.
func (r *AdmittedDescendantRoot) Rename(oldLeaf, newLeaf string) error {
	if err := validateAdmittedLeaf(oldLeaf); err != nil {
		return err
	}
	if err := validateAdmittedLeaf(newLeaf); err != nil {
		return err
	}
	return r.withUse("before rename publication", "after rename publication", func(root *os.Root) error {
		return root.Rename(oldLeaf, newLeaf)
	})
}

// Remove removes one local leaf from the pinned directory. Generation drift is
// refused before unlink.
func (r *AdmittedDescendantRoot) Remove(leaf string) error {
	if err := validateAdmittedLeaf(leaf); err != nil {
		return err
	}
	return r.withUse("before unlink", "after unlink", func(root *os.Root) error {
		return root.Remove(leaf)
	})
}

// RecoverRegularFileRemovalValidated performs the existing durable validated
// removal protocol descriptor-relative to this pinned generation. An absent
// canonical leaf is accepted only as the recovery state of an earlier unlink;
// the parent is re-synced before success.
func (r *AdmittedDescendantRoot) RecoverRegularFileRemovalValidated(leaf string, validator func([]byte) error) error {
	if err := validateAdmittedLeaf(leaf); err != nil {
		return err
	}
	return r.withUse("before validated removal recovery", "after validated removal recovery", func(root *os.Root) error {
		display := filepath.Join(r.guard.canonicalHome, r.relative, leaf)
		return removeRootedRegularFileDurableValidatedAt(
			root,
			leaf,
			display,
			r.kind,
			validator,
			nil,
			true,
		)
	})
}

// Sync persists directory-entry changes for the pinned generation.
func (r *AdmittedDescendantRoot) Sync() error {
	return r.withUse("before directory sync", "after directory sync", func(root *os.Root) error {
		directory, err := root.Open(".")
		if err != nil {
			return err
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if errors.Is(syncErr, syscall.EINVAL) {
			syncErr = nil
		}
		return errors.Join(syncErr, closeErr)
	})
}
