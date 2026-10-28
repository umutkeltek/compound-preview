package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func streamedDurabilityArchive(t *testing.T) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	if err := Init(source, "test", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := os.WriteFile(filepath.Join(source, "ledger", fmt.Sprintf("item-%d.json", i)), []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := BackupWithFormat(source, archive, ArchiveFormatStreamedV2, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	return archive
}

func TestStreamedRestoreDurabilityBoundaryOrdering(t *testing.T) {
	archive := streamedDurabilityArchive(t)
	target := filepath.Join(t.TempDir(), "restored")
	fileSync, barrier, parentSync, rename, dirSync := restoreStreamedFileSync, restoreStreamedDeviceBarrier, restoreStreamedPublishParentSync, restoreNoReplace, restoreSyncStagingDirectory
	t.Cleanup(func() {
		restoreStreamedFileSync, restoreStreamedDeviceBarrier, restoreStreamedPublishParentSync, restoreNoReplace, restoreSyncStagingDirectory = fileSync, barrier, parentSync, rename, dirSync
	})
	files, barriers, parents, ledgerDirs := 0, 0, 0, 0
	published := false
	restoreStreamedFileSync = func(file *os.File) error {
		if barriers != 0 {
			t.Fatal("file sync after publication barrier")
		}
		files++
		return fileSync(file)
	}
	restoreSyncStagingDirectory = func(root *os.Root, rel, display, operation string) error {
		if rel == "ledger" {
			ledgerDirs++
		}
		return dirSync(root, rel, display, operation)
	}
	restoreStreamedDeviceBarrier = func(staging *archiveRestoreTarget) error {
		barriers++
		if files != 4 || ledgerDirs != 1 {
			t.Fatalf("barrier before file/directory syncs: files=%d dirs=%d", files, ledgerDirs)
		}
		if barriers == 1 && published {
			t.Fatal("first barrier followed publication")
		}
		if barriers == 2 && (!published || parents != 1) {
			t.Fatal("second barrier preceded durable parent publication")
		}
		return barrier(staging)
	}
	restoreNoReplace = func(parent *os.File, from, to string) error {
		if barriers != 1 {
			t.Fatalf("rename after %d barriers, want one", barriers)
		}
		if err := rename(parent, from, to); err != nil {
			return err
		}
		published = true
		return nil
	}
	restoreStreamedPublishParentSync = func(parent *os.File, info os.FileInfo) error {
		if !published {
			t.Fatal("publication parent sync before rename")
		}
		parents++
		return parentSync(parent, info)
	}
	if _, err := RestoreArchive(archive, target, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	if files != 4 || barriers != 2 || parents != 1 {
		t.Fatalf("missing durability calls: files=%d barriers=%d parents=%d", files, barriers, parents)
	}
}

func TestStreamedRestoreDurabilityFailures(t *testing.T) {
	for _, phase := range []string{"file", "prepublish_barrier", "postpublish_parent", "postpublish_barrier"} {
		t.Run(phase, func(t *testing.T) {
			archive := streamedDurabilityArchive(t)
			target := filepath.Join(t.TempDir(), "restored")
			fileSync, barrier, parentSync := restoreStreamedFileSync, restoreStreamedDeviceBarrier, restoreStreamedPublishParentSync
			t.Cleanup(func() {
				restoreStreamedFileSync, restoreStreamedDeviceBarrier, restoreStreamedPublishParentSync = fileSync, barrier, parentSync
			})
			injected := errors.New("injected durability failure")
			restoreStreamedFileSync = func(file *os.File) error {
				if phase == "file" {
					return injected
				}
				return fileSync(file)
			}
			barriers := 0
			restoreStreamedDeviceBarrier = func(staging *archiveRestoreTarget) error {
				barriers++
				if phase == "prepublish_barrier" && barriers == 1 || phase == "postpublish_barrier" && barriers == 2 {
					return injected
				}
				return barrier(staging)
			}
			restoreStreamedPublishParentSync = func(parent *os.File, info os.FileInfo) error {
				if phase == "postpublish_parent" {
					return injected
				}
				return parentSync(parent, info)
			}
			_, err := RestoreArchive(archive, target, archiveTestNow)
			if !errors.Is(err, injected) {
				t.Fatalf("lost durability failure: %v", err)
			}
			_, statErr := os.Stat(target)
			if strings.HasPrefix(phase, "postpublish") {
				if statErr != nil || !strings.Contains(err.Error(), "published") {
					t.Fatalf("published target not retained or error untruthful: stat=%v err=%v", statErr, err)
				}
			} else if !os.IsNotExist(statErr) {
				t.Fatalf("failed durability published target: %v", statErr)
			}
		})
	}
}

func TestLegacyRestoreDoesNotUseStreamedDurability(t *testing.T) {
	archive := backupArchiveFixture(t)
	fileSync, barrier, parentSync := restoreStreamedFileSync, restoreStreamedDeviceBarrier, restoreStreamedPublishParentSync
	t.Cleanup(func() {
		restoreStreamedFileSync, restoreStreamedDeviceBarrier, restoreStreamedPublishParentSync = fileSync, barrier, parentSync
	})
	restoreStreamedFileSync = func(*os.File) error { return errors.New("legacy entered streamed file sync") }
	restoreStreamedDeviceBarrier = func(*archiveRestoreTarget) error { return errors.New("legacy entered streamed barrier") }
	restoreStreamedPublishParentSync = func(*os.File, os.FileInfo) error { return errors.New("legacy entered streamed parent sync") }
	if _, err := RestoreArchive(archive, filepath.Join(t.TempDir(), "restored"), archiveTestNow); err != nil {
		t.Fatal(err)
	}
}

func TestStreamedRestoreVerifiesBeforeDurabilityBarrier(t *testing.T) {
	archive := streamedDurabilityArchive(t)
	beforePublish, barrier := restoreBeforePublish, restoreStreamedDeviceBarrier
	t.Cleanup(func() { restoreBeforePublish, restoreStreamedDeviceBarrier = beforePublish, barrier })
	restoreBeforePublish = func(staging, _ string) error {
		return os.WriteFile(filepath.Join(staging, "ledger", "item-0.json"), []byte("changed"), 0o600)
	}
	calls := 0
	restoreStreamedDeviceBarrier = func(staging *archiveRestoreTarget) error { calls++; return barrier(staging) }
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreArchive(archive, target, archiveTestNow); err == nil {
		t.Fatal("accepted changed staged bytes")
	}
	if calls != 0 {
		t.Fatalf("device barrier ran before final verification: %d", calls)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("changed staging published: %v", err)
	}
}
