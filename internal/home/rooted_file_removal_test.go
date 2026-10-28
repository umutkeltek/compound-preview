package home

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRemovalFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "state", "victim.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("owned bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, path
}

func TestRemoveRootedRegularFileDurableValidatedRemovesExactFile(t *testing.T) {
	home, path := writeRemovalFixture(t)
	validated := false
	err := RemoveRootedRegularFileDurableValidated(home, path, "test file", func(data []byte) error {
		validated = string(data) == "owned bytes\n"
		return nil
	})
	if err != nil || !validated {
		t.Fatalf("validated removal failed: err=%v validated=%v", err, validated)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("file survived validated removal: %v", statErr)
	}
}

func TestRemoveRootedRegularFileDurableValidatedRequiresExistingRemovalChain(t *testing.T) {
	homePath := t.TempDir()
	path := filepath.Join(homePath, "missing.json")

	err := RemoveRootedRegularFileDurableValidated(homePath, path, "test file", nil)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing required removal error = %v, want os.ErrNotExist", err)
	}
}

// FALSIFIER (parent generation): a recovery rooted in a directory descriptor
// must not report success after the canonical parent path is replaced. The
// replacement target is current authority; the detached recovery residue must
// remain untouched for an operator or a later generation-aware reconciler.
func TestRecoverRootedRegularFileRemovalDurableValidatedRefusesParentSymlinkSwap(t *testing.T) {
	homePath := t.TempDir()
	parentPath := filepath.Join(homePath, "state")
	detachedParentPath := filepath.Join(homePath, "state-detached")
	replacementParentPath := filepath.Join(homePath, "replacement")
	if err := os.MkdirAll(parentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(replacementParentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parentPath, "victim.json")
	quarantinePath := path + rootedRemovalPendingSuffix
	if err := os.WriteFile(quarantinePath, []byte("owned bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(replacementParentPath, filepath.Base(path))
	if err := os.WriteFile(replacementPath, []byte("replacement bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	previous := removeRootedBeforeUnlink
	t.Cleanup(func() { removeRootedBeforeUnlink = previous })
	removeRootedBeforeUnlink = func(string) error {
		if err := os.Rename(parentPath, detachedParentPath); err != nil {
			return err
		}
		return os.Symlink(replacementParentPath, parentPath)
	}

	err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", func(data []byte) error {
		if string(data) != "owned bytes\n" {
			return fmt.Errorf("unexpected recovery bytes %q", data)
		}
		return nil
	})
	if err == nil {
		t.Fatal("recovery reported success after canonical parent generation changed")
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil || string(body) != "replacement bytes\n" {
		t.Fatalf("canonical replacement target changed: body=%q err=%v", body, readErr)
	}
	detachedQuarantinePath := filepath.Join(detachedParentPath, filepath.Base(quarantinePath))
	body, readErr = os.ReadFile(detachedQuarantinePath)
	if readErr != nil || string(body) != "owned bytes\n" {
		t.Fatalf("detached recovery residue changed: body=%q err=%v", body, readErr)
	}
}

func TestRecoverRootedRegularFileRemovalDurableValidatedRefusesParentDirectoryGenerationSwap(t *testing.T) {
	homePath := t.TempDir()
	parentPath := filepath.Join(homePath, "state")
	detachedParentPath := filepath.Join(homePath, "state-detached")
	replacementParentPath := filepath.Join(homePath, "replacement")
	if err := os.MkdirAll(parentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(replacementParentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parentPath, "victim.json")
	quarantinePath := path + rootedRemovalPendingSuffix
	if err := os.WriteFile(quarantinePath, []byte("owned bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(replacementParentPath, filepath.Base(path))
	if err := os.WriteFile(replacementPath, []byte("replacement bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	previous := removeRootedBeforeUnlink
	t.Cleanup(func() { removeRootedBeforeUnlink = previous })
	removeRootedBeforeUnlink = func(string) error {
		if err := os.Rename(parentPath, detachedParentPath); err != nil {
			return err
		}
		return os.Rename(replacementParentPath, parentPath)
	}

	err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", func(data []byte) error {
		if string(data) != "owned bytes\n" {
			return fmt.Errorf("unexpected recovery bytes %q", data)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "parent generation changed") {
		t.Fatalf("recovery did not reject a different directory generation: %v", err)
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil || string(body) != "replacement bytes\n" {
		t.Fatalf("canonical directory replacement changed: body=%q err=%v", body, readErr)
	}
	detachedQuarantinePath := filepath.Join(detachedParentPath, filepath.Base(quarantinePath))
	body, readErr = os.ReadFile(detachedQuarantinePath)
	if readErr != nil || string(body) != "owned bytes\n" {
		t.Fatalf("detached recovery residue changed: body=%q err=%v", body, readErr)
	}
}

// FALSIFIER (final quarantine swap): a foreign file atomically swapped onto
// the predictable quarantine pathname after content/inode validation must
// never be unlinked. A path-based final removal deletes bytes the validator
// never reviewed; the unlink must stay bound to the validated inode.
func TestRemoveRootedRegularFileDurableValidatedFinalQuarantineSwapPreservesReplacement(t *testing.T) {
	home, path := writeRemovalFixture(t)
	quarantinePath := path + ".remove-pending"
	var replacementInfo os.FileInfo
	removeRootedBeforeUnlink = func(reported string) error {
		if reported != quarantinePath {
			t.Fatalf("seam reported %q, want %q", reported, quarantinePath)
		}
		swap := path + ".swap"
		if err := os.WriteFile(swap, []byte("owned bytes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(swap, quarantinePath); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(quarantinePath)
		if err != nil {
			t.Fatal(err)
		}
		replacementInfo = info
		return nil
	}
	defer func() { removeRootedBeforeUnlink = nil }()

	err := RemoveRootedRegularFileDurableValidated(home, path, "test file", nil)
	if err == nil || !strings.Contains(err.Error(), "file changed after validation") {
		t.Fatalf("byte-identical foreign swap was not refused: %v", err)
	}
	survivor := path
	info, statErr := os.Lstat(survivor)
	if statErr != nil {
		survivor = quarantinePath
		info, statErr = os.Lstat(survivor)
	}
	if statErr != nil {
		t.Fatalf("replacement did not survive refusal anywhere: %v", statErr)
	}
	if !os.SameFile(replacementInfo, info) {
		t.Fatalf("survivor at %s is not the replacement inode", survivor)
	}
	body, readErr := os.ReadFile(survivor)
	if readErr != nil || string(body) != "owned bytes\n" {
		t.Fatalf("replacement bytes were not preserved: err=%v body=%q", readErr, body)
	}
}

func TestRemoveRootedRegularFileDurableValidatedValidatorRefusalRestoresFile(t *testing.T) {
	home, path := writeRemovalFixture(t)
	err := RemoveRootedRegularFileDurableValidated(home, path, "test file", func([]byte) error {
		return os.ErrPermission
	})
	if err == nil {
		t.Fatal("validator refusal did not error")
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil || string(body) != "owned bytes\n" {
		t.Fatalf("refused file was not left in place: err=%v body=%q", readErr, body)
	}
}

func TestRemoveRootedRegularFileDurableValidatedRecoversExactRemovalChains(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "original and quarantine hardlink",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Link(path, path+".remove-pending"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "quarantine only",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Rename(path, path+".remove-pending"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "repeated quarantine suffix only",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Rename(path, path+".remove-pending.remove-pending"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath, path := writeRemovalFixture(t)
			tc.setup(t, path)
			validated := 0
			err := RemoveRootedRegularFileDurableValidated(homePath, path, "test file", func(data []byte) error {
				validated++
				if string(data) != "owned bytes\n" {
					return fmt.Errorf("unexpected test file bytes %q", data)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("recover exact removal chain: %v", err)
			}
			if validated == 0 {
				t.Fatal("recovery removed a residue the validator never accepted")
			}
			for _, candidate := range []string{
				path,
				path + ".remove-pending",
				path + ".remove-pending.remove-pending",
			} {
				if _, statErr := os.Lstat(candidate); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("removal chain entry survived at %s: %v", candidate, statErr)
				}
			}
		})
	}
}

func TestRecoverRootedRegularFileRemovalDurableValidatedRetryAfterCompletedRemovalIsIdempotent(t *testing.T) {
	homePath, path := writeRemovalFixture(t)
	validate := func(data []byte) error {
		if string(data) != "owned bytes\n" {
			return fmt.Errorf("unexpected test file bytes %q", data)
		}
		return nil
	}
	if err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", validate); err != nil {
		t.Fatalf("first removal: %v", err)
	}
	if err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", validate); err != nil {
		t.Fatalf("idempotent removal retry: %v", err)
	}
}

func TestReadRootedRegularFileRemovalChainTreatsMissingParentAsAbsentWithoutMaterializing(t *testing.T) {
	homePath := t.TempDir()
	path := filepath.Join(homePath, "state", "nested", "missing.json")

	body, present, err := ReadRootedRegularFileRemovalChain(homePath, path, "test file")
	if err != nil {
		t.Fatal(err)
	}
	if present || len(body) != 0 {
		t.Fatalf("missing removal chain = (%q, %v), want empty and absent", body, present)
	}
	entries, err := os.ReadDir(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("read-only missing-parent probe materialized home entries: %+v", entries)
	}
}

func TestRemovalChainReadAndMutationDistinguishMissingAndUnsafeParents(t *testing.T) {
	t.Run("missing parent", func(t *testing.T) {
		homePath := t.TempDir()
		path := filepath.Join(homePath, "state", "nested", "victim.json")

		body, present, err := ReadRootedRegularFileRemovalChain(homePath, path, "test file")
		if err != nil || present || len(body) != 0 {
			t.Fatalf("missing-parent read = (%q, %v, %v), want absent", body, present, err)
		}
		if err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", nil); err == nil {
			t.Fatal("missing-parent mutation was accepted as completed removal")
		}
		entries, err := os.ReadDir(homePath)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("missing-parent read or mutation materialized state: %+v", entries)
		}
	})

	t.Run("symlink parent", func(t *testing.T) {
		homePath := t.TempDir()
		outside := t.TempDir()
		outsidePath := filepath.Join(outside, "victim.json")
		if err := os.WriteFile(outsidePath, []byte("outside bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(homePath, "state")); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(homePath, "state", "victim.json")

		if _, _, err := ReadRootedRegularFileRemovalChain(homePath, path, "test file"); err == nil {
			t.Fatal("removal-chain read followed a symlinked parent")
		}
		if err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", nil); err == nil {
			t.Fatal("removal-chain mutation followed a symlinked parent")
		}
		body, err := os.ReadFile(outsidePath)
		if err != nil || string(body) != "outside bytes\n" {
			t.Fatalf("unsafe-parent probes changed outside file: body=%q err=%v", body, err)
		}
	})

	t.Run("non-directory parent", func(t *testing.T) {
		homePath := t.TempDir()
		parentPath := filepath.Join(homePath, "state")
		if err := os.WriteFile(parentPath, []byte("parent bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parentPath, "victim.json")

		if _, _, err := ReadRootedRegularFileRemovalChain(homePath, path, "test file"); err == nil {
			t.Fatal("removal-chain read crossed a non-directory parent")
		}
		if err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", nil); err == nil {
			t.Fatal("removal-chain mutation crossed a non-directory parent")
		}
		body, err := os.ReadFile(parentPath)
		if err != nil || string(body) != "parent bytes\n" {
			t.Fatalf("non-directory-parent probes changed parent file: body=%q err=%v", body, err)
		}
	})
}

func TestReadRootedRegularFileRemovalChainCostDoesNotScaleWithSiblings(t *testing.T) {
	sparseHome, sparsePath := writeRemovalFixture(t)
	denseHome, densePath := writeRemovalFixture(t)
	for index := 0; index < 1_000; index++ {
		sibling := filepath.Join(filepath.Dir(densePath), fmt.Sprintf("sibling-%04d.json", index))
		if err := os.WriteFile(sibling, []byte("unrelated\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	measure := func(homePath, path string) float64 {
		t.Helper()
		var lastErr error
		allocations := testing.AllocsPerRun(3, func() {
			body, present, err := ReadRootedRegularFileRemovalChain(homePath, path, "test file")
			lastErr = err
			if err == nil && (!present || string(body) != "owned bytes\n") {
				lastErr = fmt.Errorf("unexpected removal-chain read: present=%v body=%q", present, body)
			}
		})
		if lastErr != nil {
			t.Fatalf("measure removal-chain inspection: %v", lastErr)
		}
		return allocations
	}

	sparseAllocations := measure(sparseHome, sparsePath)
	denseAllocations := measure(denseHome, densePath)
	if denseAllocations > sparseAllocations+128 {
		t.Fatalf("removal-chain inspection allocations scale with sibling count: sparse=%.0f dense=%.0f", sparseAllocations, denseAllocations)
	}
}

func TestRemoveRootedRegularFileDurableRefusesUnvalidatedRecoveryResidue(t *testing.T) {
	homePath, path := writeRemovalFixture(t)
	residue := path + ".remove-pending"
	if err := os.Rename(path, residue); err != nil {
		t.Fatal(err)
	}

	err := RemoveRootedRegularFileDurable(homePath, path, "test file")
	if err == nil || !strings.Contains(err.Error(), "recovery requires content or inode validation") {
		t.Fatalf("unvalidated recovery error = %v, want validator refusal", err)
	}
	body, readErr := os.ReadFile(residue)
	if readErr != nil || string(body) != "owned bytes\n" {
		t.Fatalf("unvalidated recovery residue changed: body=%q err=%v", body, readErr)
	}
}

func TestRemoveRootedRegularFileDurableValidatedRefusesAmbiguousRemovalChains(t *testing.T) {
	corruptErr := errors.New("corrupt test file residue")
	tests := []struct {
		name       string
		setup      func(t *testing.T, path string)
		validator  func([]byte) error
		wantError  string
		wantIs     error
		assertKept func(t *testing.T, path string)
	}{
		{
			name: "split inodes",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path+".remove-pending", []byte("owned bytes\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			validator: func(data []byte) error {
				if string(data) != "owned bytes\n" {
					return corruptErr
				}
				return nil
			},
			wantError: "do not identify one validated file",
			assertKept: func(t *testing.T, path string) {
				t.Helper()
				original, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				quarantine, err := os.Lstat(path + ".remove-pending")
				if err != nil {
					t.Fatal(err)
				}
				if os.SameFile(original, quarantine) {
					t.Fatal("split removal-chain fixture unexpectedly shares one inode")
				}
			},
		},
		{
			name: "corrupt quarantine only",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".remove-pending", []byte("corrupt bytes\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			validator: func(data []byte) error {
				if string(data) != "owned bytes\n" {
					return corruptErr
				}
				return nil
			},
			wantIs: corruptErr,
			assertKept: func(t *testing.T, path string) {
				t.Helper()
				body, err := os.ReadFile(path + ".remove-pending")
				if err != nil || string(body) != "corrupt bytes\n" {
					t.Fatalf("corrupt quarantine changed: body=%q err=%v", body, err)
				}
			},
		},
		{
			name: "symlink quarantine only",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "operator-data")
				if err := os.WriteFile(target, []byte("preserve\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path+".remove-pending"); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "regular non-symlink removal residue",
			assertKept: func(t *testing.T, path string) {
				t.Helper()
				info, err := os.Lstat(path + ".remove-pending")
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("symlink quarantine changed: info=%v err=%v", info, err)
				}
			},
		},
		{
			name: "nonregular quarantine only",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path+".remove-pending", 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "regular non-symlink removal residue",
			assertKept: func(t *testing.T, path string) {
				t.Helper()
				info, err := os.Lstat(path + ".remove-pending")
				if err != nil || !info.IsDir() {
					t.Fatalf("directory quarantine changed: info=%v err=%v", info, err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath, path := writeRemovalFixture(t)
			tc.setup(t, path)
			err := RemoveRootedRegularFileDurableValidated(homePath, path, "test file", tc.validator)
			if tc.wantIs != nil {
				if !errors.Is(err, tc.wantIs) {
					t.Fatalf("removal-chain refusal error = %v, want errors.Is(%v)", err, tc.wantIs)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("removal-chain refusal error = %v, want %q", err, tc.wantError)
			}
			tc.assertKept(t, path)
		})
	}
}

func TestRecoverRootedRegularFileRemovalDurableValidatedRetryResyncsAfterFinalParentSyncFailure(t *testing.T) {
	homePath, path := writeRemovalFixture(t)
	injected := errors.New("injected final removal parent sync failure")
	previous := syncRootedCASDirectoryFile
	t.Cleanup(func() { syncRootedCASDirectoryFile = previous })
	failureInjected := false
	syncRootedCASDirectoryFile = func(file *os.File) error {
		_, canonicalErr := os.Lstat(path)
		_, quarantineErr := os.Lstat(path + rootedRemovalPendingSuffix)
		if !failureInjected && errors.Is(canonicalErr, os.ErrNotExist) && errors.Is(quarantineErr, os.ErrNotExist) {
			failureInjected = true
			return injected
		}
		return file.Sync()
	}
	validate := func(data []byte) error {
		if string(data) != "owned bytes\n" {
			return fmt.Errorf("unexpected test file bytes %q", data)
		}
		return nil
	}
	err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", validate)
	if !errors.Is(err, injected) {
		t.Fatalf("final parent-sync removal error = %v, want injected failure", err)
	}
	if !failureInjected {
		t.Fatal("final parent-sync removal failure was not injected")
	}
	body, present, readErr := ReadRootedRegularFileRemovalChain(homePath, path, "test file")
	if readErr != nil || !present || string(body) != "owned bytes\n" {
		t.Fatalf("final parent-sync failure lost durable removal evidence: present=%v body=%q err=%v", present, body, readErr)
	}

	syncRootedCASDirectoryFile = previous
	if err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", validate); err != nil {
		t.Fatalf("retry must re-sync the absent removal state: %v", err)
	}
	assertRemovalPathMissing(t, path)
	assertRemovalPathMissing(t, path+rootedRemovalPendingSuffix)
	assertRemovalPathMissing(t, path+rootedRemovalPendingSuffix+rootedRemovalPendingSuffix)
}

func TestRecoverRootedRegularFileRemovalDurableValidatedRestoresEvidenceAfterCleanupParentSyncFailure(t *testing.T) {
	homePath, path := writeRemovalFixture(t)
	injected := errors.New("injected removal evidence cleanup parent sync failure")
	previous := syncRootedCASDirectoryFile
	t.Cleanup(func() { syncRootedCASDirectoryFile = previous })

	quarantinePath := path + rootedRemovalPendingSuffix
	evidencePath := quarantinePath + rootedRemovalPendingSuffix
	failureInjected := false
	syncRootedCASDirectoryFile = func(file *os.File) error {
		_, canonicalErr := os.Lstat(path)
		_, quarantineErr := os.Lstat(quarantinePath)
		_, evidenceErr := os.Lstat(evidencePath)
		if !failureInjected &&
			errors.Is(canonicalErr, os.ErrNotExist) &&
			errors.Is(quarantineErr, os.ErrNotExist) &&
			errors.Is(evidenceErr, os.ErrNotExist) {
			failureInjected = true
			return injected
		}
		return file.Sync()
	}
	validate := func(data []byte) error {
		if string(data) != "owned bytes\n" {
			return fmt.Errorf("unexpected test file bytes %q", data)
		}
		return nil
	}

	err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", validate)
	if !errors.Is(err, injected) {
		t.Fatalf("evidence-cleanup parent-sync error = %v, want injected failure", err)
	}
	if !failureInjected {
		t.Fatal("evidence-cleanup parent-sync failure was not injected")
	}
	body, present, readErr := ReadRootedRegularFileRemovalChain(homePath, path, "test file")
	if readErr != nil || !present || string(body) != "owned bytes\n" {
		t.Fatalf("evidence-cleanup sync failure lost recovery frame: present=%v body=%q err=%v", present, body, readErr)
	}

	syncRootedCASDirectoryFile = previous
	if err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", validate); err != nil {
		t.Fatalf("evidence-cleanup recovery retry: %v", err)
	}
	assertRemovalPathMissing(t, path)
	assertRemovalPathMissing(t, quarantinePath)
	assertRemovalPathMissing(t, evidencePath)
}

func TestRecoverRootedRegularFileRemovalDurableValidatedResidueRetryKeepsEvidenceOnParentSyncFailure(t *testing.T) {
	homePath, path := writeRemovalFixture(t)
	quarantinePath := path + rootedRemovalPendingSuffix
	if err := os.Rename(path, quarantinePath); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected residue removal parent sync failure")
	previous := syncRootedCASDirectoryFile
	t.Cleanup(func() { syncRootedCASDirectoryFile = previous })
	failureInjected := false
	syncRootedCASDirectoryFile = func(file *os.File) error {
		_, canonicalErr := os.Lstat(path)
		_, quarantineErr := os.Lstat(quarantinePath)
		if !failureInjected && errors.Is(canonicalErr, os.ErrNotExist) && errors.Is(quarantineErr, os.ErrNotExist) {
			failureInjected = true
			return injected
		}
		return file.Sync()
	}
	validate := func(data []byte) error {
		if string(data) != "owned bytes\n" {
			return fmt.Errorf("unexpected test file bytes %q", data)
		}
		return nil
	}

	err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", validate)
	if !errors.Is(err, injected) {
		t.Fatalf("residue parent-sync removal error = %v, want injected failure", err)
	}
	if !failureInjected {
		t.Fatal("residue parent-sync removal failure was not injected")
	}
	body, present, readErr := ReadRootedRegularFileRemovalChain(homePath, path, "test file")
	if readErr != nil || !present || string(body) != "owned bytes\n" {
		t.Fatalf("residue parent-sync failure lost durable removal evidence: present=%v body=%q err=%v", present, body, readErr)
	}

	syncRootedCASDirectoryFile = previous
	if err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", validate); err != nil {
		t.Fatalf("residue retry recovery: %v", err)
	}
	assertRemovalPathMissing(t, path)
	assertRemovalPathMissing(t, quarantinePath)
	assertRemovalPathMissing(t, quarantinePath+rootedRemovalPendingSuffix)
}

func TestRestoreRootedRemovalEvidenceFailureBoundariesNeverLeavePartialFrame(t *testing.T) {
	injected := errors.New("injected removal evidence restore failure")
	tests := []struct {
		name            string
		want            error
		stage           rootedRemovalEvidenceRestoreStage
		shortWriteBytes int
		wantStaging     bool
	}{
		{
			name:  "create",
			want:  injected,
			stage: rootedRemovalRestoreCreate,
		},
		{
			name:        "chmod",
			want:        injected,
			stage:       rootedRemovalRestoreChmod,
			wantStaging: true,
		},
		{
			name:        "write",
			want:        injected,
			stage:       rootedRemovalRestoreWrite,
			wantStaging: true,
		},
		{
			name:            "short_write",
			want:            io.ErrShortWrite,
			shortWriteBytes: len("owned bytes\n") - 1,
			wantStaging:     true,
		},
		{
			name:        "file_sync",
			want:        injected,
			stage:       rootedRemovalRestoreFileSync,
			wantStaging: true,
		},
		{
			name:        "close",
			want:        injected,
			stage:       rootedRemovalRestoreClose,
			wantStaging: true,
		},
		{
			name:        "publication",
			want:        injected,
			stage:       rootedRemovalRestorePublication,
			wantStaging: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath, path := writeRemovalFixture(t)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			root, relativePath, displayPath, err := openRootedPath(homePath, path, "test file")
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			parent, chain, _, err := inspectRootedRemovalChain(root, relativePath, displayPath, "test file")
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			if len(chain) != 0 {
				t.Fatalf("unexpected removal chain before restore: %d", len(chain))
			}

			originalLeaf := filepath.Base(relativePath)
			evidenceLeaf := originalLeaf + rootedRemovalPendingSuffix
			evidencePath := path + rootedRemovalPendingSuffix
			stagingPath := filepath.Join(filepath.Dir(path), rootedRemovalEvidenceStagingLeaf(originalLeaf))
			ops := defaultRootedRemovalEvidenceRestoreOps()
			ops.shortWriteBytes = tc.shortWriteBytes
			if tc.stage != "" {
				ops.fault = func(stage rootedRemovalEvidenceRestoreStage) error {
					if stage == tc.stage {
						return injected
					}
					return nil
				}
			}
			err = restoreRootedRemovalEvidenceWithOps(
				parent,
				originalLeaf,
				evidenceLeaf,
				displayPath+rootedRemovalPendingSuffix,
				"test file",
				[]byte("owned bytes\n"),
				0o644,
				ops,
			)
			if !errors.Is(err, tc.want) {
				t.Fatalf("restore failure = %v, want errors.Is(%v)", err, tc.want)
			}
			if _, statErr := os.Lstat(evidencePath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed restore left a partial recovery frame: %v", statErr)
			}
			_, stagingErr := os.Lstat(stagingPath)
			if tc.wantStaging && stagingErr != nil {
				t.Fatalf("failed restore did not retain named staging recovery: %v", stagingErr)
			}
			if !tc.wantStaging && !errors.Is(stagingErr, os.ErrNotExist) {
				t.Fatalf("pre-create failure left staging residue: %v", stagingErr)
			}
			body, present, readErr := ReadRootedRegularFileRemovalChain(homePath, path, "test file")
			if tc.wantStaging {
				if readErr == nil || present || body != nil || !strings.Contains(readErr.Error(), "removal recovery staging is pending") {
					t.Fatalf("staging fault was reported as clean absence: present=%v body=%q err=%v", present, body, readErr)
				}
			} else if readErr != nil || present || body != nil {
				t.Fatalf("pre-create fault changed clean absence: present=%v body=%q err=%v", present, body, readErr)
			}
			if err := RecoverRootedRegularFileRemovalDurableValidated(
				homePath,
				path,
				"test file",
				func(data []byte) error {
					if string(data) != "owned bytes\n" {
						return fmt.Errorf("unexpected recovery bytes %q", data)
					}
					return nil
				},
			); err != nil {
				t.Fatalf("public removal recovery retry: %v", err)
			}
			if _, statErr := os.Lstat(evidencePath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("public recovery left removal evidence: %v", statErr)
			}
			if _, statErr := os.Lstat(stagingPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("public recovery did not reconcile restore staging: %v", statErr)
			}
		})
	}
}

func TestRestoreRootedRemovalEvidencePublishesOnlyExactCompleteFrame(t *testing.T) {
	homePath, path := writeRemovalFixture(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	root, relativePath, displayPath, err := openRootedPath(homePath, path, "test file")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	parent, chain, _, err := inspectRootedRemovalChain(root, relativePath, displayPath, "test file")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if len(chain) != 0 {
		t.Fatalf("unexpected removal chain before restore: %d", len(chain))
	}

	originalLeaf := filepath.Base(relativePath)
	evidenceLeaf := originalLeaf + rootedRemovalPendingSuffix
	if err := restoreRootedRemovalEvidence(
		parent,
		originalLeaf,
		evidenceLeaf,
		displayPath+rootedRemovalPendingSuffix,
		"test file",
		[]byte("owned bytes\n"),
		0o640,
	); err != nil {
		t.Fatalf("restore exact removal evidence: %v", err)
	}
	body, present, readErr := ReadRootedRegularFileRemovalChain(homePath, path, "test file")
	if readErr != nil || !present || string(body) != "owned bytes\n" {
		t.Fatalf("published removal evidence = present=%v body=%q err=%v", present, body, readErr)
	}
	evidenceInfo, err := os.Lstat(path + rootedRemovalPendingSuffix)
	if err != nil || evidenceInfo.Mode().Perm() != 0o640 {
		t.Fatalf("published removal evidence mode = %v err=%v, want 0640", evidenceInfo, err)
	}
	stagingPath := filepath.Join(filepath.Dir(path), rootedRemovalEvidenceStagingLeaf(originalLeaf))
	if _, err := os.Lstat(stagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful publication left staging: %v", err)
	}
}

func TestRestoreRootedRemovalEvidencePartialWriteNeverBecomesRemovalAuthority(t *testing.T) {
	homePath, path := writeRemovalFixture(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	root, relativePath, displayPath, err := openRootedPath(homePath, path, "test file")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	parent, chain, _, err := inspectRootedRemovalChain(root, relativePath, displayPath, "test file")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if len(chain) != 0 {
		t.Fatalf("unexpected removal chain before restore: %d", len(chain))
	}

	data := []byte("owned bytes\n")
	ops := defaultRootedRemovalEvidenceRestoreOps()
	ops.shortWriteBytes = len(data) - 1
	originalLeaf := filepath.Base(relativePath)
	evidenceLeaf := originalLeaf + rootedRemovalPendingSuffix
	err = restoreRootedRemovalEvidenceWithOps(
		parent,
		originalLeaf,
		evidenceLeaf,
		displayPath+rootedRemovalPendingSuffix,
		"test file",
		data,
		0o644,
		ops,
	)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("partial restore error = %v, want io.ErrShortWrite", err)
	}
	body, present, readErr := ReadRootedRegularFileRemovalChain(homePath, path, "test file")
	if readErr == nil || present || body != nil || !strings.Contains(readErr.Error(), "removal recovery staging is pending") {
		t.Fatalf("partial staging was not reported as non-authoritative recovery-required state: present=%v body=%q err=%v", present, body, readErr)
	}
	stagingPath := filepath.Join(filepath.Dir(path), rootedRemovalEvidenceStagingLeaf(originalLeaf))
	body, readErr = os.ReadFile(stagingPath)
	if readErr != nil || string(body) != string(data[:len(data)-1]) {
		t.Fatalf("partial staging is not named and discoverable: body=%q err=%v", body, readErr)
	}
}

func TestReadRootedRegularFileRemovalChainFailsClosedOnConflictingOrUnsafeStaging(t *testing.T) {
	tests := []struct {
		name          string
		setup         func(t *testing.T, stagingPath string)
		recoverable   bool
		wantErrorText string
	}{
		{
			name: "conflicting_regular",
			setup: func(t *testing.T, stagingPath string) {
				t.Helper()
				if err := os.WriteFile(stagingPath, []byte("conflicting bytes\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			recoverable:   true,
			wantErrorText: "removal recovery staging is pending",
		},
		{
			name: "symlink",
			setup: func(t *testing.T, stagingPath string) {
				t.Helper()
				targetPath := stagingPath + ".target"
				if err := os.WriteFile(targetPath, []byte("target bytes\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(targetPath, stagingPath); err != nil {
					t.Fatal(err)
				}
			},
			wantErrorText: "must be a regular file",
		},
		{
			name: "nonregular_directory",
			setup: func(t *testing.T, stagingPath string) {
				t.Helper()
				if err := os.Mkdir(stagingPath, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantErrorText: "must be a regular file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath, path := writeRemovalFixture(t)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			originalLeaf := filepath.Base(path)
			stagingPath := filepath.Join(filepath.Dir(path), rootedRemovalEvidenceStagingLeaf(originalLeaf))
			tc.setup(t, stagingPath)

			body, present, err := ReadRootedRegularFileRemovalChain(homePath, path, "test file")
			if err == nil || present || body != nil || !strings.Contains(err.Error(), tc.wantErrorText) {
				t.Fatalf("unsafe staging reader result: present=%v body=%q err=%v", present, body, err)
			}
			err = RemoveRootedRegularFileDurableValidated(homePath, path, "test file", nil)
			if err == nil {
				t.Fatal("ordinary removal silently accepted staging-only recovery state")
			}

			err = RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", func([]byte) error {
				return fmt.Errorf("staging bytes are not authoritative validation input")
			})
			if tc.recoverable {
				if err != nil {
					t.Fatalf("explicit recovery did not reconcile reserved regular staging: %v", err)
				}
				if _, err := os.Lstat(stagingPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("explicit recovery left regular staging: %v", err)
				}
				if err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", nil); err != nil {
					t.Fatalf("explicit recovery retry was not idempotent: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrorText) {
				t.Fatalf("explicit recovery silently accepted unsafe staging: %v", err)
			}
			if _, err := os.Lstat(stagingPath); err != nil {
				t.Fatalf("unsafe staging was not preserved after refusal: %v", err)
			}
		})
	}
}

func TestRestoreRootedRemovalEvidenceParentSyncFailureKeepsExactFrame(t *testing.T) {
	homePath, path := writeRemovalFixture(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	root, relativePath, displayPath, err := openRootedPath(homePath, path, "test file")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	parent, chain, _, err := inspectRootedRemovalChain(root, relativePath, displayPath, "test file")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if len(chain) != 0 {
		t.Fatalf("unexpected removal chain before restore: %d", len(chain))
	}

	injected := errors.New("injected restored-evidence parent sync failure")
	ops := defaultRootedRemovalEvidenceRestoreOps()
	ops.fault = func(stage rootedRemovalEvidenceRestoreStage) error {
		if stage == rootedRemovalRestoreParentSync {
			return injected
		}
		return nil
	}
	originalLeaf := filepath.Base(relativePath)
	evidenceLeaf := originalLeaf + rootedRemovalPendingSuffix
	err = restoreRootedRemovalEvidenceWithOps(
		parent,
		originalLeaf,
		evidenceLeaf,
		displayPath+rootedRemovalPendingSuffix,
		"test file",
		[]byte("owned bytes\n"),
		0o644,
		ops,
	)
	if !errors.Is(err, injected) {
		t.Fatalf("parent sync restore failure = %v, want injected error", err)
	}
	body, present, readErr := ReadRootedRegularFileRemovalChain(homePath, path, "test file")
	if readErr == nil || !present || body != nil || !strings.Contains(readErr.Error(), "removal recovery staging is pending") {
		t.Fatalf("parent sync failure hid recovery-required staging: present=%v body=%q err=%v", present, body, readErr)
	}
	evidencePath := path + rootedRemovalPendingSuffix
	stagingPath := filepath.Join(filepath.Dir(path), rootedRemovalEvidenceStagingLeaf(originalLeaf))
	evidenceInfo, evidenceErr := os.Lstat(evidencePath)
	stagingInfo, stagingErr := os.Lstat(stagingPath)
	if evidenceErr != nil || stagingErr != nil || !os.SameFile(evidenceInfo, stagingInfo) {
		t.Fatalf("parent sync failure did not retain one exact staged/published inode: evidence=%v staging=%v", evidenceErr, stagingErr)
	}
	body, readErr = os.ReadFile(evidencePath)
	if readErr != nil || string(body) != "owned bytes\n" {
		t.Fatalf("parent sync failure changed exact published evidence: body=%q err=%v", body, readErr)
	}
	if err := RecoverRootedRegularFileRemovalDurableValidated(homePath, path, "test file", func(data []byte) error {
		if string(data) != "owned bytes\n" {
			return fmt.Errorf("unexpected recovery bytes %q", data)
		}
		return nil
	}); err != nil {
		t.Fatalf("public parent sync retry: %v", err)
	}
	if _, err := os.Lstat(stagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("public parent sync retry did not reconcile staging: %v", err)
	}
	if _, err := os.Lstat(evidencePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("public parent sync retry left removal evidence: %v", err)
	}
}

func assertRemovalPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removal path survived at %s: %v", path, err)
	}
}
