package landlease

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProofReleaseRequiresInstalledRepositoryBarrier(t *testing.T) {
	homes, backing, generation := proofDomainFixture(t)
	ticket := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
	resource, barrier := acquireDomainProof(t, ticket, "proof:existing-holder")
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}
	identity := resource.ProofIdentity()
	root := proofRepositoryRoot(backing, generation)
	path := filepath.Join(root, executionBarrierRelPath, executionBarrierID(proofRepositoryHolder(identity))+".lock")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	before, err := resource.StatusReadOnly(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var unknown *ExecutionUnattestedError
	if _, err := resource.AttestExecutionTerminated(before.Holder); !errors.As(err, &unknown) {
		t.Fatalf("missing repository barrier attested: %v", err)
	}
	called := false
	if _, err := resource.ReleaseAfter(before.Holder, time.Now(), func() error { called = true; return nil }); !errors.As(err, &unknown) || called {
		t.Fatalf("missing repository barrier reached finalizer: called=%v err=%v", called, err)
	}
	after, err := resource.StatusReadOnly(time.Now())
	if err != nil || after.Holder != before.Holder || after.ExpiresAt != before.ExpiresAt {
		t.Fatalf("refusal changed holder: %+v %v", after, err)
	}
	// A new repository has no installed barrier yet. Admission must still create it.
	independent := beginDomainTicket(t, homes[0], ProofCapacityNativeConsumer, "3")
	_, fresh := acquireDomainProof(t, independent, "proof:new-repository")
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
}
