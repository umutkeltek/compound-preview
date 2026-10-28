package candidates

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReadRecordRejectsRewriteWithUnchangedStat(t *testing.T) {
	originalSnapshot, originalHook := candidateIdentitySnapshot, candidateAfterOpenHook
	t.Cleanup(func() {
		candidateIdentitySnapshot, candidateAfterOpenHook = originalSnapshot, originalHook
	})
	// Preserve inode, mode and size; model both writes occurring in one tick.
	// No sleeps or changes to the filesystem timestamps are needed.
	candidateIdentitySnapshot = func(stat *unix.Stat_t) candidateFileIdentity {
		identity := candidateIdentityFromStat(stat)
		identity.mtimeSecond, identity.mtimeNano = 100, 0
		identity.ctimeSecond, identity.ctimeNano = 100, 0
		return identity
	}
	homePath := t.TempDir()
	path := writeCustodyContractFile(t, homePath, "rewrite.json", `{"id":"rewrite","marker":"before"}`)
	candidateAfterOpenHook = func(string) {
		if err := os.WriteFile(path, []byte(`{"id":"rewrite","marker":"after!"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ReadRecord(homePath, Memory, "rewrite")
	var corruption *Corruption
	if !errors.As(err, &corruption) || corruption.Code != CorruptionRaced {
		t.Fatalf("same-stat rewrite error = %v, want raced corruption", err)
	}
}
