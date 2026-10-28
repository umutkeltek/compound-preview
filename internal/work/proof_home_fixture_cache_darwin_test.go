package work

import (
	"os"
	"syscall"
	"testing"
)

type fixtureParentInfo struct {
	os.FileInfo
	mode os.FileMode
	uid  uint32
}

func (i fixtureParentInfo) Mode() os.FileMode { return i.mode }
func (i fixtureParentInfo) IsDir() bool       { return i.mode.IsDir() }
func (i fixtureParentInfo) Sys() any          { return &syscall.Stat_t{Uid: i.uid} }

func TestHomeFixtureSharedParentAdmission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  os.FileMode
		uid   uint32
		allow bool
	}{
		{"root sticky shared directory", os.ModeDir | os.ModeSticky | 0777, 0, true},
		{"root directory without external writes", os.ModeDir | 0755, 0, true},
		{"world writable without sticky", os.ModeDir | 0777, 0, false},
		{"group writable without sticky", os.ModeDir | 0775, 0, false},
		{"foreign owner can rename private children", os.ModeDir | os.ModeSticky | 0777, 12345, false},
		{"symlink parent", os.ModeDir | os.ModeSymlink | os.ModeSticky | 0777, 0, false},
		{"ordinary file", 0600, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := homeFixtureSharedParentSafe(fixtureParentInfo{mode: tc.mode, uid: tc.uid}); got != tc.allow {
				t.Fatalf("shared parent admission = %v, want %v", got, tc.allow)
			}
		})
	}
	if homeFixtureSharedParentSafe(nil) {
		t.Fatal("missing shared parent admitted")
	}
}
