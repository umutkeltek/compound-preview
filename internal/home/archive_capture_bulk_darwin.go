//go:build darwin

package home

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/unix"
)

const captureBulkCommon = unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_ERROR | unix.ATTR_CMN_NAME | unix.ATTR_CMN_DEVID | unix.ATTR_CMN_OBJTYPE | unix.ATTR_CMN_MODTIME | unix.ATTR_CMN_CHGTIME | unix.ATTR_CMN_OWNERID | unix.ATTR_CMN_GRPID | unix.ATTR_CMN_ACCESSMASK | unix.ATTR_CMN_FILEID

type captureBulkEntry struct {
	name string
	info fs.FileInfo
}
type captureBulkFileInfo struct {
	name string
	stat syscall.Stat_t
}

func (f *captureBulkFileInfo) Name() string { return f.name }
func (f *captureBulkFileInfo) Size() int64  { return f.stat.Size }
func (f *captureBulkFileInfo) Mode() fs.FileMode {
	mode := fs.FileMode(f.stat.Mode & 0777)
	if f.stat.Mode&syscall.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if f.stat.Mode&syscall.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if f.stat.Mode&syscall.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}
func (f *captureBulkFileInfo) ModTime() time.Time {
	return time.Unix(f.stat.Mtimespec.Sec, f.stat.Mtimespec.Nsec)
}
func (f *captureBulkFileInfo) IsDir() bool { return false }
func (f *captureBulkFileInfo) Sys() any    { return &f.stat }

// Darwin packs attributes at four-byte boundaries, independently of Go struct
// alignment. PACK_INVAL_ATTRS reserves every requested field; returned masks
// still decide whether a value may be consumed.
func decodeCaptureBulk(buf []byte, count int) ([]captureBulkEntry, error) {
	if count < 0 || count > len(buf)/96 {
		return nil, fmt.Errorf("invalid bulk capture count")
	}
	entries := make([]captureBulkEntry, 0, count)
	for i := 0; i < count; i++ {
		if len(buf) < 96 {
			return nil, fmt.Errorf("short bulk capture record")
		}
		size := uint64(binary.LittleEndian.Uint32(buf[:4]))
		if size < 96 || size > uint64(len(buf)) || size%4 != 0 {
			return nil, fmt.Errorf("invalid bulk capture length")
		}
		rec := buf[:int(size)]
		common := binary.LittleEndian.Uint32(rec[4:8])
		file := binary.LittleEndian.Uint32(rec[16:20])
		if common & ^uint32(captureBulkCommon) != 0 || file & ^uint32(unix.ATTR_FILE_DATALENGTH) != 0 || binary.LittleEndian.Uint32(rec[8:12]) != 0 || binary.LittleEndian.Uint32(rec[12:16]) != 0 || binary.LittleEndian.Uint32(rec[20:24]) != 0 {
			return nil, fmt.Errorf("unexpected bulk capture attributes")
		}
		if code := binary.LittleEndian.Uint32(rec[24:28]); code != 0 {
			return nil, fmt.Errorf("bulk capture entry: %w", syscall.Errno(code))
		}
		if common&(unix.ATTR_CMN_RETURNED_ATTRS|unix.ATTR_CMN_NAME) != unix.ATTR_CMN_RETURNED_ATTRS|unix.ATTR_CMN_NAME {
			return nil, fmt.Errorf("bulk capture name unavailable")
		}
		// File-group fields are omitted for directories even with PACK_INVAL_ATTRS.
		// Common fields have fixed reserved slots; only returned file fields add
		// their eight-byte slot before the variable name.
		fixed := int64(96)
		if file&unix.ATTR_FILE_DATALENGTH != 0 {
			fixed = 104
		}
		if int64(len(rec)) < fixed {
			return nil, fmt.Errorf("short bulk capture file fields")
		}
		start := int64(28) + int64(int32(binary.LittleEndian.Uint32(rec[28:32])))
		length := int64(binary.LittleEndian.Uint32(rec[32:36]))
		if start < fixed || length < 2 || start > int64(len(rec)) || length > int64(len(rec))-start {
			return nil, fmt.Errorf("invalid bulk capture name bounds")
		}
		raw := rec[start : start+length]
		name := string(raw[:len(raw)-1])
		if raw[len(raw)-1] != 0 || strings.ContainsAny(name, "/\x00") || name == "." || name == ".." || !utf8.ValidString(name) {
			return nil, fmt.Errorf("invalid bulk capture name")
		}
		entry := captureBulkEntry{name: name}
		required := uint32(captureBulkCommon & ^unix.ATTR_CMN_ERROR)
		if common&required == required && file&unix.ATTR_FILE_DATALENGTH != 0 && binary.LittleEndian.Uint32(rec[40:44]) == 1 {
			mod := syscall.Timespec{Sec: int64(binary.LittleEndian.Uint64(rec[44:52])), Nsec: int64(binary.LittleEndian.Uint64(rec[52:60]))}
			changed := syscall.Timespec{Sec: int64(binary.LittleEndian.Uint64(rec[60:68])), Nsec: int64(binary.LittleEndian.Uint64(rec[68:76]))}
			length := int64(binary.LittleEndian.Uint64(rec[96:104]))
			if length < 0 || mod.Nsec < 0 || mod.Nsec >= 1e9 || changed.Nsec < 0 || changed.Nsec >= 1e9 {
				return nil, fmt.Errorf("invalid bulk capture metadata")
			}
			access := binary.LittleEndian.Uint32(rec[84:88])
			entry.info = &captureBulkFileInfo{name: name, stat: syscall.Stat_t{
				Dev: int32(binary.LittleEndian.Uint32(rec[36:40])), Ino: binary.LittleEndian.Uint64(rec[88:96]), Size: length,
				Mode: uint16(access&07777) | syscall.S_IFREG, Uid: binary.LittleEndian.Uint32(rec[76:80]), Gid: binary.LittleEndian.Uint32(rec[80:84]), Mtimespec: mod, Ctimespec: changed,
			}}
		}
		entries = append(entries, entry)
		buf = buf[int(size):]
	}
	return entries, nil
}

var captureReadBulk = func(dir *os.File, buf []byte) (int, error) {
	attrs := unix.Attrlist{Bitmapcount: 5, Commonattr: captureBulkCommon, Fileattr: unix.ATTR_FILE_DATALENGTH}
	n, _, errno := syscall.Syscall6(unix.SYS_GETATTRLISTBULK, dir.Fd(), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), unix.FSOPT_PACK_INVAL_ATTRS, 0)
	runtime.KeepAlive(dir)
	runtime.KeepAlive(attrs)
	runtime.KeepAlive(buf)
	if errno != 0 {
		return 0, errno
	}
	if n > uintptr(len(buf)/96) {
		return 0, fmt.Errorf("invalid bulk capture syscall count")
	}
	return int(n), nil
}

// Only capture reconciliation uses bulk metadata. Directories, links and
// unsupported metadata retain rooted Lstat; cache misses also re-stat before
// cloning. Neither restore nor the general archive walker changes.
func walkCaptureRootBulk(root *os.Root, dir string, visit func(string, fs.FileInfo) error) error {
	return runCaptureBulkTraversal(root, dir, visit, 0)
}

func (w *captureBulkTraversal) walk(dir string, expected fs.FileInfo) (resultErr error) {
	defer func() { w.fail(resultErr) }()
	if err := w.check(); err != nil {
		return err
	}
	root := w.root
	var children sync.WaitGroup
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
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, openedInfo) || (expected != nil && !os.SameFile(expected, before)) {
		return fmt.Errorf("capture directory identity changed: %s", dir)
	}
	local, err := root.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer local.Close()
	defer func() { w.fail(resultErr); children.Wait() }()
	localInfo, err := local.Stat(".")
	if err != nil || !os.SameFile(before, localInfo) {
		return fmt.Errorf("capture directory changed while pinning children: %s", dir)
	}
	// uint64 backing gives the kernel an aligned, bounded 128 KiB buffer.
	storage := make([]uint64, (128<<10)/8)
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&storage[0])), len(storage)*8)
	emitted := false
	for {
		if err := w.check(); err != nil {
			return err
		}
		n, err := captureReadBulk(opened, buf)
		if err != nil {
			if !emitted && (errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EINVAL)) {
				if err := walkArchiveRootBatched(root, dir, w.emit); err != nil {
					return err
				}
				break
			}
			return err
		}
		if n == 0 {
			break
		}
		entries, err := decodeCaptureBulk(buf, n)
		if err != nil {
			return err
		}
		emitted = true
		for _, child := range entries {
			rel := filepath.ToSlash(filepath.Join(dir, child.name))
			if err := validateArchivePath(rel); err != nil {
				return err
			}
			info := child.info
			if info == nil {
				info, err = local.Lstat(child.name)
				if err != nil {
					return err
				}
			}
			if err := w.emit(rel, info); err != nil {
				if errors.Is(err, fs.SkipDir) && info.IsDir() {
					continue
				}
				return err
			}
			if info.IsDir() {
				if err := w.descend(filepath.FromSlash(rel), info, &children); err != nil {
					return err
				}
			}
		}
	}
	children.Wait()
	if err := w.check(); err != nil {
		return err
	}
	after, err := root.Lstat(dir)
	if err != nil || !after.IsDir() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) {
		return fmt.Errorf("capture directory identity changed after traversal: %s", dir)
	}
	return nil
}
