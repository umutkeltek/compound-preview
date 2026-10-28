package home

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func newWorkAdmissionTestHome(t *testing.T) string {
	t.Helper()
	homePath := t.TempDir()
	materializeWorkAdmissionAuthorityRoot(t, homePath)
	return homePath
}

func materializeWorkAdmissionAuthorityRoot(t *testing.T, homePath string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(homePath, workAdmissionAuthorityRootRelative), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestWorkAdmissionLockUsesLegacyCapabilityLifecycleInode(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	workID := "work_admission_compatibility"
	wantPath := filepath.Join(homePath, "lock", "capability-lifecycle-work-admission", workID+".lock")

	if err := WithWorkAdmissionLock(homePath, workID, func() error {
		if _, err := os.Stat(wantPath); err != nil {
			return err
		}
		acquired, err := TryWithWorkAdmissionLock(homePath, workID, func() error {
			t.Fatal("nonblocking admission callback ran while the work lock was held")
			return nil
		})
		if err != nil {
			return err
		}
		if acquired {
			t.Fatal("nonblocking admission acquired the legacy inode while it was held")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	acquired, err := TryWithWorkAdmissionLock(homePath, workID, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("nonblocking admission did not acquire the work lock after release")
	}
}

func TestWorkAdmissionLockIsNonReentrantAcrossWorkIDs(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	callbackRan := false
	if err := WithWorkAdmissionLock(homePath, "work_nonreentrant_a", func() error {
		acquired, err := TryWithWorkAdmissionLock(homePath, "work_nonreentrant_b", func() error {
			callbackRan = true
			return nil
		})
		if err != nil {
			return err
		}
		if acquired {
			return errors.New("nested different-work admission acquired the global authority lock")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if callbackRan {
		t.Fatal("nested different-work admission invoked its callback")
	}
}

func TestWorkAdmissionLockRejectsInvalidIdentityWithoutFootprint(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	for _, workID := range []string{"", ".", "../work_escape", "nested/work", " work_padded"} {
		t.Run(workID, func(t *testing.T) {
			if err := WithWorkAdmissionLock(homePath, workID, func() error {
				t.Fatal("invalid work identity callback ran")
				return nil
			}); err == nil {
				t.Fatalf("WithWorkAdmissionLock(%q) succeeded", workID)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(homePath, "lock")); !os.IsNotExist(err) {
		t.Fatalf("invalid identities materialized lock state: %v", err)
	}
}

func TestWorkAdmissionLockReleasesFileLockAfterCallbackPanic(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	workID := "work_admission_panic_release"
	func() {
		defer func() {
			if recovered := recover(); recovered != "injected work admission panic" {
				t.Fatalf("recovered %v, want injected panic", recovered)
			}
		}()
		_ = WithWorkAdmissionLock(homePath, workID, func() error {
			panic("injected work admission panic")
		})
	}()

	acquired, err := TryWithWorkAdmissionLock(homePath, workID, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("callback panic leaked the cross-process work admission lock")
	}
}

func TestWorkAdmissionLockRejectsSymlinksBeneathPinnedHome(t *testing.T) {
	workID := "work_admission_symlink_custody"
	tests := []struct {
		name  string
		setup func(t *testing.T, homePath, outsidePath, lockPath string)
	}{
		{
			name: "lock directory",
			setup: func(t *testing.T, homePath, outsidePath, _ string) {
				t.Helper()
				if err := os.Symlink(outsidePath, filepath.Join(homePath, "lock")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "admission directory",
			setup: func(t *testing.T, homePath, outsidePath, _ string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(homePath, "lock"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outsidePath, filepath.Join(homePath, "lock", "capability-lifecycle-work-admission")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock leaf",
			setup: func(t *testing.T, _ string, outsidePath, lockPath string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
					t.Fatal(err)
				}
				outsideFile := filepath.Join(outsidePath, "outside.lock")
				if err := os.WriteFile(outsideFile, []byte("sentinel"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outsideFile, lockPath); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			homePath := newWorkAdmissionTestHome(t)
			outsidePath := t.TempDir()
			lockPath := filepath.Join(homePath, "lock", "capability-lifecycle-work-admission", workID+".lock")
			test.setup(t, homePath, outsidePath, lockPath)
			callbackRan := false
			err := WithWorkAdmissionLock(homePath, workID, func() error {
				callbackRan = true
				return nil
			})
			if err == nil {
				t.Fatal("unsafe symlink path was accepted")
			}
			if callbackRan {
				t.Fatal("callback ran through unsafe symlink path")
			}
			if test.name == "lock leaf" {
				body, readErr := os.ReadFile(filepath.Join(outsidePath, "outside.lock"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !bytes.Equal(body, []byte("sentinel")) {
					t.Fatalf("outside lock body changed: %q", body)
				}
			}
		})
	}
}

func TestWorkAdmissionLockRejectsHardLinkedAndNonregularLeaves(t *testing.T) {
	workID := "work_admission_leaf_custody"
	tests := []struct {
		name  string
		setup func(t *testing.T, outsidePath, lockPath string)
	}{
		{
			name: "hard link",
			setup: func(t *testing.T, outsidePath, lockPath string) {
				t.Helper()
				outsideFile := filepath.Join(outsidePath, "outside.lock")
				if err := os.WriteFile(outsideFile, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(outsideFile, lockPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory",
			setup: func(t *testing.T, _ string, lockPath string) {
				t.Helper()
				if err := os.Mkdir(lockPath, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			homePath := newWorkAdmissionTestHome(t)
			outsidePath := t.TempDir()
			lockPath := filepath.Join(homePath, "lock", "capability-lifecycle-work-admission", workID+".lock")
			if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
				t.Fatal(err)
			}
			test.setup(t, outsidePath, lockPath)
			callbackRan := false
			err := WithWorkAdmissionLock(homePath, workID, func() error {
				callbackRan = true
				return nil
			})
			if err == nil {
				t.Fatalf("unsafe %s leaf was accepted", test.name)
			}
			if callbackRan {
				t.Fatalf("callback ran through unsafe %s leaf", test.name)
			}
		})
	}
}

func TestWorkAdmissionLockRejectsLeafReplacementBeforeOpen(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	workID := "work_admission_replace_before_open"
	lockPath := filepath.Join(homePath, workAdmissionDirectoryRelative, workID+".lock")
	if err := WithWorkAdmissionLock(homePath, workID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(filepath.Dir(lockPath), "replacement.lock")
	if err := os.WriteFile(replacement, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	previous := workAdmissionBeforeOpen
	t.Cleanup(func() { workAdmissionBeforeOpen = previous })
	var hookErr error
	workAdmissionBeforeOpen = func(string) {
		if hookErr == nil {
			hookErr = os.Rename(replacement, lockPath)
		}
	}
	callbackRan := false
	err := WithWorkAdmissionLock(homePath, workID, func() error {
		callbackRan = true
		return nil
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || callbackRan {
		t.Fatalf("path replacement before open was accepted: err=%v callback=%v", err, callbackRan)
	}
}

func TestWorkAdmissionLockRevalidatesLeafAfterFlock(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	workID := "work_admission_replace_after_flock"
	lockPath := filepath.Join(homePath, workAdmissionDirectoryRelative, workID+".lock")
	if err := WithWorkAdmissionLock(homePath, workID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(filepath.Dir(lockPath), "replacement.lock")
	if err := os.WriteFile(replacement, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	previous := workAdmissionAfterFlock
	t.Cleanup(func() { workAdmissionAfterFlock = previous })
	var hookErr error
	workAdmissionAfterFlock = func(string) {
		if hookErr == nil {
			hookErr = os.Rename(replacement, lockPath)
		}
	}
	callbackRan := false
	err := WithWorkAdmissionLock(homePath, workID, func() error {
		callbackRan = true
		return nil
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || callbackRan {
		t.Fatalf("path replacement after flock was accepted: err=%v callback=%v", err, callbackRan)
	}

	workAdmissionAfterFlock = previous
	if err := WithWorkAdmissionLock(homePath, workID, func() error { return nil }); err != nil {
		t.Fatalf("replacement refusal leaked admission lock: %v", err)
	}
}

func TestWorkAdmissionLockRevalidatesPinnedHomeAfterFlock(t *testing.T) {
	parent := t.TempDir()
	homePath := filepath.Join(parent, "home")
	oldHomePath := filepath.Join(parent, "home-old")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	materializeWorkAdmissionAuthorityRoot(t, homePath)
	workID := "work_admission_home_replace"
	previous := workAdmissionAfterFlock
	t.Cleanup(func() { workAdmissionAfterFlock = previous })
	var hookErr error
	workAdmissionAfterFlock = func(string) {
		if hookErr != nil {
			return
		}
		if err := os.Rename(homePath, oldHomePath); err != nil {
			hookErr = err
			return
		}
		hookErr = os.Mkdir(homePath, 0o700)
	}
	callbackRan := false
	err := WithWorkAdmissionLock(homePath, workID, func() error {
		callbackRan = true
		return nil
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || callbackRan {
		t.Fatalf("pinned-home replacement was accepted: err=%v callback=%v", err, callbackRan)
	}
}

func TestWorkAdmissionLockAllowsStableHomeSymlinkAlias(t *testing.T) {
	parent := t.TempDir()
	realHome := filepath.Join(parent, "real-home")
	aliasHome := filepath.Join(parent, "home-alias")
	if err := os.Mkdir(realHome, 0o700); err != nil {
		t.Fatal(err)
	}
	materializeWorkAdmissionAuthorityRoot(t, realHome)
	if err := os.Symlink(realHome, aliasHome); err != nil {
		t.Fatal(err)
	}
	workID := "work_admission_home_alias"
	if err := WithWorkAdmissionLock(aliasHome, workID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(realHome, workAdmissionDirectoryRelative, workID+".lock")); err != nil {
		t.Fatalf("stable home alias did not use canonical legacy inode: %v", err)
	}
}

func TestWorkAdmissionLockExcludesRawLegacyFlock(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	workID := "work_admission_raw_legacy"
	lockPath := filepath.Join(homePath, workAdmissionDirectoryRelative, workID+".lock")
	if err := WithWorkAdmissionLock(homePath, workID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := syscall.Flock(int(raw.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	callbackRan := false
	acquired, err := TryWithWorkAdmissionLock(homePath, workID, func() error {
		callbackRan = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if acquired || callbackRan {
		t.Fatalf("raw legacy flock did not exclude shared API: acquired=%v callback=%v", acquired, callbackRan)
	}
	if err := syscall.Flock(int(raw.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	acquired, err = TryWithWorkAdmissionLock(homePath, workID, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("shared API did not acquire after raw legacy flock release")
	}
}

func TestWorkAdmissionLockRejectsUnsafeOwnershipAndModes(t *testing.T) {
	workID := "work_admission_unsafe_mode"
	tests := []struct {
		name  string
		setup func(t *testing.T, homePath, lockPath string)
	}{
		{
			name: "home mode",
			setup: func(t *testing.T, homePath, _ string) {
				t.Helper()
				if err := os.Chmod(homePath, 0o770); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock directory mode",
			setup: func(t *testing.T, homePath, _ string) {
				t.Helper()
				lockDir := filepath.Join(homePath, "lock")
				if err := os.Mkdir(lockDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(lockDir, 0o777); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "admission directory mode",
			setup: func(t *testing.T, _ string, lockPath string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Dir(lockPath), 0o777); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock file mode",
			setup: func(t *testing.T, _ string, lockPath string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(lockPath, 0o666); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			homePath := newWorkAdmissionTestHome(t)
			lockPath := filepath.Join(homePath, workAdmissionDirectoryRelative, workID+".lock")
			test.setup(t, homePath, lockPath)
			callbackRan := false
			err := WithWorkAdmissionLock(homePath, workID, func() error {
				callbackRan = true
				return nil
			})
			if err == nil || callbackRan {
				t.Fatalf("unsafe %s was accepted: err=%v callback=%v", test.name, err, callbackRan)
			}
		})
	}

	homePath := newWorkAdmissionTestHome(t)
	previousUID := workAdmissionCurrentUID
	t.Cleanup(func() { workAdmissionCurrentUID = previousUID })
	workAdmissionCurrentUID = func() int { return os.Geteuid() + 1 }
	callbackRan := false
	err := WithWorkAdmissionLock(homePath, workID, func() error {
		callbackRan = true
		return nil
	})
	if err == nil || callbackRan {
		t.Fatalf("non-owner home was accepted: err=%v callback=%v", err, callbackRan)
	}
}

func TestWorkAdmissionGuardScopesExactIdentityAndExpires(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	otherHome := t.TempDir()
	workID := "work_admission_guard_scope"
	var escaped *WorkAdmissionGuard

	err := WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
		if guard == nil {
			t.Fatal("WithWorkAdmissionGuard supplied a nil guard")
		}
		escaped = guard
		if err := guard.Validate(homePath, workID); err != nil {
			t.Fatalf("active guard rejected exact identity: %v", err)
		}
		if err := guard.Validate(otherHome, workID); err == nil {
			t.Fatal("active guard accepted a different canonical home")
		}
		if err := guard.Validate(homePath, "work_admission_guard_other"); err == nil {
			t.Fatal("active guard accepted a different work ID")
		}
		var zero WorkAdmissionGuard
		if err := zero.Validate(homePath, workID); err == nil {
			t.Fatal("zero WorkAdmissionGuard validated")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if escaped == nil {
		t.Fatal("guard did not escape for expiry falsifier")
	}
	if err := escaped.Validate(homePath, workID); err == nil {
		t.Fatal("escaped guard remained valid after callback exit")
	}
}

func TestTryWorkAdmissionGuardReportsBusyAndProvidesActiveGuard(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	workID := "work_admission_guard_try"
	busyCallbackRan := false
	if err := WithWorkAdmissionLock(homePath, workID, func() error {
		acquired, err := TryWithWorkAdmissionGuard(homePath, workID, func(*WorkAdmissionGuard) error {
			busyCallbackRan = true
			return nil
		})
		if err != nil {
			return err
		}
		if acquired {
			t.Fatal("TryWithWorkAdmissionGuard acquired a busy work lock")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if busyCallbackRan {
		t.Fatal("TryWithWorkAdmissionGuard invoked its callback while busy")
	}

	callbackRan := false
	acquired, err := TryWithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
		callbackRan = true
		return guard.Validate(homePath, workID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !acquired || !callbackRan {
		t.Fatalf("TryWithWorkAdmissionGuard result = acquired:%v callback:%v, want both true", acquired, callbackRan)
	}
}

func TestWorkAdmissionGuardExpiresAfterCallbackPanic(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	workID := "work_admission_guard_panic"
	var escaped *WorkAdmissionGuard
	func() {
		defer func() {
			if recovered := recover(); recovered != "injected guard panic" {
				t.Fatalf("recovered %v, want injected guard panic", recovered)
			}
		}()
		_ = WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
			escaped = guard
			if err := guard.Validate(homePath, workID); err != nil {
				t.Fatalf("active guard rejected exact identity before panic: %v", err)
			}
			panic("injected guard panic")
		})
	}()

	if escaped == nil {
		t.Fatal("panic callback did not receive a guard")
	}
	if err := escaped.Validate(homePath, workID); err == nil {
		t.Fatal("guard remained valid after callback panic")
	}
	acquired, err := TryWithWorkAdmissionLock(homePath, workID, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("guard callback panic leaked the work-admission lock")
	}
}

const (
	workAdmissionCrossProcessRoleEnv = "COMPOUND_TEST_WORK_ADMISSION_ROLE"
	workAdmissionCrossProcessRootEnv = "COMPOUND_TEST_WORK_ADMISSION_ROOT"

	workAdmissionCrossProcessHolderRole = "holder-a"
	workAdmissionCrossProcessWaiterRole = "waiter-a"
	workAdmissionCrossProcessOtherRole  = "holder-b"

	workAdmissionCrossProcessWorkA = "work_cross_process_admission_a"
	workAdmissionCrossProcessWorkB = "work_cross_process_admission_b"
)

func TestWorkAdmissionLockSerializesAllWorkAcrossProcesses(t *testing.T) {
	if runWorkAdmissionCrossProcessHelper(t) {
		return
	}

	root := t.TempDir()
	homePath := filepath.Join(root, "home")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	materializeWorkAdmissionAuthorityRoot(t, homePath)

	holder := startWorkAdmissionCrossProcessChild(t, root, workAdmissionCrossProcessHolderRole)
	if err := holder.waitForSignal(filepath.Join(root, "holder-a.ready"), 10*time.Second); err != nil {
		t.Fatal(err)
	}

	waiter := startWorkAdmissionCrossProcessChild(t, root, workAdmissionCrossProcessWaiterRole)
	if err := waiter.waitForSignal(filepath.Join(root, "waiter-a.busy"), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waiter.waitForSignal(filepath.Join(root, "waiter-a.blocking"), 10*time.Second); err != nil {
		t.Fatal(err)
	}

	other := startWorkAdmissionCrossProcessChild(t, root, workAdmissionCrossProcessOtherRole)
	if err := other.waitForSignal(filepath.Join(root, "holder-b.busy"), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := other.waitForSignal(filepath.Join(root, "holder-b.blocking"), 10*time.Second); err != nil {
		t.Fatal(err)
	}

	assertWorkAdmissionCrossProcessChildBlocked(t, waiter, filepath.Join(root, "waiter-a.acquired"))
	assertWorkAdmissionCrossProcessChildBlocked(t, other, filepath.Join(root, "holder-b.acquired"))

	if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder-a.release")); err != nil {
		t.Fatal(err)
	}
	if err := holder.wait(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waiter.wait(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := other.wait(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waiter.waitForSignal(filepath.Join(root, "waiter-a.acquired"), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := other.waitForSignal(filepath.Join(root, "holder-b.acquired"), 10*time.Second); err != nil {
		t.Fatal(err)
	}

	for _, child := range []*workAdmissionCrossProcessChild{holder, waiter, other} {
		if child.cmd.ProcessState == nil || !child.cmd.ProcessState.Success() {
			t.Fatalf("cross-process child did not terminate successfully: %v", child.result())
		}
	}
}

func runWorkAdmissionCrossProcessHelper(t *testing.T) bool {
	role := os.Getenv(workAdmissionCrossProcessRoleEnv)
	if role == "" {
		return false
	}
	root := os.Getenv(workAdmissionCrossProcessRootEnv)
	if root == "" {
		t.Fatal("cross-process work-admission helper root is required")
	}
	homePath := filepath.Join(root, "home")

	switch role {
	case workAdmissionCrossProcessHolderRole:
		if err := WithWorkAdmissionLock(homePath, workAdmissionCrossProcessWorkA, func() error {
			if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder-a.ready")); err != nil {
				return err
			}
			return waitForWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder-a.release"), 10*time.Second)
		}); err != nil {
			t.Fatal(err)
		}
	case workAdmissionCrossProcessWaiterRole:
		acquired, err := TryWithWorkAdmissionLock(homePath, workAdmissionCrossProcessWorkA, func() error {
			return errors.New("same-work nonblocking probe acquired while authority was held")
		})
		if err != nil {
			t.Fatal(err)
		}
		if acquired {
			t.Fatal("same-work nonblocking probe acquired while authority was held")
		}
		if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "waiter-a.busy")); err != nil {
			t.Fatal(err)
		}
		previousBeforeFlock := homeMutationGuardBeforeFlock
		blockingSignaled := false
		homeMutationGuardBeforeFlock = func(home string) {
			if !blockingSignaled {
				blockingSignaled = true
				if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "waiter-a.blocking")); err != nil {
					panic(err)
				}
			}
			previousBeforeFlock(home)
		}
		defer func() { homeMutationGuardBeforeFlock = previousBeforeFlock }()
		if err := WithWorkAdmissionLock(homePath, workAdmissionCrossProcessWorkA, func() error {
			if err := validateWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder-a.release")); err != nil {
				return fmt.Errorf("same-work waiter acquired before durable holder release signal: %w", err)
			}
			return writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "waiter-a.acquired"))
		}); err != nil {
			t.Fatal(err)
		}
		if !blockingSignaled {
			t.Fatal("same-work waiter never reached blocking flock readiness point")
		}
	case workAdmissionCrossProcessOtherRole:
		acquired, err := TryWithWorkAdmissionLock(homePath, workAdmissionCrossProcessWorkB, func() error {
			return errors.New("different-work nonblocking probe acquired while authority was held")
		})
		if err != nil {
			t.Fatal(err)
		}
		if acquired {
			t.Fatal("different-work nonblocking probe acquired while authority was held")
		}
		if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder-b.busy")); err != nil {
			t.Fatal(err)
		}
		previousBeforeFlock := homeMutationGuardBeforeFlock
		blockingSignaled := false
		homeMutationGuardBeforeFlock = func(home string) {
			if !blockingSignaled {
				blockingSignaled = true
				if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder-b.blocking")); err != nil {
					panic(err)
				}
			}
			previousBeforeFlock(home)
		}
		defer func() { homeMutationGuardBeforeFlock = previousBeforeFlock }()
		if err := WithWorkAdmissionLock(homePath, workAdmissionCrossProcessWorkB, func() error {
			if err := validateWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder-a.release")); err != nil {
				return fmt.Errorf("different-work waiter acquired before durable holder release signal: %w", err)
			}
			return writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder-b.acquired"))
		}); err != nil {
			t.Fatal(err)
		}
		if !blockingSignaled {
			t.Fatal("different-work waiter never reached blocking flock readiness point")
		}
	default:
		t.Fatalf("unknown cross-process work-admission helper role %q", role)
	}
	return true
}

type workAdmissionCrossProcessChild struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan struct{}
	err    error
}

func startWorkAdmissionCrossProcessChild(t *testing.T, root, role string) *workAdmissionCrossProcessChild {
	t.Helper()
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestWorkAdmissionLockSerializesAllWorkAcrossProcesses$",
		"-test.count=1",
	)
	cmd.Env = append(
		os.Environ(),
		workAdmissionCrossProcessRoleEnv+"="+role,
		workAdmissionCrossProcessRootEnv+"="+root,
	)
	child := &workAdmissionCrossProcessChild{
		cmd:  cmd,
		done: make(chan struct{}),
	}
	cmd.Stdout = &child.stdout
	cmd.Stderr = &child.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cross-process work-admission helper %s: %v", role, err)
	}
	go func() {
		child.err = cmd.Wait()
		close(child.done)
	}()
	t.Cleanup(child.stop)
	return child
}

func (child *workAdmissionCrossProcessChild) wait(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-child.done:
		return child.result()
	case <-timer.C:
		return fmt.Errorf("cross-process work-admission helper timed out after %s", timeout)
	}
}

func (child *workAdmissionCrossProcessChild) waitForSignal(path string, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := validateWorkAdmissionCrossProcessSignal(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-child.done:
			// The child can publish its final signal and exit between the last
			// file poll and this wakeup. Linux commonly orders the events this
			// way, so revalidate the file after exit before calling it missing.
			if err := validateWorkAdmissionCrossProcessSignal(path); err == nil {
				return nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := child.result(); err != nil {
				return err
			}
			return fmt.Errorf(
				"cross-process work-admission helper exited without signal %s",
				filepath.Base(path),
			)
		case <-ticker.C:
		case <-timer.C:
			return fmt.Errorf(
				"cross-process work-admission helper timed out after %s waiting for signal %s",
				timeout,
				filepath.Base(path),
			)
		}
	}
}

func (child *workAdmissionCrossProcessChild) result() error {
	if child.err == nil {
		return nil
	}
	return fmt.Errorf(
		"cross-process work-admission helper: %w\nstdout:\n%s\nstderr:\n%s",
		child.err,
		child.stdout.String(),
		child.stderr.String(),
	)
}

func (child *workAdmissionCrossProcessChild) stop() {
	select {
	case <-child.done:
		return
	default:
	}
	_ = child.cmd.Process.Kill()
	<-child.done
}

func assertWorkAdmissionCrossProcessChildBlocked(
	t *testing.T,
	child *workAdmissionCrossProcessChild,
	acquiredSignal string,
) {
	t.Helper()
	select {
	case <-child.done:
		t.Fatalf("cross-process work-admission waiter exited before authority release: %v", child.result())
	default:
	}
	if err := validateWorkAdmissionCrossProcessSignal(acquiredSignal); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			t.Fatalf("cross-process work-admission waiter acquired before authority release: %s", filepath.Base(acquiredSignal))
		}
		t.Fatal(err)
	}
}

func writeWorkAdmissionCrossProcessSignal(path string) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	cleanup := func(cause error) error {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return cause
	}
	if _, err := file.WriteString("ready\n"); err != nil {
		return cleanup(err)
	}
	if err := file.Sync(); err != nil {
		return cleanup(err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Link(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Remove(tempPath); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func validateWorkAdmissionCrossProcessSignal(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("cross-process signal is not a nonempty regular file: %s", filepath.Base(path))
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func waitForWorkAdmissionCrossProcessSignal(path string, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := validateWorkAdmissionCrossProcessSignal(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			return fmt.Errorf("timed out after %s waiting for cross-process signal %s", timeout, filepath.Base(path))
		}
	}
}
