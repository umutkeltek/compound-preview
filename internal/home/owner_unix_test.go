//go:build unix

package home

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type syntheticOwnerInfo struct {
	uid uint32
}

func (i syntheticOwnerInfo) Name() string       { return "synthetic" }
func (i syntheticOwnerInfo) Size() int64        { return 0 }
func (i syntheticOwnerInfo) Mode() fs.FileMode  { return 0o700 | fs.ModeDir }
func (i syntheticOwnerInfo) ModTime() time.Time { return time.Time{} }
func (i syntheticOwnerInfo) IsDir() bool        { return true }
func (i syntheticOwnerInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }

func TestOwnedByCurrentUsesKernelUID(t *testing.T) {
	current := uint32(os.Getuid())
	if !ownedByCurrent(syntheticOwnerInfo{uid: current}) {
		t.Fatal("current kernel uid was rejected")
	}
	foreign := current + 1
	if foreign == current {
		foreign++
	}
	if ownedByCurrent(syntheticOwnerInfo{uid: foreign}) {
		t.Fatal("foreign kernel uid was accepted")
	}
}

func TestRepairPrivateCustodyRejectsForeignOwnedAncestorWhenPermitted(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	writePrivateObject(t, homePath, []byte("foreign ancestor"), 0o644)
	privatePath := filepath.Join(homePath, "private")
	foreign := os.Getuid() + 1
	if err := os.Chown(privatePath, foreign, -1); err != nil {
		t.Skipf("current test user cannot construct foreign-owned fixture: %v", err)
	}

	report := RepairPrivateCustody(homePath, PrivateCustodyOptions{})
	if report.OK || report.Counts.Manual == 0 || report.Changed {
		t.Fatalf("foreign-owned private ancestor was mutated or accepted: %+v", report)
	}
}
