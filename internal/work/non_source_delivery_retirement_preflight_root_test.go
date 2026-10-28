package work

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/authority/gitrepository"
)

// Recovery custody accounts for every reference before content-addressed storage
// deduplicates bodies. Equal digests must not reduce the admission byte count.
func TestRetirementRecoveryPreflightCountsEqualBodiesAtDifferentPaths(t *testing.T) {
	dir := t.TempDir()
	fileProof := func(name string, size int64) gitrepository.WorktreeRetirementRecoveryProof {
		t.Helper()
		path := filepath.Join(dir, name)
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		if err := file.Truncate(size); err != nil {
			t.Fatal(err)
		}
		hash := sha256.New()
		if n, err := io.Copy(hash, file); err != nil || n != size {
			t.Fatalf("hash fixture: bytes=%d want=%d err=%v", n, size, err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		digest := fmt.Sprintf("%x", hash.Sum(nil))
		return gitrepository.WorktreeRetirementRecoveryProof{
			Reference: "sha256-file:" + digest + ":" + path,
			Kind:      "sha256-file",
			Digest:    "sha256:" + digest,
			Path:      path,
		}
	}
	first := fileProof("first.bundle", 64<<20)
	second := fileProof("second.bundle", 64<<20)
	third := fileProof("one-byte.bundle", 1)
	if first.Path == second.Path || first.Digest != second.Digest {
		t.Fatal("fixture must have distinct paths with equal content digests")
	}
	planFor := func(proofs ...gitrepository.WorktreeRetirementRecoveryProof) gitrepository.WorktreeRetirementPlan {
		plan := gitrepository.WorktreeRetirementPlan{RecoveryProofs: proofs}
		for _, proof := range proofs {
			plan.RecoveryRefs = append(plan.RecoveryRefs, proof.Reference)
		}
		return plan
	}
	t.Run("exact aggregate capacity remains admissible", func(t *testing.T) {
		if err := PreflightRetirementRecoveryInventory(planFor(first, second)); err != nil {
			t.Fatalf("two 64 MiB references must fit the 128 MiB aggregate limit: %v", err)
		}
	})
	t.Run("extra file is independently admissible", func(t *testing.T) {
		if err := PreflightRetirementRecoveryInventory(planFor(third)); err != nil {
			t.Fatalf("valid one-byte recovery file must pass by itself: %v", err)
		}
	})
	t.Run("one byte beyond aggregate capacity refuses", func(t *testing.T) {
		if err := PreflightRetirementRecoveryInventory(planFor(first, second, third)); err == nil {
			t.Fatal("equal body digests must not hide a 128 MiB + 1 byte recovery inventory")
		}
	})
}
