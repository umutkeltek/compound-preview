package home

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkAdmissionRecoveryMarkerFaultsNeverFalseReportSuccess(t *testing.T) {
	injected := errors.New("injected recovery marker persistence failure")
	tests := []struct {
		name   string
		inject func(*testing.T)
	}{
		{
			name: "create",
			inject: func(t *testing.T) {
				original := workAdmissionRecoveryOpenFile
				workAdmissionRecoveryOpenFile = func(*os.Root, string, int, os.FileMode) (*os.File, error) {
					return nil, injected
				}
				t.Cleanup(func() { workAdmissionRecoveryOpenFile = original })
			},
		},
		{
			name: "write",
			inject: func(t *testing.T) {
				original := workAdmissionRecoveryWrite
				workAdmissionRecoveryWrite = func(*os.File, []byte) (int, error) {
					return 0, injected
				}
				t.Cleanup(func() { workAdmissionRecoveryWrite = original })
			},
		},
		{
			name: "file_sync",
			inject: func(t *testing.T) {
				original := workAdmissionRecoveryFileSync
				workAdmissionRecoveryFileSync = func(*os.File) error { return injected }
				t.Cleanup(func() { workAdmissionRecoveryFileSync = original })
			},
		},
		{
			name: "rename",
			inject: func(t *testing.T) {
				original := workAdmissionRecoveryRename
				workAdmissionRecoveryRename = func(*os.Root, string, string) error { return injected }
				t.Cleanup(func() { workAdmissionRecoveryRename = original })
			},
		},
		{
			name: "parent_sync",
			inject: func(t *testing.T) {
				original := workAdmissionRecoveryParentSync
				workAdmissionRecoveryParentSync = func(*os.File) error { return injected }
				t.Cleanup(func() { workAdmissionRecoveryParentSync = original })
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			homePath := t.TempDir()
			if err := os.Mkdir(filepath.Join(homePath, "work"), 0o700); err != nil {
				t.Fatal(err)
			}
			test.inject(t)

			ready := make(chan struct{})
			release := make(chan struct{})
			holderErr := make(chan error, 1)
			go func() {
				holderErr <- WithWorkAdmissionGuard(homePath, "work_recovery_marker_fault", func(*WorkAdmissionGuard) error {
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
			close(release)

			select {
			case err := <-holderErr:
				if err == nil || !strings.Contains(err.Error(), "recovery_required") ||
					!strings.Contains(err.Error(), injected.Error()) {
					t.Fatalf("holder error = %v, want recovery_required plus injected persistence failure", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("holder did not return after recovery marker fault")
			}
		})
	}
}

func TestWorkAdmissionRecoveryMarkerRejectsUnsafeOrMalformedState(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "malformed",
			setup: func(t *testing.T, markerPath string) {
				t.Helper()
				if err := os.WriteFile(markerPath, []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, markerPath string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "marker.json")
				if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, markerPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory",
			setup: func(t *testing.T, markerPath string) {
				t.Helper()
				if err := os.Mkdir(markerPath, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			homePath := t.TempDir()
			if err := os.Mkdir(filepath.Join(homePath, "work"), 0o700); err != nil {
				t.Fatal(err)
			}
			markerDir := filepath.Join(homePath, "lock", "capability-lifecycle-work-admission")
			if err := os.MkdirAll(markerDir, 0o700); err != nil {
				t.Fatal(err)
			}
			markerPath := filepath.Join(markerDir, "recovery-required.json")
			test.setup(t, markerPath)

			entered := false
			acquired, err := TryWithWorkAdmissionGuard(homePath, "work_recovery_marker_rejected", func(*WorkAdmissionGuard) error {
				entered = true
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "recovery_required") {
				t.Fatalf("unsafe marker error = %v, want recovery_required", err)
			}
			if acquired || entered {
				t.Fatalf("unsafe marker admitted mutation: acquired=%v entered=%v", acquired, entered)
			}
		})
	}
}
