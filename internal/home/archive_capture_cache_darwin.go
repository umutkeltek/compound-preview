//go:build darwin

package home

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// This temporary index is an accelerator, never archive membership or authority.
// Two million fixed slots bound disk use at 128 MiB and heap use at one record.
// The existing 1.5-million-entry limit keeps the table below 72% occupancy.
const captureCacheSlots = 1 << 21
const captureCacheSlotBytes = 64

type archiveCaptureCache struct {
	slotBuffer             [captureCacheSlotBytes]byte
	readBuffer             []byte
	readOffset             int64
	index, journal         *os.File
	indexInfo, journalInfo fs.FileInfo
	indexHash, journalHash string
}

func newArchiveCaptureCache(stage *os.Root) (*archiveCaptureCache, error) {
	f, err := stage.OpenFile("cache-index", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Truncate(captureCacheSlots * captureCacheSlotBytes); err != nil {
		f.Close()
		return nil, err
	}
	return &archiveCaptureCache{index: f}, nil
}

func captureCacheKey(rel string, info fs.FileInfo) ([32]byte, error) {
	stamp := captureStamp(info)
	var encoded [64]byte
	values := [8]uint64{stamp.Device, stamp.Inode, uint64(stamp.Size), uint64(stamp.Mode), uint64(stamp.Modified.Unix()), uint64(stamp.Modified.Nanosecond()), uint64(stamp.Changed.Unix()), uint64(stamp.Changed.Nanosecond())}
	for i, v := range values {
		binary.LittleEndian.PutUint64(encoded[i*8:], v)
	}
	hash := sha256.New()
	hash.Write([]byte(rel))
	hash.Write([]byte{0})
	hash.Write(encoded[:])
	var key [32]byte
	copy(key[:], hash.Sum(nil))
	return key, nil
}

func (c *archiveCaptureCache) slot(key [32]byte, insert bool) ([]byte, int64, error) {
	start := binary.LittleEndian.Uint64(key[:8]) % captureCacheSlots
	buf := c.slotBuffer[:]
	// Refuse pathological/corrupt probe chains rather than spending unbounded
	// time in the admission fence. Normal load is bounded independently above.
	for probe := uint64(0); probe < 4096; probe++ {
		offset := int64((start+probe)%captureCacheSlots) * captureCacheSlotBytes
		if _, err := c.index.ReadAt(buf[:], offset); err != nil {
			return nil, 0, err
		}
		occupied := binary.LittleEndian.Uint32(buf[40:44]) != 0
		if !occupied {
			if insert {
				return buf[:], offset, nil
			}
			return nil, 0, nil
		}
		if bytes.Equal(buf[:32], key[:]) {
			return buf[:], offset, nil
		}
	}
	return nil, 0, fmt.Errorf("capture cache probe budget exceeded")
}

func (c *archiveCaptureCache) add(rel string, info fs.FileInfo, offset int64, length, body int, size int64) error {
	key, err := captureCacheKey(rel, info)
	if err != nil {
		return err
	}
	slot, pos, err := c.slot(key, true)
	if err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(slot[40:44]) != 0 {
		return fmt.Errorf("duplicate capture cache key: %s", rel)
	}
	copy(slot[:32], key[:])
	binary.LittleEndian.PutUint64(slot[32:40], uint64(offset))
	if offset < 0 || length < 1 || length > archiveCaptureRecordBytes || body < 1 || size < 0 {
		return fmt.Errorf("invalid capture cache entry")
	}
	binary.LittleEndian.PutUint32(slot[40:44], uint32(length))
	binary.LittleEndian.PutUint32(slot[44:48], uint32(body))
	binary.LittleEndian.PutUint64(slot[48:56], uint64(size))
	_, err = c.index.WriteAt(slot, pos)
	return err
}

func captureFileDigest(f *os.File, size int64) (string, error) {
	hash := sha256.New()
	n, err := io.Copy(hash, io.NewSectionReader(f, 0, size))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", io.ErrUnexpectedEOF
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateCaptureCacheFile(name string, info fs.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return fmt.Errorf("capture cache is not a regular bounded file: %s", name)
	}
	if name == "cache-index" && info.Size() != captureCacheSlots*captureCacheSlotBytes {
		return fmt.Errorf("capture cache index size changed")
	}
	if name == "warm-journal" && info.Size() > archiveStreamTotalMeta {
		return fmt.Errorf("capture warm journal exceeds metadata limit")
	}
	return nil
}

func (c *archiveCaptureCache) seal(stage *os.Root, journalHash string) error {
	var err error
	c.indexInfo, err = c.index.Stat()
	if err != nil {
		return err
	}
	if err := validateCaptureCacheFile("cache-index", c.indexInfo); err != nil {
		return err
	}
	// Check both sizes before hashing either file.
	journalInfo, err := stage.Lstat("journal")
	if err != nil {
		return err
	}
	if err := validateCaptureCacheFile("warm-journal", journalInfo); err != nil {
		return err
	}
	c.indexHash, err = captureFileDigest(c.index, c.indexInfo.Size())
	if err != nil {
		return err
	}
	if err = stage.Rename("journal", "warm-journal"); err != nil {
		return err
	}
	c.journal, err = stage.Open("warm-journal")
	if err != nil {
		return err
	}
	c.journalInfo, err = c.journal.Stat()
	if err != nil {
		return err
	}
	if err := validateCaptureCacheFile("warm-journal", c.journalInfo); err != nil {
		return err
	}
	if !os.SameFile(journalInfo, c.journalInfo) || journalInfo.Size() != c.journalInfo.Size() || journalInfo.Mode() != c.journalInfo.Mode() || !journalInfo.ModTime().Equal(c.journalInfo.ModTime()) {
		return fmt.Errorf("capture warm journal changed during seal")
	}
	c.journalHash = journalHash
	return c.verify(stage, true)
}

func (c *archiveCaptureCache) verify(stage *os.Root, full bool) error {
	for _, item := range []struct {
		name string
		file *os.File
		info fs.FileInfo
		hash string
	}{
		{"cache-index", c.index, c.indexInfo, c.indexHash},
		{"warm-journal", c.journal, c.journalInfo, c.journalHash},
	} {
		if item.file == nil || item.info == nil {
			return fmt.Errorf("capture cache is not sealed")
		}
		live, err := stage.Lstat(item.name)
		if err != nil {
			return err
		}
		opened, err := item.file.Stat()
		if err != nil {
			return err
		}
		for _, info := range []fs.FileInfo{item.info, live, opened} {
			if err := validateCaptureCacheFile(item.name, info); err != nil {
				return err
			}
		}
		stamp := captureStamp(item.info)
		if !stamp.matches(live) || !stamp.matches(opened) {
			return fmt.Errorf("capture cache changed: %s", item.name)
		}
		if full {
			digest, err := captureFileDigest(item.file, item.info.Size())
			if err != nil {
				return err
			}
			if digest != item.hash {
				return fmt.Errorf("capture cache digest changed: %s", item.name)
			}
		}
	}
	return nil
}

// archiveCachedBody exposes only what reconciliation consumes. The complete
// record stays sealed in the warm journal and is replayed without reconstruction.
type archiveCachedBody struct {
	Body      int
	Size      int64
	Reference archiveCaptureReference
}

func (c *archiveCaptureCache) lookup(rel string, info fs.FileInfo) (archiveCachedBody, bool, error) {
	key, err := captureCacheKey(rel, info)
	if err != nil {
		return archiveCachedBody{}, false, err
	}
	slot, _, err := c.slot(key, false)
	if err != nil || slot == nil {
		return archiveCachedBody{}, false, err
	}
	offset := binary.LittleEndian.Uint64(slot[32:40])
	length := binary.LittleEndian.Uint32(slot[40:44])
	body := binary.LittleEndian.Uint32(slot[44:48])
	size := binary.LittleEndian.Uint64(slot[48:56])
	if c.journalInfo == nil || offset > uint64(c.journalInfo.Size()) || size > uint64(archiveStreamTarLimits().MaxEntryBytes) {
		return archiveCachedBody{}, false, fmt.Errorf("invalid capture cache record range")
	}
	ref := archiveCaptureReference{Path: rel, SourceKey: hex.EncodeToString(key[:]), Offset: int64(offset), Length: int(length), Body: int(body), Size: int64(size)}
	if err := c.validateReference(ref); err != nil {
		return archiveCachedBody{}, false, err
	}
	return archiveCachedBody{Body: ref.Body, Size: ref.Size, Reference: ref}, true, nil
}

func (c *archiveCaptureCache) validateReference(ref archiveCaptureReference) error {
	key, err := hex.DecodeString(ref.SourceKey)
	if err != nil || len(key) != sha256.Size || ref.Path == "" || ref.Body < 1 || ref.Size < 0 || c.journalInfo == nil || ref.Offset < 0 || ref.Length < 1 || ref.Length > archiveCaptureRecordBytes || ref.Offset > c.journalInfo.Size() || int64(ref.Length) > c.journalInfo.Size()-ref.Offset {
		return fmt.Errorf("invalid capture warm reference")
	}
	return nil
}

func (c *archiveCaptureCache) readReference(ref archiveCaptureReference) ([]byte, error) {
	if err := c.validateReference(ref); err != nil {
		return nil, err
	}
	return c.readRecord(ref.Offset, ref.Length)
}

func (c *archiveCaptureCache) close() error {
	var first error
	for _, f := range []*os.File{c.journal, c.index} {
		if f != nil {
			if err := f.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (c *archiveCaptureCache) readRecord(offset int64, length int) ([]byte, error) {
	if length > 64<<10 {
		data := make([]byte, length)
		_, err := c.journal.ReadAt(data, offset)
		return data, err
	}
	if offset < c.readOffset || offset+int64(length) > c.readOffset+int64(len(c.readBuffer)) {
		if cap(c.readBuffer) < 64<<10 {
			c.readBuffer = make([]byte, 64<<10)
		} else {
			c.readBuffer = c.readBuffer[:64<<10]
		}
		n, err := c.journal.ReadAt(c.readBuffer, offset)
		if err != nil && err != io.EOF {
			return nil, err
		}
		c.readBuffer = c.readBuffer[:n]
		c.readOffset = offset
	}
	start := offset - c.readOffset
	if start < 0 || start+int64(length) > int64(len(c.readBuffer)) {
		return nil, io.ErrUnexpectedEOF
	}
	return c.readBuffer[start : start+int64(length)], nil
}
