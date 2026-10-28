package home

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	workAdmissionReplacementRoleEnv    = "COMPOUND_TEST_WORK_ADMISSION_REPLACEMENT_ROLE"
	workAdmissionReplacementRootEnv    = "COMPOUND_TEST_WORK_ADMISSION_REPLACEMENT_ROOT"
	workAdmissionReplacementVariantEnv = "COMPOUND_TEST_WORK_ADMISSION_REPLACEMENT_VARIANT"

	workAdmissionReplacementHolderRole    = "holder"
	workAdmissionReplacementContenderRole = "contender"
	workAdmissionReplacementWorkID        = "work_admission_descendant_replacement"

	workAdmissionAuthorityRootRoleEnv       = "COMPOUND_TEST_WORK_ADMISSION_AUTHORITY_ROOT_ROLE"
	workAdmissionAuthorityRootHolderRole    = "holder"
	workAdmissionAuthorityRootContenderRole = "contender"
	workAdmissionAuthorityRootHolderID      = "work_admission_authority_root_holder"
	workAdmissionAuthorityRootContenderID   = "work_admission_authority_root_contender"
)

func TestWorkAdmissionLockRequiresMaterializedAuthorityRootWithoutFootprint(t *testing.T) {
	homePath := t.TempDir()
	callbackRan := false
	err := WithWorkAdmissionLock(homePath, "work_missing_authority_root", func() error {
		callbackRan = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "authority root") {
		t.Fatalf("missing work authority root error = %v, want authority-root refusal", err)
	}
	if callbackRan {
		t.Fatal("callback ran without a materialized work authority root")
	}
	entries, err := os.ReadDir(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("missing authority-root refusal materialized footprint: %v", entries)
	}
}

func TestWorkAdmissionLockRejectsUnsafeAuthorityRootWithoutLockFootprint(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, homePath string) {
				t.Helper()
				target := t.TempDir()
				if err := os.Symlink(target, filepath.Join(homePath, "work")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "regular file",
			setup: func(t *testing.T, homePath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(homePath, "work"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "writable by group",
			setup: func(t *testing.T, homePath string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(homePath, "work"), 0o770); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(homePath, "work"), 0o770); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			homePath := t.TempDir()
			test.setup(t, homePath)
			callbackRan := false
			err := WithWorkAdmissionLock(homePath, "work_unsafe_authority_root", func() error {
				callbackRan = true
				return nil
			})
			if err == nil || callbackRan {
				t.Fatalf("unsafe work authority root was accepted: err=%v callback=%v", err, callbackRan)
			}
			if _, err := os.Lstat(filepath.Join(homePath, "lock")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe authority-root refusal materialized lock footprint: %v", err)
			}
		})
	}
}

func TestWorkAdmissionLockNilCallbackDoesNotMaterializeLockFootprint(t *testing.T) {
	homePath := t.TempDir()
	if err := os.Mkdir(filepath.Join(homePath, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WithWorkAdmissionLock(homePath, "work_nil_callback", nil); err == nil {
		t.Fatal("nil work-admission callback accepted")
	}
	if _, err := os.Lstat(filepath.Join(homePath, "lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nil callback materialized lock footprint: %v", err)
	}
}

func TestWorkAdmissionGuardRejectsSameProcessAuthorityRootReplacementBypass(t *testing.T) {
	homePath := t.TempDir()
	if err := os.Mkdir(filepath.Join(homePath, "work"), 0o700); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	release := make(chan struct{})
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- WithWorkAdmissionGuard(homePath, "work_authority_root_holder", func(guard *WorkAdmissionGuard) error {
			if err := guard.Validate(homePath, "work_authority_root_holder"); err != nil {
				return err
			}
			close(ready)
			<-release
			return nil
		})
	}()
	<-ready

	workRoot := filepath.Join(homePath, "work")
	if err := os.Rename(workRoot, workRoot+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	syncWorkAdmissionReplacementDirectory(t, homePath)

	contenderEntered := false
	acquired, contenderErr := TryWithWorkAdmissionGuard(homePath, "work_authority_root_contender", func(*WorkAdmissionGuard) error {
		contenderEntered = true
		return nil
	})
	if contenderErr != nil {
		t.Errorf("same-process contender error = %v, want busy without error", contenderErr)
	}
	if acquired || contenderEntered {
		t.Errorf("same-process contender bypassed replaced authority root: acquired=%v entered=%v", acquired, contenderEntered)
	}

	close(release)
	select {
	case err := <-holderErr:
		if err == nil || !strings.Contains(err.Error(), "recovery_required") {
			t.Errorf("holder error = %v, want durable recovery_required", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("holder did not return after release")
	}

	markerPath := filepath.Join(
		homePath,
		"lock",
		"capability-lifecycle-work-admission",
		"recovery-required.json",
	)
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Errorf("read durable work-admission recovery marker: %v", err)
	} else if !strings.Contains(string(marker), "recovery_required") {
		t.Errorf("recovery marker = %q, want recovery_required", marker)
	}

	laterEntered := false
	laterAcquired, laterErr := TryWithWorkAdmissionGuard(homePath, "work_authority_root_later", func(*WorkAdmissionGuard) error {
		laterEntered = true
		return nil
	})
	if laterErr == nil || !strings.Contains(laterErr.Error(), "recovery_required") {
		t.Errorf("later admission error = %v, want recovery_required fence", laterErr)
	}
	if laterAcquired || laterEntered {
		t.Errorf("later admission crossed recovery fence: acquired=%v entered=%v", laterAcquired, laterEntered)
	}
}

func TestWorkAdmissionGuardRejectsCrossProcessAuthorityRootReplacementBypass(t *testing.T) {
	if runWorkAdmissionAuthorityRootCrossProcessHelper(t) {
		return
	}

	root := t.TempDir()
	homePath := filepath.Join(root, "home")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(homePath, "work"), 0o700); err != nil {
		t.Fatal(err)
	}

	holder := startWorkAdmissionAuthorityRootCrossProcessChild(
		t,
		root,
		workAdmissionAuthorityRootHolderRole,
	)
	if err := holder.waitForSignal(filepath.Join(root, "holder.ready"), 10*time.Second); err != nil {
		t.Fatal(err)
	}

	workRoot := filepath.Join(homePath, "work")
	if err := os.Rename(workRoot, workRoot+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	syncWorkAdmissionReplacementDirectory(t, homePath)

	contender := startWorkAdmissionAuthorityRootCrossProcessChild(
		t,
		root,
		workAdmissionAuthorityRootContenderRole,
	)
	if err := contender.waitForSignal(filepath.Join(root, "contender.work-root-free"), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := contender.waitForSignal(filepath.Join(root, "contender.home-flock-reached"), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	assertWorkAdmissionCrossProcessChildBlocked(t, contender, filepath.Join(root, "contender.entered"))

	if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder.release")); err != nil {
		t.Fatal(err)
	}
	if err := holder.wait(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := contender.waitForSignal(filepath.Join(root, "contender.fenced"), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := contender.wait(10 * time.Second); err != nil {
		t.Fatal(err)
	}

	markerPath := filepath.Join(
		homePath,
		"lock",
		"capability-lifecycle-work-admission",
		"recovery-required.json",
	)
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(marker), "recovery_required") {
		t.Fatalf("recovery marker = %q, want recovery_required", marker)
	}

	entered := false
	acquired, err := TryWithWorkAdmissionGuard(homePath, "work_admission_authority_root_later", func(*WorkAdmissionGuard) error {
		entered = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "recovery_required") {
		t.Fatalf("later admission error = %v, want recovery_required fence", err)
	}
	if acquired || entered {
		t.Fatalf("later admission crossed recovery fence: acquired=%v entered=%v", acquired, entered)
	}
}

func runWorkAdmissionAuthorityRootCrossProcessHelper(t *testing.T) bool {
	role := os.Getenv(workAdmissionAuthorityRootRoleEnv)
	if role == "" {
		return false
	}
	root := os.Getenv(workAdmissionReplacementRootEnv)
	if root == "" {
		t.Fatal("cross-process authority-root helper root is required")
	}
	homePath := filepath.Join(root, "home")

	switch role {
	case workAdmissionAuthorityRootHolderRole:
		err := WithWorkAdmissionGuard(homePath, workAdmissionAuthorityRootHolderID, func(guard *WorkAdmissionGuard) error {
			if err := guard.Validate(homePath, workAdmissionAuthorityRootHolderID); err != nil {
				return err
			}
			if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder.ready")); err != nil {
				return err
			}
			return waitForWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder.release"), 10*time.Second)
		})
		if err == nil || !strings.Contains(err.Error(), "recovery_required") {
			t.Fatalf("authority-root holder error = %v, want recovery_required", err)
		}
	case workAdmissionAuthorityRootContenderRole:
		authority, err := os.Open(filepath.Join(homePath, workAdmissionAuthorityRootRelative))
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = authority.Close()
			t.Fatalf("replacement work authority was not independently lockable: %v", err)
		}
		if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_UN); err != nil {
			_ = authority.Close()
			t.Fatal(err)
		}
		if err := authority.Close(); err != nil {
			t.Fatal(err)
		}
		if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "contender.work-root-free")); err != nil {
			t.Fatal(err)
		}
		previousBeforeFlock := homeMutationGuardBeforeFlock
		signaled := false
		homeMutationGuardBeforeFlock = func(home string) {
			if !signaled {
				signaled = true
				if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "contender.home-flock-reached")); err != nil {
					panic(err)
				}
			}
			previousBeforeFlock(home)
		}
		defer func() { homeMutationGuardBeforeFlock = previousBeforeFlock }()

		err = WithWorkAdmissionGuard(homePath, workAdmissionAuthorityRootContenderID, func(*WorkAdmissionGuard) error {
			return writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "contender.entered"))
		})
		if err == nil || !strings.Contains(err.Error(), "recovery_required") {
			t.Fatalf("authority-root contender error = %v, want recovery_required fence", err)
		}
		if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "contender.fenced")); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown authority-root helper role %q", role)
	}
	return true
}

func startWorkAdmissionAuthorityRootCrossProcessChild(t *testing.T, root, role string) *workAdmissionCrossProcessChild {
	t.Helper()
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestWorkAdmissionGuardRejectsCrossProcessAuthorityRootReplacementBypass$",
		"-test.count=1",
	)
	cmd.Env = append(
		os.Environ(),
		workAdmissionAuthorityRootRoleEnv+"="+role,
		workAdmissionReplacementRootEnv+"="+root,
	)
	child := &workAdmissionCrossProcessChild{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = &child.stdout
	cmd.Stderr = &child.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cross-process authority-root helper %s: %v", role, err)
	}
	go func() {
		child.err = cmd.Wait()
		close(child.done)
	}()
	t.Cleanup(child.stop)
	return child
}

func TestWorkAdmissionGuardRejectsCrossProcessDescendantReplacementBypass(t *testing.T) {
	if runWorkAdmissionReplacementCrossProcessHelper(t) {
		return
	}

	tests := []struct {
		name    string
		replace func(*testing.T, string)
	}{
		{
			name: "leaf",
			replace: func(t *testing.T, homePath string) {
				t.Helper()
				lockPath := filepath.Join(
					homePath,
					workAdmissionDirectoryRelative,
					workAdmissionReplacementWorkID+".lock",
				)
				if err := os.Rename(lockPath, lockPath+".held"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				syncWorkAdmissionReplacementDirectory(t, filepath.Dir(lockPath))
			},
		},
		{
			name: "parent",
			replace: func(t *testing.T, homePath string) {
				t.Helper()
				admissionDirectory := filepath.Join(homePath, workAdmissionDirectoryRelative)
				if err := os.Rename(admissionDirectory, admissionDirectory+".held"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(admissionDirectory, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					filepath.Join(admissionDirectory, workAdmissionReplacementWorkID+".lock"),
					nil,
					0o644,
				); err != nil {
					t.Fatal(err)
				}
				syncWorkAdmissionReplacementDirectory(t, filepath.Dir(admissionDirectory))
				syncWorkAdmissionReplacementDirectory(t, admissionDirectory)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			homePath := filepath.Join(root, "home")
			if err := os.Mkdir(homePath, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(homePath, "work"), 0o700); err != nil {
				t.Fatal(err)
			}

			holder := startWorkAdmissionReplacementCrossProcessChild(
				t,
				root,
				workAdmissionReplacementHolderRole,
				test.name,
			)
			if err := holder.waitForSignal(filepath.Join(root, "holder.ready"), 10*time.Second); err != nil {
				t.Fatal(err)
			}
			test.replace(t, homePath)

			contender := startWorkAdmissionReplacementCrossProcessChild(
				t,
				root,
				workAdmissionReplacementContenderRole,
				test.name,
			)
			if err := contender.waitForSignal(filepath.Join(root, "contender.authority-probed"), 10*time.Second); err != nil {
				t.Fatal(err)
			}
			if err := validateWorkAdmissionCrossProcessSignal(filepath.Join(root, "contender.authority-contended")); err != nil {
				if freeErr := validateWorkAdmissionCrossProcessSignal(filepath.Join(root, "contender.authority-free")); freeErr == nil {
					t.Fatal("cross-process contender acquired work authority while holder retained it")
				}
				t.Fatalf("work-authority probe produced no contention evidence: %v", err)
			}
			if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder.release")); err != nil {
				t.Fatal(err)
			}
			if err := holder.wait(10 * time.Second); err != nil {
				t.Fatal(err)
			}
			if err := contender.waitForSignal(filepath.Join(root, "contender.fenced"), 10*time.Second); err != nil {
				t.Fatal(err)
			}
			if err := contender.wait(10 * time.Second); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func runWorkAdmissionReplacementCrossProcessHelper(t *testing.T) bool {
	role := os.Getenv(workAdmissionReplacementRoleEnv)
	if role == "" {
		return false
	}
	root := os.Getenv(workAdmissionReplacementRootEnv)
	if root == "" {
		t.Fatal("cross-process work-admission replacement helper root is required")
	}
	homePath := filepath.Join(root, "home")

	switch role {
	case workAdmissionReplacementHolderRole:
		err := WithWorkAdmissionGuard(homePath, workAdmissionReplacementWorkID, func(guard *WorkAdmissionGuard) error {
			if err := guard.Validate(homePath, workAdmissionReplacementWorkID); err != nil {
				return fmt.Errorf("validate holder guard before replacement: %w", err)
			}
			if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder.ready")); err != nil {
				return err
			}
			return waitForWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder.release"), 10*time.Second)
		})
		if err == nil || (!strings.Contains(err.Error(), "identity changed") &&
			!strings.Contains(err.Error(), "path generation changed")) {
			t.Fatalf("replacement holder error = %v, want custody-generation error", err)
		}
	case workAdmissionReplacementContenderRole:
		authority, err := os.Open(filepath.Join(homePath, workAdmissionAuthorityRootRelative))
		if err != nil {
			t.Fatal(err)
		}
		probeErr := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		probeSignal := "contender.authority-contended"
		switch {
		case probeErr == nil:
			probeSignal = "contender.authority-free"
			if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_UN); err != nil {
				_ = authority.Close()
				t.Fatal(err)
			}
		case errors.Is(probeErr, syscall.EWOULDBLOCK), errors.Is(probeErr, syscall.EAGAIN):
		default:
			_ = authority.Close()
			t.Fatalf("nonblocking work-authority probe error = %v, want lock contention", probeErr)
		}
		if err := authority.Close(); err != nil {
			t.Fatal(err)
		}
		if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, probeSignal)); err != nil {
			t.Fatal(err)
		}
		if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "contender.authority-probed")); err != nil {
			t.Fatal(err)
		}
		err = WithWorkAdmissionGuard(homePath, workAdmissionReplacementWorkID, func(guard *WorkAdmissionGuard) error {
			if err := guard.Validate(homePath, workAdmissionReplacementWorkID); err != nil {
				return fmt.Errorf("validate contender guard: %w", err)
			}
			if err := validateWorkAdmissionCrossProcessSignal(filepath.Join(root, "holder.release")); err != nil {
				return fmt.Errorf("contender entered before durable holder release signal: %w", err)
			}
			if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "contender.entered")); err != nil {
				return err
			}
			return waitForWorkAdmissionCrossProcessSignal(filepath.Join(root, "contender.release"), 10*time.Second)
		})
		if err == nil || !strings.Contains(err.Error(), "recovery_required") {
			t.Fatalf("contender error = %v, want recovery_required fence", err)
		}
		if err := writeWorkAdmissionCrossProcessSignal(filepath.Join(root, "contender.fenced")); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown work-admission replacement helper role %q", role)
	}
	return true
}

func startWorkAdmissionReplacementCrossProcessChild(
	t *testing.T,
	root string,
	role string,
	variant string,
) *workAdmissionCrossProcessChild {
	t.Helper()
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestWorkAdmissionGuardRejectsCrossProcessDescendantReplacementBypass$",
		"-test.count=1",
	)
	cmd.Env = append(
		os.Environ(),
		workAdmissionReplacementRoleEnv+"="+role,
		workAdmissionReplacementRootEnv+"="+root,
		workAdmissionReplacementVariantEnv+"="+variant,
	)
	child := &workAdmissionCrossProcessChild{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = &child.stdout
	cmd.Stderr = &child.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cross-process work-admission replacement helper %s: %v", role, err)
	}
	go func() {
		child.err = cmd.Wait()
		close(child.done)
	}()
	t.Cleanup(child.stop)
	return child
}
func syncWorkAdmissionReplacementDirectory(t *testing.T, path string) {
	t.Helper()
	directory, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		t.Fatal(err)
	}
}
