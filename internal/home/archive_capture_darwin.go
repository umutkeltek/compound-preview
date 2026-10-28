//go:build darwin

package home

import (
	"archive/tar"
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Reconciliation under admission is the source-consistency boundary. Warm
// clones are only a cache. Independently locked writers remain per-file
// observations. No live source is consulted after final capture succeeds.
var archiveCaptureBeforeClone = func(string) error { return nil }
var archiveCaptureClone = unix.Fclonefileat
var archiveStreamCaptureReady = func(*capturedArchive) error { return nil }
var archiveWarmCaptureReady = func(*capturedArchive) error { return nil }
var archiveCaptureReconcileStarted = func(*capturedArchive) error { return nil }
var archiveCaptureNow = time.Now
var archiveCaptureReconciled = func(*capturedArchive, error) {}

// Cooperative deadline: checked between entries and after each clone. A single
// blocked filesystem syscall cannot be preempted; no unlimited fallback exists.
const archiveCaptureReconcileBudget = 30 * time.Second

const archiveCaptureRecordBytes = 128 << 10

type archiveCaptureStamp struct {
	Device   uint64
	Inode    uint64
	Size     int64
	Mode     fs.FileMode
	Modified time.Time
	Changed  time.Time
}

func captureStamp(info fs.FileInfo) archiveCaptureStamp {
	stat := info.Sys().(*syscall.Stat_t)
	return archiveCaptureStamp{Device: uint64(stat.Dev), Inode: stat.Ino, Size: info.Size(), Mode: info.Mode(),
		Modified: info.ModTime(), Changed: time.Unix(stat.Ctimespec.Sec, stat.Ctimespec.Nsec)}
}
func (s archiveCaptureStamp) matches(info fs.FileInfo) bool {
	other := captureStamp(info)
	return s.Device == other.Device && s.Inode == other.Inode && s.Size == other.Size &&
		s.Mode == other.Mode && s.Modified.Equal(other.Modified) && s.Changed.Equal(other.Changed)
}

type archiveCaptureReference struct {
	Path      string `json:"path"`
	SourceKey string `json:"source_key"`
	Offset    int64  `json:"offset"`
	Length    int    `json:"length"`
	Body      int    `json:"body"`
	Size      int64  `json:"size"`
}

type archiveCaptureRecord struct {
	Cached    *archiveCaptureReference `json:"cached,omitempty"`
	SourceKey string                   `json:"source_key,omitempty"`
	encoded   []byte
	Header    tar.Header
	Link      *archiveLink
	Body      int
	Stamp     archiveCaptureStamp
}

func marshalArchiveCaptureRecord(record archiveCaptureRecord) ([]byte, error) {
	// JSON replaces invalid UTF-8; refuse rather than silently rename a path.
	if !utf8.ValidString(record.Header.Name) {
		return nil, fmt.Errorf("capture path must be valid UTF-8")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(data)+1 > archiveCaptureRecordBytes {
		return nil, fmt.Errorf("capture record exceeds limit")
	}
	return append(data, '\n'), nil
}

type capturedArchive struct {
	shards             *archiveCaptureShards
	admissionDuration  time.Duration
	cache              *archiveCaptureCache
	references         int
	root, tmp, stage   *os.Root
	stageName          string
	stageInfo, tmpInfo fs.FileInfo
	journalInfo        fs.FileInfo
	manifest           []byte
	homeSchema         string
	records, bodies    int
	journalHash        string
	closed             bool
}

func backupCapturedStreamedArchive(homePath, targetPath string, now time.Time, schema string) (report ArchiveReport, resultErr error) {
	if err := WithAutonomyAdmissionLock(homePath, func() error {
		return prepareArchiveTarget(homePath, targetPath, includeBackupPath)
	}); err != nil {
		return ArchiveReport{}, err
	}
	captured, err := captureStreamedArchive(homePath)
	if err != nil {
		return ArchiveReport{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, captured.close()) }()
	if err := captured.cache.seal(captured.stage, captured.journalHash); err != nil {
		return ArchiveReport{}, err
	}
	if err := archiveWarmCaptureReady(captured); err != nil {
		return ArchiveReport{}, err
	}
	if err := captured.cache.verify(captured.stage, true); err != nil {
		return ArchiveReport{}, err
	}
	reconcileErr := WithAutonomyAdmissionLock(homePath, func() error {
		if err := prepareArchiveTarget(homePath, targetPath, includeBackupPath); err != nil {
			return err
		}
		return captured.reconcile(homePath)
	})
	archiveCaptureReconciled(captured, reconcileErr)
	if reconcileErr != nil {
		return ArchiveReport{}, reconcileErr
	}
	// Hashing and cleanup stay outside admission, including on failed attempts.
	if err := captured.cache.verify(captured.stage, true); err != nil {
		return ArchiveReport{}, err
	}
	if err := archiveStreamCaptureReady(captured); err != nil {
		return ArchiveReport{}, err
	}
	source := &archiveStreamSource{manifest: captured.manifest, homeSchema: captured.homeSchema, walk: captured.walk, close: captured.close}
	return writeStreamedArchiveSource(homePath, targetPath, now, schema, source)
}

func captureStreamedArchive(homePath string) (_ *capturedArchive, resultErr error) {
	if err := validateArchiveLinkSourceHome(homePath); err != nil {
		return nil, err
	}
	root, err := openStableArchiveRoot(homePath)
	if err != nil {
		return nil, err
	}
	c := &capturedArchive{root: root}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, c.close())
		}
	}()
	if err := archiveStreamRootOpened(homePath); err != nil {
		return nil, err
	}
	if err := validateStreamedSourceHealth(root); err != nil {
		return nil, err
	}
	c.manifest, err = readRootedHomeManifestBounded(root)
	if err != nil {
		return nil, err
	}
	manifest, err := decodeHomeManifest(c.manifest)
	if err != nil {
		return nil, err
	}
	c.homeSchema = manifest.SchemaVersion
	if err := archiveAfterHomeManifestPinned(filepath.Join(homePath, ManifestName)); err != nil {
		return nil, err
	}
	if err := c.createStage(); err != nil {
		return nil, err
	}
	c.shards, err = newArchiveCaptureShards(c.stage)
	if err != nil {
		return nil, err
	}
	c.cache, err = newArchiveCaptureCache(c.stage)
	if err != nil {
		return nil, err
	}
	if err := c.writeCaptureJournal(homePath, true, time.Time{}); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *capturedArchive) writeCaptureJournal(homePath string, warm bool, deadline time.Time) error {
	journal, err := c.stage.OpenFile("journal", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer journal.Close()
	digest := sha256.New()
	buffered := bufio.NewWriterSize(journal, 1<<20)
	bounded := &boundedArchiveOutput{writer: io.MultiWriter(buffered, digest), remaining: archiveStreamTotalMeta, limit: archiveStreamTotalMeta, kind: "capture journal"}
	budget := archiveEntryBudget{limits: archiveStreamTarLimits()}
	c.records, c.references = 0, 0
	links := 0
	var offset int64
	checkDeadline := func() error {
		if !deadline.IsZero() && !archiveCaptureNow().Before(deadline) {
			return fmt.Errorf("snapshot_busy: backup reconciliation exceeded admission budget")
		}
		return nil
	}
	walk := walkArchiveRootBatched
	if !warm {
		walk = walkCaptureRootConcurrent
	}
	err = walk(c.root, ".", func(rel string, info fs.FileInfo) error {
		if err := checkDeadline(); err != nil {
			return err
		}
		if rel == ManifestName {
			return nil
		}
		if !includeBackupPath(rel, info) {
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("archive included source entry must be regular file or directory: %s", rel)
		}
		header, err := archiveFileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		if err := budget.observe(header); err != nil {
			return err
		}
		record := archiveCaptureRecord{Header: *header}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > archiveStreamMaxLinks {
				return fmt.Errorf("archive link count exceeds %d", archiveStreamMaxLinks)
			}
			link, err := captureArchiveLink(c.root, homePath, rel, info)
			if err != nil {
				return err
			}
			record.Link = &link
		} else if info.Mode().IsRegular() {
			key, err := captureCacheKey(rel, info)
			if err != nil {
				return err
			}
			record.SourceKey = hex.EncodeToString(key[:])
			reused := false
			if !warm {
				cached, found, err := c.cache.lookup(rel, info)
				if err != nil {
					return err
				}
				if found {
					if cached.Body > c.bodies {
						return fmt.Errorf("capture cache body out of range")
					}
					// The unchanged warm journal binds the expected clone stamp.
					// Mandatory replay checks it before and during serialization, before
					// publication. Repeating that private-body stat under admission adds
					// no source consistency and makes the fence grow with cold metadata.
					record.Body = cached.Body
					record.Header.Size = cached.Size
					record.encoded, err = json.Marshal(struct {
						Cached *archiveCaptureReference `json:"cached"`
					}{&cached.Reference})
					if err != nil {
						return err
					}
					record.encoded = append(record.encoded, '\n')
					if len(record.encoded) > archiveCaptureRecordBytes {
						return fmt.Errorf("capture reference exceeds limit")
					}
					reused = true
				}
			}
			if !reused {
				if _, bulk := info.(*captureBulkFileInfo); bulk {
					current, err := c.root.Lstat(filepath.FromSlash(rel))
					if err != nil {
						return err
					}
					if !captureStamp(info).matches(current) {
						return fmt.Errorf("capture source changed after bulk observation: %s", rel)
					}
					info = current
				}
				c.bodies++
				record.Body = c.bodies
				metadata, stamp, err := c.clone(rel, info, record.Body)
				if err != nil {
					return err
				}
				record.Header, record.Stamp = *metadata, stamp
			}
			c.references++
			delta := record.Header.Size - header.Size
			if delta > 0 {
				if record.Header.Size > budget.limits.MaxEntryBytes || budget.expandedBytes > budget.limits.MaxExpandedBytes-delta {
					return fmt.Errorf("captured event exceeds archive byte budget")
				}
				budget.expandedBytes += delta
			}
		}
		if err := checkDeadline(); err != nil {
			return err
		}
		data := record.encoded
		if data == nil {
			data, err = marshalArchiveCaptureRecord(record)
			if err != nil {
				return err
			}
		}
		if _, err := bounded.Write(data); err != nil {
			return err
		}
		if warm && info.Mode().IsRegular() {
			if err := c.cache.add(rel, info, offset, len(data), record.Body, record.Header.Size); err != nil {
				return err
			}
		}
		offset += int64(len(data))
		c.records++
		return nil
	})
	if err != nil {
		return err
	}
	if err := checkDeadline(); err != nil {
		return err
	}
	if err := buffered.Flush(); err != nil {
		return err
	}
	if err := journal.Close(); err != nil {
		return err
	}
	c.journalInfo, err = c.stage.Lstat("journal")
	if err != nil {
		return err
	}
	c.journalHash = hex.EncodeToString(digest.Sum(nil))
	if err := c.validateStage(); err != nil {
		return err
	}
	return checkDeadline()
}

func (c *capturedArchive) reconcile(homePath string) error {
	started := time.Now()
	defer func() { c.admissionDuration = time.Since(started) }()
	deadline := archiveCaptureNow().Add(archiveCaptureReconcileBudget)
	current, err := os.Lstat(homePath)
	if err != nil {
		return err
	}
	opened, err := c.root.Stat(".")
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, opened) {
		return fmt.Errorf("capture source home identity changed")
	}
	if err := c.validateStage(); err != nil {
		return err
	}
	if err := c.cache.verify(c.stage, false); err != nil {
		return err
	}
	if err := validateStreamedSourceHealth(c.root); err != nil {
		return err
	}
	c.manifest, err = readRootedHomeManifestBounded(c.root)
	if err != nil {
		return err
	}
	manifest, err := decodeHomeManifest(c.manifest)
	if err != nil {
		return err
	}
	c.homeSchema = manifest.SchemaVersion
	if err := archiveCaptureReconcileStarted(c); err != nil {
		return err
	}
	if err := c.writeCaptureJournal(homePath, false, deadline); err != nil {
		return err
	}
	if err := c.cache.verify(c.stage, false); err != nil {
		return err
	}
	if !archiveCaptureNow().Before(deadline) {
		return fmt.Errorf("snapshot_busy: backup reconciliation exceeded admission budget")
	}
	return nil
}

func (c *capturedArchive) createStage() error {
	var err error
	c.tmpInfo, err = c.root.Lstat("tmp")
	if err != nil {
		return err
	}
	if !c.tmpInfo.IsDir() || c.tmpInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("capture temporary root must be a regular directory")
	}
	c.tmp, err = c.root.OpenRoot("tmp")
	if err != nil {
		return err
	}
	opened, err := c.tmp.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(c.tmpInfo, opened) {
		return fmt.Errorf("capture temporary root changed")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	c.stageName = "home-backup-snapshot-" + hex.EncodeToString(nonce[:])
	if err := c.tmp.Mkdir(c.stageName, 0o700); err != nil {
		return err
	}
	c.stageInfo, err = c.tmp.Lstat(c.stageName)
	if err != nil {
		return err
	}
	c.stage, err = c.tmp.OpenRoot(c.stageName)
	if err != nil {
		return err
	}
	return c.validateStage()
}

func (c *capturedArchive) validateStage() error {
	tmp, err := c.root.Lstat("tmp")
	if err != nil {
		return err
	}
	stage, err := c.tmp.Lstat(c.stageName)
	if err != nil {
		return err
	}
	opened, err := c.stage.Stat(".")
	if err != nil {
		return err
	}
	if !tmp.IsDir() || tmp.Mode()&os.ModeSymlink != 0 || !os.SameFile(c.tmpInfo, tmp) ||
		!stage.IsDir() || stage.Mode()&os.ModeSymlink != 0 || stage.Mode().Perm() != 0o700 ||
		!os.SameFile(c.stageInfo, stage) || !os.SameFile(c.stageInfo, opened) {
		return fmt.Errorf("capture stage identity or permissions changed")
	}
	if c.shards != nil {
		return c.shards.validate()
	}
	return nil
}

func sameCaptureSource(before, after fs.FileInfo, event bool) bool {
	return after.Mode().IsRegular() && os.SameFile(before, after) &&
		(event || captureStamp(before).matches(after))
}
func (c *capturedArchive) clone(rel string, before fs.FileInfo, body int) (_ *tar.Header, _ archiveCaptureStamp, resultErr error) {
	source, err := c.root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, archiveCaptureStamp{}, err
	}
	defer source.Close()
	event := strings.HasPrefix(rel, "events/")
	opened, err := source.Stat()
	if err != nil {
		return nil, archiveCaptureStamp{}, err
	}
	current, err := c.root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return nil, archiveCaptureStamp{}, err
	}
	if !sameCaptureSource(before, opened, event) || !sameCaptureSource(before, current, event) {
		return nil, archiveCaptureStamp{}, fmt.Errorf("capture source identity or metadata changed: %s", rel)
	}
	if err := archiveCaptureBeforeClone(rel); err != nil {
		return nil, archiveCaptureStamp{}, err
	}
	if err := c.shards.ensure(body); err != nil {
		return nil, archiveCaptureStamp{}, err
	}
	bodyRoot, dir, name := c.shards.body(body)
	if err := archiveCaptureClone(int(source.Fd()), int(dir.Fd()), name, 0); err != nil {
		return nil, archiveCaptureStamp{}, fmt.Errorf("immutable capture clone unavailable for %s: %w", rel, err)
	}
	after, err := c.root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return nil, archiveCaptureStamp{}, err
	}
	end, err := source.Stat()
	if err != nil {
		return nil, archiveCaptureStamp{}, err
	}
	if !sameCaptureSource(before, after, event) || !sameCaptureSource(before, end, event) {
		return nil, archiveCaptureStamp{}, fmt.Errorf("capture source changed during clone: %s", rel)
	}
	metadata, err := bodyRoot.Lstat(name)
	if err != nil {
		return nil, archiveCaptureStamp{}, err
	}
	if !metadata.Mode().IsRegular() || (!event && (metadata.Size() != before.Size() || !metadata.ModTime().Equal(before.ModTime()))) {
		return nil, archiveCaptureStamp{}, fmt.Errorf("capture clone metadata changed: %s", rel)
	}
	header, err := archiveFileInfoHeader(metadata, "")
	if err != nil {
		return nil, archiveCaptureStamp{}, err
	}
	header.Name = rel
	if err := bodyRoot.Chmod(name, 0o600); err != nil {
		return nil, archiveCaptureStamp{}, err
	}
	sealed, err := bodyRoot.Lstat(name)
	if err != nil {
		return nil, archiveCaptureStamp{}, err
	}
	if !os.SameFile(metadata, sealed) || sealed.Size() != metadata.Size() || !sealed.ModTime().Equal(metadata.ModTime()) {
		return nil, archiveCaptureStamp{}, fmt.Errorf("capture clone changed while sealing: %s", rel)
	}
	return header, captureStamp(sealed), nil
}

func (c *capturedArchive) walk(visit func(archiveStreamItem) error) error {
	if err := c.validateStage(); err != nil {
		return err
	}
	journal, err := c.stage.Open("journal")
	if err != nil {
		return err
	}
	defer journal.Close()
	opened, err := journal.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(c.journalInfo, opened) || !captureStamp(c.journalInfo).matches(opened) {
		return fmt.Errorf("capture journal changed before replay")
	}
	digest := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(io.LimitReader(journal, c.journalInfo.Size()+1), digest))
	scanner.Buffer(make([]byte, 4096), archiveCaptureRecordBytes)
	count, bodies := 0, 0
	for scanner.Scan() {
		count++
		if count > c.records {
			return fmt.Errorf("capture journal record count changed")
		}
		var record archiveCaptureRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return err
		}
		if record.Cached != nil {
			if record.Header.Name != "" || record.Body != 0 || record.Link != nil || record.SourceKey != "" {
				return fmt.Errorf("mixed capture reference and full record")
			}
			ref := *record.Cached
			data, err := c.cache.readReference(ref)
			if err != nil {
				return err
			}
			record = archiveCaptureRecord{}
			if err := json.Unmarshal(data, &record); err != nil {
				return err
			}
			if record.Cached != nil || record.Link != nil || record.Header.Typeflag != tar.TypeReg || record.Header.Name != ref.Path || record.SourceKey != ref.SourceKey || record.Body != ref.Body || record.Header.Size != ref.Size {
				return fmt.Errorf("capture warm reference identity mismatch")
			}
		}
		item := archiveStreamItem{rel: record.Header.Name, info: record.Header.FileInfo()}
		if record.Header.Typeflag == tar.TypeSymlink {
			if record.Link == nil || record.Link.Path != item.rel || record.Body != 0 {
				return fmt.Errorf("invalid captured link")
			}
			item.link = func() (archiveLink, error) { return *record.Link, nil }
		} else if record.Header.Typeflag == tar.TypeReg {
			if record.Link != nil || record.Header.Size != record.Stamp.Size {
				return fmt.Errorf("invalid captured body")
			}
			bodies++
			if record.Body < 1 || record.Body > c.bodies {
				return fmt.Errorf("capture body identifier out of range")
			}
			bodyRoot, _, name := c.shards.body(record.Body)
			if bodyRoot == nil {
				return fmt.Errorf("capture body shard missing")
			}
			before, err := bodyRoot.Lstat(name)
			if err != nil {
				return err
			}
			if !record.Stamp.matches(before) {
				return fmt.Errorf("capture body changed before replay: %s", item.rel)
			}
			item.write = func(tw archiveEntryWriter) (ArchiveEntry, error) {
				return writeRootedArchiveFileAt(tw, bodyRoot, name, item.rel, before, record.Header.FileInfo())
			}
		} else if record.Header.Typeflag != tar.TypeDir || record.Link != nil || record.Body != 0 {
			return fmt.Errorf("invalid capture entry type")
		}
		if err := visit(item); err != nil {
			return err
		}
		if record.Body != 0 {
			bodyRoot, _, name := c.shards.body(record.Body)
			if bodyRoot == nil {
				return fmt.Errorf("capture body shard missing")
			}
			after, err := bodyRoot.Lstat(name)
			if err != nil {
				return err
			}
			if !record.Stamp.matches(after) {
				return fmt.Errorf("capture body changed during replay: %s", item.rel)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	after, err := c.stage.Lstat("journal")
	if err != nil {
		return err
	}
	end, err := journal.Stat()
	if err != nil {
		return err
	}
	if count != c.records || bodies != c.references || hex.EncodeToString(digest.Sum(nil)) != c.journalHash ||
		!os.SameFile(c.journalInfo, after) || !captureStamp(c.journalInfo).matches(after) || !captureStamp(c.journalInfo).matches(end) {
		return fmt.Errorf("capture journal changed during replay")
	}
	if err := c.validateStage(); err != nil {
		return err
	}
	if err := journal.Close(); err != nil {
		return err
	}
	if err := c.cache.verify(c.stage, true); err != nil {
		return err
	}
	// Reclaim our private inputs before publication, including on cleanup error.
	return c.close()
}

func (c *capturedArchive) close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	var result error
	if c.cache != nil {
		result = errors.Join(result, c.cache.close())
	}
	if c.stage != nil {
		for _, name := range []string{"cache-index", "warm-journal"} {
			if err := c.stage.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
	}
	if c.stage != nil {
		if err := c.stage.Remove("journal"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			result = errors.Join(result, err)
		}
		if c.shards != nil {
			for n := 1; n <= c.bodies; n++ {
				bodyRoot, _, name := c.shards.body(n)
				if bodyRoot == nil {
					continue
				}
				if err := bodyRoot.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
					result = errors.Join(result, err)
				}
			}
			result = errors.Join(result, c.shards.close())
		}
		result = errors.Join(result, c.stage.Close())
	}
	if c.tmp != nil {
		if c.stageInfo != nil {
			current, err := c.tmp.Lstat(c.stageName)
			if err == nil && os.SameFile(c.stageInfo, current) && current.IsDir() {
				result = errors.Join(result, c.tmp.Remove(c.stageName))
			} else {
				result = errors.Join(result, fmt.Errorf("capture stage identity changed during cleanup"), err)
			}
		}
		result = errors.Join(result, c.tmp.Close())
	}
	if c.root != nil {
		result = errors.Join(result, c.root.Close())
	}
	return result
}
