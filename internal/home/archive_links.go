package home

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	ArchiveFormatStreamedV3              = "streamed-v3"
	ArchiveFormatStreamedV3EventSnapshot = "streamed-v3-event-snapshot"
	ArchiveStreamSchemaV3                = "compound.home_archive.v0.3"
	archiveStreamMaxLinks                = 4096
)

// Links are bounded, checksum-bound index records, never tar link headers.
// Extraction finishes regular bodies before creating any link. The original
// spelling remains evidence; Target is its relocatable in-home equivalent.
type archiveLink struct {
	Path           string `json:"path"`
	OriginalTarget string `json:"original_target"`
	Target         string `json:"target"`
	SHA256         string `json:"sha256"`
}

var archiveAfterLinkRead = func(string) error { return nil }

func archiveLinkDigest(link archiveLink) string {
	sum := sha256.Sum256([]byte(link.Path + "\x00" + link.OriginalTarget + "\x00" + link.Target))
	return hex.EncodeToString(sum[:])
}

func validateArchiveLinkSourceHome(sourceHome string) error {
	if !utf8.ValidString(sourceHome) || !filepath.IsAbs(sourceHome) || filepath.Clean(sourceHome) != sourceHome || len(sourceHome) > archiveMaxPathBytes || strings.ContainsAny(sourceHome, "\x00\\") {
		return fmt.Errorf("link source home must be a bounded canonical UTF-8 absolute path")
	}
	return nil
}

func portableArchiveLink(sourceHome, name, original string) (string, error) {
	if err := validateArchiveLinkSourceHome(sourceHome); err != nil {
		return "", err
	}
	if !utf8.ValidString(name) || !utf8.ValidString(original) {
		return "", fmt.Errorf("archive link path and target must be UTF-8")
	}
	if err := validateArchivePath(name); err != nil {
		return "", err
	}
	if original == "" || len(original) > archiveMaxPathBytes || strings.ContainsAny(original, "\x00\\") {
		return "", fmt.Errorf("archive link target is empty, oversized or nonportable: %s", name)
	}
	// Cleaning can change symlink semantics (notably file/ and alias/../file).
	// Refuse unsupported spellings rather than silently changing their meaning.
	if filepath.Clean(original) != original {
		return "", fmt.Errorf("archive link target is not canonical: %s", name)
	}
	parent := filepath.Join(sourceHome, filepath.Dir(filepath.FromSlash(name)))
	destination := original
	if !filepath.IsAbs(original) {
		destination = filepath.Join(parent, original)
	}
	rel, err := filepath.Rel(sourceHome, destination)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("archive link escapes source home: %s", name)
	}
	if len(filepath.ToSlash(rel)) > archiveMaxPathBytes || len(strings.Split(filepath.ToSlash(rel), "/")) > archiveMaxPathDepth {
		return "", fmt.Errorf("archive link target exceeds path bounds: %s", name)
	}
	portable, err := filepath.Rel(parent, filepath.Join(sourceHome, rel))
	if err != nil {
		return "", err
	}
	portable = filepath.ToSlash(portable)
	if len(portable) > archiveMaxPathBytes || len(strings.Split(portable, "/")) > archiveMaxPathDepth {
		return "", fmt.Errorf("archive portable link target exceeds path bounds: %s", name)
	}
	return portable, nil
}

func captureArchiveLink(root *os.Root, homePath, name string, before fs.FileInfo) (archiveLink, error) {
	link := archiveLink{Path: name}
	if before.Mode()&os.ModeSymlink == 0 {
		return link, fmt.Errorf("archive source is not a link: %s", name)
	}
	var err error
	link.OriginalTarget, err = root.Readlink(filepath.FromSlash(name))
	if err != nil {
		return link, err
	}
	if err := archiveAfterLinkRead(name); err != nil {
		return link, err
	}
	after, err := root.Lstat(filepath.FromSlash(name))
	if err != nil || after.Mode()&os.ModeSymlink == 0 || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return link, fmt.Errorf("archive link identity changed during capture: %s", name)
	}
	again, err := root.Readlink(filepath.FromSlash(name))
	if err != nil || again != link.OriginalTarget {
		return link, fmt.Errorf("archive link target changed during capture: %s", name)
	}
	link.Target, err = portableArchiveLink(homePath, name, link.OriginalTarget)
	if err != nil {
		return link, err
	}
	link.SHA256 = archiveLinkDigest(link)
	return link, nil
}

func validateArchiveLinks(index archiveStreamIndex, schema string, seen map[[sha256.Size]byte]bool, budget *archiveEntryBudget) error {
	if schema == ArchiveStreamSchemaV2 {
		if index.SourceHome != "" || len(index.Links) != 0 {
			return fmt.Errorf("v0.2 archive cannot declare links")
		}
		return nil
	}
	if len(index.Links) > archiveStreamMaxLinks {
		return fmt.Errorf("archive link count exceeds %d", archiveStreamMaxLinks)
	}
	if err := validateArchiveLinkSourceHome(index.SourceHome); err != nil {
		return err
	}
	for _, link := range index.Links {
		if strings.HasSuffix(link.Path, "/") || link.Path == ManifestName || !habitatBackupIncluded(link.Path, false) {
			return fmt.Errorf("archive link path excluded or reserved: %s", link.Path)
		}
		target, err := portableArchiveLink(index.SourceHome, link.Path, link.OriginalTarget)
		if err != nil {
			return err
		}
		if target != link.Target || archiveLinkDigest(link) != link.SHA256 {
			return fmt.Errorf("archive link metadata mismatch: %s", link.Path)
		}
		key := sha256.Sum256([]byte(link.Path))
		if _, exists := seen[key]; exists {
			return fmt.Errorf("archive link duplicates entry: %s", link.Path)
		}
		parent := filepath.ToSlash(filepath.Dir(link.Path))
		if parent != "." && !seen[sha256.Sum256([]byte(parent))] {
			return fmt.Errorf("archive link parent is not an archived directory: %s", link.Path)
		}
		if err := budget.observe(&tar.Header{Name: link.Path, Typeflag: tar.TypeSymlink}); err != nil {
			return err
		}
		seen[key] = false
	}
	return nil
}

func createArchiveLinks(target *archiveRestoreTarget, links []archiveLink) error {
	for _, link := range links {
		parent := filepath.Dir(filepath.FromSlash(link.Path))
		if _, err := lstatRootedDirectoryPath(target.root, parent, filepath.Join(target.displayPath, parent), "archive restore link", false); err != nil {
			return err
		}
		if err := target.root.Symlink(filepath.FromSlash(link.Target), filepath.FromSlash(link.Path)); err != nil {
			return fmt.Errorf("restore archive link %s: %w", link.Path, err)
		}
	}
	return nil
}

func verifyArchiveLink(target *archiveRestoreTarget, link archiveLink) error {
	name := filepath.FromSlash(link.Path)
	before, err := target.root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("restored archive link missing or changed: %s", link.Path)
	}
	if err := requireStreamedRestoreDevice(target.stagingInfo, before); err != nil {
		return err
	}
	got, err := target.root.Readlink(name)
	if err != nil || got != filepath.FromSlash(link.Target) {
		return fmt.Errorf("restored archive link target mismatch: %s", link.Path)
	}
	after, err := target.root.Lstat(name)
	if err != nil || after.Mode()&os.ModeSymlink == 0 || !os.SameFile(before, after) {
		return fmt.Errorf("restored archive link identity changed: %s", link.Path)
	}
	return nil
}
