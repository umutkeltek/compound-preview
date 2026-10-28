package landlease

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// oldProofAdmission reproduces the v0.2 admission algorithm at 9b75765b6,
// proof_domain.go:275-315. Keep its strict schema and slot-0-only reads.
func oldProofAdmission(t *testing.T, ticket *ProofAdmission, holder string) error {
	t.Helper()
	return ticket.ticket.queue.withTurn(context.Background(), ticket.ticket, func() error {
		body, err := os.ReadFile(proofDomainManifestPath(ticket.backing))
		if err != nil {
			return err
		}
		var manifest proofDomainManifest
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&manifest); err != nil {
			return err
		}
		if manifest.SchemaVersion != "compound.proof_domain.v0.2" || manifest.Phase != "active" || manifest.Generation != ticket.identity.Generation {
			return fmt.Errorf("old client refuses proof domain")
		}
		now := time.Now()
		for _, class := range []ProofCapacityClass{ProofCapacitySystemFull, ProofCapacityNativeConsumer} {
			expected := ticket.identity
			expected.Class = class
			status, err := proofAdmissionSnapshot(proofClassRoot(ticket.backing, expected.Generation, class), now, &leaseBinding{identity: &expected, acquisition: true})
			if err != nil {
				return err
			}
			if status.Holder == "" {
				continue
			}
			if class == ticket.identity.Class {
				if status.Held {
					return &ProofAdmissionBusyError{Holder: status.Holder, Reason: "capacity occupied"}
				}
			} else if status.ProofCoordination.RepositoryFingerprint == ticket.identity.RepositoryFingerprint {
				return &ProofAdmissionBusyError{Holder: status.Holder, Reason: "same repository requires exact predecessor recovery"}
			}
		}
		repoRoot := proofRepositoryRoot(ticket.backing, ticket.identity.Generation)
		repoHolder := proofRepositoryHolder(&ticket.identity)
		pending, err := executionBarrierPending(repoRoot, repoHolder)
		if err != nil {
			return err
		}
		if pending {
			return &ExecutionPendingError{Holder: repoHolder}
		}
		root := proofClassRoot(ticket.backing, ticket.identity.Generation, ticket.identity.Class)
		binding := &leaseBinding{identity: &ticket.identity, acquisition: true, repositoryRoot: repoRoot}
		var repoBarrier *ExecutionBarrier
		_, barrier, err := acquireWithProofIdentityAndBarrierPrepared(root, holder, time.Minute, now, func(AcquireOutcome) error {
			var err error
			repoBarrier, err = lockExecutionBarrier(repoRoot, repoHolder)
			return err
		}, &ticket.identity, binding)
		if barrier != nil {
			t.Cleanup(func() { _ = barrier.Close() })
		}
		if repoBarrier != nil {
			t.Cleanup(func() { _ = repoBarrier.Close() })
		}
		return err
	})
}

func TestProofOldAdmissionAfterSlotOneCrashRefuses(t *testing.T) {
	homes, _, _ := proofDomainFixture(t)
	first, firstBarrier := acquireDomainProof(t, beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2"), "first")
	dead, deadBarrier := acquireDomainProof(t, beginDomainTicket(t, homes[1], ProofCapacitySystemFull, "3"), "crashed")
	requireProofSlot(t, dead, 1)
	if err := deadBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	releaseDomainProof(t, first, firstBarrier, "first")
	ticket := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "3")
	if err := oldProofAdmission(t, ticket, "old-client-successor"); err == nil {
		t.Fatal("old v0.2 admission admitted same repository on slot 0 while unrecovered slot 1 remained")
	}
}

func TestProofDomainV2KeepsSingleSlotAdmissionAndRefusesRemote(t *testing.T) {
	homes, backing := unmigratedProofDomainFixture(t)
	if err := MigrateProofDomain(backing, homes, allowRemoteFixture, nil); err != nil {
		t.Fatal(err)
	}
	first, barrier := acquireDomainProof(t, beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2"), "v2-proof")
	requireProofSlot(t, first, 0)
	second := beginDomainTicket(t, homes[1], ProofCapacitySystemFull, "3")
	requireProofBusy(t, second, "capacity occupied")
	rows, err := ProofResourceStatuses(homes[0], time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Slot != 0 || rows[1].Slot != 0 {
		t.Fatalf("v0.2 advertises extra slots: %+v", rows)
	}
	request := RemoteExecutionRequest{ID: "v2_request", Digest: strings.Repeat("a", 64)}
	if err := first.PrepareRemoteExecution("v2-proof", request, allowRemoteFixture); err == nil {
		t.Fatal("v0.2 prepared remote execution")
	}
	releaseDomainProof(t, first, barrier, "v2-proof")
	next, nextBarrier := acquireDomainProof(t, second, "v2-next")
	requireProofSlot(t, next, 0)
	releaseDomainProof(t, next, nextBarrier, "v2-next")
	// An incompatible historical writer must never make a nonzero slot invisible.
	identity, _ := NewProofCoordinationIdentity(ProofDomainMigrationGeneration(backing, homes), ProofCapacitySystemFull, strings.Repeat("4", 64))
	identity, _ = identity.WithSlot(1)
	lease := newLease("unsupported-slot", 60, time.Now().Add(-time.Hour))
	lease.ProofCoordination = &identity
	path, _ := statePath(proofSlotRoot(backing, identity.Generation, identity.Class, 1))
	if err := writeLeaseAtomic(path, lease); err != nil {
		t.Fatal(err)
	}
	ticket := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "5")
	if err := ticket.WithTurn(context.Background(), false, func(*Resource) error { t.Fatal("unsupported slot ignored"); return nil }); err == nil {
		t.Fatal("v0.2 admitted with a hidden nonzero slot")
	}
}

func TestProofDomainV2PreservesOriginalClassFIFO(t *testing.T) {
	homes, backing := unmigratedProofDomainFixture(t)
	if err := MigrateProofDomain(backing, homes, allowRemoteFixture, nil); err != nil {
		t.Fatal(err)
	}
	acquireDomainProof(t, beginDomainTicket(t, homes[0], ProofCapacityNativeConsumer, "2"), "native")
	earlier := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
	later := beginDomainTicket(t, homes[1], ProofCapacitySystemFull, "3")
	requireQueueBusy(t, later, earlier)
}
