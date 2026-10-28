//go:build darwin

package home

import (
	"encoding/binary"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCapturedBulkMetadataMatchesRootedStat(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 1800; i++ {
		name := filepath.Join(dir, fmtBulkName(i))
		if err := os.WriteFile(name, []byte("unique "+fmtBulkName(i)), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fmtBulkName(0), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, fmtBulkName(0)), fs.ModeSetuid|0750); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, fmtBulkName(1)), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	seen := map[string]bool{}
	bulk := 0
	err = walkCaptureRootBulk(root, ".", func(rel string, info fs.FileInfo) error {
		if seen[rel] {
			t.Fatalf("duplicate %s", rel)
		}
		seen[rel] = true
		actual, err := root.Lstat(rel)
		if err != nil {
			return err
		}
		if !captureStamp(info).matches(actual) {
			t.Fatalf("metadata mismatch for %s: bulk=%+v rooted=%+v", rel, captureStamp(info), captureStamp(actual))
		}
		if _, ok := info.(*captureBulkFileInfo); ok {
			bulk++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1802 || bulk != 1800 {
		t.Fatalf("seen=%d bulk=%d", len(seen), bulk)
	}
}
func fmtBulkName(i int) string { return fmt.Sprintf("%06d.fixture", i) }
func bulkSyntheticRecord() []byte {
	b := make([]byte, 112)
	put := binary.LittleEndian
	put.PutUint32(b[:4], 112)
	put.PutUint32(b[4:8], captureBulkCommon)
	put.PutUint32(b[16:20], unix.ATTR_FILE_DATALENGTH)
	put.PutUint32(b[28:32], 76)
	put.PutUint32(b[32:36], 2)
	put.PutUint32(b[36:40], 1)
	put.PutUint32(b[40:44], 1)
	put.PutUint32(b[84:88], 0640)
	put.PutUint64(b[88:96], 42)
	b[104] = 'x'
	return b
}
func TestCapturedBulkRejectsMalformedRecords(t *testing.T) {
	cases := map[string]func([]byte){
		"short length":         func(b []byte) { binary.LittleEndian.PutUint32(b[:4], 100) },
		"oversize length":      func(b []byte) { binary.LittleEndian.PutUint32(b[:4], 4096) },
		"name inside fields":   func(b []byte) { binary.LittleEndian.PutUint32(b[28:32], 0) },
		"negative name offset": func(b []byte) { binary.LittleEndian.PutUint32(b[28:32], 0xffffffff) },
		"name overflow":        func(b []byte) { binary.LittleEndian.PutUint32(b[32:36], 4096) },
		"no terminator":        func(b []byte) { b[105] = 'a' },
		"path separator":       func(b []byte) { b[104] = '/' },
		"invalid utf8":         func(b []byte) { b[104] = 0xff },
		"unknown attrs":        func(b []byte) { binary.LittleEndian.PutUint32(b[8:12], 1) },
		"entry error":          func(b []byte) { binary.LittleEndian.PutUint32(b[24:28], uint32(syscall.EIO)) },
		"bad nanoseconds":      func(b []byte) { binary.LittleEndian.PutUint64(b[52:60], 1e9) },
		"negative size":        func(b []byte) { binary.LittleEndian.PutUint64(b[96:104], ^uint64(0)) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := bulkSyntheticRecord()
			mutate(b)
			if _, err := decodeCaptureBulk(b, 1); err == nil {
				t.Fatal("malformed kernel record accepted")
			}
		})
	}
	if _, err := decodeCaptureBulk(bulkSyntheticRecord(), 2); err == nil {
		t.Fatal("count overflow accepted")
	}
	b := bulkSyntheticRecord()
	binary.LittleEndian.PutUint32(b[4:8], captureBulkCommon & ^uint32(unix.ATTR_CMN_MODTIME))
	entries, err := decodeCaptureBulk(b, 1)
	if err != nil || entries[0].info != nil {
		t.Fatalf("missing metadata was trusted: %v %v", entries, err)
	}
}
func TestCapturedBulkFallbackAndLateFailure(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("file", []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	original := captureReadBulk
	t.Cleanup(func() { captureReadBulk = original })
	captureReadBulk = func(*os.File, []byte) (int, error) { return 0, syscall.ENOTSUP }
	seen := 0
	if err := walkCaptureRootBulk(root, ".", func(_ string, info fs.FileInfo) error {
		seen++
		if _, ok := info.(*captureBulkFileInfo); ok {
			t.Fatal("fallback used bulk")
		}
		return nil
	}); err != nil || seen != 1 {
		t.Fatalf("fallback: %d %v", seen, err)
	}
	calls := 0
	captureReadBulk = func(_ *os.File, b []byte) (int, error) {
		calls++
		if calls == 1 {
			copy(b, bulkSyntheticRecord())
			return 1, nil
		}
		return 0, syscall.ENOTSUP
	}
	seen = 0
	err = walkCaptureRootBulk(root, ".", func(string, fs.FileInfo) error { seen++; return nil })
	if !errors.Is(err, syscall.ENOTSUP) || seen != 1 {
		t.Fatalf("late error retried: %d %v", seen, err)
	}
}
