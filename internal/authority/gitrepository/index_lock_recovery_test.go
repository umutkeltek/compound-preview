package gitrepository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexLockLivenessFailsClosedOnProbeWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		idle         bool
	}{
		{"idle", "exit 1", true}, {"holder", "echo p123; exit 0", false}, {"warning", "echo warning >&2; exit 1", false}, {"ambiguous success", "exit 0", false}, {"probe failure", "exit 2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "lsof"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			err := LSOFIndexLockLiveness(context.Background(), filepath.Join(dir, "index"))
			if (err == nil) != tc.idle {
				t.Fatalf("idle=%v error=%v", tc.idle, err)
			}
		})
	}
}

func TestRecoverIndexLockFinalBoundaryRechecks(t *testing.T) {
	for _, mode := range []string{"expired", "holder", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			r, now := indexLockFixture(t)
			state, err := InspectIndexLock(context.Background(), r, now, idleIndexLock)
			if err != nil {
				t.Fatal(err)
			}
			authCalls, probeCalls := 0, 0
			authorize := func() error {
				authCalls++
				if mode == "expired" && authCalls == 3 {
					return fmt.Errorf("window expired after journal sync")
				}
				return nil
			}
			probe := func(context.Context, ...string) error {
				probeCalls++
				if probeCalls == 3 {
					switch mode {
					case "holder":
						return fmt.Errorf("late live holder")
					case "replacement":
						if err := os.Rename(filepath.Join(r, ".git/index.lock"), filepath.Join(r, ".git/old-lock")); err != nil {
							t.Fatal(err)
						}
						writeSnapshotFile(t, r, ".git/index.lock", "late replacement")
					}
				}
				return nil
			}
			result, err := RecoverIndexLock(context.Background(), state, probe, authorize)
			if err == nil || result.Changed {
				t.Fatalf("late boundary ignored: %+v %v", result, err)
			}
			data, err := os.ReadFile(filepath.Join(r, ".git/index.lock"))
			if err != nil {
				t.Fatal("lock removed")
			}
			if mode == "replacement" && !strings.Contains(string(data), "late replacement") {
				t.Fatal("replacement changed")
			}
		})
	}
}

func TestRecoverIndexLockRetainsPrivatePreimages(t *testing.T) {
	r, now := indexLockFixture(t)
	s, err := InspectIndexLock(context.Background(), r, now, idleIndexLock)
	if err != nil {
		t.Fatal(err)
	}
	result, err := RecoverIndexLock(context.Background(), s, idleIndexLock, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Phase != "verified" {
		t.Fatalf("unexpected recovery %+v", result)
	}
	if _, err := os.Stat(filepath.Join(r, ".git/index.lock")); !os.IsNotExist(err) {
		t.Fatal("lock still present")
	}
	for _, name := range []string{"preimage.json", "move-started.json", "lock.preimage", "verified.json"} {
		info, err := os.Stat(filepath.Join(result.RecoveryDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("private backup mode %o", info.Mode().Perm())
		}
	}
	if _, err := InspectRecoveredIndexLock(context.Background(), s.Snapshot, idleIndexLock); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverIndexLockDenialAndDriftNeverMoveLock(t *testing.T) {
	for _, mode := range []string{"deny", "replace", "wip"} {
		t.Run(mode, func(t *testing.T) {
			r, now := indexLockFixture(t)
			s, err := InspectIndexLock(context.Background(), r, now, idleIndexLock)
			if err != nil {
				t.Fatal(err)
			}
			check := func() error {
				switch mode {
				case "deny":
					return fmt.Errorf("denied")
				case "replace":
					if err := os.Rename(filepath.Join(r, ".git/index.lock"), filepath.Join(r, ".git/old-lock")); err != nil {
						t.Fatal(err)
					}
					writeSnapshotFile(t, r, ".git/index.lock", "active writer")
				case "wip":
					writeSnapshotFile(t, r, "seed.txt", "new foreign changes")
				}
				return nil
			}
			result, err := RecoverIndexLock(context.Background(), s, idleIndexLock, check)
			if err == nil || result.Changed {
				t.Fatalf("unsafe move result=%+v err=%v", result, err)
			}
			if _, err := os.Stat(filepath.Join(r, ".git/index.lock")); err != nil {
				t.Fatal("lock removed")
			}
		})
	}
}

func TestRecoverIndexLockPostMoveFailureRetainsEvidence(t *testing.T) {
	r, now := indexLockFixture(t)
	s, err := InspectIndexLock(context.Background(), r, now, idleIndexLock)
	if err != nil {
		t.Fatal(err)
	}
	result, err := recoverIndexLock(context.Background(), s, idleIndexLock, func() error { return nil }, func() error { return fmt.Errorf("simulated crash after move") })
	if err == nil || !result.Changed || result.Phase != "moved" {
		t.Fatalf("lost partial failure: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(result.RecoveryDir, "lock.preimage")); err != nil {
		t.Fatal(err)
	}
	writeSnapshotFile(t, r, ".git/index.lock", "new writer")
	if _, err := RecoverIndexLock(context.Background(), s, idleIndexLock, func() error { return nil }); err == nil {
		t.Fatal("retry moved replacement")
	}
	data, err := os.ReadFile(filepath.Join(r, ".git/index.lock"))
	if err != nil || string(data) != "new writer" {
		t.Fatal("replacement damaged")
	}
}

func TestRecoverIndexLockNeverOverwritesDestination(t *testing.T) {
	r, now := indexLockFixture(t)
	s, err := InspectIndexLock(context.Background(), r, now, idleIndexLock)
	if err != nil {
		t.Fatal(err)
	}
	result, err := RecoverIndexLockJournaled(context.Background(), s, idleIndexLock, func() error { return nil }, func(result IndexLockRecoveryResult) error {
		return os.WriteFile(filepath.Join(result.RecoveryDir, "lock.preimage"), []byte("foreign recovery"), 0600)
	})
	if err == nil || result.Changed {
		t.Fatalf("overwrote destination: %+v %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(result.RecoveryDir, "lock.preimage"))
	if err != nil || string(data) != "foreign recovery" {
		t.Fatal("foreign recovery damaged")
	}
	if _, err := os.Stat(filepath.Join(r, ".git/index.lock")); err != nil {
		t.Fatal("original lock moved")
	}
}
