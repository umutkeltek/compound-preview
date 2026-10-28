//go:build darwin

package home

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCapturedBackupRejectsPresealSourceKeyForgery(t *testing.T) {
	captureTestHooks(t)
	root, target := captureTestHome(t)
	var first string
	var oldInfo fs.FileInfo
	forged := false
	archiveCaptureBeforeClone = func(rel string) error {
		if first == "" {
			first = rel
			var err error
			oldInfo, err = os.Lstat(filepath.Join(root, rel))
			return err
		}
		if forged {
			return nil
		}
		forged = true
		stages, err := filepath.Glob(filepath.Join(root, "tmp", "home-backup-snapshot-*"))
		if err != nil || len(stages) != 1 {
			return fmt.Errorf("fixture stage: %v", err)
		}
		index, err := os.OpenFile(filepath.Join(stages[0], "cache-index"), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer index.Close()
		cache := &archiveCaptureCache{index: index}
		key, err := captureCacheKey(first, oldInfo)
		if err != nil {
			return err
		}
		original, _, err := cache.slot(key, false)
		if err != nil || original == nil {
			return fmt.Errorf("fixture original slot: %v", err)
		}
		original = append([]byte(nil), original...)
		if err := os.WriteFile(filepath.Join(root, first), bytes.Repeat([]byte("z"), int(oldInfo.Size())), 0600); err != nil {
			return err
		}
		current, err := os.Lstat(filepath.Join(root, first))
		if err != nil {
			return err
		}
		newKey, err := captureCacheKey(first, current)
		if err != nil {
			return err
		}
		if newKey == key {
			return fmt.Errorf("fixture did not change source key")
		}
		_, pos, err := cache.slot(newKey, true)
		if err != nil {
			return err
		}
		copy(original[:32], newKey[:])
		_, err = index.WriteAt(original, pos)
		return err
	}
	_, err := BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now())
	if !forged || err == nil || !strings.Contains(err.Error(), "warm reference identity mismatch") {
		t.Fatalf("forged pre-seal source mapping accepted or wrong refusal: forged=%v err=%v", forged, err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("published: %v", err)
	}
	requireCaptureClean(t, root)
}

func TestCapturedBackupPreservesReplacedBodyShard(t *testing.T) {
	captureTestHooks(t)
	root, target := captureTestHome(t)
	var replacement string
	archiveWarmCaptureReady = func(c *capturedArchive) error {
		stage := filepath.Join(root, "tmp", c.stageName)
		shard := filepath.Join(stage, captureShardName(0))
		if err := os.Rename(shard, shard+"-displaced"); err != nil {
			return err
		}
		if err := os.Mkdir(shard, 0700); err != nil {
			return err
		}
		replacement = filepath.Join(shard, captureBodyLeaf(1))
		return os.WriteFile(replacement, []byte("replacement must survive"), 0600)
	}
	if _, err := BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now()); err == nil {
		t.Fatal("replaced shard accepted")
	}
	body, err := os.ReadFile(replacement)
	if err != nil || string(body) != "replacement must survive" {
		t.Fatalf("cleanup touched replacement: %q %v", body, err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("published: %v", err)
	}
}

func TestCapturedBackupRejectsReferenceIndexCorruption(t *testing.T) {
	for _, field := range []string{"offset", "length", "body", "size"} {
		t.Run(field, func(t *testing.T) {
			captureTestHooks(t)
			root, target := captureTestHome(t)
			var first string
			corrupted := false
			archiveCaptureBeforeClone = func(rel string) error {
				if first == "" {
					first = rel
					return nil
				}
				if corrupted {
					return nil
				}
				corrupted = true
				info, err := os.Lstat(filepath.Join(root, first))
				if err != nil {
					return err
				}
				stages, err := filepath.Glob(filepath.Join(root, "tmp", "home-backup-snapshot-*"))
				if err != nil || len(stages) != 1 {
					return fmt.Errorf("stage: %v", err)
				}
				f, err := os.OpenFile(filepath.Join(stages[0], "cache-index"), os.O_RDWR, 0)
				if err != nil {
					return err
				}
				defer f.Close()
				cache := &archiveCaptureCache{index: f}
				key, err := captureCacheKey(first, info)
				if err != nil {
					return err
				}
				slot, pos, err := cache.slot(key, false)
				if err != nil || slot == nil {
					return fmt.Errorf("slot: %v", err)
				}
				switch field {
				case "offset":
					binary.LittleEndian.PutUint64(slot[32:40], ^uint64(0))
				case "length":
					binary.LittleEndian.PutUint32(slot[40:44], archiveCaptureRecordBytes+1)
				case "body":
					binary.LittleEndian.PutUint32(slot[44:48], 0)
				case "size":
					binary.LittleEndian.PutUint64(slot[48:56], ^uint64(0))
				}
				_, err = f.WriteAt(slot, pos)
				return err
			}
			if _, err := BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now()); err == nil || !corrupted {
				t.Fatalf("corrupt reference accepted: %v", err)
			}
			if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("published: %v", err)
			}
			requireCaptureClean(t, root)
		})
	}
}

// Diagnostic only: real namespace/stat/index work, synthetic clone records.
// This does not serialize, verify, or restore an archive and cannot certify backup.
func BenchmarkCapturedReconcileCardinality(b *testing.B) {
	if b.N != 1 {
		b.Fatal("use -benchtime=1x")
	}
	count := 100000
	if raw := os.Getenv("COMPOUND_CAPTURE_BENCH_FILES"); raw != "" {
		var err error
		count, err = strconv.Atoi(raw)
		if err != nil || count < 1 || count > 1000000 {
			b.Fatal("invalid cardinality")
		}
	}
	b.StopTimer()
	root := filepath.Join(b.TempDir(), "home")
	if retained := os.Getenv("COMPOUND_CAPTURE_RECONCILE_FIXTURE"); retained != "" {
		// Explicitly retained synthetic fixtures avoid rebuilding a million files
		// for each diagnostic. They are not backup/restore evidence.
		marker, err := os.ReadFile(filepath.Join(filepath.Dir(retained), "reconcile-fixture"))
		if err != nil || string(marker) != fmt.Sprintf("compound synthetic reconcile fixture\n%s\n%d\n", retained, count) {
			b.Fatal("retained diagnostic fixture lacks exact caller-owned marker")
		}
		info, err := os.Lstat(retained)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !filepath.IsAbs(retained) {
			b.Fatal("invalid retained fixture")
		}
		root = retained
	}
	if err := Init(root, "test", time.Now()); err != nil {
		b.Fatal(err)
	}
	if err := WithAutonomyAdmissionLock(root, func() error { return nil }); err != nil {
		b.Fatal(err)
	}
	for part := 0; part < 256; part++ {
		if err := os.MkdirAll(filepath.Join(root, "context", fmt.Sprintf("%03d", part)), 0700); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < count; i++ {
		name := filepath.Join(root, "context", fmt.Sprintf("%03d", i%256), fmt.Sprintf("%09d", i))
		data, err := os.ReadFile(name)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.WriteFile(name, []byte("fixture"), 0600); err != nil {
				b.Fatal(err)
			}
		} else if err != nil || string(data) != "fixture" {
			b.Fatal("retained fixture body changed")
		}
		if i > 0 && i%100000 == 0 {
			b.Logf("diagnostic-fixture=%d", i)
		}
	}
	opened, err := openStableArchiveRoot(root)
	if err != nil {
		b.Fatal(err)
	}
	c := &capturedArchive{root: opened}
	defer func() {
		c.bodies = 0
		if err := c.close(); err != nil {
			b.Error(err)
		}
	}()
	if err := c.createStage(); err != nil {
		b.Fatal(err)
	}
	c.shards, err = newArchiveCaptureShards(c.stage)
	if err != nil {
		b.Fatal(err)
	}
	c.cache, err = newArchiveCaptureCache(c.stage)
	if err != nil {
		b.Fatal(err)
	}
	journal, err := c.stage.OpenFile("journal", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		b.Fatal(err)
	}
	digest := sha256.New()
	writer := bufio.NewWriterSize(io.MultiWriter(journal, digest), 1<<20)
	var offset int64
	err = walkArchiveRootBatched(c.root, ".", func(rel string, info fs.FileInfo) error {
		if rel == ManifestName {
			return nil
		}
		if !includeBackupPath(rel, info) {
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		header, err := archiveFileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		record := archiveCaptureRecord{Header: *header}
		if info.Mode().IsRegular() {
			c.bodies++
			record.Body = c.bodies
			record.Stamp = captureStamp(info)
			key, err := captureCacheKey(rel, info)
			if err != nil {
				return err
			}
			record.SourceKey = hex.EncodeToString(key[:])
		}
		data, err := marshalArchiveCaptureRecord(record)
		if err != nil {
			return err
		}
		if _, err = writer.Write(data); err != nil {
			return err
		}
		if record.Body > 0 {
			if err := c.cache.add(rel, info, offset, len(data), record.Body, header.Size); err != nil {
				return err
			}
		}
		offset += int64(len(data))
		return nil
	})
	if err != nil {
		journal.Close()
		b.Fatal(err)
	}
	if err = writer.Flush(); err != nil {
		b.Fatal(err)
	}
	if err = journal.Close(); err != nil {
		b.Fatal(err)
	}
	c.journalHash = hex.EncodeToString(digest.Sum(nil))
	if err = c.cache.seal(c.stage, c.journalHash); err != nil {
		b.Fatal(err)
	}
	bodies := c.bodies
	b.ResetTimer()
	b.StartTimer()
	err = WithAutonomyAdmissionLock(root, func() error { return c.reconcile(root) })
	b.StopTimer()
	b.Logf("diagnostic-reconcile files=%d records=%d references=%d elapsed=%s error=%v; no archive/restore proof", count, c.records, c.references, c.admissionDuration, err)
	if err != nil {
		b.Fatal(err)
	}
	if c.bodies != bodies {
		b.Fatal("diagnostic unexpectedly cloned live data")
	}
	b.ReportMetric(c.admissionDuration.Seconds(), "reconcile-s")
}
