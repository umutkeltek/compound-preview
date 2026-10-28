package home

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// probeStreamedArchive inspects the first raw tar block. A v0.1 archive may
// begin with arbitrarily sized PAX metadata before its first logical member,
// so using tar.Reader.Next here would narrow the legacy read contract.
func probeStreamedArchive(f *os.File) (streamed bool, resultErr error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	defer func() {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("rewind archive source: %w", err))
		}
	}()
	gz, err := gzip.NewReader(newBoundedArchiveSource(f, defaultArchiveReadLimits.MaxSourceBytes))
	if err != nil {
		return false, err
	}
	defer gz.Close()
	gz.Multistream(false)
	var first [512]byte
	if _, err := io.ReadFull(gz, first[:]); err != nil {
		return false, err
	}
	return string(bytes.TrimRight(first[:100], "\x00")) == ArchiveStreamHeaderName, nil
}

func decodeStrictArchiveJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	canonical, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, canonical) {
		return fmt.Errorf("non-canonical or duplicate JSON fields")
	}
	return nil
}

func readStreamedArchive(archivePath string, f *os.File, restore, staged *archiveRestoreTarget, digest io.Writer) (VerifyReport, error) {
	return readStreamedArchiveWithLimits(archivePath, f, restore, staged, digest, archiveStreamTarLimits())
}

func readStreamedArchiveWithLimits(archivePath string, f *os.File, restore, staged *archiveRestoreTarget, digest io.Writer, limits archiveReadLimits) (VerifyReport, error) {
	return readStreamedArchiveWithMetadataLimits(archivePath, f, restore, staged, digest, limits, defaultArchiveStreamMetadataLimits)
}

func readStreamedArchiveWithMetadataLimits(archivePath string, f *os.File, restore, staged *archiveRestoreTarget, digest io.Writer, limits archiveReadLimits, metadataLimits archiveStreamMetadataLimits) (VerifyReport, error) {
	if err := validateArchiveReadLimits(limits); err != nil {
		return VerifyReport{}, err
	}
	if err := validateArchiveStreamMetadataLimits(metadataLimits); err != nil {
		return VerifyReport{}, err
	}
	var sourceReader io.Reader = f
	if digest != nil {
		sourceReader = io.TeeReader(f, digest)
	}
	source := newBoundedArchiveSource(sourceReader, limits.MaxSourceBytes)
	buffered := bufio.NewReader(source)
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return VerifyReport{}, err
	}
	defer gz.Close()
	gz.Multistream(false)
	decoded := newBoundedArchiveSource(gz, limits.MaxDecodedBytes)
	tr := tar.NewReader(decoded)
	budget := archiveEntryBudget{limits: limits}
	seen := make(map[[sha256.Size]byte]bool)
	pending := make([]ArchiveEntry, 0, metadataLimits.ChunkRecords)
	chunks := make([]archiveStreamChunk, 0, 256)
	var homeManifestBytes []byte
	var homeSchema string
	var streamSchema string
	var index archiveStreamIndex
	var files, archivedDirs int
	var bytesTotal, metaBytes, headerBytes int64
	indexSeen := false
	for {
		beforeNext := decoded.consumed
		hdr, nextErr := tr.Next()
		consumed := decoded.consumed - beforeNext
		if consumed > metadataLimits.HeaderOverhead || headerBytes > metadataLimits.TotalHeaderOverhead-consumed {
			return VerifyReport{}, fmt.Errorf("archive tar header metadata exceeds bounded profile")
		}
		headerBytes += consumed
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return VerifyReport{}, streamArchiveReadError(archivePath, source, decoded, nextErr)
		}
		name := filepath.ToSlash(hdr.Name)
		for key := range hdr.PAXRecords {
			if key != "path" && key != "mtime" {
				return VerifyReport{}, fmt.Errorf("archive contains unsupported PAX header %s", key)
			}
		}
		if len(hdr.Xattrs) != 0 || hdr.Linkname != "" {
			return VerifyReport{}, fmt.Errorf("archive contains unsupported tar header fields for %s", name)
		}
		if err := budget.observe(hdr); err != nil {
			return VerifyReport{}, err
		}
		canonical := strings.TrimSuffix(name, "/")
		key := sha256.Sum256([]byte(canonical))
		if _, exists := seen[key]; exists {
			return VerifyReport{}, fmt.Errorf("archive contains duplicate entry %s", name)
		}
		if parent := filepath.ToSlash(filepath.Dir(canonical)); parent != "." {
			if !seen[sha256.Sum256([]byte(parent))] {
				return VerifyReport{}, fmt.Errorf("archive directory parent must precede child %s", name)
			}
		}
		seen[key] = hdr.Typeflag == tar.TypeDir
		if budget.entries == 1 {
			if name != ArchiveStreamHeaderName || (hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA) || hdr.Size < 1 || hdr.Size > metadataLimits.HeaderBytes {
				return VerifyReport{}, fmt.Errorf("archive stream header must be the first bounded regular member")
			}
			data, err := io.ReadAll(io.LimitReader(tr, metadataLimits.HeaderBytes+1))
			if err != nil {
				return VerifyReport{}, streamArchiveReadError(archivePath, source, decoded, err)
			}
			var header archiveStreamHeader
			if err := decodeStrictArchiveJSON(data, &header); err != nil {
				return VerifyReport{}, fmt.Errorf("parse archive stream header: %w", err)
			}
			if (header.SchemaVersion != ArchiveStreamSchemaV2 && header.SchemaVersion != ArchiveStreamSchemaV3) || header.Kind != ArchiveKindBackup {
				return VerifyReport{}, asArchiveCompatibilityError(fmt.Errorf("unsupported archive stream descriptor %q/%q", header.SchemaVersion, header.Kind))
			}
			streamSchema = header.SchemaVersion
			continue
		}
		if indexSeen {
			return VerifyReport{}, fmt.Errorf("archive contains member after final index: %s", name)
		}
		if len(pending) == metadataLimits.ChunkRecords && archiveStreamChunkName(len(chunks)) != name {
			return VerifyReport{}, fmt.Errorf("archive manifest chunk required after %d files", metadataLimits.ChunkRecords)
		}
		if name == archiveStreamIndexName {
			if len(pending) != 0 || len(chunks) == 0 || hdr.Typeflag != tar.TypeReg || hdr.Size < 1 || hdr.Size > metadataLimits.IndexBytes {
				return VerifyReport{}, fmt.Errorf("archive stream index is missing chunks or exceeds %d bytes", metadataLimits.IndexBytes)
			}
			data, err := io.ReadAll(io.LimitReader(tr, metadataLimits.IndexBytes+1))
			if err != nil {
				return VerifyReport{}, streamArchiveReadError(archivePath, source, decoded, err)
			}
			if metaBytes > metadataLimits.TotalBytes-int64(len(data)) {
				return VerifyReport{}, fmt.Errorf("archive stream metadata exceeds %d bytes", metadataLimits.TotalBytes)
			}
			if err := decodeStrictArchiveJSON(data, &index); err != nil {
				return VerifyReport{}, fmt.Errorf("parse archive stream index: %w", err)
			}
			if err := validateArchiveLinks(index, streamSchema, seen, &budget); err != nil {
				return VerifyReport{}, err
			}
			files += len(index.Links)
			if err := validateStreamIndex(index, chunks, files, bytesTotal, budget.entries, homeSchema, streamSchema); err != nil {
				return VerifyReport{}, err
			}
			indexSeen = true
			continue
		}
		if n, isChunk := archiveStreamChunkNumber(name); isChunk {
			if n != len(chunks) || len(pending) == 0 || hdr.Typeflag != tar.TypeReg || hdr.Size < 1 || hdr.Size > metadataLimits.ChunkBytes || len(chunks) >= metadataLimits.Chunks {
				return VerifyReport{}, fmt.Errorf("archive manifest chunk missing, reordered, or over limit: %s", name)
			}
			data, err := io.ReadAll(io.LimitReader(tr, metadataLimits.ChunkBytes+1))
			if err != nil {
				return VerifyReport{}, streamArchiveReadError(archivePath, source, decoded, err)
			}
			if metaBytes > metadataLimits.TotalBytes-int64(len(data)) {
				return VerifyReport{}, fmt.Errorf("archive stream metadata exceeds %d bytes", metadataLimits.TotalBytes)
			}
			if err := checkStreamChunk(data, pending, staged, metadataLimits.ChunkBytes); err != nil {
				return VerifyReport{}, err
			}
			sum := sha256.Sum256(data)
			chunks = append(chunks, archiveStreamChunk{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Records: len(pending)})
			metaBytes += int64(len(data))
			pending = pending[:0]
			continue
		}
		if name == ArchiveStreamHeaderName || strings.HasPrefix(name, archiveStreamChunkPrefix) || name == ArchiveManifestName {
			return VerifyReport{}, fmt.Errorf("archive contains invalid metadata member %s", name)
		}
		if files == 0 && name != ManifestName {
			return VerifyReport{}, fmt.Errorf("embedded home manifest must be first archive file")
		}
		if hdr.Typeflag == tar.TypeDir {
			if hdr.Size != 0 {
				return VerifyReport{}, fmt.Errorf("archive directory entry %s declares %d bytes", name, hdr.Size)
			}
			if !habitatBackupIncluded(canonical, true) {
				return VerifyReport{}, fmt.Errorf("archive contains excluded backup directory %s", name)
			}
			archivedDirs++
			if restore != nil {
				if err := restore.ensureDirectory(name, fs.FileMode(hdr.Mode)); err != nil {
					return VerifyReport{}, err
				}
			}
			if staged != nil {
				info, err := staged.root.Lstat(filepath.FromSlash(canonical))
				if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != fs.FileMode(hdr.Mode).Perm() {
					return VerifyReport{}, fmt.Errorf("restored archive directory changed: %s: %w", canonical, err)
				}
			}
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return VerifyReport{}, fmt.Errorf("archive contains unsupported entry %s", name)
		}
		if strings.HasSuffix(name, "/") {
			return VerifyReport{}, fmt.Errorf("archive regular file path has trailing slash: %s", name)
		}
		if name != ManifestName && !habitatBackupIncluded(name, false) {
			return VerifyReport{}, fmt.Errorf("archive contains excluded backup file %s", name)
		}
		if len(pending) >= metadataLimits.ChunkRecords {
			return VerifyReport{}, fmt.Errorf("archive manifest chunk record count exceeds %d", metadataLimits.ChunkRecords)
		}
		var embedded bytes.Buffer
		var entryWriter io.Writer = io.Discard
		sum := sha256.New()
		entryWriter = sum
		if name == ManifestName {
			if hdr.Size > manifestMaxBytes {
				return VerifyReport{}, fmt.Errorf("embedded home manifest exceeds %d bytes", manifestMaxBytes)
			}
			entryWriter = io.MultiWriter(sum, &embedded)
		}
		var n int64
		if restore != nil {
			out, err := restore.createFile(name, fs.FileMode(hdr.Mode))
			if err != nil {
				return VerifyReport{}, err
			}
			n, err = io.CopyN(out.file, io.TeeReader(tr, entryWriter), hdr.Size)
			if err != nil {
				return VerifyReport{}, out.abort(streamArchiveReadError(archivePath, source, decoded, err))
			}
			if err := out.commit(); err != nil {
				return VerifyReport{}, err
			}
		} else {
			var err error
			n, err = io.CopyN(entryWriter, tr, hdr.Size)
			if err != nil {
				return VerifyReport{}, streamArchiveReadError(archivePath, source, decoded, err)
			}
		}
		if name == ManifestName {
			homeManifestBytes = append([]byte(nil), embedded.Bytes()...)
			homeManifest, err := decodeHomeManifest(homeManifestBytes)
			if err != nil {
				return VerifyReport{}, err
			}
			homeSchema = homeManifest.SchemaVersion
		}
		entry := ArchiveEntry{Path: name, Size: n, SHA256: hex.EncodeToString(sum.Sum(nil))}
		pending = append(pending, entry)
		files++
		if bytesTotal > limits.MaxExpandedBytes-n {
			return VerifyReport{}, fmt.Errorf("archive verified bytes exceed %d", limits.MaxExpandedBytes)
		}
		bytesTotal += n
	}
	if err := rejectTrailingDecodedArchiveData(decoded); err != nil {
		return VerifyReport{}, streamArchiveReadError(archivePath, source, decoded, err)
	}
	if _, err := buffered.Peek(1); err == nil {
		return VerifyReport{}, fmt.Errorf("archive contains trailing compressed data")
	} else if !errors.Is(err, io.EOF) {
		return VerifyReport{}, streamArchiveReadError(archivePath, source, decoded, err)
	}
	if err := source.limitError(archivePath); err != nil {
		return VerifyReport{}, err
	}
	if err := decoded.limitErrorFor("decoded archive", "tar stream"); err != nil {
		return VerifyReport{}, err
	}
	if !indexSeen || len(pending) != 0 || len(homeManifestBytes) == 0 {
		return VerifyReport{}, fmt.Errorf("archive stream final index or home manifest is missing")
	}
	if restore != nil {
		if err := createArchiveLinks(restore, index.Links); err != nil {
			return VerifyReport{}, err
		}
	}
	if staged != nil {
		linkPaths := make(map[string]struct{}, len(index.Links))
		for _, link := range index.Links {
			if err := verifyArchiveLink(staged, link); err != nil {
				return VerifyReport{}, err
			}
			linkPaths[link.Path] = struct{}{}
		}
		stagedCount := 0
		stagedArchiveDirs := 0
		generatedDirs := make(map[[sha256.Size]byte]struct{})
		for _, rel := range HabitatDirs() {
			generatedDirs[sha256.Sum256([]byte(filepath.ToSlash(rel)))] = struct{}{}
		}
		for _, rel := range privateCustodyKnownDirs {
			generatedDirs[sha256.Sum256([]byte(filepath.ToSlash(rel)))] = struct{}{}
		}
		err := walkArchiveRootBatched(staged.root, ".", func(rel string, info fs.FileInfo) error {
			if err := requireStreamedRestoreDevice(staged.stagingInfo, info); err != nil {
				return fmt.Errorf("staged archive device changed at %s: %w", rel, err)
			}
			if info.IsDir() {
				key := sha256.Sum256([]byte(rel))
				if seen[key] {
					stagedArchiveDirs++
					if stagedArchiveDirs > archivedDirs {
						return fmt.Errorf("restored archive contains extra directory %s", rel)
					}
				} else if _, allowed := generatedDirs[key]; !allowed {
					return fmt.Errorf("restored archive contains extra directory %s", rel)
				}
				return nil
			}
			_, isLink := linkPaths[rel]
			if (isLink && info.Mode()&os.ModeSymlink == 0) || (!isLink && !info.Mode().IsRegular()) {
				return fmt.Errorf("restored archive contains non-regular entry %s", rel)
			}
			stagedCount++
			if stagedCount > files {
				return fmt.Errorf("restored archive contains extra file %s", rel)
			}
			return nil
		})
		if err != nil {
			return VerifyReport{}, err
		}
		if stagedCount != files {
			return VerifyReport{}, fmt.Errorf("restored archive file count %d differs from manifest %d", stagedCount, files)
		}
		if stagedArchiveDirs != archivedDirs {
			return VerifyReport{}, fmt.Errorf("restored archive directory count %d differs from archive %d", stagedArchiveDirs, archivedDirs)
		}
	}
	return VerifyReport{Kind: ArchiveKindBackup, ArchivePath: archivePath, Files: files, Bytes: bytesTotal, OK: true}, nil
}

func streamArchiveReadError(path string, source, decoded *boundedArchiveSource, err error) error {
	if limitErr := decoded.limitErrorFor("decoded archive", "tar stream"); limitErr != nil {
		return limitErr
	}
	if limitErr := source.limitError(path); limitErr != nil {
		return limitErr
	}
	return err
}

func checkStreamChunk(data []byte, pending []ArchiveEntry, staged *archiveRestoreTarget, maxBytes int64) error {
	var expected bytes.Buffer
	expected.WriteByte('[')
	for i, entry := range pending {
		if i > 0 {
			expected.WriteByte(',')
		}
		record, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if int64(expected.Len()+len(record)+1) > maxBytes {
			return fmt.Errorf("archive manifest chunk exceeds %d bytes", maxBytes)
		}
		expected.Write(record)
	}
	expected.WriteByte(']')
	if !bytes.Equal(data, expected.Bytes()) {
		return fmt.Errorf("archive manifest chunk differs from archived files")
	}
	if staged != nil {
		for _, entry := range pending {
			if err := verifyStreamedStagedFile(staged, entry); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateStreamIndex(index archiveStreamIndex, chunks []archiveStreamChunk, files int, bytesTotal int64, members int, homeSchema, streamSchema string) error {
	if index.SchemaVersion != streamSchema || index.Kind != ArchiveKindBackup {
		return asArchiveCompatibilityError(fmt.Errorf("archive stream index version or kind mismatch"))
	}
	if index.HomeSchemaVersion != homeSchema || homeSchema != SchemaVersion {
		return asArchiveCompatibilityError(fmt.Errorf("archive stream home schema mismatch"))
	}
	if _, err := time.Parse(time.RFC3339, index.CreatedAt); err != nil {
		return fmt.Errorf("archive stream creation time invalid: %w", err)
	}
	if index.Files != files || index.Bytes != bytesTotal || index.Members != members || !reflect.DeepEqual(index.Chunks, chunks) {
		return fmt.Errorf("archive stream final index does not match archived entries")
	}
	if !reflect.DeepEqual(index.ExcludedRoots, backupExcludedRoots) {
		return fmt.Errorf("archive stream excluded roots differ from backup profile")
	}
	return nil
}

func verifyStreamedStagedFile(target *archiveRestoreTarget, entry ArchiveEntry) error {
	rel := filepath.FromSlash(entry.Path)
	before, err := target.root.Lstat(rel)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return fmt.Errorf("restored archive entry must remain regular: %s", entry.Path)
	}
	if err := requireStreamedRestoreDevice(target.stagingInfo, before); err != nil {
		return err
	}
	f, err := target.root.Open(rel)
	if err != nil {
		return err
	}
	opened, statErr := f.Stat()
	sum := sha256.New()
	n, copyErr := io.Copy(sum, f)
	closeErr := f.Close()
	after, afterErr := target.root.Lstat(rel)
	if err := errors.Join(statErr, copyErr, closeErr, afterErr); err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		return fmt.Errorf("restored archive entry identity changed while verifying: %s", entry.Path)
	}
	if n != entry.Size || hex.EncodeToString(sum.Sum(nil)) != entry.SHA256 {
		return fmt.Errorf("restored archive checksum mismatch for %s", entry.Path)
	}
	return nil
}

func (a *verifiedBackupArchive) verifyStreamedStaging(target *archiveRestoreTarget) error {
	if !a.streamed || a.file == nil {
		return fmt.Errorf("streamed archive source is not open")
	}
	if _, err := a.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := sha256.New()
	report, err := readStreamedArchiveWithLimits(a.path, a.file, nil, target, digest, a.limits)
	if err != nil {
		return err
	}
	if !bytes.Equal(a.preflightDigest, digest.Sum(nil)) || report != a.streamedReport {
		return fmt.Errorf("archive source changed between preflight and staged verification: %s", a.path)
	}
	return nil
}
