package home

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const restoreStagingPrefix = ".compound-restore-staging-"

var (
	restoreArchiveOpened           = func(string) error { return nil }
	restoreArchivePreflighted      = func(string) error { return nil }
	restoreBeforePublish           = func(string, string) error { return nil }
	restoreSyncParent              = syncRestoreDirectory
	restoreSyncStagingDirectory    = syncRootedDirectory
	restoreNoReplace               = restoreRenameNoReplace
	restoreOpenStaging             = func(parent *os.Root, name string) (*os.Root, error) { return parent.OpenRoot(name) }
	restoreBeforeStagingValidation = func(string) error { return nil }
	restoreCleanupWalk             = func(root *os.Root, fn fs.WalkDirFunc) error {
		return walkRestoreCleanupBatched(root, ".", func(path string, info fs.FileInfo) error {
			return fn(filepath.ToSlash(path), fs.FileInfoToDirEntry(info), nil)
		})
	}
	restoreCleanupRemove = func(root *os.Root, name string) error { return root.Remove(name) }
	restoreCleanupSync   = func(root *os.Root, display string) error {
		return syncRootedDirectory(root, ".", display, "archive restore cleanup")
	}
)

// RestoreArchive verifies and extracts an archive through one open handle into
// a private sibling staging directory, validates the staged home, and only then
// publishes it to a new absent target name.
func RestoreArchive(archivePath, targetHome string, now time.Time) (RestoreReport, error) {
	report, err := restoreArchiveWithLimits(archivePath, targetHome, now, defaultArchiveReadLimits)
	return report, withManifestRecoveryGuidance(err)
}

func restoreArchiveWithLimits(archivePath, targetHome string, now time.Time, limits archiveReadLimits) (report RestoreReport, resultErr error) {
	if strings.TrimSpace(archivePath) == "" {
		return RestoreReport{}, fmt.Errorf("archive path required")
	}
	if strings.TrimSpace(targetHome) == "" {
		return RestoreReport{}, fmt.Errorf("target home required")
	}
	archivePath, err := filepath.Abs(expandUser(archivePath))
	if err != nil {
		return RestoreReport{}, err
	}
	targetHome, err = filepath.Abs(expandUser(targetHome))
	if err != nil {
		return RestoreReport{}, err
	}
	parentPath := filepath.Dir(targetHome)
	targetName := filepath.Base(targetHome)
	if !filepath.IsLocal(targetName) || targetName == "." {
		return RestoreReport{}, fmt.Errorf("restore target must name one new child of its parent: %s", targetHome)
	}
	parent, parentFD, parentInfo, err := openStableRestoreParent(parentPath)
	if err != nil {
		return RestoreReport{}, err
	}
	defer parent.Close()
	defer parentFD.Close()
	if err := requireAbsentRestoreTarget(parent, targetName, targetHome); err != nil {
		return RestoreReport{}, err
	}
	archive, err := openVerifiedBackupArchive(archivePath, limits)
	if err != nil {
		return RestoreReport{}, err
	}
	defer func() {
		if err := archive.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close verified archive source %s: %w", archivePath, err))
		}
	}()
	stagingName, staging, stagingInfo, err := createRestoreStaging(parent, parentFD, parentPath)
	if err != nil {
		return RestoreReport{}, err
	}
	stagingPath := filepath.Join(parentPath, stagingName)
	published := false
	defer func() {
		if !published {
			if err := cleanupRestoreStaging(parent, staging, stagingName, stagingInfo, stagingPath); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("restore cleanup incomplete for private staging %s; inspect manually before retrying: %w", stagingPath, err))
			}
		}
		if err := staging.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close restore staging %s: %w", stagingPath, err))
		}
	}()

	target := &archiveRestoreTarget{root: staging, displayPath: stagingPath, deferFileParentSync: archive.streamed, stagingInfo: stagingInfo}
	if restoreArchivePreflighted != nil {
		if err := restoreArchivePreflighted(archivePath); err != nil {
			return RestoreReport{}, err
		}
	}
	manifest, actual, err := archive.extract(target)
	if err != nil {
		return RestoreReport{}, err
	}
	verify := archive.streamedReport
	if !archive.streamed {
		verify, err = verifyArchiveManifestWithLimits(archivePath, manifest, actual, limits)
		if err != nil {
			return RestoreReport{}, err
		}
	}
	if err := target.completeHabitat(); err != nil {
		return RestoreReport{}, err
	}
	if err := verifyRestoreStaging(parent, staging, stagingName, stagingInfo, stagingPath); err != nil {
		return RestoreReport{}, err
	}
	if err := target.finalizeDirectoryModes(); err != nil {
		return RestoreReport{}, err
	}
	if archive.streamed {
		if err := target.validatePinnedHomeWithStreamed(true); err != nil {
			return RestoreReport{}, fmt.Errorf("restored home failed rooted health check: %w", err)
		}
	} else if report := DoctorWithOptions(stagingPath, DoctorOptions{Now: now}); !report.OK || !report.PrivateCustody.OK {
		return RestoreReport{}, fmt.Errorf(
			"restored home failed doctor: manifest_ok=%t dirs=%d/%d habitat_ok=%t unknown_roots=%v unknown_entries=%v private_custody=%s findings=%v",
			report.ManifestOK,
			report.DirsOK,
			len(report.Dirs),
			report.HabitatOK,
			report.Habitat.UnknownRoots,
			report.Habitat.UnknownEntries,
			report.PrivateCustody.Status,
			report.PrivateCustody.Findings,
		)
	}
	if err := verifyRestoreStaging(parent, staging, stagingName, stagingInfo, stagingPath); err != nil {
		return RestoreReport{}, err
	}
	if err := target.validatePinnedHomeWithStreamed(archive.streamed); err != nil {
		return RestoreReport{}, err
	}
	if err := target.syncTree(); err != nil {
		return RestoreReport{}, fmt.Errorf("sync restore staging: %w", err)
	}
	if err := verifyRestoreParent(parent, parentFD, parentPath, parentInfo); err != nil {
		return RestoreReport{}, err
	}
	if err := requireAbsentRestoreTarget(parent, targetName, targetHome); err != nil {
		return RestoreReport{}, err
	}
	if err := restoreBeforePublish(stagingPath, targetHome); err != nil {
		return RestoreReport{}, err
	}
	if err := verifyRestoreParent(parent, parentFD, parentPath, parentInfo); err != nil {
		return RestoreReport{}, err
	}
	if err := verifyRestoreStaging(parent, staging, stagingName, stagingInfo, stagingPath); err != nil {
		return RestoreReport{}, err
	}
	if err := target.validatePinnedHomeWithStreamed(archive.streamed); err != nil {
		return RestoreReport{}, err
	}
	if archive.streamed {
		if err := archive.verifyStreamedStaging(target); err != nil {
			return RestoreReport{}, err
		}
		if err := restoreStreamedDeviceBarrier(target); err != nil {
			return RestoreReport{}, fmt.Errorf("streamed restore pre-publication durability failed: %w", err)
		}
	} else if err := target.verifyEntries(manifest, limits); err != nil {
		return RestoreReport{}, err
	}
	if err := restoreNoReplace(parentFD, stagingName, targetName); err != nil {
		if targetErr := verifyPublishedRestoreTarget(parent, staging, targetName, stagingInfo, targetHome); targetErr == nil {
			published = true
			resultErr := fmt.Errorf("restore publication returned an error after the target acquired the staged identity; target preserved at %s for manual verification: %w", targetHome, err)
			if parentErr := verifyRestoreParent(parent, parentFD, parentPath, parentInfo); parentErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("published restore parent verification failed: %w", parentErr))
			}
			if syncErr := restoreSyncParent(parentFD); syncErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("published restore parent sync failed: %w", syncErr))
			}
			return RestoreReport{}, resultErr
		}
		return RestoreReport{}, fmt.Errorf("publish restored home without replacement: %w", err)
	}
	published = true
	if err := verifyPublishedRestoreTarget(parent, staging, targetName, stagingInfo, targetHome); err != nil {
		return RestoreReport{}, fmt.Errorf("restore published but target identity changed; inspect target manually before retrying: %w", err)
	}
	if err := verifyRestoreParent(parent, parentFD, parentPath, parentInfo); err != nil {
		return RestoreReport{}, fmt.Errorf("restore published but target parent identity changed; inspect target manually before retrying: %w", err)
	}
	if err := restoreSyncParent(parentFD); err != nil {
		return RestoreReport{}, fmt.Errorf("restore published but parent sync failed; inspect target manually before retrying: %w", err)
	}
	if archive.streamed {
		if err := restoreStreamedPublishParentSync(parentFD, stagingInfo); err != nil {
			return RestoreReport{}, fmt.Errorf("restore published but durable parent sync failed; target preserved for manual verification: %w", err)
		}
		if err := restoreStreamedDeviceBarrier(target); err != nil {
			return RestoreReport{}, fmt.Errorf("restore published but final device flush failed; target preserved for manual verification: %w", err)
		}
	}
	return RestoreReport{ArchivePath: archivePath, TargetHome: targetHome, Kind: verify.Kind, Files: verify.Files, Bytes: verify.Bytes}, nil
}

func openStableRestoreParent(path string) (*os.Root, *os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, nil, nil, fmt.Errorf("restore target parent must be a non-symlink directory: %s", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, nil, err
	}
	directory, err := os.Open(path)
	if err != nil {
		_ = root.Close()
		return nil, nil, nil, err
	}
	if err := verifyRestoreParent(root, directory, path, before); err != nil {
		_ = directory.Close()
		_ = root.Close()
		return nil, nil, nil, err
	}
	return root, directory, before, nil
}

func verifyRestoreParent(root *os.Root, directory *os.File, path string, before os.FileInfo) error {
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	descriptor, err := directory.Stat()
	if err != nil {
		return err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !opened.IsDir() || !descriptor.IsDir() || !os.SameFile(before, opened) || !os.SameFile(before, descriptor) || !os.SameFile(before, after) {
		return fmt.Errorf("restore target parent identity changed: %s", path)
	}
	return nil
}

func requireAbsentRestoreTarget(parent *os.Root, name, display string) error {
	if _, err := parent.Lstat(name); err == nil {
		return fmt.Errorf("target home %s already exists; restore target must be a new absent path", display)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func createRestoreStaging(parent *os.Root, parentFD *os.File, parentPath string) (string, *os.Root, os.FileInfo, error) {
	for i := 0; i < 32; i++ {
		name := restoreStagingPrefix + randomArchiveSuffix()
		display := filepath.Join(parentPath, name)
		if err := parent.Mkdir(name, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", nil, nil, err
		}
		created, err := parent.Lstat(name)
		if err != nil || created.Mode()&os.ModeSymlink != 0 || !created.IsDir() || created.Mode().Perm() != 0o700 || !ownedByCurrent(created) {
			cause := errors.Join(err, fmt.Errorf("created restore staging directory is not private and stable"))
			return "", nil, nil, failRestoreStagingAllocation(parent, nil, name, created, display, cause)
		}
		root, err := restoreOpenStaging(parent, name)
		if err != nil {
			return "", nil, nil, failRestoreStagingAllocation(parent, nil, name, created, display, fmt.Errorf("open pinned staging root: %w", err))
		}
		hookErr := restoreBeforeStagingValidation(display)
		info, err := parent.Lstat(name)
		opened, openErr := root.Stat(".")
		stable := err == nil && openErr == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() && opened.IsDir() && os.SameFile(created, info) && os.SameFile(created, opened)
		if hookErr != nil || !stable || info.Mode().Perm() != 0o700 || opened.Mode().Perm() != 0o700 || !ownedByCurrent(info) || !ownedByCurrent(opened) {
			cause := errors.Join(hookErr, err, openErr, fmt.Errorf("restore staging directory must remain stable and private"))
			return "", nil, nil, failRestoreStagingAllocation(parent, root, name, created, display, cause)
		}
		if err := restoreSyncParent(parentFD); err != nil {
			return "", nil, nil, failRestoreStagingAllocation(parent, root, name, created, display, fmt.Errorf("sync staging allocation: %w", err))
		}
		return name, root, info, nil
	}
	return "", nil, nil, fmt.Errorf("could not allocate private restore staging beneath %s", parentPath)
}

func failRestoreStagingAllocation(parent *os.Root, staging *os.Root, name string, before os.FileInfo, display string, cause error) error {
	result := fmt.Errorf("initialize private restore staging %s: %w", display, cause)
	if staging == nil || before == nil {
		result = errors.Join(result, fmt.Errorf("restore staging was not safely pinned; path left untouched at %s; inspect manually before retrying", display))
	} else if err := cleanupRestoreStaging(parent, staging, name, before, display); err != nil {
		result = errors.Join(result, fmt.Errorf("restore staging allocation cleanup incomplete for %s; inspect manually before retrying: %w", display, err))
	}
	if staging != nil {
		if err := staging.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close failed restore staging %s: %w", display, err))
		}
	}
	return result
}

func verifyRestoreStaging(parent, staging *os.Root, name string, before os.FileInfo, display string) error {
	leaf, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	opened, err := staging.Stat(".")
	if err != nil {
		return err
	}
	if leaf.Mode()&os.ModeSymlink != 0 || !leaf.IsDir() || !opened.IsDir() || !os.SameFile(before, leaf) || !os.SameFile(before, opened) || leaf.Mode().Perm() != 0o700 || opened.Mode().Perm() != 0o700 || !ownedByCurrent(leaf) || !ownedByCurrent(opened) {
		return fmt.Errorf("restore staging identity changed: %s", display)
	}
	return nil
}

func verifyPublishedRestoreTarget(parent, target *os.Root, name string, before os.FileInfo, display string) error {
	leaf, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	opened, err := target.Stat(".")
	if err != nil {
		return err
	}
	if leaf.Mode()&os.ModeSymlink != 0 || !leaf.IsDir() || !opened.IsDir() || !os.SameFile(before, leaf) || !os.SameFile(before, opened) || leaf.Mode().Perm() != 0o700 || opened.Mode().Perm() != 0o700 || !ownedByCurrent(leaf) || !ownedByCurrent(opened) {
		return fmt.Errorf("published restore target is not the private staged directory: %s", display)
	}
	return nil
}

func cleanupRestoreStaging(parent, staging *os.Root, name string, before os.FileInfo, display string) error {
	if staging == nil || before == nil {
		return fmt.Errorf("restore staging was not safely pinned; path left untouched at %s; inspect manually before retrying", display)
	}
	opened, err := staging.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) || opened.Mode().Perm() != 0o700 || !ownedByCurrent(opened) {
		return fmt.Errorf("restore staging descriptor is not the pinned private directory at %s; path left untouched; inspect manually before retrying: %w", display, err)
	}
	if err := clearRestoreStaging(staging, display); err != nil {
		return fmt.Errorf("clear pinned restore staging %s; private data may remain; inspect manually before retrying: %w", display, err)
	}
	leaf, err := parent.Lstat(name)
	if err != nil || leaf.Mode()&os.ModeSymlink != 0 || !leaf.IsDir() || !os.SameFile(before, leaf) || leaf.Mode().Perm() != 0o700 || !ownedByCurrent(leaf) {
		return fmt.Errorf("restore staging pathname no longer names the cleared pinned directory at %s; replacement was left untouched; inspect the parent manually before retrying: %w", display, err)
	}
	return fmt.Errorf("private restore staging retained empty at %s for explicit cleanup; inspect manually before retrying", display)
}

func clearRestoreStaging(staging *os.Root, display string) error {
	if err := restoreCleanupWalk(staging, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		if err := restoreCleanupRemove(staging, filepath.FromSlash(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	opened, err := staging.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := opened.ReadDir(1)
	closeErr := opened.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) != 0 {
		return fmt.Errorf("pinned restore staging is not empty after cleanup")
	}
	return restoreCleanupSync(staging, display)
}

type archiveRestoreTarget struct {
	root                *os.Root
	displayPath         string
	deferFileParentSync bool
	stagingInfo         os.FileInfo
	directoryModes      map[string]fs.FileMode
}

type archiveRestoreFile struct {
	root            *os.Root
	file            *os.File
	rel             string
	parentRel       string
	display         string
	openedInfo      os.FileInfo
	deferParentSync bool
}

func (t *archiveRestoreTarget) ensureDirectory(name string, mode fs.FileMode) error {
	rel := filepath.Clean(filepath.FromSlash(strings.TrimSuffix(name, "/")))
	if !filepath.IsLocal(rel) || rel == "." {
		return fmt.Errorf("invalid archive directory path %q", name)
	}
	parentRel := filepath.Dir(rel)
	if _, err := lstatRootedDirectoryPath(t.root, parentRel, filepath.Join(t.displayPath, parentRel), "archive restore", false); err != nil {
		return fmt.Errorf("archive directory parent must precede child %s: %w", name, err)
	}
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o755
	}
	// Restrictive final modes must not prevent extraction of later children.
	// The enclosing private staging root remains mode 0700 throughout.
	finalPerm := perm
	perm |= 0o700
	if _, err := t.root.Lstat(rel); errors.Is(err, os.ErrNotExist) {
		if err := t.root.Mkdir(rel, perm); err != nil {
			return err
		}
		if err := syncRootedDirectory(t.root, parentRel, filepath.Join(t.displayPath, parentRel), "archive restore"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := rootedChmodStable(t.root, rel, filepath.Join(t.displayPath, rel), "archive restore", perm, true, privateCustodyHooks{}); err != nil {
		return err
	}
	exists, err := rootedDirectoryExists(t.root, rel, filepath.Join(t.displayPath, rel), "archive restore")
	if err != nil || !exists {
		if err != nil {
			return err
		}
		return fmt.Errorf("archive directory disappeared after creation: %s", name)
	}
	leaf, err := t.root.Lstat(rel)
	if err != nil || leaf.Mode()&os.ModeSymlink != 0 || !leaf.IsDir() || leaf.Mode().Perm() != perm || !ownedByCurrent(leaf) {
		return fmt.Errorf("archive directory must be stable, current-user-owned, and mode %04o: %s", perm, name)
	}
	if err := syncRootedDirectory(t.root, rel, filepath.Join(t.displayPath, rel), "archive restore"); err != nil {
		return err
	}
	if perm != finalPerm {
		if t.directoryModes == nil {
			t.directoryModes = make(map[string]fs.FileMode)
		}
		t.directoryModes[rel] = finalPerm
	} else {
		delete(t.directoryModes, rel)
	}
	return nil
}

func (t *archiveRestoreTarget) finalizeDirectoryModes() error {
	paths := make([]string, 0, len(t.directoryModes))
	for path := range t.directoryModes {
		paths = append(paths, path)
	}
	// Children first: a parent's final mode may deny traversal to its children.
	sort.Slice(paths, func(i, j int) bool {
		a, b := strings.Count(paths[i], string(filepath.Separator)), strings.Count(paths[j], string(filepath.Separator))
		if a != b {
			return a > b
		}
		return paths[i] < paths[j]
	})
	for _, rel := range paths {
		if err := rootedChmodStable(t.root, rel, filepath.Join(t.displayPath, rel), "archive restore", t.directoryModes[rel], true, privateCustodyHooks{}); err != nil {
			return fmt.Errorf("restore directory permissions %s: %w", rel, err)
		}
	}
	t.directoryModes = nil
	return nil
}

func (t *archiveRestoreTarget) completeHabitat() error {
	manifestRaw, err := t.readRegularFile(ManifestName)
	if err != nil {
		return fmt.Errorf("read restored manifest: %w", err)
	}
	if _, err := decodeHomeManifest(manifestRaw); err != nil {
		return fmt.Errorf("validate restored manifest: %w", err)
	}
	for _, rel := range HabitatDirs() {
		if err := t.ensureHabitatDirectory(filepath.ToSlash(rel), archiveHabitatDirMode(rel)); err != nil {
			return err
		}
	}
	for _, rel := range privateCustodyKnownDirs {
		if err := t.ensureDirectory(filepath.ToSlash(rel), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (t *archiveRestoreTarget) ensureHabitatDirectory(rel string, missingMode fs.FileMode) error {
	rel = filepath.Clean(filepath.FromSlash(rel))
	info, err := t.root.Lstat(rel)
	if errors.Is(err, os.ErrNotExist) {
		return t.ensureDirectory(filepath.ToSlash(rel), missingMode)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ownedByCurrent(info) {
		return fmt.Errorf("restored habitat path must be a stable current-user-owned directory: %s", rel)
	}
	exists, err := rootedDirectoryExists(t.root, rel, filepath.Join(t.displayPath, rel), "archive restore")
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("restored habitat directory disappeared: %s", rel)
	}
	return nil
}

func (t *archiveRestoreTarget) readRegularFile(rel string) ([]byte, error) {
	before, err := t.root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("archive restore file must be regular: %s", rel)
	}
	file, err := t.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, manifestMaxBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := t.root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		return nil, fmt.Errorf("archive restore file identity changed while reading: %s", rel)
	}
	if int64(len(data)) > manifestMaxBytes {
		return nil, fmt.Errorf("archive restore file exceeds %d bytes: %s", manifestMaxBytes, rel)
	}
	return data, nil
}

func (t *archiveRestoreTarget) validatePinnedHome() error {
	return t.validatePinnedHomeWithStreamed(false)
}

func (t *archiveRestoreTarget) validatePinnedHomeWithStreamed(streamed bool) error {
	rootInfo, err := t.root.Stat(".")
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode().Perm() != 0o700 || !ownedByCurrent(rootInfo) {
		return fmt.Errorf("restore staging root must remain current-user-owned mode 0700: %s", t.displayPath)
	}
	if streamed {
		if err := validateStreamedSourceHealth(t.root); err != nil {
			return fmt.Errorf("restored home failed rooted habitat validation: %w", err)
		}
		return validateStreamedPrivateCustody(t.root, t.displayPath)
	}

	for _, rel := range append(append([]string(nil), HabitatDirs()...), privateCustodyKnownDirs...) {
		rel = filepath.FromSlash(rel)
		info, err := t.root.Lstat(rel)
		if err != nil {
			return fmt.Errorf("restored habitat directory missing %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("restored habitat path must be a directory: %s", rel)
		}
	}

	entries, err := fs.ReadDir(t.root.FS(), ".")
	if err != nil {
		return err
	}
	if inventory := inspectHabitatEntries(entries); !inventory.OK {
		return fmt.Errorf("restored home contains unknown habitat: roots=%v entries=%v", inventory.UnknownRoots, inventory.UnknownEntries)
	}

	manifestRaw, err := t.readRegularFile(ManifestName)
	if err != nil {
		return fmt.Errorf("read restored manifest: %w", err)
	}
	if _, err := decodeHomeManifest(manifestRaw); err != nil {
		return fmt.Errorf("validate restored manifest: %w", err)
	}
	privateReport := PrivateCustodyReport{
		SchemaVersion: PrivateCustodyReportSchema,
		Root:          filepath.Join(t.displayPath, "private", "artifacts", "sha256"),
		Status:        "ok",
		OK:            true,
	}
	safe := true
	for _, rel := range privateCustodyKnownDirs {
		if safe {
			safe = checkPrivateDir(&privateReport, t.root, t.displayPath, rel, PrivateCustodyOptions{}, privateCustodyHooks{})
		}
	}
	if safe {
		checkPrivateObjects(&privateReport, t.root, t.displayPath, PrivateCustodyOptions{}, privateCustodyHooks{})
	}
	privateReport.finish(PrivateCustodyOptions{})
	if !privateReport.OK {
		return fmt.Errorf("restored private custody failed rooted validation: status=%s findings=%v", privateReport.Status, privateReport.Findings)
	}
	return nil
}

func (t *archiveRestoreTarget) verifyEntries(manifest ArchiveManifest, limits archiveReadLimits) error {
	if len(manifest.Entries) > limits.MaxEntries {
		return fmt.Errorf("restored archive manifest entry count exceeds %d", limits.MaxEntries)
	}
	actual := make(map[string]ArchiveEntry, len(manifest.Entries))
	err := fs.WalkDir(t.root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." || entry.IsDir() {
			return nil
		}
		rel := filepath.FromSlash(path)
		before, err := t.root.Lstat(rel)
		if err != nil {
			return err
		}
		if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
			return fmt.Errorf("restored archive entry must remain regular: %s", path)
		}
		file, err := t.root.Open(rel)
		if err != nil {
			return err
		}
		opened, statErr := file.Stat()
		hash := sha256.New()
		n, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		after, afterErr := t.root.Lstat(rel)
		if err := errors.Join(statErr, copyErr, closeErr, afterErr); err != nil {
			return err
		}
		if !opened.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(before, after) {
			return fmt.Errorf("restored archive entry identity changed while verifying: %s", path)
		}
		actual[filepath.ToSlash(path)] = ArchiveEntry{Path: filepath.ToSlash(path), Size: n, SHA256: fmt.Sprintf("%x", hash.Sum(nil))}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := verifyArchiveManifestWithLimits(t.displayPath, manifest, actual, limits); err != nil {
		return fmt.Errorf("restored staging checksum verification failed: %w", err)
	}
	return nil
}

func (t *archiveRestoreTarget) createFile(name string, mode fs.FileMode) (*archiveRestoreFile, error) {
	rel := filepath.Clean(filepath.FromSlash(name))
	if !filepath.IsLocal(rel) || rel == "." {
		return nil, fmt.Errorf("invalid archive file path %q", name)
	}
	parentRel := filepath.Dir(rel)
	if parentRel != "." {
		if _, err := lstatRootedDirectoryPath(t.root, parentRel, filepath.Join(t.displayPath, parentRel), "archive restore", false); err != nil {
			return nil, fmt.Errorf("archive file parent must precede child %s: %w", name, err)
		}
	}
	if _, err := t.root.Lstat(rel); err == nil {
		return nil, fmt.Errorf("archive restore target already exists: %s", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o600
	}
	file, err := t.root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	leaf, err := t.root.Lstat(rel)
	if err != nil || leaf.Mode()&os.ModeSymlink != 0 || !leaf.Mode().IsRegular() || !opened.Mode().IsRegular() || !os.SameFile(leaf, opened) || leaf.Mode().Perm() != perm || opened.Mode().Perm() != perm || !ownedByCurrent(leaf) || !ownedByCurrent(opened) {
		_ = file.Close()
		return nil, fmt.Errorf("archive restore created unstable nonregular file %s", name)
	}
	if t.deferFileParentSync {
		if err := requireStreamedRestoreDevice(t.stagingInfo, opened); err != nil {
			return nil, errors.Join(err, file.Close())
		}
	}
	return &archiveRestoreFile{root: t.root, file: file, rel: rel, parentRel: parentRel, display: filepath.Join(t.displayPath, rel), openedInfo: opened, deferParentSync: t.deferFileParentSync}, nil
}

func (f *archiveRestoreFile) abort(cause error) error {
	_ = f.file.Close()
	return cause
}

func (f *archiveRestoreFile) commit() error {
	syncFile := (*os.File).Sync
	if f.deferParentSync {
		syncFile = restoreStreamedFileSync
	}
	if err := syncFile(f.file); err != nil {
		return f.abort(err)
	}
	if err := f.file.Close(); err != nil {
		return err
	}
	leaf, err := f.root.Lstat(f.rel)
	if err != nil || leaf.Mode()&os.ModeSymlink != 0 || !leaf.Mode().IsRegular() || !os.SameFile(leaf, f.openedInfo) {
		return fmt.Errorf("archive restore file identity changed after write: %s", f.display)
	}
	if f.deferParentSync {
		return nil
	}
	return restoreSyncStagingDirectory(f.root, f.parentRel, f.display, "archive restore")
}

func (t *archiveRestoreTarget) syncTree() error {
	return walkArchiveRootPostorderBatched(t.root, ".", func(path string, info fs.FileInfo) error {
		if !info.IsDir() {
			return nil
		}
		if err := restoreSyncStagingDirectory(t.root, path, filepath.Join(t.displayPath, path), "archive restore"); err != nil {
			return err
		}
		if t.deferFileParentSync {
			return syncStreamedRestoreDirectory(t.root, path, t.stagingInfo, info)
		}
		return nil
	})
}

func syncRestoreDirectory(file *os.File) error {
	syncErr := file.Sync()
	if syncErr != nil && !errors.Is(syncErr, fs.ErrInvalid) {
		return syncErr
	}
	return nil
}
