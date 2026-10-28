package home

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// validateStreamedPrivateCustody applies the same digest, ownership and mode
// checks as the diagnostic report without retaining an okay finding per file.
func validateStreamedPrivateCustody(root *os.Root, homePath string) error {
	report := PrivateCustodyReport{SchemaVersion: PrivateCustodyReportSchema, Root: homePath, OK: true}
	for _, rel := range privateCustodyKnownDirs {
		if !checkPrivateDir(&report, root, homePath, rel, PrivateCustodyOptions{}, privateCustodyHooks{}) {
			report.finish(PrivateCustodyOptions{})
			return fmt.Errorf("restored private custody failed rooted validation: status=%s findings=%v", report.Status, report.Findings)
		}
	}
	report.finish(PrivateCustodyOptions{})
	if !report.OK {
		return fmt.Errorf("restored private custody failed rooted validation: status=%s findings=%v", report.Status, report.Findings)
	}
	rootRel := filepath.FromSlash("private/artifacts/sha256")
	before, err := root.Lstat(rootRel)
	if err != nil {
		return err
	}
	dir, err := root.Open(rootRel)
	if err != nil {
		return err
	}
	opened, statErr := dir.Stat()
	if statErr != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		_ = dir.Close()
		return fmt.Errorf("private digest directory identity changed: %w", statErr)
	}
	checked := 0
	for {
		batch, readErr := dir.ReadDir(archiveWalkBatch)
		for _, child := range batch {
			checked++
			if checked > archiveStreamMaxEntries {
				_ = dir.Close()
				return fmt.Errorf("private digest object count exceeds %d", archiveStreamMaxEntries)
			}
			name := child.Name()
			if !validPrivateDigestName(name) {
				_ = dir.Close()
				return fmt.Errorf("invalid private digest object name %s", name)
			}
			rel := filepath.Join(rootRel, name)
			info, err := stableDigestInfo(root, rel, absDisplay(homePath, rel), name)
			if err != nil {
				_ = dir.Close()
				return fmt.Errorf("invalid private digest object %s: %w", name, err)
			}
			if !ownedByCurrent(info) || info.Mode().Perm() != 0o600 {
				_ = dir.Close()
				return fmt.Errorf("private digest object must be current-user-owned mode 0600: %s", name)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = dir.Close()
			return readErr
		}
	}
	closeErr := dir.Close()
	after, afterErr := root.Lstat(rootRel)
	if err := errors.Join(closeErr, afterErr); err != nil {
		return err
	}
	if after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) {
		return fmt.Errorf("private digest directory identity changed while scanning")
	}
	return nil
}
