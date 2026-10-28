package landlease

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func unmigratedProofDomainFixture(t *testing.T) ([]string, string) {
	t.Helper()
	backing, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	homes := []string{t.TempDir(), t.TempDir()}
	for i, h := range homes {
		homes[i], err = filepath.EvalSymlinks(h)
		if err != nil {
			t.Fatal(err)
		}
		if err := MigrateLegacyLayout(homes[i], backing, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(homes)
	return homes, backing
}
func TestProofMigrationInterruptedFenceRefusesOldAndNewClients(t *testing.T) {
	for _, stage := range []string{"proof-fenced", "proof-prepared"} {
		t.Run(stage, func(t *testing.T) {
			homes, backing := unmigratedProofDomainFixture(t)
			omitted, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := MigrateLegacyLayout(omitted, backing, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			err = MigrateProofDomain(backing, homes, func() error { return nil }, func(got string) error {
				if got == stage {
					return errors.New("interrupted")
				}
				return nil
			})
			if err == nil {
				t.Fatal("injected interruption missing")
			}
			// This is the exact scoped path an old binary would use, bypassing new routing.
			if _, err := Acquire(scopedRoot(backing, ProofResourceKey()), "old", time.Minute, time.Now()); err == nil {
				t.Fatal("old client bypassed permanent fence")
			}
			if _, err := ProofResource(homes[0]).Acquire("upgraded", time.Minute, time.Now()); err == nil {
				t.Fatal("upgraded historical client admitted during cutover")
			}
			if _, err := ProofResource(omitted).Acquire("omitted-old", time.Minute, time.Now()); err == nil {
				t.Fatal("omitted scoped home bypassed shared fence")
			}
			if err := MigrateProofDomain(backing, homes, func() error { return nil }, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := ProofCoordinationFor(omitted, ProofCapacityNativeConsumer, strings.Repeat("a", 64)); err == nil {
				t.Fatal("unenrolled home admitted")
			}
			if _, err := ProofCoordinationFor(homes[0], ProofCapacityNativeConsumer, strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestProofMigrationRefusesLeaseDescendantsAndOwnerCensus(t *testing.T) {
	for _, mode := range []string{"expired-lease", "descendant", "census"} {
		t.Run(mode, func(t *testing.T) {
			homes, backing := unmigratedProofDomainFixture(t)
			root := scopedRoot(backing, ProofResourceKey())
			if mode == "expired-lease" {
				if _, err := Acquire(root, "old", time.Second, time.Now().Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "descendant" {
				barrier, err := lockExecutionBarrier(root, "descendant")
				if err != nil {
					t.Fatal(err)
				}
				defer barrier.Close()
			}
			err := MigrateProofDomain(backing, homes, func() error {
				if mode == "census" {
					return errors.New("pending recovery")
				}
				return nil
			}, nil)
			if err == nil {
				t.Fatal("unsafe migration admitted")
			}
			if _, err := os.Stat(proofDomainManifestPath(backing)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refusal published manifest: %v", err)
			}
		})
	}
}
func TestProofMigrationActiveManifestRequiresExactFence(t *testing.T) {
	homes, backing, _ := proofDomainFixture(t)
	path, _ := statePath(scopedRoot(backing, ProofResourceKey()))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := ProofCoordinationFor(homes[0], ProofCapacityNativeConsumer, strings.Repeat("a", 64)); err == nil {
		t.Fatal("active manifest without fence admitted")
	}
}

func TestProofDomainRevisionUpgradeIsExplicitAndQuiescent(t *testing.T) {
	homes, backing := unmigratedProofDomainFixture(t)
	if err := MigrateProofDomain(backing, homes, func() error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := MigrateProofDomain(backing, homes, func() error { calls++; return nil }, nil, ProofDomainFencedV3); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("owner authority not revalidated: %d", calls)
	}
	status, err := ReadProofDomainStatus(backing)
	if err != nil {
		t.Fatal(err)
	}
	if status.SchemaVersion != proofDomainFencedSchema || status.Phase != "active" {
		t.Fatalf("wrong revision: %+v", status)
	}
	if err := MigrateProofDomain(backing, homes, func() error { return nil }, nil); err == nil {
		t.Fatal("revision downgrade accepted")
	}
}

func TestProofDomainRevisionUpgradeInterruptionAndUnknownVersion(t *testing.T) {
	homes, backing := unmigratedProofDomainFixture(t)
	if err := MigrateProofDomain(backing, homes, func() error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	err := MigrateProofDomain(backing, homes, func() error { return nil }, func(stage string) error {
		if stage == "proof-prepared" {
			return errors.New("interrupted upgrade")
		}
		return nil
	}, ProofDomainFencedV3)
	if err == nil {
		t.Fatal("fault missing")
	}
	status, err := ReadProofDomainStatus(backing)
	if err != nil || status.SchemaVersion != proofDomainFencedSchema || status.Phase != "prepared" {
		t.Fatalf("not safely prepared: %+v %v", status, err)
	}
	if _, err := ProofCoordinationFor(homes[0], ProofCapacitySystemFull, strings.Repeat("a", 64)); err == nil {
		t.Fatal("prepared domain admitted")
	}
	if err := MigrateProofDomain(backing, homes, func() error { return errors.New("pause expired") }, nil, ProofDomainFencedV3); err == nil {
		t.Fatal("stale authority resumed")
	}
	if err := MigrateProofDomain(backing, homes, func() error { return nil }, nil, ProofDomainFencedV3); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := readProofDomain(backing)
	if err != nil {
		t.Fatal(err)
	}
	manifest.SchemaVersion = "compound.proof_domain.v999"
	if err := writeProofDomainJSON(proofDomainManifestPath(backing), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProofDomainStatus(backing); err == nil {
		t.Fatal("unknown domain revision accepted")
	}
	if err := MigrateProofDomain(backing, homes, func() error { return nil }, nil, ProofDomainFencedV3); err == nil {
		t.Fatal("unknown domain upgraded")
	}
}

func TestProofDomainRevisionUpgradeRefusesOldLiveAndExpiredSlots(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			homes, backing := unmigratedProofDomainFixture(t)
			if err := MigrateProofDomain(backing, homes, func() error { return nil }, nil); err != nil {
				t.Fatal(err)
			}
			generation := ProofDomainMigrationGeneration(backing, homes)
			identity, _ := NewProofCoordinationIdentity(generation, ProofCapacitySystemFull, strings.Repeat("a", 64))
			root := proofClassRoot(backing, generation, ProofCapacitySystemFull)
			now := time.Now()
			if expired {
				now = now.Add(-time.Hour)
			}
			lease := newLease("old-proof", 60, now)
			lease.ProofCoordination = &identity
			path, _ := statePath(root)
			if err := writeLeaseAtomic(path, lease); err != nil {
				t.Fatal(err)
			}
			if err := MigrateProofDomain(backing, homes, func() error { return nil }, nil, ProofDomainFencedV3); err == nil {
				t.Fatal("unrecovered old slot allowed upgrade")
			}
			status, err := ReadProofDomainStatus(backing)
			if err != nil || status.SchemaVersion != proofDomainSchema || status.Phase != "active" {
				t.Fatalf("refusal changed active revision: %+v %v", status, err)
			}
		})
	}
}
