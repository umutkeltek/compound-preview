package home

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	ArchiveFormatStreamedV2   = "streamed-v2"
	ArchiveStreamSchemaV2     = "compound.home_archive.v0.2"
	ArchiveStreamHeaderName   = "compound-home-archive-header.json"
	archiveStreamIndexName    = "compound-home-archive-index.json"
	archiveStreamChunkPrefix  = "compound-home-archive-chunk-"
	archiveStreamHeaderBytes  = 4 << 10
	archiveStreamChunkBytes   = 8 << 20
	archiveStreamIndexBytes   = 1 << 20
	archiveStreamTotalMeta    = 1 << 30
	archiveStreamChunkRecords = 4096
	archiveStreamMaxChunks    = 1024
	archiveStreamMaxEntries   = 1_500_000
	archiveWalkBatch          = 256
)

type archiveStreamMetadataLimits struct {
	HeaderBytes         int64
	ChunkBytes          int64
	IndexBytes          int64
	TotalBytes          int64
	ChunkRecords        int
	Chunks              int
	HeaderOverhead      int64
	TotalHeaderOverhead int64
}

var defaultArchiveStreamMetadataLimits = archiveStreamMetadataLimits{
	HeaderBytes:         archiveStreamHeaderBytes,
	ChunkBytes:          archiveStreamChunkBytes,
	IndexBytes:          archiveStreamIndexBytes,
	TotalBytes:          archiveStreamTotalMeta,
	ChunkRecords:        archiveStreamChunkRecords,
	Chunks:              archiveStreamMaxChunks,
	HeaderOverhead:      16 << 10,
	TotalHeaderOverhead: 4 << 30,
}

func validateArchiveStreamMetadataLimits(l archiveStreamMetadataLimits) error {
	if l.HeaderBytes < 1 || l.HeaderBytes > archiveStreamHeaderBytes ||
		l.ChunkBytes < 1 || l.ChunkBytes > archiveStreamChunkBytes ||
		l.IndexBytes < 1 || l.IndexBytes > archiveStreamIndexBytes ||
		l.TotalBytes < 1 || l.TotalBytes > archiveStreamTotalMeta ||
		l.ChunkRecords < 1 || l.ChunkRecords > archiveStreamChunkRecords ||
		l.Chunks < 1 || l.Chunks > archiveStreamMaxChunks ||
		l.HeaderOverhead < 512 || l.HeaderOverhead > 16<<10 ||
		l.TotalHeaderOverhead < 512 || l.TotalHeaderOverhead > 4<<30 {
		return fmt.Errorf("invalid archive stream metadata limits")
	}
	return nil
}

type archiveStreamHeader struct {
	SchemaVersion string `json:"schema_version"`
	Kind          string `json:"kind"`
}

type archiveStreamChunk struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Records int    `json:"records"`
}

type archiveStreamIndex struct {
	SchemaVersion     string               `json:"schema_version"`
	Kind              string               `json:"kind"`
	CreatedAt         string               `json:"created_at"`
	HomeSchemaVersion string               `json:"home_schema_version"`
	ExcludedRoots     []string             `json:"excluded_roots"`
	Files             int                  `json:"files"`
	Bytes             int64                `json:"bytes"`
	Members           int                  `json:"members"`
	Chunks            []archiveStreamChunk `json:"chunks"`
	SourceHome        string               `json:"source_home,omitempty"`
	Links             []archiveLink        `json:"links,omitempty"`
}

func archiveStreamTarLimits() archiveReadLimits {
	limits := defaultArchiveReadLimits
	limits.MaxEntries = archiveStreamMaxEntries
	return limits
}

func archiveStreamTarLimitsFrom(admitted archiveReadLimits) archiveReadLimits {
	if admitted == defaultArchiveReadLimits {
		admitted.MaxEntries = archiveStreamMaxEntries
	} else if admitted.MaxEntries > archiveStreamMaxEntries {
		admitted.MaxEntries = archiveStreamMaxEntries
	}
	return admitted
}

func archiveStreamChunkName(n int) string {
	return archiveStreamChunkPrefix + fmt.Sprintf("%06d.json", n)
}

type archiveStreamChunkWriter struct {
	tar       archiveEntryWriter
	limits    archiveStreamMetadataLimits
	buf       bytes.Buffer
	records   int
	metaBytes int64
	chunks    []archiveStreamChunk
}

var archiveStreamRootOpened = func(string) error { return nil }

// reserve flushes the preceding chunk before the next file enters the tar.
// A SHA-256 hex digest has a fixed length, so this record has the same encoded
// size as the record produced after streaming the file.
func (w *archiveStreamChunkWriter) reserve(path string, size int64) error {
	record, err := json.Marshal(ArchiveEntry{Path: path, Size: size, SHA256: strings.Repeat("0", sha256.Size*2)})
	if err != nil {
		return err
	}
	if int64(len(record)+2) > w.limits.ChunkBytes {
		return fmt.Errorf("archive manifest record exceeds %d bytes", w.limits.ChunkBytes)
	}
	comma := 0
	if w.records > 0 {
		comma = 1
	}
	if w.records == w.limits.ChunkRecords || int64(w.buf.Len()+comma+len(record)+1) > w.limits.ChunkBytes {
		return w.flush()
	}
	return nil
}

func (w *archiveStreamChunkWriter) add(entry ArchiveEntry) error {
	record, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if int64(len(record)+2) > w.limits.ChunkBytes {
		return fmt.Errorf("archive manifest record exceeds %d bytes", w.limits.ChunkBytes)
	}
	comma := 0
	if w.records > 0 {
		comma = 1
	}
	if w.records == w.limits.ChunkRecords || int64(w.buf.Len()+comma+len(record)+1) > w.limits.ChunkBytes {
		return fmt.Errorf("archive manifest chunk capacity was not reserved before file %s", entry.Path)
	}
	if w.records == 0 {
		w.buf.WriteByte('[')
	} else {
		w.buf.WriteByte(',')
	}
	w.buf.Write(record)
	w.records++
	if w.records == w.limits.ChunkRecords {
		return w.flush()
	}
	return nil
}

func (w *archiveStreamChunkWriter) flush() error {
	if w.records == 0 {
		return nil
	}
	if len(w.chunks) >= w.limits.Chunks {
		return fmt.Errorf("archive manifest chunk count exceeds %d", w.limits.Chunks)
	}
	w.buf.WriteByte(']')
	data := w.buf.Bytes()
	if w.metaBytes > w.limits.TotalBytes-int64(len(data)) {
		return fmt.Errorf("archive manifest metadata exceeds %d bytes", w.limits.TotalBytes)
	}
	name := archiveStreamChunkName(len(w.chunks))
	entry, err := writeBytesEntry(w.tar, name, data, 0o600)
	if err != nil {
		return err
	}
	w.chunks = append(w.chunks, archiveStreamChunk{Name: name, Size: entry.Size, SHA256: entry.SHA256, Records: w.records})
	w.metaBytes += entry.Size
	w.buf.Reset()
	w.records = 0
	return nil
}

func backupStreamed(homePath, targetPath string, now time.Time) (ArchiveReport, error) {
	return backupStreamedVersion(homePath, targetPath, now, ArchiveStreamSchemaV2)
}

func backupStreamedVersion(homePath, targetPath string, now time.Time, schema string) (ArchiveReport, error) {
	return backupStreamedVersionOptions(homePath, targetPath, now, schema, false)
}

func backupStreamedVersionOptions(homePath, targetPath string, now time.Time, schema string, snapshotEvents bool) (ArchiveReport, error) {
	if snapshotEvents {
		if err := requireArchiveEventSnapshotSupport(); err != nil {
			return ArchiveReport{}, err
		}
	}

	homePath, targetPath, err := normalizeArchiveInputs(homePath, targetPath, ArchiveKindBackup, now)
	if err != nil {
		return ArchiveReport{}, err
	}
	if snapshotEvents {
		return backupCapturedStreamedArchive(homePath, targetPath, now, schema)
	}
	var report ArchiveReport
	err = WithAutonomyAdmissionLock(homePath, func() error {
		if err := prepareArchiveTarget(homePath, targetPath, includeBackupPath); err != nil {
			return err
		}
		var writeErr error
		report, writeErr = writeStreamedHomeArchiveOptions(homePath, targetPath, now, schema, snapshotEvents)
		return writeErr
	})
	if err != nil {
		return ArchiveReport{}, err
	}
	return report, nil
}

func writeStreamedHomeArchiveVersion(homePath, targetPath string, now time.Time, schema string) (ArchiveReport, error) {
	return writeStreamedHomeArchiveOptions(homePath, targetPath, now, schema, false)
}

func writeStreamedHomeArchiveOptions(homePath, targetPath string, now time.Time, schema string, snapshotEvents bool) (ArchiveReport, error) {
	source, err := openLiveArchiveStreamSource(homePath, schema, snapshotEvents)
	if err != nil {
		return ArchiveReport{}, err
	}
	defer source.close()
	return writeStreamedArchiveSource(homePath, targetPath, now, schema, source)
}

func writeStreamedArchiveSource(homePath, targetPath string, now time.Time, schema string, source *archiveStreamSource) (ArchiveReport, error) {

	metadataLimits := defaultArchiveStreamMetadataLimits
	if err := validateArchiveStreamMetadataLimits(metadataLimits); err != nil {
		return ArchiveReport{}, err
	}
	aw, err := beginArchivePublication(homePath, targetPath)
	if err != nil {
		return ArchiveReport{}, err
	}
	defer aw.cleanup()
	tw, gz, err := newBudgetedArchiveWriters(aw.file, archiveStreamTarLimits())
	if err != nil {
		return ArchiveReport{}, err
	}
	header, err := json.Marshal(archiveStreamHeader{SchemaVersion: schema, Kind: ArchiveKindBackup})
	if err != nil {
		return ArchiveReport{}, err
	}
	if int64(len(header)) > metadataLimits.HeaderBytes {
		return ArchiveReport{}, fmt.Errorf("archive stream header exceeds %d bytes", metadataLimits.HeaderBytes)
	}
	if _, err := writeBytesEntry(tw, ArchiveStreamHeaderName, header, 0o600); err != nil {
		return ArchiveReport{}, err
	}
	manifestEntry, err := writeBytesEntry(tw, ManifestName, source.manifest, 0o644)
	if err != nil {
		return ArchiveReport{}, err
	}
	chunks := &archiveStreamChunkWriter{tar: tw, limits: metadataLimits}
	if err := chunks.add(manifestEntry); err != nil {
		return ArchiveReport{}, err
	}
	files, bytesWritten := 1, manifestEntry.Size
	var links []archiveLink
	seen := make(map[[sha256.Size]byte]struct{})
	seen[sha256.Sum256([]byte(ManifestName))] = struct{}{}
	err = source.walk(func(item archiveStreamItem) error {
		rel, info := item.rel, item.info
		if rel == ManifestName {
			return nil
		}
		if !includeBackupPath(rel, info) {
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() && !(schema == ArchiveStreamSchemaV3 && info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("archive included source entry must be regular file or directory: %s", rel)
		}
		key := sha256.Sum256([]byte(rel))
		if _, exists := seen[key]; exists {
			return fmt.Errorf("archive contains duplicate source entry %s", rel)
		}
		seen[key] = struct{}{}
		if info.Mode()&os.ModeSymlink != 0 {
			if len(links) >= archiveStreamMaxLinks {
				return fmt.Errorf("archive link count exceeds %d", archiveStreamMaxLinks)
			}
			link, err := item.link()
			if err != nil {
				return err
			}
			if err := tw.budget.observe(&tar.Header{Name: rel, Typeflag: tar.TypeSymlink}); err != nil {
				return err
			}
			links = append(links, link)
			files++
			return nil
		}
		if info.IsDir() {
			return writeDirEntry(tw, rel, info.Mode())
		}
		if err := chunks.reserve(rel, info.Size()); err != nil {
			return err
		}
		entry, entryErr := item.write(tw)
		if entryErr != nil {
			return entryErr
		}
		files++
		bytesWritten += entry.Size
		return chunks.add(entry)
	})
	if err != nil {
		return ArchiveReport{}, err
	}
	if err := chunks.flush(); err != nil {
		return ArchiveReport{}, err
	}
	index := archiveStreamIndex{
		SchemaVersion:     schema,
		Kind:              ArchiveKindBackup,
		CreatedAt:         now.UTC().Format(time.RFC3339),
		HomeSchemaVersion: source.homeSchema,
		ExcludedRoots:     append([]string(nil), backupExcludedRoots...),
		Files:             files,
		Bytes:             bytesWritten,
		Members:           tw.budget.entries + 1,
		Chunks:            chunks.chunks,
	}
	if schema == ArchiveStreamSchemaV3 {
		index.SourceHome, index.Links = homePath, links
	}
	indexBytes, err := json.Marshal(index)
	if err != nil {
		return ArchiveReport{}, err
	}
	if int64(len(indexBytes)) > metadataLimits.IndexBytes || chunks.metaBytes > metadataLimits.TotalBytes-int64(len(indexBytes)) {
		return ArchiveReport{}, fmt.Errorf("archive stream index or metadata exceeds limit")
	}
	if _, err := writeBytesEntry(tw, archiveStreamIndexName, indexBytes, 0o600); err != nil {
		return ArchiveReport{}, err
	}
	if err := closeArchiveWriters(tw, gz); err != nil {
		return ArchiveReport{}, err
	}
	if err := aw.publish(); err != nil {
		return ArchiveReport{}, err
	}
	return ArchiveReport{Kind: ArchiveKindBackup, ArchivePath: targetPath, Files: files, Bytes: bytesWritten, Skipped: index.ExcludedRoots}, nil
}

func openStableArchiveRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("archive home root must be a non-symlink directory: %s", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err == nil {
		var after os.FileInfo
		after, err = os.Lstat(path)
		if err == nil && (after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(before, after)) {
			err = fmt.Errorf("archive home root identity changed: %s", path)
		}
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

func readRootedHomeManifestBounded(root *os.Root) ([]byte, error) {
	before, err := root.Lstat(ManifestName)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() > manifestMaxBytes {
		return nil, fmt.Errorf("home manifest must be bounded non-symlink regular file")
	}
	f, err := root.Open(ManifestName)
	if err != nil {
		return nil, err
	}
	opened, statErr := f.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = f.Close()
		return nil, fmt.Errorf("home manifest identity changed while opening: %w", statErr)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, manifestMaxBytes+1))
	closeErr := f.Close()
	after, afterErr := root.Lstat(ManifestName)
	if err := errors.Join(readErr, closeErr, afterErr); err != nil {
		return nil, err
	}
	if int64(len(data)) > manifestMaxBytes || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(before, after) || int64(len(data)) != opened.Size() {
		return nil, fmt.Errorf("home manifest size or identity changed while reading")
	}
	return data, nil
}

func validateStreamedSourceHealth(root *os.Root) error {
	manifest, err := readRootedHomeManifestBounded(root)
	if err != nil {
		return fmt.Errorf("home manifest: %w", err)
	}
	if _, err := decodeHomeManifest(manifest); err != nil {
		return err
	}
	for _, rel := range HabitatDirs() {
		info, err := root.Lstat(filepath.FromSlash(rel))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("home habitat directory missing or unsafe %s: %w", rel, err)
		}
	}
	return validateStreamedHabitatRoot(root)
}

func validateStreamedHabitatRoot(root *os.Root) error {
	before, err := root.Lstat(".")
	if err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	opened, statErr := dir.Stat()
	if statErr != nil || !os.SameFile(before, opened) {
		_ = dir.Close()
		return fmt.Errorf("home root identity changed during habitat inventory: %w", statErr)
	}
	for {
		batch, readErr := dir.ReadDir(archiveWalkBatch)
		if inventory := inspectHabitatEntries(batch); !inventory.OK {
			_ = dir.Close()
			return fmt.Errorf("home contains unknown habitat entries: roots=%v entries=%v", inventory.UnknownRoots, inventory.UnknownEntries)
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
	after, afterErr := root.Lstat(".")
	if err := errors.Join(closeErr, afterErr); err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return fmt.Errorf("home root identity changed during habitat inventory")
	}
	return nil
}

// walkArchiveRootBatched never materializes a full directory listing. The
// callback receives directories before their children and may skip a subtree
// by returning fs.SkipDir.
func walkArchiveRootBatched(root *os.Root, dir string, visit func(string, fs.FileInfo) error) error {
	opened, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer opened.Close()
	before, err := root.Lstat(dir)
	if err != nil {
		return err
	}
	openedInfo, err := opened.Stat()
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() || !os.SameFile(before, openedInfo) {
		return fmt.Errorf("archive directory identity changed: %s: %w", dir, err)
	}
	local, err := root.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer local.Close()
	localInfo, err := local.Stat(".")
	if err != nil || !os.SameFile(before, localInfo) {
		return fmt.Errorf("archive directory identity changed while pinning children: %s: %w", dir, err)
	}
	for {
		batch, readErr := opened.ReadDir(archiveWalkBatch)
		for _, child := range batch {
			rel := child.Name()
			if dir != "." {
				rel = filepath.ToSlash(filepath.Join(dir, rel))
			}
			if err := validateArchivePath(rel); err != nil {
				return err
			}
			info, err := local.Lstat(child.Name())
			if err != nil {
				return err
			}
			if err := visit(rel, info); err != nil {
				if errors.Is(err, fs.SkipDir) && info.IsDir() {
					continue
				}
				return err
			}
			if info.IsDir() {
				if err := walkArchiveRootBatched(root, filepath.FromSlash(rel), visit); err != nil {
					return err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	after, err := root.Lstat(dir)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) {
		return fmt.Errorf("archive directory identity changed after traversal: %s: %w", dir, err)
	}
	return nil
}

// walkArchiveRootPostorderBatched visits a child before its parent, holding
// only one directory batch per level. Removal and sync can therefore act on
// each entry without retaining a whole-tree path list.
func walkArchiveRootPostorderBatched(root *os.Root, dir string, visit func(string, fs.FileInfo) error) error {
	before, err := root.Lstat(dir)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return fmt.Errorf("archive directory must remain non-symlink: %s", dir)
	}
	opened, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer opened.Close()
	openedInfo, err := opened.Stat()
	if err != nil || !os.SameFile(before, openedInfo) {
		return fmt.Errorf("archive directory identity changed: %s: %w", dir, err)
	}
	for {
		batch, readErr := opened.ReadDir(archiveWalkBatch)
		for _, child := range batch {
			rel := child.Name()
			if dir != "." {
				rel = filepath.Join(dir, rel)
			}
			info, err := root.Lstat(rel)
			if err != nil {
				return err
			}
			if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				if err := walkArchiveRootPostorderBatched(root, rel, visit); err != nil {
					return err
				}
			} else if err := visit(rel, info); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	after, err := root.Lstat(dir)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) {
		return fmt.Errorf("archive directory identity changed after traversal: %s: %w", dir, err)
	}
	return visit(dir, after)
}

// walkRestoreCleanupBatched reopens each directory for every batch. Removing
// entries from an active readdir stream can skip siblings on some filesystems.
func walkRestoreCleanupBatched(root *os.Root, dir string, visit func(string, fs.FileInfo) error) error {
	before, err := root.Lstat(dir)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return fmt.Errorf("restore cleanup directory must remain non-symlink: %s", dir)
	}
	visited := 0
	for {
		opened, err := root.Open(dir)
		if err != nil {
			return err
		}
		openedInfo, statErr := opened.Stat()
		if statErr != nil || !os.SameFile(before, openedInfo) {
			_ = opened.Close()
			return fmt.Errorf("restore cleanup directory identity changed: %s: %w", dir, statErr)
		}
		batch, readErr := opened.ReadDir(archiveWalkBatch)
		closeErr := opened.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return errors.Join(readErr, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if len(batch) == 0 {
			break
		}
		visited += len(batch)
		if visited > archiveStreamMaxEntries {
			return fmt.Errorf("restore cleanup visits exceed %d entries in %s", archiveStreamMaxEntries, dir)
		}
		for _, child := range batch {
			rel := child.Name()
			if dir != "." {
				rel = filepath.Join(dir, rel)
			}
			info, err := root.Lstat(rel)
			if err != nil {
				return err
			}
			if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				if err := walkRestoreCleanupBatched(root, rel, visit); err != nil {
					return err
				}
			} else if err := visit(rel, info); err != nil {
				return err
			}
		}
	}
	after, err := root.Lstat(dir)
	if err != nil || !os.SameFile(before, after) {
		return fmt.Errorf("restore cleanup directory identity changed after traversal: %s: %w", dir, err)
	}
	return visit(dir, after)
}

func writeRootedArchiveFile(tw archiveEntryWriter, root *os.Root, rel string, before fs.FileInfo) (ArchiveEntry, error) {
	return writeRootedArchiveFileAt(tw, root, rel, rel, before, nil)
}

func writeRootedArchiveFileAt(tw archiveEntryWriter, root *os.Root, sourceRel, rel string, before fs.FileInfo, metadataOverride fs.FileInfo) (entry ArchiveEntry, resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("archive entry %q: %w", rel, resultErr)
		}
	}()
	if !before.Mode().IsRegular() {
		return ArchiveEntry{}, fmt.Errorf("source must be regular")
	}
	f, err := root.Open(filepath.FromSlash(sourceRel))
	if err != nil {
		return ArchiveEntry{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return ArchiveEntry{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || before.Size() != opened.Size() {
		return ArchiveEntry{}, fmt.Errorf("source identity changed before copying")
	}
	headerInfo := opened
	if metadataOverride != nil {
		headerInfo = metadataOverride
	}
	hdr, err := archiveFileInfoHeader(headerInfo, "")
	if err != nil {
		return ArchiveEntry{}, err
	}
	hdr.Name = rel
	if err := tw.WriteHeader(hdr); err != nil {
		return ArchiveEntry{}, err
	}
	sum := sha256.New()
	n, err := io.Copy(tw, io.TeeReader(f, sum))
	if err != nil {
		return ArchiveEntry{}, err
	}
	if n != opened.Size() {
		return ArchiveEntry{}, fmt.Errorf("source size changed while copying (expected %d, read %d): %w", opened.Size(), n, io.ErrUnexpectedEOF)
	}
	after, err := root.Lstat(filepath.FromSlash(sourceRel))
	if err != nil {
		return ArchiveEntry{}, err
	}
	end, err := f.Stat()
	if err != nil {
		return ArchiveEntry{}, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(before, after) || !os.SameFile(before, end) || end.Size() != opened.Size() || !end.ModTime().Equal(opened.ModTime()) {
		return ArchiveEntry{}, fmt.Errorf("source identity or metadata changed while copying")
	}
	return ArchiveEntry{Path: rel, Size: n, SHA256: hex.EncodeToString(sum.Sum(nil))}, nil
}

func archiveStreamChunkNumber(name string) (int, bool) {
	if !strings.HasPrefix(name, archiveStreamChunkPrefix) || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, archiveStreamChunkPrefix), ".json"))
	return n, err == nil && archiveStreamChunkName(n) == name
}
